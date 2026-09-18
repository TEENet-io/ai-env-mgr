package dbstore

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestPublishedPoliciesAreImmutableAndRollBack(t *testing.T) {
	s, ctx := newTestStore(t)

	// Nothing published yet is an ordinary answer, and the only one the caller
	// may turn into the built-in default. A read that failed must not become
	// "the fleet has no policy".
	if _, err := s.Policies().Current(ctx); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("Current with nothing published: error = %v, want ErrNotFound", err)
	}

	first, err := s.Policies().Publish(ctx,
		[]byte(`{"blockEnabled":true,"syncIntervalMinutes":30}`), "首个策略", "admin")
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if first.Version <= 0 {
		t.Fatalf("published version = %d, want a positive number", first.Version)
	}

	second, err := s.Policies().Publish(ctx,
		[]byte(`{"blockEnabled":true,"syncIntervalMinutes":5,"appLockerAllowPaths":["C:\\Tools\\Codex\\*"]}`),
		"放行 Codex", "admin")
	if err != nil {
		t.Fatalf("publish again: %v", err)
	}
	if second.Version <= first.Version {
		t.Fatalf("second version %d is not after the first %d", second.Version, first.Version)
	}

	current, err := s.Policies().Current(ctx)
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if current.Version != second.Version {
		t.Fatalf("current version = %d, want the newly published %d", current.Version, second.Version)
	}

	// A bad path reaching every machine is exactly when there has to be
	// something to go back to.
	rolled, err := s.Policies().Rollback(ctx, first.Version, "admin")
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rolled.Version != first.Version {
		t.Errorf("rollback returned version %d, want %d", rolled.Version, first.Version)
	}
	current, err = s.Policies().Current(ctx)
	if err != nil {
		t.Fatalf("Current after rollback: %v", err)
	}
	if current.Version != first.Version {
		t.Errorf("current = %d after rollback, want %d", current.Version, first.Version)
	}

	// The old version is still there, byte for byte: a rollback is not a new
	// decision and must not be recorded as one.
	versions, err := s.Policies().List(ctx, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(versions) != 2 {
		t.Errorf("there are %d versions after a rollback, want 2: a rollback is not a new decision", len(versions))
	}
	stored, err := s.Policies().ByVersion(ctx, second.Version)
	if err != nil {
		t.Fatalf("ByVersion: %v", err)
	}
	var content struct {
		SyncIntervalMinutes int      `json:"syncIntervalMinutes"`
		AppLockerAllowPaths []string `json:"appLockerAllowPaths"`
	}
	if err := json.Unmarshal(stored.Content, &content); err != nil {
		t.Fatalf("stored policy is not readable JSON: %v", err)
	}
	if content.SyncIntervalMinutes != 5 || len(content.AppLockerAllowPaths) != 1 ||
		content.AppLockerAllowPaths[0] != `C:\Tools\Codex\*` {
		t.Errorf("stored policy = %+v, want what was published", content)
	}
}

func TestPublishRejectsSomethingThatIsNotAPolicy(t *testing.T) {
	s, ctx := newTestStore(t)
	_, err := s.Policies().Publish(ctx, []byte(`{"blockEnabled": true`), "", "admin")
	if err == nil {
		t.Fatal("truncated JSON was published")
	}
	if !strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("error = %v, want it to name the problem", err)
	}
	if _, err := s.Policies().Rollback(ctx, 999, "admin"); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("rolling back to a version that does not exist: error = %v, want ErrNotFound", err)
	}
}

func TestPolicyListIsNewestFirst(t *testing.T) {
	s, ctx := newTestStore(t)
	for _, note := range []string{"one", "two", "three"} {
		if _, err := s.Policies().Publish(ctx, []byte(`{"blockEnabled":true}`), note, "admin"); err != nil {
			t.Fatalf("publish %s: %v", note, err)
		}
	}
	list, err := s.Policies().List(ctx, 2)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list returned %d versions, want the 2 asked for", len(list))
	}
	if list[0].Note != "three" || list[1].Note != "two" {
		t.Errorf("list = %q, %q; want newest first", list[0].Note, list[1].Note)
	}
	all, err := s.Policies().List(ctx, 0)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("unlimited list returned %d, want 3", len(all))
	}
}
