自动构建的二进制。

## 本版更新（v1.2.3，相对 v1.1.0 新增）

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
