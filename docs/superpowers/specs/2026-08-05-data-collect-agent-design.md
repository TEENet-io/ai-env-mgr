# 会话采集集成进 agent — 设计文档

> 状态：设计待 review · 日期 2026-08-05 · 范围：仅采集端，不含分析端
>
> 参照实现：`/root/sun_home/ai-usage-analysis/scripts/collect_to_oss.py`。本设计的核心增量逻辑与该脚本**保持一致**，只把两处换成 ai-env-mgr 的做法：归属靠 binding 的员工目录、去重只靠本地 state。

---

## 1. 目标与边界

把绑定员工的 Claude Code / Codex **原始会话文件**增量收集到 OSS，供后续离线分析。

**做**：agent 每个 sync 周期扫描绑定员工的 `.claude`/`.codex` 会话文件，`(mtime,size)` 变化的、且已静默的，原文 PUT 到 `agent_workdir/{员工}/data_collect/` 下。

**不做**：

- 不解析、不脱敏、不聚类——原文照传（`analyze_sessions.py` 才做脱敏，是独立的管理员离线工具，本期不动）。
- 不采集 ChatGPT 桌面版数据。
- 不采集未绑定机器、不采集绑定员工以外的 profile。
- agent 不读回任何 `data_collect/` 内容（只写权限）。

**与主线一致**：这是"数据全量本地归集"，不是隐私墙。原文含密钥/PII 是预期内的，由 OSS 侧（私有桶、服务端加密、内网 endpoint、HTTPS、保留/审计策略）承担安全边界——这些是既有约束，采集只是复用。

## 2. 采什么、写到哪

只采**绑定员工**那一个 profile（`Machine.ProfileDir(binding.User)`）：

| 源（员工 profile 下） | OSS 目标键 |
|---|---|
| `<profile>/.claude/projects/**/*.jsonl` | `agent_workdir/{员工}/data_collect/.claude/projects/**/*.jsonl` |
| `<profile>/.codex/sessions/**/rollout-*.jsonl` | `agent_workdir/{员工}/data_collect/.codex/sessions/**/rollout-*.jsonl` |

保留 `.claude/`、`.codex/`、`rollout-` 这层目录结构：`analyze_sessions.py` 正是靠这些路径片段识别来源（`.codex/`、`rollout-` 判 `source_kind`）。采集端与分析端因此天然对齐，无需额外约定。

**键里不含主机名/系统用户名**：归属由员工目录 `{员工}/` 承担，binding 已经告诉我们是谁。这是移植进 Go 相比 Python 脚本的主要收益——Python 靠 `<hostname>/<user>/` 猜归属，agent 直接知道。

## 3. 增量与去抖（全靠本地 state）

**硬约束**：agent 对 `data_collect/` 只有 `PutObject`，不能 LIST/GET。所以去重不能查 OSS，只能靠本地 state。这与 `collect_to_oss.py` 一致，且本身是安全特性：一台机器的密钥泄露也读不回任何人的对话。

- **state 文件**：`<StateDir>/collect-state.json`，`StateDir` = `C:\ProgramData\AIEnvMgr`。
- **格式**：`{ "源绝对路径": [mtime, size] }`。与脚本一致——追加写一定改变 size，`(mtime,size)` 足以判变化。
- **每周期**：`stat` 每个匹配文件；`(mtime,size)` 与 state 不同才传；传成功才写回 state；**传失败不写 state → 下周期自动重试**。
- **去抖**：文件在最近 `quiet` 秒（默认 60）内还在变，判定"会话还在写"，本周期跳过，等静默后整份传——避免正在增长的文件每周期重传半截。
- **源文件被删**：从 state 移除该条；**OSS 上的副本保留**（受桶的版本/生命周期/保留策略管），与脚本一致。
- **state 丢失**（ProgramData 被清）：最坏全量重传一次，PUT 覆盖同键，幂等无害。

## 4. 何时采集、多久一次

- 作为 `RunOnce()` 里 **credentials 之后、status 之前**的一个独立步骤，由独立的 `Collector` 单元承担（自己的错误收集，不塞进现有 policy/creds 逻辑）。
- **节奏 = sync 周期**：周期设多短就采多勤（等价于脚本每分钟轮询），不额外开快循环。
- 采集统计（本周期传了几个文件、错误列表）并进 `status`，`admin.exe status` 能看到"这台机器在采、采了多少"。

## 5. 开关：默认关，策略集中控制

`policy.json` 新增字段：

- `collect_enabled bool`，默认 **false**。
- `collect_quiet_seconds int`，可选，默认 60（对齐脚本 `--quiet`）。
- `collect_since string`，可选（`YYYY-MM-DD`），默认空 = 采全部历史（对齐脚本 `--since`）。按源文件 mtime 的 UTC 日期比较，早于此日期的不采。

**代码先合入、功能默认不启用**——正好对上 `OSS布局.md` §6"这条权限在功能落地前不要提前加"。管理员在 OSS 加好写权限后，把 `collect_enabled` 置 true，全体 agent 下周期开始采；置回 false 即停。与 block policy 同一套下发机制，零额外通道。

## 6. 权限：只加一条 PutObject，绝不给读

功能启用时，`dist/ram-policy-agent.json` 加（`OSS布局.md` §6 已写好原文）：

```json
{ "Effect": "Allow", "Action": ["oss:PutObject"],
  "Resource": ["acs:oss:*:*:your-bucket/agent_workdir/*/data_collect/*"] }
```

只写不读。一台机器的密钥泄露 ≠ 全员对话泄露，这个的敏感度比配置/凭据高一个量级。**功能落地前不要提前加这条。**

## 7. 组件与接口（隔离边界）

```
Collector（internal/agentcore/collect.go，纯逻辑、平台无关、可 fake 测）
  依赖：
    - Store.Put(key, data)                     ← 复用现有 ossclient
    - FileSource 接口：列举 + stat + 读取会话文件
    - 本地 state 读写（StateDir 下 JSON）
    - 时钟（去抖/since 判断，注入以便测试）
  产出：CollectResult{ uploaded int, errors []string }

FileSource（cmd/agent 提供真实实现，agentcore 提供 fake）
  - Sessions(profileDir) []SessionFile       // 展开两个 glob
  - Stat(path) (mtime, size)
  - Open(path) io.ReadCloser
```

`Collector` 不知道 Windows、不知道 OSS SDK、不知道 glob 具体怎么走——都从接口进来，整个采集周期能用 fake 在任何平台测。

## 8. 代码改动清单（本期）

- **新增** `go/internal/agentcore/collect.go` — `Collector`、`CollectOnce()`、本地 state 读写、去抖、since、遍历、PUT、失败重试。
- **新增** `go/internal/agentcore/collect_test.go` — 覆盖：增量（变了才传）、去抖（未静默跳过）、失败重试（不写 state）、源删除（清 state 留 OSS）、since 过滤、键结构正确、`collect_enabled=false` 时完全不动。
- `go/internal/ossclient/client.go` — 已有 `DataCollectPrefix(user)`；补一个 helper 把 profile 内相对路径（`.claude/...` 或 `.codex/...`）拼成完整键。
- `go/internal/model/model.go` — `Policy` 加 `CollectEnabled` / `CollectQuietSeconds` / `CollectSince`。
- `go/internal/agentcore/sync.go` — `RunOnce` 在 creds 后、status 前，`pol.CollectEnabled` 时调用 collector；结果并进 status。
- `go/internal/status/` — status 加采集统计字段（uploaded 数、错误）。
- `go/cmd/agent/` — 真实 `FileSource`（Windows profile 下的 glob/stat/open），wire 进 syncer。
- **文档**：`OSS布局.md` §6、`dist/ram-policy-agent.json`、`操作手册.md`、`架构说明.md` 同步。

## 9. 明确不做（YAGNI）

- 不做多 profile 采集（binding 是一机一人）。
- 不做 STS 临时凭据（沿用现有长期 AK/SK 机制）。
- 不做分析端集成进 admin.exe（本期只采集）。
- 不做 agent 侧读回/校验已上传对象（只写权限，靠本地 state 幂等）。

## 10. 验收

- `go test ./...` 全绿，含 `collect_test.go`。
- `collect_enabled=false`：agent 不产生任何 `data_collect/` 对象、不写 collect-state。
- `collect_enabled=true`（测试前缀）：绑定员工的会话文件出现在 `agent_workdir/{员工}/data_collect/.claude|.codex/...`，键结构与源目录一致。
- 重复 sync：未变化文件不重传（state 命中）；改动一个文件只重传该文件。
- 删除一个源文件：state 清除该条，OSS 副本仍在。
