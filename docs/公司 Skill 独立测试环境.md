# 公司 Skill 独立测试环境

2026-11-16。用于测试“后台派发 → Agent 同步 → Codex b13 加载”，线上后台 8090、定制工具测试后台 8850 保持原样。

## 当前测试入口

本机测试后台监听 `127.0.0.1:18864`，使用独立 PostgreSQL 实例中的 `aienv_skills_manual` 数据库、独立加密密钥和管理员账号。没有导入线上账号、设备令牌、业务数据或云服务密钥。Worker 关闭，对象存储写入禁止；网关、SLS 和无影云接口未接入。

Windows 通过临时 HTTPS 地址访问，后台和测试 Agent 使用同一个地址，无需 Windows SSH 隧道或额外的公网入站端口。服务端使用 [Cloudflare Quick Tunnel](https://developers.cloudflare.com/tunnel/get-started/quick-tunnels/)。该地址用于本次开发测试，停止并重新创建通道后会改变。

本机当前地址、首次登录信息和测试包在以下目录，均不提交 Git：

```text
/home/sun/.cache/ai-env-mgr-company-skills/manual-test/
  public-url.txt
  first-admin.txt
  master.key
  agent-skills-test.exe
  console.test
```

浏览器登录测试地址后，打开 `/test`：这里有 Agent 下载、启动命令和测试员工绑定表单。首次登录按现有流程设置验证器；线上账号不能用于这套独立测试库。

## Windows 测试步骤

1. 保留 Codex b13、已有模型登录和现有 Agent 服务。测试无感刷新时使用下节的完整新包；单独 `/test/download` 只提供新 Agent，不包含桌面适配。
2. 在管理员终端运行页面给出的命令，只运行一个测试 Agent 窗口：

   ```powershell
   .\agent-skills-test.exe skills-test --console https://本次测试地址
   ```

3. 让窗口持续运行，默认每 5 秒同步一次。在测试后台 `/test` 刷新，选择刚连接的电脑，填写员工实际使用 b13 的 Windows 用户名，创建并绑定。用户名可由员工终端中的 `whoami` 确认。
4. 到 `/skills` 新建或导入一个文字/模板 skill，选择测试员工试用或派发；观察应有版本和实际回报。
5. 在 b13 的新对话检查 skill 列表并调用；用普通员工账号验证公司原件只读。再测试更新、回退、撤回。安装新桌面适配并使用新 Agent 后，保持窗口打开，在新对话确认更新、回退与撤回；等待刷新时后台应明确显示，不强行关闭。
6. 结束时先在后台撤回。按 Ctrl+C 停止测试 Agent 后，也可执行以下命令清理本测试模式管理的内容：

   ```powershell
   .\agent-skills-test.exe skills-test --cleanup
   ```

测试结束并清理后，再安排正式 Agent 升级和验收，避免测试原件与正式 Agent 的管理记录发生同名冲突。

### 测试用 Skill

`/test/teenet-delivery-check-v1.zip` 和 `/test/teenet-delivery-check-v2.zip` 提供两个可导入的测试包。业务名称可填“公司技能下发测试”，英文 Skill 标识必须填 `teenet-delivery-check`。

v1 的源文件在 `examples/skills/teenet-delivery-check`。两个包的 SKILL.md 相同，仅 `references/check-marker.txt` 中的版本和校验码不同，用于检查附件更新和回退。先派发 v1，在 b13 新对话发送 `$teenet-delivery-check 做一次公司技能下发测试`；然后编辑同一个 skill 导入 v2，再开新对话验证附件版本变成 v2。

Skill 要求每次读取随包标记文件，返回实际版本、校验码和读取路径；读取失败必须如实报告。结合后台版本回报判断派发状态，不凭一句“成功”判断整条链路已验收。

### 不关闭 Codex 的自动刷新测试包

管理员登录同一个测试后台后下载 `/test/skill-live-refresh.zip`。完整解压，停止原测试 Agent（Ctrl+C），手动退出 Codex 一次，然后在新解压目录的管理员 PowerShell 执行：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\install-skill-live-refresh.ps1
.\agent-skills-test.exe skills-test
```

找不到目录时脚本会要求输入安装目录，仍支持 `-CodexInstallDir "D:\Codex"`。安装器检查锁定桌面版本与原生调用形态，备份 app.asar 并验证覆盖；不更改模型登录。已有 `Codex Skill Test` 快捷方式继续使用。首次安装关闭一次是为了载入新代码，此后更新/回退/撤回不再主动关闭窗口。

保持 Codex 打开，下载 `/test/teenet-delivery-check-v3.zip`，在后台编辑原 `teenet-delivery-check` 导入并保存，保持原派发员工。不要新建另一个同名 Skill。等设备回报后，在 Codex **新建对话而不退出程序**，发送 `$teenet-delivery-check 做一次公司技能下发测试`。

v3 同时改变描述、正文和附件，预期输出五行，包含 `版本：v3`、`校验码：TEENET-SKILL-CHECK-V3-6C91E2`、`指令校验：TEENET-BODY-V3-91A6F0`，及机器级 `references\check-marker.txt` 路径。正文校验值没有放进附件，可区分只读到新附件与已加载新指令。后台数值版本按实际保存次数递增，不要求恰好为 3。

然后保持窗口打开，回退旧版本并开新对话验证；撤回后确认目录不存在，新对话无法调用。已进入旧对话的指令不能追溯撤销。若后台显示“文件已同步，等待 Codex 刷新”，检查新 Agent 和适配是否均安装，不靠关闭进程冒充自动刷新。

包内 `kit-manifest.json` 记录 Agent/桌面源提交及每个文件 SHA-256，不包含模型 Key。需撤销适配时先退出 Codex，执行同一安装脚本追加 `-Rollback`，按原始 ASAR 备份精确恢复；如桌面此后被其他安装器升级，拒绝覆盖。

### 新机器的模型接入

测试 Agent 不负责首次模型登录。b13 尚未配置模型时，管理员登录测试后台后可从 `/test/codex-model-setup.zip` 下载专用接入包。该下载只允许管理员，禁止公共缓存；包内含临时模型 Key，不提交 Git，不与公开 Skill ZIP 混放到匿名下载入口。

在绑定员工的 Windows 账号下解压，先彻底退出 Codex，再从解压目录执行：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\setup-codex-skill-test.ps1
# 如果 Codex 安装在其他目录，追加 -CodexInstallDir "D:\Codex"
```

安装目录优先从 Codex 安装器的 HKCU/HKLM 卸载注册项读取，支持安装器沿用其他盘的旧位置；便携包可从当前会话中的运行进程定位，也会检查常见用户和 Program Files 目录。显式指定目录时检查启动器、桌面程序和内置 Node，缺失时报告具体目录及预期文件，不再仅假设 `%USERPROFILE%\Codex`。

脚本复用 b13 自带 Node，兼容现有 PowerShell ConstrainedLanguage 环境。它将连接、模型目录和 API Key 模式配置放在 `%LOCALAPPDATA%\TEENet\CodexSkillTest`，限制目录访问权限，创建 `Codex Skill Test` 桌面快捷方式；通过该入口设置进程级 `CODEX_HOME` 后调用原 b13 启动器。原 Codex 快捷方式仍使用原配置。测试 Agent 继续向机器级目录下发 Skill。

本次接入包限定 `weipeng`，连接生产网关 `https://litellm.teenet.app/v1`，使用独立 Key `skill-test-weipeng-20261116`，模型 `gpt-6.1-sol`，7 天有效、总额度上限 5 美元。没有轮换既有员工 Key。模型目录由现有 `catalog.Build` 生成。安装后删除下载的 ZIP 和解压的 `connection.json`；测试完毕可撤销该 Key 并删除测试目录和快捷方式。

本地验证包括模型真实 Responses 请求、独立目录写入及原配置保留、错误用户/过期/权限失败拒绝、PowerShell 语法和管理员下载权限。2026-11-16 用户已使用本包接入 b13，并完成下述 v1 首次读取、v2 更新读取、回退 v1 读取和撤回；Windows 普通员工权限、原生 ACL 自动化和其他下发场景仍待验证。

#### 真机：独立 CODEX_HOME 的执行器被 AppLocker 拦截

2026-11-16，Agent 上报 `teenet-delivery-check` v1 为 `applied`，b13 尝试读取附件时返回错误 1260。真机 AppLocker 8004 明确记录拦截 `%OSDRIVE%\USERS\WEIPENG\APPDATA\LOCAL\TEENET\CODEXSKILLTEST\.SANDBOX-BIN\CODEX-COMMAND-RUNNER-0.153.4.EXE`。根因是独立测试 `CODEX_HOME` 改变了命令执行器的位置；现有 Agent 的本机兼容规则只覆盖用户 `.codex\.sandbox-bin` 和 `.cache\codex-runtimes`，测试 Agent 不修改这些规则。

仅对这台测试机，由管理员合并已确认执行器的哈希规则：

```powershell
$runnerPath = 'C:\Users\weipeng\AppData\Local\TEENet\CodexSkillTest\.sandbox-bin\codex-command-runner-0.153.4.exe'
Get-AppLockerFileInformation -Path $runnerPath -ErrorAction Stop |
    New-AppLockerPolicy -RuleType Hash -User 'S-1-1-0' -RuleNamePrefix 'CodexSkillTest' -ErrorAction Stop |
    Set-AppLockerPolicy -Merge -ErrorAction Stop
```

`-Merge` 保留已有规则及目标策略的强制模式，见 [Microsoft Set-AppLockerPolicy](https://learn.microsoft.com/en-us/powershell/module/applocker/set-applockerpolicy)。该规则只接受同一执行器内容的哈希；更换执行器版本需要重新验证。没有放行整个用户目录，也没有关闭沙箱或 AppLocker。完成后重开 `Codex Skill Test`，在新对话中重测附件读取；结果仍须以真机回报为准。

随后真机最新 8004 事件（14:31:55、14:32:01）显示下一处拦截为 `C:\Users\weipeng\.cache\codex-runtimes\codex-primary-runtime\dependencies\native\powershell\pwsh.exe`。这是 Codex 下载的 PowerShell，不是系统的 `powershell.exe`；测试 Agent 不管理原 Agent 的运行时兼容规则，因此该文件也需要单独的本机哈希规则：

```powershell
$runtimePath = 'C:\Users\weipeng\.cache\codex-runtimes\codex-primary-runtime\dependencies\native\powershell\pwsh.exe'
Get-AppLockerFileInformation -Path $runtimePath -ErrorAction Stop |
    New-AppLockerPolicy -RuleType Hash -User 'S-1-1-0' -RuleNamePrefix 'CodexSkillTest-PowerShell' -ErrorAction Stop |
    Set-AppLockerPolicy -Merge -ErrorAction Stop
```

同时回传的 `Test-AppLockerPolicy` 对 `weipeng` 显示 runner 为 `Allowed`，匹配 `Admins-allow-all`。这只证明该用户通过管理员规则；不能代替沙箱账号的实际执行验证，也不能据此确认新增哈希规则被命中。最终仍需新对话实际读取标记文件成功。若继续出现 1260，应先核对最新事件的时间和进程路径，不能将所有后续拦截都当作最初的 runner 拦截。

两处处理后，用户已回传 b13 的实际执行结果：`teenet-delivery-check`、`v1`、`TEENET-SKILL-CHECK-V1-8D42C7`，读取路径为 `C:\ProgramData\OpenAI\Codex\skills\teenet-delivery-check\references\check-marker.txt`，并确认可以使用。随后同一 Skill 导入 v2，Agent 于 14:39:19（Asia/Shanghai）回报版本 2、`applied`，用户回传同一路径中的版本 `v2` 和校验码 `TEENET-SKILL-CHECK-V2-B7359A`。再选择历史版本 v1，Agent 于 14:41:22 回报版本 1、`applied`，用户读取结果也恢复为 v1 和对应校验码。

随后撤回该 Skill，Agent 于 14:45:22 回报 `applied`、`skills: []`、无错误。用户检查机器级 Skill 目录得到 `False`，Codex 当前会话可用技能列表也不再包含该项。撤回通过；此时要求执行已撤回 Skill 而得到“未通过/不可用”属于预期。首次下发、更新、回退和撤回四项基本流程通过。

用户又一次新增 v1、更新 v2，确认新增不关闭、更新会关闭，重开后读取到 v2。以上为旧 Agent 的实测记录。用户随后要求接入无感刷新，当前新包已移除公司 Skill 同步时的关闭逻辑；原来下载到 Windows 的旧二进制不会自动替换，必须停止旧进程并使用新包。

## 测试 Agent 的边界

测试模式复用正式 Agent 的 `agentcore.Syncer`、设备配置 API 和 `managedskills.Manager`，实际写入 Codex 的机器级 skill 目录，并使用实际 Windows 权限和缓存刷新逻辑。

该模式的状态位于 `%ProgramData%\AIEnvMgr\skills-test`，不读取或替换正式 Agent 的设备令牌。凭据、浏览器策略、AppLocker、采集、普通软件安装和 Agent/Codex 自更新不在本次测试范围内；现有模型登录保留。机器详情里的策略和凭据字段不是这次验收的依据，使用 skill 的版本回报判断派发结果。

下载包通过 `main.skillsTestBuild=1` 构建，仅接受 `skills-test` 和 `version` 命令，不能安装、停止或替换现有 Agent 服务。测试模式仅接受本机回环或临时 `trycloudflare.com` 地址，拒绝连接线上控制台。

## 复建与管理

独立后台由测试二进制中的 `TestManualSkillsConsole` 启动，正式 admin 构建不包含该入口。它复用正常登录、角色、CSRF、Skill 管理和设备 API。额外的 `/test/bind` 仅允许已登录管理员在测试库创建员工和绑定设备。

关键环境变量：`SKILLS_MANUAL_CONSOLE=on`、`SKILLS_MANUAL_PG_DSN`、`SKILLS_MANUAL_KEY_FILE`、`SKILLS_MANUAL_PUBLIC_URL`、`SKILLS_MANUAL_AGENT_FILE`。默认监听地址为 `127.0.0.1:18864`，只允许回环监听；始终使用专门的 `aienv_skills_manual` 数据库，不重置已有测试数据。

```sh
cd go
go test -c -o /path/to/console.test ./internal/adminweb
# 设置上述专用测试环境变量后：
/path/to/console.test -test.run '^TestManualSkillsConsole$' -test.v -test.timeout=0

# Windows 测试包：把地址替换为当前 HTTPS 测试地址。
GOOS=windows GOARCH=amd64 go build -trimpath \
  -ldflags '-s -w -X main.skillsTestBuild=1 -X main.consoleURL=https://本次测试地址' \
  -o agent-skills-test.exe ./cmd/agent
```

当前服务端以用户服务运行：

```sh
systemctl --user status ai-env-skills-test ai-env-skills-test-tunnel
systemctl --user stop ai-env-skills-test ai-env-skills-test-tunnel
```

停用测试环境只操作上述用户服务；不要停止系统级 `ai-env-mgr-console.service`。

本地已验证 HTTPS 登录页、设备接口入口、下载包摘要、测试绑定的管理员/CSRF 门禁，以及测试模式下未绑定、凭据撤回和收到新凭据时保留原登录。Windows 上已完成实际启动、b13 v1 调用、v2 更新读取、回退 v1 读取和撤回；普通员工权限及其他场景仍待现场验收。

## 本次无感刷新验证记录

2026-11-16：Agent Windows 原生测试 CI 43825288779 通过，包含公司文件与刷新清单 ACL；桌面 CI 43825935754 完整回归及真实 `0.153.4` 同进程更新、回退、撤回探测通过。本机最终 ZIP 自带依赖的真实 ASAR 安装、重复安装及精确回退验证通过。用户已回传实际 b13 的 v3 正文和附件读取结果，二者与测试包一致；用户随后明确确认全程未关闭 Codex，后台更新后新建对话验证，因此本机“不关闭窗口更新”人工验收通过；随后保持窗口打开的回退 v2 和撤回也经用户确认通过。真实长任务保持仍待现场验证。

当前 ZIP 源版本为 Agent `c72bb68`、桌面 `afb63f0`，后续提交仅补探测和文档，不影响包内运行时代码。`skill-live-refresh.zip` SHA256 为 `b28f5e89a0a3326c1c85fa87a7d9754c531097b1dbab8e9a5fda528eeb92037f`。v3 ZIP SHA256 为 `838378ec06acf3c2669b578a81bfebd90c5a5846dabac23fa49d3e55657bd9e5`。下载均在原独立测试后台；员工、派发和目录数据没有自动更改。

用户真机 v3 结果：`版本：v3`、附件 `TEENET-SKILL-CHECK-V3-6C91E2`、正文 `TEENET-BODY-V3-91A6F0`，路径为 `C:\ProgramData\OpenAI\Codex\skills\teenet-delivery-check\references\check-marker.txt`。2026-11-16 15:59:44（Asia/Shanghai）设备最近回报版本 3、`applied`、无错误，内容 SHA256 为 `b58b471ed98b6585a22aa8013c6cbfec27a6feac8bf3922b145182511e122ce4`。完整记录见主验收文档“用户 b13 真机”一节。

2026-11-16 用户随后确认不关闭窗口的回退 v2、撤回均可用：回退返回 v2、`TEENET-SKILL-CHECK-V2-B7359A`，输出恢复为旧版四行；撤回后新对话不再列出或调用该 Skill，且不沿用历史校验码。16:04:28（Asia/Shanghai）设备最终回报 `applied`、`skills: []`、无错误，generation 为 `2c57979a12ae06a1b10013c6b064ca96`。本机无感更新、回退、撤回的基本生命周期验收通过，生产环境保持原样。
