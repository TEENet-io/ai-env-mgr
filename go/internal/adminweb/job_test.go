package adminweb

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The point of the change: the request must come back at once, even though the
// work behind it moves hundreds of megabytes.
func TestPublishReturnsWithoutWaiting(t *testing.T) {
	fs := newFakeStore()
	s := newTestServer(t, fs)
	cookie := signIn(t, s)

	release := make(chan struct{})
	started := make(chan struct{})
	var r jobRunner
	go func() {
		_ = r.start("test", "1.0.0", func(func(string)) error {
			close(started)
			<-release // hold the job open
			return nil
		})
	}()
	<-started

	// Meanwhile the console still answers.
	done := make(chan int, 1)
	go func() {
		rec := post(t, s, "/settings/interval", cookie,
			url.Values{"csrf": {csrfOf(t, s, cookie)}, "minutes": {"20"}})
		done <- rec.Code
	}()
	select {
	case code := <-done:
		if code != http.StatusSeeOther {
			t.Fatalf("request returned %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the console blocked while a job was running")
	}
	close(release)
}

// Two publishes racing would both write the same policy fields, and whichever
// upload finished second would silently win.
func TestOnlyOnePublishAtATime(t *testing.T) {
	var r jobRunner
	release := make(chan struct{})
	started := make(chan struct{})
	if err := r.start("codex", "1.0.0", func(func(string)) error {
		close(started)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started

	if err := r.start("codex", "2.0.0", func(func(string)) error { return nil }); err == nil {
		t.Fatal("a second publish started while one was running")
	}
	close(release)

	// Once it finishes, the next one is allowed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snap := r.snapshot(); snap != nil && !snap.Running() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := r.start("codex", "2.0.0", func(func(string)) error { return nil }); err != nil {
		t.Fatalf("a publish was refused after the previous one finished: %v", err)
	}
}

// A failure has to survive to the page: the operator is no longer watching the
// request that started it.
func TestFailureIsKeptForThePage(t *testing.T) {
	var r jobRunner
	done := make(chan struct{})
	if err := r.start("agent", "1.0.0", func(func(string)) error {
		defer close(done)
		return errors.New("the download returned HTTP 404")
	}); err != nil {
		t.Fatal(err)
	}
	<-done

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snap := r.snapshot(); snap != nil && !snap.Running() {
			if snap.State != jobFailed {
				t.Fatalf("state = %q, want failed", snap.State)
			}
			if !strings.Contains(snap.Err, "404") {
				t.Fatalf("the reason was lost: %q", snap.Err)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the job never finished")
}

// The step text is written by the job and read by the page from another
// goroutine.
func TestJobProgressIsRaceFree(t *testing.T) {
	var r jobRunner
	release := make(chan struct{})
	started := make(chan struct{})
	_ = r.start("codex", "1.0.0", func(setStep func(string)) error {
		close(started)
		for i := 0; i < 200; i++ {
			setStep("下载中")
		}
		<-release
		return nil
	})
	<-started
	for i := 0; i < 200; i++ {
		_ = r.snapshot()
	}
	close(release)
}
