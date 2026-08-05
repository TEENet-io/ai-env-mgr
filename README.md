# Airlock

**受控 AI 沙盒** —— 让员工在企业云电脑上使用 Codex / Claude / ChatGPT，同时做到：

- **员工不知道 AI 账户密码** —— 管理员代为登录，凭据由 agent 自动投递到位
- **浏览器进不去外部 AI 网站** —— 企业策略 + AppLocker，标准用户无法解除
- **离职能远程吊销** —— 删掉云端凭据，机器下次同步就清掉本地登录态

气闸(airlock)的意思：进出只有一条受控通道，钥匙不在使用者手上。

面向阿里云无影共享型云电脑（文件系统隔离、无 AD 域），也适用于任何 Windows 桌面群。

## 组成

| 组件 | 位置 | 作用 |
|---|---|---|
| `dist/agent.exe` | 每台云电脑（进镜像） | Windows 服务，定时从 OSS 拉取策略与凭据并应用到本机 |
| `dist/admin.exe` | 管理员工作机 | 代员工登录 AI 工具、下发封禁策略、查看全局状态 |
| `scripts/01-Install-AITools.ps1` | 模板机 | 安装 Codex CLI / Claude Code / ChatGPT 应用 |
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

```powershell
admin.exe user add work1 --codex ai-work1@company.com --claude ai-work1@company.com
admin.exe login --user work1 --tool all
```

用镜像开一台云电脑、创建同名 Windows 标准用户即可。员工登录后 Agent 自动把凭据投递到位。

## 常用命令

```powershell
admin.exe status                        # 查看所有机器
admin.exe add-site gemini.google.com    # 增加封禁网站
admin.exe disable-block                 # 临时全局开放
admin.exe enable-block                  # 恢复封禁
admin.exe set-interval 5                # 调整同步频率
admin.exe login --user work1 --tool all # 凭据过期重新登录
```

## 文档

| 文件 | 内容 |
|---|---|
| `dist/部署说明.md` | **从这里开始** —— OSS 配置、镜像制作、日常运维、排障 |
| `OSS布局.md` | **OSS 结构、对象格式、RAM 权限的唯一权威来源** |
| `架构说明.md` | 架构决策与取舍 |
| `实施计划.md` | 功能详述与模块划分 |
| `操作手册.md` | 员工入职离职流程 |

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
