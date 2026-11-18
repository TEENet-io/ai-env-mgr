# 公司 Skill 下发与验收

日期：2026-11-13，更新：2026-11-18。目标：2026 年 11 月内完成交付。当前状态：2026-11-18 已上线生产（后台 `v1.5.20-20-gcdcdc0c`，迁移 0032；全员目标 Agent 1.5.21、Codex b44）。生产测试机 hv8uqpity23nkc7 上新建、ZIP 导入、更新、回退及不关闭 Codex 刷新均通过；普通员工只读权限通过。多员工范围隔离、按部门派发与组织变动、后台角色、运行中长任务和独立人员验收待完成。

## 依据与复用

- 11-13 周五纪要第二节：徐州客户希望业务场景直接制作成 skill，配合我方环境使用；梦寒沿用 Agent 下发方式，在 Wei 之前的版本上升级，本月内完成。
- 《全局 Skill 管理 · 需求与实现判断》v0.4：采用机器级目录、设备 API、按员工或部门派发，一期包括制作、试用、派发、版本和审计。
- 已复用 `origin/feature/personal-memory-mvp-agent` 的提交 `fcf6693`，在当前主干上接入该分支，再增加可变业务 skill 清单。旧版个人记忆的固定 skill 同步保持独立；新功能不读写记忆卡片。
- `codex-kiosk` 的 `origin/probe/company-skills` 已记录 Codex 0.153.4 识别机器级 skill 的实测结果。原始 b13 使用已有机器级加载能力；为支持无感刷新，现增加固定版本桌面薄适配，见下文。个人 skill 安装逻辑保持原样。

11-13 纪要第五节要求开发者之外的另一人测试、至少两人认可才上生产。自动化测试不代替这一签字，也不另搭每客户预发环境。

2026-11-16 已按用户要求另外启动 [独立测试环境](公司%20Skill%20独立测试环境.md)，线上后台保留现有版本。测试后台和测试 Agent 通过单独的临时 HTTPS 地址连接，Codex 沿用 b13。

## 使用方式

工作间后台新增 `/skills`，Viewer 和 Security 只读，Operator 和 Admin 可管理。客户工单中心新增 `/customer/skills`，沿用已有客户经理角色；普通员工联系人不能访问公司管理清单和写接口。

2026-11-16 已调整为“Skill 清单 → 单个 Skill 的内容、派发范围、历史版本”，派发结果和操作记录独立展示；具体入口和交互见 [管理界面说明](公司%20Skill%20管理界面.md)。

1. 手动填写业务名称、用途、使用场景和步骤，系统生成 SKILL.md。也可导入我方已制作的 ZIP，ZIP 内直接放 SKILL.md，或以 skill 名称作为唯一外层目录；首次导入自动识别英文标识。同名 Skill 须通过已有条目的“上传新版”更新，避免从新增入口误覆盖。
2. 可附模板、说明和参考资料。新增 skill 时选择一位员工试用；不选择则只保存，不向员工派发。试用对象需明确选择工作间员工，不凭客户联系人邮箱自动推断。
3. 勾选员工或部门并保存。部门按实时员工档案计算，员工转部门后跟随调整，离职和删除员工不再收到内容。
4. 修改生成新的内容版本；已派发的对象同步到该版本。可选择历史版本回退，也可全部撤回，内容历史保留。
5. 页面分别显示后台应有版本、设备最近上报版本、同步状态和最后上报时间。离线设备仍显示等待同步；“已同步”仅指最近一次回报。
6. 公司 skill 的保存、试用、派发、回退、撤回均记录操作身份、时间和变化，客户经理也能看到。通用审计只保存元数据，不复制业务说明和模板正文。

手动制作默认生成 `teenet-` 前缀标识。导入可使用已有公司前缀，名称须为包含短横线的小写英文、数字标识，最多 64 字节。建议统一前缀，减少与个人 skill 重名。

## 下发契约

配置沿用 `/agent/v1/config` 和 `/agent/v1/wait`，通过设备令牌认证，由服务端根据当前绑定员工计算清单。员工未获派发的 skill 内容不会出现在其设备配置里。

`skills` 字段有三种语义：不存在表示旧服务器不支持，新 Agent 保持已有状态；存在但清单为空表示撤回；存在且非空表示该设备完整的应有清单。发布会唤醒长轮询，ETag 仅由该设备实际配置决定，不因另一员工的 skill 改动而变化。

Agent 将内容写到 `%ProgramData%\OpenAI\Codex\skills\<名称>`。顶层 Codex 目录取消继承，SYSTEM 和 Administrators 完全控制，Users 只读、执行；文件继承权限，不递归套用目录权限。保护失败、链接/重解析点、不可信的既有所有者或子目录写权限都会阻止同步并上报。管理状态单独保存在 `%ProgramData%\AIEnvMgr\company-skills\state.json`，仅管理员和 SYSTEM 可读写。

每个版本包含 SHA-256，验证整个清单后才更新。Agent 只接管自己的管理记录；不会覆盖机器目录下同名但未受管的 skill，也不会删除其他目录和个人 `.codex\skills`。内容先写暂存目录，再交换；所有权和待刷新用户先落盘，服务中断后可重试并清理旧备份。

当前实现不再为公司 Skill 新增、更新、回退、撤回或转绑主动停止 Codex。Agent 发布受保护的 `company-skills.json`（位于机器级 Codex 根目录），每次实际变更生成新的随机 generation；桌面适配在现有 app-server 连接调用 `skills/list(forceReload=true)`，通知界面刷新后写入当前用户的 generation 回执。Agent 保留未确认的刷新记录，后续同步重试；若用户未运行 Codex，则当前无需等待客户端刷新。仍使用旧测试 Agent 的电脑会保留原先关闭行为，必须换用新包。

解绑、离职或绑定档案不存在时移除旧的受管内容。网络或数据库读取失败不会当作空清单撤回。`agent.exe prepare-image` 同样清理公司的受管 skill，防止测试模板把上一位员工的业务说明带入克隆机。

当前沿用“一台电脑绑定一位员工”的派发模型。机器级目录对本机所有普通用户可读，不提供多员工共用一台机器时的内容隔离。

## 本次范围与限制

- 一期交付文字说明及模板，不下发可执行脚本，不安装 MCP，不改动模型、令牌和员工个人配置。需要新连接时仍由我方接入现有环境。
- 未实现自动发现业务场景、使用次数、员工反馈、管理员级 hooks；这些按需求草案属于后续阶段。
- 普通员工不能修改公司原件，但 Codex 本身允许个人停用、同名个人 skill 和复制另存；本次不锁个人配置，不承诺防绕开。
- 内容和历史复用 PostgreSQL `settings` 的版本校验与事务；新迁移 `0032_customer_skill_audit` 为通用审计补充 customer 操作身份。清单更新和审计追加同一事务，审计失败则整次修改回滚。
- 限制：最多 64 个 skill，每个最多 64 个版本；单文件 1 MiB、单 skill 4 MiB、单员工应有内容 8 MiB、整个清单和历史序列化后 32 MiB。超过时明确拒绝，不丢弃旧版本。后续体量增长可拆成独立版本表和对象存储。

## 已做验证

本地 `make check` 通过：格式、Linux/Windows vet、默认测试矩阵、Windows 全仓交叉编译。新增和目标 PostgreSQL 测试通过，覆盖：

- 导入 ZIP 的路径穿越、Windows 特殊路径、大小写冲突、链接、脚本和重复文件拒绝；模板中文文件名及空格。
- 试用范围、员工和实时部门派发、版本更新与回退、撤回、离职和解绑，其他员工的配置不变。
- 清单乐观锁、审计失败事务回滚、客户经理与员工权限、CSRF。
- 不覆盖未受管同名目录、失败不撤回有效版本、更新/撤回刷新、失败跨进程重试、转绑仅刷新相关员工、交换中断后的备份清理。
- 管理页模板，以及 1440 和 390 像素宽浏览器检查，无页面横向溢出。

Windows 原生 companyskills、managedskills、agentcore、agent 四组测试由 CI 43825288779 执行通过，包含公司文件及新增刷新清单的只读 ACL；唯一跳过的是需要数据库的 Catalog 子用例，不替代普通员工真机验收。2026-11-16 已在启用 AppLocker 的 Windows 云电脑完成下述首次下发人工验证；普通员工权限和拒绝写入仍待专项验收。

2026-11-16 已修复此前 6 个既有数据库回归用例：

- 3 个渠道测试：Operator 的渠道页是只读页面，没有操作表单。改为从该操作员自己的账号页取得有效 CSRF，继续检查暂停、试调用、健康检测接口均拒绝越权请求。
- 3 个版本库和发布测试：按候选、已验收、稳定的流程准备版本状态。新增检查确保 candidate 和 accepted 均不能设为全局、被拒绝时不修改策略或创建下发任务；只有 stable 才显示全局发布入口并能成功设置全局目标。

本轮 `make check` 和带 `TEST_PG_DSN` 的 `go test -p 1 -count=1 ./...` 均通过。完整数据库回归共 29 个测试包、1064 个测试及子用例通过，0 失败；4 个按运行条件跳过，分别为预览生成、真实 OSS 和两个真实网关测试。`-count=1` 保证本轮实际执行测试。

CI 的 PostgreSQL 检查现已覆盖全仓测试，保留 Windows 原生 skill 和 ACL 验证作业。其余 Windows 云电脑验收与独立人员认可仍待完成。

### 2026-11-16 基本生命周期真机结果

独立测试后台向 `lm0b1jcaty0wblw` 绑定员工 `weipeng` 派发 `teenet-delivery-check` v1。Agent 回报 `applied`，版本 1，内容 SHA-256 为 `0bbe89c2e10ec9a2de9623a089eb196e40e843602fbcdbaaf2dfe256e133b154`。用户使用 b13，在修复本机执行器及下载版 PowerShell 的 AppLocker 拦截后，回传以下技能执行结果：

```text
技能：teenet-delivery-check
版本：v1
校验码：TEENET-SKILL-CHECK-V1-8D42C7
读取文件：C:\ProgramData\OpenAI\Codex\skills\teenet-delivery-check\references\check-marker.txt
```

结果与 v1 ZIP 附件一致，首次导入、员工派发、Agent 写入、b13 加载和附件读取链路通过。本机策略处理见 [独立测试环境](公司%20Skill%20独立测试环境.md)。该账号的策略检查命中管理员规则，本结果不替代普通员工只读 ACL 验收。

随后同一个 Skill 导入 v2。设备于 14:39:19（Asia/Shanghai）回报 `applied`、版本 2，SHA-256 为 `00c21b2695084f097ee5ec51ce561a914ab47862d4812256c073a849126fe5d3`。用户回传 b13 读取结果为版本 `v2`、校验码 `TEENET-SKILL-CHECK-V2-B7359A`，文件仍为上述机器级目录中的 `references\check-marker.txt`。结果与 v2 包一致，版本更新及新附件读取通过。

再选择历史版本 v1 回退。设备于 14:41:22（Asia/Shanghai）回报 `applied`、版本 1，SHA-256 恢复为 v1 的 `0bbe89c2e10ec9a2de9623a089eb196e40e843602fbcdbaaf2dfe256e133b154`。用户回传 b13 同一路径中的版本 `v1`、校验码 `TEENET-SKILL-CHECK-V1-8D42C7`，历史版本回退及附件恢复通过。

最后在后台撤回。设备于 14:45:22（Asia/Shanghai）回报 `applied`、`skills: []`，错误清单为空。用户执行 `Test-Path 'C:\ProgramData\OpenAI\Codex\skills\teenet-delivery-check'` 返回 `False`；Codex 回报当前会话的可用技能列表中不再包含该 Skill，无法执行。结合空清单、目录删除及会话不可用，撤回验证通过。Codex 对“执行测试”回复“未通过”是已撤回技能不能调用的预期结果，不作为派发失败记录；其回复中的目录读取被拒绝也不单独作为新的 ACL 故障证据。

首次下发、更新、回退和撤回四项基本流程已通过。用户随后再次验证：新增 v1 未关闭 Codex，更新 v2 时自动关闭，重开后读取到 v2，当前刷新策略的这两项行为与实现一致。常规无变更轮询稳定性、对其他员工进程的影响、普通员工只读权限及范围隔离仍待专项验证。

旧版刷新体验问题：用户指出更新时直接关闭 Codex 会影响使用。旧版代码用按用户名执行 `taskkill /F` 清除进程缓存，会打断正在进行的工作；行为测试通过不代表该交互已获产品认可。建议普通更新优先无感刷新，无法刷新时明确显示待用户重开；撤回需区分文件移除和已加载会话状态，明确是否要求立即强制生效。官方 App Server 提供 `skills/changed` 通知和 `skills/list` 的 `forceReload`，见 [官方协议](https://learn.chatgpt.com/docs/app-server#skills)，但 b13 对机器级目录的刷新行为以及 Agent 到现有桌面进程的接入尚未验证，不能仅凭协议能力取消缓存处理。这是当时的实现记录；用户随后要求接入自动刷新，当前方案见下节。

进一步核对已有的 `codex-kiosk origin/probe/company-skills` 探测提交 `316f987`：`probe-results/company-skills-42982767578.json` 记录 Windows `codex-cli 0.153.4` 同一 app-server 进程新增公司 Skill 后能列出，删除两个 Skill 并等待 4 秒后，普通列表和新会话目录仍保留旧项；调用 `skills/list(forceReload=true)` 后列表清空，期间仅收到一次 `skills/changed`。这证明同版本内核存在不重启的强制重扫能力，同时自动监听不足以覆盖该探测中的删除事件。该用例没有验证更新 SKILL.md 正文，也没有通过 b13 桌面连接正在运行的 app-server；后续应验证 Agent 原子交换目录、正文及附件更新、撤回和桌面刷新联动。不得启动一个新 app-server 刷新后，就声称现有桌面进程的缓存已刷新；已进入旧对话的指令也不能承诺被追溯替换。该探测作为后续接入的依据；当前实现已移除公司 Skill 同步的关闭策略。

## 不关闭 Codex 的自动刷新（2026-11-16）

用户授权接入后，变更分为 Agent 同步与桌面刷新两层。该桌面直接使用原生 app-server，不经过 Web Gateway，因此仅在锁定的桌面 `26.901.51231` 原生 `sendInternalRequest` 边界接入薄适配。b13 可安装一次性覆盖适配，不需要改模型配置或升级线上工作间后台。具体契约和回退见 codex-kiosk 的 `docs/company-skills-live-refresh.md`。

- 桌面每 2 秒检查 Agent 发布的 generation，并在列技能、新建/恢复/复制对话及发送消息前补查；重扫使用正在运行的同一连接，不另起内核、不发送中断指令。
- 回执位于 `%USERPROFILE%\AppData\Local\TEENet\company-skills-refresh.json`。只有匹配用户和本次 generation 的回执才确认；旧回执、损坏文件、链接及超大文件不确认。回执是状态遥测，不是安全授权或强撤回证明。
- 后台区分“文件已同步，等待 Codex 刷新”和“文件已同步，当前无需等待刷新（最近上报）”。没有回执时保留 `refresh_pending`，不会降级为强行关闭进程。
- 已进入旧对话的指令不追溯替换；验证新版应新建对话。已有任务不会被同步逻辑主动取消，但文件仍按版本更新，不提供任务级文件快照。
- 第一次安装桌面适配需手动退出一次 Codex，之后才使用新刷新逻辑。必须先停止旧测试 Agent，防止旧二进制仍主动关闭窗口。正式安装流程未全量发布。

已完成本地刷新/回执/失败重试测试、完整 PostgreSQL 回归、Node 回归及企业包重建验证；固定 b8 载体的原生边界覆盖和逐字节回退验证通过（8853 个其他文件内容保持）。Windows 原生桌面连接探测 CI 43825935754 通过，真实内核 `0.153.4` 在同一 PID 完成新增、正文/描述更新、回退、撤回与回执，完整桌面/Gateway 回归也通过。用户 b13 已完成本轮新包的 v3 正文及附件读取，并明确确认更新期间窗口全程没有关闭；不关闭窗口更新在该机的人工验收通过，详见下文。

新增 v3 测试包同时修改 SKILL.md 的描述、正文与附件。新对话应输出附件校验码 `TEENET-SKILL-CHECK-V3-6C91E2` 和仅存在于正文的 `TEENET-BODY-V3-91A6F0`。原 v1/v2 只改附件，不足以证明指令缓存刷新。下载与操作见独立测试环境文档。

### 用户 b13 真机：v3 正文和附件读取通过

2026-11-16，用户在 `C:\Tools\Codex` 安装刷新适配后回传：

```text
技能：teenet-delivery-check
版本：v3
校验码：TEENET-SKILL-CHECK-V3-6C91E2
指令校验：TEENET-BODY-V3-91A6F0
读取文件：C:\ProgramData\OpenAI\Codex\skills\teenet-delivery-check\references\check-marker.txt
```

设备 `lm0b1jcaty0wblw`、员工 `weipeng` 于 15:59:44（Asia/Shanghai）最近回报 `applied`、版本 3，SHA256 为 `b58b471ed98b6585a22aa8013c6cbfec27a6feac8bf3922b145182511e122ce4`，generation 为 `855e42e18ab703dd2bdb4caa5e886517`，错误清单为空。

输出同时匹配仅存在于 SKILL.md 正文的校验值与附件校验值，确认 v3 正文执行和机器级附件读取通过。用户随后明确确认：Codex 窗口全程保持打开，后台更新 v3 后新建对话执行，未重开程序；本机“不关闭窗口更新”人工验收通过。用户随后按保持窗口打开的步骤完成新适配下的人工回退与撤回，结果见下节；真实长任务保持仍待验证。此前旧 Agent 的生命周期结果单独保留。

### 用户 b13 真机：不关闭窗口回退 v2、撤回通过

2026-11-16，用户按保持 Codex 窗口打开、每次新建对话的步骤验证回退和撤回，并明确回复“都可以了”。回退 v2 的实际输出为：

```text
技能：teenet-delivery-check
版本：v2
校验码：TEENET-SKILL-CHECK-V2-B7359A
读取文件：C:\ProgramData\OpenAI\Codex\skills\teenet-delivery-check\references\check-marker.txt
```

该结果恢复为旧版四行输出，不再包含 v3 新增的“指令校验”行，附件版本和校验码也恢复为 v2。回退后的正文与附件一致，人工回退验收通过。

撤回后，用户回传“当前可用技能列表中没有 teenet-delivery-check，无法执行本次公司技能下发测试。未沿用历史版本或校验码。”同机于 16:04:28（Asia/Shanghai）最近回报 `applied`、`skills: []`，generation 为 `2c57979a12ae06a1b10013c6b064ca96`，错误清单为空。结合 Agent 空清单与新对话不可调用，人工撤回验收通过。

本机新 Agent 与 b13 刷新适配的无感更新、回退、撤回基本生命周期验收完成；未据此将普通员工只读权限、多员工范围隔离、真实运行中长任务或生产上线标为通过。生产后台及正式 Agent 未升级。

### 普通员工只读权限真机结果（2026-11-18）

测试环境已迁到 ec2-new，见独立测试环境文档。设备 `lm0b1jcaty0wblw` 已派发 `teenet-delivery-check` v3（回报 `applied`，内容 SHA256 `b58b471ed98b6585a22aa8013c6cbfec27a6feac8bf3922b145182511e122ce4`）。在同一台云电脑新建本地标准用户 `skilltest`（不在管理员组），用 `Start-Process powershell -Credential` 以该用户打开 PowerShell 逐项检查：

| 操作 | 结果 | 结论 |
|---|---|---|
| 读取 `SKILL.md`、`references\check-marker.txt` | 成功，附件为 `version=v3`、`TEENET-SKILL-CHECK-V3-6C91E2` | 通过 |
| `Add-Content` 追加 `SKILL.md` | 访问被拒绝 | 通过 |
| 在 Skill 目录新建 `new.txt` | 访问被拒绝 | 通过 |
| 在机器级 `skills` 下新建子目录 `fake` | 访问被拒绝 | 通过 |
| 改名 Skill 目录 | 访问被拒绝 | 通过 |
| `Remove-Item -Recurse -Force` 删除 Skill 目录 | 各文件及目录均拒绝 | 通过 |
| 新建个人 skill `C:\Users\skilltest\.codex\skills\my-own` | 成功 | 通过 |

检查后设备于 13:02:26（Asia/Shanghai）回报的 SHA256 不变。Windows PowerShell 5.1 的 `Get-Content` 默认按 ANSI 解码无 BOM 的 UTF-8，`description` 中文显示为乱码，属于显示问题，不是文件损坏。普通员工只读一项通过；该用户未接入 Codex，未验证其会话内的列表和调用。

## 生产上线与验收（2026-11-18）

### 部署内容

生产后台在 ali0 运行，原版本为 `feat/oidc-aliyun-sms` 的 `4bafcd8`（迁移 24），不是 main。上线版本在 `4bafcd8` 之上只合入本功能，分支 `release/prod-4bafcd8-skills`，未带入该分支后续提交。

| 时间（北京时间） | 内容 | 版本 / 位置 |
|---|---|---|
| 15:49 | 后台上线公司 Skill，执行迁移 `0032_customer_skill_audit` | `v1.5.20-17-ge301c9c` |
| 16:30 | 后台修复：上传框拖放、版本列表按版本号排序 | `v1.5.20-20-gcdcdc0c` |
| 15:12 | Agent 1.5.20 上传 OSS，不含个人记忆同步 | `agent_workdir/_agent/1.5.20/agent.exe`，SHA256 `5c39fa03…4713fd9` |
| 16:32 | Agent 1.5.21：等待刷新期间每 10 秒复查 Codex 确认 | `agent_workdir/_agent/1.5.21/agent.exe`，SHA256 `642a063b…c2cf970` |
| 15:12 | Codex 企业版 b44 = b13 + 无感刷新适配；b14–b43 已被个人记忆试验包占用 | `agent_workdir/_codex/codex-setup-26.901.51231-b44.exe`，SHA256 `214b0157…873c452a` |
| 16:52 | 全员目标改为 Agent 1.5.21、Codex b44 | 后台「软件安装」 |

迁移编号用 0032，因为 `feat/oidc-aliyun-sms` 已占用 0024–0031；迁移器拒绝数据库中存在而程序未携带的迁移，main 已同步改为 0032，该分支部署前须合入 main。上线前在生产表结构副本上演练：0001–0024 校验和一致，仅 0032 待执行，down/up 往返正常。生产审计表现有 `actor_type` 仅 admin、device。数据库备份 `/var/backups/ai-env-mgr/pre-skills-20261118T074854.dump`，旧程序保留为 `/usr/local/bin/*.prev-*`。

### 生产真机结果

测试机 `hv8uqpity23nkc7`，员工 weipeng，Agent 1.5.20 → 1.5.21，Codex b43 → b44。b44 安装后输入框下的个人记忆按钮消失；Codex 执行命令正常，未被 AppLocker 拦截；模型登录保留。

| 场景 | 操作 | 结果 |
|---|---|---|
| 旧后台兼容 | 1.5.20 连接未升级的生产后台 | 通过：设备配置无 Skill 字段时跳过同步，上报不带新字段 |
| 首次派发 | `teenet-delivery-check` v2 派发给 weipeng | 通过：新对话读取 v2 与 `TEENET-SKILL-CHECK-V2-B7359A` |
| 不关窗口更新 | 更新到 v3，Codex 保持打开 | 通过：读取 v3 附件与正文校验 `TEENET-BODY-V3-91A6F0` |
| 刷新确认 | 1.5.20 下状态停在「等待 Codex 刷新」 | 发现问题：Codex 确认比 Agent 检查晚约 1 秒，须等下一次完整同步（最长 30 分钟）；1.5.21 修复 |
| 回退 | 1.5.21 下回退到 v2（16:42:42） | 通过：16:43 前后台显示已同步 |
| ZIP 导入新 Skill | 拖放导入 `teenet-meeting-minutes` v1（模板 `.md` + 术语表 `.csv`） | 通过：按模板输出，术语按表替换，正文 `TEENET-MIN-BODY-V1-4C27`、模板 `TEENET-MIN-TPL-V1-A913` |
| 上传新版 | 同一 Skill 上传 v2 | 通过：新增「下一次会议」一节，正文 `TEENET-MIN-BODY-V2-7E50`、模板 `TEENET-MIN-TPL-V2-D681` |
| 自动选用 | 不写 `$teenet-meeting-minutes`，只说「整理一下：…」 | 通过：Codex 按 Skill 说明自动选中 |
| 普通员工账号 | lm0b1jcaty0wblw（员工 menghan，非管理员）升级到 Agent 1.5.21、Codex b44 后打开 Codex | 发现问题：被 AppLocker 拦截（见下文「AppLocker 放行目录丢失」），恢复放行目录后可正常打开 |
| 其他模型 | Codex 选 Claude（`drop_params` 生效后）调用 `teenet-meeting-minutes` | 发现问题：PowerShell 输出乱码，模型猜错术语并漏掉 v2 章节（见下文「中文乱码」）；Agent 1.5.22 修复后同一请求输出正确 |

17:37 设备回报 `applied`，两个 Skill 均为 v2。

Agent 1.5.22 下，lm0b 用 Claude 重复同一请求（19:19）：后台→AI 环境管理后台、云电脑→无影云电脑、agent/skill→Agent/Skill，包含 v2 的「下一次会议」，正文 `TEENET-MIN-BODY-V2-7E50`、模板 `TEENET-MIN-TPL-V2-D681`；原文无具体日期，按规则写「未注明」。

### 上线过程中发现的问题

- 11-13 全员 Agent 目标被设为 1.5.9，设备由 1.5.19 降级。原因是版本列表按登记时间排列，生产库 11-09 重建时按字符串顺序批量登记，1.5.9 排在最前。已改为按版本号排序。
- main 曾因 Skill 分支基于个人记忆 Agent 分支而带入 `1e0cb7d`（个人记忆同步，无后台开关）。已在 main 上回退，个人记忆仍在 `feature/personal-memory-mvp-agent`。
- Codex 选 Claude 或 Gemini 时，请求带 `verbosity` 与 `prompt_cache_key`，LiteLLM 报 `UnsupportedParamsError`。已提 [litellm-config#18](https://github.com/TEENet-io/litellm-config/pull/18) 为这些模型开启 `drop_params`。Gemini 另因 GCP 项目 `coral-matrix-falcon-7821` 于 11-18 13:50–17:11 间停用结算（`BILLING_DISABLED`）而不可用，需恢复结算。
- **AppLocker 放行目录丢失**：11-09 重建生产库时，策略中的 `appLockerAllowPaths`（`C:\tools\Codex\*`、VS Code、WeChat、Weixin）和 `appLockerMode`、`baidu.com` 屏蔽、`collectSince` 未带入，同步间隔由 1 分钟变为 30 分钟。11-13 发布策略后 Agent 删除了这些放行规则，普通员工账号运行 `C:\tools\Codex` 下的 Codex 被拦截（管理员账号不受影响，故此前未被发现）。依据为 11-09 重建前备份 `aienv-2026-11-09.sql.gz` 中的第 49 版策略。18:27 在「封禁策略」页恢复 4 个放行目录（策略第 7 版）后 menghan 可正常打开。`appLockerMode`、`baidu.com`、同步间隔尚未恢复。同日 lm0b 有 1004 次无影组件（`Program Files (x86)\Alibaba\Wuying\*`）被拦记录，现有 `%PROGRAMFILES%\*` 规则应已覆盖，原因待查。
- **中文乱码**：中文 Windows 控制台为 GBK（代码页 936），PowerShell 以 `-Encoding UTF8` 读取 UTF-8 文件后仍以 GBK 输出，Codex 收到乱码。Claude 隐式调用 Skill 时据此猜错术语、漏掉章节，并在结尾声明内容未经核实。Agent 1.5.22（`agent_workdir/_agent/1.5.22/agent.exe`，SHA256 `627cf07f…53af24db`）在绑定员工的 `%USERPROFILE%\.codex\AGENTS.md` 中维护受管说明块，要求先设置 `[Console]::OutputEncoding` 与 `$OutputEncoding` 为 UTF-8、不据乱码猜测；块外内容不动，其他用户与解绑时移除。
- lm0b 正式 Agent 服务自 11-16 起处于停止状态（当日测试时执行过 `Stop-Service`），11-18 重新设为自动启动并启动后恢复上报。
- 仍有 2 台设备离线（最后在线 11-16 至 11-17），开机后升级到全员目标版本；安装 Codex 时会关闭正在运行的 Codex。

## 独立人员验收与上线

在按需启动的测试环境和一台测试云电脑执行，记录版本、SHA-256、测试人、时间、结果。不要直接指定全局 Agent 版本。

1. 先启动新后台，确认迁移完成；旧 Agent 忽略新字段，不会自动收到新功能。给一台测试电脑部署新 Agent，通过现有定向下发流程核对版本。
2. 以普通员工登录，打开 Codex；后台新建业务 skill，先只给测试员工。确认其他员工的设备配置没有该 skill；测试电脑的新对话可列出并调用，参考模板可读取。
3. 普通员工尝试修改、追加文件、新建公司 skill 子目录、改名和删除，均应失败；确认员工仍能读取公司说明、创建个人 skill 和使用既有个人记忆功能。
4. 发布第二版、回退第一版、全部撤回，分别核对磁盘、设备回报和不退出 Codex 时新对话中的技能列表。保持 Codex 窗口及正在运行的任务，确认不再因 Skill 同步关闭；模拟刷新失败或 Agent 服务重启，确认保留待刷新并重试。
5. 测试按部门派发、转部门、解绑、转绑和离职。离线时页面不能提前显示撤回成功；恢复网络后再核对文件和回报。
6. 检查公司目录里未受管的 skill 和员工个人 skill 保留；检查 `prepare-image` 后自己的公司 skill 和旧员工状态已清空。
7. 客户经理可以管理及查看操作记录；员工联系人不能越权，后台 Viewer 只能读。检查两人同时修改不会静默覆盖。
8. 开发者与另一位独立测试者认可后，按上线 SOP 再安排生产定向验证、扩大范围和上线观察。当前不把测试用户的长期使用电脑自动当作这台测试机。

回退前先撤回新功能的派发，等待在线设备回报空清单并完成缓存刷新，再回退 Agent；旧 Agent 不会清理本功能创建的目录。保留数据库内容与审计，不删除历史。若 customer 审计已产生，向下执行 0032 会拒绝恢复旧限制，不应重写审计来强行降级。
