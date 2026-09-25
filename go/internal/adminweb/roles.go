package adminweb

import (
	"net/http"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// Roles, least to most. security reads everything, including the audit
// trail, and changes nothing; it ranks with viewer until it needs more.
var roleRank = map[string]int{
	repo.RoleViewer:   1,
	repo.RoleSecurity: 1,
	repo.RoleOperator: 2,
	repo.RoleAdmin:    3,
}

// minRole is the least role a request needs, by method and path. A path is
// matched exactly first, then by the longest prefix ending in "/". Anything
// not listed needs admin: a new route forgotten here is closed, not open.
//
// Reads are for everybody who can sign in. Day-to-day writes -- employees,
// machines, their budgets and models, a version pinned to chosen machines --
// are for operators. What changes every machine or cannot be undone is for
// admins, following the operations process (ai工作间流程 §2): the block
// policy, the settings, the fleet-wide version, deleting an account, and
// managing administrators. Signing out and one's own account are for
// everybody.
var minRole = map[string]string{
	"GET /overview":                 repo.RoleViewer,
	"GET /users":                    repo.RoleViewer,
	"GET /users/detail":             repo.RoleViewer,
	"GET /sites":                    repo.RoleViewer,
	"GET /settings":                 repo.RoleViewer,
	"GET /settings/":                repo.RoleViewer,
	"GET /log":                      repo.RoleViewer,
	"GET /logs":                     repo.RoleViewer,
	"GET /tasks":                    repo.RoleViewer,
	"GET /audit":                    repo.RoleViewer,
	"GET /audit.csv":                repo.RoleViewer,
	"GET /usage":                    repo.RoleViewer,
	"GET /usage.csv":                repo.RoleViewer,
	"POST /usage/":                  repo.RoleOperator,
	"GET /alerts":                   repo.RoleViewer,
	"POST /alerts/":                 repo.RoleOperator,
	"POST /alerts/test":             repo.RoleAdmin,
	"POST /settings/alerts":         repo.RoleAdmin,
	"POST /settings/alert-channels": repo.RoleAdmin,
	"POST /settings/rotation":       repo.RoleAdmin,
	"POST /settings/device-channel": repo.RoleAdmin,
	"GET /releases":                 repo.RoleViewer,
	"GET /rollouts":                 repo.RoleViewer,
	"GET /rollouts/":                repo.RoleViewer,
	"GET /machines/":                repo.RoleViewer,
	"GET /account":                  repo.RoleViewer,
	"POST /account":                 repo.RoleViewer,
	"GET /enrol":                    repo.RoleViewer,
	"POST /enrol":                   repo.RoleViewer,
	"POST /logout":                  repo.RoleViewer,
	"GET /logout":                   repo.RoleViewer,
	"GET /healthz":                  repo.RoleViewer,
	"GET /static/":                  repo.RoleViewer,
	"POST /users/":                  repo.RoleOperator,
	"POST /users/delete":            repo.RoleAdmin,
	"POST /machines/":               repo.RoleOperator,
	"POST /models/":                 repo.RoleOperator,
	"POST /channels/":               repo.RoleAdmin,
	"POST /sites/":                  repo.RoleAdmin,
	"POST /settings/":               repo.RoleAdmin,
	"POST /releases/":               repo.RoleOperator,
	"POST /releases/global":         repo.RoleAdmin,
	"POST /releases/global-clear":   repo.RoleAdmin,
	"POST /rollouts/":               repo.RoleOperator,
	"POST /tasks/":                  repo.RoleOperator,
	"GET /admins":                   repo.RoleAdmin,
	"GET /admins/":                  repo.RoleAdmin,
	"POST /admins/":                 repo.RoleAdmin,
	"GET /rollout":                  repo.RoleViewer, // redirects in the database mode
	"GET /agent/":                   repo.RoleViewer,
	"GET /codex/":                   repo.RoleViewer,
}

// requiredRole is the least role for a request.
func requiredRole(method, path string) string {
	if role, ok := minRole[method+" "+path]; ok {
		return role
	}
	best, bestLen := repo.RoleAdmin, -1
	for key, role := range minRole {
		m, p, _ := strings.Cut(key, " ")
		if m != method || !strings.HasSuffix(p, "/") || !strings.HasPrefix(path, p) {
			continue
		}
		if len(p) > bestLen {
			best, bestLen = role, len(p)
		}
	}
	return best
}

// allowed reports whether a role may make this request.
func allowed(role, method, path string) bool {
	have, ok := roleRank[role]
	if !ok {
		return false
	}
	return have >= roleRank[requiredRole(method, path)]
}

// forbid answers a request the role may not make. A page, not a redirect: the
// person should see why, and a redirect to the sign-in form would suggest
// their session is the problem.
func (s *Server) forbid(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "")
	data.Error = "你的角色是 " + sess.admin.Role + "，这个操作需要 " + requiredRole(r.Method, r.URL.Path) + "。找 admin 角色的管理员来做，或让他们调整你的角色。"
	s.render(w, "forbidden.html", http.StatusForbidden, data)
}

// mayPost is the templates' question "may this person press that button":
// an admin-only control is left out of an operator's page instead of
// answering with a refusal. The server still checks every request; this
// only keeps the page honest. An empty role is the single-user mode, where
// everything is allowed.
func mayPost(role, path string) bool {
	return role == "" || allowed(role, http.MethodPost, path)
}
