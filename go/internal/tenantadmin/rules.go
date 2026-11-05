package tenantadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
)

// Upstream is LiteLLM, called with the administrator key.
type Upstream interface {
	Do(ctx context.Context, method, path string, body []byte) (status int, payload []byte, err error)
}

// call is a request that passed the rules, rebuilt from the checked parts
// only: nothing the console sent reaches LiteLLM unless a rule named it.
type call struct {
	method    string
	path      string // with query
	body      []byte
	transform func([]byte) ([]byte, error) // applied to a 2xx response
}

// denied is a request outside the tenant's team. It is answered 403, never
// 404: the console reads 404 from a delete as "already gone".
type denied struct{ reason string }

func (d denied) Error() string { return d.reason }

func deny(format string, args ...any) error { return denied{fmt.Sprintf(format, args...)} }

// lookupFailed is a rule that could not tell whose something is, because
// LiteLLM did not answer. It is answered 502 and nothing is forwarded.
type lookupFailed struct{ err error }

func (l lookupFailed) Error() string { return l.err.Error() }

// Fields a console may send. Anything else in a body refuses the request:
// these lists are what keeps out user_role=proxy_admin, teams, allowed_routes
// and the like.
var (
	userFields    = []string{"user_id", "user_alias", "max_budget", "budget_duration", "rpm_limit", "tpm_limit", "max_parallel_requests", "metadata", "models"}
	newUserFields = append(slices.Clone(userFields), "user_role", "auto_create_key")
	userMetadata  = []string{"department"}
	keyFields     = []string{"key_alias", "user_id", "models", "team_id", "metadata"}
	keyMetadata   = []string{"windows_user", "epoch", "employee"}
)

// owned reports whether a user id or key alias is the team's. The console
// names both <team>-emp-<user>[...] (litellm.EmployeeUserID), team ids carry
// no hyphen, and /user/new and /key/generate only accept names with the
// caller's own prefix, so no other tenant can hold a name with this one.
func owned(team, name string) bool {
	prefix := team + "-emp-"
	return len(name) > len(prefix) && strings.HasPrefix(name, prefix)
}

// plan checks one request from team and returns what to send to LiteLLM.
// path is relative to /tenant-admin.
func plan(ctx context.Context, up Upstream, team, method, path string, query url.Values, body []byte) (call, error) {
	switch method + " " + path {
	case "GET /team/info":
		if err := onlyParams(query, "team_id"); err != nil {
			return call{}, err
		}
		if query.Get("team_id") != team {
			return call{}, deny("team_id must be %s", team)
		}
		c := get("/team/info", url.Values{"team_id": {team}})
		// The console only asks whether the team exists. Its members include
		// the gateway account that created it, which is not the tenant's to see.
		c.transform = func([]byte) ([]byte, error) { return json.Marshal(map[string]string{"team_id": team}) }
		return c, nil

	case "POST /team/member_add":
		m, err := object(body, "team_id", "member")
		if err != nil {
			return call{}, err
		}
		if str(m, "team_id") != team {
			return call{}, deny("team_id must be %s", team)
		}
		member, err := object(m["member"], "role", "user_id")
		if err != nil {
			return call{}, deny("member: %v", err)
		}
		if str(member, "role") != "user" {
			return call{}, deny("member.role must be user")
		}
		if !owned(team, str(member, "user_id")) {
			return call{}, deny("member.user_id must start with %s-emp-", team)
		}
		return post("/team/member_add", m), nil

	case "POST /user/new", "POST /user/update":
		allowed := userFields
		if path == "/user/new" {
			allowed = newUserFields
		}
		m, err := object(body, allowed...)
		if err != nil {
			return call{}, err
		}
		if !owned(team, str(m, "user_id")) {
			return call{}, deny("user_id must start with %s-emp-", team)
		}
		if raw, ok := m["user_role"]; ok && !isJSON(raw, `"internal_user"`) {
			return call{}, deny("user_role must be internal_user")
		}
		if raw, ok := m["auto_create_key"]; ok && !isJSON(raw, `false`) {
			return call{}, deny("auto_create_key must be false")
		}
		if err := stringMap(m["metadata"], userMetadata); err != nil {
			return call{}, err
		}
		if err := stringList(m["models"]); err != nil {
			return call{}, err
		}
		return post(path, m), nil

	case "GET /user/info":
		if err := onlyParams(query, "user_id"); err != nil {
			return call{}, err
		}
		id := query.Get("user_id")
		if !owned(team, id) {
			return call{}, deny("user_id must start with %s-emp-", team)
		}
		return get("/user/info", url.Values{"user_id": {id}}), nil

	case "POST /user/delete":
		m, err := object(body, "user_ids")
		if err != nil {
			return call{}, err
		}
		var ids []string
		if json.Unmarshal(m["user_ids"], &ids) != nil || len(ids) == 0 {
			return call{}, deny("user_ids must be a list of user ids")
		}
		for _, id := range ids {
			if !owned(team, id) {
				return call{}, deny("user_ids: %q does not start with %s-emp-", id, team)
			}
		}
		return post("/user/delete", m), nil

	case "GET /user/list":
		if err := onlyParams(query, "page", "page_size", "team"); err != nil {
			return call{}, err
		}
		if t := query.Get("team"); t != "" && t != team {
			return call{}, deny("team must be %s", team)
		}
		q := pick(query, "page", "page_size")
		q.Set("team", team)
		c := get("/user/list", q)
		c.transform = func(payload []byte) ([]byte, error) { return onlyOwnedUsers(team, payload) }
		return c, nil

	case "POST /key/generate":
		m, err := object(body, keyFields...)
		if err != nil {
			return call{}, err
		}
		if str(m, "team_id") != team {
			return call{}, deny("team_id must be %s", team)
		}
		if !owned(team, str(m, "user_id")) {
			return call{}, deny("user_id must start with %s-emp-", team)
		}
		if !owned(team, str(m, "key_alias")) {
			return call{}, deny("key_alias must start with %s-emp-", team)
		}
		if err := stringMap(m["metadata"], keyMetadata); err != nil {
			return call{}, err
		}
		if err := stringList(m["models"]); err != nil {
			return call{}, err
		}
		return post("/key/generate", m), nil

	case "POST /key/update":
		m, err := object(body, "key", "models")
		if err != nil {
			return call{}, err
		}
		if err := stringList(m["models"]); err != nil {
			return call{}, err
		}
		if err := keyInTeam(ctx, up, team, str(m, "key")); err != nil {
			return call{}, err
		}
		return post("/key/update", m), nil

	case "POST /key/delete":
		m, err := object(body, "keys", "key_aliases")
		if err != nil {
			return call{}, err
		}
		if len(m) != 1 {
			return call{}, deny("send exactly one of keys, key_aliases")
		}
		var names []string
		for _, raw := range m {
			if json.Unmarshal(raw, &names) != nil || len(names) == 0 {
				return call{}, deny("expected a non-empty list of strings")
			}
		}
		for _, name := range names {
			if _, byAlias := m["key_aliases"]; byAlias {
				err = aliasInTeam(ctx, up, team, name)
			} else {
				err = keyInTeam(ctx, up, team, name)
			}
			if err != nil {
				return call{}, err
			}
		}
		return post("/key/delete", m), nil

	case "GET /key/list":
		if err := onlyParams(query, "page", "size", "return_full_object", "key_alias", "team_id"); err != nil {
			return call{}, err
		}
		if t := query.Get("team_id"); t != "" && t != team {
			return call{}, deny("team_id must be %s", team)
		}
		q := pick(query, "page", "size", "return_full_object", "key_alias")
		q.Set("team_id", team)
		return get("/key/list", q), nil

	case "GET /model/info":
		if err := onlyParams(query); err != nil {
			return call{}, err
		}
		c := get("/model/info", nil)
		c.transform = redactModels
		return c, nil
	}
	return call{}, deny("%s %s is not available to tenants", method, path)
}

// keyInTeam checks a token handle (plaintext or hash) belongs to team. A key
// the gateway does not know is let through: LiteLLM then answers 404 itself,
// which is the truth.
func keyInTeam(ctx context.Context, up Upstream, team, handle string) error {
	if handle == "" {
		return deny("empty key")
	}
	status, payload, err := up.Do(ctx, http.MethodGet, "/key/info?"+url.Values{"key": {handle}}.Encode(), nil)
	if err != nil {
		return lookupFailed{err}
	}
	if status == http.StatusNotFound {
		return nil
	}
	if status != http.StatusOK {
		return lookupFailed{fmt.Errorf("look up key: gateway answered %d", status)}
	}
	var out struct {
		Info struct {
			TeamID *string `json:"team_id"`
		} `json:"info"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return lookupFailed{fmt.Errorf("look up key: %w", err)}
	}
	if out.Info.TeamID == nil || *out.Info.TeamID != team {
		return deny("that key is not in team %s", team)
	}
	return nil
}

// aliasInTeam checks every key carrying alias is team's. LiteLLM deletes by
// alias across the gateway, so the name prefix alone is not trusted.
func aliasInTeam(ctx context.Context, up Upstream, team, alias string) error {
	if !owned(team, alias) {
		return deny("key alias %q does not start with %s-emp-", alias, team)
	}
	q := url.Values{"key_alias": {alias}, "return_full_object": {"true"}, "size": {"100"}}
	status, payload, err := up.Do(ctx, http.MethodGet, "/key/list?"+q.Encode(), nil)
	if err != nil {
		return lookupFailed{err}
	}
	if status != http.StatusOK {
		return lookupFailed{fmt.Errorf("look up key alias: gateway answered %d", status)}
	}
	var out struct {
		Keys []struct {
			Alias  string  `json:"key_alias"`
			TeamID *string `json:"team_id"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return lookupFailed{fmt.Errorf("look up key alias: %w", err)}
	}
	for _, k := range out.Keys {
		if k.Alias == alias && (k.TeamID == nil || *k.TeamID != team) {
			return deny("key alias %q is not in team %s", alias, team)
		}
	}
	return nil
}

// onlyOwnedUsers drops users of the team that are not the tenant's
// employees: LiteLLM makes whoever created a team its admin member, and that
// is our gateway account. Other fields of the page pass through as they are.
func onlyOwnedUsers(team string, payload []byte) ([]byte, error) {
	var page map[string]json.RawMessage
	if err := json.Unmarshal(payload, &page); err != nil {
		return nil, err
	}
	var users []map[string]json.RawMessage
	if err := json.Unmarshal(page["users"], &users); err != nil {
		return nil, err
	}
	kept := make([]map[string]json.RawMessage, 0, len(users))
	for _, u := range users {
		if owned(team, str(u, "user_id")) {
			kept = append(kept, u)
		}
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	page["users"] = raw
	return json.Marshal(page)
}

// redactModels keeps what a console reads from /model/info (litellm.Model)
// and drops where each model is served from: litellm_params, which names
// upstream endpoints, and the provider. The channel a console groups models
// by is worked out here, before the fields it is inferred from are dropped.
func redactModels(payload []byte) ([]byte, error) {
	var in struct {
		Data []litellm.Model `json:"data"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	type model struct {
		Name string            `json:"model_name"`
		Info litellm.ModelInfo `json:"model_info"`
	}
	out := struct {
		Data []model `json:"data"`
	}{Data: make([]model, 0, len(in.Data))}
	for _, m := range in.Data {
		info := m.Info
		info.Channel = litellm.ChannelOf(m)
		info.LitellmProvider = ""
		out.Data = append(out.Data, model{Name: m.Name, Info: info})
	}
	return json.Marshal(out)
}

func get(path string, q url.Values) call {
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return call{method: http.MethodGet, path: path}
}

// post sends the checked fields re-encoded rather than the bytes received,
// so LiteLLM parses exactly what the rules looked at.
func post(path string, m map[string]json.RawMessage) call {
	body, _ := json.Marshal(m)
	return call{method: http.MethodPost, path: path, body: body}
}

// object decodes a JSON object and refuses fields outside allowed.
func object(raw []byte, allowed ...string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, deny("expected a JSON object")
	}
	for k := range m {
		if !slices.Contains(allowed, k) {
			return nil, deny("field %q is not allowed", k)
		}
	}
	return m, nil
}

func str(m map[string]json.RawMessage, k string) string {
	var s string
	if json.Unmarshal(m[k], &s) != nil {
		return ""
	}
	return s
}

func isJSON(raw json.RawMessage, want string) bool {
	return string(bytes.TrimSpace(raw)) == want
}

// stringMap allows an absent or null value, or an object of strings whose
// keys are in allowed.
func stringMap(raw json.RawMessage, allowed []string) error {
	if len(raw) == 0 || isJSON(raw, "null") {
		return nil
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) != nil {
		return deny("metadata must be an object of strings")
	}
	for k := range m {
		if !slices.Contains(allowed, k) {
			return deny("metadata field %q is not allowed", k)
		}
	}
	return nil
}

func stringList(raw json.RawMessage) error {
	if len(raw) == 0 || isJSON(raw, "null") {
		return nil
	}
	var l []string
	if json.Unmarshal(raw, &l) != nil {
		return deny("models must be a list of strings")
	}
	return nil
}

func onlyParams(q url.Values, allowed ...string) error {
	for k := range q {
		if !slices.Contains(allowed, k) {
			return deny("query parameter %q is not allowed", k)
		}
	}
	return nil
}

func pick(q url.Values, keys ...string) url.Values {
	out := url.Values{}
	for _, k := range keys {
		if v := q.Get(k); v != "" {
			out.Set(k, v)
		}
	}
	return out
}
