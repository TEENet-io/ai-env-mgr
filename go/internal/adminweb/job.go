package adminweb

import (
	"fmt"
	"sync"
	"time"
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
	Kind    string // "agent" or "codex", for the wording on the page
	Version string
	State   jobState
	Step    string // what it is doing right now
	Err     string
	Started time.Time
	Ended   time.Time
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

// jobRunner holds the one publish that may be in flight.
//
// One at a time on purpose. Two publishes of the same kind would race to write
// the same policy fields, and the loser would silently win: the fleet would
// end up pointed at whichever upload happened to finish second, which is not
// necessarily the one the operator started last.
type jobRunner struct {
	mu      sync.Mutex
	current *job
}

var errJobBusy = fmt.Errorf("上一个发布还在进行中，等它结束再发下一个")

// start runs fn in the background, refusing if something is already running.
func (r *jobRunner) start(kind, version string, fn func(setStep func(string)) error) error {
	r.mu.Lock()
	if r.current != nil && r.current.Running() {
		r.mu.Unlock()
		return errJobBusy
	}
	j := &job{Kind: kind, Version: version, State: jobRunning, Step: "准备中", Started: time.Now()}
	r.current = j
	r.mu.Unlock()

	setStep := func(step string) {
		r.mu.Lock()
		j.Step = step
		r.mu.Unlock()
	}
	go func() {
		err := fn(setStep)
		r.mu.Lock()
		defer r.mu.Unlock()
		j.Ended = time.Now()
		if err != nil {
			j.State, j.Err = jobFailed, err.Error()
			return
		}
		j.State, j.Step = jobDone, "完成"
	}()
	return nil
}

// wait blocks until nothing is running, or until the deadline passes. It
// reports whether the job finished.
//
// This exists for shutdown. A publish moves ~700 MB and writes the policy only
// at the very end, so a process that exits mid-flight destroys the work and
// leaves nothing behind to say so: no policy change, no audit line, and a job
// page that comes back empty because the state lived in memory. An operator
// then sees a fleet that was never given the new version and no reason why.
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
	defer r.mu.Unlock()
	if r.current == nil {
		return nil
	}
	c := *r.current
	return &c
}
