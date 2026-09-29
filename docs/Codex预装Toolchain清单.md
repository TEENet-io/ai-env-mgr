# Codex Windows 云电脑预装 Toolchain 清单

**版本**：v1.0  
**更新日期**：2026-09-29  
**适用范围**：AI Env Mgr 管理的 Windows x64 云电脑、Codex 构建机和 Agent/Admin 运维机

## 先给结论

Codex 桌面程序本身不要求系统另外安装 Python 或 Node.js 才能启动。当前发行包已经包含桌面应用所需的运行组件；`agent.exe` 和 `admin.exe` 也是 Go 静态编译的单文件程序，不依赖 Python、Node.js 或系统 Go 运行时。

但是，Codex 会代表员工执行本机的命令、脚本和项目工具。员工要让 Codex 完成 Python、JavaScript/TypeScript、Git 等任务，就必须在这台云电脑上预装对应工具，并且让标准用户的 `PATH` 能找到它们。

因此要区分两件事：

1. **能不能启动 Codex**：不需要 Python/Node.js。
2. **Codex 能不能完成某类开发任务**：取决于该语言或工具是否已安装，并且版本、权限、网络和 AppLocker 策略是否可用。

## 一、员工云电脑的分层清单

### A. 基线必备：所有 Codex 云电脑都应具备

| 组件 | 是否必需 | 作用 | 备注 |
|---|---|---|---|
| Windows 10/11 x64 | 是 | 运行 Codex、Agent 和员工会话 | 以实际镜像支持矩阵为准 |
| Codex 桌面包 | 是 | 员工使用的 Codex 应用 | 当前安装目录为 `C:\Tools\Codex`；由镜像或 Admin 下发 |
| `powershell.exe`（Windows PowerShell 5.1） | 是 | Codex 执行 Windows 命令；Agent 读取/应用 AppLocker 和安装结果 | Windows 自带；AppLocker 必须允许 Codex 使用它 |
| `cmd.exe` | 是 | 兼容批处理、安装器和部分项目脚本 | Windows 自带 |
| Windows 用户环境变量 `PATH` | 是 | 让 Codex 找到已安装工具 | 必须用标准员工用户实际登录后验证，不要只验证 SYSTEM 环境 |
| Agent Windows 服务 | 是 | 下发凭据、策略、软件和更新，上传状态/日志 | `agent.exe` 为单文件 Go 程序，不需要 Go/Python/Node |
| AppLocker / 文件权限基线 | 是 | 限制脚本宿主和可写目录，允许已审核工具运行 | `C:\Tools`、`Program Files` 的 ACL 不能让标准用户写入 |

`msiexec.exe`、`taskkill.exe`、`curl.exe`、`tar.exe` 等 Windows 组件由 Agent 或安装任务按需调用，不需要另外下载。`wscript.exe`、`cscript.exe`、`mshta.exe`、`regedit.exe` 和 `reg.exe` 仍按现有 AppLocker 策略限制，不应为了“工具链完整”而放开。

### B. 推荐开发工具链：员工有代码任务时预装

| 组件 | 推荐级别 | 适用任务 | 安装建议 |
|---|---|---|---|
| Git for Windows | 推荐 | clone、branch、diff、commit、远程仓库操作 | 机器级安装到 `C:\Program Files\Git`，确认 `git.exe` 在标准用户 `PATH` |
| Python 3（x64） | 推荐 | `.py` 脚本、测试、虚拟环境、数据处理 | 机器级安装；保留 `python.exe` 和 `py.exe`；不要预装未知项目的全局 pip 包 |
| Node.js 当前 LTS（x64） | 推荐 | JavaScript/TypeScript、前端构建、Node CLI | 机器级安装到 `C:\Program Files\nodejs`，随 Node 提供 `npm` |
| npm | 随 Node.js | 安装 JS 依赖和运行 npm scripts | 不单独安装第二套 npm；锁定 Node LTS 与 npm 版本 |
| OpenSSH Client | 可选推荐 | Git SSH、远程调试、代码仓库访问 | Windows 可选功能或受控安装；不预置私钥 |
| 7-Zip | 可选推荐 | 解压非标准归档、检查发布包 | 仅在团队确有使用需求时安装；普通 zip 优先用 Windows 原生命令 |

这一层是“开发能力”而不是 Codex 的启动依赖。若当前员工只使用 Codex 对话、文本和已有应用，不写 Python/Node 项目，可以先不安装 Python 和 Node，减少补丁、依赖和供应链风险。

### C. 按项目安装：不要放入所有镜像

以下工具有明确项目需求时再由管理员审核、安装并登记版本：

| 组件 | 典型用途 | 不默认预装的原因 |
|---|---|---|
| `pnpm` / `yarn` | 特定 JavaScript 项目 | 与 npm 的锁文件和全局版本可能冲突；组织应统一选择一种 |
| Java JDK | Java/Gradle/Maven | 体积和补丁维护成本较高 |
| Go | Go 项目本地编译 | 员工运行 Codex 不需要；Agent/Admin 已是编译后的二进制 |
| Rust / Cargo | Rust 项目 | 编译链体积大，需单独缓存与升级策略 |
| .NET SDK | .NET 项目 | 版本需要随项目锁定 |
| WSL2 | Linux 工具链兼容 | 需要虚拟化、内核和额外网络/磁盘管理 |
| Docker Desktop | 容器开发 | 需要虚拟化与更高权限，不应默认交给标准用户 |
| AWS/GCP/Azure CLI | 云资源运维 | 可能扩大凭据和云资源权限范围；按岗位发放 |
| 数据库客户端/本地数据库 | 项目测试 | 会产生额外端口、数据和清理责任 |

### D. 仅构建/发布机需要

这些工具不应因为 Codex 员工端需要而装进员工镜像：

| 组件 | 用途 | 当前依据 |
|---|---|---|
| Go（以仓库 `go.mod` 为准） | 构建 `agent.exe`、`admin.exe` | 当前仓库 `go.mod` 声明 Go 1.26；CI 通过 `go-version-file` 读取 |
| Node.js + npm | Codex-only 补丁、测试和 Web Gateway 构建 | `codex-only-local/BUILDING.md` 的构建要求，不是桌面运行要求 |
| PowerShell 7 (`pwsh`) | 执行 Codex 打包脚本 | 构建脚本需要；员工端只依赖 Windows PowerShell 5.1 |
| Inno Setup 6 | 生成 `setup.exe` | 构建安装包，不随员工镜像发布 |
| Git | 拉取源码、标签和发布记录 | 构建机需要，员工镜像按 B 层需求决定 |

Codex 的构建机可按 `codex-only-local/BUILDING.md` 单独维护，不要把该文档中的构建版本直接当成员工云电脑的强制预装版本。

## 二、当前建议的镜像方案

### 最小镜像（只需要受控使用 Codex）

- Windows 10/11 x64
- Codex 桌面包
- Agent 服务
- Windows PowerShell 5.1、`cmd.exe`
- Git（如果员工需要拉取或提交代码）
- AppLocker、`C:\Tools` 和 `Program Files` ACL 基线

这个方案不预装 Python、Node.js、Java、Go、Rust、Docker 或云 CLI。

### 通用开发镜像（当前更适合开发团队）

在最小镜像上增加：

- Git for Windows
- Python 3 x64
- Node.js 当前 LTS + npm
- OpenSSH Client
- 7-Zip（如确有归档需求）

然后把 Java、Go、Rust、.NET、WSL2、Docker 等留给项目或岗位镜像，不要继续堆进所有机器。

### 版本固定原则

不要在本文永久写死“最新版本”。版本应记录在镜像 BOM 或 Admin 软件清单中，并随镜像发布：

1. Python 使用组织批准的 3.x 小版本。
2. Node 使用组织批准的 LTS 小版本；npm 随 Node 一起验证。
3. Git、7-Zip、OpenSSH 按月或按季度补丁。
4. 版本升级先在一台验收机测试，再进入新镜像；不要让 Codex 自己下载并升级运行时。
5. 软件安装任务必须记录安装包 URL、SHA-256、静默参数、检测路径和安装日志。

## 三、安装位置、权限和 PATH 要求

推荐机器级安装位置：

```text
C:\Program Files\Git\
C:\Program Files\nodejs\
C:\Program Files\Python311\ 或组织批准的 Python 目录
C:\Program Files\7-Zip\
C:\Tools\Codex\
C:\Tools\ai-env-mgr.exe
```

要求：

- 工具安装目录由 Administrators/SYSTEM 写入，标准用户只读执行。
- `PATH` 采用机器级 PATH；安装后新建用户会话才能看到变更。
- 不要把可执行文件放在 `%TEMP%`、下载目录或员工可写目录，再通过 AppLocker 放行。
- 便携工具若必须放在 `C:\Tools`，需同时加入 Admin 下发的 AppLocker allow path，并校验目录 ACL。
- Python 虚拟环境、Node `node_modules` 和项目构建缓存应放在项目目录或用户目录，不写入受保护的公共安装目录。

## 四、验收清单

验收必须以标准员工用户进行一次；不能只以管理员或 Agent 的 SYSTEM 身份验证。

### 1. 基线命令

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

### 2. 版本和最小功能

```powershell
git --version
python --version
py --version
node --version
npm --version
ssh -V
```

未安装的可选工具显示“找不到命令”是预期结果，不代表 Codex 安装失败。

### 3. Codex 任务验收

至少验证以下任务，并保存一次任务日志：

- PowerShell：读取当前目录、创建并删除一个测试文件。
- Git：对测试仓库执行 `status`、`diff`，不使用真实密钥。
- Python：运行一个无网络的 `.py` 脚本；如安装了 Python，创建并删除临时 venv。
- Node：运行一个锁定依赖的最小 `npm` 脚本；如安装了 Node，验证 `npm ci` 不会要求交互输入。
- 归档：使用 Windows 原生 zip 或已批准的 7-Zip 解压测试包。
- Agent：确认软件安装、状态同步和日志上传不被长时间的构建命令阻塞。

同时检查 Windows AppLocker 日志：没有不应出现的 `powershell.exe`、`cmd.exe`、Git、Python 或 Node 阻断事件；不要为了消除阻断而放开 `wscript.exe`、`mshta.exe` 等脚本宿主。

## 五、安全和运维边界

- Python/Node 的包管理器会执行第三方代码。只允许组织批准的源、锁文件和项目依赖，禁止把 API Key 写入 `pip`、npm 配置或项目提交。
- 不在系统级全局安装项目依赖；优先 Python venv 和项目级 `node_modules`。
- Codex 可以调用已安装的命令，但“能调用”不等于“有权限访问所有目录、网络和云账号”。权限仍由 Windows 用户、AppLocker、网络策略和 Admin 下发策略共同决定。
- Agent 负责受控安装和更新，不应因为某个项目临时缺少 Python/Node 就自行下载未知版本。
- 任何新增语言运行时都要在软件清单中补齐：供应商、版本、来源、SHA-256、安装参数、检测规则、卸载方式、负责人和补丁周期。

## 六、最终建议

当前建议直接制作两种镜像：

1. **Codex 基础镜像**：Codex + Agent + Windows PowerShell/cmd + 安全基线；不强制 Python/Node。
2. **Codex 开发镜像**：基础镜像 + Git + Python 3 x64 + Node.js LTS/npm + OpenSSH；其他语言和 Docker 按项目单独安装。

这样既能保证 Codex 的基本可用性，也不会把不必要的运行时、包仓库和高权限工具带到所有员工机器上。
