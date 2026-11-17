# 新公司开通与员工开户 SOP

版本：2026-11-06
负责人：Wei
给谁看：网关运维、后台管理员
管哪些东西：多家公司共用一个 LiteLLM 网关时，新公司的建立、员工开户和 key 派生。

## 决定

- 免费版 LiteLLM 的超级 key 能操作网关上所有公司的数据，无法区分客户。所以各公司后台不持有超级 key，改用自研小服务 `tenant-admin` 派生 key。
- LiteLLM 本身不改。`tenant-admin` 的 4100 端口只绑定网关机本机，外部只能经 `https://litellm.teenet.app/tenant-admin/` 访问，并且要带租户令牌。
- 暂不购买 LiteLLM 付费版。以后如果这套流程太麻烦，再评估。

## 先认识几个词

| 词 | 意思 |
|---|---|
| 公司（team） | LiteLLM 用 team 区分公司。team ID 是 2–32 位小写字母、数字或下划线，定了不能改。下文以 `wewe` 为例。 |
| 后台 | 每家公司一套的管理网站，服务名 `ai-env-mgr-console`。 |
| 超级 key | 网关的管理员密钥。只有 `tenant-admin` 和我们的网关运维持有。 |
| 租户令牌 | `ta-` 开头，每家公司的后台一个，只能操作本公司的员工和 key。 |
| 员工 key | LiteLLM 的 virtual key，员工用 Codex 调模型时使用。由后台派生，自动下发到云电脑。 |

三者的关系：

```text
超级 key ── 只在 tenant-admin 和网关运维手里
租户令牌 ── 后台拿它调 tenant-admin，只能管本公司
员工 key ── 后台经 tenant-admin 派生，员工调模型时直连 LiteLLM
```

员工调模型不经过 `tenant-admin`。它挂了，员工照常使用，只是后台暂时不能开户和改预算。

## 1. 开通真实客户前要解决的问题

目前只有 bippo 一家在用，第二家公司的完整流程还没走过。以下问题来自 2026-11-06 的代码核对，解决前第二家公司无法完全隔离。

| 编号 | 问题 | 后果 | 位置 |
|---|---|---|---|
| B1 | 用量汇总不按公司过滤 | 用量页会混入其他公司的员工和花费 | `go/internal/worker/usage.go` 的 `usageQuery` |
| B2 | 日志页会显示所有 `emp-*` 旧记录 | 新公司能看到 bippo 迁移前的调用记录 | `go/internal/adminweb/logs.go` 的 `filterQuery` |
| B3 | 各公司共用 SLS 日志库 `audit`，后台的凭证能读整个库 | 持有后台服务器的人能读其他公司的日志 | `ops/sls/`、`console.env` |
| B4 | OSS 存储桶写死为 `ai-collect-sg` | 所有公司的后台读写同一个桶 | `go/cmd/admin/credentials.go` |
| B5 | Agent 的后台地址在构建时写死 | 新公司的云电脑连不上自己的后台，要单独出一版 Agent | `.github/workflows/release.yml` 的 `AGENT_CONSOLE_URL` |

下面三项是 2026-11-05 记录的待办，开通前先核对是否已完成：

- 两个仓库的代码已 push。
- `tenant-admin/tenants.json` 和 `tenant-admin.env` 已备份。
- `https://litellm.teenet.app/tenant-admin/healthz` 已接入告警。

## 2. 开通新公司

### 第 1 步：确认信息

确认公司名、team ID、后台域名、公司联系人，以及后台由谁部署。

### 第 2 步：准备后台环境

按 `数据库模式部署.md` 的“环境变量”和“第一次启动”准备服务器、数据库、主密钥和域名。这一步先不启动后台：后台启动时要用租户令牌确认自己属于哪家公司，所以令牌要先签发。

如果后台要把日志送到 SLS，Logtail 的地址要和服务器所在区域匹配。内网地址不能跨区域，2026-10-23 就因此中断过两周。

### 第 3 步：建 team 并签发租户令牌

SSH 登录网关机（`ec2-user@13.229.202.110`），运行：

```bash
docker exec tenant-admin tenant-admin token new -team wewe
```

这条命令在 LiteLLM 上建好 team，并打印 team、令牌 ID 和令牌。**令牌只显示这一次。**

### 第 4 步：配置并启动后台

| 谁部署后台 | 令牌怎么交付 |
|---|---|
| 我们 | 直接写进后台服务器的 `/etc/ai-env-mgr/console.env`，不经过聊天或邮件 |
| 客户 | 用一次性加密链接交给对方技术负责人 |

在 `console.env`（权限 600）里设置：

```bash
AIENVMGR_GATEWAY_URL=https://litellm.teenet.app
AIENVMGR_GATEWAY_ADMIN_URL=https://litellm.teenet.app/tenant-admin
AIENVMGR_GATEWAY_ADMIN_KEY=<租户令牌>
AIENVMGR_WORKER=on
AIENVMGR_TOOL_URL=<这家公司定制工具的地址>
```

`AIENVMGR_WORKER` 必须是 `on`，否则开户任务只排队、不执行。`AIENVMGR_TOOL_URL` 可以不填，填了后台和客户工单中心才会显示“定製工具”链接。

启动后台后，访问 `/healthz` 应返回 `ok`。

### 第 5 步：验证隔离

在后台服务器上，把令牌放进环境变量 `TOKEN`，再运行：

```bash
A=https://litellm.teenet.app/tenant-admin
curl -sS -H "Authorization: Bearer $TOKEN" $A/whoami
curl -sS -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $TOKEN" "$A/team/info?team_id=bippo"
curl -sS -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $TOKEN" "$A/user/info?user_id=bippo-emp-test"
curl -sS -m 5 http://13.229.202.110:4100/
```

| 命令 | 应该看到 |
|---|---|
| 第 1 条 | `{"team":"wewe"}` |
| 第 2、3 条 | `403` |
| 第 4 条 | 连接失败，说明 4100 端口没有暴露 |

再打开后台的“设置”页，“组织”应只读显示 `wewe`。

### 第 6 步：建账号

1. 按 `数据库模式部署.md` 用 `first-admin.txt` 登录 `/ops/login`，绑定验证器，修改密码，然后删除这个文件。
2. 在 `/admins` 给每位运维人员开账号，一人一个，用户名写本人名字，不用共用账号。
3. 客户需要提工单时，在工单页“添加客户联系人”。客户从 `/customer/login` 登录。

### 第 7 步：走一遍测试员工

按第 3 节开一个测试员工，确认能开 key、能改模型、能关户，然后关户。

### 第 8 步：登记和备份

1. 在 `gateway-litellm/开通记录.md`（首次开通时新建）加一行：公司、team ID、后台地址、令牌 ID、签发日期、负责人。**不记令牌明文。**
2. 备份网关机上 `~/gateway-litellm/tenant-admin/tenants.json`。

## 3. 员工开户和 key 派生

员工 key 全部由后台派生，不在 LiteLLM 管理界面手工开。手工开的 key 后台不知道，无法下发和吊销。

1. **准备云电脑。** 在无影控制台用公司镜像开云电脑，建 Windows 标准用户，见 `操作手册.md` §4.2 第 1–3 步。
2. **开户。** 在后台“员工账号”页填 Windows 用户、邮箱、姓名、部门、月预算和可用模型，点“开户并下发配置”。
3. **系统派生 key。** 后台的 Worker 自动完成以下动作，不用手工操作：
   - 建网关用户 `wewe-emp-<Windows 用户>`，并加入 team。
   - 派生员工 key，别名是 `wewe-emp-<Windows 用户>-<账号 ID 前 8 位>-e<版本号>`。
   - 把 key 加密存进数据库。
4. **确认任务成功。** 在“告警 › 后台任务”确认没有失败的任务。400 类错误不会重试，要人工处理。
5. **绑定机器。** 机器开机后出现在“总览”页，状态为“未分配”，把它绑定给员工。Agent 下一次同步时写入 key。
6. **验证。** 让员工在 Codex 里发一句话，确认模型有回复，再到日志页确认有这名员工的调用记录。

开户到第 6 步通过才算完成。任务显示“已提交”不算。

## 4. key 的日常管理

| 要做的事 | 怎么做 | 说明 |
|---|---|---|
| 改预算、改模型 | 后台员工详情页 | 预算按员工算，公司后台不能设 team 预算 |
| 员工 key 泄露 | 员工详情页点“重发” | 旧 key 吊销，新 key 自动下发 |
| 员工离职 | 员工详情页关户 | key 吊销，见 `操作手册.md` §4.4 |
| 公司停用 | 在后台逐个关户，再暂停公司 | 只暂停公司不会停掉员工 key，员工仍能调模型 |

租户令牌的查看、轮换和吊销，公司的暂停，以及超级 key 的轮换，见 `tenant-admin使用SOP.md`。

## 5. 常见故障

`tenant-admin` 自身和令牌相关的故障（401、403、429 等）见 `tenant-admin使用SOP.md` 第 4 节。

| 现象 | 原因 | 处理 |
|---|---|---|
| 开户任务一直排队 | `AIENVMGR_WORKER` 不是 `on` | 改配置并重启后台 |
| “渠道”页试用和健康检查失败 | 这两项不开放给公司后台 | 由网关运维执行 |
| 删除账号时旧用户 `emp-*` 删不掉 | `tenant-admin` 不放行没有公司前缀的用户 | 由网关运维清理 |

## 6. 执行记录模板

```text
事务：开通新公司 / 员工开户 / 令牌轮换
公司和 team ID：
执行人 / 复核人：
开始与结束时间：
令牌 ID（不写明文）：
后台任务 ID：
验证证据：
未完成项 / 负责人 / 截止时间：
```

## 相关文档

- 小服务的使用：`docs/tenant-admin使用SOP.md`
- 设计：`docs/superpowers/specs/2026-11-05-tenant-admin-proxy-design.md`
- 部署：`docs/数据库模式部署.md`，网关仓库 `gateway-litellm/README.md`
- 员工入职和离职：`docs/操作手册.md` §4.2、§4.4
