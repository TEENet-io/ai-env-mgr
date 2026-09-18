# Agent 与 Codex 发布流程 实施计划(第一期)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把"上传即全机队发布"改成"上传候选 → 版本库 → 指定机器试发 → 按回执确认 → 扩大或回滚",大包不再驻留内存,旧 Agent 继续走全局通道不受影响。

**Architecture:** 版本库(`release_artifacts`)、发布任务(`release_rollouts`)、设备目标(`release_targets`)三张表进 PostgreSQL,制品按版本存 OSS;设备目标是"期望状态",不进 `tasks` 表。定向目标搭在每台机器已经在读的绑定对象上(`_bindings/<机器>`),不新增 OSS 前缀、不改 RAM 授权;Agent 1.2.16 读绑定里的按机目标覆盖全局策略,并在状态里回报目标、代次、延后原因;Worker 导入状态时按回执结算目标。控制台上传走落盘 + 分片上传,进程内存与包大小无关。

**Tech Stack:** Go 1.26,PostgreSQL 15,pgx v5,阿里云 OSS Go SDK v3(`bucket.UploadFile` 分片上传、`bucket.CopyObject` 服务端复制),现有 `internal/ops`、`internal/worker`、`internal/agentcore`、`internal/adminweb`。**不引入新依赖。**

**Spec:** `docs/superpowers/specs/2026-09-18-release-pipeline-design.md`(= `设计_Agent与Codex发布流程_2026-09.md`)。本计划覆盖 spec §8 的第 1、2、4 项在旧协议(OSS 通道)上的实现;§8 第 3 项(Agent 独立更新器与启动失败恢复,Agent 1.3.0)另写计划;spec §7 第 2 步"统一 API 与设备身份"属 RDS 阶段 2,不在本计划内。

## Global Constraints

- 上传候选包不改变任何设备的目标;设为全局目标、创建发布任务都是独立的显式动作(spec §2、§3.1)。
- 同一(产品, 版本)只对应一个 SHA256;内容不同必须换版本号;版本表上唯一约束是规则本身(spec §3.1)。
- 控制台处理 Codex 包时进程内存与包大小无关:落盘到 spool 目录、边下边算 SHA256、分片上传;失败清理本次临时文件(spec §6)。控制台 systemd 单元的 `MemoryMax=600M` **不放宽**。
- 同一设备、同一产品同时只有一条 `pending` 目标(部分唯一索引);新目标代次递增,旧回执(代次不匹配)不能改新目标的状态(spec §4.2)。
- 离线设备保持 `pending`,不计成功;"排除"必须带原因,报表里排除与成功分开计数(spec §3.4)。
- 旧 Agent(< 1.2.16)只认 `policy.json` 的全局目标;后台拒绝给它们建设备目标(spec §3.3、§7)。首个 1.2.16 由旧全局通道发布。
- 一台机器一个通道:定向目标只覆盖全局目标,不叠加;目标对象不可用时 Agent 不回退到别的来源(spec §7)。
- 制品第一期不清理,OSS 里所有版本保留(spec §3.4 的清理策略推后)。
- 秘密不进日志、审计、错误信息;GitHub token 只在一次请求里用,不落盘。
- 提交信息英文祈使句,尾注只有 `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`;不加 `Claude-Session`;`make check` 必须通过;库相关测试用 `TEST_PG_DSN='postgres://postgres:devpass@127.0.0.1:5433/aienv_test?sslmode=disable'`。
- 采集器不再碰 `.claude`;不放开 wscript/cscript/mshta/regedit/reg。

## 文件结构

| 文件 | 职责 |
|---|---|
| `go/db/migrations/0006_releases.{up,down}.sql` | 三张表 |
| `go/internal/repo/releases.go` | 制品、发布任务、设备目标的类型与仓储接口 |
| `go/internal/dbstore/releases.go` (+`_test.go`) | 仓储实现 |
| `go/internal/model/version.go` (+`_test.go`) | 版本号比较、定向最低 Agent 版本 |
| `go/internal/model/model.go` | `Binding` 的按机目标;`Status` 的目标/代次/延后原因 |
| `go/internal/ossclient/client.go` | 按版本的 agent key、服务端复制、从文件分片上传 |
| `go/internal/ghrelease/download.go` (+`_test.go`) | 流式下载到文件 |
| `go/internal/ops/releases.go` (+`_test.go`) | 登记制品、设全局目标、建/停/取消/排除/重试发布任务 |
| `go/internal/worker/export.go` (+`_test.go`) | 策略导出前复制 agent 制品到固定 key;绑定对象带按机目标 |
| `go/internal/worker/status.go` (+`_test.go`) | 导入状态后结算设备目标 |
| `go/internal/agentcore/{sync,codex}.go` (+`_test.go`) | 目标解析、代次标记、延后原因、状态字段 |
| `go/internal/status/status.go` | 新字段进 `Status` |
| `go/internal/adminweb/releases.go`、`rollouts.go` (+`_test.go`)、`assets/releases.html`、`assets/rollout_new.html`、`assets/rollouts.html`、`assets/rollout_detail.html` | 版本库页、发布任务页 |
| `go/internal/adminweb/publish.go`、`download.go`、`server.go`、`dbmode.go`、`dbbackend.go` | 落盘上传、路由、spool 配置 |
| `docs/OSS布局.md`、`docs/数据库模式部署.md`、`docs/操作手册.md` | 布局、部署、发布手册 |

---

### Task 1: 版本号比较、按版本的 OSS key、服务端复制与文件上传

**Files:**
- Create: `go/internal/model/version.go`, `go/internal/model/version_test.go`
- Modify: `go/internal/ossclient/client.go:205-220`
- Test: `go/internal/ossclient/client_test.go`(已有,追加)

**Interfaces:**
- Produces: `model.CompareVersions(a, b string) int`;`model.MinTargetAgentVersion = "1.2.16"`;`ossclient.AgentVersionKey(version string) string`;`(*ossclient.Client).Copy(src, dst string) error`;`(*ossclient.Client).PutFile(key, path string, onProgress func(done, total int64)) error`。

- [ ] **Step 1: 写版本比较的失败测试**

`go/internal/model/version_test.go`:

```go
package model

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.16", "1.2.9", 1},
		{"1.2.9", "1.2.16", -1},
		{"v1.2.16", "1.2.16", 0},
		{"1.3.0", "1.2.16", 1},
		{"1.2.16", "1.2.16.1", -1},
		{"dev", "1.2.16", -1},   // 非数字视为最低
		{"", "1.2.16", -1},
		{"1.2.16-rc1", "1.2.16", -1}, // 预发布低于正式
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestAgentCanTakeTargets(t *testing.T) {
	if !AgentCanTakeTargets("1.2.16") || !AgentCanTakeTargets("1.3.0") {
		t.Fatal("1.2.16 and later must be able to take per-machine targets")
	}
	if AgentCanTakeTargets("1.2.15") || AgentCanTakeTargets("") {
		t.Fatal("older agents only read the fleet policy")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd go && go test ./internal/model/ -run 'TestCompareVersions|TestAgentCanTakeTargets'`
Expected: FAIL,`undefined: CompareVersions`

- [ ] **Step 3: 实现**

`go/internal/model/version.go`:

```go
package model

import (
	"strconv"
	"strings"
)

// MinTargetAgentVersion is the first agent that reads per-machine release
// targets from its binding object. Anything older only reads the fleet
// policy, so a target aimed at it would sit there unread and the console
// refuses to create one.
const MinTargetAgentVersion = "1.2.16"

// AgentCanTakeTargets reports whether an agent of this version reads
// per-machine targets.
func AgentCanTakeTargets(version string) bool {
	return CompareVersions(version, MinTargetAgentVersion) >= 0
}

// CompareVersions orders two dotted version strings: -1, 0 or 1.
//
// A leading "v" is ignored. Numeric components compare as numbers, so 1.2.16
// is after 1.2.9. A pre-release suffix ("1.2.16-rc1") sorts before the plain
// version. A string with no numeric component at all ("dev", "") is the
// lowest of all: an agent that cannot say what it is gets nothing aimed at it.
func CompareVersions(a, b string) int {
	an, apre := splitVersion(a)
	bn, bpre := splitVersion(b)
	for i := 0; i < len(an) || i < len(bn); i++ {
		x, y := 0, 0
		if i < len(an) {
			x = an[i]
		}
		if i < len(bn) {
			y = bn[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case apre == bpre:
		return 0
	case apre == "": // the plain version is after its pre-releases
		return 1
	case bpre == "":
		return -1
	case apre < bpre:
		return -1
	default:
		return 1
	}
}

// splitVersion returns the numeric components and the pre-release suffix.
// A version with no numeric component returns nil, which compares below
// everything.
func splitVersion(v string) ([]int, string) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	pre := ""
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v, pre = v[:i], v[i+1:]
	}
	var nums []int
	for _, part := range strings.Split(v, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, ""
		}
		nums = append(nums, n)
	}
	if len(nums) == 0 {
		return nil, ""
	}
	return nums, pre
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd go && go test ./internal/model/ -run 'TestCompareVersions|TestAgentCanTakeTargets'`
Expected: PASS

- [ ] **Step 5: 写 OSS key 的失败测试**

追加到 `go/internal/ossclient/client_test.go`:

```go
func TestAgentVersionKeyIsUnderTheAgentPrefix(t *testing.T) {
	got := AgentVersionKey("1.2.16")
	if got != "agent_workdir/_agent/1.2.16/agent.exe" {
		t.Fatalf("AgentVersionKey = %q", got)
	}
	// The agent's RAM policy grants GetObject on _agent/*, so a versioned
	// key must stay under that prefix or no machine could fetch it.
	if !strings.HasPrefix(got, AgentPrefix) {
		t.Fatalf("%q is outside %q", got, AgentPrefix)
	}
	if AgentVersionKey("../x") == "agent_workdir/_agent/../x/agent.exe" {
		t.Fatal("a version must be sanitised like every other key segment")
	}
}
```

- [ ] **Step 6: 跑测试确认失败**

Run: `cd go && go test ./internal/ossclient/ -run TestAgentVersionKey`
Expected: FAIL,`undefined: AgentVersionKey`

- [ ] **Step 7: 实现 key、Copy、PutFile**

在 `go/internal/ossclient/client.go` 的 `AgentBinaryKey` 之后加:

```go
// AgentVersionKey is where one version of the agent binary is kept.
//
// AgentBinaryKey is the single object every agent in the field reads; this
// is the versioned copy behind it. Setting a version as the fleet target
// copies it to AgentBinaryKey server-side (see worker.OSSExport), so a
// rollback is a copy rather than a re-upload, and the bytes a version name
// refers to never change.
func AgentVersionKey(version string) string {
	return AgentPrefix + sanitiseSegment(version) + "/agent.exe"
}
```

在 `PutProgress` 之后加:

```go
// Copy duplicates an object inside the bucket without the bytes passing
// through this process. Used to point the fixed agent key at a versioned
// artifact.
func (c *Client) Copy(src, dst string) error {
	if _, err := c.bucket.CopyObject(src, dst); err != nil {
		return fmt.Errorf("copy %q to %q: %w", src, dst, err)
	}
	return nil
}

// putFilePartSize is the multipart chunk. 16 MB keeps a 700 MB installer at
// under fifty parts and bounds what is in memory at any moment to a few
// parts, whatever the file size.
const putFilePartSize = 16 << 20

// PutFile uploads a local file in parts, reading it from disk as it goes.
//
// This is the only way a large package should ever reach the bucket from
// the console: Put takes the whole object in memory, and the console runs
// under a memory cap that a Codex installer alone would exceed.
func (c *Client) PutFile(key, path string, onProgress func(done, total int64)) error {
	options := []oss.Option{oss.Routines(3)}
	if onProgress != nil {
		options = append(options, oss.Progress(&putProgress{report: onProgress}))
	}
	if err := c.bucket.UploadFile(key, path, putFilePartSize, options...); err != nil {
		return fmt.Errorf("upload %q to %q: %w", path, key, err)
	}
	return nil
}
```

- [ ] **Step 8: 跑测试与 vet**

Run: `cd go && go test ./internal/ossclient/ ./internal/model/ && go vet ./internal/ossclient/`
Expected: PASS

- [ ] **Step 9: 提交**

```bash
cd go && git add internal/model/version.go internal/model/version_test.go internal/ossclient/client.go internal/ossclient/client_test.go ../docs/superpowers/specs/2026-09-18-release-pipeline-design.md ../docs/superpowers/plans/2026-09-18-release-pipeline.md
git commit -m "Version ordering, versioned agent keys, server-side copy and multipart upload

Groundwork for the release pipeline: the console will keep every agent
build under its own key and copy the chosen one to the fixed key the
fleet reads, and will upload packages from disk in parts rather than
from memory.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: 下载流式落盘

**Files:**
- Modify: `go/internal/ghrelease/download.go:42-160`
- Test: `go/internal/ghrelease/download_test.go`

**Interfaces:**
- Produces: `ghrelease.FetchToFile(url, token string, timeout time.Duration, dest string, maxBytes int64, onProgress func(done, total int64)) (Fetched, error)`,`type Fetched struct{ SHA256 string; Size int64 }`。
- `Fetch` / `FetchProgress` 保留给 CLI 与旧页面,但内部改为调用同一个流式函数再读回,不再各自缓冲。

- [ ] **Step 1: 写失败测试**

追加到 `go/internal/ghrelease/download_test.go`(该文件已有 `httptest` 服务器的写法,照用):

```go
func TestFetchToFileStreamsAndChecksums(t *testing.T) {
	payload := bytes.Repeat([]byte("codex"), 200_000) // ~1 MB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(payload)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "pkg.bin")
	var last int64
	got, err := FetchToFile(srv.URL+"/x", "", time.Minute, dest, 10<<20, func(done, total int64) { last = done })
	if err != nil {
		t.Fatalf("FetchToFile: %v", err)
	}
	sum := sha256.Sum256(payload)
	if got.SHA256 != hex.EncodeToString(sum[:]) || got.Size != int64(len(payload)) {
		t.Fatalf("got %+v", got)
	}
	if last != int64(len(payload)) {
		t.Fatalf("progress ended at %d, want %d", last, len(payload))
	}
	onDisk, _ := os.ReadFile(dest)
	if !bytes.Equal(onDisk, payload) {
		t.Fatal("the file on disk is not what was served")
	}
}

func TestFetchToFileRefusesOversizeAndCleansUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("x"), 4096))
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "pkg.bin")
	_, err := FetchToFile(srv.URL+"/x", "", time.Minute, dest, 1024, nil)
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v, want an oversize refusal", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("a refused download must not leave a partial file behind")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd go && go test ./internal/ghrelease/ -run TestFetchToFile`
Expected: FAIL,`undefined: FetchToFile`

- [ ] **Step 3: 实现**

在 `download.go` 中,把 `get` 改为写入 `io.Writer`,新增 `FetchToFile`;`Fetch`/`FetchProgress` 改为先落到临时文件再读回(它们只被 CLI 和旧页面用,包都在 64 MB 以内):

```go
// Fetched describes a download that reached disk whole.
type Fetched struct {
	SHA256 string // hex
	Size   int64
}

// FetchToFile downloads url to dest, hashing as it goes, and never holds
// more than a buffer of the body in memory. It refuses a body larger than
// maxBytes and removes dest on any failure, so a caller either has a
// complete, checksummed file or nothing.
//
// A partial file is worse than none: the next step uploads what is on disk,
// and a truncated installer with a checksum computed over the truncation
// would pass every check on the way to a desktop.
func FetchToFile(url, token string, timeout time.Duration, dest string, maxBytes int64, onProgress func(done, total int64)) (Fetched, error) {
	client := &http.Client{Timeout: timeout}
	resolved, err := resolveAssetURL(client, url, token)
	if err != nil {
		return Fetched{}, err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Fetched{}, err
	}
	sum := sha256.New()
	size, err := get(client, resolved, token, maxBytes, io.MultiWriter(f, sum), onProgress)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(dest)
		return Fetched{}, err
	}
	return Fetched{SHA256: hex.EncodeToString(sum.Sum(nil)), Size: size}, nil
}
```

`get` 的签名改为 `func get(client *http.Client, url, token string, maxBytes int64, w io.Writer, onProgress func(done, total int64)) (int64, error)`:保留现有的 URL 前缀检查、`Accept: application/octet-stream`、token 头、404/HTML 判断;主体改为:

```go
	if maxBytes > 0 && resp.ContentLength > maxBytes {
		return 0, fmt.Errorf("the download is %d MB, larger than the %d MB limit", resp.ContentLength>>20, maxBytes>>20)
	}
	if onProgress != nil {
		onProgress(0, resp.ContentLength)
	}
	var body io.Reader = resp.Body
	if onProgress != nil {
		body = &countingReader{r: resp.Body, total: resp.ContentLength, report: onProgress}
	}
	if maxBytes > 0 {
		body = io.LimitReader(body, maxBytes+1)
	}
	n, err := io.Copy(w, body)
	if err != nil {
		return n, fmt.Errorf("download: %w", err)
	}
	if maxBytes > 0 && n > maxBytes {
		return n, fmt.Errorf("the download is larger than the %d MB limit", maxBytes>>20)
	}
	return n, nil
```

`resolveAssetURL` 是现在 `Fetch` 里把 GitHub 浏览器链接换成 API 资产链接的那段(已有,抽成函数即可)。`Fetch` 与 `FetchProgress`:

```go
func FetchProgress(url, token string, timeout time.Duration, onProgress func(done, total int64)) ([]byte, error) {
	tmp, err := os.CreateTemp("", "ghrelease-*")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	tmp.Close()
	defer os.Remove(name)
	if _, err := FetchToFile(url, token, timeout, name, 0, onProgress); err != nil {
		return nil, err
	}
	return os.ReadFile(name)
}
```

- [ ] **Step 4: 跑整个包的测试**

Run: `cd go && go test ./internal/ghrelease/`
Expected: PASS(旧的五个测试也过)

- [ ] **Step 5: 提交**

```bash
cd go && git add internal/ghrelease/
git commit -m "ghrelease: download to a file, hashing as it goes

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: 三张表、仓储接口与实现

**Files:**
- Create: `go/db/migrations/0006_releases.up.sql`, `go/db/migrations/0006_releases.down.sql`, `go/internal/repo/releases.go`, `go/internal/dbstore/releases.go`, `go/internal/dbstore/releases_test.go`
- Modify: `go/internal/repo/repo.go:47-63`(Store 接口加 `Releases()`), `go/internal/dbstore/store.go:39-52`

**Interfaces:**
- Produces(下游任务全部依赖这些名字):

```go
package repo

const (
	ProductAgent = "agent"
	ProductCodex = "codex"
)

type ArtifactStatus string

const (
	ArtifactCandidate ArtifactStatus = "candidate"
	ArtifactAccepted  ArtifactStatus = "accepted"
	ArtifactStable    ArtifactStatus = "stable"
	ArtifactRetired   ArtifactStatus = "retired"
)

type Artifact struct {
	ID, Product, Version, SHA256 string
	SizeBytes                    int64
	ObjectKey                    string
	Status                       ArtifactStatus
	Notes, Source                string
	MinAgentVersion              string
	AcceptanceNote, AcceptedBy   string
	AcceptedAt                   *time.Time
	CreatedBy                    string
	CreatedAt, UpdatedAt         time.Time
}

type NewArtifact struct {
	Product, Version, SHA256 string
	SizeBytes                int64
	ObjectKey                string
	Notes, Source            string
	MinAgentVersion          string
	CreatedBy                string
}

type RolloutKind string

const (
	RolloutRelease  RolloutKind = "release"
	RolloutRollback RolloutKind = "rollback"
)

type Rollout struct {
	ID, Product, ArtifactID string
	Kind                    RolloutKind
	RollbackOf              string // rollout id, for a rollback
	Note, CreatedBy         string
	CreatedAt               time.Time
	PausedAt                *time.Time
	PausedBy                string
}

type NewRollout struct {
	Product, ArtifactID string
	Kind                RolloutKind
	RollbackOf          string
	Note, CreatedBy     string
}

type TargetStatus string

const (
	TargetPending    TargetStatus = "pending"
	TargetSucceeded  TargetStatus = "succeeded"
	TargetFailed     TargetStatus = "failed"
	TargetCancelled  TargetStatus = "cancelled"
	TargetSuperseded TargetStatus = "superseded"
	TargetExcluded   TargetStatus = "excluded"
)

type Target struct {
	ID, DeviceID, Product, ArtifactID, RolloutID string
	Generation                                   int
	Status                                       TargetStatus
	ResultNote, ReportedVersion, ExcludeReason   string
	CreatedAt, UpdatedAt                         time.Time
	FinishedAt                                   *time.Time
}

type Releases interface {
	CreateArtifact(ctx context.Context, a NewArtifact) (Artifact, error) // ErrDuplicate on (product, version)
	ArtifactByID(ctx context.Context, id string) (Artifact, error)
	ArtifactByVersion(ctx context.Context, product, version string) (Artifact, error)
	ListArtifacts(ctx context.Context, product string) ([]Artifact, error) // newest first; "" = both products
	SetArtifactStatus(ctx context.Context, id string, status ArtifactStatus, note, by string) (Artifact, error)

	CreateRollout(ctx context.Context, r NewRollout) (Rollout, error)
	RolloutByID(ctx context.Context, id string) (Rollout, error)
	ListRollouts(ctx context.Context, limit int) ([]Rollout, error) // newest first
	SetRolloutPaused(ctx context.Context, id string, paused bool, by string) (Rollout, error)

	// CreateTarget opens a target for one device. An open target for the
	// same device and product is marked superseded first; the new one gets
	// the next generation for that pair.
	CreateTarget(ctx context.Context, deviceID, product, artifactID, rolloutID string) (Target, error)
	OpenTarget(ctx context.Context, deviceID, product string) (Target, error)
	TargetByID(ctx context.Context, id string) (Target, error)
	TargetsByRollout(ctx context.Context, rolloutID string) ([]Target, error)
	OpenTargets(ctx context.Context) ([]Target, error)
	// FinishTarget moves a pending target to a terminal status. A target that
	// is not pending is left alone and ErrConflict is returned: a late
	// receipt must not rewrite a decision already recorded.
	FinishTarget(ctx context.Context, id string, status TargetStatus, note, reportedVersion string) (Target, error)
	CancelPending(ctx context.Context, rolloutID string) (int, error)
}
```

- [ ] **Step 1: 写迁移**

`go/db/migrations/0006_releases.up.sql`:

```sql
-- Release pipeline (spec 2026-09-18): what has been built, what has been
-- handed to which machine, and what came of it.

-- One row per package. (product, version) is unique and the sha256 is set
-- once: a version name refers to exactly one set of bytes, forever. A
-- rebuild with different content is a new version.
create table release_artifacts (
  id                uuid primary key default gen_random_uuid(),
  product           text not null check (product in ('agent', 'codex')),
  version           text not null check (version <> ''),
  sha256            text not null check (length(sha256) = 64),
  size_bytes        bigint not null check (size_bytes > 0),
  object_key        text not null check (object_key <> ''),
  status            text not null default 'candidate'
                    check (status in ('candidate', 'accepted', 'stable', 'retired')),
  notes             text not null default '',
  source            text not null default '',
  -- The oldest agent this package may be aimed at. For codex, the agent
  -- that installs it; for agent, the agent that replaces itself.
  min_agent_version text not null default '',
  acceptance_note   text not null default '',
  accepted_by       text not null default '',
  accepted_at       timestamptz,
  created_by        text not null default '',
  created_at        timestamptz not null default now(),
  updated_at        timestamptz not null default now(),
  unique (product, version)
);

-- One row per "hand this version to these machines" decision, including a
-- rollback, which is the same decision with an older version.
create table release_rollouts (
  id          uuid primary key default gen_random_uuid(),
  product     text not null check (product in ('agent', 'codex')),
  artifact_id uuid not null references release_artifacts(id),
  kind        text not null default 'release' check (kind in ('release', 'rollback')),
  rollback_of uuid references release_rollouts(id),
  note        text not null default '',
  created_by  text not null default '',
  created_at  timestamptz not null default now(),
  paused_at   timestamptz,
  paused_by   text not null default ''
);

-- Desired state, one row per device per product per decision. Not a task:
-- nothing on the server carries it out. The device pulls it and its report
-- settles it.
create table release_targets (
  id               uuid primary key default gen_random_uuid(),
  device_id        uuid not null references devices(id),
  product          text not null check (product in ('agent', 'codex')),
  artifact_id      uuid not null references release_artifacts(id),
  rollout_id       uuid not null references release_rollouts(id),
  -- Counts targets per (device, product). The agent echoes it back, so a
  -- receipt for an earlier generation cannot settle a later one.
  generation       integer not null check (generation > 0),
  status           text not null default 'pending'
                   check (status in ('pending', 'succeeded', 'failed', 'cancelled', 'superseded', 'excluded')),
  result_note      text not null default '',
  reported_version text not null default '',
  exclude_reason   text not null default '',
  created_at       timestamptz not null default now(),
  updated_at       timestamptz not null default now(),
  finished_at      timestamptz,
  unique (device_id, product, generation),
  constraint release_targets_finished_matches_status
    check ((status = 'pending') = (finished_at is null))
);
-- The rule itself: one open target per device and product.
create unique index release_targets_one_open
  on release_targets (device_id, product) where status = 'pending';
create index release_targets_by_rollout on release_targets (rollout_id);
```

`0006_releases.down.sql`:

```sql
drop table release_targets;
drop table release_rollouts;
drop table release_artifacts;
```

- [ ] **Step 2: 写仓储类型与接口**

新建 `go/internal/repo/releases.go`,内容即上面 Interfaces 块(加 `package repo` 与 `import ("context"; "time")`)。在 `repo.go` 的 `Store` 接口里加一行 `Releases() Releases`。

- [ ] **Step 3: 写失败测试**

`go/internal/dbstore/releases_test.go`:

```go
package dbstore

import (
	"errors"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestAVersionNamesOneSetOfBytes(t *testing.T) {
	s, ctx := newTestStore(t)
	a, err := s.Releases().CreateArtifact(ctx, repo.NewArtifact{
		Product: repo.ProductCodex, Version: "0.42.0", SHA256: strings.Repeat("a", 64),
		SizeBytes: 700 << 20, ObjectKey: "agent_workdir/_codex/codex-setup-0.42.0.exe", CreatedBy: "admin",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.Status != repo.ArtifactCandidate {
		t.Fatalf("a new artifact is %q, want candidate", a.Status)
	}
	_, err = s.Releases().CreateArtifact(ctx, repo.NewArtifact{
		Product: repo.ProductCodex, Version: "0.42.0", SHA256: strings.Repeat("b", 64),
		SizeBytes: 1, ObjectKey: "x", CreatedBy: "admin",
	})
	if !errors.Is(err, repo.ErrDuplicate) {
		t.Fatalf("second package under the same version: err = %v, want ErrDuplicate", err)
	}
	// The same version of the other product is a different thing.
	if _, err := s.Releases().CreateArtifact(ctx, repo.NewArtifact{
		Product: repo.ProductAgent, Version: "0.42.0", SHA256: strings.Repeat("c", 64),
		SizeBytes: 1, ObjectKey: "y", CreatedBy: "admin",
	}); err != nil {
		t.Fatalf("agent 0.42.0 beside codex 0.42.0: %v", err)
	}
	got, err := s.Releases().ArtifactByVersion(ctx, repo.ProductCodex, "0.42.0")
	if err != nil || got.ID != a.ID {
		t.Fatalf("ByVersion = %+v, %v", got, err)
	}
	accepted, err := s.Releases().SetArtifactStatus(ctx, a.ID, repo.ArtifactAccepted, "tested on WIN-TEST-01", "admin")
	if err != nil || accepted.AcceptedAt == nil || accepted.AcceptedBy != "admin" || accepted.AcceptanceNote != "tested on WIN-TEST-01" {
		t.Fatalf("accept: %+v, %v", accepted, err)
	}
	list, err := s.Releases().ListArtifacts(ctx, repo.ProductCodex)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListArtifacts(codex) = %d, %v", len(list), err)
	}
}

func TestOneOpenTargetPerDeviceAndGenerationsClimb(t *testing.T) {
	s, ctx := newTestStore(t)
	device, _ := s.Devices().EnsureByHostname(ctx, "WIN-01")
	old := mustArtifact(t, s, repo.ProductCodex, "0.41.0")
	next := mustArtifact(t, s, repo.ProductCodex, "0.42.0")
	r1, _ := s.Releases().CreateRollout(ctx, repo.NewRollout{Product: repo.ProductCodex, ArtifactID: old.ID, Kind: repo.RolloutRelease, CreatedBy: "admin"})
	r2, _ := s.Releases().CreateRollout(ctx, repo.NewRollout{Product: repo.ProductCodex, ArtifactID: next.ID, Kind: repo.RolloutRelease, CreatedBy: "admin"})

	t1, err := s.Releases().CreateTarget(ctx, device.ID, repo.ProductCodex, old.ID, r1.ID)
	if err != nil || t1.Generation != 1 || t1.Status != repo.TargetPending {
		t.Fatalf("first target: %+v, %v", t1, err)
	}
	t2, err := s.Releases().CreateTarget(ctx, device.ID, repo.ProductCodex, next.ID, r2.ID)
	if err != nil || t2.Generation != 2 {
		t.Fatalf("second target: %+v, %v", t2, err)
	}
	was, _ := s.Releases().TargetByID(ctx, t1.ID)
	if was.Status != repo.TargetSuperseded || was.FinishedAt == nil {
		t.Fatalf("the first target should be superseded, is %q", was.Status)
	}
	open, err := s.Releases().OpenTarget(ctx, device.ID, repo.ProductCodex)
	if err != nil || open.ID != t2.ID {
		t.Fatalf("OpenTarget = %+v, %v", open, err)
	}
	// A receipt for the superseded generation cannot touch anything.
	if _, err := s.Releases().FinishTarget(ctx, t1.ID, repo.TargetSucceeded, "", "0.41.0"); !errors.Is(err, repo.ErrConflict) {
		t.Fatalf("finishing a superseded target: err = %v, want ErrConflict", err)
	}
	done, err := s.Releases().FinishTarget(ctx, t2.ID, repo.TargetSucceeded, "", "0.42.0")
	if err != nil || done.Status != repo.TargetSucceeded || done.ReportedVersion != "0.42.0" || done.FinishedAt == nil {
		t.Fatalf("finish: %+v, %v", done, err)
	}
	if _, err := s.Releases().OpenTarget(ctx, device.ID, repo.ProductCodex); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("after finishing there is no open target: err = %v", err)
	}
	// A new target after success starts generation 3, not 1.
	t3, _ := s.Releases().CreateTarget(ctx, device.ID, repo.ProductCodex, old.ID, r1.ID)
	if t3.Generation != 3 {
		t.Fatalf("generation after a finished pair = %d, want 3", t3.Generation)
	}
}

func TestCancelPendingLeavesFinishedTargetsAlone(t *testing.T) {
	s, ctx := newTestStore(t)
	a := mustArtifact(t, s, repo.ProductAgent, "1.2.16")
	r, _ := s.Releases().CreateRollout(ctx, repo.NewRollout{Product: repo.ProductAgent, ArtifactID: a.ID, Kind: repo.RolloutRelease, CreatedBy: "admin"})
	d1, _ := s.Devices().EnsureByHostname(ctx, "WIN-01")
	d2, _ := s.Devices().EnsureByHostname(ctx, "WIN-02")
	t1, _ := s.Releases().CreateTarget(ctx, d1.ID, repo.ProductAgent, a.ID, r.ID)
	s.Releases().CreateTarget(ctx, d2.ID, repo.ProductAgent, a.ID, r.ID)
	s.Releases().FinishTarget(ctx, t1.ID, repo.TargetSucceeded, "", "1.2.16")

	n, err := s.Releases().CancelPending(ctx, r.ID)
	if err != nil || n != 1 {
		t.Fatalf("CancelPending = %d, %v; want 1", n, err)
	}
	paused, err := s.Releases().SetRolloutPaused(ctx, r.ID, true, "admin")
	if err != nil || paused.PausedAt == nil || paused.PausedBy != "admin" {
		t.Fatalf("pause: %+v, %v", paused, err)
	}
	resumed, _ := s.Releases().SetRolloutPaused(ctx, r.ID, false, "admin")
	if resumed.PausedAt != nil {
		t.Fatal("resume must clear paused_at")
	}
	targets, _ := s.Releases().TargetsByRollout(ctx, r.ID)
	if len(targets) != 2 {
		t.Fatalf("TargetsByRollout = %d rows", len(targets))
	}
}

func mustArtifact(t *testing.T, s *Store, product, version string) repo.Artifact {
	t.Helper()
	a, err := s.Releases().CreateArtifact(t.Context(), repo.NewArtifact{
		Product: product, Version: version, SHA256: strings.Repeat("0", 64),
		SizeBytes: 1, ObjectKey: "k/" + product + "/" + version, CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("artifact %s %s: %v", product, version, err)
	}
	return a
}
```

- [ ] **Step 4: 跑测试确认失败**

Run: `cd go && TEST_PG_DSN='postgres://postgres:devpass@127.0.0.1:5433/aienv_test?sslmode=disable' go test ./internal/dbstore/ -run 'TestAVersionNames|TestOneOpenTarget|TestCancelPending'`
Expected: FAIL,`s.Releases undefined`

- [ ] **Step 5: 实现仓储**

`go/internal/dbstore/store.go` 加 `func (s *Store) Releases() repo.Releases { return releaseRepo{s.q} }`。

`go/internal/dbstore/releases.go`:

```go
package dbstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type releaseRepo struct{ q querier }

const artifactColumns = `id, product, version, sha256, size_bytes, object_key, status, notes, source,
	min_agent_version, acceptance_note, accepted_by, accepted_at, created_by, created_at, updated_at`

func scanArtifact(row scanner) (repo.Artifact, error) {
	var a repo.Artifact
	err := row.Scan(&a.ID, &a.Product, &a.Version, &a.SHA256, &a.SizeBytes, &a.ObjectKey, &a.Status,
		&a.Notes, &a.Source, &a.MinAgentVersion, &a.AcceptanceNote, &a.AcceptedBy, &a.AcceptedAt,
		&a.CreatedBy, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

func (r releaseRepo) CreateArtifact(ctx context.Context, n repo.NewArtifact) (repo.Artifact, error) {
	a, err := scanArtifact(r.q.QueryRow(ctx,
		`insert into release_artifacts
		   (product, version, sha256, size_bytes, object_key, notes, source, min_agent_version, created_by)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 returning `+artifactColumns,
		n.Product, n.Version, n.SHA256, n.SizeBytes, n.ObjectKey, n.Notes, n.Source, n.MinAgentVersion, n.CreatedBy))
	if err != nil {
		return repo.Artifact{}, mapError(err, "register artifact")
	}
	return a, nil
}

func (r releaseRepo) ArtifactByID(ctx context.Context, id string) (repo.Artifact, error) {
	a, err := scanArtifact(r.q.QueryRow(ctx, `select `+artifactColumns+` from release_artifacts where id = $1`, id))
	if err != nil {
		return repo.Artifact{}, mapError(err, "read artifact")
	}
	return a, nil
}

func (r releaseRepo) ArtifactByVersion(ctx context.Context, product, version string) (repo.Artifact, error) {
	a, err := scanArtifact(r.q.QueryRow(ctx,
		`select `+artifactColumns+` from release_artifacts where product = $1 and version = $2`, product, version))
	if err != nil {
		return repo.Artifact{}, mapError(err, "read artifact")
	}
	return a, nil
}

func (r releaseRepo) ListArtifacts(ctx context.Context, product string) ([]repo.Artifact, error) {
	rows, err := r.q.Query(ctx,
		`select `+artifactColumns+` from release_artifacts
		  where $1 = '' or product = $1
		  order by created_at desc`, product)
	if err != nil {
		return nil, mapError(err, "list artifacts")
	}
	defer rows.Close()
	var out []repo.Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, mapError(err, "list artifacts")
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r releaseRepo) SetArtifactStatus(ctx context.Context, id string, status repo.ArtifactStatus, note, by string) (repo.Artifact, error) {
	a, err := scanArtifact(r.q.QueryRow(ctx,
		`update release_artifacts set
		   status = $2,
		   acceptance_note = case when $2 in ('accepted', 'stable') then $3 else acceptance_note end,
		   accepted_by     = case when $2 in ('accepted', 'stable') and accepted_at is null then $4 else accepted_by end,
		   accepted_at     = case when $2 in ('accepted', 'stable') and accepted_at is null then now() else accepted_at end,
		   updated_at = now()
		 where id = $1
		 returning `+artifactColumns, id, string(status), note, by))
	if err != nil {
		return repo.Artifact{}, mapError(err, "set artifact status")
	}
	return a, nil
}

const rolloutColumns = `id, product, artifact_id, kind, coalesce(rollback_of::text, ''), note, created_by, created_at, paused_at, paused_by`

func scanRollout(row scanner) (repo.Rollout, error) {
	var ro repo.Rollout
	err := row.Scan(&ro.ID, &ro.Product, &ro.ArtifactID, &ro.Kind, &ro.RollbackOf, &ro.Note, &ro.CreatedBy,
		&ro.CreatedAt, &ro.PausedAt, &ro.PausedBy)
	return ro, err
}

func (r releaseRepo) CreateRollout(ctx context.Context, n repo.NewRollout) (repo.Rollout, error) {
	var rollbackOf *string
	if n.RollbackOf != "" {
		rollbackOf = &n.RollbackOf
	}
	ro, err := scanRollout(r.q.QueryRow(ctx,
		`insert into release_rollouts (product, artifact_id, kind, rollback_of, note, created_by)
		 values ($1, $2, $3, $4, $5, $6) returning `+rolloutColumns,
		n.Product, n.ArtifactID, string(n.Kind), rollbackOf, n.Note, n.CreatedBy))
	if err != nil {
		return repo.Rollout{}, mapError(err, "create rollout")
	}
	return ro, nil
}

func (r releaseRepo) RolloutByID(ctx context.Context, id string) (repo.Rollout, error) {
	ro, err := scanRollout(r.q.QueryRow(ctx, `select `+rolloutColumns+` from release_rollouts where id = $1`, id))
	if err != nil {
		return repo.Rollout{}, mapError(err, "read rollout")
	}
	return ro, nil
}

func (r releaseRepo) ListRollouts(ctx context.Context, limit int) ([]repo.Rollout, error) {
	rows, err := r.q.Query(ctx, `select `+rolloutColumns+` from release_rollouts order by created_at desc limit $1`, limit)
	if err != nil {
		return nil, mapError(err, "list rollouts")
	}
	defer rows.Close()
	var out []repo.Rollout
	for rows.Next() {
		ro, err := scanRollout(rows)
		if err != nil {
			return nil, mapError(err, "list rollouts")
		}
		out = append(out, ro)
	}
	return out, rows.Err()
}

func (r releaseRepo) SetRolloutPaused(ctx context.Context, id string, paused bool, by string) (repo.Rollout, error) {
	ro, err := scanRollout(r.q.QueryRow(ctx,
		`update release_rollouts set
		   paused_at = case when $2 then now() else null end,
		   paused_by = case when $2 then $3 else '' end
		 where id = $1 returning `+rolloutColumns, id, paused, by))
	if err != nil {
		return repo.Rollout{}, mapError(err, "pause rollout")
	}
	return ro, nil
}

const targetColumns = `id, device_id, product, artifact_id, rollout_id, generation, status, result_note,
	reported_version, exclude_reason, created_at, updated_at, finished_at`

func scanTarget(row scanner) (repo.Target, error) {
	var t repo.Target
	err := row.Scan(&t.ID, &t.DeviceID, &t.Product, &t.ArtifactID, &t.RolloutID, &t.Generation, &t.Status,
		&t.ResultNote, &t.ReportedVersion, &t.ExcludeReason, &t.CreatedAt, &t.UpdatedAt, &t.FinishedAt)
	return t, err
}

// CreateTarget supersedes and inserts in one statement, so two administrators
// aiming at the same machine at once end with one open target, not two: the
// partial unique index rejects the loser and mapError reports ErrDuplicate.
func (r releaseRepo) CreateTarget(ctx context.Context, deviceID, product, artifactID, rolloutID string) (repo.Target, error) {
	t, err := scanTarget(r.q.QueryRow(ctx,
		`with closed as (
		   update release_targets
		      set status = 'superseded', finished_at = now(), updated_at = now(),
		          result_note = 'replaced by a newer target'
		    where device_id = $1 and product = $2 and status = 'pending'
		 ), gen as (
		   select coalesce(max(generation), 0) + 1 as next
		     from release_targets where device_id = $1 and product = $2
		 )
		 insert into release_targets (device_id, product, artifact_id, rollout_id, generation)
		 select $1, $2, $3, $4, next from gen
		 returning `+targetColumns, deviceID, product, artifactID, rolloutID))
	if err != nil {
		return repo.Target{}, mapError(err, "create target")
	}
	return t, nil
}

func (r releaseRepo) OpenTarget(ctx context.Context, deviceID, product string) (repo.Target, error) {
	t, err := scanTarget(r.q.QueryRow(ctx,
		`select `+targetColumns+` from release_targets
		  where device_id = $1 and product = $2 and status = 'pending'`, deviceID, product))
	if err != nil {
		return repo.Target{}, mapError(err, "read open target")
	}
	return t, nil
}

func (r releaseRepo) TargetByID(ctx context.Context, id string) (repo.Target, error) {
	t, err := scanTarget(r.q.QueryRow(ctx, `select `+targetColumns+` from release_targets where id = $1`, id))
	if err != nil {
		return repo.Target{}, mapError(err, "read target")
	}
	return t, nil
}

func (r releaseRepo) TargetsByRollout(ctx context.Context, rolloutID string) ([]repo.Target, error) {
	return r.listTargets(ctx, `rollout_id = $1`, rolloutID)
}

func (r releaseRepo) OpenTargets(ctx context.Context) ([]repo.Target, error) {
	return r.listTargets(ctx, `status = 'pending' and $1 = ''`, "")
}

func (r releaseRepo) listTargets(ctx context.Context, where string, arg any) ([]repo.Target, error) {
	rows, err := r.q.Query(ctx, `select `+targetColumns+` from release_targets where `+where+` order by created_at`, arg)
	if err != nil {
		return nil, mapError(err, "list targets")
	}
	defer rows.Close()
	var out []repo.Target
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, mapError(err, "list targets")
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r releaseRepo) FinishTarget(ctx context.Context, id string, status repo.TargetStatus, note, reportedVersion string) (repo.Target, error) {
	if status == repo.TargetPending {
		return repo.Target{}, errors.New("finish target: pending is not a terminal status")
	}
	t, err := scanTarget(r.q.QueryRow(ctx,
		`update release_targets
		    set status = $2, result_note = $3, reported_version = $4,
		        exclude_reason = case when $2 = 'excluded' then $3 else exclude_reason end,
		        finished_at = now(), updated_at = now()
		  where id = $1 and status = 'pending'
		  returning `+targetColumns, id, string(status), note, reportedVersion))
	if errors.Is(mapError(err, ""), repo.ErrNotFound) {
		// Either it does not exist or it is already settled; both mean the
		// caller's picture of it is stale.
		if _, lookup := r.TargetByID(ctx, id); lookup == nil {
			return repo.Target{}, fmt.Errorf("finish target: already settled: %w", repo.ErrConflict)
		}
		return repo.Target{}, mapError(err, "finish target")
	}
	if err != nil {
		return repo.Target{}, mapError(err, "finish target")
	}
	return t, nil
}

func (r releaseRepo) CancelPending(ctx context.Context, rolloutID string) (int, error) {
	tag, err := r.q.Exec(ctx,
		`update release_targets
		    set status = 'cancelled', result_note = 'cancelled before the machine started',
		        finished_at = now(), updated_at = now()
		  where rollout_id = $1 and status = 'pending'`, rolloutID)
	if err != nil {
		return 0, mapError(err, "cancel pending targets")
	}
	return int(tag.RowsAffected()), nil
}
```

- [ ] **Step 6: 跑测试确认通过**

Run: `cd go && TEST_PG_DSN='postgres://postgres:devpass@127.0.0.1:5433/aienv_test?sslmode=disable' go test ./internal/dbstore/`
Expected: PASS(含迁移校验测试:0006 有 up/down、校验和写入)

- [ ] **Step 7: 确认授权网格覆盖新表**

Run: `cd go && TEST_PG_DSN='postgres://postgres:devpass@127.0.0.1:5433/aienv_test?sslmode=disable' go test ./internal/dbstore/ -run TestGrants -v`
Expected: PASS(`grants.sql` 用 `grant ... on all tables in schema public`,迁移后重放,新表自动纳入;如该测试名不同,以 `db_test.go` 里验证 `aienv_app` 能读新建表的那个为准)

- [ ] **Step 8: 提交**

```bash
cd go && git add db/migrations/0006_releases.up.sql db/migrations/0006_releases.down.sql internal/repo/releases.go internal/repo/repo.go internal/dbstore/releases.go internal/dbstore/releases_test.go internal/dbstore/store.go
git commit -m "Release artifacts, rollouts and per-device targets

Three tables: what was built, the decision to hand it to some machines,
and one desired-state row per machine. Targets are not tasks; nothing on
the server carries them out.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: ops:登记制品、全局目标、发布任务

**Files:**
- Create: `go/internal/ops/releases.go`, `go/internal/ops/releases_test.go`
- Modify: `go/internal/ops/ops.go:44-56`(审计动作常量)

**Interfaces:**
- Consumes: Task 3 的 `repo.Releases`;`model.AgentCanTakeTargets`;现有 `s.mutatePolicy`、`s.enqueueDeviceExport`、`s.auditTarget`。
- Produces:

```go
func (s *Service) RegisterArtifact(ctx context.Context, a repo.NewArtifact, actor, requestID string) (repo.Artifact, error)
func (s *Service) SetArtifactStatus(ctx context.Context, id string, status repo.ArtifactStatus, note, actor, requestID string) (repo.Artifact, error)
func (s *Service) SetGlobalTarget(ctx context.Context, product, version, actor, requestID string) (model.Policy, error)
func (s *Service) ClearGlobalTarget(ctx context.Context, product, actor, requestID string) (model.Policy, error)
type RolloutSpec struct {
	Product, ArtifactID string
	DeviceIDs           []string
	Kind                repo.RolloutKind
	RollbackOf          string
	Note                string
	Actor, RequestID    string
}
func (s *Service) CreateRollout(ctx context.Context, spec RolloutSpec) (repo.Rollout, error)
func (s *Service) SetRolloutPaused(ctx context.Context, rolloutID string, paused bool, actor, requestID string) error
func (s *Service) CancelRollout(ctx context.Context, rolloutID, actor, requestID string) (int, error)
func (s *Service) ExcludeTarget(ctx context.Context, targetID, reason, actor, requestID string) error
func (s *Service) RetryTarget(ctx context.Context, targetID, actor, requestID string) (repo.Target, error)
```

- [ ] **Step 1: 写失败测试**

`go/internal/ops/releases_test.go`(`newService`、`openTasks` 在 `ops_test.go` 里已有):

```go
package ops

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func newArtifact(product, version string) repo.NewArtifact {
	return repo.NewArtifact{Product: product, Version: version, SHA256: strings.Repeat("a", 64),
		SizeBytes: 1, ObjectKey: "k/" + version, CreatedBy: "admin"}
}

func TestRegisteringAnArtifactChangesNoTarget(t *testing.T) {
	svc, store, ctx := newService(t)
	a, err := svc.RegisterArtifact(ctx, newArtifact(repo.ProductAgent, "1.2.16"), "admin", "r1")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// Same bytes again is the same artifact, not an error: a retried upload
	// must land on the row it already made.
	again, err := svc.RegisterArtifact(ctx, newArtifact(repo.ProductAgent, "1.2.16"), "admin", "r2")
	if err != nil || again.ID != a.ID {
		t.Fatalf("re-register: %+v, %v", again, err)
	}
	other := newArtifact(repo.ProductAgent, "1.2.16")
	other.SHA256 = strings.Repeat("b", 64)
	if _, err := svc.RegisterArtifact(ctx, other, "admin", "r3"); err == nil || !strings.Contains(err.Error(), "different") {
		t.Fatalf("different bytes under a used version: err = %v", err)
	}
	pol, _, _ := svc.CurrentPolicy(ctx)
	if pol.AgentUpdateVersion != "" {
		t.Fatal("registering must not aim the fleet at anything")
	}
	if tasks := openTasks(t, ctx, store); len(tasks) != 0 {
		t.Fatalf("registering queued %d task(s); it should queue none", len(tasks))
	}
}

func TestSettingTheGlobalTargetTakesTheArtifactsChecksum(t *testing.T) {
	svc, store, ctx := newService(t)
	a, _ := svc.RegisterArtifact(ctx, newArtifact(repo.ProductCodex, "0.42.0"), "admin", "r1")
	if _, err := svc.SetGlobalTarget(ctx, repo.ProductCodex, "0.99.0", "admin", "r2"); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("aiming at an unregistered version: err = %v", err)
	}
	pol, err := svc.SetGlobalTarget(ctx, repo.ProductCodex, "0.42.0", "admin", "r3")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if pol.CodexVersion != "0.42.0" || pol.CodexSHA256 != a.SHA256 || pol.CodexKey != a.ObjectKey || pol.CodexRolloutPct != 100 {
		t.Fatalf("policy after set: %+v", pol)
	}
	if tasks := openTasks(t, ctx, store); len(tasks) != 1 || tasks[0].Kind != repo.TaskOSSExport {
		t.Fatalf("a policy change must queue exactly one export, got %d", len(tasks))
	}
	svc.SetArtifactStatus(ctx, a.ID, repo.ArtifactRetired, "", "admin", "r4")
	if _, err := svc.SetGlobalTarget(ctx, repo.ProductCodex, "0.42.0", "admin", "r5"); err == nil {
		t.Fatal("a retired artifact must not be aimed at anyone")
	}
	pol, _ = svc.ClearGlobalTarget(ctx, repo.ProductCodex, "admin", "r6")
	if pol.CodexVersion != "" || pol.CodexKey != "" {
		t.Fatalf("clear left %+v", pol)
	}
}

func TestARolloutAimsOnlyAtTheChosenMachines(t *testing.T) {
	svc, store, ctx := newService(t)
	a, _ := svc.RegisterArtifact(ctx, newArtifact(repo.ProductCodex, "0.42.0"), "admin", "r1")
	win1, _ := store.Devices().EnsureByHostname(ctx, "WIN-01")
	win2, _ := store.Devices().EnsureByHostname(ctx, "WIN-02")
	old, _ := store.Devices().EnsureByHostname(ctx, "WIN-OLD")
	now := time.Now()
	store.Devices().MarkSeen(ctx, win1.ID, "1.2.16", now)
	store.Devices().MarkSeen(ctx, win2.ID, "1.2.16", now)
	store.Devices().MarkSeen(ctx, old.ID, "1.2.15", now)

	_, err := svc.CreateRollout(ctx, RolloutSpec{Product: repo.ProductCodex, ArtifactID: a.ID,
		DeviceIDs: []string{win1.ID, old.ID}, Kind: repo.RolloutRelease, Actor: "admin", RequestID: "r2"})
	if err == nil || !strings.Contains(err.Error(), "WIN-OLD") {
		t.Fatalf("a machine on 1.2.15 cannot read a target; err = %v", err)
	}
	if n := len(openTasks(t, ctx, store)); n != 0 {
		t.Fatalf("a refused rollout must leave nothing behind, found %d task(s)", n)
	}

	r, err := svc.CreateRollout(ctx, RolloutSpec{Product: repo.ProductCodex, ArtifactID: a.ID,
		DeviceIDs: []string{win1.ID}, Kind: repo.RolloutRelease, Note: "试发", Actor: "admin", RequestID: "r3"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.Releases().OpenTarget(ctx, win1.ID, repo.ProductCodex); err != nil {
		t.Fatalf("WIN-01 has no open target: %v", err)
	}
	if _, err := store.Releases().OpenTarget(ctx, win2.ID, repo.ProductCodex); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("WIN-02 was not chosen and must have no target, err = %v", err)
	}
	tasks := openTasks(t, ctx, store)
	if len(tasks) != 1 || tasks[0].DeviceID != win1.ID {
		t.Fatalf("want one binding export for WIN-01, got %+v", tasks)
	}
	pol, _, _ := svc.CurrentPolicy(ctx)
	if pol.CodexVersion != "" {
		t.Fatal("a targeted rollout must not touch the fleet policy")
	}

	// Pause and resume each re-export the pending machines.
	if err := svc.SetRolloutPaused(ctx, r.ID, true, "admin", "r4"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got, _ := store.Releases().RolloutByID(ctx, r.ID); got.PausedAt == nil {
		t.Fatal("not paused")
	}
	if n := len(openTasks(t, ctx, store)); n != 2 {
		t.Fatalf("pausing must queue a fresh export, tasks = %d", n)
	}
}

func TestExcludeAndRetryAreExplicitAndRecorded(t *testing.T) {
	svc, store, ctx := newService(t)
	a, _ := svc.RegisterArtifact(ctx, newArtifact(repo.ProductAgent, "1.2.17"), "admin", "r1")
	win1, _ := store.Devices().EnsureByHostname(ctx, "WIN-01")
	store.Devices().MarkSeen(ctx, win1.ID, "1.2.16", time.Now())
	r, _ := svc.CreateRollout(ctx, RolloutSpec{Product: repo.ProductAgent, ArtifactID: a.ID,
		DeviceIDs: []string{win1.ID}, Kind: repo.RolloutRelease, Actor: "admin", RequestID: "r2"})
	target, _ := store.Releases().OpenTarget(ctx, win1.ID, repo.ProductAgent)

	if err := svc.ExcludeTarget(ctx, target.ID, "", "admin", "r3"); err == nil {
		t.Fatal("excluding needs a reason")
	}
	if err := svc.ExcludeTarget(ctx, target.ID, "机器已停用", "admin", "r4"); err != nil {
		t.Fatalf("exclude: %v", err)
	}
	got, _ := store.Releases().TargetByID(ctx, target.ID)
	if got.Status != repo.TargetExcluded || got.ExcludeReason != "机器已停用" {
		t.Fatalf("after exclude: %+v", got)
	}

	// Retry is only for a failed target, and makes a new generation.
	if _, err := svc.RetryTarget(ctx, target.ID, "admin", "r5"); err == nil {
		t.Fatal("an excluded target is not retried")
	}
	t2, _ := store.Releases().CreateTarget(ctx, win1.ID, repo.ProductAgent, a.ID, r.ID)
	store.Releases().FinishTarget(ctx, t2.ID, repo.TargetFailed, "checksum mismatch", "")
	t3, err := svc.RetryTarget(ctx, t2.ID, "admin", "r6")
	if err != nil || t3.Generation != t2.Generation+1 || t3.RolloutID != r.ID {
		t.Fatalf("retry: %+v, %v", t3, err)
	}
	events, _ := store.Audit().ByTarget(ctx, "device", win1.ID, 10)
	var actions []string
	for _, ev := range events {
		actions = append(actions, ev.Action)
	}
	for _, want := range []string{ActionTargetExclude, ActionTargetRetry} {
		found := false
		for _, a := range actions {
			found = found || a == want
		}
		if !found {
			t.Fatalf("audit has %v, missing %s", actions, want)
		}
	}
}

func TestAgentCanTakeTargetsIsWhatTheRolloutChecks(t *testing.T) {
	if !model.AgentCanTakeTargets("1.2.16") {
		t.Fatal("the floor moved; update the rollout check and this test together")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd go && TEST_PG_DSN='postgres://postgres:devpass@127.0.0.1:5433/aienv_test?sslmode=disable' go test ./internal/ops/ -run 'Artifact|Rollout|ExcludeAndRetry'`
Expected: FAIL,`svc.RegisterArtifact undefined`

- [ ] **Step 3: 实现**

在 `ops.go` 的动作常量里加:

```go
	ActionArtifactRegister = "release.artifact_register"
	ActionArtifactStatus   = "release.artifact_status"
	ActionGlobalTarget     = "release.global_target"
	ActionRolloutCreate    = "release.rollout_create"
	ActionRolloutPause     = "release.rollout_pause"
	ActionRolloutCancel    = "release.rollout_cancel"
	ActionTargetExclude    = "release.target_exclude"
	ActionTargetRetry      = "release.target_retry"
```

`go/internal/ops/releases.go`:

```go
package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// RegisterArtifact records a package that is already in the bucket. It aims
// nothing at anybody: a candidate is a row, not a rollout.
//
// The same bytes under the same version is the same artifact, returned as it
// is, so a retried upload converges. Different bytes under a used version is
// refused: the version name is the promise.
func (s *Service) RegisterArtifact(ctx context.Context, a repo.NewArtifact, actor, requestID string) (repo.Artifact, error) {
	if a.Product != repo.ProductAgent && a.Product != repo.ProductCodex {
		return repo.Artifact{}, fmt.Errorf("unknown product %q", a.Product)
	}
	if strings.TrimSpace(a.Version) == "" || len(a.SHA256) != 64 || a.SizeBytes <= 0 || a.ObjectKey == "" {
		return repo.Artifact{}, errors.New("an artifact needs a version, its SHA-256, its size and its object key")
	}
	var result repo.Artifact
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		existing, err := tx.Releases().ArtifactByVersion(ctx, a.Product, a.Version)
		switch {
		case err == nil:
			if !strings.EqualFold(existing.SHA256, a.SHA256) {
				return fmt.Errorf("%s %s already names a different package (sha256 %s…); a rebuild needs a new version",
					a.Product, a.Version, existing.SHA256[:12])
			}
			result = existing
			return nil
		case !errors.Is(err, repo.ErrNotFound):
			return err
		}
		created, err := tx.Releases().CreateArtifact(ctx, a)
		if err != nil {
			return err
		}
		result = created
		return s.auditTarget(ctx, tx, actor, requestID, ActionArtifactRegister, "artifact", created.ID, nil,
			map[string]any{"product": a.Product, "version": a.Version, "sha256": a.SHA256, "size": a.SizeBytes, "source": a.Source})
	})
	if err != nil {
		return repo.Artifact{}, fmt.Errorf("register %s %s: %w", a.Product, a.Version, err)
	}
	return result, nil
}

// SetArtifactStatus records acceptance, promotion to stable, or retirement.
func (s *Service) SetArtifactStatus(ctx context.Context, id string, status repo.ArtifactStatus, note, actor, requestID string) (repo.Artifact, error) {
	var result repo.Artifact
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		before, err := tx.Releases().ArtifactByID(ctx, id)
		if err != nil {
			return err
		}
		after, err := tx.Releases().SetArtifactStatus(ctx, id, status, note, actor)
		if err != nil {
			return err
		}
		result = after
		return s.auditTarget(ctx, tx, actor, requestID, ActionArtifactStatus, "artifact", id,
			map[string]any{"status": before.Status}, map[string]any{"status": after.Status, "note": note})
	})
	if err != nil {
		return repo.Artifact{}, fmt.Errorf("set artifact status: %w", err)
	}
	return result, nil
}

// SetGlobalTarget points every machine that reads the fleet policy at a
// registered artifact. This is the old channel, and stays the only channel
// for agents older than 1.2.16. The checksum and key come from the artifact
// row, never from the form.
func (s *Service) SetGlobalTarget(ctx context.Context, product, version, actor, requestID string) (model.Policy, error) {
	artifact, err := s.store.Releases().ArtifactByVersion(ctx, product, version)
	if err != nil {
		return model.Policy{}, fmt.Errorf("set global %s target: %w", product, err)
	}
	if artifact.Status == repo.ArtifactRetired {
		return model.Policy{}, fmt.Errorf("set global %s target: %s is retired", product, version)
	}
	pol, err := s.mutatePolicy(ctx, "set global "+product+" target", actor, requestID, func(p *model.Policy) error {
		switch product {
		case repo.ProductAgent:
			p.AgentUpdateVersion, p.AgentUpdateSHA256 = artifact.Version, artifact.SHA256
		case repo.ProductCodex:
			p.CodexVersion, p.CodexSHA256, p.CodexKey = artifact.Version, artifact.SHA256, artifact.ObjectKey
			p.CodexRolloutPct = 100 // agents 1.2.5-1.2.7 gate on it; see SetCodexUpdate
		default:
			return fmt.Errorf("unknown product %q", product)
		}
		return nil
	})
	if err != nil {
		return model.Policy{}, err
	}
	return pol, nil
}

// ClearGlobalTarget is the kill switch for the old channel.
func (s *Service) ClearGlobalTarget(ctx context.Context, product, actor, requestID string) (model.Policy, error) {
	return s.mutatePolicy(ctx, "clear global "+product+" target", actor, requestID, func(p *model.Policy) error {
		switch product {
		case repo.ProductAgent:
			p.AgentUpdateVersion, p.AgentUpdateSHA256 = "", ""
		case repo.ProductCodex:
			p.CodexVersion, p.CodexSHA256, p.CodexKey, p.CodexRolloutPct = "", "", "", 0
		default:
			return fmt.Errorf("unknown product %q", product)
		}
		return nil
	})
}

// RolloutSpec is one decision: this artifact, these machines.
type RolloutSpec struct {
	Product, ArtifactID string
	DeviceIDs           []string
	Kind                repo.RolloutKind
	RollbackOf          string
	Note                string
	Actor, RequestID    string
}

// CreateRollout opens one target per chosen machine and queues each machine's
// binding export. It is refused whole if any machine cannot take a target:
// a rollout that silently skipped a machine would be reported as complete
// with that machine never updated.
func (s *Service) CreateRollout(ctx context.Context, spec RolloutSpec) (repo.Rollout, error) {
	if len(spec.DeviceIDs) == 0 {
		return repo.Rollout{}, errors.New("choose at least one machine")
	}
	if spec.Kind == "" {
		spec.Kind = repo.RolloutRelease
	}
	var result repo.Rollout
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		artifact, err := tx.Releases().ArtifactByID(ctx, spec.ArtifactID)
		if err != nil {
			return err
		}
		if artifact.Product != spec.Product {
			return fmt.Errorf("artifact %s is a %s package, not %s", artifact.Version, artifact.Product, spec.Product)
		}
		if artifact.Status == repo.ArtifactRetired {
			return fmt.Errorf("%s %s is retired", artifact.Product, artifact.Version)
		}
		rollout, err := tx.Releases().CreateRollout(ctx, repo.NewRollout{
			Product: spec.Product, ArtifactID: artifact.ID, Kind: spec.Kind,
			RollbackOf: spec.RollbackOf, Note: spec.Note, CreatedBy: spec.Actor,
		})
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, id := range spec.DeviceIDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			device, err := tx.Devices().ByID(ctx, id)
			if err != nil {
				return err
			}
			if device.Status == repo.DeviceRevoked {
				return fmt.Errorf("%s is revoked", device.Hostname)
			}
			if !model.AgentCanTakeTargets(device.AgentVersion) {
				return fmt.Errorf("%s runs agent %q, which only reads the fleet policy; targets need %s or later",
					device.Hostname, device.AgentVersion, model.MinTargetAgentVersion)
			}
			if artifact.MinAgentVersion != "" && model.CompareVersions(device.AgentVersion, artifact.MinAgentVersion) < 0 {
				return fmt.Errorf("%s runs agent %s; %s %s needs %s or later",
					device.Hostname, device.AgentVersion, artifact.Product, artifact.Version, artifact.MinAgentVersion)
			}
			target, err := tx.Releases().CreateTarget(ctx, device.ID, spec.Product, artifact.ID, rollout.ID)
			if err != nil {
				return err
			}
			if err := s.enqueueDeviceExport(ctx, tx, device, "target:"+target.ID); err != nil {
				return err
			}
			if err := s.auditTarget(ctx, tx, spec.Actor, spec.RequestID, ActionRolloutCreate, "device", device.ID, nil,
				map[string]any{"rollout": rollout.ID, "product": spec.Product, "version": artifact.Version,
					"generation": target.Generation, "kind": spec.Kind}); err != nil {
				return err
			}
		}
		result = rollout
		return nil
	})
	if err != nil {
		return repo.Rollout{}, fmt.Errorf("create rollout: %w", err)
	}
	return result, nil
}

// SetRolloutPaused stops, or resumes, machines that have not started. The
// export omits the target of a paused rollout, so a machine that reads its
// binding sees nothing to do; a machine already installing carries on and
// reports.
func (s *Service) SetRolloutPaused(ctx context.Context, rolloutID string, paused bool, actor, requestID string) error {
	action := ActionRolloutPause
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		rollout, err := tx.Releases().SetRolloutPaused(ctx, rolloutID, paused, actor)
		if err != nil {
			return err
		}
		if err := s.reexportPending(ctx, tx, rollout.ID, fmt.Sprintf("pause:%s:%t:%d", rollout.ID, paused, s.now().UnixNano())); err != nil {
			return err
		}
		return s.auditTarget(ctx, tx, actor, requestID, action, "rollout", rollout.ID, nil,
			map[string]any{"paused": paused})
	})
	if err != nil {
		return fmt.Errorf("pause rollout: %w", err)
	}
	return nil
}

// CancelRollout closes every pending target. Finished ones keep their result.
func (s *Service) CancelRollout(ctx context.Context, rolloutID, actor, requestID string) (int, error) {
	var n int
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		targets, err := tx.Releases().TargetsByRollout(ctx, rolloutID)
		if err != nil {
			return err
		}
		n, err = tx.Releases().CancelPending(ctx, rolloutID)
		if err != nil {
			return err
		}
		for _, t := range targets {
			if t.Status != repo.TargetPending {
				continue
			}
			device, err := tx.Devices().ByID(ctx, t.DeviceID)
			if err != nil {
				return err
			}
			if err := s.enqueueDeviceExport(ctx, tx, device, "cancel:"+t.ID); err != nil {
				return err
			}
		}
		return s.auditTarget(ctx, tx, actor, requestID, ActionRolloutCancel, "rollout", rolloutID, nil,
			map[string]any{"cancelled": n})
	})
	if err != nil {
		return 0, fmt.Errorf("cancel rollout: %w", err)
	}
	return n, nil
}

// ExcludeTarget takes one machine out of a rollout, with a reason that goes
// into the report: excluded is neither success nor failure.
func (s *Service) ExcludeTarget(ctx context.Context, targetID, reason, actor, requestID string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("excluding a machine needs a reason")
	}
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		target, err := tx.Releases().FinishTarget(ctx, targetID, repo.TargetExcluded, reason, "")
		if err != nil {
			return err
		}
		device, err := tx.Devices().ByID(ctx, target.DeviceID)
		if err != nil {
			return err
		}
		if err := s.enqueueDeviceExport(ctx, tx, device, "exclude:"+target.ID); err != nil {
			return err
		}
		return s.auditTarget(ctx, tx, actor, requestID, ActionTargetExclude, "device", device.ID, nil,
			map[string]any{"rollout": target.RolloutID, "target": target.ID, "reason": reason})
	})
	if err != nil {
		return fmt.Errorf("exclude machine: %w", err)
	}
	return nil
}

// RetryTarget opens a new generation for a machine whose target failed. The
// agent keys its one-attempt marker on version and generation, so a new
// generation is what makes it try the same version again.
func (s *Service) RetryTarget(ctx context.Context, targetID, actor, requestID string) (repo.Target, error) {
	var result repo.Target
	err := s.store.InTx(ctx, func(tx repo.Store) error {
		failed, err := tx.Releases().TargetByID(ctx, targetID)
		if err != nil {
			return err
		}
		if failed.Status != repo.TargetFailed {
			return fmt.Errorf("only a failed target is retried; this one is %s", failed.Status)
		}
		device, err := tx.Devices().ByID(ctx, failed.DeviceID)
		if err != nil {
			return err
		}
		next, err := tx.Releases().CreateTarget(ctx, device.ID, failed.Product, failed.ArtifactID, failed.RolloutID)
		if err != nil {
			return err
		}
		if err := s.enqueueDeviceExport(ctx, tx, device, "target:"+next.ID); err != nil {
			return err
		}
		result = next
		return s.auditTarget(ctx, tx, actor, requestID, ActionTargetRetry, "device", device.ID,
			map[string]any{"target": failed.ID, "generation": failed.Generation, "error": failed.ResultNote},
			map[string]any{"target": next.ID, "generation": next.Generation})
	})
	if err != nil {
		return repo.Target{}, fmt.Errorf("retry target: %w", err)
	}
	return result, nil
}

// reexportPending queues a binding export for every machine still pending
// in a rollout.
func (s *Service) reexportPending(ctx context.Context, tx repo.Store, rolloutID, marker string) error {
	targets, err := tx.Releases().TargetsByRollout(ctx, rolloutID)
	if err != nil {
		return err
	}
	for _, t := range targets {
		if t.Status != repo.TargetPending {
			continue
		}
		device, err := tx.Devices().ByID(ctx, t.DeviceID)
		if err != nil {
			return err
		}
		if err := s.enqueueDeviceExport(ctx, tx, device, marker+":"+device.ID); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd go && TEST_PG_DSN='postgres://postgres:devpass@127.0.0.1:5433/aienv_test?sslmode=disable' go test ./internal/ops/`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
cd go && git add internal/ops/releases.go internal/ops/releases_test.go internal/ops/ops.go
git commit -m "ops: register artifacts, aim the fleet or chosen machines, pause, exclude, retry

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: Agent 1.2.16:按机目标、代次标记、延后原因、状态字段

**Files:**
- Modify: `go/internal/model/model.go`(`Binding`、`Status`), `go/internal/status/status.go`(`Report`、`Build`), `go/internal/agentcore/sync.go:232-245,416-427,470-510`, `go/internal/agentcore/codex.go`
- Test: `go/internal/agentcore/codex_test.go`, `go/internal/agentcore/sync_test.go`, `go/internal/agentcore/targets_test.go`(新)

**Interfaces:**
- Produces(Task 6、7 依赖):

```go
// model
type ReleaseTarget struct {
	Version    string `json:"version"`
	SHA256     string `json:"sha256"`
	Key        string `json:"key,omitempty"` // object key; empty = the product's default key
	Generation int    `json:"generation"`
}
// Binding 新增
AgentTarget *ReleaseTarget `json:"agentTarget,omitempty"`
CodexTarget *ReleaseTarget `json:"codexTarget,omitempty"`
// Status 新增
AgentUpdateTarget     string `json:"agentUpdateTarget,omitempty"`
AgentUpdateGeneration int    `json:"agentUpdateGeneration,omitempty"`
AgentUpdateState      string `json:"agentUpdateState,omitempty"` // "", "pending", "failed"
CodexTarget           string `json:"codexTarget,omitempty"`
CodexTargetGeneration int    `json:"codexTargetGeneration,omitempty"`
CodexDeferReason      string `json:"codexDeferReason,omitempty"` // "in_use", "disk"
```

- agentcore 常量:`AgentUpdatePending = "pending"`, `AgentUpdateFailed = "failed"`, `CodexDeferInUse = "in_use"`, `CodexDeferDisk = "disk"`。
- 标记文件格式改为 `<version>@<generation>`(`codex-target`、`update-target`)。

- [ ] **Step 1: 写目标解析的失败测试**

`go/internal/agentcore/targets_test.go`:

```go
package agentcore

import (
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

func TestTheBindingTargetReplacesTheFleetTarget(t *testing.T) {
	pol := model.Policy{
		AgentUpdateVersion: "1.2.17", AgentUpdateSHA256: "aaaa",
		CodexVersion: "0.42.0", CodexSHA256: "cccc", CodexKey: "agent_workdir/_codex/codex-setup-0.42.0.exe",
	}
	agent, codex := effectiveTargets(pol, model.Binding{})
	if agent.Version != "1.2.17" || agent.Key != "agent_workdir/_agent/agent.exe" || agent.Generation != 0 {
		t.Fatalf("fleet agent target = %+v", agent)
	}
	if codex.Version != "0.42.0" || codex.Key != pol.CodexKey || codex.Generation != 0 {
		t.Fatalf("fleet codex target = %+v", codex)
	}

	b := model.Binding{
		User:        "work1",
		AgentTarget: &model.ReleaseTarget{Version: "1.2.16", SHA256: "bbbb", Key: "agent_workdir/_agent/1.2.16/agent.exe", Generation: 3},
		CodexTarget: &model.ReleaseTarget{Version: "", Generation: 2}, // an explicit "nothing" for this machine
	}
	agent, codex = effectiveTargets(pol, b)
	if agent.Version != "1.2.16" || agent.Key != "agent_workdir/_agent/1.2.16/agent.exe" || agent.Generation != 3 {
		t.Fatalf("overridden agent target = %+v", agent)
	}
	if codex.Version != "" {
		t.Fatalf("a present-but-empty override means no target, got %+v", codex)
	}
}

func TestTargetMarkersCarryTheGeneration(t *testing.T) {
	if got := targetMarker(model.ReleaseTarget{Version: "0.42.0", Generation: 2}); got != "0.42.0@2" {
		t.Fatalf("marker = %q", got)
	}
	if got := targetMarker(model.ReleaseTarget{Version: "0.42.0"}); got != "0.42.0@0" {
		t.Fatalf("fleet marker = %q", got)
	}
}

func TestAnUnboundMachineStillReadsItsTargets(t *testing.T) {
	store := newFakeStore()
	store.set("agent_workdir/_bindings/PC-1", []byte(`{"user":"","agentTarget":{"version":"1.2.16","sha256":"x","generation":1}}`), "e1")
	s := newSyncer(t, store, &fakeApplier{})
	binding, bound := s.loadBinding("PC-1")
	if bound {
		t.Fatal("no user means not bound")
	}
	if binding.AgentTarget == nil || binding.AgentTarget.Version != "1.2.16" {
		t.Fatalf("targets must survive an empty user: %+v", binding)
	}
}
```

- [ ] **Step 2: 写 Codex 行为的失败测试**

追加到 `codex_test.go`(沿用 `newFakeStore`、`fakeCodex`、`newCodexSyncer`、`codexPolicy`):

```go
func TestCodexReportsWhyItDeferred(t *testing.T) {
	store := newFakeStore()
	codex := &fakeCodex{installed: "0.41.0", running: true, free: 100 << 30}
	s := newCodexSyncer(t, store, codex)
	pol := codexPolicy(store, "0.42.0", []byte("installer"))
	store.set("agent_workdir/policy.json", mustPolicyJSON(t, pol), "p1")

	st, _ := s.RunOnce()
	if st.CodexState != CodexDeferred || st.CodexDeferReason != CodexDeferInUse {
		t.Fatalf("state %q reason %q, want deferred/in_use", st.CodexState, st.CodexDeferReason)
	}
	if st.CodexTarget != "0.42.0" || st.CodexTargetGeneration != 0 {
		t.Fatalf("the report must name the target: %q gen %d", st.CodexTarget, st.CodexTargetGeneration)
	}

	codex.running, codex.free = false, 1<<30
	st, _ = s.RunOnce()
	if st.CodexDeferReason != CodexDeferDisk {
		t.Fatalf("reason = %q, want disk", st.CodexDeferReason)
	}
}

func TestANewGenerationRetriesAFailedCodexInstall(t *testing.T) {
	store := newFakeStore()
	codex := &fakeCodex{installed: "0.41.0", free: 100 << 30, installErr: errors.New("boom")}
	s := newCodexSyncer(t, store, codex)
	pol := codexPolicy(store, "0.42.0", []byte("installer"))
	store.set("agent_workdir/policy.json", mustPolicyJSON(t, pol), "p1")
	sum := sha256.Sum256([]byte("installer"))
	target := &model.ReleaseTarget{Version: "0.42.0", SHA256: hex.EncodeToString(sum[:]), Key: pol.CodexKey, Generation: 1}
	store.set("agent_workdir/_bindings/"+s.Machine.Name(), mustJSON(t, model.Binding{User: "work1", CodexTarget: target}), "b1")

	s.RunOnce()
	st, _ := s.RunOnce()
	if codex.installs != 1 || st.CodexState != CodexFailed || st.CodexTargetGeneration != 1 {
		t.Fatalf("generation 1: installs = %d, state = %q, gen = %d", codex.installs, st.CodexState, st.CodexTargetGeneration)
	}

	target.Generation = 2
	store.set("agent_workdir/_bindings/"+s.Machine.Name(), mustJSON(t, model.Binding{User: "work1", CodexTarget: target}), "b2")
	st, _ = s.RunOnce()
	if codex.installs != 2 || st.CodexTargetGeneration != 2 {
		t.Fatalf("generation 2 must try again: installs = %d, gen = %d", codex.installs, st.CodexTargetGeneration)
	}
}
```

`mustPolicyJSON` / `mustJSON`:若 `sync_test.go` 里没有,加两个 `json.Marshal` 加 `t.Fatalf` 的四行辅助函数。

- [ ] **Step 3: 写 agent 自更新的失败测试**

追加到 `sync_test.go`(沿用 `setupUpdate`):

```go
func TestTheAgentReportsAFailedUpdateAfterwards(t *testing.T) {
	bin := []byte("new agent")
	sum := sha256.Sum256(bin)
	s, store, up := setupUpdate(t, "1.2.17", hex.EncodeToString(sum[:]), bin)
	// A binding target for a versioned key wins over the fleet key.
	store.set("agent_workdir/_agent/1.2.17/agent.exe", bin, "v")
	store.set("agent_workdir/_bindings/"+s.Machine.Name(), mustJSON(t, model.Binding{User: "work1",
		AgentTarget: &model.ReleaseTarget{Version: "1.2.17", SHA256: hex.EncodeToString(sum[:]),
			Key: "agent_workdir/_agent/1.2.17/agent.exe", Generation: 4}}), "b")

	st, _ := s.RunOnce()
	if st.AgentUpdateTarget != "1.2.17" || st.AgentUpdateGeneration != 4 || st.AgentUpdateState != AgentUpdatePending {
		t.Fatalf("first cycle: %q gen %d state %q", st.AgentUpdateTarget, st.AgentUpdateGeneration, st.AgentUpdateState)
	}
	if store.downloadCount("agent_workdir/_agent/1.2.17/agent.exe") != 1 || len(up.applied) != 1 {
		t.Fatal("the versioned key must be the one fetched and applied")
	}
	// The process was not actually replaced (the fake did nothing), so the
	// next cycle runs the old version with the marker set: that is a failed
	// update, and the report must say so instead of trying again.
	st, _ = s.RunOnce()
	if st.AgentUpdateState != AgentUpdateFailed || len(up.applied) != 1 {
		t.Fatalf("second cycle: state %q, applied %d", st.AgentUpdateState, len(up.applied))
	}
}
```

- [ ] **Step 4: 跑测试确认失败**

Run: `cd go && go test ./internal/agentcore/ -run 'Target|Defer|Generation|FailedUpdate'`
Expected: FAIL(编译错误,`effectiveTargets` 等未定义)

- [ ] **Step 5: 加模型字段**

`model.go`:在 `Binding` 的 `RestartCodexAt` 之后加(含上面 Interfaces 里的 `ReleaseTarget` 类型):

```go
	// AgentTarget and CodexTarget are this machine's release targets (agent
	// 1.2.16+). Present, they REPLACE the fleet-wide target in policy.json
	// for this machine -- a present target with an empty Version means "this
	// machine: nothing". Absent, the fleet policy applies as before, which is
	// what every binding object already in the bucket says.
	//
	// They ride on the binding for the same reason RestartCodex does: it is
	// the one per-machine object every agent reads every cycle, and the
	// agent's RAM policy already grants it. Older agents ignore the fields.
	AgentTarget *ReleaseTarget `json:"agentTarget,omitempty"`
	CodexTarget *ReleaseTarget `json:"codexTarget,omitempty"`
```

`Status` 加 Interfaces 里列出的六个字段,放在 `CodexState` 之后,注释说明:target 与 generation 是这台机器本周期所认的目标,控制台用它们判断一份回执属于哪一代;`AgentUpdateState` 为 `failed` 表示上一周期已尝试该目标而本进程仍是旧版本。

`status.go` 的 `Report` 加同名六个字段,`Build` 逐一赋值。

- [ ] **Step 6: 实现 agentcore**

`sync.go`:

```go
const (
	AgentUpdatePending = "pending" // fetched and verified; applied after this report
	AgentUpdateFailed  = "failed"  // attempted in an earlier cycle and this is still the old binary
)

// effectiveTargets decides what this machine should be running. A target on
// the binding replaces the fleet target for that product, even when it says
// "nothing"; otherwise the fleet policy applies.
func effectiveTargets(pol model.Policy, b model.Binding) (agent, codex model.ReleaseTarget) {
	agent = model.ReleaseTarget{Version: pol.AgentUpdateVersion, SHA256: pol.AgentUpdateSHA256, Key: ossclient.AgentBinaryKey()}
	if b.AgentTarget != nil {
		agent = *b.AgentTarget
		if agent.Key == "" {
			agent.Key = ossclient.AgentBinaryKey()
		}
	}
	codex = model.ReleaseTarget{Version: pol.CodexVersion, SHA256: pol.CodexSHA256, Key: pol.CodexKey}
	if b.CodexTarget != nil {
		codex = *b.CodexTarget
	}
	return agent, codex
}

// targetMarker is what the one-attempt markers record: the version and the
// generation, so a new generation of the same version is a new attempt.
func targetMarker(t model.ReleaseTarget) string {
	return t.Version + "@" + strconv.Itoa(t.Generation)
}
```

`loadBinding` 改为:解析成功就返回对象,`bound = b.User != ""`(不再把 `User == ""` 当成解析失败),这样未绑定机器的目标也读得到。

`RunOnce`:在 `loadBinding` 之后算 `agentTarget, codexTarget := effectiveTargets(pol, binding)`;`updateCodex(codexTarget, &errs)` 返回 `codexOutcome{Version, State, Reason string}`;`prepareUpdate(agentTarget, &errs)` 返回 `(data []byte, state string)`;`status.Build` 传入 `CodexTarget: codexTarget.Version, CodexTargetGeneration: codexTarget.Generation, CodexDeferReason: outcome.Reason, AgentUpdateTarget: agentTarget.Version, AgentUpdateGeneration: agentTarget.Generation, AgentUpdateState: updateState`;应用更新前 `s.writeMarker(updateMarkerFile, targetMarker(agentTarget))`。

`prepareUpdate`:

```go
func (s *Syncer) prepareUpdate(target model.ReleaseTarget, errs *[]string) ([]byte, string) {
	if target.Version == "" || target.Version == s.Version || s.Updater == nil {
		return nil, ""
	}
	if s.readMarker(updateMarkerFile) == targetMarker(target) {
		// Tried already and this is still the old binary: the update did not
		// take. Say so every cycle until the console moves the generation.
		return nil, AgentUpdateFailed
	}
	data, _, err := s.Store.Get(target.Key)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("update: fetch binary: %v", err))
		return nil, AgentUpdateFailed
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, target.SHA256) {
		*errs = append(*errs, fmt.Sprintf("update: checksum mismatch (got %s, want %s); not applying", got, target.SHA256))
		s.writeMarker(updateMarkerFile, targetMarker(target)) // one attempt per generation, like Codex
		return nil, AgentUpdateFailed
	}
	return data, AgentUpdatePending
}
```

`codex.go`:`codexEligible(target model.ReleaseTarget, installed string)`(把 `pol.Codex*` 换成 `target.*`);`updateCodex(target, errs) codexOutcome`;`Running()` 为真 → `Reason = CodexDeferInUse`,磁盘不足 → `Reason = CodexDeferDisk`;所有 `writeMarker(codexMarkerFile, pol.CodexVersion)` 改为 `targetMarker(target)`;`readMarker(...) == targetMarker(target)` 判断"已尝试"。常量:

```go
const (
	CodexDeferInUse = "in_use"
	CodexDeferDisk  = "disk"
)
```

- [ ] **Step 7: 跑 agentcore 全部测试**

Run: `cd go && go test ./internal/agentcore/ ./internal/status/ ./internal/model/`
Expected: PASS。`TestCodexTriesAFailedVersionOnce` 等旧测试若断言旧标记格式(`"0.42.0"`),改为 `"0.42.0@0"`。

- [ ] **Step 8: Windows 构建**

Run: `cd go && GOOS=windows GOARCH=amd64 go build ./... && GOOS=windows GOARCH=amd64 go vet ./cmd/agent/`
Expected: 无输出

- [ ] **Step 9: 提交**

```bash
cd go && git add internal/model/model.go internal/status/status.go internal/agentcore/
git commit -m "agent: per-machine release targets, generations, and why Codex waited

The binding object may now carry a target for this machine that replaces
the fleet one. Markers record version and generation, so a retry is a
new generation rather than a renamed version. The report names the target
it acted on, the generation, and the reason an install was deferred.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: 导出:固定 key 复制、绑定对象带目标

**Files:**
- Modify: `go/internal/worker/export.go:20-25,113-135,150-192`
- Test: `go/internal/worker/export_test.go`

**Interfaces:**
- Consumes: `repo.Releases`(Task 3)、`model.Binding.AgentTarget/CodexTarget`(Task 5)、`ossclient.AgentBinaryKey`。
- Produces: `worker.ObjectStore` 接口新增 `Copy(src, dst string) error`;`fakeObjects.Copy`;`dbmode.go` 里 `st.objects` 传给 `OSSExport{Objects: ...}` 的值必须实现 `Copy`(`*ossclient.Client` 已实现,Task 1)。

- [ ] **Step 1: 写失败测试**

追加到 `export_test.go`。先给 `fakeObjects` 加:

```go
func (f *fakeObjects) Copy(src, dst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[src]
	if !ok {
		return ossclient.ErrNotFound
	}
	f.objects[dst] = append([]byte(nil), data...)
	f.copies = append(f.copies, src+" -> "+dst)
	return nil
}
```

(`fakeObjects` 若没有 `mu`,照现有 Put 的写法;加字段 `copies []string`。)

```go
func TestThePolicyExportCopiesTheChosenAgentBuildToTheFixedKey(t *testing.T) {
	store, ctx := newWorkerStore(t)
	objects := newFakeObjects()
	objects.objects[ossclient.AgentVersionKey("1.2.16")] = []byte("agent 1.2.16")
	a, _ := store.Releases().CreateArtifact(ctx, repo.NewArtifact{Product: repo.ProductAgent, Version: "1.2.16",
		SHA256: strings.Repeat("a", 64), SizeBytes: 12, ObjectKey: ossclient.AgentVersionKey("1.2.16"), CreatedBy: "t"})
	_ = a
	pol := model.DefaultPolicy()
	pol.AgentUpdateVersion, pol.AgentUpdateSHA256 = "1.2.16", strings.Repeat("a", 64)
	content, _ := json.Marshal(pol)
	published, _ := store.Policies().Publish(ctx, content, "aim", "t")

	h := OSSExport{Store: store, Objects: objects}
	if _, err := h.Run(ctx, repo.Task{Kind: repo.TaskOSSExport,
		Payload: mustJSON(t, map[string]any{"policy_version": published.Version})}); err != nil {
		t.Fatalf("export: %v", err)
	}
	if got, _ := objects.get(ossclient.AgentBinaryKey()); string(got) != "agent 1.2.16" {
		t.Fatalf("fixed key holds %q", got)
	}
	if got, _ := objects.get(ossclient.PolicyKey()); !bytes.Contains(got, []byte(`"agentUpdateVersion": "1.2.16"`)) {
		t.Fatal("policy.json was not written after the copy")
	}
	// The copy comes before the policy: a fleet pointed at a key that does
	// not hold the build yet is a fleet failing checksums.
	if len(objects.copies) != 1 {
		t.Fatalf("copies = %v", objects.copies)
	}
}

func TestTheBindingCarriesTheMachinesTargetsAndSurvivesUnbinding(t *testing.T) {
	store, ctx := newWorkerStore(t)
	objects := newFakeObjects()
	device, _ := store.Devices().EnsureByHostname(ctx, "PC-7")
	store.Devices().MarkSeen(ctx, device.ID, "1.2.16", time.Now())
	a, _ := store.Releases().CreateArtifact(ctx, repo.NewArtifact{Product: repo.ProductCodex, Version: "0.42.0",
		SHA256: strings.Repeat("c", 64), SizeBytes: 1, ObjectKey: "agent_workdir/_codex/codex-setup-0.42.0.exe", CreatedBy: "t"})
	r, _ := store.Releases().CreateRollout(ctx, repo.NewRollout{Product: repo.ProductCodex, ArtifactID: a.ID, Kind: repo.RolloutRelease, CreatedBy: "t"})
	target, _ := store.Releases().CreateTarget(ctx, device.ID, repo.ProductCodex, a.ID, r.ID)

	h := OSSExport{Store: store, Objects: objects}
	run := func() model.Binding {
		t.Helper()
		if _, err := h.Run(ctx, repo.Task{Kind: repo.TaskOSSExport, Payload: mustJSON(t, map[string]any{"device_id": device.ID})}); err != nil {
			t.Fatalf("export: %v", err)
		}
		data, ok := objects.get(ossclient.BindingKey("PC-7"))
		if !ok {
			t.Fatal("no binding object")
		}
		var b model.Binding
		json.Unmarshal(data, &b)
		return b
	}

	// Unbound, but with a target: the object exists, with no user.
	b := run()
	if b.User != "" || b.CodexTarget == nil || b.CodexTarget.Version != "0.42.0" ||
		b.CodexTarget.Generation != target.Generation || b.CodexTarget.SHA256 != a.SHA256 || b.CodexTarget.Key != a.ObjectKey {
		t.Fatalf("unbound binding = %+v", b)
	}
	if b.AgentTarget != nil {
		t.Fatal("no agent target was opened, so none may be written")
	}

	// Paused: the target is withheld, not cancelled.
	store.Releases().SetRolloutPaused(ctx, r.ID, true, "t")
	if b = run(); b.CodexTarget != nil {
		t.Fatalf("a paused rollout must not reach the machine: %+v", b)
	}
	store.Releases().SetRolloutPaused(ctx, r.ID, false, "t")

	// Finished: nothing left to say, and with nobody bound the object goes.
	store.Releases().FinishTarget(ctx, target.ID, repo.TargetSucceeded, "", "0.42.0")
	h.Run(ctx, repo.Task{Kind: repo.TaskOSSExport, Payload: mustJSON(t, map[string]any{"device_id": device.ID})})
	if _, ok := objects.get(ossclient.BindingKey("PC-7")); ok {
		t.Fatal("no user and no target: the binding object should be deleted")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd go && TEST_PG_DSN='postgres://postgres:devpass@127.0.0.1:5433/aienv_test?sslmode=disable' go test ./internal/worker/ -run 'CopiesTheChosen|CarriesTheMachinesTargets'`
Expected: FAIL(`Copy` 不在接口上 / 固定 key 为空 / 未绑定即删除)

- [ ] **Step 3: 实现**

`ObjectStore` 接口加 `Copy(src, dst string) error`。

`exportPolicy`:在 `Objects.Put(PolicyKey)` 之前:

```go
	var pol model.Policy
	if err := json.Unmarshal(current.Content, &pol); err != nil {
		return Result{}, Permanent(fmt.Errorf("policy version %d is not readable: %w", current.Version, err))
	}
	if pol.AgentUpdateVersion != "" {
		artifact, err := h.Store.Releases().ArtifactByVersion(ctx, repo.ProductAgent, pol.AgentUpdateVersion)
		switch {
		case err == nil:
			// Before the policy, every time: the fixed key must hold the
			// build the policy names before any agent reads the policy.
			if err := h.Objects.Copy(artifact.ObjectKey, ossclient.AgentBinaryKey()); err != nil {
				return Result{}, ClassError("oss_copy", err)
			}
		case errors.Is(err, repo.ErrNotFound):
			// A version set before the version library existed: the fixed key
			// was written directly and is left as it is.
		default:
			return Result{}, err
		}
	}
```

`exportBinding`:把"未绑定 → 删除"改成先算目标:

```go
	targets, err := h.machineTargets(ctx, device.ID)
	if err != nil {
		return Result{}, err
	}
	binding, err := h.Store.Bindings().Open(ctx, device.ID)
	unbound := errors.Is(err, repo.ErrNotFound)
	if err != nil && !unbound {
		return Result{}, err
	}
	if unbound && targets.AgentTarget == nil && targets.CodexTarget == nil {
		if err := h.Objects.Delete(ossclient.BindingKey(device.Hostname)); err != nil {
			return Result{}, ClassError("oss_delete", err)
		}
		return Result{Note: "unbound"}, nil
	}
	object := model.Binding{AgentTarget: targets.AgentTarget, CodexTarget: targets.CodexTarget}
	if !unbound {
		employee, err := h.Store.Employees().ByID(ctx, binding.EmployeeID)
		if err != nil {
			return Result{}, err
		}
		object.User, object.BoundAt, object.Note = employee.WindowsUser, binding.BoundAt.UTC().Format(time.RFC3339), binding.Note
		// restart nonce as before
	}
```

```go
// machineTargets reads the open targets of one machine into binding fields.
// A paused rollout's target is withheld: the machine sees nothing to do,
// and the target stays pending for when the rollout resumes.
func (h OSSExport) machineTargets(ctx context.Context, deviceID string) (model.Binding, error) {
	var out model.Binding
	for _, product := range []string{repo.ProductAgent, repo.ProductCodex} {
		target, err := h.Store.Releases().OpenTarget(ctx, deviceID, product)
		if errors.Is(err, repo.ErrNotFound) {
			continue
		}
		if err != nil {
			return out, err
		}
		rollout, err := h.Store.Releases().RolloutByID(ctx, target.RolloutID)
		if err != nil {
			return out, err
		}
		if rollout.PausedAt != nil {
			continue
		}
		artifact, err := h.Store.Releases().ArtifactByID(ctx, target.ArtifactID)
		if err != nil {
			return out, err
		}
		rt := &model.ReleaseTarget{Version: artifact.Version, SHA256: artifact.SHA256, Key: artifact.ObjectKey, Generation: target.Generation}
		if product == repo.ProductAgent {
			out.AgentTarget = rt
		} else {
			out.CodexTarget = rt
		}
	}
	return out, nil
}
```

注意 `Result.Note`:绑定且有目标时写 `"PC-7 -> work1, codex 0.42.0 gen 2"`,便于任务页看。

- [ ] **Step 4: 跑 worker 全部测试**

Run: `cd go && TEST_PG_DSN='postgres://postgres:devpass@127.0.0.1:5433/aienv_test?sslmode=disable' go test ./internal/worker/`
Expected: PASS(`TestBindingAndRestartReachTheMachine`、`TestForgettingAMachineRemovesItsObjects` 仍过:无目标时行为不变)

- [ ] **Step 5: 提交**

```bash
cd go && git add internal/worker/export.go internal/worker/export_test.go
git commit -m "worker: copy the chosen agent build to the fixed key; bindings carry targets

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: 按回执结算设备目标

**Files:**
- Create: `go/internal/worker/settle.go`, `go/internal/worker/settle_test.go`
- Modify: `go/internal/worker/status.go:60-80`

**Interfaces:**
- Consumes: `model.Status` 的新字段(Task 5)、`repo.Releases.OpenTarget/FinishTarget/ArtifactByID`。
- Produces: `settleTargets(ctx, store repo.Store, deviceID string, s model.Status) error`,由 `StatusImport.Run` 在 `Reports().Import` 返回 `written == true` 后调用。

- [ ] **Step 1: 写失败测试**

`go/internal/worker/settle_test.go`:

```go
package worker

import (
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func openCodexTarget(t *testing.T, store repo.Store, hostname, version string) (repo.Device, repo.Target) {
	t.Helper()
	ctx := t.Context()
	device, _ := store.Devices().EnsureByHostname(ctx, hostname)
	a, err := store.Releases().CreateArtifact(ctx, repo.NewArtifact{Product: repo.ProductCodex, Version: version,
		SHA256: strings.Repeat("c", 64), SizeBytes: 1, ObjectKey: "k", CreatedBy: "t"})
	if err != nil {
		a, _ = store.Releases().ArtifactByVersion(ctx, repo.ProductCodex, version)
	}
	r, _ := store.Releases().CreateRollout(ctx, repo.NewRollout{Product: repo.ProductCodex, ArtifactID: a.ID, Kind: repo.RolloutRelease, CreatedBy: "t"})
	target, _ := store.Releases().CreateTarget(ctx, device.ID, repo.ProductCodex, a.ID, r.ID)
	return device, target
}

func TestAMatchingFreshReportSucceedsTheTarget(t *testing.T) {
	store, ctx := newWorkerStore(t)
	device, target := openCodexTarget(t, store, "PC-1", "0.42.0")
	later := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)

	if err := settleTargets(ctx, store, device.ID, model.Status{LastSync: later, CodexVersion: "0.42.0", CodexTarget: "0.42.0", CodexTargetGeneration: target.Generation}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	got, _ := store.Releases().TargetByID(ctx, target.ID)
	if got.Status != repo.TargetSucceeded || got.ReportedVersion != "0.42.0" {
		t.Fatalf("after a matching report: %+v", got)
	}
}

func TestAStaleReportSettlesNothing(t *testing.T) {
	store, ctx := newWorkerStore(t)
	device, target := openCodexTarget(t, store, "PC-2", "0.42.0")
	earlier := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	// The machine already had 0.42.0 before the target existed (say, a
	// rollback to what it has). Old evidence is not evidence of this target.
	settleTargets(ctx, store, device.ID, model.Status{LastSync: earlier, CodexVersion: "0.42.0"})
	got, _ := store.Releases().TargetByID(ctx, target.ID)
	if got.Status != repo.TargetPending {
		t.Fatalf("a report older than the target settled it: %+v", got)
	}
}

func TestAFailureForThisGenerationFailsTheTarget(t *testing.T) {
	store, ctx := newWorkerStore(t)
	device, target := openCodexTarget(t, store, "PC-3", "0.42.0")
	later := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)

	// A failure reported for an older generation is about a target that no
	// longer exists.
	settleTargets(ctx, store, device.ID, model.Status{LastSync: later, CodexVersion: "0.41.0",
		CodexTarget: "0.42.0", CodexTargetGeneration: target.Generation - 1, CodexState: "failed",
		Errors: []string{"codex: install: exit status 2"}})
	if got, _ := store.Releases().TargetByID(ctx, target.ID); got.Status != repo.TargetPending {
		t.Fatalf("an older generation's failure settled it: %+v", got)
	}

	settleTargets(ctx, store, device.ID, model.Status{LastSync: later, CodexVersion: "0.41.0",
		CodexTarget: "0.42.0", CodexTargetGeneration: target.Generation, CodexState: "failed",
		Errors: []string{"policy: x", "codex: install: exit status 2"}})
	got, _ := store.Releases().TargetByID(ctx, target.ID)
	if got.Status != repo.TargetFailed || got.ResultNote != "codex: install: exit status 2" {
		t.Fatalf("after a failure for this generation: %+v", got)
	}
}

func TestDeferredAndOfflineStayPending(t *testing.T) {
	store, ctx := newWorkerStore(t)
	device, target := openCodexTarget(t, store, "PC-4", "0.42.0")
	later := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	settleTargets(ctx, store, device.ID, model.Status{LastSync: later, CodexVersion: "0.41.0",
		CodexTarget: "0.42.0", CodexTargetGeneration: target.Generation, CodexState: "deferred", CodexDeferReason: "in_use"})
	if got, _ := store.Releases().TargetByID(ctx, target.ID); got.Status != repo.TargetPending {
		t.Fatalf("deferred is not a result: %+v", got)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd go && TEST_PG_DSN='postgres://postgres:devpass@127.0.0.1:5433/aienv_test?sslmode=disable' go test ./internal/worker/ -run 'SucceedsTheTarget|StaleReport|FailsTheTarget|StayPending'`
Expected: FAIL,`undefined: settleTargets`

- [ ] **Step 3: 实现**

`go/internal/worker/settle.go`:

```go
package worker

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/agentcore"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// settleTargets turns a machine's report into a result for its open
// targets. Success is the machine running the version, reported after the
// target was made; failure is the machine saying it tried this generation
// and could not. Anything else -- deferred, downloading, an older report --
// leaves the target pending. Nothing here guesses.
func settleTargets(ctx context.Context, store repo.Store, deviceID string, s model.Status) error {
	reportedAt, err := time.Parse(time.RFC3339, s.LastSync)
	if err != nil {
		return nil // a report with no usable time cannot settle anything
	}
	kinds := []struct {
		product    string
		running    string // what the machine has
		target     string // what it says it is aiming at
		generation int
		state      string
		prefix     string // the error line that explains a failure
	}{
		{repo.ProductAgent, s.AgentVersion, s.AgentUpdateTarget, s.AgentUpdateGeneration, s.AgentUpdateState, "update:"},
		{repo.ProductCodex, s.CodexVersion, s.CodexTarget, s.CodexTargetGeneration, s.CodexState, "codex:"},
	}
	for _, k := range kinds {
		target, err := store.Releases().OpenTarget(ctx, deviceID, k.product)
		if errors.Is(err, repo.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if !reportedAt.After(target.CreatedAt) {
			continue
		}
		artifact, err := store.Releases().ArtifactByID(ctx, target.ArtifactID)
		if err != nil {
			return err
		}
		switch {
		case k.running == artifact.Version:
			_, err = store.Releases().FinishTarget(ctx, target.ID, repo.TargetSucceeded, "", k.running)
		case k.target == artifact.Version && k.generation == target.Generation &&
			(k.state == agentcore.CodexFailed || k.state == agentcore.AgentUpdateFailed):
			_, err = store.Releases().FinishTarget(ctx, target.ID, repo.TargetFailed, firstError(s.Errors, k.prefix), k.running)
		}
		if err != nil && !errors.Is(err, repo.ErrConflict) {
			return err
		}
	}
	return nil
}

// firstError picks the line about this product, or a generic note.
func firstError(lines []string, prefix string) string {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return "the machine reported failure without a matching error line"
}
```

`status.go` 的 `Run`:在 `imported++` 之后、`MarkSeen` 之前加 `if err := settleTargets(ctx, h.Store, device.ID, status); err != nil { return Result{}, err }`。

`agentcore` 是纯逻辑包,worker 引用它的常量不会把 Windows 代码拉进来;若 `go vet` 报循环依赖(agentcore 不 import worker,不会),把两个常量复制到 `model` 包。

- [ ] **Step 4: 跑测试确认通过**

Run: `cd go && TEST_PG_DSN='postgres://postgres:devpass@127.0.0.1:5433/aienv_test?sslmode=disable' go test ./internal/worker/`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
cd go && git add internal/worker/settle.go internal/worker/settle_test.go internal/worker/status.go
git commit -m "worker: settle release targets from what the machine reports

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 8: 控制台:落盘上传与版本库页

**Files:**
- Create: `go/internal/adminweb/releases.go`, `go/internal/adminweb/releases_test.go`, `go/internal/adminweb/assets/releases.html`
- Modify: `go/internal/adminweb/server.go:319,345-349`(路由), `go/internal/adminweb/dbmode.go:40-56`(`DatabaseOptions.SpoolDir`), `go/internal/adminweb/download.go`, `go/internal/adminweb/dbbackend.go:512-558`, `go/internal/adminweb/handlers.go:45-90`(`pageData` 加字段), `go/internal/adminweb/assets/_layout.html`(导航), `go/cmd/admin/web_cmd.go:40-53`

**Interfaces:**
- Consumes: `ghrelease.FetchToFile`(Task 2)、`ops.RegisterArtifact/SetArtifactStatus/SetGlobalTarget/ClearGlobalTarget`(Task 4)、`ossclient.AgentVersionKey/CodexInstallerKey`、`(*ossclient.Client).PutFile`。
- Produces:
  - `DatabaseOptions.SpoolDir string`(env `AIENVMGR_SPOOL_DIR`,默认 `/var/lib/ai-env-mgr/spool`);`maxPackageBytes = 2 << 30`。
  - `type packageUploader interface{ PutFile(key, path string, onProgress func(done, total int64)) error }`;`dbState.objects` 必须实现它,否则 DB 模式启动时报错(测试用假实现)。
  - 路由:`GET /releases`,`POST /releases/upload`,`POST /releases/status`,`POST /releases/global`,`POST /releases/global-clear`。DB 模式下 `/rollout`、`/agent/publish`、`/agent/cancel`、`/codex/publish`、`/codex/cancel` 一律 303 到 `/releases`。
  - `pageData` 新增 `Artifacts map[string][]artifactRow`、`GlobalTargets map[string]string`(product → version)。
  - `dbBackend.PublishAgentUpdate/PublishCodexUpdate` 返回 `errors.New("use /releases")`(DB 模式不再有整包进内存的路径;接口保留给 legacy)。

- [ ] **Step 1: 写落盘阶段的失败测试**

`go/internal/adminweb/releases_test.go`:

```go
package adminweb

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStagePackageWritesTheUploadToSpoolAndHashesIt(t *testing.T) {
	spool := t.TempDir()
	payload := bytes.Repeat([]byte("agent"), 10_000)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("version", "1.2.16")
	fw, _ := mw.CreateFormFile("file", "agent.exe")
	fw.Write(payload)
	mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/releases/upload", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if err := parseUpload(r); err != nil {
		t.Fatal(err)
	}

	stage, err := stagePackage(r, spool, maxPackageBytes)
	if err != nil {
		t.Fatalf("stagePackage: %v", err)
	}
	staged, err := stage(nil)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	defer staged.Remove()
	sum := sha256.Sum256(payload)
	if staged.SHA256 != hex.EncodeToString(sum[:]) || staged.Size != int64(len(payload)) || staged.Source != "agent.exe" {
		t.Fatalf("staged = %+v", staged)
	}
	if !strings.HasPrefix(staged.Path, spool) {
		t.Fatalf("staged outside the spool: %s", staged.Path)
	}
	info, _ := os.Stat(staged.Path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("spool file mode %v, want 0600", info.Mode().Perm())
	}
	staged.Remove()
	if _, err := os.Stat(staged.Path); !os.IsNotExist(err) {
		t.Fatal("Remove must delete the spool file")
	}
}

func TestStagePackageFromURLNeverBuffersTheBody(t *testing.T) {
	spool := t.TempDir()
	served := bytes.Repeat([]byte("x"), 3<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(served)
	}))
	defer srv.Close()

	r := httptest.NewRequest(http.MethodPost, "/releases/upload", strings.NewReader("url="+srv.URL+"/codex.exe&version=0.42.0"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	parseUpload(r)
	stage, err := stagePackage(r, spool, 1<<20) // a limit below the body
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stage(nil); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("an oversize download must be refused, err = %v", err)
	}
	entries, _ := os.ReadDir(spool)
	if len(entries) != 0 {
		t.Fatalf("a refused download left %d file(s) in the spool", len(entries))
	}
	if _, err := os.Stat(filepath.Join(spool, "x")); err == nil {
		t.Fatal("unexpected")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd go && go test ./internal/adminweb/ -run TestStagePackage`
Expected: FAIL,`undefined: stagePackage`

- [ ] **Step 3: 实现落盘阶段与上传任务**

`go/internal/adminweb/releases.go`(第一部分):

```go
package adminweb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ghrelease"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// maxPackageBytes bounds one package on disk. The Codex installer is ~700 MB;
// two gigabytes leaves room for it to grow without letting a typo fill the
// disk the database shares.
const maxPackageBytes = 2 << 30

// packageUploader is the one method of the object store that moves a large
// file without holding it: the console runs under a memory cap smaller than
// a Codex installer, so Put is not an option for packages.
type packageUploader interface {
	PutFile(key, path string, onProgress func(done, total int64)) error
}

// stagedPackage is a package on local disk, complete and checksummed.
type stagedPackage struct {
	Path   string
	SHA256 string
	Size   int64
	Source string // the URL or the uploaded file's name, for the record
}

func (p stagedPackage) Remove() { os.Remove(p.Path) }

// stagePackage validates the request now and returns a closure that puts the
// bytes on disk later, in the background job. A browser upload is read from
// the request body here (it is gone when the handler returns) straight into
// the spool file; a URL is fetched later, straight into the spool file. In
// neither case is the package ever whole in memory.
func stagePackage(r *http.Request, spool string, maxBytes int64) (func(onProgress func(done, total int64)) (stagedPackage, error), error) {
	if err := os.MkdirAll(spool, 0o700); err != nil {
		return nil, fmt.Errorf("spool directory: %w", err)
	}
	if url := formValue(r, "url"); url != "" {
		token := releaseToken(r)
		return func(onProgress func(done, total int64)) (stagedPackage, error) {
			dest := filepath.Join(spool, "pkg-"+time.Now().UTC().Format("20060102-150405")+".bin")
			fetched, err := ghrelease.FetchToFile(url, token, 30*time.Minute, dest, maxBytes, onProgress)
			if err != nil {
				return stagedPackage{}, err
			}
			return stagedPackage{Path: dest, SHA256: fetched.SHA256, Size: fetched.Size, Source: url}, nil
		}, nil
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		return nil, fmt.Errorf("give a URL or choose a file")
	}
	defer f.Close()
	if hdr.Size > maxBytes {
		return nil, fmt.Errorf("that file is %d MB; packages are capped at %d MB", hdr.Size>>20, maxBytes>>20)
	}
	dest := filepath.Join(spool, "pkg-"+time.Now().UTC().Format("20060102-150405")+".bin")
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, sum), io.LimitReader(f, maxBytes+1))
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil && n > maxBytes {
		err = fmt.Errorf("the upload is larger than the %d MB limit", maxBytes>>20)
	}
	if err != nil {
		os.Remove(dest)
		return nil, err
	}
	staged := stagedPackage{Path: dest, SHA256: hex.EncodeToString(sum.Sum(nil)), Size: n, Source: hdr.Filename}
	return func(onProgress func(done, total int64)) (stagedPackage, error) {
		if onProgress != nil {
			onProgress(n, n)
		}
		return staged, nil
	}, nil
}

// actionReleaseUpload stages a candidate: download or upload to the spool,
// multipart-upload to the versioned key, register the row. It aims the
// package at nobody.
func (s *Server) actionReleaseUpload(sess *session, r *http.Request) error {
	product := formValue(r, "product")
	version := strings.TrimSpace(formValue(r, "version"))
	notes := formValue(r, "notes")
	minAgent := strings.TrimSpace(formValue(r, "min_agent"))
	if product != repo.ProductAgent && product != repo.ProductCodex {
		return fmt.Errorf("choose agent or codex")
	}
	if version == "" {
		return fmt.Errorf("a version is required")
	}
	// Refuse a used version before moving a single byte.
	if existing, err := s.dbm.store.Releases().ArtifactByVersion(r.Context(), product, version); err == nil {
		return fmt.Errorf("%s %s is already registered (sha256 %s…); a rebuild needs a new version", product, version, existing.SHA256[:12])
	} else if !errors.Is(err, repo.ErrNotFound) {
		return err
	}
	uploader, ok := s.dbm.objects.(packageUploader)
	if !ok {
		return fmt.Errorf("the object store cannot upload from a file")
	}
	stage, err := stagePackage(r, s.opts.Database.SpoolDir, maxPackageBytes)
	if err != nil {
		return err
	}
	key := ossclient.AgentVersionKey(version)
	if product == repo.ProductCodex {
		key = ossclient.CodexInstallerKey(version)
	}
	actor, requestID := sess.admin.Username, s.clientKey(r)
	ops := s.dbm.ops
	return s.jobs.start(product, version, func(setStep func(string), setProgress func(done, total int64)) error {
		setStep("获取安装包")
		staged, err := stage(setProgress)
		if err != nil {
			return err
		}
		defer staged.Remove()
		setStep("上传到 OSS")
		if err := uploader.PutFile(key, staged.Path, setProgress); err != nil {
			return err
		}
		setStep("登记版本")
		_, err = ops.RegisterArtifact(context.Background(), repo.NewArtifact{
			Product: product, Version: version, SHA256: staged.SHA256, SizeBytes: staged.Size,
			ObjectKey: key, Notes: notes, Source: staged.Source, MinAgentVersion: minAgent, CreatedBy: actor,
		}, actor, requestID)
		if err != nil {
			return err
		}
		logAudit(requestID, "registered %s %s as a candidate (%d bytes, sha256 %s)", product, version, staged.Size, staged.SHA256)
		return nil
	})
}
```

`sess.admin.Username` 用 `dbSession` 里实际存管理员用户名的字段名(见 `dbmode.go`)。

- [ ] **Step 4: 写版本库页与其余动作**

同文件(第二部分):

```go
type artifactRow struct {
	repo.Artifact
	SizeMB   string
	IsGlobal bool
}

func (s *Server) handleReleases(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "releases")
	data.Job = s.jobs.snapshot()
	pol, _, err := s.dbm.ops.CurrentPolicy(r.Context())
	if err != nil {
		data.Error = "could not read the policy"
	}
	data.GlobalTargets = map[string]string{repo.ProductAgent: pol.AgentUpdateVersion, repo.ProductCodex: pol.CodexVersion}
	all, err := s.dbm.store.Releases().ListArtifacts(r.Context(), "")
	if err != nil {
		data.Error = "could not list the version library"
	}
	data.Artifacts = map[string][]artifactRow{}
	for _, a := range all {
		data.Artifacts[a.Product] = append(data.Artifacts[a.Product], artifactRow{
			Artifact: a, SizeMB: fmt.Sprintf("%.1f", float64(a.SizeBytes)/(1<<20)),
			IsGlobal: data.GlobalTargets[a.Product] == a.Version,
		})
	}
	s.render(w, "releases.html", http.StatusOK, data)
}

func (s *Server) actionReleaseStatus(sess *session, r *http.Request) error {
	status := repo.ArtifactStatus(formValue(r, "status"))
	switch status {
	case repo.ArtifactAccepted, repo.ArtifactStable, repo.ArtifactRetired, repo.ArtifactCandidate:
	default:
		return fmt.Errorf("unknown status %q", status)
	}
	_, err := s.dbm.ops.SetArtifactStatus(r.Context(), formValue(r, "id"), status, formValue(r, "note"), sess.admin.Username, s.clientKey(r))
	return err
}

func (s *Server) actionReleaseGlobal(sess *session, r *http.Request) error {
	product, version := formValue(r, "product"), formValue(r, "version")
	if err := confirmMatches(r, "confirm", version); err != nil {
		return err
	}
	if _, err := s.dbm.ops.SetGlobalTarget(r.Context(), product, version, sess.admin.Username, s.clientKey(r)); err != nil {
		return err
	}
	logAudit(s.clientKey(r), "aimed the fleet at %s %s", product, version)
	return nil
}

func (s *Server) actionReleaseGlobalClear(sess *session, r *http.Request) error {
	_, err := s.dbm.ops.ClearGlobalTarget(r.Context(), formValue(r, "product"), sess.admin.Username, s.clientKey(r))
	return err
}
```

路由(`server.go`,放在 DB 模式那段 `if` 里):

```go
		mux.HandleFunc("/releases", s.requireSession(s.handleReleases))
		mux.HandleFunc("/releases/upload", s.requirePost("/releases", s.actionReleaseUpload))
		mux.HandleFunc("/releases/status", s.requirePost("/releases", s.actionReleaseStatus))
		mux.HandleFunc("/releases/global", s.requirePost("/releases", s.actionReleaseGlobal))
		mux.HandleFunc("/releases/global-clear", s.requirePost("/releases", s.actionReleaseGlobalClear))
		for _, old := range []string{"/rollout", "/agent/publish", "/agent/cancel", "/codex/publish", "/codex/cancel"} {
			mux.HandleFunc(old, movedTo("/releases"))
		}
```

注意 `mux.HandleFunc` 同一路径注册两次会 panic:把 legacy 的那五条注册移到 `else` 分支(非 DB 模式才注册)。

`DatabaseOptions` 加 `SpoolDir string`;`openDatabaseMode` 里为空则设默认 `/var/lib/ai-env-mgr/spool`,并 `os.MkdirAll(..., 0o700)`,失败即启动失败;`web_cmd.go` 加 `SpoolDir: os.Getenv("AIENVMGR_SPOOL_DIR")`。

`assets/releases.html`:沿用 `rollout.html` 的 `head`/`nav`/`notices`/job 面板,主体为:

```html
  {{range $product := list "agent" "codex"}}
  <h2>{{$product}}</h2>
  <p class="muted">全局目标:{{with index $.GlobalTargets $product}}<span class="mono">{{.}}</span>
    <form method="post" action="/releases/global-clear" class="inline"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="product" value="{{$product}}"><button class="link">清除(尚未更新的机器停止尝试)</button></form>
    {{else}}未设置{{end}}</p>
  <div class="panel scroll"><table>
    <thead><tr><th>版本</th><th>状态</th><th>大小</th><th>SHA-256</th><th>最低 agent</th><th>登记</th><th>验收</th><th>说明</th><th></th></tr></thead>
    <tbody>{{range index $.Artifacts $product}}
      <tr>
        <td class="mono">{{.Version}}{{if .IsGlobal}} <span class="tag">全局目标</span>{{end}}</td>
        <td>{{.Status}}</td><td>{{.SizeMB}} MB</td><td class="mono muted">{{slice .SHA256 0 12}}…</td>
        <td class="mono">{{.MinAgentVersion}}</td><td>{{.CreatedBy}} {{.CreatedAt.Format "01-02 15:04"}}</td>
        <td>{{if .AcceptedAt}}{{.AcceptedBy}}:{{.AcceptanceNote}}{{end}}</td><td>{{.Notes}}</td>
        <td>
          {{if ne (printf "%s" .Status) "retired"}}
          <form method="post" action="/releases/status" class="inline"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="status" value="accepted"><input name="note" placeholder="验收记录(测试机、日期)" size="18"><button class="link">登记验收</button></form>
          <form method="post" action="/releases/global" class="inline"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="product" value="{{$product}}"><input type="hidden" name="version" value="{{.Version}}"><input name="confirm" placeholder="输入版本号确认" size="12"><button class="link">设为全局目标</button></form>
          <a href="/rollouts/new?artifact={{.ID}}">创建发布…</a>
          <form method="post" action="/releases/status" class="inline"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="status" value="retired"><button class="link">停用</button></form>
          {{end}}
        </td>
      </tr>{{else}}<tr><td colspan="9" class="muted">还没有登记任何版本</td></tr>{{end}}
    </tbody></table></div>
  {{end}}

  <h2>上传候选版本</h2>
  <div class="caution">上传只把包放进版本库,<strong>不会改变任何机器</strong>。设为全局目标或创建发布任务才会下发。</div>
  <form method="post" action="/releases/upload" enctype="multipart/form-data" class="form">
    <input type="hidden" name="csrf" value="{{.CSRF}}">
    <label>产品 <select name="product"><option value="agent">agent</option><option value="codex">codex</option></select></label>
    <label>版本 <input name="version" required></label>
    <label>最低 agent 版本 <input name="min_agent" placeholder="1.2.16"></label>
    <label>URL <input name="url" placeholder="https://github.com/.../releases/download/..."></label>
    <label>Token(私有仓库,只用一次) <input name="token" type="password" autocomplete="off"></label>
    <label>或选择文件 <input type="file" name="file"></label>
    <label>变更说明 <textarea name="notes" rows="3"></textarea></label>
    <button>上传候选</button>
  </form>
```

`list` 若模板函数表里没有,在 `render` 的 `FuncMap` 加 `"list": func(xs ...string) []string { return xs }`。导航 `_layout.html`:DB 模式把"发布更新"指向 `/releases`,并加"发布任务"指向 `/rollouts`。

- [ ] **Step 5: 写页面测试**

追加到 `releases_test.go`(沿用 `dbmode_test.go` 的 `newDatabaseServer`、`signedIn`、`csrfFrom`、`dbPost`、`dbGet`;`newDatabaseServer` 返回的 fake 对象存储需要实现 `PutFile`:把文件读进 map 即可,只是测试):

```go
func TestReleasesPageListsCandidatesAndAimsOnlyOnRequest(t *testing.T) {
	s, _ := newDatabaseServer(t)
	cookie := signedIn(t, s)
	ctx := t.Context()
	a, err := s.dbm.ops.RegisterArtifact(ctx, repo.NewArtifact{Product: repo.ProductCodex, Version: "0.42.0",
		SHA256: strings.Repeat("c", 64), SizeBytes: 700 << 20, ObjectKey: "agent_workdir/_codex/codex-setup-0.42.0.exe", CreatedBy: "t"}, "t", "r")
	if err != nil {
		t.Fatal(err)
	}
	page := dbGet(t, s.Handler(), "/releases", cookie)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "0.42.0") || !strings.Contains(page.Body.String(), "全局目标:未设置") {
		t.Fatalf("releases page: %d %s", page.Code, page.Body.String()[:200])
	}
	csrf := csrfFrom(t, s, cookie, "/releases")
	rec := dbPost(t, s.Handler(), "/releases/global", url.Values{"csrf": {csrf}, "product": {"codex"}, "version": {"0.42.0"}, "confirm": {"0.42.0"}}, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("global: %d %s", rec.Code, rec.Body.String())
	}
	pol, _, _ := s.dbm.ops.CurrentPolicy(ctx)
	if pol.CodexVersion != "0.42.0" || pol.CodexSHA256 != a.SHA256 {
		t.Fatalf("policy = %+v", pol)
	}
	// The old fleet-wide publish route is gone in database mode.
	if rec := dbGet(t, s.Handler(), "/rollout", cookie); rec.Code != http.StatusMovedPermanently && rec.Code != http.StatusSeeOther {
		t.Fatalf("/rollout should redirect, got %d", rec.Code)
	}
}
```

- [ ] **Step 6: 跑 adminweb 测试**

Run: `cd go && TEST_PG_DSN='postgres://postgres:devpass@127.0.0.1:5433/aienv_test?sslmode=disable' go test ./internal/adminweb/`
Expected: PASS

- [ ] **Step 7: 提交**

```bash
cd go && git add internal/adminweb/ cmd/admin/web_cmd.go
git commit -m "adminweb: version library; packages go through the spool, never through memory

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 9: 控制台:发布任务与设备进度

**Files:**
- Create: `go/internal/adminweb/rollouts.go`, `go/internal/adminweb/rollouts_test.go`, `go/internal/adminweb/assets/rollout_new.html`, `go/internal/adminweb/assets/rollouts.html`, `go/internal/adminweb/assets/rollout_detail.html`
- Modify: `go/internal/adminweb/server.go`(路由), `go/internal/adminweb/handlers.go`(`pageData` 加 `Rollout *rolloutView`、`Rollouts []rolloutView`、`Candidates []deviceCandidate`、`Artifact *repo.Artifact`)

**Interfaces:**
- Consumes: `ops.CreateRollout/SetRolloutPaused/CancelRollout/ExcludeTarget/RetryTarget`(Task 4)、`repo.Releases`、`repo.Reports`、`model.Status`、`model.AgentCanTakeTargets`。
- Produces: 路由 `GET /rollouts`,`GET /rollouts/new?artifact=<id>`,`GET /rollouts/detail?id=<id>`,`POST /rollouts/create|pause|resume|cancel|exclude|retry|rollback`;纯函数 `targetRowState(t repo.Target, artifact repo.Artifact, report *model.Status, lastSync *time.Time, now time.Time) (label, detail string)`。

- [ ] **Step 1: 写行状态的失败测试**

`go/internal/adminweb/rollouts_test.go`:

```go
package adminweb

import (
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestTargetRowStateNamesEveryStageWithoutGuessing(t *testing.T) {
	now := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	made := now.Add(-30 * time.Minute)
	artifact := repo.Artifact{Product: repo.ProductCodex, Version: "0.42.0"}
	pending := repo.Target{Status: repo.TargetPending, Generation: 2, CreatedAt: made}
	recent := now.Add(-2 * time.Minute)
	old := now.Add(-3 * time.Hour)

	cases := []struct {
		name     string
		target   repo.Target
		report   *model.Status
		lastSync *time.Time
		label    string
	}{
		{"never reported", pending, nil, nil, "待执行(离线)"},
		{"online, not started", pending, &model.Status{CodexVersion: "0.41.0"}, &recent, "待执行(在线)"},
		{"offline for hours", pending, &model.Status{CodexVersion: "0.41.0"}, &old, "待执行(离线)"},
		{"deferred in use", pending, &model.Status{CodexTarget: "0.42.0", CodexTargetGeneration: 2, CodexState: "deferred", CodexDeferReason: "in_use"}, &recent, "延后:正在使用"},
		{"deferred disk", pending, &model.Status{CodexTarget: "0.42.0", CodexTargetGeneration: 2, CodexState: "deferred", CodexDeferReason: "disk"}, &recent, "延后:磁盘不足"},
		{"downloading", pending, &model.Status{CodexTarget: "0.42.0", CodexTargetGeneration: 2, CodexState: "downloading"}, &recent, "下载中"},
		{"installing", pending, &model.Status{CodexTarget: "0.42.0", CodexTargetGeneration: 2, CodexState: "installing"}, &recent, "安装中"},
		{"installed, not yet settled", pending, &model.Status{CodexVersion: "0.42.0"}, &recent, "待健康确认"},
		{"old generation's failure", pending, &model.Status{CodexTarget: "0.42.0", CodexTargetGeneration: 1, CodexState: "failed"}, &recent, "待执行(在线)"},
		{"succeeded", repo.Target{Status: repo.TargetSucceeded, ReportedVersion: "0.42.0"}, nil, nil, "成功"},
		{"failed", repo.Target{Status: repo.TargetFailed, ResultNote: "codex: install: exit status 2"}, nil, nil, "失败"},
		{"excluded", repo.Target{Status: repo.TargetExcluded, ExcludeReason: "机器已停用"}, nil, nil, "已排除"},
		{"cancelled", repo.Target{Status: repo.TargetCancelled}, nil, nil, "已取消"},
		{"superseded", repo.Target{Status: repo.TargetSuperseded}, nil, nil, "已取代"},
	}
	for _, c := range cases {
		label, _ := targetRowState(c.target, artifact, c.report, c.lastSync, now)
		if label != c.label {
			t.Errorf("%s: label = %q, want %q", c.name, label, c.label)
		}
	}
}

func TestRolloutSummaryKeepsExcludedApartFromSuccess(t *testing.T) {
	targets := []repo.Target{
		{Status: repo.TargetSucceeded}, {Status: repo.TargetSucceeded},
		{Status: repo.TargetExcluded}, {Status: repo.TargetFailed}, {Status: repo.TargetPending},
	}
	sum := summariseTargets(targets)
	if sum.Succeeded != 2 || sum.Excluded != 1 || sum.Failed != 1 || sum.Pending != 1 || sum.Total != 5 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.Complete() {
		t.Fatal("a rollout with a pending machine is not complete")
	}
	sum = summariseTargets([]repo.Target{{Status: repo.TargetSucceeded}, {Status: repo.TargetExcluded}})
	if !sum.Complete() {
		t.Fatal("all machines succeeded or were explicitly excluded: complete")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd go && go test ./internal/adminweb/ -run 'TargetRowState|RolloutSummary'`
Expected: FAIL,`undefined: targetRowState`

- [ ] **Step 3: 实现纯函数**

`go/internal/adminweb/rollouts.go`(第一部分):

```go
package adminweb

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ops"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// onlineWithin is how recent a report has to be for a machine to count as
// reachable on this page. Agents heartbeat every few minutes.
const onlineWithin = 15 * time.Minute

// targetRowState says what one machine is doing about its target, from the
// target row and the machine's own report. It reads; it never decides --
// settlement happens in the worker, and this label must agree with it.
func targetRowState(t repo.Target, artifact repo.Artifact, report *model.Status, lastSync *time.Time, now time.Time) (label, detail string) {
	switch t.Status {
	case repo.TargetSucceeded:
		return "成功", "实际版本 " + t.ReportedVersion
	case repo.TargetFailed:
		return "失败", t.ResultNote
	case repo.TargetExcluded:
		return "已排除", t.ExcludeReason
	case repo.TargetCancelled:
		return "已取消", ""
	case repo.TargetSuperseded:
		return "已取代", "已有更新的目标"
	}
	online := lastSync != nil && now.Sub(*lastSync) <= onlineWithin
	waiting := "待执行(离线)"
	if online {
		waiting = "待执行(在线)"
	}
	if report == nil || lastSync == nil {
		return waiting, "从未上报"
	}
	running, seenTarget, seenGen, state, reason := reportFor(t.Product, report)
	if running == artifact.Version && lastSync.After(t.CreatedAt) {
		return "待健康确认", "已是目标版本,等待结算"
	}
	if seenTarget != artifact.Version || seenGen != t.Generation {
		return waiting, "最后联系 " + lastSync.Format("01-02 15:04")
	}
	switch state {
	case "deferred":
		switch reason {
		case "in_use":
			return "延后:正在使用", ""
		case "disk":
			return "延后:磁盘不足", ""
		default:
			return "延后", reason
		}
	case "downloading":
		return "下载中", ""
	case "installing":
		return "安装中", ""
	case "pending":
		return "下载中", "已校验,重启中"
	}
	return waiting, "最后联系 " + lastSync.Format("01-02 15:04")
}

func reportFor(product string, s *model.Status) (running, target string, generation int, state, reason string) {
	if product == repo.ProductAgent {
		return s.AgentVersion, s.AgentUpdateTarget, s.AgentUpdateGeneration, s.AgentUpdateState, ""
	}
	return s.CodexVersion, s.CodexTarget, s.CodexTargetGeneration, s.CodexState, s.CodexDeferReason
}

type targetSummary struct{ Total, Pending, Succeeded, Failed, Excluded, Cancelled, Superseded int }

// Complete is the spec's condition: every machine succeeded, or was
// explicitly excluded with a reason. Excluded is not success and is not
// counted as it.
func (s targetSummary) Complete() bool {
	return s.Total > 0 && s.Pending == 0 && s.Failed == 0 && s.Cancelled == 0
}

func summariseTargets(targets []repo.Target) targetSummary {
	var s targetSummary
	for _, t := range targets {
		s.Total++
		switch t.Status {
		case repo.TargetPending:
			s.Pending++
		case repo.TargetSucceeded:
			s.Succeeded++
		case repo.TargetFailed:
			s.Failed++
		case repo.TargetExcluded:
			s.Excluded++
		case repo.TargetCancelled:
			s.Cancelled++
		case repo.TargetSuperseded:
			s.Superseded++
		}
	}
	return s
}
```

- [ ] **Step 4: 跑纯函数测试**

Run: `cd go && go test ./internal/adminweb/ -run 'TargetRowState|RolloutSummary'`
Expected: PASS

- [ ] **Step 5: 页面与动作**

同文件(第二部分)。视图类型:

```go
type deviceCandidate struct {
	repo.Device
	CodexVersion string
	LastSync     *time.Time
	Online       bool
	Compatible   bool
	Why          string // why not compatible
	HasOpen      bool   // an open target for this product already
}

type targetRow struct {
	repo.Target
	Hostname string
	Label    string
	Detail   string
}

type rolloutView struct {
	repo.Rollout
	Artifact repo.Artifact
	Summary  targetSummary
	Targets  []targetRow
	Paused   bool
}
```

处理器:

- `handleRolloutNew`:读 `artifact` 参数 → `ArtifactByID`;`Devices().List`、`Reports().List`;每台机器算 `Compatible = model.AgentCanTakeTargets(d.AgentVersion) && (artifact.MinAgentVersion == "" || CompareVersions(d.AgentVersion, artifact.MinAgentVersion) >= 0)`,不兼容时 `Why` 写明版本;`HasOpen` 来自 `OpenTarget`;渲染 `rollout_new.html`(表格 + 复选框 + 备注 + 回滚时的 `rollback_of` 隐藏字段)。
- `actionRolloutCreate`:`r.PostForm["device"]` 为设备 ID 列表 → `ops.CreateRollout(RolloutSpec{...Actor: sess.admin.Username, RequestID: s.clientKey(r)})`,成功 303 到 `/rollouts/detail?id=<id>`(用 `requirePostBack` 的写法把目标地址算出来)。
- `handleRollouts`:`ListRollouts(50)`,每条带 `Summary`。
- `handleRolloutDetail`:`RolloutByID`、`ArtifactByID`、`TargetsByRollout`;每台机器 `Devices().ByID`、`Reports().Get`(`ErrNotFound` → nil),`json.Unmarshal(report.Report, &status)`,`Label, Detail = targetRowState(t, artifact, &status, report.LastSyncAt, time.Now())`。
- `actionRolloutPause/Resume/Cancel/Exclude/Retry`:各调 ops 一行,`requirePostBack` 回到 detail 页。
- `actionRolloutRollback`:参数 `id`(要回滚的发布)与 `artifact`(旧版本);设备集合 = 原任务里 `Succeeded` 的机器;`ops.CreateRollout(Kind: repo.RolloutRollback, RollbackOf: id)`。页面上在 detail 里给一个下拉选同产品其他制品(默认最近一个 `stable`)。

模板要点(`rollout_detail.html`):顶部 `{{.Rollout.Artifact.Product}} {{.Rollout.Artifact.Version}}`、`{{.Rollout.Note}}`、操作者/时间/`{{if .Rollout.RollbackOf}}回滚自…{{end}}`;汇总行 `成功 N / 失败 N / 待执行 N / 已排除 N / 已取消 N`,`{{if .Rollout.Summary.Complete}}<span class="ok">已完成</span>{{end}}`;设备表列:机器、状态标签、详情、代次、创建、结束;失败行有"重试"按钮,待执行行有"排除(原因)"表单;页脚 暂停/继续、取消未开始、回滚(选版本 + 输入版本号确认)。

路由(DB 模式块内):

```go
		mux.HandleFunc("/rollouts", s.requireSession(s.handleRollouts))
		mux.HandleFunc("/rollouts/new", s.requireSession(s.handleRolloutNew))
		mux.HandleFunc("/rollouts/detail", s.requireSession(s.handleRolloutDetail))
		mux.HandleFunc("/rollouts/create", s.requirePostBack(backToRollout, s.actionRolloutCreate))
		mux.HandleFunc("/rollouts/pause", s.requirePostBack(backToRollout, s.actionRolloutPause))
		mux.HandleFunc("/rollouts/resume", s.requirePostBack(backToRollout, s.actionRolloutResume))
		mux.HandleFunc("/rollouts/cancel", s.requirePostBack(backToRollout, s.actionRolloutCancel))
		mux.HandleFunc("/rollouts/exclude", s.requirePostBack(backToRollout, s.actionRolloutExclude))
		mux.HandleFunc("/rollouts/retry", s.requirePostBack(backToRollout, s.actionRolloutRetry))
		mux.HandleFunc("/rollouts/rollback", s.requirePostBack(backToRollout, s.actionRolloutRollback))
```

`backToRollout` 仿照 `backToAccount`:从表单 `id` 得到 `/rollouts/detail?id=<id>`。`actionRolloutCreate` 需要把新建的 ID 带回去:让动作函数返回 `(string, error)` 的变体若 `requirePostBack` 不支持,就先 303 到 `/rollouts`(列表最上面即新建的)。

- [ ] **Step 6: 写端到端测试**

追加到 `rollouts_test.go`:

```go
func TestChoosingOneMachineLeavesTheOtherAlone(t *testing.T) {
	s, _ := newDatabaseServer(t)
	cookie := signedIn(t, s)
	ctx := t.Context()
	store := s.dbm.store
	a, _ := s.dbm.ops.RegisterArtifact(ctx, repo.NewArtifact{Product: repo.ProductCodex, Version: "0.42.0",
		SHA256: strings.Repeat("c", 64), SizeBytes: 1, ObjectKey: "k", CreatedBy: "t"}, "t", "r")
	pcA, _ := store.Devices().EnsureByHostname(ctx, "PC-A")
	pcB, _ := store.Devices().EnsureByHostname(ctx, "PC-B")
	store.Devices().MarkSeen(ctx, pcA.ID, "1.2.16", time.Now())
	store.Devices().MarkSeen(ctx, pcB.ID, "1.2.16", time.Now())

	page := dbGet(t, s.Handler(), "/rollouts/new?artifact="+a.ID, cookie)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "PC-A") || !strings.Contains(page.Body.String(), "PC-B") {
		t.Fatalf("new rollout page: %d", page.Code)
	}
	csrf := csrfFrom(t, s, cookie, "/rollouts/new?artifact="+a.ID)
	rec := dbPost(t, s.Handler(), "/rollouts/create", url.Values{"csrf": {csrf}, "product": {"codex"}, "artifact": {a.ID},
		"device": {pcA.ID}, "note": {"试发"}}, cookie)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := store.Releases().OpenTarget(ctx, pcA.ID, repo.ProductCodex); err != nil {
		t.Fatalf("PC-A: %v", err)
	}
	if _, err := store.Releases().OpenTarget(ctx, pcB.ID, repo.ProductCodex); err == nil {
		t.Fatal("PC-B was not chosen and must have no target")
	}
	rollouts, _ := store.Releases().ListRollouts(ctx, 1)
	detail := dbGet(t, s.Handler(), "/rollouts/detail?id="+rollouts[0].ID, cookie)
	body := detail.Body.String()
	if detail.Code != 200 || !strings.Contains(body, "PC-A") || strings.Contains(body, "PC-B") || !strings.Contains(body, "待执行") {
		t.Fatalf("detail page: %d", detail.Code)
	}
}
```

- [ ] **Step 7: 跑 adminweb 全部测试与 `make check`**

Run: `cd go && TEST_PG_DSN='postgres://postgres:devpass@127.0.0.1:5433/aienv_test?sslmode=disable' go test ./internal/adminweb/ && make check`
Expected: PASS;`all checks passed`

- [ ] **Step 8: 提交**

```bash
cd go && git add internal/adminweb/
git commit -m "adminweb: rollouts aimed at chosen machines, with per-machine progress

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 10: 部署、文档、Agent 1.2.16 发布说明

**Files:**
- Modify: `docs/OSS布局.md`(§7、§7b、目录树), `docs/数据库模式部署.md`(环境变量、systemd、磁盘), `docs/操作手册.md`(新增"发布 Agent 与 Codex"一节), `docs/superpowers/plans/2026-09-12-rds-phase0-1.md`(进度表加一行指向本计划)
- Host: `/etc/ai-env-mgr/console.env`, `/etc/systemd/system/ai-env-mgr-console.service`

- [ ] **Step 1: OSS布局.md**

目录树 `_agent/` 下加 `{版本}/agent.exe  按版本留存;agent.exe 是当前全局目标的服务端副本`;§7 加一段"版本库之后":`agent.exe` 由控制台在导出策略前从 `_agent/{版本}/agent.exe` 服务端复制,不再由人直接上传;§7/§7b 各加"按机目标"小节,列出 `_bindings/<机器>` 的 `agentTarget`、`codexTarget` 字段(version、sha256、key、generation),说明"存在即覆盖全局;version 为空表示本机不装;旧 Agent 忽略";写明 RAM 授权不变(`_agent/*` 读、`_codex/*` 读、`_bindings/*` 读)。

- [ ] **Step 2: 数据库模式部署.md**

环境变量表加 `AIENVMGR_SPOOL_DIR`(默认 `/var/lib/ai-env-mgr/spool`,需 2 GB 以上空闲;上传失败会自动清理,但进程被杀时可能残留,`find … -mtime +1 -delete` 放进现有的每日 cron);systemd 单元加:

```ini
ReadWritePaths=/var/lib/ai-env-mgr/spool
```

写明 `MemoryMax=600M` 不变,原因是包不再进内存;"还没做的"里加"Agent 独立更新器与启动失败恢复(另一份计划)"。

- [ ] **Step 3: 操作手册.md 新增"发布 Agent 与 Codex"**

按 spec §3 写成可照做的步骤,每步对应页面:

1. 构建候选:CI 产物、版本号、SHA256、变更说明;`/releases` 上传候选(URL 或文件);核对页面上的 SHA256 与 CI 一致。
2. 测试机验收:spec §3.2 的必测表原样搬进来;在 `/releases` 登记验收(测试机名、日期)。
3. 试发:`/releases` → 创建发布 → 勾 1~2 台(页面只让勾 agent ≥ 1.2.16 的机器)→ 观察 `/rollouts/detail`;成功 = 实际版本匹配且更新后正常上报;至少观察一个工作日。
4. 扩大:同一版本再建一个发布任务勾其余机器(范围是快照,之后新机器不自动加入)。
5. 收尾:全部成功或逐台排除(必须写原因);离线机器保持待执行。
6. 暂停与回滚:暂停只拦未开始的;回滚 = 在 detail 页选旧版本再建任务;写明"回滚不等于降级一定可用,制品记录里的已验证回滚版本为准"。
7. 旧 Agent(< 1.2.16):只能走"设为全局目标";首个 1.2.16 必须这样发,且这一步没有自动恢复保护(spec §7)。

- [ ] **Step 4: 主机改动**

```bash
sudo mkdir -p /var/lib/ai-env-mgr/spool && sudo chmod 700 /var/lib/ai-env-mgr/spool
sudo sed -i '/^AIENVMGR_WORKER=/a AIENVMGR_SPOOL_DIR=/var/lib/ai-env-mgr/spool' /etc/ai-env-mgr/console.env
sudo sed -i '/^MemoryMax=/a ReadWritePaths=/var/lib/ai-env-mgr/spool' /etc/systemd/system/ai-env-mgr-console.service
cd go && go build -trimpath -o /tmp/ai-env-admin ./cmd/admin && sudo install -m 755 /tmp/ai-env-admin /usr/local/bin/ai-env-admin
sudo systemctl daemon-reload && sudo systemctl restart ai-env-mgr-console
sleep 2 && curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8090/healthz
sudo journalctl -u ai-env-mgr-console -n 5 --no-pager
```

Expected: healthz `200`;日志里有迁移 0006 已应用(控制台以 `aienv_app` 启动只读迁移记录,所以先以特权账号跑 `AIENVMGR_DB_DSN='postgres://postgres:...' go run ./cmd/migrate up`,再重启);回滚:`go run ./cmd/migrate down 5`,换回上一个二进制。

- [ ] **Step 5: 用真实包走一遍(Worker 仍 OFF)**

在 `/releases` 上传当前 agent `v1.2.15` 的 GitHub release 资产作 agent 候选(不设目标),看:内存 `systemctl status` 的 Memory 峰值 < 200 MB;OSS 出现 `_agent/1.2.15/agent.exe`;版本库一行,SHA256 与 GitHub 一致。再用一份 Codex 安装器(约 700 MB)重复,峰值仍 < 300 MB。记录两个数字到本计划"实施记录"。

- [ ] **Step 6: Agent 1.2.16 发布说明**

在 `docs/superpowers/plans/2026-09-12-rds-phase0-1.md` 的进度表后加:

> **Agent 1.2.16**(本计划 Task 5):移除 Claude Code(`81609e1`);读绑定里的按机目标;标记带代次;上报目标、代次、延后原因、自更新状态。由旧全局通道发布:`/releases` 上传 → 测试机验收 → 设为全局目标。这次发布没有自动恢复保护。

- [ ] **Step 7: 提交**

```bash
git add docs/ && git commit -m "docs: release pipeline runbook, spool directory, per-machine targets in the OSS layout

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

## 验收清单(spec §8 第 1、2、4 项)

| 项 | 怎么证明 | 对应任务 |
|---|---|---|
| 上传候选不改变设备 | `TestRegisteringAnArtifactChangesNoTarget`;真机:上传后策略与绑定对象字节不变 | 4、8 |
| 不同内容不能覆盖同版本 | `TestAVersionNamesOneSetOfBytes`;页面提前拒绝已占用版本 | 3、8 |
| 大包不整包驻留内存 | Task 10 Step 5 记录的 Memory 峰值;`MemoryMax=600M` 未改 | 1、2、8 |
| 只选 A 时 B 不更新 | `TestARolloutAimsOnlyAtTheChosenMachines`、`TestChoosingOneMachineLeavesTheOtherAlone`;真机:两台 1.2.16,只勾一台 | 4、9 |
| 离线不误报成功 | `TestAStaleReportSettlesNothing`、`TestDeferredAndOfflineStayPending`;页面"待执行(离线)" | 7、9 |
| 旧回执不能覆盖新目标 | `TestOneOpenTargetPerDeviceAndGenerationsClimb`、`TestAFailureForThisGenerationFailsTheTarget` | 3、7 |
| 暂停、回滚、排除、重试 | `TestExcludeAndRetryAreExplicitAndRecorded`;真机走 detail 页四个按钮 | 4、9 |
| 旧 Agent 不受影响 | 1.2.15 机器对带 `agentTarget` 的绑定对象照常工作(字段被忽略);`TestBindingAndRestartReachTheMachine` 不变 | 5、6 |

spec §8 第 3 项(独立更新器与失败恢复)在 `2026-09-1x-agent-updater.md` 里另行验收;在它通过之前,Agent 的定向发布只用于试发,不做全机队。

## 实施记录

(执行时填写:日期、任务、提交、验收结果、内存峰值。)
