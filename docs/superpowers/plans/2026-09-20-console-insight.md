# 控制台"看见数据"实施计划(计划 A)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把库里已有的数据(审计、绑定历史、发布目标、机器上报)做成能筛、能看、能导出的页面,并给管理员分权。不采新数据、不加 JavaScript、不引依赖。

**Architecture:** 全部是读路径:`repo` 加筛选查询,`adminweb` 加页面和纯函数聚合,模板沿用现有 `panel/table/tag` 样式。唯一的写路径是角色判定:一张"路由 → 最低角色"表,在 `requireSession` 之后统一判。CSV 导出流式写响应,不缓存。

**Tech Stack:** Go 1.26、PostgreSQL 15、pgx v5、html/template;现有 `internal/repo`、`internal/dbstore`、`internal/adminweb`。

**Spec:** 本文件"范围"一节(用户 2026-09-20 确认);背景见 `docs/superpowers/plans/2026-09-18-release-pipeline.md`。

## Global Constraints

- 只读页面不写库、不排任务;角色判定拒绝时返回 403 页,不做重定向到登录。
- 所有列表分页或限量(默认 200,CSV 上限 50000 行),不做无界查询。
- 时间一律按浏览器所在地展示:模板用 `.Local`;筛选参数按日期字符串 `2026-09-01`。
- 模板不依赖 JavaScript;图用表格 + 现有 `.bar` 分段条。
- 每个任务:先写失败测试(纯函数单测 + 一个 DB 模式端到端),再实现;`make check` 过;提交信息英文祈使句,尾注 `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`。
- 数据库测试:`TEST_PG_DSN='postgres://postgres:devpass@127.0.0.1:5433/aienv_test?sslmode=disable'`。

## 范围

| 项 | 内容 | 关键决定 |
|---|---|---|
| A1 全局审计页 | `/audit`:按目标类型/目标/操作/管理员/日期筛选,分页,CSV | 只读 `audit_events`;CSV 流式 |
| A2 机器时间线 | `/machines/detail?machine=`:绑定历史、发布目标与结果、相关审计、最近上报 | 不加表 |
| A3 员工履历 | 员工详情"履历"一节:本人审计 + 绑定过的机器 | 复用 A1 查询 |
| A4 发布报表 | `/rollouts` 顶部汇总:每版本成功台数、下发→成功中位时长、失败原因 Top、总在延后的机器 | 从 `release_targets` + `device_reported_state` 算 |
| A5 机队看板 | 总览顶部:agent/Codex 版本分布、未上报时长分布、AppLocker 模式分布 | `device_reported_state` 聚合 |
| A6 管理员角色 | `admin` 全部;`operator` 账号与发布,不管管理员;`viewer` 只读 | 路由表 + 中间件;现有账号保持 admin |

---

### Task A1: 全局审计页与 CSV

**Files:**
- Modify: `go/internal/repo/repo.go`(`Audit` 接口加 `Search`)、`go/internal/dbstore/audit.go`、`go/internal/dbstore/audit_test.go`
- Create: `go/internal/adminweb/audit.go`、`go/internal/adminweb/audit_test.go`、`go/internal/adminweb/assets/audit.html`
- Modify: `go/internal/adminweb/server.go`(路由 `/audit`、`/audit.csv`)、`handlers.go`(`pageData` 加 `AuditPage`)、`assets/_layout.html`(导航 9 审计)

**Interfaces:**
```go
// repo
type AuditFilter struct {
	TargetType, TargetID, Action, ActorID string
	From, To                              time.Time // zero = open
	Offset, Limit                         int
}
Search(ctx context.Context, f AuditFilter) (events []AuditEvent, total int, err error)
```
- 页面查询参数:`target_type`、`target`、`action`、`actor`、`from`、`to`、`page`;每页 100。
- `/audit.csv` 同样参数,列:`occurred_at,actor_type,actor,action,target_type,target,result,request_id,before,after`;`Content-Disposition: attachment; filename="audit-<from>-<to>.csv"`;上限 50000 行,超出在最后一行写 `# truncated`。
- 目标名称解析:`target_type=employee` 的 `target_id` 是 UUID,页面显示 Windows 用户名(一次性把员工表读成 map),device 同理显示主机名。

- [x] 测试 `TestAuditSearchFiltersAndPages`(dbstore):写 5 条不同 actor/action/时间的事件,按 actor、action、时间段、分页各查一次,核对 total 与顺序(新→旧)。
- [x] 实现 `Search`:动态 where(`$1 = '' or actor_id = $1` 这种形式,避免拼 SQL),`count(*) over()` 取 total。
- [x] 测试 `TestAuditPageFiltersAndExportsCSV`(adminweb,DB 模式):开户两个人、改一次额度,`/audit?action=account.quota` 只列一条;`/audit.csv?actor=<admin>` 返回 `text/csv`、含表头和 3 行;`target` 列显示用户名不显示 UUID。
- [x] 实现页面:筛选表单在顶部(一行),结果表(时间、管理员、操作、对象、变化摘要、请求 ID),分页链接保留筛选参数;"变化摘要"= before/after 里不同的键,`k: a → b`,复用 `describeJSONDiff` 的思路(放到 adminweb 一个纯函数 `summariseChange(before, after []byte) string`,单测)。
- [x] 提交:`adminweb: a searchable audit page with CSV export`

### Task A2: 机器时间线

**Files:**
- Create: `go/internal/adminweb/machine.go`、`machine_test.go`、`assets/machine.html`
- Modify: `server.go`(`/machines/detail`)、`assets/overview.html`(主机名变链接)、`handlers.go`(`pageData.MachinePage`)

**Interfaces:**
```go
type machinePage struct {
	Device   repo.Device
	Report   *model.Status; LastSync *time.Time
	Bindings []bindingRow   // 员工用户名、绑定/解绑时间、谁操作、备注
	Targets  []targetRow    // 复用 rollouts.go 的 targetRow,加版本与产品
	Events   []auditRow     // 复用 A1 的行类型,target=device
	SyncGaps []gapRow       // 最近 30 天里超过 2 倍间隔的静默段(从 audit 无法得,只有 last_sync;用 device_reported_state 的 last_sync_at 单点 + 任务 status_import 时间做不到 → 第一期只显示"最后上报"和"最长静默"= now-last_sync,列在 Report 上;SyncGaps 不做)
}
```
- 数据来源:`Devices().ByHostname`、`Reports().Get`、`Bindings().History`、`Releases()` 加 `TargetsByDevice(ctx, deviceID) ([]Target, error)`、`Audit().Search(target_type=device)`;绑定行里的员工名从 `Employees().ByID`(含已删除:ByID 不过滤)。
- [x] 测试 `TestMachinePageTellsTheMachinesStory`:一台机器绑 A 解绑绑 B、一次发布成功一次失败、一次"立即同步",页面按时间倒序都能看到,且已删除员工的名字仍显示。
- [x] 实现 + 总览主机名链接 + 导航不加项(从总览进)。
- [x] 提交:`adminweb: one page per machine: bindings, targets, audit`

### Task A3: 员工履历

**Files:**
- Modify: `go/internal/adminweb/accounts.go`(`handleUserDetail` 加 `Machines` 历史)、`assets/user.html`(履历一节)、`dbbackend.go`(`Audit` 改走 `Search`,带管理员与变化摘要)

- [x] 测试 `TestUserDetailShowsTheEmployeesHistory`:开户、改模型、绑机器、解绑、下线 → 详情页履历按时间倒序含这五条和机器名。
- [x] 实现:`bindingHistoryByEmployee` 需要 `Bindings().HistoryByEmployee(ctx, employeeID)`(repo + dbstore + 测试)。
- [x] 提交:`adminweb: an employee's full history on the detail page`

### Task A4: 发布报表

**Files:**
- Modify: `go/internal/adminweb/rollouts.go`、`rollouts_test.go`、`assets/rollouts.html`
- Modify: `go/internal/repo/releases.go`、`dbstore/releases.go`(`AllTargets(ctx, since time.Time)`)

**Interfaces(纯函数,单测):**
```go
type versionReport struct { Product, Version string; Succeeded, Failed, Pending, Excluded int; MedianToSuccess time.Duration; TopFailures []countRow }
type countRow struct { Label string; N int }
func reportByVersion(targets []repo.Target, artifacts map[string]repo.Artifact) []versionReport
func alwaysDeferred(targets []repo.Target, reports map[string]*model.Status, now time.Time) []deferredRow // pending 超过 24h 且上报 deferred 的机器,带原因
```
- [x] 测试:三个目标(成功 10 分钟、成功 30 分钟、失败"codex: install: exit 2")→ 中位 20 分钟、失败 Top 1 条;延后 36 小时的机器进列表。
- [x] 实现 + `/rollouts` 顶部"版本报表"表格(默认最近 90 天)。
- [x] 提交:`adminweb: release report by version`

### Task A5: 机队看板

**Files:**
- Modify: `go/internal/adminweb/fleet.go`、`fleet_test.go`(若无则新建)、`assets/overview.html`、`handlers.go`

**Interfaces:**
```go
type distribution struct { Title string; Rows []countRow; Total int }
func fleetDistributions(machines []admincore.MachineState, now time.Time) []distribution // agent 版本、Codex 版本、AppLocker 模式、未上报时长(<15m, <1h, <1d, <7d, ≥7d, 从未)
```
- [x] 测试:五台机器给定版本与最后上报 → 四个分布的行与计数。
- [x] 实现:总览"机队状态"条下面一排四个小面板,每行 `标签 · 数量 · 分段条(宽度=百分比,复用 .bar/.w{n})`。
- [x] 提交:`adminweb: fleet distributions on the overview`

### Task A6: 管理员角色

**Files:**
- Create: `go/internal/adminweb/roles.go`、`roles_test.go`
- Modify: `server.go`(每条路由注册时给最低角色)、`session.go`(`requireSession` 之后判)、`assets/admins.html`(角色下拉三项)、`dbmode.go`(创建管理员时校验角色)、`assets/_layout.html`(viewer 隐藏只写入口的按钮不做,按钮照显示,POST 被拒返回 403 页说明)

**Interfaces:**
```go
const (roleViewer = repo.RoleViewer; roleOperator = repo.RoleOperator; roleAdmin = repo.RoleAdmin)
// minRole 是路由前缀 → 最低角色;找不到的默认 admin(宁严勿松)。
var minRole = map[string]string{
	"GET /overview": roleViewer, "GET /users": roleViewer, ... 所有 GET 为 viewer
	"POST /users/": roleOperator, "POST /machines/": roleOperator, "POST /releases/": roleOperator, "POST /rollouts/": roleOperator, "POST /sites/": roleOperator, "POST /settings/": roleOperator,
	"POST /admins/": roleAdmin, "GET /admins": roleAdmin, "POST /tasks/": roleOperator,
}
func allowed(role, method, path string) bool
```
- `security` 角色暂等同 `viewer` + `/audit`(A1)全量,先不单独区分。
- [x] 测试 `TestRolesGateWrites`:viewer 能 GET 总览、POST 开户得 403;operator 能开户、POST /admins/create 得 403;admin 全通;未知路由默认 admin。
- [x] 测试 `TestAdminsPageOffersThreeRoles`。
- [x] 实现:中间件 `s.requireRole(handler)` 包在 `requireSession` 里,用 `minRole` 判;403 页面复用 `notices` 显示"你的角色是 X,这个操作需要 Y"。
- [x] 提交:`adminweb: viewer, operator and admin roles`

---

## 验收

- A1:对生产库查 `action=account.reissue` 能看到 9 月 19 日三条;CSV 下载在 Excel 打开中文不乱码(UTF-8 BOM)。
- A2:三台机器各打开一次,绑定历史与总览一致。
- A6:新建一个 viewer 账号登录,总览可看、关户按钮返回 403。

## 实施记录

2026-09-20:A1–A6 全部完成并部署到 47.84.48.181,提交 `180979b`(审计页)、`090de04`(机器页)、`911663d`(员工履历)、`cc97768`(发布报表与机队分布)、`30558ee`(角色)。生产库里 `account.reissue` 自 9 月 19 日起 3 条,审计页可见;现有管理员保持 `admin`。计划 B(用量快照、告警、凭据轮换)另写。
