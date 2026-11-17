package adminweb

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// localDirStore implements store + worker.PackageSource + deviceapi.Presigner
// over a local directory on disk, so a local test console needs zero cloud
// credentials and cannot touch any production OSS bucket.
type localDirStore struct {
	root string
	mu   sync.RWMutex
}

func newLocalDirStore(root string) (*localDirStore, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	s := &localDirStore{root: abs}
	if _, ok, err := s.Head(ossclient.PolicyKey()); err == nil && !ok {
		_ = s.Put(ossclient.PolicyKey(), []byte(`{"syncIntervalMinutes":15}`))
	}
	return s, nil
}

func (s *localDirStore) resolve(key string) (string, error) {
	cleaned := path.Clean("/" + strings.TrimPrefix(key, "/"))
	rel := strings.TrimPrefix(cleaned, "/")
	if rel == "" || rel == "." {
		return "", fmt.Errorf("invalid empty object key %q", key)
	}
	full := filepath.Join(s.root, filepath.FromSlash(rel))
	if !strings.HasPrefix(full, s.root+string(filepath.Separator)) {
		return "", fmt.Errorf("object key escapes root: %q", key)
	}
	return full, nil
}

func (s *localDirStore) Verify() error { return nil }

func (s *localDirStore) Get(key string) ([]byte, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, err := s.resolve(key)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", ossclient.ErrNotFound
		}
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:8]), nil
}

func (s *localDirStore) Put(key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.resolve(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp." + randHex(6)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (s *localDirStore) PutFile(key, srcPath string, onProgress func(done, total int64)) error {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return err
	}
	if err := s.Put(key, data); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(int64(len(data)), int64(len(data)))
	}
	return nil
}

func (s *localDirStore) List(prefix string) ([]string, error) {
	infos, err := s.ListInfo(prefix)
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(infos))
	for i, info := range infos {
		keys[i] = info.Key
	}
	return keys, nil
}

func (s *localDirStore) ListInfo(prefix string) ([]ossclient.ObjectInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []ossclient.ObjectInfo
	err := filepath.WalkDir(s.root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || strings.Contains(d.Name(), ".tmp.") {
			return nil
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, ossclient.ObjectInfo{
			Key:          key,
			Size:         st.Size(),
			LastModified: st.ModTime().UTC(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (s *localDirStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *localDirStore) Copy(src, dst string) error {
	data, _, err := s.Get(src)
	if err != nil {
		return err
	}
	return s.Put(dst, data)
}

func (s *localDirStore) Head(key string) (string, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, err := s.resolve(key)
	if err != nil {
		return "", false, err
	}
	st, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	return fmt.Sprintf("%x-%x", st.Size(), st.ModTime().UnixNano()), true, nil
}

func (s *localDirStore) HashObject(key string) (string, int64, error) {
	data, _, err := s.Get(key)
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), int64(len(data)), nil
}

func (s *localDirStore) SignedURL(key string, _ time.Duration) (string, error) {
	return "https://mock-oss.local/" + strings.TrimPrefix(key, "/"), nil
}

func randHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// StartMockGateway launches an in-memory LiteLLM management API stub on
// 127.0.0.1:0 so a local test console can onboard/offboard employees, sync
// model catalogs, and reconcile keys without ever contacting production LiteLLM.
func StartMockGateway() (baseURL, adminKey string, stop func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", nil, err
	}
	adminKey = "mock-admin-" + randHex(12)
	mg := &mockGatewayState{
		adminKey: adminKey,
		teams:    map[string]bool{},
		users:    map[string]litellm.User{},
		keys:     map[string]mockStoredKey{},
		models: []litellm.Model{
			{
				Name: "gpt-5.5",
				Info: litellm.ModelInfo{
					ID: "mock-gpt-5.5", Mode: "chat", DisplayName: "GPT-5.5 (Mock)",
					ContextWindow: 272000, ReasoningLevels: []string{"low", "medium", "high"},
					DefaultReasoning: "medium", Modalities: []string{"text", "image"},
					CatalogVisible: true, LitellmProvider: "openai", Channel: "mock",
				},
				Params: litellm.ModelParams{Model: "openai/gpt-5.5"},
			},
			{
				Name: "claude-sonnet-5-5",
				Info: litellm.ModelInfo{
					ID: "mock-claude-sonnet-5-5", Mode: "chat", DisplayName: "Claude Sonnet 5.5 (Mock)",
					ContextWindow: 200000, Modalities: []string{"text", "image"},
					CatalogVisible: true, LitellmProvider: "anthropic", Channel: "mock",
				},
				Params: litellm.ModelParams{Model: "anthropic/claude-sonnet-5-5"},
			},
			{
				Name: "deepseek-v3.2",
				Info: litellm.ModelInfo{
					ID: "mock-deepseek-v3.2", Mode: "chat", DisplayName: "DeepSeek V3.2 (Mock)",
					ContextWindow: 128000, Modalities: []string{"text"},
					CatalogVisible: true, LitellmProvider: "deepseek", Channel: "mock",
				},
				Params: litellm.ModelParams{Model: "deepseek/deepseek-chat"},
			},
		},
	}
	srv := &http.Server{Handler: mg}
	go func() { _ = srv.Serve(ln) }()
	return "http://" + ln.Addr().String(), adminKey, func() { _ = srv.Close() }, nil
}

type mockStoredKey struct {
	litellm.Key
	Plain  string
	TeamID string
}

type mockGatewayState struct {
	mu       sync.Mutex
	adminKey string
	teams    map[string]bool
	users    map[string]litellm.User
	keys     map[string]mockStoredKey // keyed by Token hash
	models   []litellm.Model
}

func (m *mockGatewayState) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+m.adminKey {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	m.mu.Lock()
	defer m.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/model/info":
		_ = json.NewEncoder(w).Encode(map[string]any{"data": m.models})

	case r.Method == http.MethodGet && r.URL.Path == "/health":
		id := r.URL.Query().Get("model_id")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"healthy_count":       1,
			"unhealthy_count":     0,
			"healthy_endpoints":   []map[string]string{{"model_id": id}},
			"unhealthy_endpoints": []any{},
		})

	case r.Method == http.MethodGet && r.URL.Path == "/team/info":
		tid := r.URL.Query().Get("team_id")
		if !m.teams[tid] {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"team not found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"team_id": tid})

	case r.Method == http.MethodPost && r.URL.Path == "/team/new":
		var body struct {
			TeamID string `json:"team_id"`
		}
		_ = decodeBody(r.Body, &body)
		m.teams[body.TeamID] = true
		_ = json.NewEncoder(w).Encode(map[string]any{"team_id": body.TeamID})

	case r.Method == http.MethodPost && r.URL.Path == "/team/member_add":
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})

	case r.Method == http.MethodGet && r.URL.Path == "/user/info":
		uid := r.URL.Query().Get("user_id")
		u, ok := m.users[uid]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"user not found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"user_info": u})

	case r.Method == http.MethodPost && (r.URL.Path == "/user/new" || r.URL.Path == "/user/update"):
		var body struct {
			UserID         string            `json:"user_id"`
			Alias          string            `json:"user_alias"`
			MaxBudget      *float64          `json:"max_budget"`
			BudgetDuration string            `json:"budget_duration"`
			RPMLimit       *int              `json:"rpm_limit"`
			TPMLimit       *int              `json:"tpm_limit"`
			MaxParallel    *int              `json:"max_parallel_requests"`
			Models         []string          `json:"models"`
			Metadata       map[string]string `json:"metadata"`
		}
		_ = decodeBody(r.Body, &body)
		u := m.users[body.UserID]
		u.UserID = body.UserID
		u.Alias = body.Alias
		if body.MaxBudget != nil {
			u.MaxBudget = body.MaxBudget
		}
		if body.BudgetDuration != "" {
			u.BudgetDuration = body.BudgetDuration
		}
		if body.RPMLimit != nil {
			u.RPMLimit = body.RPMLimit
		}
		if body.TPMLimit != nil {
			u.TPMLimit = body.TPMLimit
		}
		if body.MaxParallel != nil {
			u.MaxParallel = body.MaxParallel
		}
		if body.Models != nil {
			u.Models = body.Models
		}
		if body.Metadata != nil {
			u.Metadata = body.Metadata
		}
		m.users[body.UserID] = u
		_ = json.NewEncoder(w).Encode(u)

	case r.Method == http.MethodPost && r.URL.Path == "/user/delete":
		var body struct {
			UserIDs []string `json:"user_ids"`
		}
		_ = decodeBody(r.Body, &body)
		for _, id := range body.UserIDs {
			delete(m.users, id)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"deleted": len(body.UserIDs)})

	case r.Method == http.MethodGet && r.URL.Path == "/user/list":
		var list []litellm.User
		for _, u := range m.users {
			list = append(list, u)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"users": list, "total_pages": 1})

	case r.Method == http.MethodPost && r.URL.Path == "/key/generate":
		var body struct {
			KeyAlias string   `json:"key_alias"`
			UserID   string   `json:"user_id"`
			TeamID   string   `json:"team_id"`
			Models   []string `json:"models"`
		}
		_ = decodeBody(r.Body, &body)
		for _, existing := range m.keys {
			if existing.KeyAlias == body.KeyAlias && body.KeyAlias != "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"key_alias already exists"}`))
				return
			}
		}
		plain := "sk-mock-" + randHex(16)
		sum := sha256.Sum256([]byte(plain))
		tok := hex.EncodeToString(sum[:])
		k := litellm.Key{
			Token:    tok,
			KeyAlias: body.KeyAlias,
			UserID:   body.UserID,
			Models:   body.Models,
		}
		m.keys[tok] = mockStoredKey{Key: k, Plain: plain, TeamID: body.TeamID}
		out := k
		out.Key = plain
		_ = json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodPost && r.URL.Path == "/key/update":
		var body struct {
			Key    string   `json:"key"`
			Models []string `json:"models"`
		}
		_ = decodeBody(r.Body, &body)
		if tok, ok := m.resolveHandle(body.Key); ok {
			sk := m.keys[tok]
			sk.Models = body.Models
			m.keys[tok] = sk
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})

	case r.Method == http.MethodPost && r.URL.Path == "/key/delete":
		var body struct {
			Keys       []string `json:"keys"`
			KeyAliases []string `json:"key_aliases"`
		}
		_ = decodeBody(r.Body, &body)
		for _, h := range body.Keys {
			if tok, ok := m.resolveHandle(h); ok {
				delete(m.keys, tok)
			}
		}
		for _, alias := range body.KeyAliases {
			for tok, sk := range m.keys {
				if sk.KeyAlias == alias {
					delete(m.keys, tok)
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})

	case r.Method == http.MethodGet && r.URL.Path == "/key/list":
		team := r.URL.Query().Get("team_id")
		var list []litellm.Key
		for _, sk := range m.keys {
			if team != "" && sk.TeamID != team {
				continue
			}
			list = append(list, sk.Key)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": list, "total_pages": 1})

	default:
		http.NotFound(w, r)
	}
}

func (m *mockGatewayState) resolveHandle(h string) (string, bool) {
	if _, ok := m.keys[h]; ok {
		return h, true
	}
	sum := sha256.Sum256([]byte(h))
	tok := hex.EncodeToString(sum[:])
	_, ok := m.keys[tok]
	return tok, ok
}

func decodeBody(r io.Reader, out any) error {
	defer func() {
		if rc, ok := r.(io.Closer); ok {
			_ = rc.Close()
		}
	}()
	return json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(out)
}
