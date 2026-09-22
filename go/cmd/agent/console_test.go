package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/agentapi"
)

type fakeWaiter struct {
	mu      sync.Mutex
	answers []func() (bool, error)
	need    bool
	resets  []string
	calls   int
}

func (f *fakeWaiter) Wait(ctx context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.answers) == 0 {
		return false, nil
	}
	a := f.answers[0]
	f.answers = f.answers[1:]
	return a()
}
func (f *fakeWaiter) NeedsEnrol() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.need }
func (f *fakeWaiter) Reset(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resets = append(f.resets, token)
	f.need = false
}

func fastOptions(slept *[]time.Duration) waiterOptions {
	var mu sync.Mutex
	return waiterOptions{minBackoff: 5 * time.Second, maxBackoff: 5 * time.Minute, enrolRetry: time.Hour,
		sleep: func(d time.Duration, stop <-chan struct{}) bool {
			mu.Lock()
			*slept = append(*slept, d)
			mu.Unlock()
			select {
			case <-stop:
				return false
			default:
				return true
			}
		}}
}

func TestConsoleWaiterNudgesOnChangeAndBacksOffOnErrors(t *testing.T) {
	w := &fakeWaiter{}
	w.answers = []func() (bool, error){
		func() (bool, error) { return true, nil },                                      // change → nudge
		func() (bool, error) { return false, nil },                                     // quiet
		func() (bool, error) { return false, errors.New("timeout") },                   // error → 5s
		func() (bool, error) { return false, errors.New("timeout") },                   // error → 10s
		func() (bool, error) { return true, nil },                                      // recovered → nudge, backoff reset
		func() (bool, error) { w.need = true; return false, agentapi.ErrUnauthorized }, // refused → re-enrol (Wait holds the lock)
	}
	stop := make(chan struct{})
	notify := make(chan struct{}, 8)
	var slept []time.Duration
	enrols := 0
	reenrol := func() (string, error) {
		enrols++
		if enrols == 1 {
			return "", agentapi.ErrAlreadyEnrolled
		}
		return "tok-new", nil
	}
	opts := fastOptions(&slept)
	done := make(chan struct{})
	go func() { consoleWaiter(w, stop, notify, reenrol, opts); close(done) }()
	deadline := time.After(3 * time.Second)
	for {
		w.mu.Lock()
		resets := len(w.resets)
		w.mu.Unlock()
		if resets == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no re-enrolment: calls=%d slept=%v enrols=%d", w.calls, slept, enrols)
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(stop)
	<-done
	if len(notify) != 2 {
		t.Fatalf("nudges = %d, want 2", len(notify))
	}
	if len(slept) < 3 || slept[0] != 5*time.Second || slept[1] != 10*time.Second || slept[2] != time.Hour {
		t.Fatalf("slept = %v (5s, 10s for the errors, then an hour for the refused enrolment)", slept)
	}
	if enrols != 2 || w.resets[0] != "tok-new" {
		t.Fatalf("enrols=%d resets=%v", enrols, w.resets)
	}
}
