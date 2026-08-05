# 会话采集集成进 agent — 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 agent 每个 sync 周期把绑定员工的 `.claude`/`.codex` 原始会话文件增量上传到 `agent_workdir/{员工}/data_collect/`，逻辑与 `/root/sun_home/ai-usage-analysis/scripts/collect_to_oss.py` 一致。

**Architecture:** 新增一个平台无关的 `Collector` 单元（`internal/agentcore/collect.go`），去重靠本地 state（因为对 `data_collect/` 只有写权限，不能查 OSS），去抖/since 与脚本一致。`Syncer.RunOnce` 在 credentials 之后、status 之前调用它，仅当 `policy.collectEnabled` 为真。真实文件遍历由 `cmd/agent` 提供，测试用 fake。

**Tech Stack:** Go 1.x，标准库 + 现有 `ossclient`/`model`/`status`；测试用 `go test`。

## Global Constraints

- 模块路径 `github.com/TEENet-io/airlock`；所有 OSS 键必须经 `internal/ossclient` 的 helper 生成，不手工拼接。
- agent 对 `data_collect/` **只写不读**：Collector 只用 `Put`，绝不 `Get`/`List`/`Head` 该前缀。
- **原文照传**，不解析、不脱敏。
- 采集功能**默认关闭**（`collectEnabled` 默认 `false`）；代码合入不等于启用，权限与开关由管理员在功能落地时再开（见 `OSS布局.md` §6）。
- 只采**绑定员工**一个 profile；未绑定或该员工无本地 profile 时不采。
- 去重靠本地 state；传失败不写 state（下周期重试）；源文件删除只清 state，OSS 副本保留。
- `go test ./...` 必须全绿；新代码平台无关，能在 Linux 上测。
- 提交信息结尾附带：
  `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`
  `Claude-Session: https://claude.ai/code/session_01FqVXPP67Qb7ma1Vz4qcZgg`

---

### Task 1: 共享类型与键 helper（model + ossclient）

**Files:**
- Modify: `go/internal/model/model.go`（`Policy`、`Status` 加字段）
- Modify: `go/internal/ossclient/client.go`（加 `DataCollectKey`）
- Test: `go/internal/ossclient/client_test.go`（若不存在则创建）

**Interfaces:**
- Produces:
  - `model.Policy` 新增 `CollectEnabled bool` / `CollectQuietSeconds int` / `CollectSince string`
  - `model.Status` 新增 `CollectEnabled bool` / `CollectUploaded int`
  - `ossclient.DataCollectKey(user, rel string) string` — 返回 `agent_workdir/{user}/data_collect/{rel}`，`rel` 用正斜杠、清理 `..`

- [ ] **Step 1: 给 ossclient 写失败测试**

在 `go/internal/ossclient/client_test.go` 追加（无该文件则新建，`package ossclient`）：

```go
func TestDataCollectKey(t *testing.T) {
	cases := []struct{ user, rel, want string }{
		{"work1", ".claude/projects/p/a.jsonl", "agent_workdir/work1/data_collect/.claude/projects/p/a.jsonl"},
		{"work1", ".codex/sessions/2026/rollout-x.jsonl", "agent_workdir/work1/data_collect/.codex/sessions/2026/rollout-x.jsonl"},
		{"work1", `.claude\projects\a.jsonl`, "agent_workdir/work1/data_collect/.claude/projects/a.jsonl"},   // 反斜杠归一
		{"work1", "../../etc/passwd", "agent_workdir/work1/data_collect/etc/passwd"},                          // .. 不得逃逸
	}
	for _, c := range cases {
		if got := DataCollectKey(c.user, c.rel); got != c.want {
			t.Errorf("DataCollectKey(%q,%q)=%q want %q", c.user, c.rel, got, c.want)
		}
	}
}
```

- [ ] **Step 2: 运行，确认失败**

Run: `cd go && go test ./internal/ossclient/ -run TestDataCollectKey -v`
Expected: 编译失败 `undefined: DataCollectKey`。

- [ ] **Step 3: 实现 `DataCollectKey`**

在 `go/internal/ossclient/client.go` 的 `DataCollectPrefix` 之后追加：

```go
// DataCollectKey builds the object key for one collected session file inside
// an employee's data_collect directory. rel is the file's path relative to the
// employee profile (for example ".claude/projects/p/a.jsonl").
//
// rel is rooted and cleaned so a crafted "../" cannot climb out of the
// employee's directory; agents have write-only access here, and this keeps a
// bad relative path from landing an object anywhere else in the bucket.
func DataCollectKey(user, rel string) string {
	rel = strings.ReplaceAll(rel, `\`, "/")
	clean := strings.TrimPrefix(path.Clean("/"+rel), "/")
	return DataCollectPrefix(user) + clean
}
```

（`path` 与 `strings` 已在该文件 import。）

- [ ] **Step 4: 给 model 加字段**

`go/internal/model/model.go` 中 `Policy` 结构体，在 `UpdatedAt` 之前插入：

```go
	// Collection of raw AI session files. Off by default: the code ships
	// before the feature is enabled, and the agent's write-only permission on
	// data_collect is added only when an administrator turns this on. See
	// OSS布局.md §6.
	CollectEnabled      bool   `json:"collectEnabled"`
	CollectQuietSeconds int    `json:"collectQuietSeconds,omitempty"` // debounce; 0 -> default 60
	CollectSince        string `json:"collectSince,omitempty"`        // YYYY-MM-DD UTC; empty -> all history
```

`Status` 结构体，在 `AppLockerMode` 之后、`Errors` 之前插入：

```go
	CollectEnabled  bool `json:"collectEnabled"`
	CollectUploaded int  `json:"collectUploaded"`
```

`DefaultPolicy()` 不改：`CollectEnabled` 保持零值 `false`。

- [ ] **Step 5: 运行 ossclient 测试 + 全量编译**

Run: `cd go && go test ./internal/ossclient/ -run TestDataCollectKey -v && go build ./...`
Expected: PASS 且编译通过。

- [ ] **Step 6: 提交**

```bash
git add go/internal/ossclient/client.go go/internal/ossclient/client_test.go go/internal/model/model.go
git commit -m "feat(collect): add DataCollectKey and collection policy/status fields"
```

---

### Task 2: Collector 核心逻辑（agentcore/collect.go）

**Files:**
- Create: `go/internal/agentcore/collect.go`
- Test: `go/internal/agentcore/collect_test.go`

**Interfaces:**
- Consumes: `ossclient.DataCollectKey`（Task 1）；`Machine` 接口（`sync.go` 已定义，含 `ProfileDir(user) string`）。
- Produces:
  - `type Putter interface { Put(key string, data []byte) error }`
  - `type SessionFile struct { Path, Rel string; ModTime time.Time; Size int64 }`
  - `type FileSource interface { Sessions(profileDir string) ([]SessionFile, error); Open(path string) (io.ReadCloser, error) }`
  - `type CollectResult struct { Uploaded int; Errors []string }`
  - `type Collector struct { Store Putter; Source FileSource; Machine Machine; StateDir string; Now func() time.Time }`
  - `type CollectRunner interface { CollectOnce(user string, quietSeconds int, since string) CollectResult }`
  - `func (c *Collector) CollectOnce(user string, quietSeconds int, since string) CollectResult`

- [ ] **Step 1: 写第一个失败测试（首次全传 + 键结构）**

创建 `go/internal/agentcore/collect_test.go`：

```go
package agentcore

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSource returns a fixed set of session files with in-memory content.
type fakeSource struct {
	files   []SessionFile
	content map[string][]byte // keyed by Path
	openErr error
}

func (s *fakeSource) Sessions(profileDir string) ([]SessionFile, error) { return s.files, nil }
func (s *fakeSource) Open(path string) (io.ReadCloser, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}
	return io.NopCloser(strings.NewReader(string(s.content[path]))), nil
}

// sf builds a SessionFile plus its content in one call.
func (s *fakeSource) add(rel, body string, mod time.Time) {
	p := filepath.Join("/profiles/work1", filepath.FromSlash(rel))
	s.files = append(s.files, SessionFile{Path: p, Rel: rel, ModTime: mod, Size: int64(len(body))})
	if s.content == nil {
		s.content = map[string][]byte{}
	}
	s.content[p] = []byte(body)
}

func newCollector(t *testing.T, store *fakeStore, src *fakeSource, now time.Time) *Collector {
	t.Helper()
	return &Collector{
		Store:    store,
		Source:   src,
		Machine:  &fakeMachine{name: "DESKTOP-A", localUsers: []string{"work1"}, profileDir: "/profiles"},
		StateDir: t.TempDir(),
		Now:      func() time.Time { return now },
	}
}

func TestCollectFirstRunUploadsAllWithCorrectKeys(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour) // well past the debounce window
	src := &fakeSource{}
	src.add(".claude/projects/p/a.jsonl", "line1\n", old)
	src.add(".codex/sessions/2026/rollout-x.jsonl", "line2\n", old)
	store := newFakeStore()

	res := newCollector(t, store, src, now).CollectOnce("work1", 60, "")

	if res.Uploaded != 2 {
		t.Fatalf("uploaded=%d errors=%v want 2", res.Uploaded, res.Errors)
	}
	if _, ok := store.puts["agent_workdir/work1/data_collect/.claude/projects/p/a.jsonl"]; !ok {
		t.Errorf("claude key missing; puts=%v", keysOf(store.puts))
	}
	if _, ok := store.puts["agent_workdir/work1/data_collect/.codex/sessions/2026/rollout-x.jsonl"]; !ok {
		t.Errorf("codex key missing; puts=%v", keysOf(store.puts))
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
```

- [ ] **Step 2: 运行，确认失败**

Run: `cd go && go test ./internal/agentcore/ -run TestCollectFirstRun -v`
Expected: 编译失败（`Collector`、`SessionFile` 等未定义）。

- [ ] **Step 3: 实现 Collector（含 state / 去抖 / since / prune）**

创建 `go/internal/agentcore/collect.go`：

```go
package agentcore

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/TEENet-io/airlock/internal/ossclient"
)

// Putter is the write-only slice of the store the collector uses. It is
// deliberately narrower than Store: the agent has no read access to
// data_collect, so a leaked machine key cannot pull back anyone's
// conversations. Encoding that as a type keeps the guarantee from eroding.
type Putter interface {
	Put(key string, data []byte) error
}

// SessionFile is one raw AI session file found under an employee profile.
type SessionFile struct {
	Path    string    // absolute source path on this machine
	Rel     string    // path relative to the profile, posix (".claude/projects/...")
	ModTime time.Time // last modification time
	Size    int64     // size in bytes
}

// FileSource enumerates and reads session files. It is an interface so the
// collector can be tested with in-memory files on any platform.
type FileSource interface {
	Sessions(profileDir string) ([]SessionFile, error)
	Open(path string) (io.ReadCloser, error)
}

// CollectResult reports one collection pass.
type CollectResult struct {
	Uploaded int
	Errors   []string
}

// CollectRunner is what Syncer needs from a collector, so the sync test can
// substitute a fake without building a real Collector.
type CollectRunner interface {
	CollectOnce(user string, quietSeconds int, since string) CollectResult
}

const collectStateFile = "collect-state.json"

// defaultQuietSeconds mirrors collect_to_oss.py's --quiet default: only upload
// a file that has been idle this long, so a session still being written is not
// re-uploaded half-formed on every pass.
const defaultQuietSeconds = 60

// Collector uploads changed session files to an employee's data_collect
// directory. Deduplication is local-only (see Putter): it keeps a state file
// of (mtime,size) per source path and uploads a file only when that changes.
type Collector struct {
	Store    Putter
	Source   FileSource
	Machine  Machine
	StateDir string
	Now      func() time.Time
}

// sig is the change signature stored per source file: modification time (unix
// nanoseconds) and size. An append always changes size, so the pair is enough.
type sig [2]int64

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Collector) statePath() string { return filepath.Join(c.StateDir, collectStateFile) }

func (c *Collector) loadState() map[string]sig {
	state := map[string]sig{}
	data, err := os.ReadFile(c.statePath())
	if err != nil {
		return state
	}
	_ = json.Unmarshal(data, &state) // a corrupt state file just means "re-upload"
	return state
}

func (c *Collector) saveState(state map[string]sig) {
	if err := os.MkdirAll(c.StateDir, 0o700); err != nil {
		return
	}
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	tmp := c.statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, c.statePath())
}

// CollectOnce runs one pass for a single employee. Mirrors run_once() in
// collect_to_oss.py: skip unchanged, debounce still-being-written files, skip
// history before `since`, retry failed uploads next pass, and drop deleted
// sources from state while leaving their uploaded copies in OSS.
func (c *Collector) CollectOnce(user string, quietSeconds int, since string) CollectResult {
	if quietSeconds <= 0 {
		quietSeconds = defaultQuietSeconds
	}
	var res CollectResult
	profileDir := c.Machine.ProfileDir(user)
	files, err := c.Source.Sessions(profileDir)
	if err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("collect: list sessions: %v", err))
		return res
	}

	state := c.loadState()
	now := c.now()
	seen := map[string]bool{}

	for _, f := range files {
		seen[f.Path] = true
		cur := sig{f.ModTime.UnixNano(), f.Size}

		// since: skip files whose UTC date is before the cutoff (as in the script).
		if since != "" && f.ModTime.UTC().Format("2006-01-02") < since {
			continue
		}
		// unchanged since last upload.
		if old, ok := state[f.Path]; ok && old == cur {
			continue
		}
		// debounce: modified within the quiet window -> still being written.
		if now.Sub(f.ModTime) < time.Duration(quietSeconds)*time.Second {
			continue
		}

		rc, err := c.Source.Open(f.Path)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("collect: open %s: %v", f.Rel, err))
			continue // no state write -> retried next pass
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("collect: read %s: %v", f.Rel, err))
			continue
		}
		if err := c.Store.Put(ossclient.DataCollectKey(user, f.Rel), data); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("collect: upload %s: %v", f.Rel, err))
			continue // failure -> not recorded, retried next pass
		}
		state[f.Path] = cur
		res.Uploaded++
	}

	// Source deleted: forget it locally. The uploaded object stays in OSS,
	// subject to the bucket's lifecycle/retention policy.
	for p := range state {
		if !seen[p] {
			delete(state, p)
		}
	}
	c.saveState(state)
	return res
}
```

- [ ] **Step 4: 运行首个测试，确认通过**

Run: `cd go && go test ./internal/agentcore/ -run TestCollectFirstRun -v`
Expected: PASS。

- [ ] **Step 5: 补齐行为测试（未变化/改动/去抖/失败重试/删除/since）**

在 `collect_test.go` 追加：

```go
func TestCollectSkipsUnchanged(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	src := &fakeSource{}
	src.add(".claude/projects/p/a.jsonl", "x\n", old)
	store := newFakeStore()
	c := newCollector(t, store, src, now)

	if r := c.CollectOnce("work1", 60, ""); r.Uploaded != 1 {
		t.Fatalf("first pass uploaded=%d want 1", r.Uploaded)
	}
	if r := c.CollectOnce("work1", 60, ""); r.Uploaded != 0 {
		t.Fatalf("second pass uploaded=%d want 0 (unchanged)", r.Uploaded)
	}
}

func TestCollectReuploadsModified(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	src := &fakeSource{}
	src.add(".claude/projects/p/a.jsonl", "x\n", old)
	store := newFakeStore()
	c := newCollector(t, store, src, now)
	c.CollectOnce("work1", 60, "")

	// same path, larger size + newer (still older than quiet window) mtime
	src.files[0].Size = 99
	src.files[0].ModTime = now.Add(-2 * time.Minute)
	if r := c.CollectOnce("work1", 60, ""); r.Uploaded != 1 {
		t.Fatalf("uploaded=%d want 1 after modification", r.Uploaded)
	}
}

func TestCollectDebounceSkipsFreshFile(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	src := &fakeSource{}
	src.add(".claude/projects/p/a.jsonl", "x\n", now.Add(-10*time.Second)) // within 60s quiet
	store := newFakeStore()

	if r := newCollector(t, store, src, now).CollectOnce("work1", 60, ""); r.Uploaded != 0 {
		t.Fatalf("uploaded=%d want 0 (still being written)", r.Uploaded)
	}
}

func TestCollectFailedUploadRetries(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	src := &fakeSource{}
	src.add(".claude/projects/p/a.jsonl", "x\n", old)
	store := newFakeStore()
	store.putErr = errNotFound // any error: proves state was not recorded
	c := newCollector(t, store, src, now)

	if r := c.CollectOnce("work1", 60, ""); len(r.Errors) == 0 {
		t.Fatal("want an error on failed upload")
	}
	store.putErr = nil
	if r := c.CollectOnce("work1", 60, ""); r.Uploaded != 1 {
		t.Fatalf("uploaded=%d want 1 on retry", r.Uploaded)
	}
}

func TestCollectPrunesDeletedSourceKeepsOSSCopy(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	src := &fakeSource{}
	src.add(".claude/projects/p/a.jsonl", "x\n", old)
	store := newFakeStore()
	c := newCollector(t, store, src, now)
	c.CollectOnce("work1", 60, "")

	src.files = nil // source deleted
	c.CollectOnce("work1", 60, "")

	// OSS copy remains
	if _, ok := store.puts["agent_workdir/work1/data_collect/.claude/projects/p/a.jsonl"]; !ok {
		t.Error("uploaded object should remain after source deletion")
	}
	// state no longer tracks it -> if the same path reappears it uploads again
	src.add(".claude/projects/p/a.jsonl", "x\n", old)
	if r := c.CollectOnce("work1", 60, ""); r.Uploaded != 1 {
		t.Fatalf("uploaded=%d want 1 after source reappears", r.Uploaded)
	}
}

func TestCollectSinceSkipsHistory(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	src := &fakeSource{}
	src.add(".claude/projects/p/old.jsonl", "x\n", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	src.add(".claude/projects/p/new.jsonl", "y\n", time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC))
	store := newFakeStore()

	r := newCollector(t, store, src, now).CollectOnce("work1", 60, "2026-08-01")
	if r.Uploaded != 1 {
		t.Fatalf("uploaded=%d want 1 (only files on/after 2026-08-01)", r.Uploaded)
	}
	if _, ok := store.puts["agent_workdir/work1/data_collect/.claude/projects/p/new.jsonl"]; !ok {
		t.Error("the newer file should have been uploaded")
	}
}
```

- [ ] **Step 6: 运行全部 collector 测试**

Run: `cd go && go test ./internal/agentcore/ -run TestCollect -v`
Expected: 全部 PASS。

- [ ] **Step 7: 提交**

```bash
git add go/internal/agentcore/collect.go go/internal/agentcore/collect_test.go
git commit -m "feat(collect): add Collector with local-state dedup, debounce, since, prune"
```

---

### Task 3: 接入 Syncer.RunOnce 与 status

**Files:**
- Modify: `go/internal/agentcore/sync.go`（`Syncer` 加 `Collector CollectRunner`；`RunOnce` 调用）
- Modify: `go/internal/status/status.go`（`Report` 加字段；`Build` 映射）
- Test: `go/internal/agentcore/sync_test.go`（加两个用例）

**Interfaces:**
- Consumes: `CollectRunner`、`CollectResult`（Task 2）；`model.Policy.CollectEnabled` 等（Task 1）。
- Produces: `Syncer.Collector CollectRunner` 字段；`status.Report{ CollectEnabled bool; CollectUploaded int }`。

- [ ] **Step 1: 写失败测试（启用/未启用）**

在 `go/internal/agentcore/sync_test.go` 追加。先加一个 fake 采集器：

```go
type fakeCollector struct {
	calls    []string // users it was called for
	uploaded int
}

func (f *fakeCollector) CollectOnce(user string, quietSeconds int, since string) CollectResult {
	f.calls = append(f.calls, user)
	return CollectResult{Uploaded: f.uploaded}
}
```

再加两个用例（复用现有 `bind`、`newFakeStore`、`newSyncer`、`fakeApplier`；`policy` 的写法参照该文件中已有的 policy 落盘辅助——若已有 `putPolicy`/`setPolicy` 就用它，否则内联 `store.set(ossclient.PolicyKey(), mustJSON(pol), "petag")`）：

```go
func TestRunOnceRunsCollectorWhenEnabled(t *testing.T) {
	store := newFakeStore()
	app := &fakeApplier{}
	s := newSyncer(t, store, app)
	// bound to work1, which is a local user in newSyncer's fakeMachine
	bind(t, store, "DESKTOP-A", "work1")
	pol := model.DefaultPolicy()
	pol.CollectEnabled = true
	store.set(ossclient.PolicyKey(), mustJSON(t, pol), "petag")
	col := &fakeCollector{uploaded: 3}
	s.Collector = col

	st, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if len(col.calls) != 1 || col.calls[0] != "work1" {
		t.Fatalf("collector calls=%v want [work1]", col.calls)
	}
	if !st.CollectEnabled || st.CollectUploaded != 3 {
		t.Fatalf("status collectEnabled=%v uploaded=%d want true/3", st.CollectEnabled, st.CollectUploaded)
	}
}

func TestRunOnceSkipsCollectorWhenDisabled(t *testing.T) {
	store := newFakeStore()
	app := &fakeApplier{}
	s := newSyncer(t, store, app)
	bind(t, store, "DESKTOP-A", "work1")
	// DefaultPolicy has CollectEnabled == false
	store.set(ossclient.PolicyKey(), mustJSON(t, model.DefaultPolicy()), "petag")
	col := &fakeCollector{uploaded: 3}
	s.Collector = col

	st, err := s.RunOnce()
	if err != nil {
		t.Fatal(err)
	}
	if len(col.calls) != 0 {
		t.Fatalf("collector should not run when disabled; calls=%v", col.calls)
	}
	if st.CollectEnabled || st.CollectUploaded != 0 {
		t.Fatalf("status collectEnabled=%v uploaded=%d want false/0", st.CollectEnabled, st.CollectUploaded)
	}
}

// mustJSON marshals v or fails the test.
func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
```

> 注：若 `sync_test.go` 里已存在等价的 policy 落盘辅助或 `mustJSON`，直接复用、删掉此处重复定义，避免重定义编译错误。

- [ ] **Step 2: 运行，确认失败**

Run: `cd go && go test ./internal/agentcore/ -run TestRunOnce -v`
Expected: 编译失败（`Syncer.Collector` / `Status.CollectUploaded` 未定义）。

- [ ] **Step 3: status 加字段并映射**

`go/internal/status/status.go`：`Report` 结构体在 `Errors` 之前加：

```go
	CollectEnabled  bool
	CollectUploaded int
```

`Build` 的 `return model.Status{...}` 中，在 `AppLockerMode:` 之后加：

```go
		CollectEnabled:  r.CollectEnabled,
		CollectUploaded: r.CollectUploaded,
```

- [ ] **Step 4: Syncer 加字段并在 RunOnce 调用**

`go/internal/agentcore/sync.go`：`Syncer` 结构体加字段：

```go
	// Collector uploads raw session files when policy.CollectEnabled is set.
	// Optional: nil means collection is not wired in (tests, older builds).
	Collector CollectRunner
```

在 `RunOnce` 的 credentials 块结束（`}` 关闭 `else { ... }` 的 `if !bound` 分支）之后、`// ---- status ----` 之前，插入：

```go
	// ---- collection ----
	// Only when enabled by policy, only for a bound employee whose profile is
	// present: there is nothing to scan otherwise, and the object key needs the
	// employee's directory.
	collectUploaded := 0
	if pol.CollectEnabled && s.Collector != nil && bound && boundUserExists {
		cr := s.Collector.CollectOnce(binding.User, pol.CollectQuietSeconds, pol.CollectSince)
		collectUploaded = cr.Uploaded
		errs = append(errs, cr.Errors...)
	}
```

在 `status.Build(status.Report{...})` 的字段里，加：

```go
		CollectEnabled:  pol.CollectEnabled,
		CollectUploaded: collectUploaded,
```

- [ ] **Step 5: 运行 sync 测试 + 全量测试**

Run: `cd go && go test ./internal/agentcore/ ./internal/status/ -v && go build ./...`
Expected: 全部 PASS，编译通过。

- [ ] **Step 6: 提交**

```bash
git add go/internal/agentcore/sync.go go/internal/agentcore/sync_test.go go/internal/status/status.go
git commit -m "feat(collect): run collector in sync cycle and report it in status"
```

---

### Task 4: 真实文件遍历与 agent 接线（cmd/agent）

**Files:**
- Create: `go/cmd/agent/collect.go`
- Modify: `go/cmd/agent/main.go`（`newSyncer` 注入 `Collector`；`printStatus` 加一行）
- Test: `go/cmd/agent/collect_test.go`

**Interfaces:**
- Consumes: `agentcore.FileSource`、`agentcore.SessionFile`、`agentcore.Collector`（Task 2）。
- Produces: `localFileSource`（实现 `agentcore.FileSource`），遍历 `<profile>/.claude/projects/**/*.jsonl` 与 `<profile>/.codex/sessions/**/rollout-*.jsonl`。

- [ ] **Step 1: 写失败测试（真实目录遍历）**

创建 `go/cmd/agent/collect_test.go`：

```go
package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestLocalFileSourceFindsSessions(t *testing.T) {
	profile := t.TempDir()
	mk := func(rel, body string) {
		p := filepath.Join(profile, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk(".claude/projects/proj/a.jsonl", "1\n")
	mk(".codex/sessions/2026/08/rollout-abc.jsonl", "2\n")
	mk(".codex/sessions/2026/08/notes.txt", "ignore\n")        // wrong extension
	mk(".codex/sessions/2026/08/other.jsonl", "ignore\n")      // not rollout-*
	mk(".claude/projects/proj/sub/b.jsonl", "3\n")

	files, err := localFileSource{}.Sessions(profile)
	if err != nil {
		t.Fatal(err)
	}
	var rels []string
	for _, f := range files {
		rels = append(rels, f.Rel)
		if f.Size == 0 {
			t.Errorf("expected non-zero size for %s", f.Rel)
		}
	}
	sort.Strings(rels)
	want := []string{
		".claude/projects/proj/a.jsonl",
		".claude/projects/proj/sub/b.jsonl",
		".codex/sessions/2026/08/rollout-abc.jsonl",
	}
	if len(rels) != len(want) {
		t.Fatalf("rels=%v want %v", rels, want)
	}
	for i := range want {
		if rels[i] != want[i] {
			t.Fatalf("rels=%v want %v", rels, want)
		}
	}
}

func TestLocalFileSourceMissingDirsAreNotErrors(t *testing.T) {
	files, err := localFileSource{}.Sessions(t.TempDir()) // empty profile
	if err != nil {
		t.Fatalf("missing .claude/.codex must not error: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("want 0 files, got %d", len(files))
	}
}
```

- [ ] **Step 2: 运行，确认失败**

Run: `cd go && go test ./cmd/agent/ -run TestLocalFileSource -v`
Expected: 编译失败（`localFileSource` 未定义）。

- [ ] **Step 3: 实现 localFileSource**

创建 `go/cmd/agent/collect.go`：

```go
package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/TEENet-io/airlock/internal/agentcore"
)

// localFileSource finds and reads the bound employee's AI session files.
//
// It mirrors the two globs in collect_to_oss.py:
//   <profile>/.claude/projects/**/*.jsonl
//   <profile>/.codex/sessions/**/rollout-*.jsonl
// A missing .claude or .codex directory is normal (the employee may use only
// one tool) and is not an error.
type localFileSource struct{}

func (localFileSource) Sessions(profileDir string) ([]agentcore.SessionFile, error) {
	var out []agentcore.SessionFile

	roots := []struct {
		dir   string
		match func(name string) bool
	}{
		{filepath.Join(profileDir, ".claude", "projects"),
			func(n string) bool { return strings.HasSuffix(n, ".jsonl") }},
		{filepath.Join(profileDir, ".codex", "sessions"),
			func(n string) bool { return strings.HasPrefix(n, "rollout-") && strings.HasSuffix(n, ".jsonl") }},
	}

	for _, r := range roots {
		err := filepath.WalkDir(r.dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable dir/entry: skip, do not abort the whole walk
			}
			if d.IsDir() || !r.match(d.Name()) {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			rel, err := filepath.Rel(profileDir, path)
			if err != nil {
				return nil
			}
			out = append(out, agentcore.SessionFile{
				Path:    path,
				Rel:     filepath.ToSlash(rel),
				ModTime: info.ModTime(),
				Size:    info.Size(),
			})
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	return out, nil
}

func (localFileSource) Open(path string) (io.ReadCloser, error) {
	return os.Open(path)
}
```

- [ ] **Step 4: 运行遍历测试，确认通过**

Run: `cd go && go test ./cmd/agent/ -run TestLocalFileSource -v`
Expected: PASS。

- [ ] **Step 5: 把 Collector 接进 newSyncer**

`go/cmd/agent/main.go` 的 `newSyncer`，在构造 `&agentcore.Syncer{...}` 时加一个字段（放在 `FallbackInterval` 之后）：

```go
		Collector: &agentcore.Collector{
			Store:    store,
			Source:   localFileSource{},
			Machine:  machine,
			StateDir: stateDir(),
		},
```

- [ ] **Step 6: printStatus 增加采集一行**

`go/cmd/agent/main.go` 的 `printStatus`，在 `block=...` 那行 `Printf` 之后加：

```go
	fmt.Printf("collect=%v uploaded=%d\n", st.CollectEnabled, st.CollectUploaded)
```

- [ ] **Step 7: 全量编译 + 测试**

Run: `cd go && go build ./... && go test ./...`
Expected: 编译通过，全部 PASS。

- [ ] **Step 8: 提交**

```bash
git add go/cmd/agent/collect.go go/cmd/agent/collect_test.go go/cmd/agent/main.go
git commit -m "feat(collect): real session-file source and wire collector into the agent"
```

---

### Task 5: 文档同步（布局 / RAM / 手册）

**Files:**
- Modify: `OSS布局.md`（§6 标注"已实现，默认关闭"）
- Modify: `dist/ram-policy-agent.json`（把 data_collect 写权限作为**注释掉/待启用**说明，或单独文件——见下）
- Modify: `操作手册.md`（新增采集小节：开关、权限、验证）
- Modify: `架构说明.md`（如提及数据流，补一句采集）

**Interfaces:** 无代码接口；本任务只更新文档，保证与实现一致。

- [ ] **Step 1: 更新 `OSS布局.md` §6**

把 §6 开头的"本版不实现"改为"本版已实现采集端，默认关闭"，并保留 §6 里那条 `PutObject` 授权原文；补一句：启用步骤 = 在 `dist/ram-policy-agent.json` 增加该条 → 把 `policy.json` 的 `collectEnabled` 置 `true`。键结构写明：`agent_workdir/{员工}/data_collect/.claude/...` 与 `.../.codex/...`，与源目录一致。

- [ ] **Step 2: 更新 `dist/ram-policy-agent.json` 说明**

不要默认放开写权限。在同目录 `dist/部署说明.md`（或该 json 旁）写明：采集功能启用时，向 agent 策略追加：

```json
{ "Effect": "Allow", "Action": ["oss:PutObject"],
  "Resource": ["acs:oss:*:*:<your-bucket>/agent_workdir/*/data_collect/*"] }
```

"只给写、绝不给读"。功能启用前不要加。

- [ ] **Step 3: 更新 `操作手册.md`**

新增"会话采集"小节：
- 作用：把绑定员工的 `.claude`/`.codex` 原始会话增量传到 `data_collect/`，供离线分析；原文不脱敏。
- 开关：`admin.exe policy` 下发的 `collectEnabled`（默认 false）；可选 `collectQuietSeconds`（默认 60）、`collectSince`（`YYYY-MM-DD`，默认采全部历史）。
- 前置：先在 RAM 策略加 `PutObject` 写权限，再开开关。
- 验证：`agent.exe status` 看 `collect=true uploaded=N`；`admin.exe status` 看对应机器；到 OSS 测试前缀确认对象出现在 `agent_workdir/{员工}/data_collect/` 下。
- 边界：agent 只写不读；去重靠本地 state；源删除后 OSS 副本保留。

- [ ] **Step 4: 更新 `架构说明.md`**（若其中描述了数据流/OSS 对象）

补一句：sync 周期在下发策略/凭据之外，可选地把绑定员工的原始会话增量写入 `data_collect/`（默认关闭，写权限独立）。

- [ ] **Step 5: 确认构建与测试仍绿（文档改动不应影响，但一并跑一遍）**

Run: `cd go && go build ./... && go test ./...`
Expected: 全部 PASS。

- [ ] **Step 6: 提交**

```bash
git add OSS布局.md dist/ram-policy-agent.json dist/部署说明.md 操作手册.md 架构说明.md
git commit -m "docs(collect): document session collection, keep write permission opt-in"
```

---

## Self-Review

**Spec coverage:**
- §1 目标/边界 → Task 2/3（只采绑定员工、默认关、不脱敏）+ Task 5（边界文档）✓
- §2 采什么/写哪 → Task 1（`DataCollectKey`）+ Task 4（两个 glob、保留 `.claude`/`.codex`/`rollout-` 结构）✓
- §3 增量/去抖/state → Task 2（loadState/saveState、去抖、prune、失败重试）✓
- §4 何时/多久 → Task 3（RunOnce 中调用；节奏随 sync 周期）✓
- §5 开关 → Task 1（Policy 字段，默认 false）+ Task 3（enabled 才跑）✓
- §6 权限只写不读 → Task 2（`Putter` 类型级约束）+ Task 5（RAM 文档 opt-in）✓
- §7 原文照传 → Task 2（直接 `io.ReadAll` + `Put`，无脱敏）✓
- §8 改动清单 → Task 1–5 覆盖 ✓
- §10 验收 → 各 Task 的测试步骤 + Task 5 手动验证清单 ✓

**Placeholder scan:** 无 TBD/TODO；所有代码步骤含完整代码。Task 3/5 中"若已有辅助则复用"是对现有代码的适配说明，非占位。

**Type consistency:** `Collector`/`CollectResult`/`CollectRunner`/`FileSource`/`SessionFile`/`Putter` 在 Task 2 定义，Task 3/4 按同名同签名消费；`Policy.CollectEnabled/CollectQuietSeconds/CollectSince`、`Status.CollectEnabled/CollectUploaded` 在 Task 1 定义，Task 3 一致引用；`DataCollectKey(user, rel)` 签名一致。
