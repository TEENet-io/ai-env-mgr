# Codex Windows 沙盒统一管理方案

给谁看：负责 Agent 和管理后台（Admin）的开发同事。

> **这一期实际做什么（2026-10-12）：**
> - 先不在 Admin 里加"沙盒模式"配置，也不要求切换到 MXC 或 elevated 模式。
> - Agent 直接扫描本机所有员工的用户目录，给每个用户维护一条明确写出路径的 AppLocker 规则：`C:\Users\<user>\.codex\.sandbox-bin\*`。
> - 下文讲的 MXC / elevated 是以后可以选择的改进方向，不是这一期部署的前提条件。

## 先认识几个词

| 词 | 意思 |
|---|---|
| Codex | 员工使用的 AI 编程工具。它在电脑上执行命令时，会放进"沙盒"里运行。 |
| 沙盒（sandbox） | 隔离环境。Codex 执行的命令在里面运行，限制它能碰到的文件和权限。 |
| runner | Codex 用来在沙盒里启动命令的小程序，放在用户目录下的 `.codex\.sandbox-bin` 里。 |
| Agent | 装在每台员工电脑上的管理程序，以 Windows 服务身份运行。 |
| Admin | 管理后台，管理员在这里下发配置、查看机器状态。 |
| AppLocker | Windows 自带的程序白名单。不在名单里的程序不能运行。 |
| Path / Publisher / Hash 规则 | AppLocker 的三种放行方式：按文件所在路径放行；按程序的数字签名（发布者）放行；按文件指纹（SHA-256）放行。 |
| MXC | 新版 Windows 11 提供的原生隔离能力，Codex 可以直接用它做沙盒。 |
| elevated / unelevated | Codex 的两种沙盒方式。elevated 需要事先用管理员权限做好准备（例如建专用账户），隔离更强；unelevated 不需要管理员权限，隔离较弱。 |
| 用户 profile | Windows 上每个用户的个人目录，例如 `C:\Users\张三`。 |
| 灰度 | 先在少数机器上试，没问题再推到全部机器。 |

## 目标

通过 Agent 统一给所有员工电脑配置和维护 Codex 的 Windows 沙盒。要解决的报错是：

`CreateProcessWithLogonW failed: 1260`：Codex 想以另一个身份启动 runner，但被系统策略拦住了（在这里就是被 AppLocker 拦住）。

同时，**不能把用户自己能写入的目录加进 AppLocker 白名单**。

目标不是"允许运行 `.codex\\sandbox-bin` 下的所有 EXE"，而是让 Codex 用一种官方支持、能审计、能回滚的沙盒方式。

## 设计原则

1. **按机器统一管理**：Admin 下发目标模式，Agent 以 Windows 服务身份执行。
2. **安全优先**：不放行 `C:\Users\*`、`AppData`、`Temp`，也不放行整个 `.codex` 目录。
3. **先看机器能做什么**：每台机器先检测 Windows 版本、Codex 版本、是否支持 MXC、管理员是否已批准、本地有哪些策略。
4. **在后台执行**：沙盒配置、给用户预先配置、修复任务，都放进 Agent 的任务队列里执行。不能卡住同步、状态上报或管理通道。
5. **看得见**：Admin 能看到目标模式、实际模式、检测结果、最近的错误、runner 签名和最后一次应用配置的时间。
6. **能回滚**：每次切换模式都有版本号、审计记录和上一份配置的备份。

## 几种沙盒模式

| 模式 | 适用场景 | 优先级 | 说明 |
|---|---|---:|---|
| `auto` | 默认 | 1 | 依次尝试 MXC → elevated → unelevated，根据机器能力和公司限制选择 |
| `mxc` | 新版 Windows 11 | 2 | 首选。用系统原生隔离，不需要在 AppLocker 里放行用户目录 |
| `elevated` | 旧版系统，或 MXC 用不了 | 3 | Agent 为每个员工预先配置好 Codex 的 elevated 沙盒 |
| `unelevated` | 应急时保证能用 | 4 | 必须明确允许才会用，隔离能力较弱 |
| `blocked` | 出于安全原因禁止 | - | 不让 Codex 执行本地命令，只保留聊天和文件功能 |

Admin 默认下发 `auto`，同时维护一个"允许使用哪些模式"的列表，例如：

```json
{
  "codexSandbox": {
    "mode": "auto",
    "allowedModes": ["mxc", "elevated"],
    "minCodexVersion": "0.162.0",
    "revision": 1
  }
}
```

## Admin 要改什么

### 配置

新增一项按机器生效的 Codex 沙盒策略，包括：

- 目标模式：自动 / MXC / elevated / unelevated / 禁用
- 允许退回到哪些模式
- 最低 Codex 版本
- 是否允许联网的沙盒
- 灰度范围，或指定哪些机器
- 策略修订号

不要用 `appLockerAllowPaths`（现有的 AppLocker 放行路径配置）来放行 `.codex` 用户目录。这个字段继续只接受受保护的机器级目录。

### 任务中心

新增四种任务：

- `codex_sandbox_probe`：检测机器能力
- `codex_sandbox_setup`：配置沙盒
- `codex_sandbox_verify`：验证沙盒能用
- `codex_sandbox_rollback`：回滚

任务状态有：`queued`（排队中）、`running`（执行中）、`succeeded`（成功）、`failed`（失败）、`cancelled`（已取消）。每个任务保存执行步骤、错误、耗时和重试次数。

### 管理界面

机器详情页显示：

- 目标模式和实际模式
- Codex 版本
- Windows 版本和构建号
- 是否支持 MXC
- elevated 是否已配置
- runner 签名状态
- AppLocker 最近拦截了多少次
- 最近一次配置的时间和错误
- "重新探测""重新配置""回滚"按钮

## Agent 要改什么

### 检测机器能力

Agent 启动时、策略变化时，在后台收集以下信息：

- Windows 构建号、架构和版本类型（SKU）
- Codex 版本
- `prefer_mxc` 和 `windows.sandbox` 两项当前的配置
- MXC 能力检测结果
- elevated 沙盒用到的用户、权限和防火墙配置
- `CODEX_HOME/.sandbox/sandbox.log`（Codex 沙盒日志）的摘要
- runner 文件的签名、版本和 SHA-256 指纹
- 最近的 AppLocker 8004 事件（程序被 AppLocker 拦截）和 Code Integrity 3077 事件（程序被代码完整性策略拦截）

### 执行流程

1. 读取目标策略，生成任务。
2. 检测本机能力。
3. 选定实际模式：MXC、elevated、unelevated 或 blocked。
4. 给每个已有的员工用户做配置。
5. 写入用户级的 Codex 配置，或者调用官方的沙盒配置程序。
6. 运行一条最简单的验证命令：`cmd.exe /d /c echo CODEX_SANDBOX_OK`。能输出这行字，说明沙盒里能正常运行命令。
7. 确认 AppLocker 和 Code Integrity 没有新的拦截记录。
8. 上报结果和日志摘要。

以上步骤都必须由后台的工作线程执行。主同步循环只负责把任务放进队列、读取结果。

### AppLocker 兜底方案

不增加放行整个用户目录的通用 Path 规则。只支持两种方式：

1. 给数字签名已验证的 Codex runner 加 Publisher 规则（按签名放行）；或者
2. 给当前 runner 加 SHA-256 Hash 规则（按指纹放行），Codex 升级后自动换成新版本的指纹。

这些 Publisher 和 Hash 规则用 Agent 专用的名称前缀，和现有的机器级 Path 规则分开管理。

## 数据和接口

设备状态新增字段：

- `codex_sandbox_target`
- `codex_sandbox_actual`
- `codex_sandbox_capabilities`
- `codex_sandbox_revision`
- `codex_sandbox_last_apply_at`
- `codex_sandbox_last_error`
- `codex_runner_signature`
- `codex_runner_hash`
- `codex_applocker_denials`

新增接口：

- `POST /admin/machines/{id}/codex-sandbox/probe`
- `POST /admin/machines/{id}/codex-sandbox/apply`
- `POST /admin/machines/{id}/codex-sandbox/rollback`
- `GET /admin/machines/{id}/codex-sandbox`

## 上线步骤

1. **只观察阶段（Audit）**：只检测，不改动现有策略。
2. **单机阶段**：在 Peter 的电脑上验证 MXC 和 elevated。
3. **小范围灰度**：选 1–3 台机器，观察 24 小时。
4. **全面推广**：默认用 `auto`，禁止按用户目录路径放行。
5. **回滚**：恢复上一份 Codex 配置和 Agent 管理的规则。

## 验收标准

- 普通用户能在 Codex 里运行 Python、Node、`cmd` 和 PowerShell。
- 不再出现 `CreateProcessWithLogonW failed: 1260`。
- AppLocker 没有新增不该出现的 8004 拦截事件。
- 沙盒配置不会卡住 Agent 的主同步、状态上报和管理通道。
- 新建员工用户时，不需要人工创建 AppLocker 规则。
- Codex runner 更新后，不需要手动重新加白名单。
- Admin 能看到每台机器的目标模式、实际模式和失败原因。

## 当前机器的处理结论

从系统事件看，Codex 0.153.4 的 runner 放在用户目录里，被 AppLocker 拦住了。

处理方式：

- **不要**把 `%OSDRIVE%\\Users\\*\\.codex\\.sandbox-bin\\*` 这条通配规则直接下发。
- 由 Agent 列出机器上现有的用户 profile，给每个用户生成一条写明具体路径的规则。
- 用户新增或删除时，Agent 自动增加或删除对应规则。

这样做有两个好处：不需要 Admin 参与；也不会把远程 `policy.json` 里写的用户目录当成可信的输入。
