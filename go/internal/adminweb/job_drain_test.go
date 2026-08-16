package adminweb

import (
	"testing"
	"time"
)

// A publish writes the policy only at the very end, so a process that exits
// mid-upload destroys the work and leaves nothing behind to say so: no policy
// change, no audit line, and a job page that comes back empty because the
// state lived in memory. The operator is left believing a publish landed when
// nothing was published. This happened once, to a deploy.
func TestWaitBlocksUntilAPublishFinishes(t *testing.T) {
	var r jobRunner
	release := make(chan struct{})
	if err := r.start("codex", "26.810.52044-b1", func(setStep func(string), _ func(done, total int64)) error {
		setStep("上传")
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Still running: wait must not return early.
	if r.wait(200 * time.Millisecond) {
		t.Fatal("wait reported a running publish as finished")
	}

	close(release)
	if !r.wait(5 * time.Second) {
		t.Fatal("wait did not notice the publish finishing")
	}
	if j := r.snapshot(); j == nil || j.State != jobDone {
		t.Fatalf("job state = %v", j)
	}
}

// A wedged job must not hold the service down forever; the caller is told so
// it can say the publish did not land.
func TestWaitGivesUpAtTheDeadline(t *testing.T) {
	var r jobRunner
	release := make(chan struct{})
	defer close(release)
	if err := r.start("codex", "1.0.0", func(func(string), func(done, total int64)) error {
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if r.wait(300 * time.Millisecond) {
		t.Fatal("wait claimed a stuck publish had finished")
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("wait returned after %s, before its deadline", elapsed)
	}
}

// Nothing running is the common case and must not delay shutdown at all.
func TestWaitReturnsAtOnceWhenIdle(t *testing.T) {
	var r jobRunner
	start := time.Now()
	if !r.wait(5 * time.Second) {
		t.Fatal("an idle runner reported work in flight")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("an idle runner took %s to answer", elapsed)
	}
}
