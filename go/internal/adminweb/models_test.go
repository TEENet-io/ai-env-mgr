package adminweb

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// fakeCatalog is a gateway that answers /model/info with a list the test
// can change.
type fakeCatalog struct {
	mu     sync.Mutex
	models []string
}

func (f *fakeCatalog) set(models ...string) { f.mu.Lock(); f.models = models; f.mu.Unlock() }

func (f *fakeCatalog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var data []map[string]any
	for _, m := range f.models {
		data = append(data, map[string]any{"model_name": m, "model_info": map[string]any{"catalog_visible": true, "display_name": m}})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func TestTheOverviewDeliversTheModelConfiguration(t *testing.T) {
	s, _ := newDatabaseServer(t)
	gw := &fakeCatalog{}
	gw.set("gpt-5", "claude-5")
	srv := httptest.NewServer(gw)
	defer srv.Close()
	s.opts.GatewayURL, s.opts.GatewayAdminKey = srv.URL, "sk-test"
	h := s.Handler()
	cookie := signedIn(t, s)
	ctx := t.Context()
	store := s.dbm.store
	e, _ := store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "alice"})
	store.Credentials().Store(ctx, repo.NewCredential{EmployeeID: e.ID, Epoch: e.AuthEpoch, Purpose: repo.PurposeCodexGateway, Ciphertext: []byte("x"), KeyVersion: "k1"})
	pc, _ := store.Devices().EnsureByHostname(ctx, "PC-A")
	store.Bindings().Bind(ctx, pc.ID, e.ID, "", "admin")

	page := dbGet(t, h, "/overview", cookie).Body.String()
	if !strings.Contains(page, "还没有从这里下发过模型配置") || !strings.Contains(page, `action="/models/deliver"`) {
		t.Fatal("the overview should offer the first delivery")
	}
	csrf := csrfFrom(t, s, cookie, "/overview")
	rec := dbPost(t, h, "/models/deliver", url.Values{"csrf": {csrf}, "scope": {"all"}, "version": {"0"}}, cookie)
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "err=") {
		t.Fatalf("deliver: %s", loc)
	}
	page = dbGet(t, h, "/overview", cookie).Body.String()
	// No Worker in the test: the export is still queued.
	if !strings.Contains(page, "已是最新") || !strings.Contains(page, "全部下发") || !strings.Contains(page, "生成中") {
		t.Fatal("after delivering, the block should say it is current and follow the delivery")
	}

	// The export finishes and the machine syncs after it: 已收到.
	if _, err := s.dbm.db.Pool().Exec(ctx, `update tasks set status = 'succeeded', finished_at = now() - interval '1 minute'
		where idempotency_key like 'oss_export:employee:%:catalog:%'`); err != nil {
		t.Fatal(err)
	}
	synced := time.Now()
	if _, err := store.Reports().Import(ctx, repo.DeviceReport{DeviceID: pc.ID, ImportedAt: synced, LastSyncAt: &synced,
		Report: []byte(`{"errors":[]}`), SourceETag: "s1"}); err != nil {
		t.Fatal(err)
	}
	if page = dbGet(t, h, "/overview", cookie).Body.String(); !strings.Contains(page, "已收到") || !strings.Contains(page, "PC-A") {
		t.Fatal("a machine that synced after the export has the new configuration")
	}

	// The gateway changes: the block says what.
	gw.set("gpt-5", "gemini-3")
	page = dbGet(t, h, "/overview", cookie).Body.String()
	if !strings.Contains(page, "有变化") || !strings.Contains(page, `新增 <span class="mono">gemini-3</span>`) || !strings.Contains(page, `下架 <span class="mono">claude-5</span>`) {
		t.Fatal("a changed gateway should be shown as added and removed models")
	}
}
