# AppLocker 与 Codex 启动

给谁看：客户的 IT 管理员。
背景：2026-09-09 在一台真机上复现并找到了这个故障的原因。本文记录现象、原因和正确修法。

> 注意：**以文末「2026-09-14 镜像策略修订」为准**。修订后对员工放开了 `cmd.exe` 和 `powershell.exe`，现在的 `scripts/02-Manage-AIAccess.ps1` 已经这样做了。正文里说 `cmd.exe`、`powershell.exe` 不能放行的地方，是修订前的做法；`wscript.exe`、`cscript.exe`、`mshta.exe` 仍然封禁。

## 先认识几个词

| 词 | 意思 |
|---|---|
| AppLocker | Windows 自带的程序白名单。不在名单里的程序和脚本不能运行。 |
| 强制模式（Enforce） | AppLocker 真的拦截不合规的程序。 |
| 审计模式（Audit） | AppLocker 只记录、不拦截。 |
| 规则集合 | AppLocker 按文件类型分组管理规则：Exe（可执行程序）、Script（脚本）、DLL、MSI（安装包）、Appx（商店应用）。 |
| 例外名单 | 某条放行规则里单独排除的文件。规则放行一个目录，例外名单里的文件仍然不能运行。 |
| 脚本宿主 | 负责运行脚本的 Windows 程序，例如 `wscript.exe`、`cscript.exe`、`mshta.exe`。 |
| 镜像 | 新云电脑装机用的系统模板。 |
| Agent | 装在每台云电脑上的管理程序（`agent.exe`），定期从控制台同步策略。 |
| 控制台 | 管理员用的网页后台。 |
| ACL | 文件夹的访问权限设置，决定谁能读、写、执行。 |
| 本分支 | 指 agent 1.2.13 这次改动。 |

---

## 现象

员工在云电脑上打开 Codex（`C:\Tools\Codex` 下的图标或快捷方式），Windows 弹出：

> 系统管理员已阻止这个应用。有关详细信息，请与系统管理员联系。

这是 AppLocker 在**强制模式**下拒绝运行程序时的标准提示。在故障机器上运行 `agent.exe status`，输出里有 `applocker=Enforce`，意思是 AppLocker 处于强制模式，也印证了这一点。

---

## 原因：先后被拦了两次

Codex 默认这样启动：

```
C:\Windows\system32\wscript.exe "C:\Tools\Codex\Codex.vbs"
```

也就是先用 `wscript.exe` 运行脚本 `Codex.vbs`。从故障机器上读出的 `Codex.vbs` 内容，它只做两件事：

1. 设置环境变量 `CODEX_ELECTRON_ENABLE_WINDOWS_COMPUTER_USE=1`；
2. 把工作目录切到 `C:\Tools\Codex\_internal\app`，再启动这个目录下的 `ChatGPT.exe`。

这条启动路线会先后碰上两道互不相关的拦截。

### 第一道拦截：`wscript.exe` 在镜像 AppLocker 的例外名单里

镜像脚本 `scripts/02-Manage-AIAccess.ps1` 的 Exe 规则集合放行了 `%WINDIR%\*`（Windows 目录下的所有程序）。但这条规则的例外名单明确排除了 `wscript.exe`，还有 `cmd.exe`、`powershell.exe`、`cscript.exe`、`mshta.exe`。见 `scripts/02-Manage-AIAccess.ps1:114-131`：

```powershell
<FilePathRule Id="a0000000-0000-0000-0000-000000000002" Name="Everyone-allow-Windows" ...>
  <Conditions><FilePathCondition Path="%WINDIR%\*" /></Conditions>
  <Exceptions>
    ...
    <FilePathCondition Path="%SYSTEM32%\wscript.exe" />
    <FilePathCondition Path="%SYSTEM32%\cscript.exe" />
    <FilePathCondition Path="%SYSTEM32%\mshta.exe" />
    ...
  </Exceptions>
</FilePathRule>
```

所以启动在 `wscript.exe` 这一步就被拒绝了，**根本轮不到 Codex 本身**。

### 第二道拦截：就算绕过第一道，`Codex.vbs` 也会被 Script 规则集合拦下

同一个脚本的 Script 规则集合，只放行 `%WINDIR%\*` 和 `%PROGRAMFILES%\*` 下的脚本（同文件 140-142 行附近）。`C:\Tools\Codex\Codex.vbs` 不在这两个目录里，所以在 Script 集合这里又被拒绝一次。

### 脚本这关放行了也不够：Codex 目录本身不在白名单里

就算脚本这关能过，真正要运行的程序是 `C:\Tools\Codex\_internal\app\ChatGPT.exe`。镜像的 AppLocker 只放行 `%WINDIR%\*` 和 `%PROGRAMFILES%\*`，`C:\Tools\Codex` 不在其中，同样会被拒绝。

本分支（agent 1.2.13）新增的 `appLockerAllowPaths`（放行目录配置）就是解决这一部分的，但只解决这一部分。

**结论：只加白名单目录，修不好这台机器。** 两道拦截必须一起解决，缺一不可。

---

## 为什么不放行 `wscript.exe`

最省事的"修法"是把 `wscript.exe` 从例外名单里去掉。这条路被否决了，原因如下：

1. 标准用户可以往 `C:\Windows\Temp` 里写文件。
2. 镜像的 Script 规则集合允许运行 `%WINDIR%\*` 下的任意脚本，`C:\Windows\Temp` 也在其中。
3. 一旦重新放行 `wscript.exe`，员工只要把一个 `.vbs` 或 `.js` 文件放进 `C:\Windows\Temp`，再运行 `wscript.exe C:\Windows\Temp\x.vbs` 就行。
4. 这个脚本的路径是规则允许的，但内容可以是任何东西，比如下载并启动任意 exe、修改注册表。

例外名单从一开始就是为了堵这个口子：**脚本宿主 + 用户能写的目录 = 绕过整个 AppLocker 白名单**。

`cscript.exe`、`mshta.exe` 同理，都不能放行。这份例外名单不会因为 Codex 的启动方式而放松。（修订前 `cmd.exe`、`powershell.exe` 也在这个名单里，2026-09-14 修订后已对员工放开，见文末。）

---

## 正确做法

修复分两部分，缺一不可。

### 1. 加白名单目录（本分支已实现，由控制台下发）

在控制台「封禁策略」页的「AppLocker 放行目录」里，新增 `C:\tools\Codex\*`。

Agent 每次同步都会用这个配置重写本机 AppLocker 的 Exe 规则集合（代码见 `go/internal/agentcore/sync.go`、`go/internal/policy/applocker_windows.go`）。不需要改装机脚本，也不需要重新做镜像。

这一步解决"目录不在白名单"，**不解决 `wscript.exe` 被拦**。

**放行目录有硬性限制，控制台和 Agent 都会检查并拒绝不合规的目录：**

- **用户能写的位置不行**：`C:\Users\`、`%USERPROFILE%`、`%LOCALAPPDATA%`、`%APPDATA%`、`%TEMP%`、`%PUBLIC%`……
- **整个 Windows 目录树都不行**：`C:\Windows\*`、`C:\Windows\System32\*`、`%OSDRIVE%\Windows\*` 都会被拒。原因：
  - AppLocker 的多条放行规则是**叠加**的，任何一条放行，程序就能运行。
  - `<Exceptions>`（例外名单）只对它所在的那一条规则有效。
  - 镜像里的 `%WINDIR%\*` 规则带着例外名单（排除了 wscript.exe / cmd.exe / powershell.exe / cscript.exe / mshta.exe）。
  - 如果再加一条覆盖 Windows 目录的放行规则，这条新规则**没有任何例外**。原来的例外名单就等于作废，上一节说的"脚本宿主 + 用户能写的目录"口子会马上重新打开。
  - Windows 目录下没有任何东西需要额外放行。
- **范围太大的目录也不行**：`C:\*`（整个 C 盘）和 `C:\ProgramData\*`（等同于 `%PROGRAMDATA%\*`）都会被拒。放行目录必须具体到某个厂商或工具的目录，例如 `%PROGRAMDATA%\Vendor\App\*`。
- **两个方向都检查**：路径落在禁止位置里面，会被拒；路径把禁止位置包含在自己下面，也会被拒。
- **最多 64 条**：这份名单是给几个工具目录用的，不是用来批量粘贴的。

### 2. 让启动不再经过脚本宿主

`Codex.vbs` 做的两件事，都可以不用脚本完成：

- 把 `CODEX_ELECTRON_ENABLE_WINDOWS_COMPUTER_USE=1` 设成**机器级环境变量**。方法是 `setx /M`，或写注册表 HKLM `System\CurrentControlSet\Control\Session Manager\Environment`。设置一次，对这台机器上所有用户永久生效。
- 把快捷方式改成直接指向 `C:\Tools\Codex\_internal\app\ChatGPT.exe`，工作目录设为这个 `app` 目录，不再经过 `wscript.exe`。

两步做完，启动过程中不再有任何脚本宿主，两道拦截都不会再挡路。`ChatGPT.exe` 本身在第 1 步放行的 `C:\tools\Codex\*` 下面，可以正常运行。

---

## 验证

1. 等员工机器完成一次同步（等一个同步周期，或管理员在这台机器上手动触发）。然后运行 `agent.exe status`，确认输出里有：

   ```
   applocker_allow=1
   ```

   这个数字是放行目录的条数，应该和控制台发布的条数一致。

   **`applocker_allow=?` 和 `0` 不是一回事：**
   - `?` 表示 Agent 这次没能读到本机的 AppLocker 策略，意思是"不知道"。
   - `0` 表示"读到了，但目前没有任何放行目录"。

   两者处理方式不同。看到 `?` 时，不要当成"没配置"再下发一遍。应该去看 Agent 日志里读取失败的那一行，找具体原因。Agent 会把 PowerShell 的错误输出（stderr）一起记下来，超时也会写明。

   **`?` 不代表 AppIDSvc 服务没启动。** AppIDSvc 是让 AppLocker 规则真正生效的 Windows 服务。Agent 用 `Get-AppLockerPolicy -Local` 读策略，它从本机策略存储里读，不依赖 AppIDSvc。AppIDSvc 决定的是策略**是否被执行**，不决定能不能读出来。所以这个服务没启动时，`applocker_allow` 照样能读到数字，只是规则不生效。

2. 按第 2 步改好启动方式后，让员工重新打开 Codex。确认不再弹出「系统管理员已阻止这个应用」。

3. 打开事件查看器 `Applications and Services Logs → Microsoft → Windows → AppLocker → EXE and DLL`，确认这次启动**没有新增 8004 事件**（8004 表示拒绝）。
   - 修复前，8004 会记录 `wscript.exe` 和/或 `ChatGPT.exe` 被拒绝。
   - 修复后，应该只看到 8002 事件（表示允许，前提是开了审计），或者完全没有新记录。

---

## 还没做完的事

本分支（agent 1.2.13）只做了"白名单目录"这一半。下面两件事**没有做**，留给后续：

1. **机器级环境变量目前要手动设置。** Agent 已经会写 HKLM 下的策略注册表项（浏览器封禁、AppLocker 放行目录都是这样做的）。把少量机器环境变量也放进 `policy.json` 管理，是顺理成章的扩展，但这次没做。

2. **codex-kiosk 安装器仍然会创建 `wscript.exe` + `.vbs` 的快捷方式。** 根本解决办法是让安装器直接创建指向 `ChatGPT.exe` 的快捷方式，不经过脚本宿主。这要改 [`TEENet-io/codex-kiosk`](https://github.com/TEENet-io/codex-kiosk) 仓库，不在本仓库的改动范围内。

这两件事完成前，每台用当前安装器新装的机器，都会再碰到"第二道拦截"，需要按上面「正确做法」第 2 步手动改一遍。

---

## 2026-09-14 镜像策略修订（重做基础镜像前定稿）

Codex 的安装目录继续用 `C:\Tools\Codex`。Agent 下发的放行项 `C:\tools\Codex\*` 会覆盖这个目录下的全部程序。路径规则对子目录也生效，所以 Codex 自带的辅助程序不需要一个个放行。

为此，`scripts/02-Manage-AIAccess.ps1` 改了四处：

1. **运行 `-Init` 时收紧 `C:\Tools` 的权限（ACL）**：
   - 断开权限继承；SYSTEM 和 Administrators 完全控制；Users 只能读取和执行。
   - 原因：Windows 默认让 `C:\` 下新建的目录继承"Authenticated Users 可修改"的权限。这样员工能往放行目录里放任意 exe，放行规则就成了漏洞。
   - Agent 以 SYSTEM 身份安装 Codex，不受影响。Codex 运行时的数据存在用户的 AppData 里。
2. **对员工放开 `cmd.exe` 和 `powershell.exe`**：
   - Codex 在 Windows 上通过 PowerShell 执行任务。封掉的话，Codex 只能改文件，不能跑命令。
   - `wscript`、`cscript`、`mshta`、`regedit`、`reg` 继续封。
3. **`%WINDIR%\*` 的 Exe 和 Script 两条规则，都排除标准用户能写的 Windows 子目录**：
   - 包括 Temp、Tasks、tracing、spool、Com\dmp、FxsTmp、MachineKeys 等 15 处。
   - 这是放开命令行之后，堵"脚本宿主 + 能写的目录"口子的标准做法：员工能跑 PowerShell，但他在 Windows 目录里能写的地方都不在白名单内；Program Files 和 `C:\Tools` 他又写不了。
4. **Exe 规则保持只有一个规则集合**：
   - 镜像里只放固定规则。
   - Agent 每个同步周期把它管理的放行规则（id 以 `e0000000-` 开头）写进同一个集合。
   - 如果发现有多个 Exe 集合，Agent 会拒绝处理。

其他规则集合：

- DLL 集合不启用；
- MSI 仍然只允许管理员；
- Appx 仍然只允许已签名的。

镜像默认是审计模式（Audit）。新机器跑完真实任务后，用 `applocker-audit-events.ps1` 确认 Codex 目录以外没有被拦的情况，再从后台切成强制模式（Enforce）。

启动方式：

- 镜像里的快捷方式必须直接指向 `_internal\app\ChatGPT.exe`（codex-kiosk 安装器已经改了）。
- 环境变量 `CODEX_ELECTRON_ENABLE_WINDOWS_COMPUTER_USE=1` 设为机器级。
- 已经部署的机器，用 `fix-codex-launch.ps1` 逐台修复。
