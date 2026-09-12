# 统一日志阶段一实施计划:集中运行日志与基础告警

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把网关(LiteLLM)和控制台(ai-env-mgr admin)的运行日志与审计事件以统一结构落到阿里云 SLS 的 ops / audit 两个 Logstore,并把"网关不可用、上游错误率、预算 80%、采集停摆"四条告警推到企业微信。

**Architecture:** 各模块只负责把结构化事件(JSON Lines)写到本机持久文件,不直接调用 SLS;两台主机装 Logtail 负责断点续传、轮转跟踪和重试。控制台侧新增 `internal/eventlog` 包(slog JSON + 审计事件 + 轮转文件),网关侧在既有 `custom_callbacks.py` 上增加成功/失败回调写 `llm_call` 事件。独立健康探测是控制台主机上的 systemd timer,不经过网关自己的日志链。告警规则和企业微信通道配置在 SLS 内。

**Tech Stack:** Go 1.25 `log/slog`(标准库,无新依赖)、Python 3(LiteLLM 容器内)、Logtail(iLogtail)、阿里云 CLI `aliyun`(SLS OpenAPI 2020-12-30)、SLS 告警 2.0、企业微信群机器人 webhook。

**Spec:** `docs/superpowers/specs/2026-09-09-unified-logging-design.md`(Task 0 从 `/root/pp_home/windows-pc/设计_统一日志与告警_2026-09.md` rev.2 复制;仓库里 2026-09-08 的 rev.1 已被另一会话删除,本计划不处理那条未提交删除)。本计划只覆盖 spec §9「阶段一」;§9 明确阶段一"不能以此宣称完整日志中心已交付",控制台内嵌查询页、Agent 分段队列、sessions 都不在本计划内。

## Global Constraints

- SLS 项目 `windows-control-logs`,地域 `ap-southeast-1`,Logstore `ops`(保留 30 天)与 `audit`(保留 365 天,热存 30 天)。spec §0、§2.1。
- 每条事件必填:`schema_version`(本阶段固定 `1`)、`event_id`、`event_type`、`occurred_at`(RFC3339 UTC,毫秒)、`module`(`gateway` / `console` / `probe`)、`source_id`(主机名或容器名)、`level`(`debug|info|warn|error`)、`message`。spec §3.1。
- `event_id` 在源端生成、重发不变;查询与统计按 `event_id` 去重。spec §3.3。
- 落盘前脱敏:`Authorization`、`Cookie`、AccessKey、`sk-` 开头的令牌、`api_key`、`experimental_bearer_token`、凭据文件内容一律不写;提示词、模型回复正文、工具输出不写(阶段一不采集 sessions)。spec §8。
- 网关事件只记 `emp-*` 别名、`user_id`、模型组、实际模型、状态、错误分类、耗时、用量、费用估计、`litellm_call_id`;成功、失败、取消要可区分。spec §2.2、§4.2。
- 费用字段带 `cost_source=litellm_estimate`、`cost_state=estimated`,未知费用写 `null` 不写 0。spec §3.3。
- 写入身份只写不读:RAM 策略只授 `log:PostLogStoreLogs` 等写权限到本项目;查询身份只读。spec §8。
- 现有 OSS 审计 JSONL(`admin/audit/<user>.jsonl`)照旧写,SLS audit 是副本,不替代。spec §4.1 过渡期。
- 日志文件轮转不能删掉未上传内容:应用侧只做重命名式轮转(`.1`…`.5`),不 copytruncate;由 Logtail 按 inode 跟踪。spec §4.3、§4.4。
- 旧 Agent 的 64KB OSS 日志尾巴保持可读,控制台日志页标注"仅日志尾部"。spec §9 阶段一。
- 提交信息尾注只有 `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`,不加 Claude-Session。`make check`(gofmt/vet/test)必须通过。
- 生产改动(网关 compose、控制台 systemd、Logtail 安装、SLS 资源)每一步先在文中记录回滚命令,再执行。

## 前置输入(需要用户提供,Task 1 开始前)

1. 阿里云 RAM 用户 `sls-bootstrap` 的 AccessKey,授权 `AliyunLogFullAccess` + `AliyunRAMFullAccess`(仅用于 Task 1 建项目、建策略、建子用户,做完即禁用)。或者用户自己在控制台建好项目后,只提供两个子用户 AK(见 Task 1 Step 5)。
2. 企业微信群机器人 webhook 地址(`https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=...`)。
3. 确认:阶段一不改 Agent、不改无影云电脑出网策略(Agent 不直连 SLS)。

在拿到 1 之前,Task 2、3、4 可以先做(它们只写本地文件);Task 1、5、6、7 依赖它。

---

## 文件结构

```
ai-env-mgr/
  docs/superpowers/specs/2026-09-09-unified-logging-design.md   # Task 0 复制 rev.2
  ops/sls/provision.sh          # Task 1 建项目/Logstore/索引/RAM/机器组(幂等)
  ops/sls/policy-writer.json    # Task 1 写入身份策略
  ops/sls/policy-reader.json    # Task 1 查询身份策略
  ops/sls/index-ops.json        # Task 1 ops 索引
  ops/sls/index-audit.json      # Task 1 audit 索引
  ops/sls/pipeline-*.json       # Task 5 Logtail 采集配置(4 个)
  ops/sls/alerts/*.json         # Task 6 告警规则
  ops/probe/gateway-probe.sh    # Task 4 独立探测脚本
  ops/probe/gateway-probe.service / .timer
  go/internal/eventlog/eventlog.go       # Task 3 事件写入器 + 轮转
  go/internal/eventlog/eventlog_test.go
  go/internal/eventlog/redact.go         # Task 3 脱敏
  go/internal/eventlog/redact_test.go
  go/internal/admincore/audit.go         # Task 3 追加 SLS 副本
  go/internal/adminweb/server.go         # Task 3 访问日志中间件、Options.LogDir
  go/cmd/admin/web_cmd.go                # Task 3 --log-dir
  go/internal/adminweb/assets/log.html   # Task 8 "仅日志尾部"标注
gateway-litellm/
  custom_callbacks.py           # Task 2 增加 GatewayEventLogger
  docker-compose.yml            # Task 2 挂载 ./logs
  test_gateway_events.py        # Task 2 本地测试(不进容器)
  README.md                     # Task 8 决策记录
```

---

### Task 0: 把 rev.2 设计文档放进仓库

**Files:**
- Create: `docs/superpowers/specs/2026-09-09-unified-logging-design.md`

- [ ] **Step 1: 复制并校对**

```bash
cp /root/pp_home/windows-pc/设计_统一日志与告警_2026-09.md \
   /root/pp_home/windows-pc/ai-env-mgr/docs/superpowers/specs/2026-09-09-unified-logging-design.md
cd /root/pp_home/windows-pc/ai-env-mgr && head -5 docs/superpowers/specs/2026-09-09-unified-logging-design.md
```
Expected: 第一行 `# 设计:ai-env-mgr 统一日志、会话归档与告警`,第三行含 `rev.2`。

- [ ] **Step 2: 只提交这一个文件**(不要 `git add -A`,工作树里另有别的会话的未提交删除)

```bash
git add docs/superpowers/specs/2026-09-09-unified-logging-design.md
git commit -m "docs: add unified logging design rev.2 as the phase-1 spec" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 1: 建 SLS 项目、Logstore、索引、RAM 身份、机器组

**Files:**
- Create: `ops/sls/provision.sh`, `ops/sls/policy-writer.json`, `ops/sls/policy-reader.json`, `ops/sls/index-ops.json`, `ops/sls/index-audit.json`

**Interfaces:**
- Produces: 项目 `windows-control-logs`;Logstore `ops`、`audit`;机器组 `console-host`、`gateway-host`(自定义标识 `wc-console`、`wc-gateway`);RAM 用户 `wc-logs-writer`(仅写)、`wc-logs-reader`(仅读)及各自 AK。Task 5 的 Logtail 用机器组;Task 6 的告警用两个 Logstore;后续阶段二的控制台查询用 reader AK。

- [ ] **Step 1: 配置 CLI 并确认 API 名称**(用前置输入 1 的 bootstrap AK;本地 profile 名 `sls-bootstrap`,做完 Task 1 删除)

```bash
aliyun configure set --profile sls-bootstrap --mode AK --region ap-southeast-1 \
  --access-key-id "$BOOTSTRAP_AK" --access-key-secret "$BOOTSTRAP_SK"
aliyun sls help 2>&1 | grep -E "CreateProject|CreateLogStore|CreateIndex|CreateMachineGroup|CreateLogtailPipelineConfig|ApplyConfigToMachineGroup|CreateAlert|UpdateLogStore" 
```
Expected: 八个 API 名称都列出。**已核对(2026-09-12)**:本机 `/usr/local/bin/aliyun` 是 3.0.282,没有 SLS 产品;3.5.0 有,产品码是 `sls`,本计划用到的 API(CreateProject、CreateLogStore、CreateIndex、GetIndex、CreateMachineGroup、GetMachineGroup、ListMachines、CreateLogtailPipelineConfig、GetLogtailPipelineConfig、UpdateLogtailPipelineConfig、ApplyConfigToMachineGroup、CreateAlert、GetAlert、UpdateAlert、GetLogs、ListProject)全部存在。用 `https://github.com/aliyun/aliyun-cli/releases/download/v3.5.0/aliyun-cli-linux-3.5.0-amd64.tgz` 解包到 `~/bin/aliyun`(不覆盖系统的 3.0.282),后续命令都用它。

- [ ] **Step 2: 写索引定义**

`ops/sls/index-ops.json`:
```json
{
  "ttl": 30,
  "line": {"token": [",", " ", "'", "\"", ";", "=", "(", ")", "[", "]", "{", "}", "?", "@", "&", "<", ">", "/", ":", "\n", "\t", "\r"], "caseSensitive": false, "chn": false},
  "keys": {
    "event_id":    {"type": "text", "token": [], "caseSensitive": true, "doc_value": true},
    "event_type":  {"type": "text", "token": [], "doc_value": true},
    "module":      {"type": "text", "token": [], "doc_value": true},
    "source_id":   {"type": "text", "token": [], "doc_value": true},
    "level":       {"type": "text", "token": [], "doc_value": true},
    "occurred_at": {"type": "text", "token": [], "doc_value": true},
    "message":     {"type": "text", "token": [" ", ",", ":", "=", "\"", "'", "(", ")", "[", "]", "{", "}"], "doc_value": false},
    "request_id":  {"type": "text", "token": [], "caseSensitive": true, "doc_value": true},
    "employee_id": {"type": "text", "token": [], "doc_value": true},
    "status":      {"type": "long", "doc_value": true},
    "latency_ms":  {"type": "double", "doc_value": true},
    "ok":          {"type": "text", "token": [], "doc_value": true}
  }
}
```

`ops/sls/index-audit.json`:
```json
{
  "ttl": 365,
  "line": {"token": [",", " ", "'", "\"", ";", "=", "(", ")", "[", "]", "{", "}", "?", "@", "&", "<", ">", "/", ":", "\n", "\t", "\r"], "caseSensitive": false, "chn": false},
  "keys": {
    "event_id":        {"type": "text", "token": [], "caseSensitive": true, "doc_value": true},
    "event_type":      {"type": "text", "token": [], "doc_value": true},
    "module":          {"type": "text", "token": [], "doc_value": true},
    "source_id":       {"type": "text", "token": [], "doc_value": true},
    "level":           {"type": "text", "token": [], "doc_value": true},
    "occurred_at":     {"type": "text", "token": [], "doc_value": true},
    "employee_id":     {"type": "text", "token": [], "doc_value": true},
    "key_alias":       {"type": "text", "token": [], "doc_value": true},
    "model_group":     {"type": "text", "token": [], "doc_value": true},
    "model":           {"type": "text", "token": [], "doc_value": true},
    "status":          {"type": "text", "token": [], "doc_value": true},
    "error_class":     {"type": "text", "token": [], "doc_value": true},
    "error_source":    {"type": "text", "token": [], "doc_value": true},
    "litellm_call_id": {"type": "text", "token": [], "caseSensitive": true, "doc_value": true},
    "latency_ms":      {"type": "double", "doc_value": true},
    "prompt_tokens":   {"type": "long", "doc_value": true},
    "completion_tokens": {"type": "long", "doc_value": true},
    "total_tokens":    {"type": "long", "doc_value": true},
    "cost_usd":        {"type": "double", "doc_value": true},
    "cost_state":      {"type": "text", "token": [], "doc_value": true},
    "user_max_budget": {"type": "double", "doc_value": true},
    "action":          {"type": "text", "token": [], "doc_value": true},
    "target":          {"type": "text", "token": [], "doc_value": true},
    "ok":              {"type": "text", "token": [], "doc_value": true},
    "message":         {"type": "text", "token": [" ", ",", ":", "=", "\"", "'", "(", ")", "[", "]", "{", "}"], "doc_value": false}
  }
}
```

- [ ] **Step 3: 写 RAM 策略**

`ops/sls/policy-writer.json`(只写,不能查、不能删):
```json
{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["log:PostLogStoreLogs", "log:GetLogStore", "log:ListLogStores", "log:GetProject"],
      "Resource": ["acs:log:ap-southeast-1:*:project/windows-control-logs/logstore/ops", "acs:log:ap-southeast-1:*:project/windows-control-logs/logstore/audit", "acs:log:ap-southeast-1:*:project/windows-control-logs"]
    }
  ]
}
```

`ops/sls/policy-reader.json`(只读两个 Logstore):
```json
{
  "Version": "1",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["log:GetLogStoreLogs", "log:GetLogStoreHistogram", "log:GetLogStore", "log:ListLogStores", "log:GetProject", "log:GetIndex"],
      "Resource": ["acs:log:ap-southeast-1:*:project/windows-control-logs/logstore/*", "acs:log:ap-southeast-1:*:project/windows-control-logs"]
    }
  ]
}
```

- [ ] **Step 4: 写幂等的 provision.sh**

```bash
#!/usr/bin/env bash
# Creates the SLS project, both logstores, their indexes, the two machine
# groups and the two RAM identities. Safe to re-run: every create is guarded
# by a get. Run with: PROFILE=sls-bootstrap ops/sls/provision.sh
set -euo pipefail
PROFILE=${PROFILE:-sls-bootstrap}
PRODUCT=${PRODUCT:-sls}
REGION=ap-southeast-1
PROJECT=windows-control-logs
HERE=$(cd "$(dirname "$0")" && pwd)
A() { aliyun "$PRODUCT" "$@" --profile "$PROFILE" --region "$REGION" --force; }

echo "== project"
A GetProject --project "$PROJECT" >/dev/null 2>&1 || \
  A CreateProject --body "{\"projectName\":\"$PROJECT\",\"description\":\"ai-env-mgr unified logs (phase 1)\"}"

echo "== logstores"
A GetLogStore --project "$PROJECT" --logstore ops >/dev/null 2>&1 || \
  A CreateLogStore --project "$PROJECT" --body '{"logstoreName":"ops","ttl":30,"shardCount":2,"autoSplit":true,"maxSplitShard":8}'
A GetLogStore --project "$PROJECT" --logstore audit >/dev/null 2>&1 || \
  A CreateLogStore --project "$PROJECT" --body '{"logstoreName":"audit","ttl":365,"hot_ttl":30,"shardCount":2,"autoSplit":true,"maxSplitShard":8}'

echo "== indexes"
A GetIndex --project "$PROJECT" --logstore ops >/dev/null 2>&1 || \
  A CreateIndex --project "$PROJECT" --logstore ops --body "file://$HERE/index-ops.json"
A GetIndex --project "$PROJECT" --logstore audit >/dev/null 2>&1 || \
  A CreateIndex --project "$PROJECT" --logstore audit --body "file://$HERE/index-audit.json"

echo "== machine groups (custom identifier; hosts declare it in /etc/ilogtail/user_defined_id)"
for g in console-host:wc-console gateway-host:wc-gateway; do
  name=${g%%:*}; id=${g##*:}
  A GetMachineGroup --project "$PROJECT" --machineGroup "$name" >/dev/null 2>&1 || \
    A CreateMachineGroup --project "$PROJECT" --body "{\"groupName\":\"$name\",\"machineIdentifyType\":\"userdefined\",\"machineList\":[\"$id\"]}"
done

echo "== RAM identities"
R() { aliyun ram "$@" --profile "$PROFILE" --region "$REGION" --force; }
for spec in wc-logs-writer:policy-writer.json wc-logs-reader:policy-reader.json; do
  u=${spec%%:*}; p=${spec##*:}; pol=${u}-policy
  R GetUser --UserName "$u" >/dev/null 2>&1 || R CreateUser --UserName "$u" --DisplayName "$u"
  R GetPolicy --PolicyType Custom --PolicyName "$pol" >/dev/null 2>&1 || \
    R CreatePolicy --PolicyName "$pol" --PolicyDocument "$(cat "$HERE/$p")"
  R AttachPolicyToUser --PolicyType Custom --PolicyName "$pol" --UserName "$u" >/dev/null 2>&1 || true
done
echo "create AKs by hand (they print once):"
echo "  aliyun ram CreateAccessKey --UserName wc-logs-writer --profile $PROFILE"
echo "  aliyun ram CreateAccessKey --UserName wc-logs-reader --profile $PROFILE"
echo "done"
```

- [ ] **Step 5: 执行并核对**

```bash
chmod +x ops/sls/provision.sh && PROFILE=sls-bootstrap ops/sls/provision.sh
aliyun sls GetLogStore --project windows-control-logs --logstore audit --profile sls-bootstrap --region ap-southeast-1 | grep -E '"ttl"|hot_ttl'
```
Expected: `ttl: 365`,`hot_ttl: 30`。再手动执行两条 CreateAccessKey,把 writer AK 交给 Task 5(写进两台主机的 Logtail 用户标识/凭据),reader AK 封存到密码库,不写进仓库、不写进日志。

回滚:`aliyun sls DeleteProject --project windows-control-logs`(会删所有 Logstore),`aliyun ram DetachPolicyFromUser/DeletePolicy/DeleteUser`。

- [ ] **Step 6: 提交(不含任何 AK)**

```bash
git add ops/sls/provision.sh ops/sls/policy-writer.json ops/sls/policy-reader.json ops/sls/index-ops.json ops/sls/index-audit.json
git grep -n "LTAI" -- ops/ && { echo "AK leaked"; exit 1; } || true
git commit -m "ops: provision the SLS project, logstores, indexes and write/read identities" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: 网关 llm_call 事件落盘

**Files:**
- Modify: `gateway-litellm/custom_callbacks.py`(文件末尾追加类,`proxy_handler_instance` 保持不变,新增第二个实例)
- Modify: `gateway-litellm/config.yaml:185-188`(`callbacks` 列表加第二个实例)
- Modify: `gateway-litellm/docker-compose.yml:17-21`(挂载 `./logs`)
- Create: `gateway-litellm/test_gateway_events.py`

**Interfaces:**
- Produces: 文件 `gateway-litellm/logs/llm_events.jsonl`(容器内 `/app/logs/llm_events.jsonl`),每行一个 `llm_call` 事件,字段见 Step 3。Task 5 的 Logtail 配置按这个路径和字段采集到 `audit`;Task 6 的预算和错误率告警按 `cost_usd`、`status`、`employee_id` 聚合。

- [ ] **Step 1: 写失败的测试**(在本机跑,不进容器;用假 `kwargs` 模拟 LiteLLM 传给回调的 `standard_logging_object`)

`gateway-litellm/test_gateway_events.py`:
```python
"""Run: python3 -m pytest test_gateway_events.py -q  (or python3 test_gateway_events.py)"""
import asyncio, json, os, sys, tempfile, types, unittest

# custom_callbacks imports litellm; stub the two names it needs so the test
# runs on a machine without the package.
litellm = types.ModuleType("litellm")
litellm._logging = types.ModuleType("litellm._logging")
class _L:  # noqa: D401
    def warning(self, *a, **k): pass
    def error(self, *a, **k): pass
    def info(self, *a, **k): pass
litellm._logging.verbose_proxy_logger = _L()
litellm.integrations = types.ModuleType("litellm.integrations")
litellm.integrations.custom_logger = types.ModuleType("litellm.integrations.custom_logger")
class CustomLogger:  # noqa: D401
    pass
litellm.integrations.custom_logger.CustomLogger = CustomLogger
sys.modules.update({"litellm": litellm, "litellm._logging": litellm._logging,
                    "litellm.integrations": litellm.integrations,
                    "litellm.integrations.custom_logger": litellm.integrations.custom_logger})
sys.path.insert(0, os.path.dirname(__file__))
import custom_callbacks as cc  # noqa: E402


def _slo(**over):
    base = {
        "id": "chatcmpl-abc", "call_type": "aresponses", "status": "success",
        "model_group": "glm-5", "model": "bedrock/converse/zai.glm-5",
        "custom_llm_provider": "bedrock",
        "startTime": 1757400000.0, "endTime": 1757400001.5,
        "prompt_tokens": 120, "completion_tokens": 30, "total_tokens": 150,
        "response_cost": 0.00042,
        "metadata": {"user_api_key_alias": "emp-peter", "user_api_key_user_id": "emp-peter",
                     "user_api_key_hash": "88aa...", "user_api_key_max_budget": 20.0,
                     "requester_ip_address": "1.2.3.4"},
        "messages": [{"role": "user", "content": "SECRET PROMPT"}],
        "response": {"output": [{"content": "SECRET ANSWER"}]},
        "error_str": None,
    }
    base.update(over)
    return base


class T(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.path = os.path.join(self.dir, "llm_events.jsonl")
        self.lg = cc.GatewayEventLogger(path=self.path)

    def _lines(self):
        with open(self.path, encoding="utf-8") as f:
            return [json.loads(l) for l in f if l.strip()]

    def test_success_event_shape(self):
        asyncio.run(self.lg.async_log_success_event({"standard_logging_object": _slo()}, None, 0, 1))
        (e,) = self._lines()
        for k in ("schema_version", "event_id", "event_type", "occurred_at", "module", "source_id", "level", "message"):
            self.assertIn(k, e)
        self.assertEqual(e["event_type"], "llm_call")
        self.assertEqual(e["module"], "gateway")
        self.assertEqual(e["status"], "success")
        self.assertEqual(e["employee_id"], "emp-peter")
        self.assertEqual(e["model_group"], "glm-5")
        self.assertEqual(e["total_tokens"], 150)
        self.assertAlmostEqual(e["cost_usd"], 0.00042)
        self.assertEqual(e["cost_state"], "estimated")
        self.assertEqual(e["user_max_budget"], 20.0)
        self.assertAlmostEqual(e["latency_ms"], 1500.0)
        self.assertEqual(e["litellm_call_id"], "chatcmpl-abc")
        self.assertTrue(e["occurred_at"].endswith("Z"))

    def test_no_content_or_secrets(self):
        asyncio.run(self.lg.async_log_success_event({"standard_logging_object": _slo()}, None, 0, 1))
        raw = open(self.path, encoding="utf-8").read()
        for bad in ("SECRET PROMPT", "SECRET ANSWER", "88aa", "1.2.3.4", "messages", "response"):
            self.assertNotIn(bad, raw)

    def test_failure_event(self):
        slo = _slo(status="failure", error_str="litellm.RateLimitError: ThrottlingException", response_cost=None,
                   error_information={"error_class": "RateLimitError", "llm_provider": "bedrock", "error_code": "429"})
        asyncio.run(self.lg.async_log_failure_event({"standard_logging_object": slo}, None, 0, 1))
        (e,) = self._lines()
        self.assertEqual(e["status"], "failure")
        self.assertEqual(e["level"], "error")
        self.assertEqual(e["error_class"], "RateLimitError")
        self.assertEqual(e["error_source"], "bedrock")
        self.assertIsNone(e["cost_usd"])

    def test_event_id_is_unique_and_stable_per_line(self):
        for _ in range(3):
            asyncio.run(self.lg.async_log_success_event({"standard_logging_object": _slo()}, None, 0, 1))
        ids = [e["event_id"] for e in self._lines()]
        self.assertEqual(len(set(ids)), 3)

    def test_missing_slo_does_not_raise(self):
        asyncio.run(self.lg.async_log_success_event({}, None, 0, 1))
        self.assertEqual(self._lines(), [])

    def test_rotation_renames_not_truncates(self):
        lg = cc.GatewayEventLogger(path=self.path, max_bytes=600, backups=2)
        for _ in range(10):
            asyncio.run(lg.async_log_success_event({"standard_logging_object": _slo()}, None, 0, 1))
        self.assertTrue(os.path.exists(self.path + ".1"))
        self.assertFalse(os.path.exists(self.path + ".3"))


if __name__ == "__main__":
    unittest.main()
```

- [ ] **Step 2: 跑测试确认失败**

```bash
cd /root/pp_home/windows-pc/gateway-litellm && python3 test_gateway_events.py 2>&1 | tail -3
```
Expected: `AttributeError: module 'custom_callbacks' has no attribute 'GatewayEventLogger'`。

- [ ] **Step 3: 实现 GatewayEventLogger**(追加到 `custom_callbacks.py` 末尾,`proxy_handler_instance = ReasoningFlattener()` 之后)

```python
# ---------------------------------------------------------------------------
# llm_call events for the unified log (phase 1).
#
# One JSON line per finished request, appended to /app/logs/llm_events.jsonl
# (bind-mounted from ./logs on the host) and shipped to SLS by Logtail. The
# file holds *no* prompt, response, header or key material: only identity
# aliases, routing facts, status, timing, usage and LiteLLM's cost estimate.
# LiteLLM's own spend log in Postgres stays the reconciliation source.
# ---------------------------------------------------------------------------

import datetime as _dt
import logging as _logging
import logging.handlers as _handlers
import socket as _socket
import uuid as _uuid

_EVENT_PATH = os.environ.get("GATEWAY_EVENT_LOG", "/app/logs/llm_events.jsonl")


class GatewayEventLogger(CustomLogger):
    """Writes one llm_call event per request. Never raises into the proxy."""

    def __init__(self, path: str = _EVENT_PATH, max_bytes: int = 50 * 1024 * 1024, backups: int = 5):
        self._path = path
        self._source = os.environ.get("HOSTNAME") or _socket.gethostname()
        self._log = _logging.getLogger("gateway.events." + str(id(self)))
        self._log.propagate = False
        self._log.setLevel(_logging.INFO)
        os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
        # RotatingFileHandler renames (.1, .2 ...) and reopens; it never
        # truncates in place, so Logtail keeps reading the renamed file.
        h = _handlers.RotatingFileHandler(path, maxBytes=max_bytes, backupCount=backups, encoding="utf-8")
        h.setFormatter(_logging.Formatter("%(message)s"))
        self._log.addHandler(h)

    # LiteLLM calls these with (kwargs, response_obj, start_time, end_time).
    async def async_log_success_event(self, kwargs, response_obj, start_time, end_time):
        self._emit(kwargs, "success")

    async def async_log_failure_event(self, kwargs, response_obj, start_time, end_time):
        self._emit(kwargs, "failure")

    def _emit(self, kwargs, status) -> None:
        try:
            slo = (kwargs or {}).get("standard_logging_object")
            if not isinstance(slo, dict):
                return
            self._log.info(json.dumps(self._event(slo, status), ensure_ascii=False, separators=(",", ":")))
        except Exception as exc:  # noqa: BLE001 - logging must never fail a request
            verbose_proxy_logger.warning("gateway event not written: %s", exc)

    def _event(self, slo: dict, status: str) -> dict:
        meta = slo.get("metadata") or {}
        start, end = slo.get("startTime"), slo.get("endTime")
        latency_ms = round((end - start) * 1000.0, 1) if isinstance(start, (int, float)) and isinstance(end, (int, float)) else None
        occurred = _dt.datetime.fromtimestamp(end, tz=_dt.timezone.utc) if isinstance(end, (int, float)) else _dt.datetime.now(_dt.timezone.utc)
        slo_status = slo.get("status") or status
        if slo_status not in ("success", "failure"):
            slo_status = status
        err = slo.get("error_information") or {}
        cost = slo.get("response_cost")
        cost = float(cost) if isinstance(cost, (int, float)) else None
        alias = meta.get("user_api_key_alias")
        user_id = meta.get("user_api_key_user_id")
        model_group = slo.get("model_group") or (slo.get("model_map_information") or {}).get("model_map_key")
        msg = f"{slo_status} {model_group or '?'} -> {slo.get('model') or '?'} ({alias or user_id or 'anon'})"
        if slo_status == "failure":
            msg += ": " + (err.get("error_class") or "error") + (f" {err.get('error_code')}" if err.get("error_code") else "")
        return {
            "schema_version": 1,
            "event_id": str(_uuid.uuid4()),
            "event_type": "llm_call",
            "occurred_at": occurred.strftime("%Y-%m-%dT%H:%M:%S.") + f"{occurred.microsecond // 1000:03d}Z",
            "module": "gateway",
            "source_id": self._source,
            "level": "info" if slo_status == "success" else "error",
            "message": msg,
            "status": slo_status,
            "call_type": slo.get("call_type"),
            "litellm_call_id": slo.get("id"),
            "employee_id": user_id,
            "key_alias": alias,
            "model_group": model_group,
            "model": slo.get("model"),
            "provider": slo.get("custom_llm_provider"),
            "latency_ms": latency_ms,
            "prompt_tokens": slo.get("prompt_tokens"),
            "completion_tokens": slo.get("completion_tokens"),
            "total_tokens": slo.get("total_tokens"),
            "cost_usd": cost,
            "cost_source": "litellm_estimate",
            "cost_state": "estimated" if cost is not None else "unknown",
            "user_max_budget": meta.get("user_api_key_max_budget"),
            "error_class": err.get("error_class") or None,
            "error_code": str(err.get("error_code")) if err.get("error_code") not in (None, "") else None,
            "error_source": err.get("llm_provider") or None,
            "cache_hit": slo.get("cache_hit"),
        }


gateway_event_logger_instance = GatewayEventLogger()
```

注意:`response_cost`、`error_information`、`model_group`、`user_api_key_max_budget` 的键名来自 LiteLLM `StandardLoggingPayload`;Step 6 上线后必须用真实事件核对一遍,键名不同就以真实为准改代码和测试。

- [ ] **Step 4: 跑测试确认通过**

```bash
cd /root/pp_home/windows-pc/gateway-litellm && python3 test_gateway_events.py 2>&1 | tail -3
```
Expected: `OK`,6 个测试通过。

- [ ] **Step 5: 挂载与注册回调**

`docker-compose.yml` 的 `volumes:` 增加一行(放在 custom_callbacks 之后):
```yaml
      # llm_call 事件文件,Logtail 从宿主机 ./logs 采到 SLS audit。见 custom_callbacks.py 末尾。
      - ./logs:/app/logs
```
`config.yaml` 的 `litellm_settings.callbacks` 改为列表:
```yaml
  callbacks:
    - custom_callbacks.proxy_handler_instance
    - custom_callbacks.gateway_event_logger_instance
```

- [ ] **Step 6: 部署到网关并用真实请求核对键名**

```bash
ssh ec2-user@175.41.186.104 'cd ~/gateway-litellm && cp config.yaml config.yaml.bak-$(date +%Y%m%d)-pre-events && cp docker-compose.yml docker-compose.yml.bak-$(date +%Y%m%d)-pre-events && mkdir -p logs'
scp custom_callbacks.py config.yaml docker-compose.yml ec2-user@175.41.186.104:~/gateway-litellm/
ssh ec2-user@175.41.186.104 'cd ~/gateway-litellm && docker compose up -d --force-recreate litellm && sleep 20 && docker compose logs --tail 20 litellm | grep -i -E "error|callback" ; \
  K=$(grep LITELLM_MASTER_KEY .env | cut -d= -f2); \
  curl -s -o /dev/null -w "%{http_code}\n" -X POST -H "Authorization: Bearer $K" -H "Content-Type: application/json" http://127.0.0.1:4000/v1/responses -d "{\"model\":\"glm-5\",\"input\":\"ping\",\"max_output_tokens\":8}"; \
  curl -s -o /dev/null -w "%{http_code}\n" -X POST -H "Authorization: Bearer $K" -H "Content-Type: application/json" http://127.0.0.1:4000/v1/responses -d "{\"model\":\"no-such-model\",\"input\":\"ping\"}"; \
  sleep 3; tail -2 logs/llm_events.jsonl'
```
Expected: 两行事件,一条 `status=success` 带 `cost_usd` 数值、`employee_id`,一条 `status=failure` 带 `error_class`。如果某字段是 `null` 而 LiteLLM 明明有值,进容器 `docker compose exec litellm python -c "from litellm.types.utils import StandardLoggingPayload; print(StandardLoggingPayload.__annotations__.keys())"` 核对键名并回改 Step 3 与 Step 1。

回滚:`cp config.yaml.bak-*-pre-events config.yaml; cp docker-compose.yml.bak-*-pre-events docker-compose.yml; docker compose up -d --force-recreate litellm`。

- [ ] **Step 7: 记录**(gateway-litellm 不是 git 仓库,只写 README 决策记录,见 Task 8)

---

### Task 3: 控制台结构化运行日志与审计副本

**Files:**
- Create: `go/internal/eventlog/eventlog.go`, `go/internal/eventlog/eventlog_test.go`, `go/internal/eventlog/redact.go`, `go/internal/eventlog/redact_test.go`
- Modify: `go/internal/admincore/audit.go:52-80`(`appendAudit` 同时写 SLS 副本)
- Modify: `go/internal/admincore/manager.go`(`Manager` 增加 `Events *eventlog.Writer` 字段;找到 `type Manager struct` 追加)
- Modify: `go/internal/adminweb/server.go`(`Options.LogDir`;`New` 里建 writer;`Handler()` 外层加访问日志中间件)
- Modify: `go/cmd/admin/web_cmd.go`(`--log-dir` 参数)

**Interfaces:**
- Produces:
  - `eventlog.New(dir string, source string) (*Writer, error)`:`dir` 为空返回可用但只写 stderr 的 writer(本地运行不落盘)。
  - `(*Writer).Ops(level, eventType, msg string, fields map[string]any)` 写 `<dir>/admin.jsonl`。
  - `(*Writer).Audit(eventType, msg string, fields map[string]any)` 写 `<dir>/audit.jsonl`,level 固定 `info`。
  - `eventlog.Redact(map[string]any) map[string]any`:按键名与值前缀脱敏。
  - 文件:`/var/log/ai-env-mgr/admin.jsonl`(ops)、`/var/log/ai-env-mgr/audit.jsonl`(audit);轮转 `.1`…`.5`,50MB。
- Consumes:Task 2 的公共字段约定。

- [ ] **Step 1: 写失败的测试**

`go/internal/eventlog/eventlog_test.go`:
```go
package eventlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad json line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

func TestOpsAndAuditGoToSeparateFilesWithCommonFields(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "console-test")
	if err != nil {
		t.Fatal(err)
	}
	w.Ops("warn", "platform_event", "oss slow", map[string]any{"latency_ms": 1200})
	w.Audit("admin_action", "quota changed", map[string]any{"employee_id": "alice", "action": "quota"})

	ops := readLines(t, filepath.Join(dir, "admin.jsonl"))
	aud := readLines(t, filepath.Join(dir, "audit.jsonl"))
	if len(ops) != 1 || len(aud) != 1 {
		t.Fatalf("want 1 ops + 1 audit line, got %d + %d", len(ops), len(aud))
	}
	for _, e := range []map[string]any{ops[0], aud[0]} {
		for _, k := range []string{"schema_version", "event_id", "event_type", "occurred_at", "module", "source_id", "level", "message"} {
			if _, ok := e[k]; !ok {
				t.Errorf("missing %s in %v", k, e)
			}
		}
		if e["module"] != "console" || e["source_id"] != "console-test" {
			t.Errorf("module/source wrong: %v", e)
		}
		if !strings.HasSuffix(e["occurred_at"].(string), "Z") {
			t.Errorf("occurred_at not UTC: %v", e["occurred_at"])
		}
	}
	if ops[0]["level"] != "warn" || ops[0]["latency_ms"].(float64) != 1200 {
		t.Errorf("ops fields: %v", ops[0])
	}
	if aud[0]["level"] != "info" || aud[0]["employee_id"] != "alice" {
		t.Errorf("audit fields: %v", aud[0])
	}
	if ops[0]["event_id"] == aud[0]["event_id"] {
		t.Errorf("event ids must differ")
	}
}

func TestEmptyDirWritesNothingAndDoesNotFail(t *testing.T) {
	w, err := New("", "x")
	if err != nil {
		t.Fatal(err)
	}
	w.Ops("info", "agent_event", "no dir", nil)
	w.Audit("admin_action", "no dir", nil)
}

func TestRotationRenamesAndKeepsBackups(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "x")
	if err != nil {
		t.Fatal(err)
	}
	w.ops.maxBytes = 400
	w.ops.backups = 2
	for i := 0; i < 20; i++ {
		w.Ops("info", "platform_event", strings.Repeat("x", 100), nil)
	}
	if _, err := os.Stat(filepath.Join(dir, "admin.jsonl.1")); err != nil {
		t.Fatalf("expected admin.jsonl.1: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "admin.jsonl.3")); err == nil {
		t.Fatalf("admin.jsonl.3 must not exist with backups=2")
	}
	// The live file was reopened, not truncated in place: it must be small
	// and end with a full line.
	b, _ := os.ReadFile(filepath.Join(dir, "admin.jsonl"))
	if len(b) == 0 || b[len(b)-1] != '\n' || len(b) > 400+300 {
		t.Fatalf("live file wrong after rotation: %d bytes", len(b))
	}
}

func TestFieldsAreRedactedBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	w, _ := New(dir, "x")
	w.Ops("error", "platform_event", "gateway said no", map[string]any{
		"authorization": "Bearer sk-abcdef", "detail": "key sk-1234567890 rejected", "ok": true,
	})
	raw, _ := os.ReadFile(filepath.Join(dir, "admin.jsonl"))
	if strings.Contains(string(raw), "sk-abcdef") || strings.Contains(string(raw), "sk-1234567890") {
		t.Fatalf("secret leaked: %s", raw)
	}
}
```

`go/internal/eventlog/redact_test.go`:
```go
package eventlog

import "testing"

func TestRedactByKeyAndByValue(t *testing.T) {
	in := map[string]any{
		"Authorization":             "Bearer abc",
		"cookie":                    "session=1",
		"access_key_secret":         "xyz",
		"experimental_bearer_token": "sk-111",
		"note":                      "token sk-abcdefghijklmnop was used, AK LTAI5tABCDEFGHIJKLMN too",
		"employee_id":               "alice",
		"count":                     3,
		"nested":                    map[string]any{"api_key": "k", "fine": "v"},
	}
	out := Redact(in)
	for _, k := range []string{"Authorization", "cookie", "access_key_secret", "experimental_bearer_token"} {
		if out[k] != "[redacted]" {
			t.Errorf("%s not redacted: %v", k, out[k])
		}
	}
	if out["note"] != "token [redacted] was used, AK [redacted] too" {
		t.Errorf("value redaction: %v", out["note"])
	}
	if out["employee_id"] != "alice" || out["count"] != 3 {
		t.Errorf("plain fields changed: %v", out)
	}
	n := out["nested"].(map[string]any)
	if n["api_key"] != "[redacted]" || n["fine"] != "v" {
		t.Errorf("nested: %v", n)
	}
	if in["Authorization"] != "Bearer abc" {
		t.Errorf("input must not be mutated")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
cd /root/pp_home/windows-pc/ai-env-mgr/go && go test ./internal/eventlog/ 2>&1 | head -5
```
Expected: 编译失败,`undefined: New` / `undefined: Redact`。

- [ ] **Step 3: 实现 redact.go**

```go
package eventlog

import (
	"regexp"
	"strings"
)

// Keys whose value is never written, whatever it looks like. Compared
// case-insensitively after removing '-' and '_'.
var secretKeys = map[string]bool{
	"authorization": true, "cookie": true, "setcookie": true, "password": true,
	"apikey": true, "accesskeyid": true, "accesskeysecret": true, "secret": true,
	"token": true, "bearer": true, "experimentalbearertoken": true, "masterkey": true,
	"litellmmasterkey": true, "aienvmgrgatewayadminkey": true,
}

// Value patterns: LiteLLM/OpenAI style keys and Alibaba Cloud AccessKey ids.
var secretValues = regexp.MustCompile(`sk-[A-Za-z0-9_\-]{6,}|LTAI[A-Za-z0-9]{12,}`)

func normKey(k string) string {
	return strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(k))
}

// Redact returns a copy of fields with secret keys replaced by "[redacted]"
// and secret-looking substrings removed from string values, recursively.
// The input is not modified.
func Redact(fields map[string]any) map[string]any {
	if fields == nil {
		return nil
	}
	out := make(map[string]any, len(fields))
	for k, v := range fields {
		if secretKeys[normKey(k)] {
			out[k] = "[redacted]"
			continue
		}
		out[k] = redactValue(v)
	}
	return out
}

func redactValue(v any) any {
	switch x := v.(type) {
	case string:
		return secretValues.ReplaceAllString(x, "[redacted]")
	case map[string]any:
		return Redact(x)
	case []any:
		c := make([]any, len(x))
		for i := range x {
			c[i] = redactValue(x[i])
		}
		return c
	case error:
		return secretValues.ReplaceAllString(x.Error(), "[redacted]")
	default:
		return v
	}
}
```

- [ ] **Step 4: 实现 eventlog.go**

```go
// Package eventlog writes the console's structured events for the unified
// log: one JSON object per line, ops and audit in separate files, rotated by
// renaming so the shipper (Logtail) never loses a line to truncation.
//
// It deliberately does not talk to SLS. The file is the durable queue; the
// shipper owns retries, checkpoints and rotation tracking. When no directory
// is configured events go to stderr and nothing is kept, which is what a
// developer running `admin` locally wants.
package eventlog

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const SchemaVersion = 1

// Writer emits console events. Safe for concurrent use.
type Writer struct {
	source string
	ops    *rotatingFile
	audit  *rotatingFile
	stderr io.Writer
}

// New opens <dir>/admin.jsonl and <dir>/audit.jsonl. An empty dir means
// stderr only.
func New(dir, source string) (*Writer, error) {
	w := &Writer{source: source, stderr: os.Stderr}
	if dir == "" {
		return w, nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("eventlog: %w", err)
	}
	var err error
	if w.ops, err = openRotating(filepath.Join(dir, "admin.jsonl")); err != nil {
		return nil, err
	}
	if w.audit, err = openRotating(filepath.Join(dir, "audit.jsonl")); err != nil {
		return nil, err
	}
	return w, nil
}

// Ops records a runtime event. level is debug|info|warn|error.
func (w *Writer) Ops(level, eventType, msg string, fields map[string]any) {
	w.emit(w.ops, level, eventType, msg, fields)
}

// Audit records an administrative fact. Always level info.
func (w *Writer) Audit(eventType, msg string, fields map[string]any) {
	w.emit(w.audit, "info", eventType, msg, fields)
}

func (w *Writer) emit(dst *rotatingFile, level, eventType, msg string, fields map[string]any) {
	if w == nil {
		return
	}
	ev := map[string]any{
		"schema_version": SchemaVersion,
		"event_id":       newID(),
		"event_type":     eventType,
		"occurred_at":    time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"module":         "console",
		"source_id":      w.source,
		"level":          level,
		"message":        secretValues.ReplaceAllString(msg, "[redacted]"),
	}
	for k, v := range Redact(fields) {
		if _, taken := ev[k]; !taken {
			ev[k] = v
		}
	}
	line, err := json.Marshal(ev)
	if err != nil {
		fmt.Fprintf(w.stderr, "eventlog: encode: %v\n", err)
		return
	}
	line = append(line, '\n')
	if dst == nil {
		w.stderr.Write(line)
		return
	}
	if err := dst.write(line); err != nil {
		fmt.Fprintf(w.stderr, "eventlog: write %s: %v\n", dst.path, err)
	}
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// rotatingFile appends lines and, past maxBytes, renames the file to .1
// (shifting older backups up) and reopens. Whole lines only: a line is
// never split across the rotation.
type rotatingFile struct {
	mu       sync.Mutex
	path     string
	f        *os.File
	size     int64
	maxBytes int64
	backups  int
}

func openRotating(path string) (*rotatingFile, error) {
	r := &rotatingFile{path: path, maxBytes: 50 << 20, backups: 5}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("eventlog: open %s: %w", r.path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *rotatingFile) write(line []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size > 0 && r.size+int64(len(line)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			return err
		}
	}
	n, err := r.f.Write(line)
	r.size += int64(n)
	return err
}

func (r *rotatingFile) rotate() error {
	r.f.Close()
	for i := r.backups - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
	}
	if err := os.Rename(r.path, r.path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	os.Remove(fmt.Sprintf("%s.%d", r.path, r.backups+1))
	return r.open()
}
```

- [ ] **Step 5: 跑测试确认通过**

```bash
cd /root/pp_home/windows-pc/ai-env-mgr/go && go test ./internal/eventlog/ -count=1
```
Expected: `ok`。

- [ ] **Step 6: 审计副本**(`admincore/audit.go` 的 `appendAudit`,在 `json.Marshal(entry)` 成功之后、读 OSS 之前追加)

```go
	// SLS copy (phase 1): same fact, unified shape. The OSS history stays the
	// page's source; this line is the searchable, alertable duplicate.
	if m.Events != nil {
		m.Events.Audit("admin_action", fmt.Sprintf("%s %s", action, windowsUser), map[string]any{
			"action": string(action), "employee_id": strings.ToLower(windowsUser), "target": "employee", "detail": detail,
		})
	}
```
在 `Manager` 结构体加字段 `Events *eventlog.Writer // nil means no unified log`,并在 `audit.go` 的 import 里加 `"fmt"` 和 `"github.com/TEENet-io/ai-env-mgr/internal/eventlog"`。`audit_test.go` 现有测试不传 Events,必须继续通过(nil 安全)。

- [ ] **Step 7: 控制台接线**

`adminweb.Options` 增加:
```go
	// LogDir is where the unified-log files go (admin.jsonl, audit.jsonl).
	// Empty means stderr only, which is right for a developer's laptop.
	LogDir string
```
`New(opts)` 里(建 Manager 之前):
```go
	host, _ := os.Hostname()
	events, err := eventlog.New(opts.LogDir, "console@"+host)
	if err != nil {
		return nil, err
	}
	s.events = events
	// ...wherever the Manager is constructed: mgr.Events = events
```
`Server` 结构体加 `events *eventlog.Writer`。`Handler()` 返回前套一层访问日志中间件:
```go
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/static/") {
			return
		}
		level := "info"
		if rec.status >= 500 {
			level = "error"
		} else if rec.status >= 400 {
			level = "warn"
		}
		s.events.Ops(level, "http_access", r.Method+" "+r.URL.Path, map[string]any{
			"method": r.Method, "path": r.URL.Path, "status": rec.status,
			"latency_ms": float64(time.Since(start).Microseconds()) / 1000.0,
			"ok": rec.status < 400,
		})
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(c int) { r.status = c; r.ResponseWriter.WriteHeader(c) }
```
查询串不记(可能带员工名以外的东西);`Cookie` 不记。`ListenAndServe` 启动与退出处各写一条 `Ops("info","platform_event","console started"/"console stopping", {"version": version})`(version 从 Options 传入或用现有的构建变量)。

`cmd/admin/web_cmd.go` 增加参数解析,与 `--listen` 同款式:
```go
		case "--log-dir":
			if i+1 >= len(args) {
				return fmt.Errorf("--log-dir needs a value")
			}
			opts.LogDir = args[i+1]
			i++
```
`usage()` 里加一行 `[--log-dir <dir>]   write admin.jsonl / audit.jsonl there for the unified log`。

- [ ] **Step 8: 全量检查与提交**

```bash
cd /root/pp_home/windows-pc/ai-env-mgr/go && make check
cd .. && git add go/internal/eventlog go/internal/admincore/audit.go go/internal/admincore/manager.go go/internal/adminweb/server.go go/cmd/admin/web_cmd.go
git commit -m "console: write ops and audit events as JSON lines for the unified log" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```
Expected: `all checks passed`。

- [ ] **Step 9: 发布 web-34 并挂上 --log-dir**

```bash
# 构建与以往 web-3x 一致(make dist),上传到 /opt/ai-env-mgr/web-34,切 admin 软链。然后:
ssh root@47.236.115.50 'mkdir -p /var/log/ai-env-mgr && chown $(stat -c %U /opt/ai-env-mgr/web-34) /var/log/ai-env-mgr && \
  cp /etc/systemd/system/ai-env-mgr-admin.service /root/ai-env-mgr-admin.service.bak-$(date +%Y%m%d) && \
  sed -i "s|--behind-proxy|--behind-proxy --log-dir /var/log/ai-env-mgr|" /etc/systemd/system/ai-env-mgr-admin.service && \
  systemctl daemon-reload && systemctl restart ai-env-mgr-admin && sleep 2 && systemctl is-active ai-env-mgr-admin && \
  curl -s -o /dev/null -w "login %{http_code}\n" http://172.23.0.1:9080/login && tail -1 /var/log/ai-env-mgr/admin.jsonl'
```
Expected: `active`、`login 200`、最后一行是 `http_access` 或 `console started` 事件。

回滚:恢复 `.bak` 的 unit 文件、`systemctl daemon-reload && systemctl restart ai-env-mgr-admin`,软链切回 web-33。

---

### Task 4: 独立网关健康探测

**Files:**
- Create: `ops/probe/gateway-probe.sh`, `ops/probe/gateway-probe.service`, `ops/probe/gateway-probe.timer`

**Interfaces:**
- Produces:控制台主机 `/var/log/ai-env-mgr/probe.jsonl`,每分钟一条 `platform_event`(`module=probe`,`target=gateway`,`ok=true|false`,`status`,`latency_ms`)。Task 5 采到 `ops`;Task 6 的"网关不可用"告警只看这个文件的事件,不依赖网关自身日志。

- [ ] **Step 1: 探测脚本**

```bash
#!/usr/bin/env bash
# One line per run into probe.jsonl. Reads only the public health endpoint,
# carries no key. Runs from a systemd timer on the console host.
set -u
OUT=${PROBE_LOG:-/var/log/ai-env-mgr/probe.jsonl}
URL=${GATEWAY_HEALTH_URL:-https://litellm.teenet.app/health/liveliness}
start=$(date +%s%3N)
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$URL" || echo 000)
end=$(date +%s%3N)
ok=false; level=error; msg="gateway probe failed ($code)"
if [ "$code" = "200" ]; then ok=true; level=info; msg="gateway probe ok"; fi
printf '{"schema_version":1,"event_id":"%s","event_type":"platform_event","occurred_at":"%s","module":"probe","source_id":"%s","level":"%s","message":"%s","target":"gateway","url":"%s","status":%s,"ok":%s,"latency_ms":%s}\n' \
  "$(cat /proc/sys/kernel/random/uuid)" "$(date -u +%Y-%m-%dT%H:%M:%S.000Z)" "probe@$(hostname)" "$level" "$msg" "$URL" "$((10#$code))" "$ok" "$((end-start))" >> "$OUT"
```
`$((10#$code))` 把 `000` 变成数字 `0`,`200` 保持 `200`,避免 JSON 里出现前导零。

- [ ] **Step 2: 单元与 timer**

`gateway-probe.service`:
```ini
[Unit]
Description=Independent LiteLLM gateway health probe (unified log)
[Service]
Type=oneshot
ExecStart=/opt/ai-env-mgr/ops/gateway-probe.sh
```
`gateway-probe.timer`:
```ini
[Unit]
Description=Run the gateway probe every minute
[Timer]
OnBootSec=1min
OnUnitActiveSec=1min
AccuracySec=5s
[Install]
WantedBy=timers.target
```

- [ ] **Step 3: 本地验证脚本本身**

```bash
chmod +x ops/probe/gateway-probe.sh && PROBE_LOG=/tmp/claude-0/-root-pp-home-windows-pc/2a2f706f-81a1-4b1b-8845-3d82b6c45d0c/scratchpad/probe.jsonl ops/probe/gateway-probe.sh && tail -1 "$PROBE_LOG" | python3 -m json.tool | head -12
PROBE_LOG=... GATEWAY_HEALTH_URL=https://127.0.0.1:1/ ops/probe/gateway-probe.sh && tail -1 "$PROBE_LOG"
```
Expected: 第一条 `"ok": true, "status": 200`;第二条 `"ok": false, "status": 0, "level": "error"`。

- [ ] **Step 4: 部署到控制台主机**

```bash
scp ops/probe/gateway-probe.sh root@47.236.115.50:/opt/ai-env-mgr/ops/gateway-probe.sh
scp ops/probe/gateway-probe.service ops/probe/gateway-probe.timer root@47.236.115.50:/etc/systemd/system/
scp ops/probe/gateway-probe.logrotate root@47.236.115.50:/etc/logrotate.d/gateway-probe
ssh root@47.236.115.50 'chmod +x /opt/ai-env-mgr/ops/gateway-probe.sh && systemctl daemon-reload && systemctl enable --now gateway-probe.timer && logrotate -d /etc/logrotate.d/gateway-probe 2>&1 | tail -2 && sleep 65 && tail -1 /var/log/ai-env-mgr/probe.jsonl'
```
probe.jsonl 由 logrotate 按 50MB 重命名轮转(不 copytruncate),保留 5 份。
回滚:`systemctl disable --now gateway-probe.timer`。

- [ ] **Step 5: 提交**

```bash
git add ops/probe && git commit -m "ops: independent gateway health probe feeding the unified log" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: 两台主机装 Logtail 并下发采集配置

依赖 Task 1(机器组、writer AK)。Logtail 在非阿里云主机上需要用户标识和 AK;在同账号 ECS 上只需地域。官方安装文档在实施当天再核对一次命令(安装脚本 URL、参数名会变),以文档为准,这里记录目标状态。

**Files:**
- Create: `ops/sls/pipeline-console-ops.json`, `ops/sls/pipeline-console-audit.json`, `ops/sls/pipeline-gateway-audit.json`, `ops/sls/pipeline-gateway-stdout.json`, `ops/sls/apply-pipelines.sh`

**Interfaces:**
- Consumes:Task 2 的 `~/gateway-litellm/logs/llm_events.jsonl`;Task 3 的 `/var/log/ai-env-mgr/admin.jsonl`、`audit.jsonl`;Task 4 的 `probe.jsonl`;LiteLLM 容器 stdout。
- Produces:SLS `ops`/`audit` 里可查的事件,字段名与 JSON 键一致,`__time__` 取自 `occurred_at`。

- [ ] **Step 1: 控制台主机(阿里云 ECS,新加坡)装 Logtail**

```bash
ssh root@47.236.115.50 'wget -q http://logtail-release-ap-southeast-1.oss-ap-southeast-1-internal.aliyuncs.com/linux64/logtail.sh -O /root/logtail.sh && sh /root/logtail.sh install ap-southeast-1 && \
  mkdir -p /etc/ilogtail && echo wc-console > /etc/ilogtail/user_defined_id && /etc/init.d/ilogtaild restart && sleep 3 && /etc/init.d/ilogtaild status'
```
Expected: `ilogtail is running`。回滚:`sh /root/logtail.sh uninstall`。

- [ ] **Step 2: 网关主机(AWS EC2,非阿里云)装 Logtail**

```bash
ssh ec2-user@175.41.186.104 'wget -q http://logtail-release-ap-southeast-1.oss-ap-southeast-1.aliyuncs.com/linux64/logtail.sh -O ~/logtail.sh && sudo sh ~/logtail.sh install ap-southeast-1-internet && \
  sudo mkdir -p /etc/ilogtail/users && sudo touch /etc/ilogtail/users/<主账号UID> && echo wc-gateway | sudo tee /etc/ilogtail/user_defined_id && sudo /etc/init.d/ilogtaild restart'
```
`<主账号UID>` 是阿里云主账号 ID(Task 1 时 `aliyun sts GetCallerIdentity --profile sls-bootstrap` 的 `AccountId`)。若安装脚本要求写入 AK(`/etc/ilogtail/...` 或 `ilogtail_config.json`),用 **wc-logs-writer** 的 AK,绝不用 bootstrap AK。回滚同上。

- [ ] **Step 3: 确认机器组心跳**

```bash
aliyun sls ListMachines --project windows-control-logs --machineGroup console-host --profile sls-bootstrap --region ap-southeast-1
aliyun sls ListMachines --project windows-control-logs --machineGroup gateway-host --profile sls-bootstrap --region ap-southeast-1
```
Expected: 各一台机器,`lastHeartbeatTime` 在 1 分钟内。没有心跳先解决网络/标识,再往下。

- [ ] **Step 4: 采集配置**

四个配置同一形状,只有路径、Logstore、来源不同。`pipeline-console-ops.json`:
```json
{
  "configName": "console-ops",
  "logSample": "{\"schema_version\":1,\"event_type\":\"http_access\",\"occurred_at\":\"2026-09-12T01:02:03.000Z\"}",
  "inputs": [{"Type": "input_file", "FilePaths": ["/var/log/ai-env-mgr/admin.jsonl", "/var/log/ai-env-mgr/probe.jsonl"], "MaxDirSearchDepth": 0, "FileEncoding": "utf8"}],
  "processors": [
    {"Type": "processor_parse_json_native", "SourceKey": "content", "KeepingSourceWhenParseFail": true, "KeepingSourceWhenParseSucceed": false},
    {"Type": "processor_parse_timestamp_native", "SourceKey": "occurred_at", "SourceFormat": "%Y-%m-%dT%H:%M:%S.%fZ", "SourceTimezone": "GMT+00:00"}
  ],
  "flushers": [{"Type": "flusher_sls", "Logstore": "ops"}]
}
```
`pipeline-console-audit.json`:同上,`configName: console-audit`,`FilePaths: ["/var/log/ai-env-mgr/audit.jsonl"]`,`Logstore: audit`。
`pipeline-gateway-audit.json`:`configName: gateway-audit`,`FilePaths: ["/home/ec2-user/gateway-litellm/logs/llm_events.jsonl"]`,`Logstore: audit`。
`pipeline-gateway-stdout.json`(容器 stdout,LiteLLM 自身日志,进 ops;这些行不是我们的事件格式,只做多行合并和时间解析):
```json
{
  "configName": "gateway-stdout",
  "inputs": [{"Type": "service_docker_stdout", "Stdout": true, "Stderr": true, "IncludeLabel": {"com.docker.compose.service": "litellm"}, "BeginLineRegex": "^(\\d{2}:\\d{2}:\\d{2} - LiteLLM|INFO:|WARNING:|ERROR:|\\x1b\\[)"}],
  "processors": [
    {"Type": "processor_add_fields", "Fields": {"module": "gateway", "event_type": "process_log", "schema_version": "1"}, "IgnoreIfExist": true}
  ],
  "flushers": [{"Type": "flusher_sls", "Logstore": "ops"}]
}
```
`apply-pipelines.sh`:
```bash
#!/usr/bin/env bash
set -euo pipefail
PROFILE=${PROFILE:-sls-bootstrap}; PRODUCT=${PRODUCT:-sls}; REGION=ap-southeast-1; PROJECT=windows-control-logs
HERE=$(cd "$(dirname "$0")" && pwd)
A() { aliyun "$PRODUCT" "$@" --profile "$PROFILE" --region "$REGION" --force; }
apply() { # name file group
  A GetLogtailPipelineConfig --project "$PROJECT" --configName "$1" >/dev/null 2>&1 \
    && A UpdateLogtailPipelineConfig --project "$PROJECT" --configName "$1" --body "file://$HERE/$2" \
    || A CreateLogtailPipelineConfig --project "$PROJECT" --body "file://$HERE/$2"
  A ApplyConfigToMachineGroup --project "$PROJECT" --machineGroup "$3" --configName "$1"
}
apply console-ops     pipeline-console-ops.json     console-host
apply console-audit   pipeline-console-audit.json   console-host
apply gateway-audit   pipeline-gateway-audit.json   gateway-host
apply gateway-stdout  pipeline-gateway-stdout.json  gateway-host
```

- [ ] **Step 5: 下发并验证端到端**

```bash
chmod +x ops/sls/apply-pipelines.sh && PROFILE=sls-bootstrap ops/sls/apply-pipelines.sh
# 触发一条 probe、一条网关调用,90 秒后查
sleep 90
aliyun sls GetLogs --project windows-control-logs --logstore ops   --from $(( $(date +%s) - 600 )) --to $(date +%s) --query 'module: probe' --line 2 --profile sls-bootstrap --region ap-southeast-1
aliyun sls GetLogs --project windows-control-logs --logstore audit --from $(( $(date +%s) - 600 )) --to $(date +%s) --query 'event_type: llm_call' --line 2 --profile sls-bootstrap --region ap-southeast-1
```
Expected: 两个查询各返回至少一条,字段已拆开(`event_id`、`status` 等是独立键,不是整段 `content`),`__time__` 与 `occurred_at` 一致(误差 0)。若 `content` 未拆,检查 `processor_parse_json_native` 是否为该 Logtail 版本支持的名字(旧版用 `processor_json`),按 Logtail 版本改。

- [ ] **Step 6: 提交**

```bash
git add ops/sls/pipeline-*.json ops/sls/apply-pipelines.sh
git commit -m "ops: Logtail pipelines for the console and gateway hosts" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: 告警规则与企业微信通知

依赖 Task 5 有数据。用 SLS 告警 2.0(`CreateAlert`,资源类型 `alert`),行动策略里配企业微信 webhook。四条规则,阈值是 spec §7 的试点初值。

**Files:**
- Create: `ops/sls/alerts/webhook-wecom.md`(只写步骤,不写 webhook 地址)、`ops/sls/alerts/gateway-down.json`、`ops/sls/alerts/upstream-error-rate.json`、`ops/sls/alerts/budget-80.json`、`ops/sls/alerts/collection-stalled.json`、`ops/sls/alerts/apply-alerts.sh`

- [ ] **Step 1: 企业微信通道**(SLS 控制台一次性操作,记录在 `webhook-wecom.md`)

在 SLS 控制台 → 告警 → 通知对象 → 新建 Webhook 集成,类型「企业微信」,粘贴前置输入 2 的机器人地址,名称 `wecom-ops`;新建行动策略 `wecom-ops-policy`:所有等级 → `wecom-ops`,重复通知间隔 30 分钟,恢复时通知。记录两者的 ID。

- [ ] **Step 2: 规则**

`gateway-down.json`(查 probe 事件,连续 3 分钟失败):
```json
{
  "name": "wc-gateway-down",
  "displayName": "网关不可用(独立探测连续失败)",
  "description": "probe.jsonl 里最近 5 分钟内失败 ≥3 次且成功 0 次",
  "configuration": {
    "version": "2.0", "type": "default", "dashboard": "internal-alert-analysis",
    "queryList": [{"storeType": "log", "project": "windows-control-logs", "store": "ops", "query": "module: probe and target: gateway | select count_if(ok='false') as fails, count_if(ok='true') as oks", "timeSpanType": "Relative", "start": "-5m", "end": "now", "powerSqlMode": "auto"}],
    "groupConfiguration": {"type": "no_group", "fields": []},
    "conditionConfiguration": {"condition": "fails >= 3 and oks = 0", "countCondition": ""},
    "severityConfigurations": [{"severity": 10, "evalCondition": {"condition": "", "countCondition": ""}}],
    "labels": [{"key": "rule", "value": "gateway-down"}],
    "annotations": [{"key": "title", "value": "网关不可用"}, {"key": "desc", "value": "探测连续失败 ${fails} 次;请在 https://windows-control.teenet.app 查看网关页"}],
    "autoAnnotation": true, "sendResolved": true, "threshold": 1, "noDataFire": true, "noDataSeverity": 8,
    "policyConfiguration": {"alertPolicyId": "sls.builtin.dynamic", "actionPolicyId": "wecom-ops-policy", "repeatInterval": "30m", "useDefault": false}
  },
  "schedule": {"type": "FixedRate", "interval": "1m"}
}
```
`noDataFire: true` 让"探测器自己停了"也告警(采集停摆的一半)。

`upstream-error-rate.json`:query 改为
`event_type: llm_call | select model_group, count(*) as n, round(count_if(status='failure')*100.0/count(*),1) as err_pct group by model_group having count(*) >= 10`,`start: -5m`,`groupConfiguration: {"type":"custom","fields":["model_group"]}`,`condition: err_pct > 10`,severity 8,标题 `上游错误率 ${err_pct}% (${model_group})`。

`budget-80.json`(每小时评估,自然月累计,按 event_id 去重):
`event_type: llm_call and status: success | select employee_id, round(sum(cost_usd),4) as spent, max(user_max_budget) as budget from (select distinct event_id, employee_id, cost_usd, user_max_budget from log) group by employee_id having max(user_max_budget) > 0 and sum(cost_usd) >= 0.8 * max(user_max_budget)`,时间范围 `start: "-30d"`(实施时改为"本月 1 日 00:00 UTC 到 now":SLS 相对时间不支持自然月,用 `"start": "begin of month"`?不支持则退回 `-30d` 并在标题里注明是滚动 30 天),`schedule interval: 1h`,`repeatInterval: 24h`,severity 6,标题 `预算预警 ${employee_id} 已用 $${spent} / $${budget}`。`user_max_budget` 为 null 的行不触发。

`collection-stalled.json`(Logtail 心跳:用 SLS 内置的机器组心跳规则模板 `sls_app_ilogtail_heartbeat` 之类;若模板不可用,则用 `ops` 上 `noDataFire` 规则:`module: console | select count(*) as n`,`-10m`,`n = 0` 即告警,标题 `控制台日志 10 分钟无数据`)。

- [ ] **Step 3: apply-alerts.sh**

```bash
#!/usr/bin/env bash
set -euo pipefail
PROFILE=${PROFILE:-sls-bootstrap}; PRODUCT=${PRODUCT:-sls}; REGION=ap-southeast-1; PROJECT=windows-control-logs
HERE=$(cd "$(dirname "$0")" && pwd)
A() { aliyun "$PRODUCT" "$@" --profile "$PROFILE" --region "$REGION" --force; }
for f in gateway-down upstream-error-rate budget-80 collection-stalled; do
  name=$(python3 -c "import json;print(json.load(open('$HERE/$f.json'))['name'])")
  A GetAlert --project "$PROJECT" --alertName "$name" >/dev/null 2>&1 \
    && A UpdateAlert --project "$PROJECT" --alertName "$name" --body "file://$HERE/$f.json" \
    || A CreateAlert --project "$PROJECT" --body "file://$HERE/$f.json"
done
```

- [ ] **Step 4: 逐条触发验证**(spec §7 的验证方式)

1. 网关不可用:`ssh root@47.236.115.50 'sed -i s#liveliness#nope# /opt/ai-env-mgr/ops/gateway-probe.sh'`,等 4 分钟,企业微信收到「网关不可用」;改回,等 2 分钟收到恢复。
2. 上游错误率:用主密钥连发 12 次 `model: no-such-model`(404 计失败)+ 0 次成功,5 分钟内收到 `上游错误率 100% (no-such-model)`。
3. 预算:建临时用户 `emp-budgetprobe`(`max_budget: 0.01`),用它的 key 打几次 glm-5 直到累计 ≥ 0.008,下一次整点评估收到预警;删用户。
4. 采集停摆:`ssh root@47.236.115.50 '/etc/init.d/ilogtaild stop'`,10 分钟后收到告警;`start` 后恢复,并确认停机期间的 probe 行随后全部补上(查 `module: probe` 计数 = 停机分钟数)。
每一条把收到的企业微信消息截图或文字记到 Task 8 的决策记录。

- [ ] **Step 5: 提交**

```bash
git grep -n "qyapi.weixin.qq.com/cgi-bin/webhook/send?key=" -- ops/ && { echo "webhook leaked"; exit 1; } || true
git add ops/sls/alerts && git commit -m "ops: SLS alert rules for gateway, upstream errors, budget and collection health" -m "Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: 故障验收矩阵(阶段一部分)

不写新代码;按 spec §9「故障验收矩阵」跑阶段一能覆盖的四行,结果写进 `docs/superpowers/specs/2026-09-09-unified-logging-design.md` 末尾新节「阶段一验收记录(2026-09-xx)」。

- [ ] **Step 1: SLS 中断与重投**

```bash
ssh ec2-user@175.41.186.104 'sudo iptables -I OUTPUT -p tcp -d ap-southeast-1.log.aliyuncs.com --dport 443 -j REJECT'
# 用主密钥打 20 次 glm-5,记录 llm_events.jsonl 新增行数 N 和它们的 event_id
ssh ec2-user@175.41.186.104 'sudo iptables -D OUTPUT -p tcp -d ap-southeast-1.log.aliyuncs.com --dport 443 -j REJECT'
sleep 120
# 查 audit:这 20 个 event_id 各出现且仅出现一次;sum(cost_usd) 与本地文件求和一致
```
必须证明:覆盖范围内可补回,重复不影响费用。

- [ ] **Step 2: 网关重启中断**

打一个长输出请求的同时 `docker compose restart litellm`;确认失败/取消请求在 `llm_events.jsonl` 里有 `status=failure` 行(或明确记录"进程内未完成请求不产生事件"作为已知丢失边界,写进验收记录)。

- [ ] **Step 3: 轮转**

临时把 `GatewayEventLogger(max_bytes=...)` 不动,改为在宿主机 `logs/` 手工 `mv llm_events.jsonl llm_events.jsonl.1`(模拟轮转)后再打 5 次请求;确认 SLS 里旧文件尾部和新文件开头的事件都在、不重复。

- [ ] **Step 4: 秘密扫描**

```bash
ssh ec2-user@175.41.186.104 'grep -c -E "sk-[A-Za-z0-9]{6,}|LTAI|Bearer " ~/gateway-litellm/logs/llm_events.jsonl'
ssh root@47.236.115.50 'grep -c -E "sk-[A-Za-z0-9]{6,}|LTAI|Bearer |Cookie" /var/log/ai-env-mgr/*.jsonl'
```
Expected: 全部 `0`。再在 SLS 上对两个 Logstore 各跑一次同样的关键字查询,Expected 0 条。

- [ ] **Step 5: 写验收记录并提交**

每行:场景、做法、结果、已知边界。提交信息 `docs: phase-1 unified logging acceptance record`。

---

### Task 8: 文档与旧 Agent 标注

**Files:**
- Modify: `go/internal/adminweb/assets/log.html`(机器日志页标题旁加"仅日志尾部(64KB),完整日志见 SLS")
- Modify: `gateway-litellm/README.md`(决策记录:2026-09-1x 事件回调、字段、路径、Logtail、回滚)
- Modify: `docs/架构说明.md`(运维一节加 SLS 项目、两个 Logstore、告警去向、reader/writer 身份职责;注意此文件有另一会话的未提交修改,只在末尾追加,提交时用 `git add -p` 只选自己的块)
- Modify: `/root/pp_home/windows-pc/架构_整体架构总览_2026-07.md`(rev.6:日志链路一句话 + 指向设计文档)

- [ ] **Step 1: log.html 标注**,找到页面标题所在的 `<h1>`/`<h2>`,其后加:
```html
<p class="dim">这里只显示 Agent 上传的日志尾部(约 64KB)。完整运行日志与调用记录在 SLS 项目 windows-control-logs(ops / audit),阶段二接入控制台查询。</p>
```
- [ ] **Step 2: README、架构说明、总览** 按上面写。
- [ ] **Step 3: `make check`,提交** `docs: record the phase-1 unified logging deployment and mark the agent log page as tail-only`。

---

## Self-review

- Spec 覆盖:§9 阶段一四项(锁定版本与样本→Task 2 Step 6 与 Task 7;ops/audit 与索引、受限身份→Task 1;网关与控制台统一事件→Task 2、3;告警→Task 6;旧 Agent 标注→Task 8;出口条件"成功/失败/取消可区分、底稿可核验、断连可补回、费用不重复"→Task 2 字段 + Task 7 Step 1、2)。"底稿可核验"的对账脚本(SpendLogs vs SLS)未单列任务:Task 7 Step 1 用本地文件求和代替,与 Postgres SpendLogs 的定期对账留给阶段二,已在 Task 7 记录为已知边界。
- 占位符:Task 5 的安装命令与 Logtail 处理器名、Task 6 的 `begin of month`、`StandardLoggingPayload` 键名三处标明"以实施当天官方文档/真实数据为准",不是 TBD,是明确的核对动作。
- 类型一致:`eventlog.New(dir, source)`、`Ops(level, eventType, msg, fields)`、`Audit(eventType, msg, fields)`、`Redact(map[string]any)` 在 Task 3 定义,Task 3 Step 6/7 使用一致;`Manager.Events` 字段名两处一致;文件名 `admin.jsonl` / `audit.jsonl` / `probe.jsonl` / `llm_events.jsonl` 在 Task 3、4、5 一致。
