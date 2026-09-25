package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/agentapi"
	"github.com/TEENet-io/ai-env-mgr/internal/agentcore"
)

// connectConsole builds the console-backed Source when this build knows a
// console and the machine holds, or can get, a device token. Nil means the
// agent is waiting for console enrolment; the service loop keeps trying in
// that case (see enrolUntilDone).
func connectConsole(consoleURL, hostname, stateDir string) *agentcore.APISource {
	if consoleURL == "" {
		return nil
	}
	token, err := agentapi.LoadToken(stateDir)
	if errors.Is(err, agentapi.ErrNoToken) {
		token, err = enrolConsole(consoleURL, hostname, stateDir)
	}
	if err != nil {
		log.Printf("console: %v; waiting for console enrolment", err)
		return nil
	}
	return newAPISource(consoleURL, token)
}

func newAPISource(consoleURL, token string) *agentcore.APISource {
	return agentcore.NewAPISource(&agentapi.Client{BaseURL: consoleURL, Token: token, Version: version,
		HTTP: &http.Client{Timeout: 30 * time.Second}})
}

// enrolUntilDone keeps asking the console for a token until it gets one
// or the service stops, then hands the source over. A console that refuses
// (it knows this name and nobody opened the door) is asked hourly; any
// other failure backs off from five seconds to five minutes.
func enrolUntilDone(consoleURL, hostname, stateDir string, stop <-chan struct{}, adopted chan<- *agentcore.APISource, opts waiterOptions) {
	backoff := opts.minBackoff
	for {
		select {
		case <-stop:
			return
		default:
		}
		token, err := enrolConsole(consoleURL, hostname, stateDir)
		if err == nil {
			adopted <- newAPISource(consoleURL, token)
			return
		}
		wait := backoff
		if errors.Is(err, agentapi.ErrAlreadyEnrolled) {
			wait = opts.enrolRetry
			log.Printf("console: enrolment refused, the console knows this machine and nobody has allowed it to enrol; trying again in %s", wait)
		} else {
			log.Printf("console: enrolment failed: %v; trying again in %s", err, wait)
			backoff = min(backoff*2, opts.maxBackoff)
		}
		if !opts.sleep(wait, stop) {
			return
		}
	}
}

// enrolConsole asks the console for a token and keeps it.
func enrolConsole(consoleURL, hostname, stateDir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, token, err := agentapi.Enrol(ctx, consoleURL, hostname, version, &http.Client{Timeout: 20 * time.Second})
	if err != nil {
		return "", err
	}
	if err := agentapi.SaveToken(stateDir, token); err != nil {
		return "", err
	}
	log.Printf("console: enrolled as %s", hostname)
	return token, nil
}

// consoleWaiter is what the service loop runs beside the ticker in the
// console mode: one long poll after another, a nudge on the notify channel
// whenever the console says the configuration changed. Errors back off from
// five seconds to five minutes; a refused token triggers re-enrolment, and
// a refused enrolment (the console holds a token for this name) waits an
// hour before asking again.
type waiter interface {
	Wait(ctx context.Context) (bool, error)
	NeedsEnrol() bool
	Reset(token string)
}

type waiterOptions struct {
	minBackoff, maxBackoff, enrolRetry time.Duration
	sleep                              func(time.Duration, <-chan struct{}) bool
}

func defaultWaiterOptions() waiterOptions {
	return waiterOptions{minBackoff: 5 * time.Second, maxBackoff: 5 * time.Minute, enrolRetry: time.Hour, sleep: sleepUnlessStopped}
}

func sleepUnlessStopped(d time.Duration, stop <-chan struct{}) bool {
	select {
	case <-time.After(d):
		return true
	case <-stop:
		return false
	}
}

func consoleWaiter(w waiter, stop <-chan struct{}, notify chan<- struct{}, reenrol func() (string, error), opts waiterOptions) {
	backoff := opts.minBackoff
	for {
		select {
		case <-stop:
			return
		default:
		}
		if w.NeedsEnrol() {
			token, err := reenrol()
			switch {
			case err == nil:
				w.Reset(token)
				backoff = opts.minBackoff
			case errors.Is(err, agentapi.ErrAlreadyEnrolled):
				log.Printf("console: enrolment refused, the console still holds a token for this machine; an administrator can allow re-enrolment. Trying again in %s", opts.enrolRetry)
				if !opts.sleep(opts.enrolRetry, stop) {
					return
				}
				continue
			default:
				log.Printf("console: enrolment failed: %v; retrying in %s", err, backoff)
				if !opts.sleep(backoff, stop) {
					return
				}
				backoff = min(backoff*2, opts.maxBackoff)
				continue
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			select {
			case <-stop:
				cancel()
			case <-done:
			}
		}()
		changed, err := w.Wait(ctx)
		close(done)
		cancel()
		if err != nil {
			select {
			case <-stop:
				return
			default:
			}
			if !w.NeedsEnrol() {
				log.Printf("console: wait failed: %v; retrying in %s", err, backoff)
				if !opts.sleep(backoff, stop) {
					return
				}
				backoff = min(backoff*2, opts.maxBackoff)
			}
			continue
		}
		backoff = opts.minBackoff
		if changed {
			select {
			case notify <- struct{}{}:
			default: // a sync is already pending
			}
		}
	}
}
