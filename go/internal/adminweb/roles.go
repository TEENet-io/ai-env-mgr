package adminweb

import (
	"net/http"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// Roles, least to most: a viewer reads, an admin does everything else.
var roleRank = map[string]int{
	repo.RoleViewer: 1,
	repo.RoleAdmin:  2,
}

// minRole is the least role a request needs, by method and path. A path is
// matched exactly first, then by the longest prefix ending in "/". Anything
// not listed needs admin: a new route forgotten here is closed, not open.
//
// Reads are for everybody who can sign in, and so are signing out and one's
// own account. Every other write is for admins.
var minRole = map[string]string{
	"GET /overview":                      repo.RoleViewer,
	"GET /users":                         repo.RoleViewer,
	"GET /users/detail":                  repo.RoleViewer,
	"GET /sites":                         repo.RoleViewer,
	"GET /settings":                      repo.RoleViewer,
	"GET /settings/":                     repo.RoleViewer,
	"GET /log":                           repo.RoleViewer,
	"GET /logs":                          repo.RoleViewer,
	"GET /tasks":                         repo.RoleViewer,
	"GET /tickets":                       repo.RoleViewer,
	"GET /tickets/new":                   repo.RoleAdmin,
	"POST /tickets/create":               repo.RoleAdmin,
	"GET /tickets/detail":                repo.RoleViewer,
	"GET /tickets/file":                  repo.RoleViewer,
	"POST /tickets/action":               repo.RoleAdmin,
	"POST /tickets/contacts/":            repo.RoleAdmin,
	"GET /tasks/detail":                  repo.RoleViewer,
	"GET /audit":                         repo.RoleViewer,
	"GET /audit.csv":                     repo.RoleViewer,
	"GET /usage":                         repo.RoleViewer,
	"GET /usage.csv":                     repo.RoleViewer,
	"GET /usage/keys":                    repo.RoleViewer,
	"GET /usage/keys.csv":                repo.RoleViewer,
	"GET /usage/cloud":                   repo.RoleViewer,
	"GET /usage/cloud.csv":               repo.RoleViewer,
	"POST /usage/":                       repo.RoleAdmin,
	"GET /alerts":                        repo.RoleViewer,
	"POST /alerts/":                      repo.RoleAdmin,
	"POST /alerts/test":                  repo.RoleAdmin,
	"POST /settings/alerts":              repo.RoleAdmin,
	"POST /settings/alert-channels":      repo.RoleAdmin,
	"POST /settings/rotation":            repo.RoleAdmin,
	"POST /settings/device-channel":      repo.RoleAdmin,
	"POST /settings/gateway-team":        repo.RoleAdmin,
	"POST /settings/ticket-tools":        repo.RoleAdmin,
	"GET /releases":                      repo.RoleViewer,
	"GET /applications":                  repo.RoleViewer,
	"GET /rollouts":                      repo.RoleViewer,
	"GET /rollouts/":                     repo.RoleViewer,
	"GET /machines/":                     repo.RoleViewer,
	"GET /account":                       repo.RoleViewer,
	"POST /account":                      repo.RoleViewer,
	"GET /enrol":                         repo.RoleViewer,
	"POST /enrol":                        repo.RoleViewer,
	"POST /logout":                       repo.RoleViewer,
	"GET /logout":                        repo.RoleViewer,
	"GET /healthz":                       repo.RoleViewer,
	"GET /static/":                       repo.RoleViewer,
	"POST /users/":                       repo.RoleAdmin,
	"POST /users/delete":                 repo.RoleAdmin,
	"POST /machines/":                    repo.RoleAdmin,
	"GET /models":                        repo.RoleViewer,
	"GET /channels":                      repo.RoleViewer,
	"POST /models/":                      repo.RoleAdmin,
	"POST /channels/":                    repo.RoleAdmin,
	"POST /sites/":                       repo.RoleAdmin,
	"POST /settings/":                    repo.RoleAdmin,
	"POST /releases/":                    repo.RoleAdmin,
	"POST /releases/global":              repo.RoleAdmin,
	"POST /releases/global-clear":        repo.RoleAdmin,
	"POST /releases/delete":              repo.RoleAdmin,
	"POST /releases/delete-unregistered": repo.RoleAdmin,
	"POST /apps/delete":                  repo.RoleAdmin,
	"POST /rollouts/":                    repo.RoleAdmin,
	"POST /tasks/":                       repo.RoleAdmin,
	"GET /admins":                        repo.RoleAdmin,
	"GET /admins/":                       repo.RoleAdmin,
	"POST /admins/":                      repo.RoleAdmin,
	"GET /rollout":                       repo.RoleViewer, // redirects in the database mode
	"GET /agent/":                        repo.RoleViewer,
	"GET /codex/":                        repo.RoleViewer,
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
	data.Error = "你的角色是 " + sess.admin.Role + "，這個操作需要 " + requiredRole(r.Method, r.URL.Path) + "。找 admin 角色的管理員來做，或讓他們調整你的角色。"
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
