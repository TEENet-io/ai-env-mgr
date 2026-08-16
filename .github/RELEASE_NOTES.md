自动构建的二进制。

## 本版更新（v1.2.5）

**Web 控制台（新）**

`admin web` 启动浏览器控制台，功能与 TUI 菜单一一对应，命令行保留可用。

- **凭证只在会话内存里**：用 OSS AccessKey 登录，不写磁盘、不进 cookie、不进日志；cookie 里只有一个随机 ID。重启服务即全体登出——这是刻意的，主机上没有任何静态凭证可供翻找。
- **非回环地址无证书拒绝启动**：登录表单提交的凭证能往 `_agent/` 推二进制，而每台机器都会执行它，明文传输等于直接送人。反向代理终结 TLS 时加 `--behind-proxy`，且**只放宽私有地址**，公网地址仍强制要证书。
- **高危操作需要打字确认**：发布 agent / Codex、注销机器要把版本号或主机名原样再输一遍（像 GitHub 删仓库那样），并写审计日志（记真实客户端 IP，不记 token 和凭证）。
- 写操作全部 POST + CSRF 令牌 + 重定向，刷新不会重放；GET 落到写路由什么也不做。
- 无 JavaScript，CSP 的 `script-src` 为空；页面自包含，不引 CDN。深色模式跟随系统。
- 部署见 [Web控制台部署.md](../docs/Web控制台部署.md)。

**Codex 桌面版分发（新）**

- `admin codex publish|rollout|cancel|status`：管理员发布重打包的 Codex，agent 下载、校验 SHA-256、静默安装并上报实际装到的版本。
- **灰度发布**：`--rollout` 默认只发 **10%**。这个包是对上游 Codex 的重打包，补丁会随上游版本漂移，CI 绿只证明补丁应用成功，**不证明 ChatGPT 入口真的消失**——那只有 Windows 真机能验。
- **按相等比对版本，不是"只升不降"**：把目标改回旧版本就是回滚。
- agent 侧三条保护：校验和不符**拒绝安装**；同一版本**只尝试一次**（失败不会每周期重下 700 MB）；Codex 正在运行或磁盘不足则**推迟**，不把应用从员工手里抽走。
- 安装用 `/DIR=` 钉在 Codex 现有位置（镜像里是 `C:\tools\Codex`，可用 `AIENVMGR_CODEX_ROOT` 覆盖），不会把应用挪走。
- 700 MB 安装包**流式下载到磁盘**，不整包进内存——云电脑只有 4–8 GB，为了给它送更新而把它撑垮是本末倒置。
- 设计见 [Codex分发方案.md](../docs/Codex分发方案.md)。

**修正**

- **休眠上报的结果现在会记录**。原来上传成功与否被直接丢弃，一台显示"离线"的机器事后无从解释：Windows 没通知，和通知了但传不出去，看起来一模一样，而两者的解法完全不同。日志能熬过休眠，醒来第一次同步就上传。
- **Web 端的机器状态与命令行完全一致**。此前把 9 种状态压成 6 种，还把"静默 10 分钟"标成了"失联"（命令行叫 `OFFLINE`，是正常的下班关机），而**"休眠中"和"已关机"根本不显示**——区分休眠与崩溃这件事在 Web 上等于没做。判定规则已抽到 `admincore.Health`，两个前端共用。
- **OAuth 回调解析两边共用一份**。Web 端原本自己写了一套，结果用 Codex 的回调解析去处理 Claude 的 `code#state`，**Claude 代登录在 Web 上完全不可用**。
- 修掉一个约 1% 概率偶发的测试。

## 历史：v1.2.4（相对 v1.1.0 新增）

**本次 v1.2.4 新增**
- **会话采集改成 machine-wide**：一台机器上每个真人账户（排除 Administrator/系统）各采到 `data_collect/{用户}/`，**不再只采绑定那一个**——共享云电脑上多人用 AI 时每个人都会被采到。`collect stat` 也改成按 OSS 里实际有数据的用户列（含不在名册的账户）。
- **去掉 `EXTRA USERS` 状态**：一台机器有多个用户是合法的、不再报警，`LOCAL USERS` 列照旧显示都有谁。
- **admin 输出上色**：`status` / `machine list` 的 STATE 列按严重度着色（🟢OK / 🩶离开 / 🟡待分配 / 🔴要排查），`agent status` 表格化。仅终端内生效，管道/重定向保持纯文本，`NO_COLOR` 可关，Windows 自动开 VT。


**agent 侧**
- **休眠/停止上报（区分休眠与崩溃）**：云电脑休眠时会像 agent 崩溃一样静默。现在 agent 在**休眠前**上报 `suspend`、在**被停止/关机**时上报 `stopped`；**崩溃收不到通知、留不下标记**——这正是区分点。对应 `admin status` 的 STATE 分级：`SLEEPING` / `STOPPED` / `OFFLINE`（几小时内，预期）/ `STALE`（>3 天，或说要睡却没醒 → 要排查）。每晚休眠的机器不再被误报为故障。
- **关机可靠上报（PreShutdown）**：普通 `SHUTDOWN` 通知来得太晚（网络已在拆，SCM 报 "shutdown in progress"），关机时 `stopped` 常常发不出去。改为注册 **PreShutdown**，在系统拆服务/断网**之前**就收到通知（窗口约 180 秒），关机/重启也能大概率留下标记。各场景可靠性：停服务=可靠；关机/重启=大概率；真·休眠挂起=尽力而为；强制断电=无从上报（靠 OFFLINE/STALE 兜底）。
- **自更新**：`admin agent publish <exe> --version <v>` 一键滚动全体；agent 每周期比对版本 → 下载 `_agent/agent.exe` → **校验 SHA-256** → 换文件（旧的留 `agent.exe.old`）→ 重启服务。校验不过不换;同一目标只试一次不会反复重启;`admin agent cancel` 是急停。
- `admin agent publish` 支持 **`--url <github-release-url>`**：admin 直接从 release 下载 agent.exe 再推到 OSS（私有仓库用 `--token`/`GITHUB_TOKEN`）。
- **AGENT 版本列**：`admin status` 增加 AGENT 列，升级/自更新后能看谁还停在旧版。
- **心跳**：每 1 分钟刷新一次"最近露面",`admin status` 的 `LAST SYNC` 变成实时,配置同步间隔照旧。
- **日志上报 OSS**：每个完整同步周期把日志尾部（约 64KB）推到 `_logs/{主机名}.log`,管理员用 `admin log <主机名>` 远程看,不用上机器。

**admin 侧**
- **TUI**：无参数或 `admin tui` 进菜单式界面,进门只输一次 AK/SK（不落盘）,之后选数字执行。
- **bucket/endpoint 已固化进 admin**：release 版启动**只问 AK/SK**（bucket=`ai-collect-sg`、endpoint 已编入,公网 `oss-ap-southeast-1.aliyuncs.com`），不再问 bucket/endpoint,也不会误用默认地域。AK/SK 仍不落盘。
- **存活判定收紧到 10 分钟**：agent 每分钟心跳,静默超 **10 分钟**即显示 `OFFLINE`（旧版 2 小时窗口太宽,停机/关机的机器会长时间仍显示 `OK`）。
- **`machine forget <主机名>`**：彻底删除已下线机器（绑定 + 状态上报一起删,从 `admin status` 消失）。
- **`admin log <主机名>`**：读机器上传的日志。
- **`machine list` 增加 STATE 列**：绑定名册里直接显示每台机器的存活状态（OK / STOPPED / SLEEPING / OFFLINE / STALE / NO REPORT），关机上报后这里也能看到 STOPPED，不用切到 `admin status`。

**安装脚本（`scripts/01-Install-AITools.ps1`）**
- Claude Code 改走 **npm**（镜像无 winget / claude.ai 被 Cloudflare 挡时也能装;没 Node 会自动装 Node LTS）。
- ChatGPT 许可证缺失时**自动下载**;坏包会重新下。
- 给全用户桌面加 **Claude Code / ChatGPT 图标**（公共桌面,含未来新用户,用应用自己的图标）。
- `-AllExistingUsers`：自动给机器上**已存在**的全部用户装 ChatGPT GUI。

> ⚠️ **部署前必须更新 agent 的 RAM 策略**（`dist/ram-policy-agent.json` 已含,OSS 上现有子账户要手动补）：
> - `_agent/*` 的 **GetObject**（自更新读新二进制）
> - `_logs/*` 的 **PutObject**（上传日志）
> 不补这两条,自更新和日志上报会失败（其他功能不受影响）。

## 产物

| 文件 | 平台 | 凭据 |
|---|---|---|
| `agent.exe` | windows/amd64 | **已注入受限密钥**（来自仓库 Secrets），`config=built-in`,可直接进镜像 |
| `admin-linux-amd64` | linux/amd64 | **无密钥**,运行时交互输入 |
| `admin-windows-amd64.exe` | windows/amd64 | 同上 |
| `admin-darwin-arm64` | macOS (Apple Silicon) | 同上 |

## admin 怎么用凭据

`admin` **不含任何密钥**。在终端里直接运行(如 `admin status`),没有内置凭据、也没有 `admin.config.json` 时,它会**提示你输入** bucket / endpoint / AccessKeyId / AccessKeySecret(Secret 输入**不回显**),然后**当场向 OSS 验证**:

- 密钥错(`InvalidAccessKeyId`/`SignatureDoesNotMatch`)→ 提示重输;
- 密钥对但权限不足(`AccessDenied`)→ 提示去补 RAM 授权;
- 通过 → 继续执行命令。

**输入的凭据不写磁盘**,每次运行都要重新输入(这是刻意的:管理员那把是**全桶读写**密钥,不落盘最稳妥)。非终端环境(管道/CI)不会卡输入,而是直接报"未配置凭据"。

## 安全说明

- `agent.exe` 里含**受限**密钥的明文(`strings` 可见)——这是既有设计,它本来就要分发到每台云电脑;真正锁死损失的是该密钥的 RAM 策略。
- `admin` 的**读写**密钥**不在任何发布物里**,只在你运行时手动输入。
- 请确保本仓库为 **private**。

## 校验

`SHA256SUMS.txt` 内含全部二进制的 SHA-256。版本号已通过 `-X main.version=<tag>` 编入,`version` 子命令可查。
