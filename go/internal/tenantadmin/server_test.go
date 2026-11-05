package tenantadmin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeGateway stands in for LiteLLM: it records what reached it and answers
// lookups from a small table of keys.
type fakeGateway struct {
	calls []string // "METHOD path?query body"
	keys  []fakeKey
}

type fakeKey struct {
	handle, alias, team string
}

func (g *fakeGateway) Do(_ context.Context, method, path string, body []byte) (int, []byte, error) {
	g.calls = append(g.calls, strings.TrimSpace(method+" "+path+" "+string(body)))
	u, _ := url.Parse(path)
	switch {
	case method == "GET" && u.Path == "/key/info":
		if u.Query().Get("key") == "" {
			return 200, []byte(`{"info":{}}`), nil
		}
		for _, k := range g.keys {
			if k.handle == u.Query().Get("key") {
				return 200, []byte(`{"key":"` + k.handle + `","info":{"team_id":"` + k.team + `"}}`), nil
			}
		}
		return 404, []byte(`{"error":{"message":"not found"}}`), nil
	case method == "GET" && u.Path == "/key/list" && u.Query().Get("key_alias") != "":
		var out []map[string]any
		for _, k := range g.keys {
			if k.alias == u.Query().Get("key_alias") {
				out = append(out, map[string]any{"key_alias": k.alias, "team_id": k.team})
			}
		}
		raw, _ := json.Marshal(map[string]any{"keys": out})
		return 200, raw, nil
	case method == "GET" && u.Path == "/user/list":
		return 200, []byte(`{"users":[{"user_id":"bippo-emp-ann"},{"user_id":"windows-control","user_role":"proxy_admin"}],"total":2,"page":1,"total_pages":1}`), nil
	case method == "GET" && u.Path == "/team/info":
		return 200, []byte(`{"team_id":"bippo","team_info":{"members_with_roles":[{"user_id":"windows-control","role":"admin"}]}}`), nil
	case method == "GET" && u.Path == "/model/info":
		return 200, []byte(`{"data":[
			{"model_name":"gpt-6","litellm_params":{"model":"openai/gpt-6","api_base":"https://x.openai.azure.com/openai/v1","api_key":"secret"},"model_info":{"id":"m1","litellm_provider":"openai","catalog_visible":true,"display_name":"GPT 6"}},
			{"model_name":"claude","litellm_params":{"model":"bedrock/anthropic.claude"},"model_info":{"id":"m2","litellm_provider":"bedrock","catalog_visible":true}}]}`), nil
	}
	return 200, []byte(`{"ok":true}`), nil
}

// forwarded is what reached the gateway other than the rules' own lookups.
func (g *fakeGateway) forwarded() []string {
	var out []string
	for _, c := range g.calls {
		if strings.HasPrefix(c, "GET /key/info?key=") || strings.Contains(c, "key_alias=") && strings.Contains(c, "return_full_object=true&size=100") {
			continue
		}
		out = append(out, c)
	}
	return out
}

const (
	bippoToken = "ta-bippo-secret"
	bippoNext  = "ta-bippo-next"
	weweToken  = "ta-wewe-secret"
	offToken   = "ta-off-secret"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	var c Config
	for _, x := range []struct{ team, token string }{{"bippo", bippoToken}, {"bippo", bippoNext}, {"wewe", weweToken}, {"off", offToken}} {
		if _, err := c.AddToken(x.team, digestOf(x.token)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range c.Tenants {
		if c.Tenants[i].Team == "off" {
			c.Tenants[i].Enabled = false
		}
	}
	return c
}

func digestOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newTestServer(t *testing.T, g *fakeGateway) *Server {
	c := testConfig(t)
	return &Server{Upstream: g, Config: func() Config { return c }, Log: log.New(io.Discard, "", 0)}
}

func send(s *Server, token, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Real-IP", "198.51.100.7")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestOnlyAValidEnabledTokenGetsIn(t *testing.T) {
	g := &fakeGateway{}
	s := newTestServer(t, g)
	for _, token := range []string{"", "ta-nope", offToken, "Bearer " + bippoToken} {
		if rec := send(s, token, "GET", "/whoami", ""); rec.Code != 401 {
			t.Errorf("token %q: %d", token, rec.Code)
		}
	}
	// Both of a tenant's tokens work while it moves from one to the other.
	for _, token := range []string{bippoToken, bippoNext} {
		rec := send(s, token, "GET", "/whoami", "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"team":"bippo"`) {
			t.Errorf("token %q: %d %s", token, rec.Code, rec.Body)
		}
	}
	if rec := send(s, weweToken, "GET", "/whoami", ""); !strings.Contains(rec.Body.String(), `"team":"wewe"`) {
		t.Errorf("wewe: %s", rec.Body)
	}
	if len(g.calls) != 0 {
		t.Errorf("nothing should reach the gateway: %v", g.calls)
	}
}

func TestRepeatedBadTokensAreBlocked(t *testing.T) {
	s := newTestServer(t, &fakeGateway{})
	now := time.Unix(1_800_000_000, 0)
	s.Now = func() time.Time { return now }
	for i := 0; i < failureLimit+1; i++ {
		send(s, "ta-guess", "GET", "/whoami", "")
	}
	if rec := send(s, bippoToken, "GET", "/whoami", ""); rec.Code != 429 {
		t.Fatalf("after %d failures, even a good token: %d", failureLimit+1, rec.Code)
	}
	now = now.Add(blockFor + time.Second)
	if rec := send(s, bippoToken, "GET", "/whoami", ""); rec.Code != 200 {
		t.Fatalf("after the block: %d", rec.Code)
	}
}

func TestTheRules(t *testing.T) {
	cases := []struct {
		name, method, target, body string
		want                       int
		forwarded                  string // what LiteLLM receives, when allowed
	}{
		// team
		{"own team info", "GET", "/team/info?team_id=bippo", "", 200, "GET /team/info?team_id=bippo"},
		{"other team info", "GET", "/team/info?team_id=wewe", "", 403, ""},
		{"no team creation", "POST", "/team/new", `{"team_id":"bippo"}`, 403, ""},
		{"no team update", "POST", "/team/update", `{"team_id":"bippo","max_budget":1}`, 403, ""},
		{"add own member", "POST", "/team/member_add", `{"team_id":"bippo","member":{"role":"user","user_id":"bippo-emp-ann"}}`, 200, "POST /team/member_add"},
		{"add member as admin", "POST", "/team/member_add", `{"team_id":"bippo","member":{"role":"admin","user_id":"bippo-emp-ann"}}`, 403, ""},
		{"add other's user", "POST", "/team/member_add", `{"team_id":"bippo","member":{"role":"user","user_id":"wewe-emp-bob"}}`, 403, ""},
		{"add to other team", "POST", "/team/member_add", `{"team_id":"wewe","member":{"role":"user","user_id":"bippo-emp-ann"}}`, 403, ""},
		// users
		{"new own user", "POST", "/user/new", `{"user_id":"bippo-emp-ann","user_role":"internal_user","auto_create_key":false,"max_budget":20,"metadata":{"department":"ops"},"models":["gpt-6"]}`, 200, "POST /user/new"},
		{"new user as admin", "POST", "/user/new", `{"user_id":"bippo-emp-ann","user_role":"proxy_admin"}`, 403, ""},
		{"new user in other teams", "POST", "/user/new", `{"user_id":"bippo-emp-ann","teams":["wewe"]}`, 403, ""},
		{"new other's user", "POST", "/user/new", `{"user_id":"wewe-emp-bob"}`, 403, ""},
		{"new unprefixed user", "POST", "/user/new", `{"user_id":"bippo-ann"}`, 403, ""},
		{"bare prefix", "POST", "/user/new", `{"user_id":"bippo-emp-"}`, 403, ""},
		{"duplicated field", "POST", "/user/new", `{"user_id":"bippo-emp-ann","user_id":"wewe-emp-bob"}`, 403, ""},
		{"update with role", "POST", "/user/update", `{"user_id":"bippo-emp-ann","user_role":"internal_user"}`, 403, ""},
		{"update own user", "POST", "/user/update", `{"user_id":"bippo-emp-ann","max_budget":30}`, 200, "POST /user/update"},
		{"odd metadata", "POST", "/user/update", `{"user_id":"bippo-emp-ann","metadata":{"guardrails":"off"}}`, 403, ""},
		{"own user info", "GET", "/user/info?user_id=bippo-emp-ann", "", 200, "GET /user/info?user_id=bippo-emp-ann"},
		{"other's user info", "GET", "/user/info?user_id=wewe-emp-bob", "", 403, ""},
		{"pre-team user", "GET", "/user/info?user_id=emp-ann", "", 403, ""},
		{"delete own users", "POST", "/user/delete", `{"user_ids":["bippo-emp-ann","bippo-emp-cat"]}`, 200, "POST /user/delete"},
		{"delete a mix", "POST", "/user/delete", `{"user_ids":["bippo-emp-ann","wewe-emp-bob"]}`, 403, ""},
		{"list users", "GET", "/user/list?page=2&page_size=50", "", 200, "GET /user/list?page=2&page_size=50&team=bippo"},
		{"list other team's users", "GET", "/user/list?team=wewe", "", 403, ""},
		{"list users by role", "GET", "/user/list?role=proxy_admin", "", 403, ""},
		// keys
		{"generate own key", "POST", "/key/generate", `{"key_alias":"bippo-emp-ann-1a2b3c4d-e1","user_id":"bippo-emp-ann","team_id":"bippo","models":[],"metadata":{"windows_user":"ann","epoch":"1"}}`, 200, "POST /key/generate"},
		{"generate without team", "POST", "/key/generate", `{"key_alias":"bippo-emp-ann-1","user_id":"bippo-emp-ann"}`, 403, ""},
		{"generate in other team", "POST", "/key/generate", `{"key_alias":"bippo-emp-ann-1","user_id":"bippo-emp-ann","team_id":"wewe"}`, 403, ""},
		{"generate for other's user", "POST", "/key/generate", `{"key_alias":"bippo-emp-ann-1","user_id":"wewe-emp-bob","team_id":"bippo"}`, 403, ""},
		{"generate with routes", "POST", "/key/generate", `{"key_alias":"bippo-emp-ann-1","user_id":"bippo-emp-ann","team_id":"bippo","allowed_routes":["/key/list"]}`, 403, ""},
		{"update own key", "POST", "/key/update", `{"key":"hash-ann","models":["gpt-6"]}`, 200, `POST /key/update {"key":"hash-ann","models":["gpt-6"]}`},
		{"update other's key", "POST", "/key/update", `{"key":"hash-bob","models":[]}`, 403, ""},
		{"update untouched key", "POST", "/key/update", `{"key":"hash-legacy","models":[]}`, 403, ""},
		{"update with budget", "POST", "/key/update", `{"key":"hash-ann","max_budget":1}`, 403, ""},
		{"update unknown key", "POST", "/key/update", `{"key":"hash-gone","models":[]}`, 200, "POST /key/update"},
		{"delete own key", "POST", "/key/delete", `{"keys":["hash-ann"]}`, 200, "POST /key/delete"},
		{"delete other's key", "POST", "/key/delete", `{"keys":["hash-ann","hash-bob"]}`, 403, ""},
		{"delete own alias", "POST", "/key/delete", `{"key_aliases":["bippo-emp-ann-1a2b3c4d-e1"]}`, 200, "POST /key/delete"},
		{"delete other's alias", "POST", "/key/delete", `{"key_aliases":["wewe-emp-bob-1"]}`, 403, ""},
		{"delete squatted alias", "POST", "/key/delete", `{"key_aliases":["bippo-emp-squat"]}`, 403, ""},
		{"delete both ways", "POST", "/key/delete", `{"keys":["hash-ann"],"key_aliases":["bippo-emp-ann-1a2b3c4d-e1"]}`, 403, ""},
		{"list keys", "GET", "/key/list?return_full_object=true&size=100&page=1", "", 200, "GET /key/list?page=1&return_full_object=true&size=100&team_id=bippo"},
		{"list keys by substring", "GET", "/key/list?key_alias=bippo&substring_matching=true", "", 403, ""},
		{"list other team's keys", "GET", "/key/list?team_id=wewe", "", 403, ""},
		{"list keys by user", "GET", "/key/list?user_id=wewe-emp-bob", "", 403, ""},
		// everything else
		{"model info", "GET", "/model/info", "", 200, "GET /model/info"},
		{"chat", "POST", "/chat/completions", `{"model":"gpt-6"}`, 403, ""},
		{"health", "GET", "/health?model_id=m1", "", 403, ""},
		{"new model", "POST", "/model/new", `{}`, 403, ""},
		{"spend logs", "GET", "/spend/logs", "", 403, ""},
		{"not json", "POST", "/user/new", `user_id=bippo-emp-ann`, 403, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := &fakeGateway{keys: []fakeKey{
				{"hash-ann", "bippo-emp-ann-1a2b3c4d-e1", "bippo"},
				{"hash-bob", "wewe-emp-bob-1", "wewe"},
				{"hash-legacy", "emp-ann-e1", ""},
				{"hash-squat", "bippo-emp-squat", "wewe"},
			}}
			rec := send(newTestServer(t, g), bippoToken, c.method, c.target, c.body)
			if rec.Code != c.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.want, rec.Body)
			}
			fw := g.forwarded()
			if c.want != 200 {
				if len(fw) != 0 {
					t.Fatalf("a refused request reached the gateway: %v", fw)
				}
				if !strings.Contains(rec.Body.String(), "tenant_forbidden") {
					t.Fatalf("refusal body: %s", rec.Body)
				}
				return
			}
			if len(fw) != 1 || !strings.HasPrefix(fw[0], c.forwarded) {
				t.Fatalf("forwarded %v, want %q", fw, c.forwarded)
			}
		})
	}
}

func TestTheGatewaysOwnAccountStaysHidden(t *testing.T) {
	s := newTestServer(t, &fakeGateway{})
	users := send(s, bippoToken, "GET", "/user/list", "").Body.String()
	if !strings.Contains(users, "bippo-emp-ann") || strings.Contains(users, "windows-control") || !strings.Contains(users, `"total_pages":1`) {
		t.Fatalf("user list: %s", users)
	}
	if team := send(s, bippoToken, "GET", "/team/info?team_id=bippo", "").Body.String(); strings.Contains(team, "windows-control") || !strings.Contains(team, `"team_id":"bippo"`) {
		t.Fatalf("team info: %s", team)
	}
}

func TestModelInfoHidesWhereModelsAreServed(t *testing.T) {
	rec := send(newTestServer(t, &fakeGateway{}), bippoToken, "GET", "/model/info", "")
	body := rec.Body.String()
	for _, hidden := range []string{"litellm_params", "api_base", "azure.com", "secret", "bedrock/", `"litellm_provider":"openai"`} {
		if strings.Contains(body, hidden) {
			t.Errorf("response shows %q: %s", hidden, body)
		}
	}
	var out struct {
		Data []struct {
			Name string `json:"model_name"`
			Info struct {
				ID      string `json:"id"`
				Channel string `json:"channel"`
				Display string `json:"display_name"`
			} `json:"model_info"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Data) != 2 {
		t.Fatalf("%v: %s", err, body)
	}
	// The channel is worked out before its clues are dropped.
	if out.Data[0].Info.Channel != "azure" || out.Data[1].Info.Channel != "aws" || out.Data[0].Info.Display != "GPT 6" || out.Data[0].Info.ID != "m1" {
		t.Fatalf("%+v", out.Data)
	}
}

// Nothing the console sends in its headers reaches LiteLLM: a key in
// x-litellm-api-key would otherwise take precedence over the one tenant-admin
// sets.
func TestTheConsolesHeadersStayBehind(t *testing.T) {
	var got http.Header
	litellm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		io.WriteString(w, `{"ok":true}`)
	}))
	defer litellm.Close()
	c := testConfig(t)
	s := &Server{Upstream: HTTPUpstream{BaseURL: litellm.URL, AdminKey: "sk-admin"}, Config: func() Config { return c }}
	req := httptest.NewRequest("GET", "/team/info?team_id=bippo", nil)
	req.Header.Set("Authorization", "Bearer "+bippoToken)
	req.Header.Set("x-litellm-api-key", "sk-someone-elses")
	req.Header.Set("api-key", "sk-someone-elses")
	req.Header.Set("Cookie", "token=sk-someone-elses")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got.Get("Authorization") != "Bearer sk-admin" {
		t.Fatalf("Authorization = %q", got.Get("Authorization"))
	}
	for name, values := range got {
		for _, v := range values {
			if strings.Contains(v, "someone-elses") || strings.Contains(v, bippoToken) {
				t.Fatalf("header %s reached LiteLLM: %q", name, v)
			}
		}
	}
}

func TestHealthzNeedsNoTokenAndChecksTheKey(t *testing.T) {
	g := &fakeGateway{}
	if rec := send(newTestServer(t, g), "", "GET", "/healthz", ""); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(g.calls) != 1 || g.calls[0] != "GET /key/list?size=1" {
		t.Fatalf("calls %v", g.calls)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	path := t.TempDir() + "/tenants.json"
	if c, err := LoadConfig(path); err != nil || len(c.Tenants) != 0 {
		t.Fatalf("missing file: %+v %v", c, err)
	}
	plain, digest, err := NewToken()
	if err != nil || !strings.HasPrefix(plain, "ta-") {
		t.Fatal(plain, err)
	}
	var c Config
	id, err := c.AddToken("wewe", digest)
	if err != nil || id != "wewe-1" {
		t.Fatal(id, err)
	}
	if _, err := c.AddToken("We We", digest); err == nil {
		t.Fatal("a bad team was accepted")
	}
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if who, ok := loaded.Authenticate(plain); !ok || who.Team != "wewe" || who.TokenID != "wewe-1" {
		t.Fatalf("%+v %v", who, ok)
	}
	if err := loaded.RemoveToken("wewe-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.Authenticate(plain); ok {
		t.Fatal("a removed token still works")
	}
}
