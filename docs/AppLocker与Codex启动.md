# AppLocker 与 Codex 启动

> 面向：客户 IT 管理员
> 背景：2026-09-09 在一台真机上复现并定位的故障，记录现象、根因、正确修法。

---

## 现象

员工在云电脑上打开 Codex（`C:\Tools\Codex` 下的图标/快捷方式），Windows 弹出：

> 系统管理员已阻止这个应用。有关详细信息，请与系统管理员联系。

这是 AppLocker 在**强制模式**下拒绝执行的标准提示。故障机器上 `agent.exe status` 也确认了这一点：`applocker=Enforce`。

---

## 根因：两处拦截，先后发生

Codex 的默认启动方式是：

```
C:\Windows\system32\wscript.exe "C:\Tools\Codex\Codex.vbs"
```

`Codex.vbs` 只做两件事（在故障机器上读出来的）：设置环境变量
`CODEX_ELECTRON_ENABLE_WINDOWS_COMPUTER_USE=1`，把工作目录切到
`C:\Tools\Codex\_internal\app`，再启动该目录下的 `ChatGPT.exe`。

这条启动链会依次撞上两道独立的墙：

**第一道墙：`wscript.exe` 在镜像 AppLocker 的例外名单里。**

镜像脚本 `scripts/02-Manage-AIAccess.ps1` 里，Exe 规则集允许 `%WINDIR%\*`，但显式排除了
`wscript.exe`（连同 `cmd.exe`、`powershell.exe`、`cscript.exe`、`mshta.exe`）——见
`scripts/02-Manage-AIAccess.ps1:114-131`：

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

所以启动在 `wscript.exe` 这一步就被拒绝了——**根本轮不到 Codex 本身**。

**第二道墙：即便绕过第一道，`Codex.vbs` 也会被 Script 规则集拦下。**

同一脚本的 Script 规则集只允许 `%WINDIR%\*` 和 `%PROGRAMFILES%\*` 下的脚本（同文件
140-142 行一带），`C:\Tools\Codex\Codex.vbs` 两者都不在，会在 Script 集合上再挨一次拒绝。

**这条分支放行了也不够：目录本身不在白名单。** 就算脚本关能过，真正要执行的可执行文件是
`C:\Tools\Codex\_internal\app\ChatGPT.exe`，而镜像的 AppLocker 只放行
`%WINDIR%\*` 和 `%PROGRAMFILES%\*`，`C:\Tools\Codex` 不在其中，同样会被拒绝。这正是本分支
（agent 1.2.13）新增 `appLockerAllowPaths` 要解决的部分——但只解决这一部分。

**结论：加白名单目录 ≠ 修好这台机器。** 两道墙必须一起解决，缺一不可。

---

## 为什么不放行 `wscript.exe`

最省事的"修法"是把 `wscript.exe` 从例外名单里去掉，但这条路被拒绝了，理由：

`C:\Windows\Temp` 是标准用户可写的目录，而镜像的 Script 规则集允许 `%WINDIR%\*`
执行任意脚本。一旦 `wscript.exe` 重新被放行，员工只需把一个 `.vbs`/`.js` 丢进
`C:\Windows\Temp`，再用 `wscript.exe C:\Windows\Temp\x.vbs` 执行——脚本本身跑在
`%WINDIR%\*` 允许的路径下的解释器里，但脚本内容可以是任何东西（下载并启动任意
exe、修改注册表……）。这正是例外名单从一开始就要堵住的口子：**脚本宿主 +
用户可写目录 = 绕过整个 AppLocker 白名单**。

`cmd.exe`、`powershell.exe`、`cscript.exe`、`mshta.exe` 同理，都不能放行。这份例外
名单不因为 Codex 的启动方式而放松。

---

## 正确做法

修复分两部分，缺一不可：

**1. 白名单目录（本分支已实现，控制台下发）**

在控制台「封禁策略」页的「AppLocker 放行目录」新增 `C:\tools\Codex\*`。
agent 每个同步周期都会用它重写本机的 AppLocker Exe 规则集（见
`go/internal/agentcore/sync.go`、`go/internal/policy/applocker_windows.go`），不需要改
装机脚本、不需要重新出镜像。

这一步解决"目录不在白名单"，**不解决 `wscript.exe` 被拦**。

**2. 让启动不经过脚本宿主**

`Codex.vbs` 做的两件事都可以不靠脚本完成：

- `CODEX_ELECTRON_ENABLE_WINDOWS_COMPUTER_USE=1` 设成**机器级环境变量**
  （`setx /M` 或 HKLM `System\CurrentControlSet\Control\Session Manager\Environment`），
  一次设置，对该机器所有用户永久生效。
- 快捷方式改成直接指向
  `C:\Tools\Codex\_internal\app\ChatGPT.exe`，工作目录设为该 `app` 目录，不再经过
  `wscript.exe`。

两步做完，启动链里不再有任何脚本宿主，第一道、第二道墙都不再挡路；`ChatGPT.exe`
本身落在第 1 步放行的 `C:\tools\Codex\*` 之下，可以正常执行。

---

## 验证

1. 员工机器完成一次同步（等一个周期，或管理员在该机器上手动触发），确认
   `agent.exe status` 输出里：

   ```
   applocker_allow=1
   ```

   （放行目录条数，与控制台发布的条数一致）

   **`applocker_allow=?` 不等于 `0`**：`?` 表示 agent 这次没能读到本机的 AppLocker
   本地策略（比如 AppLocker 服务没起来），是"不知道"；`0` 是"确认读到了，但目前没有
   任何放行目录"。两者处理方式不同——看到 `?` 应该去查 AppIDSvc 服务状态和
   agent 日志，而不是当作"没配置"重新下发一遍。

2. 应用第 2 步的启动改法之后，员工重新打开 Codex，不再弹出「系统管理员已阻止这个应用」。

3. 事件查看器 `Applications and Services Logs → Microsoft → Windows →
   AppLocker → EXE and DLL`，确认这次启动**没有新增 8004（拒绝）事件**——8004 之前
   会记录 `wscript.exe` 和/或 `ChatGPT.exe` 被拒绝；改完之后应该只看到（如果开了审计）
   8002（允许）或完全没有新记录。

---

## 遗留

本分支（agent 1.2.13）做的只是"白名单目录"这一半，以下两件事**没有做**，留给后续：

1. **机器级环境变量目前是手动设置的。** agent 已经会写 HKLM 策略键（浏览器封禁、
   AppLocker 放行目录都是这条路），管理一小撮机器环境变量放进 `policy.json` 是很自然
   的延伸，但这次没做。

2. **codex-kiosk 安装器仍然会创建 `wscript.exe` + `.vbs` 的快捷方式。** 治本的做法是
   让安装器直接创建指向 `ChatGPT.exe` 的快捷方式，不再经过脚本宿主。这需要改
   [`TEENet-io/codex-kiosk`](https://github.com/TEENet-io/codex-kiosk) 仓库，不属于本仓库
   的改动范围。

在这两项完成之前，每台新开的、用当前安装器装出来的机器都会重新踩到"第二道墙"，需要
按上面「正确做法」第 2 步手动改一遍。
