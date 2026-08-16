# AI Env Mgr

**受控 AI 沙盒** —— 让员工在企业云电脑上使用 Codex / Claude / ChatGPT，同时做到：

- **员工不知道 AI 账户密码** —— 管理员代为登录，凭据由 agent 自动投递到位
- **浏览器进不去外部 AI 网站** —— 企业策略 + AppLocker，标准用户无法解除
- **离职能远程吊销** —— 删掉云端凭据，机器下次同步就清掉本地登录态

AI Env Mgr（AI 环境管理器）：员工的 AI 工具环境——登录、策略、凭据、下线——集中由管理员一处掌控，进出只有一条受控通道，钥匙不在使用者手上。

面向阿里云无影共享型云电脑（文件系统隔离、无 AD 域），也适用于任何 Windows 桌面群。

## 组成

| 组件 | 位置 | 作用 |
|---|---|---|
| `dist/agent.exe` | 每台云电脑（进镜像） | Windows 服务，定时从 OSS 拉取策略与凭据并应用到本机 |
| `dist/admin.exe` | 管理员工作机或一台内网服务器 | 管理控制台(浏览器):代员工登录、下发策略、查看机队、发布更新 |
| `scripts/01-Install-AITools.ps1` | 模板机 | 安装 Codex CLI / Claude Code / ChatGPT（CLI 机器级全用户；GUI 预置全用户，`-AllExistingUsers` 覆盖已有用户） |
| `scripts/02-Manage-AIAccess.ps1` | 模板机 | 配置 AppLocker |

前两者是 Go 编译的单文件二进制，无运行时依赖。后两者只在制作镜像时执行一次。

## 三步上手

**1. 准备 OSS**

创建私有 Bucket，建两个 RAM 用户（管理员读写、Agent 受限）。

两把密钥分别填进各自的源码，再编译。产出的两个 exe 自带配置，不跟任何配置文件：

```bash
# go/cmd/agent/credentials.go  ← 受限密钥 + 内网 endpoint
# go/cmd/admin/credentials.go  ← 读写密钥 + 公网 endpoint
cd go && make all
```

详见 `dist/部署说明.md`。

**2. 制作镜像**

在模板机上装 AI 工具、配 AppLocker、装 Agent 服务，然后制作自定义镜像。

**3. 新增员工**

在控制台的**员工**页登记,再到**代员工登录**页用该员工的账号完成一次 OAuth。
然后用镜像开一台云电脑、创建同名 Windows 标准用户即可——员工登录后 Agent 自动把凭据投递到位。

## 管理控制台

不带参数运行 `admin.exe`,浏览器打开 `http://127.0.0.1:8080`,用 OSS 的 AccessKey 登录:

```powershell
admin.exe
```

**密钥只在进程内存里**,不写磁盘、不进 cookie、不进日志;关掉程序即全部登出。

| 页面 | 能做什么 |
|---|---|
| 机器 | 机队状态、绑定/解绑/注销、看某台机器的日志 |
| 员工 | 登记、停用(**并吊销凭据**)、启用 |
| 代员工登录 | 替员工完成 Codex / Claude 的 OAuth |
| 封禁策略 | 增删封禁域名、临时全局开关 |
| 会话采集 | 同步间隔、采集开关与统计 |
| 文件传输 | 上传文件、生成免密下载链接 |
| 发布更新 | 发布 agent / Codex 到机队(需打字确认,Codex 有灰度) |
| 策略总览 | 只读汇总 |

要给团队共用、带域名和 HTTPS,见 [Web控制台部署.md](docs/Web控制台部署.md)。

> 命令行和 TUI 已在 v1.2.7 移除:同一批操作维护两个前端,两边会各自漂移,而每次分歧都是没人察觉的 bug。

## 文档

| 文件 | 内容 |
|---|---|
| `dist/部署说明.md` | **从这里开始** —— OSS 配置、镜像制作、日常运维、排障 |
| `docs/OSS布局.md` | **OSS 结构、对象格式、RAM 权限的唯一权威来源** |
| `docs/架构说明.md` | 架构决策与取舍 |
| `docs/操作手册.md` | 员工入职离职流程 |
| `docs/企业微信集成.md` | 企业微信告警接入 + admin 机器人可行性调研 |

## 从源码构建

```bash
cd go
make check    # gofmt + vet + test + Windows 编译
make all      # 产出 dist/agent.exe 和 dist/admin.exe
```

需要 Go 1.22+。默认交叉编译到 `windows/amd64`。

## 设计要点

- **策略走 OSS，AppLocker 进镜像** —— 封禁域名要能随时改，所以走配置中心；AppLocker 配错影响面大，固化在镜像里用灰度替换更安全。
- **Agent 不需要用户密码** —— 写别人 profile 和 HKLM 只需 SYSTEM 权限，Agent 不模拟登录。
- **凭据由管理员代持** —— 员工始终不知道 AI 账户密码。
- **休眠唤醒立即同步** —— 定时器在休眠时冻结，所以叠加了电源事件和墙钟超期检查。
