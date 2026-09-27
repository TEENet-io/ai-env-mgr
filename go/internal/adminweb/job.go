package adminweb

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// A publish moves hundreds of megabytes twice -- down from GitHub, up to OSS --
// which no browser request should be asked to hold open. Done inline, the
// Codex installer would blow past this server's write timeout and any proxy in
// front of it long before finishing, leaving the operator staring at a dead
// connection with no way to tell whether the fleet had just been given a new
// binary.
//
// So a publish is started, not awaited: the request returns at once and the
// page reports on the job as it runs.

type jobState string

const (
	jobRunning jobState = "running"
	jobDone    jobState = "done"
	jobFailed  jobState = "failed"
)

type job struct {
	ID      string
	Kind    string // "agent" or "codex", for the wording on the page
	Version string
	State   jobState
	Step    string // what it is doing right now
	Err     string
	Started time.Time
	Ended   time.Time

	// Done and Total size the bar on the page. Total is 0 when the current
	// step cannot say how much there is -- a server that sent no
	// Content-Length -- and the page then shows an indeterminate bar rather
	// than a wrong one.
	Done        int64
	Total       int64
	lastPersist time.Time
}

// Percent is how far the current step has got, 0 when it cannot be known.
func (j *job) Percent() int {
	if j.Total <= 0 || j.Done <= 0 {
		return 0
	}
	if j.Done >= j.Total {
		return 100
	}
	return int(j.Done * 100 / j.Total)
}

// Measured reports whether there is a total to measure against, which decides
// whether the page draws a real bar or an indeterminate one.
func (j *job) Measured() bool { return j.Total > 0 }

// Sized renders the transfer for humans: "123 / 700 MB".
func (j *job) Sized() string {
	if j.Total <= 0 {
		if j.Done <= 0 {
			return ""
		}
		return fmt.Sprintf("%d MB", j.Done>>20)
	}
	return fmt.Sprintf("%d / %d MB", j.Done>>20, j.Total>>20)
}

func (j *job) Running() bool { return j.State == jobRunning }

// Elapsed is how long the job has been going, for the page to show.
func (j *job) Elapsed() string {
	end := j.Ended
	if j.State == jobRunning {
		end = time.Now()
	}
	d := end.Sub(j.Started).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	return fmt.Sprintf("%d 分 %d 秒", int(d.Minutes()), int(d.Seconds())%60)
}

// jobRunner holds the one publish that may be in flight. In database mode the
// same state is mirrored to AdminJobs so a page reload or console restart does
// not erase the operator's only progress record.
//
// One at a time on purpose. Two publishes of the same kind would race to write
// the same policy fields, and the loser would silently win: the fleet would
// end up pointed at whichever upload happened to finish second, which is not
// necessarily the one the operator started last.
type jobRunner struct {
	mu         sync.Mutex
	current    *job
	persistent repo.AdminJobs
}

var errJobBusy = fmt.Errorf("上一个发布还在进行中，等它结束再发下一个")

// start runs fn in the background, refusing if something is already running.
func (r *jobRunner) start(kind, version string, fn func(setStep func(string), setProgress func(done, total int64)) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current != nil && r.current.Running() {
		return errJobBusy
	}
	j := &job{Kind: kind, Version: version, State: jobRunning, Step: "准备中", Started: time.Now()}
	if r.persistent != nil {
		id := fmt.Sprintf("admin-%d", j.Started.UnixNano())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := r.persistent.Create(ctx, id, kind, version)
		cancel()
		if err != nil {
			return fmt.Errorf("create admin job: %w", err)
		}
		j.ID = id
	}
	r.current = j

	setStep := func(step string) {
		r.mu.Lock()
		// Each step measures its own transfer, so the bar restarts with it
		// rather than carrying the previous step's numbers.
		j.Step, j.Done, j.Total = step, 0, 0
		r.mu.Unlock()
		r.persist(j, true)
	}
	setProgress := func(done, total int64) {
		r.mu.Lock()
		j.Done, j.Total = done, total
		r.mu.Unlock()
		r.persist(j, false)
	}
	go func() {
		err := fn(setStep, setProgress)
		r.mu.Lock()
		j.Ended = time.Now()
		if err != nil {
			j.State, j.Err = jobFailed, err.Error()
		} else {
			j.State, j.Step = jobDone, "完成"
		}
		copy := *j
		r.mu.Unlock()
		r.persist(&copy, true)
	}()
	return nil
}

// persist mirrors the in-memory snapshot into PostgreSQL. Progress updates
// are throttled so a fast download does not turn every network callback into a
// database round trip; step changes and terminal states are always written.
func (r *jobRunner) persist(j *job, force bool) {
	if r.persistent == nil || j.ID == "" {
		return
	}
	r.mu.Lock()
	if !force && !j.lastPersist.IsZero() && time.Since(j.lastPersist) < time.Second {
		r.mu.Unlock()
		return
	}
	j.lastPersist = time.Now()
	copy := *j
	r.mu.Unlock()
	var ended *time.Time
	if !copy.Ended.IsZero() {
		ended = &copy.Ended
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = r.persistent.Update(ctx, copy.ID, string(copy.State), copy.Step, copy.Done, copy.Total, copy.Err, ended)
	cancel()
}

// wait blocks until nothing is running, or until the deadline passes. It
// reports whether the job finished.
//
// This exists for shutdown. A publish moves ~700 MB and writes the policy only
// at the very end, so a process that exits mid-flight destroys the work and
// leaves nothing behind to say so: no policy change, no audit line, and (in
// legacy mode) a job page that comes back empty because the state lived in
// memory. Database mode records the interrupted job before serving pages, so
// the operator sees why it needs to be retried.
// It has happened -- a deploy restarted the console while a Codex publish was
// uploading.
func (r *jobRunner) wait(deadline time.Duration) bool {
	const poll = 250 * time.Millisecond
	for waited := time.Duration(0); waited < deadline; waited += poll {
		j := r.snapshot()
		if j == nil || !j.Running() {
			return true
		}
		time.Sleep(poll)
	}
	j := r.snapshot()
	return j == nil || !j.Running()
}

// snapshot returns a copy safe to render while the job keeps running.
func (r *jobRunner) snapshot() *job {
	r.mu.Lock()
	if r.current != nil {
		c := *r.current
		r.mu.Unlock()
		return &c
	}
	persistent := r.persistent
	r.mu.Unlock()
	if persistent == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	record, err := persistent.Latest(ctx)
	cancel()
	if err != nil {
		return nil
	}
	j := &job{ID: record.ID, Kind: record.Kind, Version: record.Version, State: jobState(record.State), Step: record.Step, Err: record.LastError, Started: record.StartedAt, Done: record.DoneBytes, Total: record.TotalBytes}
	if record.EndedAt != nil {
		j.Ended = *record.EndedAt
	}
	return j
}
