# 设计：windows-control 员工账号管理与用量限制

> 2026-09-07 · 上位文档 `架构_Codex多模型网关_2026-08.md`（§8.4 令牌生命周期）、
> `设计_windows-control下发Codex网关配置_2026-09.md`
>
> **要解决的问题**：今天「让一个员工能用 Codex」要在三个页面各点一次（名册、机器、网关），
> 「让他不能用」要点两个页面；漏任何一步就是半残状态。用量只有一次性美元上限，
> 没有周期重置、没有速率限制、重发令牌就归零。本设计把这些收成五个管理动作，
> 落在一个「员工账号」页面上。

## 0. 结论先行

- **员工账号的主体是网关上的 internal user**（`user_id = emp-<windowsUser小写>`），
  它承载月预算、速率、模型白名单和花费；控制台名册只存姓名、部门、在职状态。
- **额度由网关在请求路径上强制执行**，自然月 1 日自动重置。控制台不记账、不跑定时任务。
- 控制台负责把开户/关户编排成**幂等的多步动作**，并在列表页把两边的不一致标出来，
  修复按钮就是重跑动作本身。
- agent 一行不改；机器绑定页、代员工登录页不改。

## 1. 已验证的事实（设计依据）

2026-09-07 在生产网关（`litellm.teenet.app`，LiteLLM 容器 `sha256:20b5044b…`）用探针账号实测，事后已清理：

| 事实 | 实测 |
|---|---|
| `POST /user/new` 支持 `max_budget`、`budget_duration:"1mo"`、`rpm_limit`、`tpm_limit`、`max_parallel_requests`、`models`、`user_alias`、`metadata` | 全部落库；`budget_reset_at` 自动为 `2026-10-01T00:00:00Z` |
| 令牌以 `user_id` 挂在 user 下、自身不设额度 | `key/generate` 返回 `user_id=emp-probe`，`max_budget=null` |
| **user 级预算对子令牌生效** | 预算 1e-5 美元，第 1 次请求 200，第 2 次起 `429 ExceededBudget: User=emp-probe over budget` |
| 花费记在 user 上 | `GET /user/info` 的 `spend` 随请求累加；重发令牌不影响 |
| `POST /user/update`、`GET /user/list?page&page_size`、`POST /user/delete` | 均可用 |
| 现有记账数据 | `/spend/logs` 每次请求记录 token 数；配了单价的模型算出美元，glm-5.2 / deepseek-v4-flash 未配单价、花费恒为 0 |
| 现状问题 | 花费挂在令牌上，weipeng 重发后显示 0；现有 weipeng 令牌没有挂任何 user |

**未实测**：user 级 `rpm_limit` 是否对子令牌执行（预算测试先于速率触发 429）。列入验收清单 §8。

## 2. 数据模型与归属

| 数据 | 存哪 | 字段 | 以谁为准 |
|---|---|---|---|
| 名册条目 | OSS `admin/users.json`（现有） | 新增 `name`、`department`；保留 `enabled`、`codexAccount`、`claudeAccount` | 名册 |
| 员工配额与用量 | 网关 internal user，`user_id = emp-<windowsUser小写>` | `max_budget`、`budget_duration=1mo`、`rpm_limit`、`tpm_limit`、`max_parallel_requests`、`models`、`spend`、`budget_reset_at`、`user_alias=姓名`、`metadata.department` | 网关 |
| 令牌 | 网关 key，`user_id` 指向上面的 user，`key_alias` 同一字符串 | 令牌自身**不设**任何额度，全部继承 user | 网关 |
| 开户默认值 | OSS `admin/quota-defaults.json`（新） | `monthlyBudgetUSD`、`rpm`、`tpm`、`parallel` | 名册侧 |
| 操作日志 | OSS `admin/audit/<windowsUser>.jsonl`（新） | 见 §5 | 名册侧 |

`user_id` 与现有 `admincore.KeyAlias()` 是同一个字符串。它是唯一的对账把手：
名册 ↔ user ↔ key 三者靠它拼起来。

`department` 是自由文本，不建部门表。

## 3. 五个管理动作

全部实现在 `internal/admincore`，Web 层只做表单解析与跳转。每个动作最后一步写审计（§5）。

### 3.1 开户 Onboard（幂等）

输入：Windows 用户、姓名、部门、月预算、rpm/tpm/并发（默认值预填）、模型多选（全不选 = 网关全部）。

| 步 | 动作 | 失败后的可见状态 |
|---|---|---|
| 1 | 名册新增或更新，`enabled=true` | 无变化 |
| 2 | user 不存在则 `user/new`，存在则 `user/update` | 「在职无令牌」 |
| 3 | 吊销该 alias 旧令牌；`key/generate`（`user_id` 指向 user） | 「在职无令牌」 |
| 4 | `catalog.Build` + `renderCodexConfig` → `PublishCredentials` | **吊销刚发的令牌**，回到「在职无令牌」 |
| 5 | 审计 `onboard` | 只记控制台日志 |

步骤 1–3 天然幂等，重跑安全。步骤 4 的补偿沿用现有 Provision。
机器绑定不在这里做：机器要先上报才能绑，仍在机器页。

### 3.2 关户 Offboard

| 步 | 动作 | 失败后的可见状态 |
|---|---|---|
| 1 | 名册 `enabled=false` | 无变化 |
| 2 | 吊销该 alias 的令牌（网关侧即时失效） | 「已离职仍有令牌」 |
| 3 | 删 OSS `credentials.zip`（agent 下次同步清机器） | 名册停用但机器上文件未清，下次点关户重试 |
| 4 | 解绑该员工的全部机器 | 「已离职仍有机器绑定」 |
| 5 | 审计 `offboard` | 只记控制台日志 |

安全关键的两步放最前。**步骤 2 失败不中断**：继续做 3、4，最后把错误报给管理员并留下「已离职仍有令牌」标签——网关不可达不能成为机器上文件继续留着的理由。网关 user **保留**（花费历史在上面）；重新开户直接复用。

### 3.3 改额度 SetQuota

`user/update` 四个数字。即时生效，不动机器。审计 `quota`。

### 3.4 调模型 SetModels

`user/update models` + `key/update models` + 重发 `models.json`（现有 `SetCodexGatewayModels` 逻辑）。
两处都要改：令牌管「能不能用」，目录管「看不看得见」。审计 `models`。

### 3.5 重发令牌 Reissue

现有 `ProvisionCodexGateway` 收编为此动作，改为挂在 user 下。审计 `reissue`。

### 3.6 接口变化

- `litellm.Client` 新增 `UpsertUser`、`UserInfo`、`ListUsers`（分页）、`DeleteKeysByUser`；
  `GenerateKey` 增加 `userID` 参数。
- `admincore.Gateway` 接口同步扩展；测试用 `fakeGateway` 补齐。
- `admincore` 新文件 `account.go`（五个动作）、`audit.go`、`quotadefaults.go`。
  `SetUserEnabled(false)` 收编进 `Offboard`；旧函数保留给其他调用方。

## 4. 页面

### 4.1 `/users` 员工账号（重做）

吸收现有 `/gateway` 的「员工令牌」表。一行一人：

| 列 | 来源 |
|---|---|
| Windows 用户 / 姓名 / 部门 / 状态 | 名册 |
| 令牌 | 网关 key 列表 |
| 可用模型 | 网关 user |
| **本月花费 ÷ 预算** | 网关 user；进度条 ≥80% 黄、超额红；附 `budget_reset_at` |
| 速率 | 网关 user（rpm / tpm / 并发） |
| 对账标签 | §6 |

行内按钮：详情；在职则「关户」，离职则「重新开户」。
顶部「开户」表单：Windows 用户、姓名、部门、预算与速率（默认值预填）、模型多选。

### 4.2 `/users/<windowsUser>` 员工详情（新）

基本信息编辑、改额度、调模型、重发令牌、关户 / 重新开户；下方该人的操作日志倒序，最多 200 条。

### 4.3 `/gateway` 模型网关（瘦身）

只剩「可用模型」表。

### 4.4 `/settings`

新增「开户默认值」表单（月预算美元、rpm、tpm、并发）。

## 5. 操作日志

一人一个文件 `admin/audit/<windowsUser>.jsonl`，一行一条：

```json
{"at":"2026-09-07T08:12:33Z","action":"onboard","user":"weipeng","detail":{"budget":20,"rpm":60,"tpm":200000,"parallel":4,"models":["glm-5.2","gpt-5.6-sol"]}}
```

- `action` ∈ `onboard | offboard | quota | models | reissue`。
- 不记操作者：控制台登录用 OSS 密钥，没有管理员身份。多管理员是另一个子系统，不在本期。
- OSS 无原生追加，用读 → 拼一行 → 写回。控制台单实例、低频，不做并发保护。
- 写日志在每个动作最后；**写失败只记控制台日志，不让动作报错**。丢一条审计比开户回滚可接受。

## 6. 错误处理与对账

不追求跨两个存储的原子性。原则：**顺序设计成失败停在可见状态 + 列表页把状态标出来 + 修复按钮就是重跑动作**。

对账标签（列表页与详情页同时显示）：

| 标签 | 判定 | 修复按钮 |
|---|---|---|
| 在职无令牌 | `enabled` 且 alias 无 key | 重新开户 |
| 已离职仍有令牌 | `!enabled` 且 alias 有 key | 关户 |
| 有令牌无网关用户 | key 存在但 `user_id` 为空或无对应 user（现在的 weipeng） | 重新开户 |
| 已离职仍有机器绑定 | `!enabled` 且有 binding 指向该用户 | 关户 |

网关不可达时：列表仍显示名册；配额列显示「网关不可达」；开户 / 改额度 / 调模型 / 重发按钮禁用；
**关户仍可用**：名册停用与删配置包先做，令牌吊销失败留下「已离职仍有令牌」标签，网关恢复后再点一次。

## 7. 网关侧配套

- `config.yaml`：glm-5.2、deepseek-v4-flash 补 `input_cost_per_token` / `output_cost_per_token`，按中转站标价填。
  不填则这两个模型花费恒为 0，预算对它们形同虚设。价格是我们填的近似值，不是中转站真实账单。
- 现有 weipeng 令牌未挂 user：上线后在页面点一次「重新开户」即迁移，不写迁移脚本。
- 不做超额邮件 / Slack 告警；页面进度条颜色替代。

## 8. 测试与验收

**单元测试**
- `admincore`：五个动作各一组表驱动测试：成功路径、每一步失败后的可见状态、幂等重跑。
- `litellm`：`httptest` 校验 `user/new`、`user/update`、`user/info`、`user/list` 分页、`key/generate` 带 `user_id` 的请求体。
- 对账函数纯函数测试，覆盖 §6 四种标签。
- 模板渲染冒烟测试：`/users`、`/users/<u>`、瘦身后的 `/gateway`。

**上线验收**
1. 真机验证 user 级 `rpm_limit` 对子令牌执行（§1 未实测项）。
2. weipeng 通过「重新开户」迁移到 user 下，列表显示预算与本月花费。
3. 超额后在 Codex 里看到的报错文案可读（网关返回 `429 budget_exceeded`）。
4. 关户后：令牌立即 401；agent 下次同步机器上 `config.toml` / `models.json` 被清；机器页无该用户绑定。

## 9. 不做的事

- 部门级预算、LiteLLM team。部门只是文本标签。
- 月度 token 配额（LiteLLM 不原生支持，需自建账本）。
- 多管理员身份与「谁做的」审计。
- 硬删除员工（名册与网关 user 都保留历史）。
- agent 侧任何改动。
