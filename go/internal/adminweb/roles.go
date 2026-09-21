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
// Reads are for everybody who can sign in. Writes that change employees,
// machines, policy or releases are for operators. Managing administrators is
// for admins. Signing out and one's own account are for everybody.
var minRole = map[string]string{
	"GET /overview":                 repo.RoleViewer,
	"GET /users":                    repo.RoleViewer,
	"GET /users/detail":             repo.RoleViewer,
	"GET /sites":                    repo.RoleViewer,
	"GET /settings":                 repo.RoleViewer,
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
	"POST /machines/":               repo.RoleOperator,
	"POST /sites/":                  repo.RoleOperator,
	"POST /settings/":               repo.RoleOperator,
	"POST /releases/":               repo.RoleOperator,
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
