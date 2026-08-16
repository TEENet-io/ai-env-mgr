# Codex-Only 部署集成方案

**目标**：用自打包的 **`codex-only-local`** 构建替换官方 ChatGPT App，让无影云电脑上的员工只能用 Codex、无 ChatGPT 入口，并把它接进 ai-env-mgr 的**镜像 / AppLocker / agent** 体系。

**背景**：`codex-only-local` 已在**构建期**从 MSIX 改 `app.asar` + 改 Statsig feature gate，把 ChatGPT / 云任务入口从 App 自身的功能开关层去掉。本方案只做**集成与部署**，不改它的补丁逻辑。它一旦落地，运行时的 `codex-mode-guard`（UI guard）**整个作废**。

---

## Gate 0 — 法务口径（阻塞项，先过）

面向客户部署前**必须**先确认：ChatGPT Business 商业合约下，**重打包 + 未签名再分发** OpenAI 客户端给员工使用的合规口径。

- 责任人：客户 IT / 法务（必要时问 OpenAI 侧）。
- 产出：明确的"可以 / 不可以 / 有条件"结论。
- **没过之前，后续阶段只在内部测试环境做，不进客户镜像。**

## 全局约束

- 不改 `codex-only-local` 的补丁逻辑（那是独立仓库的事）。
- 任何密钥 / 证书私钥**不入仓库**。
- agent 侧只做**只读上报**——不管理、不拉起 UI 进程。
- 官方 ChatGPT MSIX 在最终镜像里**不装、不放行**。

---

## 阶段

### 阶段 1 — 产出并固定一个 Codex-only 安装包

- 用 `codex-only-local` 的 `build-offline-package.ps1` 出一个 installer（Inno Setup）+ `build-metadata.json`（记录上游 Codex 版本、`codex-only-local` commit、补丁 marker）。
- **D1 分发方式**（三选一）：
  - **(a) OSS 暂存**：installer 用 `admin file put` 传到 OSS，镜像制作时下载 —— **推荐**，和现在传 `agent.exe` 一个流程；
  - (b) 放进某个 GitHub release；
  - (c) 直接拷到模板机。
- 追溯：记录"本次用的上游版本 + `codex-only-local` 的 commit"。

### 阶段 2 — 企业签名

- 用**企业代码签名证书**签 installer 和主 exe（`ChatGPT.exe` / `Codex.exe`）。
- **D2 证书来源**：用镜像里已受信的企业 CA 签发的代码签名证书。
- 验证：`signtool verify /pa` / 文件属性→数字签名。
- 目的：让 AppLocker/WDAC 能按**发布者**放行（比 path/hash 稳）。

### 阶段 3 — 镜像安装脚本

改 `scripts/01-Install-AITools.ps1`：

- **移除**官方 ChatGPT MSIX 安装那段；
- 加装 Codex-only installer（silent：`/VERYSILENT /SUPPRESSMSGBOXES`）或部署 portable；
- 桌面 / 开始菜单快捷方式走 `Codex.vbs`（无黑窗）；
- **保留** Claude Code 安装部分不动；
- 保留"给已存在用户也装"的逻辑（现在的 `-AllExistingUsers` 思路）。

### 阶段 4 — AppLocker 放行

改 `scripts/02-Manage-AIAccess.ps1`：

- 加一条 **publisher 规则**放行签名后的 Codex-only 构建；
- 确认**不放行**官方 ChatGPT MSIX（它也不装）；
- 浏览器封禁维持不变。
- 若最终决定不签名 → 退化成 path/hash 规则（弱，且每次升级要重算 hash），不推荐。

### 阶段 5 — agent 版本上报（唯一进 Go 仓库的代码）

- agent 读"已装 Codex-only 版本"（从安装目录 `build-metadata.json` / 文件版本 / 安装时写的 marker），放进 `model.Status`。
- `admin status` 加一列 / 详情：显示每台的 Codex 构建版本 → 一眼看出谁没升级、谁还是官方 App。
- 纯**只读**，和现在报 AppLocker 模式一个套路。
- **D3 marker 放哪**：安装目录一个 `version.txt` / 一个注册表键，要和安装脚本约定一致。

### 阶段 6 — 凭据兼容性验证

- 确认 agent 投递的 `codex/auth.json` + `config.toml` 路径在 Codex-only 构建里被读到（登录态直接生效）。
- 若路径不同，调 agent 的投递路径（creds 投递已有机制）。

### 阶段 7 — 验收 & 回退 & 退役

- **干净 VM 全流程验收**：装 → agent 投凭据 → 打开 Codex（确认**无 ChatGPT 入口**）→ 本地任务 → 重启 → 卸载。
- **回退预案**：上游 Codex 更新导致补丁失配、构建失败时，员工会卡在旧版本。约定：谁负责重新适配、多久内、期间是否临时允许放行官方 App。
- **退役** `codex-mode-guard` 分支（不再需要）。

---

## 要拍板的决策

| # | 决策 | 建议 |
|---|---|---|
| D1 | 安装包分发方式 | OSS 暂存（和传 agent.exe 一致） |
| D2 | 签名证书来源 | 企业 CA 的代码签名证书 |
| D3 | 版本 marker 放哪 | 安装目录 `version.txt` + 注册表键 |
| D4 | 上游断裂时谁负责重新适配 | 指定维护责任人 + SLA |
| D5 | `codex-only-local` 仓库 | 保持独立，还是与 ai-env-mgr 版本联动 |

## 风险

- **法务（Gate 0）**：最大，先过。
- **维护跑步机**：上游每更新要重新适配；断了员工卡旧版本。
- **签名**：不签则 AppLocker 只能 path/hash 弱放行。
- **供应链**：构建从商店 CDN 拉 MSIX，pipeline 完整性要保证。

---

## 落地顺序（建议）

1. **Gate 0 法务**（并行推进，别等）
2. 阶段 1 出一个固定版本安装包 → **内部测试 VM 装上、手测无 ChatGPT 入口**
3. 阶段 2 签名 → 阶段 4 AppLocker 放行 → 阶段 3 装进镜像脚本
4. 阶段 5 agent 上报（我在 Go 仓库这边做）
5. 阶段 6 凭据验证 → 阶段 7 验收 → 退役 guard 分支

> ai-env-mgr 这边我能直接做的是**阶段 5（agent 版本上报）**和**阶段 3/4 的脚本改动**；签名、镜像、法务是你/客户侧的动作，我给具体命令和验收清单。
