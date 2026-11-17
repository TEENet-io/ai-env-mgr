package adminweb

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// providerCatalog answers /model/info with models tagged by LiteLLM
// provider, the way the live gateway does.
func providerCatalog(t *testing.T) *httptest.Server {
	models := [][4]string{{"grok-4.6", "bedrock", "aws-1", "visible"}, {"gemini-3.1-pro", "vertex_ai", "google-1", "visible"}, {"qwen3-coder", "vertex_ai", "google-hidden", "hidden"}, {"gpt-6-astra", "openai", "azure-1", "visible"}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			id := r.URL.Query().Get("model_id")
			if id == "google-1" || id == "google-hidden" {
				_ = json.NewEncoder(w).Encode(map[string]any{"healthy_count": 1, "unhealthy_count": 0, "healthy_endpoints": []map[string]any{{"model_id": id}}})
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"healthy_count": 0, "unhealthy_count": 1, "unhealthy_endpoints": []map[string]any{{"model_id": id}}})
			return
		}
		if r.URL.Path != "/model/info" {
			http.NotFound(w, r)
			return
		}
		var data []map[string]any
		for _, m := range models {
			params := map[string]any{"model": m[1] + "/" + m[0]}
			if m[1] == "openai" {
				params["api_base"] = "https://res.services.ai.azure.com/openai/v1"
			}
			data = append(data, map[string]any{"model_name": m[0], "litellm_params": params,
				"model_info": map[string]any{"id": m[2], "catalog_visible": m[3] == "visible", "display_name": m[0], "litellm_provider": m[1]}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
}

func TestAnAdministratorPausesAndResumesAChannel(t *testing.T) {
	s, _ := newDatabaseServer(t)
	srv := providerCatalog(t)
	defer srv.Close()
	s.opts.GatewayURL, s.opts.GatewayAdminKey = srv.URL, "sk-test"
	h := s.Handler()
	admin := signedIn(t, s)
	ctx := t.Context()
	e, _ := s.dbm.store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "alice"})
	s.dbm.store.Credentials().Store(ctx, repo.NewCredential{EmployeeID: e.ID, Epoch: e.AuthEpoch, Purpose: repo.PurposeCodexGateway, Ciphertext: []byte("x"), KeyVersion: "k1"})

	page := dbGet(t, h, "/channels", admin).Body.String()
	for _, want := range []string{"AWS Bedrock", "Google Vertex", "Azure OpenAI", `action="/channels/pause"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("the channel table lacks %q", want)
		}
	}
	if !strings.Contains(page, "qwen3-coder") || !strings.Contains(page, "google-hidden") {
		t.Fatal("admin channel page should include hidden deployments")
	}
	if strings.Contains(page, `action="/channels/try"`) || strings.Contains(page, `name="prompt"`) {
		t.Fatal("channel page must not offer model trial requests")
	}
	if rec := dbPost(t, h, "/channels/try", url.Values{"channel": {"google"}, "model_id": {"google-1"}, "prompt": {"hi"}}, admin); rec.Code != http.StatusNotFound {
		t.Fatalf("removed trial endpoint status = %d, want 404", rec.Code)
	}
	if modelsPage := dbGet(t, h, "/models", admin).Body.String(); strings.Contains(modelsPage, "qwen3-coder") || strings.Contains(modelsPage, `action="/channels/pause"`) {
		t.Fatal("the employee catalog must not expose hidden deployments or channel controls")
	}
	csrf := csrfFrom(t, s, admin, "/channels")
	// A pause wants the channel typed out.
	rec := dbPost(t, h, "/channels/pause", url.Values{"csrf": {csrf}, "channel": {"google"}, "pause": {"1"}, "version": {"0"}, "confirm": {"yes"}}, admin)
	if !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatal("a pause without the channel typed out was accepted")
	}
	rec = dbPost(t, h, "/channels/pause", url.Values{"csrf": {csrf}, "channel": {"google"}, "pause": {"1"}, "version": {"0"}, "confirm": {"google"}, "reason": {"Vertex 403"}}, admin)
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "err=") {
		t.Fatalf("pause: %s", loc)
	}
	state, version, _ := repo.LoadGatewayChannels(ctx, s.dbm.store.Settings())
	if _, ok := state.Paused["google"]; !ok || version != 1 {
		t.Fatalf("paused = %+v v%d", state, version)
	}
	page = dbGet(t, h, "/channels", admin).Body.String()
	if !strings.Contains(page, "已暫停") || !strings.Contains(page, "Vertex 403") {
		t.Fatal("the channel page should show Google paused and why")
	}
	if modelsPage := dbGet(t, h, "/models", admin).Body.String(); !strings.Contains(modelsPage, "全部下發") {
		t.Fatal("the models page should keep the delivery controls")
	}
	// The raw gateway catalog remains visible on the channel page, but account
	// pickers must only offer models that the current channel policy allows.
	users := dbGet(t, h, "/users", admin).Body.String()
	if strings.Contains(users, "gemini-3.1-pro") {
		t.Fatal("the onboarding picker still offers a model from the paused Google channel")
	}

	// The overview only says so, and points to the page.
	if ov := dbGet(t, h, "/overview", admin).Body.String(); !strings.Contains(ov, "Google Vertex 已暫停") || !strings.Contains(ov, `href="/channels"`) || strings.Contains(ov, `action="/channels/pause"`) {
		t.Error("the overview should summarise the pause and link to channels, without the controls")
	}

	// An operator sees it and cannot touch it.
	_, pw, _ := s.dbm.auth.CreateAccount(ctx, "olga", "", "viewer")
	op := signInAs(t, s, "olga", pw)
	if page := dbGet(t, h, "/channels", op).Body.String(); strings.Contains(page, `action="/channels/pause"`) || strings.Contains(page, `action="/channels/test"`) || strings.Contains(page, "google-hidden") {
		t.Error("an operator should not see admin controls or hidden deployments")
	}
	opCSRF := csrfFrom(t, s, op, "/account")
	if rec := dbPost(t, h, "/channels/pause", url.Values{"csrf": {opCSRF}, "channel": {"google"}, "pause": {"0"}, "version": {"1"}}, op); rec.Code != http.StatusForbidden {
		t.Errorf("operator resume: %d, want 403", rec.Code)
	}

	rec = dbPost(t, h, "/channels/pause", url.Values{"csrf": {csrf}, "channel": {"google"}, "pause": {"0"}, "version": {"1"}}, admin)
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "err=") {
		t.Fatalf("resume: %s", loc)
	}
	if state, _, _ = repo.LoadGatewayChannels(ctx, s.dbm.store.Settings()); len(state.Paused) != 0 {
		t.Fatalf("still paused: %+v", state)
	}
}

func TestChannelHealthCheckTargetsDeploymentAndRequiresAdmin(t *testing.T) {
	s, _ := newDatabaseServer(t)
	srv := providerCatalog(t)
	defer srv.Close()
	s.opts.GatewayURL, s.opts.GatewayAdminKey = srv.URL, "sk-test"
	h := s.Handler()
	admin := signedIn(t, s)
	csrf := csrfFrom(t, s, admin, "/channels")
	rec := dbPost(t, h, "/channels/test", url.Values{"csrf": {csrf}, "channel": {"google"}, "model_id": {"google-1"}}, admin)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "msg=") || strings.Contains(loc, "err=") {
		t.Fatalf("healthy deployment: %s", loc)
	}
	rec = dbPost(t, h, "/channels/test", url.Values{"csrf": {csrf}, "channel": {"aws"}, "model_id": {"google-1"}}, admin)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "err=") {
		t.Fatalf("cross-channel deployment accepted: %s", loc)
	}
	rec = dbPost(t, h, "/channels/test", url.Values{"csrf": {csrf}, "channel": {"aws"}, "model_id": {"aws-1"}}, admin)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "err=") {
		t.Fatalf("unhealthy deployment reported success: %s", loc)
	}
	events, _, err := s.dbm.store.Audit().Search(t.Context(), repo.AuditFilter{TargetType: "channel", Action: "channel_test", Limit: 10})
	if err != nil || len(events) != 2 || events[0].Result != "failed" || events[1].Result != "ok" {
		t.Fatalf("channel test audit = %+v, err = %v", events, err)
	}
	_, pw, _ := s.dbm.auth.CreateAccount(t.Context(), "olga", "", "viewer")
	op := signInAs(t, s, "olga", pw)
	opCSRF := csrfFrom(t, s, op, "/account")
	rec = dbPost(t, h, "/channels/test", url.Values{"csrf": {opCSRF}, "channel": {"google"}, "model_id": {"google-1"}}, op)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator test status = %d, want 403", rec.Code)
	}
}
