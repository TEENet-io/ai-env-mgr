package agentcore

import (
	"context"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/companyskills"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

type skillRecorder struct{ plans []model.ManagedSkills }

func (f *skillRecorder) Sync(plan model.ManagedSkills) (model.ManagedSkillsStatus, error) {
	f.plans = append(f.plans, plan)
	return model.ManagedSkillsStatus{State: "applied", User: plan.User}, nil
}

func TestAPISkillsFollowBindingAndRemainOptionalForOlderServers(t *testing.T) {
	console := newFakeConsole(t)
	s, src := apiSyncer(t, console, &fakeApplier{})
	recorder := &skillRecorder{}
	s.Skills = recorder
	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if len(recorder.plans) != 0 {
		t.Fatal("older server interpreted as revoke")
	}
	bundle, err := companyskills.FromForm("bippo-demo", "Demo", "test", "follow steps", nil)
	if err != nil {
		t.Fatal(err)
	}
	console.mu.Lock()
	console.cfg.ETag = "skills-1"
	console.cfg.HasBinding = true
	console.cfg.Binding.User = "work1"
	console.cfg.Skills = &model.ManagedSkills{User: "work1", Skills: []model.ManagedSkill{bundle}}
	console.mu.Unlock()
	st, err := s.RunOnce()
	if err != nil || len(recorder.plans) != 1 || len(recorder.plans[0].Skills) != 1 || st.ManagedSkills == nil {
		t.Fatalf("delivery: %+v %v", st, err)
	}
	console.mu.Lock()
	console.cfg.ETag = "skills-2"
	console.cfg.HasBinding = false
	console.cfg.Binding.User = ""
	console.cfg.Skills = &model.ManagedSkills{Skills: []model.ManagedSkill{}}
	console.mu.Unlock()
	if changed, err := src.Wait(context.Background()); err != nil || !changed {
		t.Fatalf("wait: %v %v", changed, err)
	}
	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if len(recorder.plans) != 2 || len(recorder.plans[1].Skills) != 0 {
		t.Fatal("unbind did not revoke")
	}
}

func TestAPISkillsDoNotReplayStaleSnapshotAfterNetworkFailure(t *testing.T) {
	console := newFakeConsole(t)
	_, src := apiSyncer(t, console, &fakeApplier{})
	if _, _, err := src.Policy(); err != nil {
		t.Fatal(err)
	}
	console.srv.Close()
	if _, _, err := src.Policy(); err == nil {
		t.Fatal("expected connection failure")
	}
	if _, err := src.ManagedSkills(); err == nil {
		t.Fatal("stale config was accepted for skill mutation")
	}
}

type pendingSkills struct {
	generation string
	confirmed  bool
}

func (f *pendingSkills) Sync(plan model.ManagedSkills) (model.ManagedSkillsStatus, error) {
	return model.ManagedSkillsStatus{State: "refresh_pending", User: plan.User, Generation: f.generation, RefreshPending: []string{plan.User}}, nil
}

func (f *pendingSkills) RecheckPending() (model.ManagedSkillsStatus, error) {
	if f.confirmed {
		return model.ManagedSkillsStatus{State: "applied", User: "work1", Generation: f.generation}, nil
	}
	return model.ManagedSkillsStatus{State: "refresh_pending", User: "work1", Generation: f.generation, RefreshPending: []string{"work1"}}, nil
}

func TestLateDesktopAckReachesTheNextHeartbeat(t *testing.T) {
	console := newFakeConsole(t)
	s, _ := apiSyncer(t, console, &fakeApplier{})
	skills := &pendingSkills{generation: "g1"}
	s.Skills = skills
	bundle, err := companyskills.FromForm("bippo-demo", "Demo", "test", "follow steps", nil)
	if err != nil {
		t.Fatal(err)
	}
	console.mu.Lock()
	console.cfg.ETag = "skills-1"
	console.cfg.HasBinding = true
	console.cfg.Binding.User = "work1"
	console.cfg.Skills = &model.ManagedSkills{User: "work1", Skills: []model.ManagedSkill{bundle}}
	console.mu.Unlock()
	if st, err := s.RunOnce(); err != nil || st.ManagedSkills == nil || st.ManagedSkills.State != "refresh_pending" {
		t.Fatalf("first sync: %+v %v", st.ManagedSkills, err)
	}
	if !s.SkillRefreshPending() {
		t.Fatal("pending refresh not visible")
	}
	if changed, err := s.RecheckSkillRefresh(); err != nil || changed {
		t.Fatalf("recheck without ack: %v %v", changed, err)
	}
	skills.confirmed = true
	if changed, err := s.RecheckSkillRefresh(); err != nil || !changed {
		t.Fatalf("recheck with ack: %v %v", changed, err)
	}
	if s.SkillRefreshPending() {
		t.Fatal("confirmed refresh still pending")
	}
	s.mu.Lock()
	ms := s.lastStatus.ManagedSkills
	s.mu.Unlock()
	if ms == nil || ms.State != "applied" || len(ms.RefreshPending) != 0 || ms.Generation != "g1" {
		t.Fatalf("heartbeat status: %+v", ms)
	}
	// An answer about an older generation never overrides a newer full sync.
	skills.generation, skills.confirmed = "g2", false
	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	skills.generation, skills.confirmed = "g1", true
	if changed, err := s.RecheckSkillRefresh(); err != nil || changed || !s.SkillRefreshPending() {
		t.Fatalf("stale recheck applied: %v %v", changed, err)
	}
}
