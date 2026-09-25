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
	models := [][2]string{{"grok-4.6", "bedrock"}, {"gemini-3.1-pro", "vertex_ai"}, {"gpt-6-astra", "openai"}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data []map[string]any
		for _, m := range models {
			params := map[string]any{"model": m[1] + "/" + m[0]}
			if m[1] == "openai" {
				params["api_base"] = "https://res.services.ai.azure.com/openai/v1"
			}
			data = append(data, map[string]any{"model_name": m[0], "litellm_params": params,
				"model_info": map[string]any{"catalog_visible": true, "display_name": m[0], "litellm_provider": m[1]}})
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

	page := dbGet(t, h, "/models", admin).Body.String()
	for _, want := range []string{"AWS Bedrock", "Google Vertex", "Azure OpenAI", `action="/channels/pause"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("the channel table lacks %q", want)
		}
	}
	csrf := csrfFrom(t, s, admin, "/models")
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
	page = dbGet(t, h, "/models", admin).Body.String()
	if !strings.Contains(page, "已暂停") || !strings.Contains(page, "Vertex 403") || !strings.Contains(page, "全部下发") {
		t.Fatal("the page should show Google paused, why, and the delivery it made")
	}
	// The raw gateway catalog remains visible on the channel page, but account
	// pickers must only offer models that the current channel policy allows.
	users := dbGet(t, h, "/users", admin).Body.String()
	if strings.Contains(users, "gemini-3.1-pro") {
		t.Fatal("the onboarding picker still offers a model from the paused Google channel")
	}

	// The overview only says so, and points to the page.
	if ov := dbGet(t, h, "/overview", admin).Body.String(); !strings.Contains(ov, "Google Vertex 已暂停") || !strings.Contains(ov, `href="/models"`) || strings.Contains(ov, `action="/channels/pause"`) {
		t.Error("the overview should summarise the pause and link to 模型与渠道, without the controls")
	}

	// An operator sees it and cannot touch it.
	_, pw, _ := s.dbm.auth.CreateAccount(ctx, "olga", "", "operator")
	op := signInAs(t, s, "olga", pw)
	if page := dbGet(t, h, "/models", op).Body.String(); !strings.Contains(page, "需要管理员") {
		t.Error("an operator should see the channels without the buttons")
	}
	opCSRF := csrfFrom(t, s, op, "/models")
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
