# Codex 分发方案

把重打包的 Codex 桌面版（[TEENet-io/codex-kiosk](https://github.com/TEENet-io/codex-kiosk)）
分发到无影云电脑，并在上游更新后持续推送新版本。

**状态：admin 侧与 agent 侧均已实现。** agent 假定 Codex 装在 `C:\tools\Codex`
（可用环境变量 `AIENVMGR_CODEX_ROOT` 覆盖），更新时用 `/DIR=` 原地安装，
不会把应用挪走。

该目录不在镜像 AppLocker 的白名单内。agent 1.2.13 起由 policy.json 的
`appLockerAllowPaths` 放行（控制台「封禁策略」页），装机脚本无需改。注意真正的可执行
文件是 `_internal\app\ChatGPT.exe`，且默认快捷方式经 `wscript.exe` 中转——后者会被
AppLocker 拦下，见 [`docs/AppLocker与Codex启动.md`](AppLocker与Codex启动.md)。

**核心原则:构建 ≠ 发布。**
CI 构建成功只说明补丁应用正确，不代表这个包可以给员工用
（是否真的去掉了 ChatGPT 入口、Computer Use 是否可用，只有 Windows 真机能验证）。
因此发布必须是管理员的显式动作。

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

员工云电脑上的 agent **只与 OSS 通信**，不接触 GitHub。
私有仓库的凭证只存在于管理员机器上。

---

## 2. 为什么不让 agent 直接读 GitHub Release

| 问题 | 说明 |
|---|---|
| 凭证扩散 | 私有仓库下载 release asset 需要 token，等于每台员工机器上都放一个能读源码的凭证 |
| 网络 | 云电脑从 GitHub 拉 700 MB × N 台，慢且不稳定；OSS 同区内网免流量费 |
| **失去发布控制** | CI 一发布全员立即更新。构建绿 ≠ 验收过 |

管理员侧则相反：`admin codex publish --url <release-asset> --token <pat>`
用管理员自己的 PAT 拉包，与现有 `admin agent publish` 完全一致。

---

## 3. 前置改造（不做则方案不成立）

### 3.1 安装位置必须是机器级 —— 已满足

当前镜像把 Codex 装在 `C:\tools\Codex`，属于机器级位置，SYSTEM 服务可写，
本方案成立。

原始安装器模板仍是 per-user 的（`PrivilegesRequired=lowest`、
`DefaultDirName={%USERPROFILE|{localappdata}}\Codex`），因此**新机器装机时
必须显式指定安装目录**，否则会装进当时那个用户的 profile，agent 就更新不到。
agent 安装时传 `/DIR=` 正是为了保证后续更新不会漂移到别处。

<details><summary>原始说明（安装器仍建议改成机器级）</summary>

当前 `installer/CodexOffline.iss.tpl`：

```ini
PrivilegesRequired=lowest
DefaultDirName={%USERPROFILE|{localappdata}}\Codex
```

agent 是 **SYSTEM 服务**。SYSTEM 执行 per-user 安装器会装进
`C:\Windows\System32\config\systemprofile\AppData\Local\Codex`，**员工看不到**。

需改为 `PrivilegesRequired=admin` + `{autopf}\Codex` + HKLM 卸载项。

> 已用 per-user 方式装过的机器不会被自动接管，需要单独清理，动手前确认。

</details>

### 3.2 包版本必须区别于 Codex 上游版本 —— 阻塞项

`build-offline-package.ps1` 目前：

```powershell
$version = $sourceMetadata.version      # 就是 Codex 上游版本，如 26.810.4967.0
```

同一个 Codex 版本、补丁修好后重新构建，版本号**完全相同**。
（2026-08-14 为适配 26.810 提交了 6 次，每次构建都是 `26.810.4967.0`。）

agent 比对版本时会认为"已是最新"，修复推不下去。

需引入包版本，如 `26.810.4967.0+b7` 或附带 commit 短哈希。

---

## 4. OSS 布局新增

沿用 `_agent/` 的形式（见 [OSS布局.md](OSS布局.md) §7）：

```
agent_workdir/
  _codex/
    codex-setup-{包版本}.exe      # 安装包，带版本号，便于回滚保留旧版
```

Agent 用户对 `_codex/` **只读**；写入只有管理员。

## 5. Policy 新增字段

照 `AgentUpdateVersion` 的语义（见 model.go）：

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

**按相等而非新旧比较**是有意的：出问题时把版本改回旧值即可回滚，
"只升不降"的逻辑做不到这一点。

## 6. Status 新增字段

```go
CodexVersion     string `json:"codexVersion,omitempty"`     // 本机已装版本
CodexUpdateState string `json:"codexUpdateState,omitempty"` // idle|downloading|installing|failed|deferred
CodexUpdateError string `json:"codexUpdateError,omitempty"`
```

`admin machine list` 增加 CODEX 列，可一眼看出全机队版本分布与卡住的机器。

---

## 7. Agent 端逻辑

照抄 `prepareUpdate()` 的骨架（sync.go:356），四条已验证的性质全部保留：

1. `CodexVersion == ""` → 不动（kill switch）
2. 已装版本 == 目标版本 → 跳过（幂等）
3. **marker 文件**记录已尝试版本 → 失败不会无限重试刷爆机器
4. SHA-256 不符 → 拒绝安装并上报

新增三条：

**灰度（已取消）**

原设计按 `crc32(主机名) % 100 < CodexRolloutPct` 分环下发。已去掉：机器数量少，
发 10% 常常一台都不命中，而它想换来的信心只能靠 Windows 真机验收，灰度给不了。
**先真机验收，再发布；发布是最后一步，不是测试手段。**

> `codexRolloutPct` 字段仍然写出且恒为 100。agent 1.2.5–1.2.7 拿它当开关，
> 字段缺失或为 0 时**一台都不装、也不报错**；新 agent 完全忽略它。

**安装前置条件**

- Codex 进程正在运行 → 本次跳过，状态记 `deferred`，下个轮询再试
  （Inno Setup 遇到文件占用会失败；也避免打断员工）
- 磁盘剩余空间 < 3 GB → 跳过并上报

**读取本机已装版本**

读 Inno Setup 自己写的卸载注册表项（机器级安装后在 HKLM）：

```
HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\{AppId}_is1
  DisplayVersion
```

这是唯一可靠来源——安装器自己写的，比在磁盘上找文件或 agent 自行记账都准。

**安装**

```
codex-setup.exe /VERYSILENT /SUPPRESSMSGBOXES /NORESTART /LOG="<日志路径>"
```

Inno Setup 退出码：`0` 成功、`1` 失败、`2`/`5` 取消、`1641`/`3010` 需重启。
失败时把日志尾部随 status 上报。

### 7.1 700 MB 不能走现有 Store 接口

```go
Get(key string) ([]byte, string, error)   // 整个对象读进内存
```

agent 自身二进制只有几 MB，如此可行；Codex 安装包 700 MB，
云电脑通常 4–8 GB 内存，拷贝峰值可能到 1.4 GB，有 OOM 风险。

需新增流式下载（`GetToFile`），落到 `%ProgramData%` 下的缓存目录，
支持断点续传，装完保留一份以便重试与回滚。

---

## 8. Admin 命令

镜像 `admin agent`（agent_cmd.go）：

```
admin codex publish <path>                                   # 本地文件
admin codex publish --url <asset-url> --version <v> --token <pat>
admin codex cancel                                           # kill switch
admin codex status                                           # 目标版本 + 机队分布
```

`publish` 内部：下载 → 算 SHA-256 → 传 `_codex/` → 写 policy 四个字段。
与 `PublishAgentUpdate` 同构。

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

阶段 1 非必需：`admin codex publish --url --token` 已能从私有 release 直接拉，
CI 直传 OSS 只是省掉管理员本地中转，可以后补。

---

## 10. 兜底通道

无影 EDS 自带 `RunCommand`（ecd 2020-09-30）：可对至多 50 台云电脑执行
PowerShell，支持 SYSTEM 权限。agent 失联的机器可用它补救，不必依赖 agent 自身。

脚本内容仍是"从 OSS 拉包 → 校验 → 静默安装"（命令体 Base64 后限 16 KB，装不下安装包）。

## 11. 关于镜像

无影换镜像（`RebuildDesktops`）会**清空系统盘**——官方原文
"原云电脑系统盘中的数据将被清除"，且要求先关机，一次至多 20 台。
桌面组改镜像默认只对新扩容的机器生效。

因此镜像**不用于日常更新**，只在低频（月度）重新烘焙一次，
让新开的云电脑出厂即为较新版本；存量机器一律走本方案的软件分发。
