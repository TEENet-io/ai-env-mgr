package agentcore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

func bindingBytes(t *testing.T, b model.Binding) []byte {
	t.Helper()
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

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
	// The format agents before 1.2.16 wrote still counts for a fleet target,
	// and never for a per-machine generation.
	if !markerMatches("0.42.0", model.ReleaseTarget{Version: "0.42.0"}) {
		t.Fatal("an old-format marker must match the fleet target")
	}
	if markerMatches("0.42.0", model.ReleaseTarget{Version: "0.42.0", Generation: 1}) {
		t.Fatal("an old-format marker must not match a per-machine generation")
	}
}

func TestAnUnboundMachineStillReadsItsTargets(t *testing.T) {
	store := newFakeStore()
	store.set(ossclient.BindingKey("DESKTOP-A"), []byte(`{"user":"","agentTarget":{"version":"1.2.16","sha256":"x","generation":1}}`), "e1")
	s := newSyncer(t, store, &fakeApplier{})
	binding, bound := s.loadBinding("DESKTOP-A")
	if bound {
		t.Fatal("no user means not bound")
	}
	if binding.AgentTarget == nil || binding.AgentTarget.Version != "1.2.16" {
		t.Fatalf("targets must survive an empty user: %+v", binding)
	}
}

func TestCodexReportsWhyItDeferred(t *testing.T) {
	store := newFakeStore()
	codex := &fakeCodex{installed: "0.41.0", running: true, free: 100 << 30}
	s := newCodexSyncer(t, store, codex)
	pol := codexPolicy(store, "0.42.0", []byte("installer"))
	store.set(ossclient.PolicyKey(), policyBytes(t, pol), "p1")

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
	store.set(ossclient.PolicyKey(), policyBytes(t, pol), "p1")
	sum := sha256.Sum256([]byte("installer"))
	target := &model.ReleaseTarget{Version: "0.42.0", SHA256: hex.EncodeToString(sum[:]), Key: pol.CodexKey, Generation: 1}
	store.set(ossclient.BindingKey(s.Machine.Name()), bindingBytes(t, model.Binding{User: "work1", CodexTarget: target}), "b1")

	s.RunOnce()
	st, _ := s.RunOnce()
	if codex.installs != 1 || st.CodexState != CodexFailed || st.CodexTargetGeneration != 1 {
		t.Fatalf("generation 1: installs = %d, state = %q, gen = %d", codex.installs, st.CodexState, st.CodexTargetGeneration)
	}

	target.Generation = 2
	store.set(ossclient.BindingKey(s.Machine.Name()), bindingBytes(t, model.Binding{User: "work1", CodexTarget: target}), "b2")
	st, _ = s.RunOnce()
	if codex.installs != 2 || st.CodexTargetGeneration != 2 {
		t.Fatalf("generation 2 must try again: installs = %d, gen = %d", codex.installs, st.CodexTargetGeneration)
	}
}

func TestTheAgentReportsAFailedUpdateAfterwards(t *testing.T) {
	bin := []byte("new agent")
	sum := sha256.Sum256(bin)
	s, store, up := setupUpdate(t, "1.2.17", hex.EncodeToString(sum[:]), bin)
	// A binding target for a versioned key wins over the fleet key.
	store.set(ossclient.AgentVersionKey("1.2.17"), bin, "v")
	store.set(ossclient.BindingKey(s.Machine.Name()), bindingBytes(t, model.Binding{User: "work1",
		AgentTarget: &model.ReleaseTarget{Version: "1.2.17", SHA256: hex.EncodeToString(sum[:]),
			Key: ossclient.AgentVersionKey("1.2.17"), Generation: 4}}), "b")

	st, _ := s.RunOnce()
	if st.AgentUpdateTarget != "1.2.17" || st.AgentUpdateGeneration != 4 || st.AgentUpdateState != AgentUpdatePending {
		t.Fatalf("first cycle: %q gen %d state %q", st.AgentUpdateTarget, st.AgentUpdateGeneration, st.AgentUpdateState)
	}
	if store.downloadCount(ossclient.AgentVersionKey("1.2.17")) != 1 || len(up.applied) != 1 {
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

func TestTheHeartbeatNoticesWhatTheConsoleChanged(t *testing.T) {
	store := newFakeStore()
	store.set(ossclient.PolicyKey(), policyBytes(t, model.DefaultPolicy()), "p1")
	s := newSyncer(t, store, &fakeApplier{})
	if _, err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	// Nothing bound, nothing changed: the heartbeat has nothing to say.
	if changed, what := s.ChangedSinceLastSync(); changed {
		t.Fatalf("nothing changed, yet %q", what)
	}
	// A binding appears (a target, a "sync now", an assignment).
	store.set(ossclient.BindingKey("DESKTOP-A"), bindingBytes(t, model.Binding{SyncRequested: "n1"}), "b1")
	if changed, what := s.ChangedSinceLastSync(); !changed || what != "binding" {
		t.Fatalf("a new binding object must be noticed: %v %q", changed, what)
	}
	s.RunOnce()
	if changed, _ := s.ChangedSinceLastSync(); changed {
		t.Fatal("after a sync the object has been seen")
	}
	// The same object rewritten with the same bytes has the same ETag: the
	// console changes the nonce so that it does not.
	store.set(ossclient.BindingKey("DESKTOP-A"), bindingBytes(t, model.Binding{SyncRequested: "n2"}), "b2")
	if changed, _ := s.ChangedSinceLastSync(); !changed {
		t.Fatal("a rewritten binding must be noticed")
	}
	s.RunOnce()
	// The fleet policy moves.
	store.set(ossclient.PolicyKey(), policyBytes(t, model.DefaultPolicy()), "p2")
	if changed, what := s.ChangedSinceLastSync(); !changed || what != "policy" {
		t.Fatalf("a new policy must be noticed: %v %q", changed, what)
	}
	s.RunOnce()
	// The binding goes away (unbound, nothing aimed): noticed too.
	delete(store.objects, ossclient.BindingKey("DESKTOP-A"))
	if changed, _ := s.ChangedSinceLastSync(); !changed {
		t.Fatal("a removed binding must be noticed")
	}
	s.RunOnce()
	if changed, _ := s.ChangedSinceLastSync(); changed {
		t.Fatal("absence, once seen, is not a change")
	}
	// A store that cannot be reached is not a change.
	store.failKey(ossclient.BindingKey("DESKTOP-A"), errors.New("network down"))
	if changed, _ := s.ChangedSinceLastSync(); changed {
		t.Fatal("an unreachable store must not trigger a sync")
	}
}
