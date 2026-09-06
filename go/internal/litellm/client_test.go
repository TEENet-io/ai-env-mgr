package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(srv.URL, "sk-admin")
}

func TestGenerateKeySendsAliasAndModels(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/key/generate" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer sk-admin" {
			t.Errorf("admin key not sent, got %q", auth)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = io.WriteString(w, `{"key":"sk-emp","key_alias":"emp-alice","models":["grok-4.6"]}`)
	})

	key, err := c.GenerateKey(context.Background(), "emp-alice", []string{"grok-4.6"}, 5, map[string]string{"employee": "alice"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if key.Key != "sk-emp" {
		t.Errorf("token = %q, want sk-emp", key.Key)
	}
	if got["key_alias"] != "emp-alice" {
		t.Errorf("key_alias = %v", got["key_alias"])
	}
	if got["max_budget"] != float64(5) {
		t.Errorf("max_budget = %v", got["max_budget"])
	}
}

func TestGenerateKeyRejectsResponseWithoutToken(t *testing.T) {
	// The token is returned exactly once. A success status with no token
	// would otherwise be delivered to an employee as an empty credential.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"key_alias":"emp-alice"}`)
	})
	if _, err := c.GenerateKey(context.Background(), "emp-alice", []string{"grok-4.6"}, 0, nil); err == nil {
		t.Fatal("expected an error when the gateway returns no token")
	}
}

func TestIsAliasTakenRecognizesDuplicate(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"Key with alias 'emp-alice' already exists."}`)
	})

	_, err := c.GenerateKey(context.Background(), "emp-alice", []string{"grok-4.6"}, 0, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !IsAliasTaken(err) {
		t.Errorf("duplicate alias not recognized: %v", err)
	}
}

func TestIsAliasTakenIgnoresOtherFailures(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"boom"}`)
	})
	_, err := c.GenerateKey(context.Background(), "emp-alice", nil, 0, nil)
	if IsAliasTaken(err) {
		t.Errorf("a 500 must not be read as a duplicate alias: %v", err)
	}
}

func TestModelsFiltersOutInvisibleEntries(t *testing.T) {
	// A model can be routable for testing without belonging in the picker.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[
			{"model_name":"grok-4.6","model_info":{"display_name":"Grok 4.6","context_window":256000,"catalog_visible":true}},
			{"model_name":"probe","model_info":{"display_name":"Probe","catalog_visible":false}},
			{"model_name":"nometa","model_info":{}}
		]}`)
	})

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 1 || models[0].Name != "grok-4.6" {
		t.Fatalf("expected only the visible model, got %+v", models)
	}
	if models[0].Info.ContextWindow != 256000 {
		t.Errorf("context window not carried through: %+v", models[0].Info)
	}
}

func TestFindKeyByAliasScansTheList(t *testing.T) {
	// There is no lookup-by-alias endpoint, so this must work off /key/list.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/key/list") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"keys":[{"key_alias":"emp-bob"},{"key_alias":"emp-alice","models":["glm-5"]}]}`)
	})

	key, found, err := c.FindKeyByAlias(context.Background(), "emp-alice")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !found || len(key.Models) != 1 || key.Models[0] != "glm-5" {
		t.Fatalf("alice not found correctly: %+v found=%v", key, found)
	}

	if _, found, _ := c.FindKeyByAlias(context.Background(), "emp-nobody"); found {
		t.Error("reported a key that does not exist")
	}
}

func TestDeleteKeyWithNoKeysMakesNoRequest(t *testing.T) {
	called := false
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	if err := c.DeleteKey(context.Background()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if called {
		t.Error("empty revocation must not reach the gateway")
	}
}

func TestListKeysFollowsPagination(t *testing.T) {
	// The gateway caps a page at 100. Reading only the first page would make
	// reconciliation report a departed employee's live token as gone.
	var pages []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		pages = append(pages, r.URL.Query().Get("page"))
		if got := r.URL.Query().Get("size"); got != "100" {
			t.Errorf("size = %q, gateway rejects anything above 100", got)
		}
		switch r.URL.Query().Get("page") {
		case "1":
			var b strings.Builder
			b.WriteString(`{"total_pages":2,"keys":[`)
			for i := 0; i < 100; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"key_alias":"emp-%03d"}`, i)
			}
			b.WriteString(`]}`)
			_, _ = io.WriteString(w, b.String())
		default:
			_, _ = io.WriteString(w, `{"total_pages":2,"keys":[{"key_alias":"emp-last"}]}`)
		}
	})

	keys, err := c.ListKeys(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 101 {
		t.Fatalf("got %d keys, want 101 across two pages", len(keys))
	}
	if keys[100].KeyAlias != "emp-last" {
		t.Errorf("second page was not appended: %+v", keys[100])
	}
	if len(pages) != 2 {
		t.Errorf("expected exactly two requests, got %v", pages)
	}
}

func TestListKeysStopsOnShortPage(t *testing.T) {
	// Belt and braces: if total_pages is ever absent, a short page still ends
	// the loop rather than spinning until the guard trips.
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"keys":[{"key_alias":"emp-alice"}]}`)
	})
	keys, err := c.ListKeys(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(keys) != 1 || calls != 1 {
		t.Errorf("short page should end the loop: %d keys in %d calls", len(keys), calls)
	}
}
