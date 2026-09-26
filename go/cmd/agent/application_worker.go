package main

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/agentapi"
	"github.com/TEENet-io/ai-env-mgr/internal/agentcore"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// The loop never stores task state locally. A process crash leaves a lease
// that Admin expires and safely reassigns; cancellation is an Admin state.
func applicationTaskLoop(s *agentcore.Syncer, stop <-chan struct{}) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		default:
		}
		if api := s.DeviceAPI(); api != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			task, err := api.Client.ClaimApplicationTask(ctx)
			cancel()
			if err != nil && !errors.Is(err, agentapi.ErrUnauthorized) {
				log.Printf("application task claim failed: %v", err)
			} else if task != nil {
				runApplicationTask(s, api.Client, *task, stop)
			}
		}
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}

func runApplicationTask(s *agentcore.Syncer, client *agentapi.Client, task model.ApplicationTask, stop <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	progress := "queued"
	finished := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				cancel()
				return
			case <-finished:
				return
			case <-ticker.C:
				mu.Lock()
				current := progress
				mu.Unlock()
				rctx, done := context.WithTimeout(context.Background(), 10*time.Second)
				err := client.RenewApplicationTask(rctx, task, current)
				done()
				if err != nil {
					// On uncertainty stop work rather than keep installing after
					// Admin has cancelled or given this lease to another agent.
					log.Printf("application task %s lease lost: %v", task.ID, err)
					cancel()
					return
				}
			}
		}
	}()
	log.Printf("application task %s claimed: %s@%s attempt=%d", task.ID, task.AppID, task.Version, task.Attempts)
	st := s.ExecuteApplication(ctx, model.DesiredApplication{AppID: task.AppID, Version: task.Version, Desired: "installed", TaskID: task.ID, LeaseToken: task.LeaseToken, AllowDowngrade: task.AllowDowngrade}, func(st model.ApplicationStatus) {
		mu.Lock()
		progress = st.State
		mu.Unlock()
		log.Printf("application %s@%s task=%s state=%s", st.AppID, st.DesiredVersion, st.TaskID, st.State)
	})
	close(finished)
	state := st.State
	if ctx.Err() != nil {
		state = agentcore.AppCancelled
	}
	if state != agentcore.AppSucceeded && state != agentcore.AppCancelled {
		state = agentcore.AppFailed
	}
	fctx, done := context.WithTimeout(context.Background(), 15*time.Second)
	err := client.FinishApplicationTask(fctx, task, state, st.LastError)
	done()
	if err != nil && !errors.Is(err, agentapi.ErrTaskLost) {
		log.Printf("application task %s result upload failed: %v", task.ID, err)
	}
	log.Printf("application task %s state=%s error=%s", task.ID, state, st.LastError)
}
