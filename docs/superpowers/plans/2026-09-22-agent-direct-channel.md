# Agent 直连通道实施计划(阶段 2,agent 1.3.0)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** agent 不再把 OSS 当命令通道。策略、绑定、发布目标、凭据、状态上报、日志上传全部走控制台的设备 API,控制台改了什么,机器几秒内知道;OSS 只保留两类用途——安装包(CI 传、机器下载)和会话采集归档(机器上传)——并且机器上不再持有共享的 OSS AK,下载和上传都用控制台临时签发的预签名 URL。

**Architecture:** 控制台在现有进程里加一组 `/agent/v1/*` 接口,以设备令牌鉴权(库里只存哈希);"有变化"的判断由一个内存 hub 完成——所有会改到某台机器配置的操作在事务提交后唤醒该机器的长轮询,并有 15 秒一次的兜底重查。agent 侧把"从哪里取配置、往哪里报状态"抽成一个 `Source` 接口,现有的 OSS 实现原样保留为一种 Source(兼容与回退),新增 API Source;主循环用长轮询替代"整点拉取 + HEAD 比对"。存量机器零接触迁移:控制台把一次性注册码写进它现有的 OSS 绑定对象,1.3.0 agent 读到就换设备令牌,然后这条通道就废了。

**Tech Stack:** Go 1.26、PostgreSQL 15、pgx v5、`net/http`(长轮询,无第三方依赖)、Windows DPAPI(设备令牌落盘)、OSS 预签名 URL(V1 签名,已有 `SignedURL`)。

**Spec:** 本文件"设计"一节(用户 2026-09-22 确认);背景见 `设计_Agent与Codex发布流程_2026-09.md` §7、`docs/superpowers/specs/2026-09-12-rds-unified-control-plane-design.md` 阶段 2。

## 先替用户定下的三件事(不同意请改)

1. **"备份"= 每晚整库 dump 到 OSS `_backup/`,保留 30 天。** 旧格式的 `_policy/`、`_bindings/`、`_status/` 对象在全机队迁完后停写、清空,不再作为备份。
2. **独立更新器不进 1.3.0**,单独作为 1.3.1(设计稿 §5)。1.3.0 只换通道,自更新沿用现在的"换文件重启服务"。
3. **跳过 1.2.16**,下一个 agent 版本直接是 1.3.0。1.2.16 的 HEAD 心跳在 API 模式下没有意义;1.3.0 在 OSS 回退模式里保留它。

## Global Constraints

- 设备令牌、注册码、预签名 URL 不进日志、不进审计 before/after、不进任务备注;库里只存 SHA-256。
- 每个接口都有超时和大小上限:请求体 ≤ 1 MB(状态、日志尾),长轮询最多挂 25 秒(Cloudflare 上限 100 秒),每台机器同时最多一个长轮询(第二个到来时前一个立即返回 204)。
- 鉴权失败一律 401 且不区分"没有这台机器"和"令牌不对";同一 IP 连续失败按现有登录限速的口径限速。
- 1.3.0 agent 必须能在**没有**控制台的环境里照常工作(OSS 回退模式),否则 API 一挂全机队失联;API 模式下控制台不可达时按退避重试(5 秒起、翻倍、最长 5 分钟),期间沿用本地缓存的配置。
- 过渡期控制台同时写 OSS 命令对象和服务 API;两者由同一份"设备配置"函数生成,不允许两条算法。
- 不改网关;不改 Codex 打包;CI 上传安装包的流程不变。
- 每个任务:先写失败测试,再实现;`make check` 过;提交信息英文祈使句,尾注 `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`。
- 数据库测试:`TEST_PG_DSN='postgres://postgres:devpass@127.0.0.1:5433/aienv_test?sslmode=disable'`。

## 设计

### 设备 API(控制台,`/agent/v1/`)

| 方法 路径 | 鉴权 | 作用 |
|---|---|---|
| `POST /agent/v1/enrol` | 注册码 | `{enrol_token, hostname, agent_version}` → `{device_id, device_token}`;注册码一次性,过期 24 小时;成功后该设备 `channel=api` |
| `GET /agent/v1/config` | 设备令牌 | 本机完整配置:`{etag, policy, binding, credentials_etag, agent_target, codex_target, sync_nonce, restart_codex}`;带 `If-None-Match` 相同则 304 |
| `GET /agent/v1/wait?etag=` | 设备令牌 | 长轮询:配置变了立刻返回 200 + 新配置;25 秒没变返回 204;每次请求都刷新 `last_seen_at`(它就是心跳) |
| `GET /agent/v1/credentials` | 设备令牌 | 绑定员工的 `credentials.zip` 字节;`ETag` 头;未绑定 404 |
| `POST /agent/v1/status` | 设备令牌 | 请求体就是今天的 `model.Status` JSON,直接进 `Reports().Import`,不再经 `_status/` |
| `POST /agent/v1/log` | 设备令牌 | 日志尾(≤ 64 KB)存 `devices.log_tail`;控制台 `/log` 页优先读它 |
| `GET /agent/v1/artifact/{product}/{version}` | 设备令牌 | 校验该版本是本机当前目标或全局目标后,302 到 15 分钟有效的预签名 GET |
| `POST /agent/v1/collect/upload-url` | 设备令牌 | `{user, rel}` → 预签名 PUT(10 分钟),key 固定为 `data_collect/<user>/<rel>`,user 必须是本机绑定的员工 |
| `POST /agent/v1/token/rotate` | 设备令牌 | 发新令牌,旧令牌 10 分钟后失效 |

配置 `etag` = 配置 JSON(去掉 etag 字段)的 SHA-256 前 16 字节 hex;客户端只做相等比较。

### 唤醒(hub)

`internal/deviceapi.Hub`:`Wake(deviceIDs ...string)`、`WakeAll()`、`Wait(ctx, deviceID) <-chan struct{}`。以下位置在**事务提交后**调用(放在 ops 里,`Service` 新增可选字段 `Notifier`):

- 策略改动(`SetBlocked*`、`SetSyncInterval`、`SetCollect`、AppLocker、全局目标 Set/Clear)→ `WakeAll`
- `BindMachine`、`UnbindMachine`、`RequestSync`、`RestartCodex`、`ForgetMachine` → `Wake(device)`
- 员工 `Offboard`、`Reopen`、`Reissue/Rotate`、`Delete`、`SetModels` → `Wake(员工当前绑定的全部设备)`
- 发布:`CreateRollout`、`CancelRollout`、`ExcludeTarget`、`RetryTarget`、`SetRolloutPaused` → `Wake(涉及设备)`
- Worker `GatewayProvision` 成功(凭据入库)→ `Wake(员工绑定设备)`;`OSSExport.exportEmployee` 写完 credentials → 同上(过渡期两边都在)

兜底:`wait` 处理器每 15 秒重算一次 etag,即使没收到唤醒;单进程部署,hub 不需要跨进程。

### 设备配置的唯一来源

新包 `internal/deviceconfig`:`Build(ctx, store, deviceID) (Config, etag, error)`。它就是今天 `worker/export.go` 里 `exportBinding` + `machineTargets` 的组合再加上策略,抽出来后 `OSSExport` 和 `deviceapi` 都调它。凭据仍由 `exportEmployee` 生成 zip,但 zip 同时存一份到库(`credential_bundles(employee_id, epoch, zip bytea, etag, created_at)`),API 直接从库返回,不再依赖 OSS 上的 `users/<user>/credentials.zip`。

### 迁移

- 设置页新增"设备通道"一节(admin):`控制台 API 地址`(默认 `https://<PublicHost>`)、`自动注册存量机器`(开关)、`继续写 OSS 命令对象`(开关,默认开)。
- 自动注册开着时,`OSSExport.exportBinding` 给 `channel='oss'` 的机器在绑定对象里多写一个字段 `enrol: {console_url, token}`(注册码每台一个,写入时生成、24 小时过期、用过即废,重新导出会换新的)。1.3.0 agent 在 OSS 模式下读到 `enrol` 就去注册;注册成功后不再读 OSS 命令对象。
- 新装机器:`agent.exe setup --console https://… --enrol <码>`;注册码在控制台"绑定机器"时生成并显示一次(也可在机器页"重新注册"再生成)。
- 全机队 `channel=api` 后:关"继续写 OSS 命令对象" → Worker 不再导出 `_policy/_bindings/`,`StatusImport` 停;清空旧对象;1.3.1 起 agent 构建不再注入 OSS AK。

### agent(1.3.0)

- `internal/agentcore.Source` 接口:`Config() (DeviceConfig, etag, error)`、`Credentials() ([]byte, etag, error)`、`ReportStatus(Status) error`、`UploadLog([]byte) error`、`FetchArtifact(product, version, dest) (sha, error)`、`CollectPut(user, rel string, data []byte) error`、`Wait(ctx, etag) (changed bool, err error)`。
- `ossSource`:用今天的 `Store` 和 key 实现,`Wait` = 睁眼 HEAD 比对(1.2.16 的逻辑)。行为不变,现有测试改到这层。
- `apiSource`:`internal/agentapi.Client`(设备令牌、基地址、`http.Client` 30 秒超时,`wait` 单独 35 秒)。
- 令牌落盘:`%ProgramData%\ai-env-mgr\device.token`,Windows 用 DPAPI(LocalSystem 作用域)加密,非 Windows 明文 0600(只有测试)。
- 主循环:API 模式下 `Wait` 循环替代分钟 ticker;返回"变了"→ `RunOnce`;`RunOnce` 之后照常按策略间隔做全量兜底(默认可放到 60 分钟);状态每次 `RunOnce` 后 POST,长轮询请求本身刷新在线时间,所以不再单独心跳。
- 启动顺序:有 device.token → API 模式;没有但有 `--enrol` 记录 → 注册;都没有 → OSS 模式,并在每次读到绑定对象时检查 `enrol` 字段。API 连续失败超过 30 分钟且没有本地缓存配置 → 回退 OSS 模式一轮(仅当二进制里还有 AK)。

### 备份

`ai-env-admin backup` 子命令:`pg_dump`(通过 `docker exec aienv-pg15 pg_dump`,由 systemd timer 在宿主机跑)输出 gzip → 上传到 `_backup/aienv-<日期>.sql.gz`,并删除 30 天前的。timer 每天 UTC 2:00。CI 用户的 RAM 策略不变;上传用控制台的 AK。

---

### Task 1: OSS 预签名 PUT 与 `deviceconfig` 抽取

**Files:**
- Modify: `go/internal/ossclient/client.go`(`SignedURL` 已有 GET;加 `SignedPutURL(key string, ttl time.Duration, contentType string) (string, error)`)、`client_test.go`
- Create: `go/internal/deviceconfig/deviceconfig.go`、`deviceconfig_test.go`
- Modify: `go/internal/worker/export.go`(`exportBinding`/`machineTargets` 改为调 `deviceconfig.Build`,行为不变)

**Interfaces:**
```go
// deviceconfig
type Config struct {
	ETag            string              `json:"etag"`
	Policy          model.Policy        `json:"policy"`
	Binding         model.Binding       `json:"binding"`          // 含 targets、sync nonce、restart nonce,与今天写进 _bindings/ 的完全一致
	CredentialsETag string              `json:"credentialsEtag"`  // 有绑定且有凭据时非空
	Enrol           *Enrol              `json:"enrol,omitempty"`  // 只在写 OSS 对象且设备未注册时填
}
type Enrol struct{ ConsoleURL, Token string }
func Build(ctx context.Context, store repo.Store, deviceID string) (Config, error) // 算好 ETag
```
- [ ] 测试 `TestSignedPutURLIsAcceptedByTheBucket`(需 `TEST_OSS_*`,没有则跳过)与纯函数 `TestSignedPutURLSignsContentType`。
- [ ] 测试 `TestBuildMatchesWhatTheExporterWrites`:同一设备,`Build` 出的 `Binding` 与现有 `exportBinding` 写出的 JSON 字节相同(先用现有函数产出期望,再切换实现)。
- [ ] 实现;`export.go` 切到 `deviceconfig.Build`,现有 export 测试全绿。
- [ ] 提交:`deviceconfig: one function builds a machine's configuration; ossclient: presigned PUT`

### Task 2: 设备令牌与注册码(库)

**Files:**
- Create: `go/db/migrations/0011_device_channel.up.sql` / `.down.sql`
- Modify: `go/internal/repo/repo.go`(`Devices` 加字段与方法;新接口 `DeviceTokens`、`EnrolTokens`、`CredentialBundles`)、`go/internal/dbstore/store.go`
- Create: `go/internal/dbstore/devicetokens.go`、`devicetokens_test.go`、`bundles.go`

**Interfaces:**
```sql
alter table devices add column channel text not null default 'oss' check (channel in ('oss','api'));
alter table devices add column enrolled_at timestamptz;
alter table devices add column log_tail text not null default '';
alter table devices add column log_tail_at timestamptz;
create table device_tokens (
  id uuid primary key default gen_random_uuid(),
  device_id uuid not null references devices(id),
  token_hash bytea not null unique,
  created_at timestamptz not null default now(),
  last_used_at timestamptz,
  revoked_at timestamptz,
  grace_until timestamptz            -- 轮换后旧令牌还能用到何时
);
create table enrol_tokens (
  id uuid primary key default gen_random_uuid(),
  device_id uuid not null references devices(id),
  token_hash bytea not null unique,
  created_by text not null default '',
  created_at timestamptz not null default now(),
  expires_at timestamptz not null,
  used_at timestamptz
);
create table credential_bundles (
  employee_id uuid not null references employees(id),
  epoch integer not null,
  zip bytea not null,
  etag text not null,
  created_at timestamptz not null default now(),
  primary key (employee_id, epoch)
);
```
```go
type DeviceTokens interface {
	Issue(ctx, deviceID string) (plaintext string, err error)            // 生成 32 字节随机,存哈希,返回明文一次
	Authenticate(ctx, plaintext string) (Device, error)                 // 哈希查找;吊销或过 grace → ErrNotFound;顺手 last_used_at
	Revoke(ctx, deviceID string) (int, error)                           // 全部作废
	Rotate(ctx, deviceID string, grace time.Duration) (plaintext string, err error)
}
type EnrolTokens interface {
	Issue(ctx, deviceID, by string, ttl time.Duration) (plaintext string, err error)   // 同设备旧码作废
	Redeem(ctx, plaintext string) (Device, error)                                      // 一次性,过期 → ErrNotFound
	OpenFor(ctx, deviceID string) (exists bool, err error)                             // 导出器判断要不要再发
}
type CredentialBundles interface {
	Put(ctx, employeeID string, epoch int, zip []byte, etag string) error
	Live(ctx, employeeID string) (zip []byte, etag string, err error)   // 员工当前 epoch 的
	Purge(ctx, employeeID string) error
}
```
- [ ] 测试 `TestDeviceTokensAuthenticateRotateRevoke`、`TestEnrolTokensAreOneShotAndExpire`、`TestCredentialBundlesFollowTheEpoch`。
- [ ] 迁移 + 实现 + `grants.sql`(`device_tokens`、`enrol_tokens` 不给 `aienv_ro` select);`make check`。
- [ ] 提交:`dbstore: device tokens, enrolment codes, credential bundles`

### Task 3: 设备 API

**Files:**
- Create: `go/internal/deviceapi/server.go`(路由、鉴权中间件、限速)、`enrol.go`、`config.go`、`status.go`、`artifact.go`、`collect.go`、`hub.go`、`server_test.go`(httptest + DB)
- Modify: `go/internal/adminweb/server.go`(挂载 `/agent/v1/` 到 `deviceapi.Handler`,在 `requireSession` 之外)、`roles.go`(`/agent/v1/` 不走管理员角色)
- Modify: `go/internal/worker/export.go`(`exportEmployee` 同时 `CredentialBundles().Put`)

**Interfaces:**
```go
type Server struct {
	Store    repo.Store
	Objects  Presigner            // SignedURL / SignedPutURL
	Hub      *Hub
	Events   *eventlog.Writer     // 只记 device_id、路径、结果,不记令牌
	Now      func() time.Time
	WaitMax  time.Duration        // 25s
	Recheck  time.Duration        // 15s
}
func (s *Server) Handler() http.Handler
```
- 鉴权:`Authorization: Bearer <token>` → `DeviceTokens().Authenticate`;失败 401;成功把 `repo.Device` 放进 context。每个请求 `MarkSeen(device, agentVersion 来自 User-Agent "ai-env-agent/1.3.0")`。
- `enrol`:限速(同 IP 每分钟 10 次);`EnrolTokens().Redeem` → `DeviceTokens().Issue` → `devices.channel='api', enrolled_at=now()` → 审计 `device.enrol`(actor `device:<hostname>`)。
- `config`/`wait`:`deviceconfig.Build`;`wait` 里 `select { hub.Wait(device) | ticker(Recheck) | ctx.Done | time.After(WaitMax) }`。
- `status`:解码 `model.Status`,`Machine` 必须等于设备主机名(不区分大小写),否则 400;`Reports().Import`;`settleTargets`(现有函数,从 StatusImport 抽成可共用)。
- `artifact`:版本必须是本机 `binding.AgentTarget/CodexTarget` 或策略全局目标之一,否则 403;302 到 `SignedURL(artifact.ObjectKey, 15m)`。
- `collect/upload-url`:`user` 必须等于本机绑定员工;`rel` 不得含 `..`、不得以 `/` 开头;返回 `SignedPutURL(ossclient.DataCollectKey(user, rel), 10m, "application/octet-stream")`。
- [ ] 测试 `TestEnrolThenConfigThenStatus`(端到端:注册码换令牌 → config 200 → 同 etag 304 → 状态 POST 入库并 MarkSeen)。
- [ ] 测试 `TestWaitReturnsWhenWoken`(goroutine 挂 wait,`hub.Wake` 后 200 且 etag 变;不唤醒 25 秒 204 — 用 `WaitMax=200ms` 跑)。
- [ ] 测试 `TestArtifactOnlyForTargetedVersions`、`TestCollectURLOnlyForTheBoundUser`、`TestTokensNeverAppearInLogs`(events 假接收器里 grep 令牌明文)。
- [ ] 实现;`make check`。
- [ ] 提交:`deviceapi: the agent's channel to the console`

### Task 4: 唤醒接入 ops 与 Worker

**Files:**
- Modify: `go/internal/ops/ops.go`、`policy.go`、`releases.go`(`Service.Notifier` 字段;每个改配置的方法在事务提交后调用)、`go/internal/worker/gateway.go`(凭据入库后唤醒)、`go/internal/adminweb/dbmode.go`(装配 hub)
- Modify: `go/internal/ops/ops_test.go`(假 Notifier 断言被唤醒的设备)

**Interfaces:**
```go
type Notifier interface { Wake(deviceIDs ...string); WakeAll() }
```
- [ ] 测试 `TestChangesWakeTheMachinesTheyTouch`:绑定 → 该机;改策略 → 全部;员工改模型 → 其绑定机;发布创建 → 目标机;开通成功 → 员工绑定机。
- [ ] 实现;`make check`。
- [ ] 提交:`ops: wake a machine's long poll when its configuration changes`

### Task 5: 控制台页面与迁移开关

**Files:**
- Create: `go/db/migrations/…`(不需要;设置走 `settings`:`SettingDeviceChannel = "device_channel"`)
- Modify: `go/internal/repo/alerts.go`(→ 移到新文件 `settings_types.go`:`DeviceChannelSettings{ConsoleURL string; AutoEnrol bool; WriteOSSObjects bool}`,默认 `{PublicHost, false, true}`)
- Modify: `go/internal/worker/export.go`(`WriteOSSObjects=false` 时 `exportPolicy/exportBinding` 直接返回 note "oss channel off";`AutoEnrol` 且设备 `channel='oss'` 且无未用注册码 → 发一个并写进对象的 `enrol` 字段)、`status.go`(`WriteOSSObjects=false` 时 StatusImport 跳过)
- Modify: `go/internal/adminweb/`:`overview.html`(机器表加"通道"列:API/OSS 标签、令牌年龄)、`machine.html`(按钮:生成注册码(显示一次)、吊销令牌、轮换令牌)、`settings.html` 新页签"设备通道"(三个设置)、`handlers.go`/`actions.go`/`roles.go`(admin)、`/log` 页优先读 `devices.log_tail`
- Modify: `docs/操作手册.md`(§4.10 设备注册与迁移)

- [ ] 测试 `TestBindShowsAnEnrolCodeOnce`、`TestAutoEnrolWritesACodeIntoTheBindingObject`(假对象存储里读回 `enrol.token`,且库里对应哈希存在)、`TestOSSChannelOffStopsExports`、`TestLogPagePrefersTheDatabaseTail`。
- [ ] 实现;`make check`。
- [ ] 提交:`adminweb: device channel settings, enrolment codes, token controls`

### Task 6: agent `Source` 抽象与 OSS 实现

**Files:**
- Create: `go/internal/agentcore/source.go`(接口)、`source_oss.go`、`source_oss_test.go`
- Modify: `go/internal/agentcore/sync.go`(`Syncer.Store` → `Syncer.Source`;`RunOnce` 里的 `s.Store.Get(PolicyKey)` 等全部改走 Source;`ChangedSinceLastSync` 移到 `ossSource.Wait`)、`sync_test.go`、`codex.go`(安装包下载改 `FetchArtifact`)、`collect.go`(`Store.Put` → `CollectPut`)
- Modify: `go/cmd/agent/main.go`(装配 `ossSource`,行为不变)

**Interfaces:**
```go
type DeviceConfig = deviceconfig.Config
type Source interface {
	Config(ctx context.Context) (DeviceConfig, error)              // ETag 在 Config 里
	Credentials(ctx context.Context) ([]byte, string, error)
	ReportStatus(ctx context.Context, st model.Status) error
	UploadLog(ctx context.Context, tail []byte) error
	FetchArtifact(ctx context.Context, product, version, dest string) (sha256hex string, err error)
	CollectPut(ctx context.Context, user, rel string, data []byte) error
	// Wait blocks until the configuration may have changed, the context ends,
	// or the source's own patience runs out; changed=false means "ask again".
	Wait(ctx context.Context, etag string) (changed bool, err error)
}
```
- `ossSource.Config` = 读 `_policy/policy.json` + `_bindings/<机器>` 拼成 `DeviceConfig`(etag 取两者 ETag 拼接);`Wait` = 1.2.16 的 HEAD 比对,睡 1 分钟。
- [ ] 测试:现有 `sync_test.go` 全部通过 `ossSource` 跑通(这是"行为不变"的证明);新增 `TestOSSSourceConfigCarriesEnrol`。
- [ ] 提交:`agentcore: a Source abstraction, with OSS as the first implementation`

### Task 7: agent API 客户端、令牌落盘、注册

**Files:**
- Create: `go/internal/agentapi/client.go`、`client_test.go`(httptest 服务端)、`go/internal/agentapi/tokenstore_windows.go`(DPAPI:`golang.org/x/sys/windows` 的 `CryptProtectData`)、`tokenstore_other.go`、`tokenstore_test.go`
- Create: `go/internal/agentcore/source_api.go`、`source_api_test.go`
- Modify: `go/cmd/agent/main.go`(启动选路:令牌 → API;`--enrol`/OSS `enrol` 字段 → 注册;否则 OSS)、`setup.go`(`--console`、`--enrol` 参数,写 `%ProgramData%\ai-env-mgr\enrol.json` 供服务首次启动使用,用完删)、`credentials.go`(`consoleURL` ldflags 变量,默认空)

**Interfaces:**
```go
type Client struct{ BaseURL string; Token string; HTTP *http.Client; UserAgent string }
func Enrol(ctx, baseURL, enrolToken, hostname, version string) (deviceID, deviceToken string, err error)
func (c *Client) Config(ctx) (deviceconfig.Config, error)          // If-None-Match 由 apiSource 管
func (c *Client) Wait(ctx, etag string) (cfg *deviceconfig.Config, changed bool, err error)
func (c *Client) Credentials(ctx) ([]byte, string, error)
func (c *Client) PostStatus(ctx, st model.Status) error
func (c *Client) PostLog(ctx, tail []byte) error
func (c *Client) ArtifactURL(ctx, product, version string) (string, error)   // 跟随 302 前取 Location
func (c *Client) CollectUploadURL(ctx, user, rel string) (string, error)
func (c *Client) Rotate(ctx) (newToken string, err error)
```
- `apiSource.Wait` 出错时按退避(5s→…→5m)睡再返回 `changed=false`;连续失败 30 分钟且 `ossFallback != nil` → 主循环切 OSS 模式一轮后再试 API。
- [ ] 测试 `TestEnrolStoresTheTokenAndSwitchesToAPI`(假服务端;令牌文件权限 0600;明文不在日志)、`TestAPISourceUsesIfNoneMatch`、`TestWaitBacksOffWhenTheConsoleIsDown`、`TestArtifactFollowsThePresignedRedirectAndVerifiesSHA`。
- [ ] 提交:`agent: talk to the console directly, enrol with a one-time code`

### Task 8: agent 主循环改为长轮询

**Files:**
- Modify: `go/cmd/agent/main.go`(`loop`:API 模式下用 `Source.Wait` goroutine 触发 `sync("console changed")`;`checkEvery` ticker 只管到期兜底和唤醒;OSS 模式行为不变)、`main_test.go`(用假 Source 验证触发次序)
- Modify: `go/internal/model/model.go`(`DefaultSyncInterval` 在 API 模式默认 60;Status 加 `Channel string`)

- [ ] 测试 `TestLoopSyncsWhenTheSourceSaysChanged`、`TestLoopFallsBackToTheIntervalWhenWaitKeepsFailing`。
- [ ] 手工:测试机装 1.3.0,控制台点"立即同步"或改策略,机器 3 秒内日志出现 `console changed sync ok`。
- [ ] 提交:`agent: long-poll the console instead of polling the bucket`

### Task 9: 发布与迁移执行(运维步骤,不是代码)

- [ ] 打 `v1.3.0`,CI 传 OSS,版本库登记、测试机验收(§4.7 表 + 本计划"验收")。
- [ ] 设置页"设备通道":填 API 地址、开"自动注册存量机器"。用"设为全局目标"把三台机器升到 1.3.0(它们是 1.2.14,只认全局目标);观察机器页"通道"列逐台变 API。
- [ ] 全部 API 后:关"继续写 OSS 命令对象";确认 `_status/` 不再更新、告警无新离线;一周后清空 `_policy/`、`_bindings/`、`_status/`、`_logs/`、`users/*/credentials.zip`。
- [ ] `docs/操作手册.md`、`docs/OSS布局.md` 更新:OSS 只剩 `_agent/`、`_codex/`、`data_collect/`、`_backup/`。

### Task 10: 每晚整库备份到 OSS

**Files:**
- Modify: `go/cmd/admin/`(子命令 `backup -file <sql.gz>`:上传到 `_backup/aienv-<YYYYMMDD>.sql.gz`,删 30 天前的;用控制台 AK)
- Create: `dist/ai-env-backup.sh`、`dist/ai-env-backup.service`、`dist/ai-env-backup.timer`(UTC 2:00;`docker exec aienv-pg15 pg_dump -U postgres aienv | gzip` → 临时文件 0600 → `ai-env-admin backup -file` → 删临时文件)
- Modify: `dist/ram-policy-agent.json` 或控制台 RAM 用户策略(允许 `_backup/*` Put/Delete/List;CI 用户不变)

- [ ] 测试 `TestBackupUploadsAndPrunes`(假对象存储:传一个、列出 31 天前的被删)。
- [ ] 部署 timer;第二天核对 `_backup/` 有文件、大小合理、能 `gunzip | head`。
- [ ] 提交:`admin: nightly database dump to the bucket`

### Task 11(1.3.1,另开计划):独立更新器

设计稿 §5。本计划不做,列在这里是为了说明 1.3.0 的自更新仍是"换文件重启",全机队定向发布 agent 仍需谨慎。

## 验收

- 控制台改策略、绑定、发目标、重发凭据、"立即同步"、"关闭 Codex",测试机 3 秒内开始处理(日志 `console changed`),状态页 5 秒内更新。
- 拔掉控制台(停服务)10 分钟:机器日志显示退避重试,本地策略照常生效,凭据不丢;恢复后 1 分钟内重新连上,不重复同步。
- 令牌吊销后机器所有请求 401,机器页显示"未注册";重新生成注册码、手工 `agent.exe setup --enrol` 后恢复。
- 机器上 `strings agent.exe | grep -i LTAI` 在 1.3.1 后为空(1.3.0 仍含 AK,用于回退)。
- 会话采集在 API 模式下照常上传到 `data_collect/`,OSS 侧只见预签名 PUT;下载安装包只见预签名 GET。
- 备份:`_backup/` 每天一个文件,30 天滚动。

## 实施顺序

1 → 2 → 3 → 4 → 5(控制台可先上线,对 1.2.14 机器无影响)→ 6 → 7 → 8(agent)→ 9(发布迁移)→ 10 随时可插。控制台部分约 5 个任务、agent 部分 3 个任务;建议控制台先合并上线,再发 agent,避免 agent 先于 API 存在。
