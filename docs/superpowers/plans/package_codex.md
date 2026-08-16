# Codex-only Fork 构建方案

```
unofficial-codex-app-offline/
├─ config/                    # 离线构建配置
├─ docs/                      # 架构、补丁和兼容性设计
├─ installer/                 # Inno Setup 安装器模板
├─ scripts/                   # 桌面版提取、修改、打包逻辑
│  ├─ build-offline-package.ps1
│  ├─ patch-app-asar.mjs
│  ├─ extract-webview-from-asar.mjs
│  ├─ resolve-store-bundle-url.mjs
│  ├─ setup-codex-offline.ps1
│  ├─ desktop-patches/
│  │  ├─ init.cjs
│  │  └─ plugin-service-compat.cjs
│  └─ test/
└─ web-gateway/
   ├─ start-web.mjs
   ├─ web-shell/              # 浏览器壳及 Electron API 模拟
   └─ gateway/
      ├─ src/
      │  ├─ server.ts
      │  ├─ codex-app-server.ts
      │  ├─ ipc/
      │  │  └─ codex/
      │  └─ official/         # 官方 ASAR/WebView 读取
      └─ test/
```
## 目标

构建一个 Windows x64 的非官方 Codex-only 客户端：

- 启动后直接进入 Codex；
- 左上角只显示不可点击的 `Codex`；
- ChatGPT / Work 菜单、快捷键和内部入口不能绕过限制；
- 不使用截图、坐标、后台轮询、鼠标钩子或 DLL 注入；
- 官方前端结构变化时停止构建和发布；
- 使用 GitHub Actions 自动测试、构建和发布，可选阿里云 Windows runner。

## 技术结论

ChatGPT、Work 和 Codex 共用同一个 Electron renderer，并不是不同的 Windows binary。左上角菜单和快捷键最终切换同一前端的产品模式。

因此，正确的控制点是构建时修改 `app.asar`，而不是运行时监控鼠标或窗口。

主要代码：

| 文件 | 职责 |
| --- | --- |
| `scripts/patch-app-asar.mjs` | 应用 Codex-only 静态补丁 |
| `scripts/verify-offline-package.ps1` | 验证产物和补丁 |
| `.github/workflows/build-offline-package.yml` | Windows 自动构建与发布 |

## Codex-only 补丁

必须同时实现三层限制。

### 1. 强制启动模式

renderer 的产品模式计算函数当前会返回 `work` 或 `codex`。补丁应使它始终返回：

```js
`codex`
```

这样可以覆盖旧用户目录中已经保存的 ChatGPT / Work 状态。

### 2. 禁用左上角菜单

应用已有原生 `codexOnly` 分支：

```js
<NQc codexOnly={value} ... />
```

将其固定为 `true` 后，组件只显示普通的 `Codex` 标题，不创建产品下拉菜单。不要使用 CSS 遮罩。

### 3. 守住统一切换入口

Windows 内置模式快捷键：

```text
Alt+1 -> Chat
Alt+2 -> Work
Alt+3 -> Codex
```

菜单、快捷键和命令面板最终进入统一模式函数 `txc(...)`。应在入口把 `nextMode` 强制归一为 `codex`，防止其他入口绕过 UI 限制。

## 版本漂移保护

在 `scripts/patch-app-asar.mjs` 中新增必需 marker：

```js
/*codex-only:product-mode-enforced*/
```

补丁必须满足：

- 启动模式、`codexOnly` 和 `txc` 三个目标分别精确匹配一次；
- 重复执行保持幂等；
- 任一目标缺失或重复时调用 `failRequiredPatch(...)`；
- 不使用跨整个 bundle 的宽泛字符串替换；
- 前端更新导致结构变化时，CI 失败并停止发布。

建议增加：

```text
scripts/test/codex-only-product-mode.test.cjs
scripts/verify-codex-only.ps1
```

验证内容：

- marker 存在且仅出现一次；
- 启动模式固定为 Codex；
- `codexOnly` 固定为 true；
- `txc` 拒绝或归一化 Chat / Work；
- `Alt+1`、`Alt+2` 后仍处于 Codex；
- 重启后仍处于 Codex。

## 会话存储边界

普通 ChatGPT 与 Codex 本地任务使用不同的后端。

| 类型 | 接口或协议 | 权威历史存储 |
| --- | --- | --- |
| 普通 ChatGPT | `/conversation`、`/conversations` | ChatGPT 云端 |
| ChatGPT Work / Codex 云任务 | `/wham/tasks` | 云端任务服务 |
| Codex 本地任务 | `thread/*`、`turn/start` app-server JSON-RPC | 本机 `.codex` |

Codex 本地任务默认存储：

```text
%USERPROFILE%\.codex\sessions\
%USERPROFILE%\.codex\state_5.sqlite
%USERPROFILE%\.codex\sqlite\state_5.sqlite
%USERPROFILE%\.codex\.codex-global-state.json
```

本地任务与工作区、Git、文件、终端、审批和工具调用状态绑定，因此由本机 `codex.exe app-server` 持久化。

需要注意：

- Codex 本地任务通常仍会调用云端模型服务；
- Codex 界面也能创建 `chatgptWorkCloud` 云任务；
- 屏蔽 ChatGPT 产品模式不会自动禁用云任务。

如果要求所有任务历史都在本机，还需要独立的 `localTasksOnly` 策略：

- 禁用 cloud / remote task 创建入口；
- 拒绝 `target.type === "chatgptWorkCloud"`；
- 禁用或过滤 `/wham/tasks` 列表和打开入口；
- 保留本地 `thread/start`、`thread/list`、`thread/read` 和 `turn/start`。

建议将两项策略分开配置：

```text
codexOnly: true
localTasksOnly: true | false
```

## GitHub Actions

仓库已经使用 Windows runner：

```yaml
jobs:
  build:
    runs-on: windows-latest
```

现有自动触发：

```yaml
on:
  workflow_dispatch:
  schedule:
    - cron: '15 3 * * *'
  push:
    branches: [main]
  pull_request:
    branches: [main]
```

发布顺序应保持：

```text
源码测试
  -> 下载并校验官方输入
  -> 应用必需补丁
  -> 构建离线包
  -> 通用离线验证
  -> Codex-only 验证
  -> SHA-256
  -> Artifact
  -> GitHub Release
```

任何 Codex-only 验证失败都不能发布 Release。

建议命名：

```text
Tag: codex-only-v26.803.10989.0-r1
Name: Codex-only 26.803.10989.0 (revision 1)
```

## 阿里云加速

推荐保留 GitHub Actions，只把完整构建 job 放到阿里云 Windows Server ECS self-hosted runner：

```yaml
runs-on: [self-hosted, Windows, X64, aliyun-codex-builder]
```

建议配置：

| 项目 | 建议 |
| --- | --- |
| 系统 | Windows Server 2022 x64 |
| CPU | 8 vCPU |
| 内存 | 16–32 GB |
| 构建盘 | 200 GB ESSD |
| Runner | `C:\actions-runner` |
| 构建目录 | `D:\cb` |
| 缓存目录 | `D:\codex-cache` |

可以缓存 MSIX、Primary Runtime、skills 和 pnpm store，但每次必须重新校验版本与哈希，并重新创建 stage 和验证目录。

公开 Fork 应采用混合 CI：

```yaml
jobs:
  test:
    runs-on: windows-latest

  build:
    needs: test
    if: github.event_name != 'pull_request'
    runs-on: [self-hosted, Windows, X64, aliyun-codex-builder]
```

这样外部 PR 不会在长期自托管 ECS 上执行任意代码。

GitHub 的定时触发只会创建任务，不会启动已关机的 ECS。第一版建议 ECS 常驻；稳定后可通过阿里云定时任务或 `workflow_job: queued` webhook 按需启动。

## 已完成与下一步

已经完成：

- 仓库和 Electron 架构分析；
- Windows 本地构建环境适配；
- 普通离线包成功构建；
- 仓库测试 `107/107` 通过；
- 完整离线验证器通过；
- Codex-only 三个补丁位置定位；
- GitHub Actions 和阿里云 runner 方案设计。

尚未完成：

- Codex-only 补丁尚未正式写入 `patch-app-asar.mjs`；
- 专用单元测试和验证器尚未实现；
- 尚未重新构建 Codex-only 产物；
- 尚未完成真实桌面的菜单、快捷键和重启验收；
- 尚未决定是否启用 `localTasksOnly`；
- 尚未部署正式 Fork 和阿里云 runner。

推荐下一步：

1. 实现三层 Codex-only 必需补丁；
2. 添加单元测试和静态验证；
3. 本地重新构建并验证；
4. 进行真实 Windows 桌面验收；
5. Fork 仓库并启用 GitHub Actions；
6. 再部署阿里云 self-hosted runner。

## 构建记录

本地构建的固定输入、排错过程、产物路径和 SHA-256 记录在工作区：

```text
outputs/unofficial-codex-app-offline-build-log.md
```

## Codex-only + local tasks implementation (2026-08-14)

The implementation is now wired into `scripts/patch-app-asar.mjs` as eight required, version-locked patches:

- force startup product mode to Codex;
- render the product selector as disabled Codex-only UI;
- normalize menu, shortcut, and internal product transitions back to Codex;
- force the composer task location to `local`;
- remove `chatgptWorkCloud` from the `create_thread` schema and reject it again at runtime;
- disable cloud task list and cloud task detail queries.

All markers are declared in the shared capability contract and required by `verify-offline-package.ps1`. If an upstream renderer update changes any targeted surface, packaging fails instead of silently producing an unrestricted build.

Important boundary: `local` describes where the task process, shell, files, and workspace run. It does not make model inference offline. ChatGPT login and OpenAI API providers still send model requests to their configured service. A network-isolated build additionally needs a local model provider and an installation/network policy that blocks cloud providers.

Validation status: all 107 repository tests passed. Store `26.803.10989.0` was patched and repacked; the extracted renderer passed JavaScript syntax checking and contained all eight markers. A fresh portable build did not finish because Windows held a file in the existing `web-gateway/node_modules` tree (`EPERM`), and dependency recreation then exceeded the one-hour command limit. Real desktop interaction and restart acceptance testing is still required before release.
