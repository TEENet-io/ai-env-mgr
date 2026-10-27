# Codex Windows 云电脑预装工具（Toolchain）清单

**版本**：v1.0
**更新日期**：2026-09-29
**适用范围**：AI Env Mgr 管理的 Windows x64 云电脑、Codex 构建机、Agent/Admin 运维机

## 先认识几个词

| 词 | 意思 |
|---|---|
| Toolchain（工具链） | 写代码、跑代码要用到的一组工具，例如 Git、Python、Node.js。 |
| Agent | 装在每台云电脑上的管理程序（`agent.exe`），负责下发配置、安装软件、上报状态。 |
| Admin | 管理后台（`admin.exe`）。 |
| 镜像 | 新云电脑装机用的系统模板。 |
| `PATH` | Windows 环境变量，列出系统去哪些目录找程序。工具不在 `PATH` 里，Codex 就找不到它。 |
| 标准用户 | 没有管理员权限的普通员工账号。 |
| SYSTEM | Windows 内置的最高权限系统账号。Agent 以这个身份运行。 |
| AppLocker | Windows 自带的程序白名单。不在名单里的程序不能运行。 |
| ACL | 文件夹的访问权限设置，决定谁能读、写、执行。 |
| 脚本宿主 | 负责运行脚本的 Windows 程序，例如 `wscript.exe`、`mshta.exe`。 |
| LTS | 长期支持版，官方维护时间较长的稳定版本。 |
| BOM | 软件物料清单，记录镜像里每个软件的版本。 |
| SHA-256 | 安装包的"指纹"，用来确认安装包没有被替换或损坏。 |
| CI | 自动构建和发布的流程。 |

## 先说结论

Codex 桌面程序启动时，不需要系统另外装 Python 或 Node.js：

- 当前的 Codex 发行包已经自带桌面应用需要的运行组件。
- `agent.exe` 和 `admin.exe` 是用 Go 编译好的单个文件，不依赖 Python、Node.js，也不依赖系统里装 Go。

但是，Codex 会替员工在本机执行命令、脚本和项目工具。员工要让 Codex 做 Python、JavaScript/TypeScript、Git 等任务，就必须在这台云电脑上装好对应工具，并且标准用户的 `PATH` 能找到它们。

所以要分清两件事：

1. **Codex 能不能启动**：不需要 Python/Node.js。
2. **Codex 能不能完成某类开发任务**：要看对应的语言或工具有没有装，以及版本、权限、网络和 AppLocker 策略是否允许使用。

## 一、员工云电脑分层清单

### A. 基础必备：所有 Codex 云电脑都要有

| 组件 | 是否必需 | 作用 | 备注 |
|---|---|---|---|
| Windows 10/11 x64 | 是 | 运行 Codex、Agent 和员工会话 | 以镜像实际支持的系统版本为准 |
| Codex 桌面包 | 是 | 员工使用的 Codex 应用 | 当前安装在 `C:\Tools\Codex`；通过镜像或 Admin 下发 |
| `powershell.exe`（Windows PowerShell 5.1） | 是 | Codex 用它执行 Windows 命令；Agent 用它读取和应用 AppLocker、读取安装结果 | Windows 自带；AppLocker 必须允许 Codex 使用它 |
| `cmd.exe` | 是 | 运行批处理、安装程序和部分项目脚本 | Windows 自带 |
| Windows 用户环境变量 `PATH` | 是 | 让 Codex 找到已安装的工具 | 必须用标准员工账号实际登录后验证，不要只在 SYSTEM 身份下验证 |
| Agent Windows 服务 | 是 | 下发凭据、策略、软件和更新，上传状态和日志 | `agent.exe` 是单文件 Go 程序，不需要装 Go/Python/Node |
| AppLocker 和文件权限基础配置 | 是 | 限制脚本宿主和可写目录，允许审核过的工具运行 | `C:\Tools`、`Program Files` 的 ACL 不能允许标准用户写入 |

`msiexec.exe`、`taskkill.exe`、`curl.exe`、`tar.exe` 等是 Windows 自带的组件，Agent 或安装任务需要时直接调用，不用另外下载。

`wscript.exe`、`cscript.exe`、`mshta.exe`、`regedit.exe` 和 `reg.exe` 仍按现有 AppLocker 策略限制。不要为了"工具链完整"而放开它们。

### B. 推荐的开发工具：员工有写代码的任务时预装

| 组件 | 推荐程度 | 用来做什么 | 安装建议 |
|---|---|---|---|
| Git for Windows | 推荐 | clone、branch、diff、commit、远程仓库操作 | 装到整台机器（机器级），位置 `C:\Program Files\Git`；确认标准用户的 `PATH` 里有 `git.exe` |
| Python 3（x64） | 推荐 | 运行 `.py` 脚本、测试、虚拟环境、数据处理 | 机器级安装；保留 `python.exe` 和 `py.exe`；不要给不明项目预装全局 pip 包 |
| Node.js 当前 LTS 版（x64） | 推荐 | JavaScript/TypeScript、前端构建、Node 命令行工具 | 机器级安装到 `C:\Program Files\nodejs`，Node 自带 `npm` |
| npm | 随 Node.js 一起装 | 安装 JS 依赖、运行 npm 脚本 | 不要单独再装一套 npm；固定 Node LTS 和 npm 的版本 |
| OpenSSH Client | 可选，推荐 | Git 走 SSH、远程调试、访问代码仓库 | 通过 Windows 可选功能或受控方式安装；不预先放私钥 |
| 7-Zip | 可选，推荐 | 解压特殊格式的压缩包、检查发布包 | 只在团队确实要用时安装；普通 zip 优先用 Windows 自带命令 |

这一层提供的是"开发能力"，不是 Codex 启动所必需的。

如果员工只用 Codex 对话、处理文本和使用已有应用，不写 Python/Node 项目，可以先不装 Python 和 Node。这样补丁、依赖和供应链风险都更少。

### C. 按项目安装：不要放进所有镜像

下面这些工具，有明确的项目需求时，再由管理员审核、安装并登记版本：

| 组件 | 典型用途 | 为什么不默认预装 |
|---|---|---|
| `pnpm` / `yarn` | 特定 JavaScript 项目 | 和 npm 的锁文件、全局版本可能冲突；组织应统一选一种 |
| Java JDK | Java/Gradle/Maven | 体积大，打补丁的维护成本高 |
| Go | 本地编译 Go 项目 | 员工运行 Codex 用不到；Agent/Admin 已经是编译好的程序 |
| Rust / Cargo | Rust 项目 | 编译工具体积大，需要单独的缓存和升级策略 |
| .NET SDK | .NET 项目 | 版本要跟着项目固定 |
| WSL2 | 兼容 Linux 工具 | 需要虚拟化、内核，以及额外的网络和磁盘管理 |
| Docker Desktop | 容器开发 | 需要虚拟化和更高权限，不应默认给标准用户 |
| AWS/GCP/Azure CLI | 管理云资源 | 可能扩大凭据和云资源的权限范围；按岗位发放 |
| 数据库客户端/本地数据库 | 项目测试 | 会带来额外的端口、数据和清理责任 |

### D. 只有构建/发布机需要

不要因为员工端的 Codex 需要，就把这些工具装进员工镜像：

| 组件 | 用途 | 依据 |
|---|---|---|
| Go（以仓库 `go.mod` 为准） | 构建 `agent.exe`、`admin.exe` | 当前仓库 `go.mod` 写的是 Go 1.26；CI 通过 `go-version-file` 读取这个版本 |
| Node.js + npm | Codex-only 补丁、测试和 Web Gateway 构建 | 这是 `codex-only-local/BUILDING.md` 里的构建要求，不是桌面运行要求 |
| PowerShell 7（`pwsh`） | 运行 Codex 打包脚本 | 构建脚本需要；员工端只依赖 Windows PowerShell 5.1 |
| Inno Setup 6 | 生成 `setup.exe` | 用来构建安装包，不随员工镜像发布 |
| Git | 拉取源码、标签和发布记录 | 构建机需要；员工镜像要不要装，按 B 层的需求决定 |

Codex 构建机可以按 `codex-only-local/BUILDING.md` 单独维护。不要把那份文档里的构建版本，直接当成员工云电脑必须预装的版本。

## 二、目前建议的镜像方案

### 最小镜像（只需要在受控环境下用 Codex）

- Windows 10/11 x64
- Codex 桌面包
- Agent 服务
- Windows PowerShell 5.1、`cmd.exe`
- Git（如果员工需要拉取或提交代码）
- AppLocker，以及 `C:\Tools` 和 `Program Files` 的 ACL 基础配置

这个方案不预装 Python、Node.js、Java、Go、Rust、Docker 或云命令行工具。

### 通用开发镜像（目前更适合开发团队）

在最小镜像的基础上增加：

- Git for Windows
- Python 3 x64
- Node.js 当前 LTS 版 + npm
- OpenSSH Client
- 7-Zip（确实需要处理压缩包时）

Java、Go、Rust、.NET、WSL2、Docker 等，留给按项目或岗位定制的镜像，不要继续往所有机器里加。

### 版本固定原则

不要在本文里写死"最新版本"。版本应记在镜像 BOM 或 Admin 软件清单里，跟镜像一起发布：

1. Python 用组织批准的 3.x 小版本。
2. Node 用组织批准的 LTS 小版本；npm 和 Node 一起验证。
3. Git、7-Zip、OpenSSH 按月或按季度打补丁。
4. 升级版本时，先在一台验收机上测试，再放进新镜像。不要让 Codex 自己下载和升级运行环境。
5. 软件安装任务必须记录：安装包 URL、SHA-256、静默安装参数、检测路径和安装日志。

## 三、安装位置、权限和 PATH 要求

推荐的机器级安装位置：

```text
C:\Program Files\Git\
C:\Program Files\nodejs\
C:\Program Files\Python311\ 或组织批准的 Python 目录
C:\Program Files\7-Zip\
C:\Tools\Codex\
C:\Tools\ai-env-mgr.exe
```

要求：

- 工具安装目录只能由 Administrators/SYSTEM 写入，标准用户只能读取和执行。
- `PATH` 用机器级的 PATH。安装后，要新开一个用户会话才能看到变化。
- 不要把程序放在 `%TEMP%`、下载目录或员工能写的目录，再靠 AppLocker 放行。
- 免安装的便携工具如果必须放在 `C:\Tools`，要同时把它加进 Admin 下发的 AppLocker 放行目录（allow path），并检查目录的 ACL。
- Python 虚拟环境、Node 的 `node_modules` 和项目构建缓存，放在项目目录或用户目录里，不要写进受保护的公共安装目录。

## 四、验收清单

必须用标准员工账号验收一次。不能只用管理员或 Agent 的 SYSTEM 身份验证。

### 1. 检查基础命令能否找到

```powershell
Get-Command powershell.exe
Get-Command cmd.exe
Get-Command git.exe -ErrorAction SilentlyContinue
Get-Command python.exe -ErrorAction SilentlyContinue
Get-Command py.exe -ErrorAction SilentlyContinue
Get-Command node.exe -ErrorAction SilentlyContinue
Get-Command npm.cmd -ErrorAction SilentlyContinue
Get-Command ssh.exe -ErrorAction SilentlyContinue
```

### 2. 检查版本和最基本的功能

```powershell
git --version
python --version
py --version
node --version
npm --version
ssh -V
```

没装的可选工具显示"找不到命令"，这是正常的，不代表 Codex 安装失败。

### 3. 用 Codex 实际跑任务

至少验证下面这些任务，并保存一次任务日志：

- PowerShell：读取当前目录，创建一个测试文件再删掉。
- Git：对测试仓库执行 `status`、`diff`，不使用真实密钥。
- Python：运行一个不联网的 `.py` 脚本；如果装了 Python，创建一个临时虚拟环境（venv）再删掉。
- Node：运行一个依赖已锁定的最小 `npm` 脚本；如果装了 Node，确认 `npm ci` 不会停下来等人输入。
- 解压：用 Windows 自带的 zip 功能或批准的 7-Zip 解压测试包。
- Agent：确认软件安装、状态同步和日志上传，不会被耗时很长的构建命令卡住。

同时检查 Windows 的 AppLocker 日志：不应出现 `powershell.exe`、`cmd.exe`、Git、Python 或 Node 被拦截的事件。不要为了消除拦截而放开 `wscript.exe`、`mshta.exe` 等脚本宿主。

## 五、安全和运维边界

- Python/Node 的包管理器会运行第三方代码。只允许用组织批准的下载源、锁文件和项目依赖。禁止把 API Key 写进 `pip` 或 npm 的配置，也禁止提交到项目里。
- 不在系统级全局安装项目依赖。优先用 Python 虚拟环境（venv）和项目自己的 `node_modules`。
- Codex 能调用已安装的命令，不等于它有权限访问所有目录、网络和云账号。权限仍由 Windows 用户、AppLocker、网络策略和 Admin 下发的策略共同决定。
- Agent 负责受控地安装和更新软件。某个项目临时缺 Python/Node 时，Agent 不应自己去下载来路不明的版本。
- 新增任何语言的运行环境，都要在软件清单里写全：供应商、版本、来源、SHA-256、安装参数、检测规则、卸载方式、负责人和补丁周期。

## 六、最终建议

目前建议直接做两种镜像：

1. **Codex 基础镜像**：Codex + Agent + Windows PowerShell/cmd + 安全基础配置；不强制装 Python/Node。
2. **Codex 开发镜像**：基础镜像 + Git + Python 3 x64 + Node.js LTS/npm + OpenSSH；其他语言和 Docker 按项目单独安装。

这样既能保证 Codex 基本可用，也不会把用不到的运行环境、包仓库和高权限工具装到所有员工机器上。
