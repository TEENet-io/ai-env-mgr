package adminweb

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestChannelTrialOnlyDisablesGemini25FlashThinking(t *testing.T) {
	cases := []struct {
		channel, model string
		want           bool
	}{
		{"vertex_ai", "vertex_ai/gemini-2.5-flash", true},
		{"vertex_ai", "vertex_ai/gemini-2.5-flash-lite", true},
		{"vertex_ai", "vertex_ai/gemini-2.5-pro", false},
		{"vertex_ai", "vertex_ai/gemini-3-flash", false},
		{"bedrock", "bedrock/gemini-2.5-flash", false},
	}
	for _, tc := range cases {
		m := litellm.Model{Info: litellm.ModelInfo{LitellmProvider: tc.channel}, Params: litellm.ModelParams{Model: tc.model}}
		if got := channelTrialDisablesThinking(m); got != tc.want {
			t.Errorf("%s / %s: disable thinking = %v, want %v", tc.channel, tc.model, got, tc.want)
		}
	}
}

// providerCatalog answers /model/info with models tagged by LiteLLM
// provider, the way the live gateway does.
func providerCatalog(t *testing.T) *httptest.Server {
	models := [][4]string{{"grok-4.6", "bedrock", "aws-1", "visible"}, {"gemini-3.1-pro", "vertex_ai", "google-1", "visible"}, {"qwen3-coder", "vertex_ai", "google-hidden", "hidden"}, {"gpt-6-astra", "openai", "azure-1", "visible"}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/completions" {
			var body struct {
				Model    string `json:"model"`
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
				MaxTokens int `json:"max_tokens"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Messages) != 1 || body.MaxTokens != 256 {
				http.Error(w, "bad trial request", http.StatusBadRequest)
				return
			}
			w.Header().Set("x-litellm-model-id", body.Model)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{"message": map[string]string{"content": "回复：" + body.Messages[0].Content}, "finish_reason": "stop"}},
				"usage":   map[string]int{"prompt_tokens": 12, "completion_tokens": 5},
			})
			return
		}
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

func TestChannelTrialAcceptsBrowserFormData(t *testing.T) {
	s, _ := newDatabaseServer(t)
	srv := providerCatalog(t)
	defer srv.Close()
	s.opts.GatewayURL, s.opts.GatewayAdminKey = srv.URL, "sk-test"
	admin := signedIn(t, s)

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for name, value := range map[string]string{
		"csrf": csrfFrom(t, s, admin, "/channels"), "channel": "google",
		"model_id": "google-1", "prompt": "测试",
	} {
		if err := form.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/channels/try", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.AddCookie(admin)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("browser FormData trial status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestParseChannelTrialFormData(t *testing.T) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("csrf", "form-token"); err != nil {
		t.Fatal(err)
	}
	if err := form.WriteField("prompt", "测试"); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/channels/try", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	if err := parseChannelTrialForm(req); err != nil {
		t.Fatal(err)
	}
	if got := req.PostFormValue("csrf"); got != "form-token" {
		t.Fatalf("CSRF from browser FormData = %q", got)
	}
	if got := req.PostFormValue("prompt"); got != "测试" {
		t.Fatalf("prompt from browser FormData = %q", got)
	}
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
	if !strings.Contains(page, "qwen3-coder") || !strings.Contains(page, `action="/channels/try"`) || !strings.Contains(page, "google-hidden") || !strings.Contains(page, `name="prompt"`) {
		t.Fatal("admin channel page should include hidden deployments and interactive trial")
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
	if !strings.Contains(page, "已暂停") || !strings.Contains(page, "Vertex 403") {
		t.Fatal("the channel page should show Google paused and why")
	}
	if modelsPage := dbGet(t, h, "/models", admin).Body.String(); !strings.Contains(modelsPage, "全部下发") {
		t.Fatal("the models page should keep the delivery controls")
	}
	// The raw gateway catalog remains visible on the channel page, but account
	// pickers must only offer models that the current channel policy allows.
	users := dbGet(t, h, "/users", admin).Body.String()
	if strings.Contains(users, "gemini-3.1-pro") {
		t.Fatal("the onboarding picker still offers a model from the paused Google channel")
	}

	// The overview only says so, and points to the page.
	if ov := dbGet(t, h, "/overview", admin).Body.String(); !strings.Contains(ov, "Google Vertex 已暂停") || !strings.Contains(ov, `href="/channels"`) || strings.Contains(ov, `action="/channels/pause"`) {
		t.Error("the overview should summarise the pause and link to channels, without the controls")
	}

	// An operator sees it and cannot touch it.
	_, pw, _ := s.dbm.auth.CreateAccount(ctx, "olga", "", "operator")
	op := signInAs(t, s, "olga", pw)
	if page := dbGet(t, h, "/channels", op).Body.String(); strings.Contains(page, `action="/channels/pause"`) || strings.Contains(page, `action="/channels/test"`) || strings.Contains(page, "google-hidden") {
		t.Error("an operator should not see admin controls or hidden deployments")
	}
	opCSRF := csrfFrom(t, s, op, "/channels")
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

func TestChannelTrialShowsActualOutputWithoutAuditingContent(t *testing.T) {
	s, _ := newDatabaseServer(t)
	srv := providerCatalog(t)
	defer srv.Close()
	s.opts.GatewayURL, s.opts.GatewayAdminKey = srv.URL, "sk-test"
	h := s.Handler()
	admin := signedIn(t, s)
	csrf := csrfFrom(t, s, admin, "/channels")
	prompt := "只回一句话"
	rec := dbPost(t, h, "/channels/try", url.Values{"csrf": {csrf}, "channel": {"google"}, "model_id": {"google-1"}, "prompt": {prompt}}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("trial status %d: %s", rec.Code, rec.Body.String())
	}
	var trial channelTrialResponse
	if err := json.NewDecoder(rec.Body).Decode(&trial); err != nil || !trial.OK || trial.Input != prompt || trial.Output != "回复："+prompt || trial.ModelID != "google-1" || trial.PromptTokens != 12 {
		t.Fatalf("trial result = %+v, err = %v", trial, err)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("trial response cache = %q", cc)
	}
	events, _, err := s.dbm.store.Audit().Search(t.Context(), repo.AuditFilter{TargetType: "channel", Action: "channel_trial", Limit: 10})
	if err != nil || len(events) != 1 || events[0].Result != "ok" || strings.Contains(string(events[0].Detail), prompt) || strings.Contains(string(events[0].Detail), trial.Output) {
		t.Fatalf("trial audit leaked content or is missing: %+v, %v", events, err)
	}
	rec = dbPost(t, h, "/channels/try", url.Values{"csrf": {csrf}, "channel": {"aws"}, "model_id": {"google-1"}, "prompt": {prompt}}, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-channel trial status = %d", rec.Code)
	}
	_, pw, _ := s.dbm.auth.CreateAccount(t.Context(), "olga", "", "operator")
	op := signInAs(t, s, "olga", pw)
	opCSRF := csrfFrom(t, s, op, "/channels")
	rec = dbPost(t, h, "/channels/try", url.Values{"csrf": {opCSRF}, "channel": {"google"}, "model_id": {"google-1"}, "prompt": {prompt}}, op)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator trial status = %d, want 403", rec.Code)
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
	_, pw, _ := s.dbm.auth.CreateAccount(t.Context(), "olga", "", "operator")
	op := signInAs(t, s, "olga", pw)
	opCSRF := csrfFrom(t, s, op, "/channels")
	rec = dbPost(t, h, "/channels/test", url.Values{"csrf": {opCSRF}, "channel": {"google"}, "model_id": {"google-1"}}, op)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator test status = %d, want 403", rec.Code)
	}
}
