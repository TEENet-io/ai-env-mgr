# 用量快照、告警、凭据轮换实施计划(计划 B)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让控制台开始"采新数据":每天把网关用量落库(超过日志保留期也能查、能对账),把已经能算出来的异常主动送到管理员手里(而不是等人来看页面),并让员工令牌与主密钥按期轮换。

**Architecture:** 三件事都搭在现有的任务队列 + 调度器上(`worker.Schedule` 加三种周期任务),数据落 PostgreSQL 新表(迁移 0009–0011),页面沿用 `adminweb` 的 html/template、无 JavaScript。用量来源是 SLS `ops` 日志库里网关写的 `llm_call` 事件(已去重的 `event_id`),按天、按员工、按模型聚合后 upsert;告警是"规则求值 → 开/关告警行 → 投递任务",规则只读库,投递走 webhook 与 SMTP,渠道配置存 `settings`,密码用主密钥密封;轮换是"到期就走一遍现有的 Reissue",加一条只对最近有同步的机器执行的保护,主密钥换钥做成 `ai-env-migrate rekey` 子命令。

**Tech Stack:** Go 1.26、PostgreSQL 15、pgx v5、html/template;现有 `internal/repo`、`internal/dbstore`、`internal/worker`、`internal/ops`、`internal/adminweb`、`internal/slsclient`、`internal/secrets`、`net/smtp`(标准库,不引第三方)。

**Spec:** 本文件"范围"与"关键决定"两节(用户 2026-09-21 确认);背景见 `docs/superpowers/plans/2026-09-20-console-insight.md`(计划 A)。

## Global Constraints

- 不改 agent,不改网关配置;所有新数据由控制台 Worker 采。
- 新周期任务都用 `enqueuePeriodic`(时间桶幂等),失败由下一桶接管;单次运行有上限(用量一次最多回填 3 天,告警求值一次最多开 200 条,轮换一天最多 N 人,N 可配、默认 5)。
- 秘密不落日志、不进代码:SMTP 密码与 webhook 签名密钥用 `secrets.Keyring` 密封后存 `settings`,页面永不回显,只显示"已设置"。
- 告警投递失败只影响投递,不影响规则求值;同一条告警在关闭前只通知一次(升级除外:预算 80% → 100% 算新告警)。
- 轮换只对"机器 24 小时内同步过"的员工执行,否则跳过并记原因;跳过不算失败。
- 模板不依赖 JavaScript;所有列表分页或限量;CSV 复用 `csvSafe` 与 BOM。
- 每个任务:先写失败测试(纯函数单测 + 一个 DB 模式端到端),再实现;`make check` 过;提交信息英文祈使句,尾注 `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`。
- 数据库测试:`TEST_PG_DSN='postgres://postgres:devpass@127.0.0.1:5433/aienv_test?sslmode=disable'`。

## 范围

| 项 | 内容 | 关键决定 |
|---|---|---|
| B1 用量快照 | 每天从 SLS 聚合前一天(并重算前两天)每员工每模型的调用数、失败数、费用、token,落 `usage_daily`;`/usage` 页按月看员工/模型/日趋势,CSV;员工详情加"最近 30 天用量" | 来源是 `ops` 日志库 `event_type: llm_call`,按 `event_id` 去重;别名 → 员工靠 `gateway_grants.key_alias`;不去网关拉 `/spend/logs` |
| B2 告警 | 规则:机器离线、发布目标失败、网关授权漂移、预算 80%/100%、任务最终失败;`alerts` 表记开/关;渠道:webhook(通用 JSON / 钉钉 / 飞书)与 SMTP;`/alerts` 页看、确认、测试发送;导航显示未关闭数 | 每 10 分钟求值;开着的告警只通知一次;条件消失自动关闭;漂移与任务失败由事件直接开告警 |
| B3 凭据轮换 | 员工令牌到期(默认 90 天)自动 Reissue,每天限量,仅对活跃机器;员工列表显示令牌年龄;主密钥换钥命令 `ai-env-migrate rekey` 重新密封所有活跃密文 | 复用 `ops.Reissue`;网关管理密钥、OSS AK 的轮换只写运维手册,不写代码(等 RAM 实例角色) |

## 关键决定(请确认)

1. **用量的真相来源是 SLS 日志,不是网关的 spend 字段。** 网关的 `user.spend` 是月累计、不分模型、月初归零,只够画预算条;日志有 `event_id`、模型、费用、token、状态,能分模型、能对账。代价是依赖 SLS 可达;SLS 不可达时当天不写,下一天回填(回填窗口 3 天)。
2. **告警先做五条规则,阈值放在 `settings.alerts`,不做规则编辑器。** 五条:绑定机器超过 24 小时未上报;发布目标失败;网关授权漂移;员工预算达 80% / 100%;任务最终失败(超过最大重试)。网关探测失败暂不做(总览已有探测条,且探测本身就是靠 SLS)。
3. **渠道两种:webhook 与 SMTP。** webhook 支持三种格式:通用(`{"title","severity","subject","detail","url"}`)、钉钉自定义机器人(含 secret 签名)、飞书自定义机器人;SMTP 用标准库 `net/smtp`,STARTTLS,收件人列表。测试发送按钮各一个。
4. **令牌轮换 = 定时 Reissue。** 不引入新的凭据机制;员工无感(agent 下一次同步取到新 credentials.zip)。默认 90 天,每天最多 5 人,机器 24 小时内没同步的跳过。
5. **主密钥换钥是离线命令,不是页面。** 步骤:在 `master.key` 里加新钥并设为 current → 重启控制台 → `ai-env-migrate rekey` 把所有活跃 `credential_versions` 与管理员 TOTP 种子用 current 重新密封 → 确认没有行再引用旧钥后 `rekey --prune-unused` 从文件里删旧钥。

---

### Task B1a: usage_daily 表与聚合查询

**Files:**
- Create: `go/db/migrations/0009_usage_daily.up.sql` / `.down.sql`
- Modify: `go/internal/repo/repo.go`(`Usage` 接口、`Store.Usage()`)、`go/internal/dbstore/store.go`
- Create: `go/internal/dbstore/usage.go`、`usage_test.go`
- Modify: `go/db/grants.sql`(`usage_daily` 给 `aienv_app` select/insert/update)

**Interfaces:**
```sql
create table usage_daily (
  day          date        not null,
  employee_id  uuid        null,            -- 解析不到员工时为 null,alias 留在 key_alias
  key_alias    text        not null,
  model_group  text        not null,        -- 网关的 model_group,'' 表示未知
  calls        integer     not null default 0,
  failures     integer     not null default 0,
  cost_usd     numeric(12,6) not null default 0,
  unpriced     integer     not null default 0,   -- cost_state=unknown 的调用数
  prompt_tokens     bigint not null default 0,
  completion_tokens bigint not null default 0,
  updated_at   timestamptz not null default now(),
  primary key (day, key_alias, model_group)
);
create index usage_daily_by_employee on usage_daily (employee_id, day desc);
```
```go
// repo
type UsageRow struct {
	Day                  time.Time
	EmployeeID, KeyAlias string
	ModelGroup           string
	Calls, Failures      int
	CostUSD              string // numeric as text
	Unpriced             int
	PromptTokens         int64
	CompletionTokens     int64
}
type UsageFilter struct {
	From, To   time.Time // 含 From,不含 To(按日)
	EmployeeID string    // "" = 全部
}
type Usage interface {
	// UpsertDay replaces one day's rows: delete day then insert, in the caller's tx.
	UpsertDay(ctx context.Context, day time.Time, rows []UsageRow) error
	ByEmployee(ctx context.Context, f UsageFilter) ([]UsageRow, error)   // 每员工汇总(跨模型)
	ByModel(ctx context.Context, f UsageFilter) ([]UsageRow, error)      // 每模型汇总
	ByDay(ctx context.Context, f UsageFilter) ([]UsageRow, error)        // 每天汇总
	Rows(ctx context.Context, f UsageFilter) ([]UsageRow, error)         // 明细,给 CSV,上限 50000
	Days(ctx context.Context) (first, last time.Time, err error)         // 有数据的范围,给月份选择
}
```

- [x] 测试 `TestUsageUpsertReplacesTheDayAndSums`(dbstore):写 9/1 三行、9/2 两行;`UpsertDay(9/1, 新两行)` 后 9/1 只剩两行;`ByEmployee` 跨模型求和、`ByModel` 跨员工求和、`ByDay` 按天排序;`Rows` 上限生效。
- [x] 迁移 + `grants.sql` + `dbstore/usage.go`;`make check`。
- [x] 提交:`dbstore: usage_daily table and aggregate queries`

### Task B1b: 用量快照任务

**Files:**
- Create: `go/internal/worker/usage.go`、`usage_test.go`
- Modify: `go/internal/worker/schedule.go`(`TaskUsageSnapshot = "usage_snapshot"`,`EveryUsageSnapshot = 24 * time.Hour`;桶时间对齐到 UTC 00:30 之后:`NotBefore = bucket.Add(30*time.Minute)`)
- Modify: `go/internal/adminweb/dbmode.go`(有 `st.sls` 才注册)
- Modify: `go/internal/slsclient/slsclient.go`(若 `GetLogs` 的 SQL 结果列名不能直接取,加 `Result.Column(name)` 小助手)

**Interfaces:**
```go
type UsageSnapshot struct {
	Store    repo.Store
	SLS      UsageSource   // 接口,测试用假实现
	Logstore string        // "ops"
	Backfill int           // 回填天数,默认 3
	Now      func() time.Time
}
type UsageSource interface {
	// DailyUsage 返回 day(UTC 日)内按 employee_id, model_group 聚合的行。
	DailyUsage(ctx context.Context, logstore string, day time.Time) ([]repo.UsageRow, error)
}
// 给页面按钮用,与 EnqueueReleaseScan 同一模式(当前小时的桶,立即可跑)。
func EnqueueUsageSnapshot(ctx context.Context, store repo.Store, at time.Time) error
```
- SLS 查询(一条,按 `event_id` 去重后聚合):
  ```
  event_type: llm_call | select employee_id, model_group,
    count(*) as calls, count_if(status='failure') as failures,
    round(sum(try_cast(cost_usd as double)),6) as cost_usd,
    count_if(cost_state='unknown') as unpriced,
    sum(try_cast(prompt_tokens as bigint)) as prompt_tokens,
    sum(try_cast(completion_tokens as bigint)) as completion_tokens
  from (select distinct event_id, employee_id, model_group, status, cost_usd, cost_state, prompt_tokens, completion_tokens from log)
  group by employee_id, model_group limit 10000
  ```
  时间窗 = `day 00:00 UTC` 到 `day+1 00:00 UTC`,按 `occurred_at` 优先、`__time__` 兜底(与 `logs.go` 的 `displayTime` 同一取舍;第一版直接用 `__time__`,注释写明误差)。
- 别名解析:先 `Grants().ByKeyAlias`(命中就得员工);没命中且形如 `emp-<user>` 则 `Employees().ByWindowsUser`(兼容旧别名与在册的旧账号);都没有则 `employee_id = null`。
- Run:对 `today-1 … today-Backfill` 每一天各查一次并 `UpsertDay`;任一天 SLS 出错则记 note 并继续下一天;全部失败才返回错误(触发重试)。
- [x] 测试 `TestUsageSnapshotFillsThreeDaysAndResolvesAliases`(worker,DB):假 `UsageSource` 给三天数据(含一个新别名、一个旧别名、一个未知别名);跑一次,`usage_daily` 三天都有,员工 id 解析正确,未知别名 `employee_id` 为 null;第二天再跑,昨天的数覆盖不叠加。
- [x] 测试 `TestUsageSnapshotSkipsADayTheLogServiceCannotAnswer`:一天出错,其余两天照写,任务成功且 note 含 "1 day skipped"。
- [x] 实现 + 调度 + 注册;`make check`。
- [x] 提交:`worker: nightly usage snapshot from the gateway logs`

### Task B1c: 用量页与员工用量

**Files:**
- Create: `go/internal/adminweb/usage.go`、`usage_test.go`、`assets/usage.html`
- Modify: `server.go`(`/usage`、`/usage.csv`、`/usage/snapshot-now` POST)、`handlers.go`(`pageData.UsagePage`)、`assets/_layout.html`(导航 10 用量)、`roles.go`(`/usage*` viewer 可看,`/usage/snapshot-now` operator)、`assets/user.html`(最近 30 天一节)、`accounts.go`(详情页装配)

**Interfaces:**
```go
type usagePage struct {
	Month      string        // "2026-09"
	Months     []string      // 有数据的月份,新→旧
	ByEmployee []usageLine   // 用户名、部门、调用、失败、费用、未计价、token
	ByModel    []usageLine
	ByDay      []usageLine   // 日期、调用、费用;柱用 .dist-bar
	Total      usageLine
	LastRun    string        // 最近一次快照任务时间与结果
}
type usageLine struct {
	Label, Sub       string // 员工名/模型名/日期;Sub 是部门或空
	Calls, Failures  int
	CostUSD          string // 保留 4 位
	Unpriced         int
	Tokens           int64  // prompt + completion
	Pct              int    // 占合计费用的百分比,给 .dist-bar
	Link             string
}
```
- 查询参数:`month=2026-09`(默认当月);`employee=<user>` 只看一人。
- `/usage.csv?month=` 导出明细行(日、员工、模型、调用、失败、费用、未计价、token),`csvSafe`。
- "立即快照"按钮:`EnqueueUsageSnapshot`(和"立即扫描 OSS"同一模式)。
- 员工详情页:最近 30 天按天一表 + 合计,链接到 `/usage?employee=`。
- [x] 测试 `TestUsagePageSumsAndExports`(adminweb,DB):预先写三天两员工两模型的行;`/usage?month=` 员工表两行、模型表两行、合计对得上;`/usage?employee=work1` 只见一人;CSV 行数正确、有 BOM;`viewer` 看得到页,点不了"立即快照"(403)。
- [x] 实现;`make check`。
- [x] 提交:`adminweb: monthly usage page with CSV and per-employee history`

### Task B2a: alerts 表、规则与求值任务

**Files:**
- Create: `go/db/migrations/0010_alerts.up.sql` / `.down.sql`
- Modify: `repo.go`(`Alert`、`Alerts` 接口、`SettingAlerts = "alerts"`)、`dbstore/store.go`;Create `dbstore/alerts.go`、`alerts_test.go`;`grants.sql`
- Create: `go/internal/worker/alerts.go`(求值)、`alerts_test.go`
- Modify: `schedule.go`(`TaskAlertEval = "alert_eval"`,`EveryAlertEval = 10 * time.Minute`)、`dbmode.go`(注册;`OnDrift` 与 Worker 的"最终失败"钩子改为开告警)

**Interfaces:**
```sql
create table alerts (
  id           uuid primary key default gen_random_uuid(),
  kind         text not null,           -- machine_offline | rollout_failed | gateway_drift | budget | task_failed
  fingerprint  text not null,           -- kind + subject (+ 等级),开着时唯一
  severity     text not null,           -- warn | crit
  subject_type text not null,           -- device | employee | grant | task | target
  subject_id   text not null,
  title        text not null,
  detail       text not null default '',
  opened_at    timestamptz not null default now(),
  resolved_at  timestamptz null,
  resolved_by  text not null default '', -- 'rule' 自动关闭 / 管理员用户名
  acked_at     timestamptz null,
  acked_by     text not null default '',
  notified_at  timestamptz null,
  notify_error text not null default ''
);
create unique index alerts_one_open on alerts (fingerprint) where resolved_at is null;
create index alerts_open on alerts (opened_at desc) where resolved_at is null;
```
```go
type Alerts interface {
	Open(ctx context.Context, a NewAlert) (Alert, bool, error)     // 已有同 fingerprint 开着 → 返回它,false
	Resolve(ctx context.Context, fingerprint, by string) (int, error)
	ResolveByID(ctx context.Context, id, by string) (Alert, error)
	Ack(ctx context.Context, id, by string) (Alert, error)
	ListOpen(ctx context.Context) ([]Alert, error)
	ListRecent(ctx context.Context, limit int) ([]Alert, error)     // 含已关闭,新→旧
	Unnotified(ctx context.Context, limit int) ([]Alert, error)
	MarkNotified(ctx context.Context, id string, at time.Time, errText string) error
	CountOpen(ctx context.Context) (int, error)
}
// settings.alerts(JSON)
type AlertSettings struct {
	OfflineAfterHours int  `json:"offline_after_hours"` // 默认 24
	BudgetWarnPercent int  `json:"budget_warn_percent"` // 默认 80
	Enabled           bool `json:"enabled"`             // 默认 true
}
// worker
type UserLister interface {
	ListUsers(ctx context.Context) ([]litellm.User, error) // *litellm.Client 已实现
}
type AlertEval struct {
	Store    repo.Store
	Gateway  UserLister        // 给预算规则;nil 则跳过该规则
	Settings repo.AlertSettings
	Now      func() time.Time
}
```
- 规则(每条都是"算出应开集合 S,开 S 里没开的,关开着但不在 S 里的",fingerprint 固定格式):
  - `machine_offline`:绑定了员工的设备,`LastSeenAt` 早于 now − OfflineAfterHours;fingerprint `machine_offline:<device_id>`;detail 含上次上报时间。
  - `rollout_failed`:`release_targets` 里 `status=failed` 且是该设备该产品最新一代;fingerprint `rollout_failed:<target_id>`;目标被重试/排除/成功后关闭。
  - `budget`:网关 `ListUsers` 的 `spend/max_budget`,≥ warn% 开 warn,≥ 100% 开 crit;fingerprint `budget:<employee_id>:warn|crit`;月初 spend 归零后关闭。
  - `gateway_drift`:不由求值产生,由 `Reconcile.OnDrift` 直接 `Open`,fingerprint `gateway_drift:<key_alias>`;下一次 Reconcile 观测一致时关闭(求值任务查 `Grants().NeedsReconcile` 之外的"最近一致"即可:简单做法是求值时遍历开着的 drift 告警,对应 grant `Actual == Desired` 就关)。
  - `task_failed`:Worker 在任务用尽重试时(`worker.go` 里已有最终失败分支)`Open`,fingerprint `task_failed:<task_id>`;任务被 `Enqueue` 复活(状态回 pending)时关闭(在 `Enqueue` 的 revive 分支之后由 ops 关,或求值时查任务状态)。
- [x] 测试 `TestAlertsOpenOncePerFingerprintAndResolve`(dbstore)。
- [x] 测试 `TestAlertEvalOpensAndClosesByRule`(worker,DB):一台绑定机器 30 小时没上报 → 开;上报后再跑 → 关;一个失败目标 → 开,重试后 → 关;假网关用户 spend 85/100 → warn,105 → crit 且 warn 关闭;设置 `enabled=false` → 什么都不开。
- [x] 实现 + 调度 + `OnDrift`/最终失败钩子;`make check`。
- [x] 提交:`worker: alert rules with open/resolve state`

### Task B2b: 渠道设置与投递

**Files:**
- Create: `go/internal/notify/notify.go`(`Channel` 接口、`Webhook{Format}`、`SMTP`)、`notify_test.go`(用 `httptest` 与本地 SMTP 假服务器)
- Modify: `repo.go`(`SettingAlertChannels = "alert_channels"`)
- Create: `go/internal/worker/notify.go`(`AlertNotify` 任务:取 `Unnotified`,逐条投递,`MarkNotified`)、`notify_test.go`
- Modify: `schedule.go`(`TaskAlertNotify`,每分钟)、`dbmode.go`(注册;渠道从 settings 读、密封字段用 `st.ring` 解开)

**Interfaces:**
```go
// settings.alert_channels(JSON;密封字段是 base64 的密文 + key_version)
type ChannelSettings struct {
	Webhook struct {
		URL     string `json:"url"`
		Format  string `json:"format"`   // generic | dingtalk | feishu
		Secret  Sealed `json:"secret"`   // 钉钉加签;可空
		Enabled bool   `json:"enabled"`
	} `json:"webhook"`
	SMTP struct {
		Host, From string
		Port       int
		Username   string
		Password   Sealed
		To         []string
		Enabled    bool
	} `json:"smtp"`
}
type Sealed struct{ Ciphertext []byte; KeyVersion string }

type Message struct{ Title, Severity, Subject, Detail, URL string; At time.Time }
type Channel interface{ Send(ctx context.Context, m Message) error }
```
- 投递:每条告警对所有启用的渠道各发一次;全部成功才 `MarkNotified(at, "")`,否则记最后错误并下一分钟再试,最多重试 10 次(告警行上 `notify_error` 可见)。
- 钉钉签名:`timestamp\nsecret` 的 HMAC-SHA256 → base64 → URL 参数(官方算法);飞书:`{"msg_type":"text","content":{"text":...}}`。
- SMTP:`net/smtp` + STARTTLS,主题 `[windows-control] <severity> <title>`,正文纯文本含链接。
- [x] 测试 `TestWebhookFormats`(三种格式的请求体与签名)、`TestSMTPSendsOneMailPerAlert`(本地假 SMTP)。
- [x] 测试 `TestAlertNotifyDeliversOnceAndRecordsFailures`(worker,DB)。
- [x] 实现;`make check`。
- [x] 提交:`notify: webhook and SMTP channels; worker delivers open alerts once`

### Task B2c: 告警页与设置

**Files:**
- Create: `go/internal/adminweb/alerts.go`、`alerts_test.go`、`assets/alerts.html`
- Modify: `server.go`(`/alerts`、`/alerts/ack`、`/alerts/resolve`、`/alerts/test` POST、`/settings/alerts`、`/settings/alert-channels` POST)、`assets/settings.html`(告警阈值与渠道两节;密码框留空=不改)、`assets/_layout.html`(导航"告警"后跟未关闭数 `tag-crit`)、`handlers.go`(`pageData.OpenAlerts int` 每页都带)、`roles.go`(看 viewer;确认/关闭 operator;设置 admin)

- 页面:上半"未关闭"(严重度、标题、对象链接到机器/员工/发布页、开启时长、通知状态、确认/关闭按钮),下半"最近 200 条历史"。
- "发送测试"按钮各渠道一个:直接调 `Channel.Send` 一条固定消息,结果作 notice 显示,不入库。
- [x] 测试 `TestAlertsPageListsAcksAndResolves`(adminweb,DB):预置两条开着的、一条关闭的;页面分区正确;`ack` 后显示确认人;`resolve` 后进历史;导航数字从 2 变 1;`viewer` 点确认得 403。
- [x] 测试 `TestAlertSettingsKeepThePasswordWhenLeftBlank`:保存渠道时密码框留空,原密文保持不变;填了才重新密封。
- [x] 实现;`make check`。
- [x] 提交:`adminweb: alerts page, thresholds and channel settings`

### Task B3a: 令牌轮换任务与列表提示

**Files:**
- Modify: `repo.go`(`SettingRotation = "rotation"`;`Credentials` 接口加 `LiveOlderThan(ctx, purpose, before time.Time, limit) ([]Credential, error)`)、`dbstore/credentials.go`、测试
- Create: `go/internal/worker/rotation.go`、`rotation_test.go`
- Modify: `schedule.go`(`TaskCredentialRotation`,每天,`NotBefore = bucket.Add(1*time.Hour)`)、`dbmode.go`(注册)、`ops/ops.go`(`ActionRotate = "account.rotate"`;新方法 `Rotate(ctx, employeeID, actor, requestID string) (repo.Employee, error)`,与 `Reissue` 共用同一事务体,只是审计 action 不同)
- Modify: `adminweb/accounts.go` + `assets/users.html`(列表加"令牌"列:签发日期 + 年龄;超过 `max_age_days` 标 `tag-warn`"待轮换")、`assets/settings.html`(轮换一节:最大年龄、每日上限、开关)

**Interfaces:**
```go
type RotationSettings struct {
	Enabled       bool `json:"enabled"`         // 默认 false(上线后由管理员打开)
	MaxAgeDays    int  `json:"max_age_days"`    // 默认 90
	PerDay        int  `json:"per_day"`         // 默认 5
	ActiveWithinH int  `json:"active_within_h"` // 默认 24:机器多久内同步过才轮换
}
type CredentialRotation struct {
	Store repo.Store
	Ops   *ops.Service
	Now   func() time.Time
}
```
- Run:读设置;`LiveOlderThan(codex_gateway, now − MaxAge, PerDay*3)`;对每个员工:必须 active、有绑定机器且该机器 `LastSeenAt` 在 ActiveWithinH 内,否则记"skipped: <原因>";满足则 `ops.Rotate(employeeID, "rotation", requestID)`,计数到 PerDay 为止;note 形如 `rotated 3, skipped 2 (machine silent), 4 more due`。
- [x] 测试 `TestRotationReissuesOldTokensOnActiveMachinesOnly`(worker,DB):三个员工凭据都 100 天前(直接改 `issued_at`),一个机器活跃、一个机器 3 天没上报、一个没绑机器;跑一次:只有第一个 epoch +1 且有 provision 任务、审计 `account.rotate`;`PerDay=1` 时第二个活跃员工留到明天;`enabled=false` 什么都不做。
- [x] 实现;`make check`。
- [x] 提交:`worker: rotate employee tokens past their maximum age`

### Task B3b: 主密钥换钥命令

**Files:**
- Modify: `go/cmd/migrate/main.go`(`rekey` 子命令)
- Create: `go/internal/secrets/rekey.go`、`rekey_test.go`(`Reseal(ctx, ring, blob, oldVersion, aad) (newBlob, newVersion, error)`)
- Modify: `repo.go`(`Credentials.ListByKeyVersion(ctx, notVersion string, limit) ([]Credential, error)`、`Credentials.Reseal(ctx, id string, ciphertext []byte, keyVersion string) error`;`Admins.ListTOTPByKeyVersion` / `Admins.ResealTOTP` 同理)、`dbstore` 实现、测试
- Modify: `docs/`(运维手册一节:换钥步骤;网关管理密钥与 OSS AK 的手工轮换步骤)

**Interfaces:**
```
ai-env-migrate rekey            # 用 master.key 的 current 重新密封所有不是 current 的活跃密文,打印各表计数
ai-env-migrate rekey --dry-run  # 只数不改
ai-env-migrate rekey --prune-unused   # 库里没有任何行引用的旧钥从 master.key 删除(先备份为 master.key.bak,0600)
```
- 只处理活跃行(`credential_versions.retired_at is null`、未禁用管理员);退役行留旧钥版本不动,旧钥在 prune 前都在,所以还能打开。
- 每 100 行一个事务;中断可重跑(幂等:已是 current 的跳过)。
- [x] 测试 `TestRekeyResealsEverythingLiveAndLeavesRetiredRows`(dbstore + secrets):两把钥,k1 密封三条(一活跃凭据、一退役凭据、一管理员 TOTP);切 current=k2 跑 rekey:活跃凭据与 TOTP 变 k2 且能用 k2 打开,退役行仍 k1;`--prune-unused` 因退役行引用 k1 而拒绝删 k1(打印原因)。
- [x] 实现;`make check`。
- [x] 提交:`migrate: rekey command reseals live secrets under the current master key`

## 验收

- 用量:上线次日 `/usage` 能看到前一天各员工费用,且与 `/log` 页同日汇总一致(允许 SLS 去重差异 ≤ 1%);SLS 停一天再恢复,回填窗口内自动补齐。
- 告警:把一台测试机 agent 停 24 小时以上收到一条离线告警(webhook 或邮件),机器恢复后告警自动关闭且不重复通知;发布一个必失败的目标收到一条,重试后关闭。
- 轮换:把一个测试员工的 `issued_at` 改到 100 天前、开启轮换,次日自动重发且机器上的 Codex 无需人工干预继续可用。
- 换钥:在测试库上跑 `rekey`,登录(TOTP)与凭据导出都正常。

## 实施顺序

B1a → B1b → B1c → B2a → B2b → B2c → B3a → B3b。B1 与 B2 相互独立,可并行;B3a 依赖 B2 不多,只共用 settings 页面。

## 实施记录(2026-09-21)

全部八个任务当天完成并上线(提交 d17b319 … e31961e,`main` 与 `rds-phase0`)。与计划的差异:

- 用量来源的日志库是 `audit`(网关的 `llm_call` 事件和日志页读的是同一个库),不是计划里写的 `ops`;日志里没有别名的调用保留在占位别名 `-` 下,不丢。
- 用量快照、轮换分别在桶时间后 30 分钟、1 小时才跑,`Schedule` 为此加了 `enqueuePeriodicAfter`;调度器停止时不再把被打断的入队当错误上报。
- 告警的"任务放弃"由 Worker 的 `OnEvent`(outcome=failed)开告警,"网关漂移"由 `Reconcile.OnDrift` 开;求值任务只负责关闭这两类。
- 轮换跳过"新令牌尚未完成开通"的人(凭据 epoch 落后于员工 epoch),避免一晚上连转两次;超出每日上限的只数"本来可以转的"。
- `rekey` 除凭据和管理员种子外,也重新密封告警渠道里的密码与签名密钥;`-prune-unused` 在退役凭据仍引用旧钥时保留旧钥并说明。
- 生产验证:首个快照 2026-09-21 00:30 UTC 跑通(9/18 六行、9/19 一行);告警规则首轮开出一条真实的"机器 70 小时未上报";`rekey -dry-run` 在生产库上报告 0 行待移(当前只有 k1)。
