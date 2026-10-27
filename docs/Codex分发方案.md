# Codex 分发方案

目标：把我们重新打包过的 Codex 桌面版（[TEENet-io/codex-kiosk](https://github.com/TEENet-io/codex-kiosk)）装到无影云电脑上；上游出新版后，持续把新版推下去。

## 先认识几个词

| 词 | 意思 |
|---|---|
| admin | 管理员用的命令行工具 / 管理后台，负责发布。 |
| agent | 装在每台云电脑上的管理程序，以 SYSTEM（Windows 最高权限的系统账户）身份作为服务运行，负责下载和安装。 |
| policy.json | 放在 OSS 上的策略文件。agent 定时读它，按里面写的目标版本去安装。 |
| OSS | 阿里云对象存储，用来放安装包和策略文件。 |
| CI / Release | CI 是 GitHub 上自动构建安装包的流程；构建产物挂在 GitHub 的 Release 页面上。 |
| PAT / token | GitHub 个人访问令牌，用来下载私有仓库里的文件。 |
| AppLocker | Windows 自带的程序白名单。不在名单里的程序不能运行。 |
| Inno Setup | Codex 安装程序用的打包工具。 |
| per-user / 机器级安装 | per-user 是装进某个用户自己的目录，只有他能用；机器级是装进公共目录，所有用户都能用。 |
| SHA-256 | 安装包的"指纹"，用来确认包没被换掉或损坏。 |
| kill switch | 紧急停止开关。这里指把目标版本清空，让 agent 停止安装。 |
| 幂等 | 同一件事做多次和做一次效果一样。这里指已经装好的版本不会重复装。 |
| 灰度 | 先给一部分机器发布，没问题再扩大。 |
| 机队 | 所有被管理的云电脑。 |

**状态：admin 和 agent 两边都已经实现。**

- agent 默认 Codex 装在 `C:\tools\Codex`（可以用环境变量 `AIENVMGR_CODEX_ROOT` 改）。
- 更新时用 `/DIR=` 参数在原目录覆盖安装，不会把程序挪到别处。

**AppLocker 放行：**

- `C:\tools\Codex` 不在镜像自带的 AppLocker 白名单里。
- 从 agent 1.2.13 起，由 policy.json 里的 `appLockerAllowPaths` 放行（在控制台「封禁策略」页设置），装机脚本不用改。
- 注意：真正的可执行文件是 `_internal\app\ChatGPT.exe`。默认快捷方式要经过 `wscript.exe` 中转，而 `wscript.exe` 会被 AppLocker 拦下。详见 [`docs/AppLocker与Codex启动.md`](AppLocker与Codex启动.md)。

**核心原则：构建成功不等于可以发布。**

- CI 构建成功，只说明补丁打得对。
- 这个包能不能给员工用（比如 ChatGPT 入口是不是真去掉了、Computer Use 能不能用），只有在 Windows 真机上才能验证。
- 所以发布必须由管理员明确操作，不能自动进行。

---

## 1. 总体流程

```
GitHub CI ──构建──> Release(私有仓库)
                        │
                   管理员下载、真机验收
                        │
              admin codex publish（拉包→传 OSS→写 policy）
                        ↓
                  agent_workdir/policy.json
                        ↓
            agent 轮询 → 版本不符 → 下载校验 → 静默安装 → 上报
                        ↓
                  admin codex status / machine list
```

- 员工云电脑上的 agent **只和 OSS 通信**，不碰 GitHub。
- 私有仓库的凭证只放在管理员的机器上。

---

## 2. 为什么不让 agent 直接从 GitHub Release 下载

| 问题 | 说明 |
|---|---|
| 凭证扩散 | 从私有仓库下载需要 token。这等于在每台员工机器上都放一把能读源码的钥匙。 |
| 网络 | N 台云电脑各从 GitHub 拉 700 MB，又慢又不稳定；OSS 在同一地域走内网，不收流量费。 |
| **失去发布控制** | CI 一发布，所有人立刻更新。构建成功不等于验收通过。 |

管理员这边则相反：用管理员自己的 PAT 拉包，命令是 `admin codex publish --url <release-asset> --token <pat>`，用法和现有的 `admin agent publish` 完全一样。

---

## 3. 必须先做的改造（不做，方案就不成立）

### 3.1 必须装在机器级目录 —— 已满足

当前镜像把 Codex 装在 `C:\tools\Codex`。这是机器级目录，SYSTEM 服务能写，所以方案成立。

但原始安装程序的模板仍然是 per-user 的（`PrivilegesRequired=lowest`、`DefaultDirName={%USERPROFILE|{localappdata}}\Codex`）。所以：

- **新机器装机时，必须明确指定安装目录。** 否则会装进当时登录用户的个人目录，agent 就更新不到。
- agent 安装时传 `/DIR=`，就是为了保证以后更新都装在同一个地方。

<details><summary>原始说明（仍建议把安装程序改成机器级）</summary>

当前 `installer/CodexOffline.iss.tpl`：

```ini
PrivilegesRequired=lowest
DefaultDirName={%USERPROFILE|{localappdata}}\Codex
```

agent 是 **SYSTEM 服务**。SYSTEM 账户运行 per-user 安装程序，会装进 `C:\Windows\System32\config\systemprofile\AppData\Local\Codex`，**员工看不到**。

需要改成 `PrivilegesRequired=admin` + `{autopf}\Codex`（即 Program Files 下），并把卸载信息写到 HKLM（整台机器共用的注册表）。

> 已经按 per-user 方式装过的机器不会被自动接管，需要单独清理。动手前先确认。

</details>

### 3.2 安装包版本号必须和 Codex 上游版本区分开 —— 卡住后续工作

`build-offline-package.ps1` 目前这样取版本号：

```powershell
$version = $sourceMetadata.version      # 就是 Codex 上游版本，如 26.810.4967.0
```

问题：

- 同一个 Codex 版本，补丁修好后重新构建，版本号**完全一样**。
- 例如 2026-08-14 为适配 26.810 提交了 6 次，每次构建出来都是 `26.810.4967.0`。
- agent 比对版本时会以为"已经是最新"，修复就推不下去。

需要加一个包版本号，例如 `26.810.4967.0+b7`，或者带上 commit 的短哈希。

---

## 4. OSS 上新增的目录

沿用 `_agent/` 的做法（见 [OSS布局.md](OSS布局.md) §7）：

```
agent_workdir/
  _codex/
    codex-setup-{包版本}.exe      # 安装包，带版本号，便于回滚保留旧版
```

agent 对 `_codex/` **只能读**；只有管理员能写。

## 5. Policy 新增字段

含义仿照 `AgentUpdateVersion`（见 model.go）：

```go
// Codex 桌面版分发。版本为空表示"不分发"，也是远程 kill switch。
// 与 AgentUpdate 一样按"相等"判断而非"更新"判断：agent 只要发现
// 本机已装版本 != CodexVersion 就安装目标版本，因此把版本改回旧值
// 即为回滚。
CodexVersion    string `json:"codexVersion,omitempty"`
CodexSHA256     string `json:"codexSHA256,omitempty"`
CodexKey        string `json:"codexKey,omitempty"`        // _codex/ 下的对象键
CodexRolloutPct int    `json:"codexRolloutPct"`            // 已废弃，恒为 100
```

**故意按"是否相等"比较，而不是比新旧。** 好处是出了问题，把目标版本改回旧值就能回滚。如果逻辑是"只升不降"，就做不到这一点。

## 6. Status 新增字段

Status 是 agent 上报给后台的机器状态。

```go
CodexVersion     string `json:"codexVersion,omitempty"`     // 本机已装版本
CodexUpdateState string `json:"codexUpdateState,omitempty"` // idle|downloading|installing|failed|deferred
CodexUpdateError string `json:"codexUpdateError,omitempty"`
```

`admin machine list` 增加一列 CODEX，可以一眼看出所有机器的版本分布，以及哪些机器卡住了。

---

## 7. Agent 端逻辑

照搬 `prepareUpdate()` 的框架（sync.go:356），保留它已经验证过的四条特性：

1. `CodexVersion == ""` → 什么都不做（kill switch）。
2. 已装版本 == 目标版本 → 跳过（幂等）。
3. 用一个 **marker 文件**记下已经尝试过的版本 → 失败后不会无限重试、把机器拖垮。
4. SHA-256 不符 → 拒绝安装并上报。

新增三条规则：安装前提条件、读取本机已装的版本、安装。原设计里的灰度已经取消，先说明这一点。

**灰度（已取消）**

- 原设计：按 `crc32(主机名) % 100 < CodexRolloutPct` 分批下发。
- 为什么去掉：
  - 机器数量少，发 10% 经常一台都选不中。
  - 灰度想换来的信心，只能靠 Windows 真机验收，灰度本身给不了。
- 结论：**先真机验收，再发布。发布是最后一步，不是测试手段。**

> `codexRolloutPct` 字段仍然会写出来，值固定为 100。原因：agent 1.2.5–1.2.7 把它当开关用，字段缺失或为 0 时**一台都不装，也不报错**。新版 agent 完全不看它。

**安装前提条件**

- Codex 正在运行 → 这次跳过，状态记为 `deferred`（推迟），下一轮再试。原因：Inno Setup 遇到文件被占用会失败；而且也避免打断员工。
- 磁盘剩余空间小于 3 GB → 跳过并上报。

**读取本机已装的版本**

读 Inno Setup 自己写的卸载注册表项（机器级安装后在 HKLM 下）：

```
HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\{AppId}_is1
  DisplayVersion
```

这是唯一可靠的来源。它是安装程序自己写的，比在磁盘上找文件、或者 agent 自己记账都准。

**安装**

```
codex-setup.exe /VERYSILENT /SUPPRESSMSGBOXES /NORESTART /LOG="<日志路径>"
```

Inno Setup 的退出码：`0` 成功，`1` 失败，`2`/`5` 被取消，`1641`/`3010` 需要重启。
失败时，把安装日志的最后一段随状态一起上报。

### 7.1 700 MB 的包不能用现有的 Store 接口下载

现有接口：

```go
Get(key string) ([]byte, string, error)   // 整个对象读进内存
```

- agent 自己的程序只有几 MB，整个读进内存没问题。
- Codex 安装包有 700 MB。云电脑一般只有 4–8 GB 内存，复制时内存占用峰值可能到 1.4 GB，有内存耗尽（OOM）的风险。

所以需要新增"边下载边写文件"的接口 `GetToFile`：

- 文件存到 `%ProgramData%` 下的缓存目录；
- 支持断点续传；
- 装完后保留一份，方便重试和回滚。

---

## 8. Admin 命令

仿照 `admin agent`（agent_cmd.go）：

```
admin codex publish <path>                                   # 本地文件
admin codex publish --url <asset-url> --version <v> --token <pat>
admin codex cancel                                           # kill switch
admin codex status                                           # 目标版本 + 机队分布
```

`publish` 内部依次：下载 → 算 SHA-256 → 上传到 `_codex/` → 写 policy 里的四个字段。结构和 `PublishAgentUpdate` 一样。

---

## 9. 实施顺序

| 阶段 | 内容 | 依赖 |
|---|---|---|
| 0 | 安装器改机器级 | 阻塞全部 |
| 0.5 | 包版本加构建号 | 阻塞版本比对 |
| 1 | CI 用 OIDC 上传 OSS（可选，也可由 admin 从 release 拉） | — |
| 2 | ossclient 流式下载 | — |
| 3 | Policy/Status 字段 + agent 更新任务 | 0、0.5、2 |
| 4 | admin codex 命令 + machine list 列 | 3 |

说明：

- "阻塞"指不做完它，后面的阶段没法进行。
- OIDC 是一种免密钥授权方式，让 CI 不存长期密钥也能上传 OSS。
- 阶段 1 不是必须的：`admin codex publish --url --token` 已经能直接从私有 Release 拉包。CI 直传 OSS 只是省掉管理员本地中转这一步，可以以后再补。

---

## 10. 备用通道

agent 失联时，可以用无影 EDS 自带的 `RunCommand`（ecd 2020-09-30 版接口）补救，不依赖 agent：

- 一次最多对 50 台云电脑执行 PowerShell，支持以 SYSTEM 权限运行。
- 脚本内容仍然是"从 OSS 拉包 → 校验 → 静默安装"。命令内容 Base64 编码后不能超过 16 KB，所以装不下安装包本身，只能让脚本去下载。

## 11. 关于镜像

无影换镜像（`RebuildDesktops`）的限制：

- 会**清空系统盘**。官方原文："原云电脑系统盘中的数据将被清除"。
- 要求先关机，一次最多 20 台。
- 桌面组改了镜像，默认只对之后新扩容的机器生效。

因此：

- 镜像**不用于日常更新**，只在低频（每月）重新制作一次，让新开的云电脑一出厂就是较新的版本。
- 已有的机器一律用本方案的软件分发来更新。
