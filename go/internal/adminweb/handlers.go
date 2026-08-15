package adminweb

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/config"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// freshAfter mirrors cmd/admin/status_cmd.go: a machine that reported within
// this window counts as alive.
const freshAfter = 10 * time.Minute

// requireSession gates a handler on a live session, passing it through.
func (s *Server) requireSession(next func(http.ResponseWriter, *http.Request, *session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.currentSession(r)
		if sess == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		next(w, r, sess)
	}
}

func (s *Server) currentSession(r *http.Request) *session {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	return s.sessions.get(c.Value)
}

type pageData struct {
	Bucket   string
	Endpoint string
	Error    string
	Machines []admincore.MachineState
	Policy   *model.Policy
}

func (s *Server) render(w http.ResponseWriter, name string, code int, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := s.tpl.ExecuteTemplate(w, name, data); err != nil {
		// The status line is already out; log rather than write a second one.
		log.Printf("adminweb: render %s: %v", name, err)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if sess := s.currentSession(r); sess != nil {
		http.Redirect(w, r, "/machines", http.StatusSeeOther)
		return
	}
	s.render(w, "login.html", http.StatusOK, pageData{})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.limiter.allow(s.clientKey(r)) {
		s.render(w, "login.html", http.StatusTooManyRequests, pageData{Error: errRateLimited.Error()})
		return
	}
	if err := r.ParseForm(); err != nil {
		s.render(w, "login.html", http.StatusBadRequest, pageData{Error: "could not read the form"})
		return
	}
	cfg := config.Config{
		Endpoint:        strings.TrimSpace(r.PostFormValue("endpoint")),
		Bucket:          strings.TrimSpace(r.PostFormValue("bucket")),
		AccessKeyID:     strings.TrimSpace(r.PostFormValue("accessKeyId")),
		AccessKeySecret: strings.TrimSpace(r.PostFormValue("accessKeySecret")),
	}
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKeyID == "" || cfg.AccessKeySecret == "" {
		s.render(w, "login.html", http.StatusBadRequest, pageData{Error: "all four fields are required"})
		return
	}

	st, err := s.dialOSS(cfg)
	if err == nil {
		// This call IS the authentication: the console has no account of its
		// own, so whether these credentials can reach the bucket is the only
		// thing that decides access.
		err = st.Verify()
	}
	if err != nil {
		// Never echo err verbatim: it can carry the credential back to the
		// browser and into any log that records the response.
		log.Printf("adminweb: sign-in from %s rejected", s.clientKey(r))
		s.render(w, "login.html", http.StatusUnauthorized, pageData{Error: "could not reach the bucket with those credentials"})
		return
	}
	mgr := &admincore.Manager{Store: st}

	id, err := s.sessions.create(mgr, cfg.Bucket, cfg.Endpoint)
	if err != nil {
		s.render(w, "login.html", http.StatusServiceUnavailable, pageData{Error: err.Error()})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		// Strict also serves as the CSRF defence: a form posted from another
		// origin arrives without this cookie, so it cannot act as the operator.
		SameSite: http.SameSiteStrictMode,
		Secure:   s.browserUsesTLS(),
	})
	log.Printf("adminweb: sign-in from %s for bucket %s", s.clientKey(r), cfg.Bucket)
	http.Redirect(w, r, "/machines", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.destroy(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   s.browserUsesTLS(),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleMachines(w http.ResponseWriter, r *http.Request, sess *session) {
	data := pageData{Bucket: sess.bucket, Endpoint: sess.endpoint}
	// Same freshness window the CLI uses (cmd/admin/status_cmd.go), so the two
	// front ends never disagree about whether a machine is alive.
	machines, err := sess.mgr.CollectMachines(freshAfter)
	if err != nil {
		data.Error = "could not list machines"
		log.Printf("adminweb: CollectMachines: %v", err)
	} else {
		data.Machines = machines
	}
	s.render(w, "machines.html", http.StatusOK, data)
}

func (s *Server) handlePolicy(w http.ResponseWriter, r *http.Request, sess *session) {
	data := pageData{Bucket: sess.bucket, Endpoint: sess.endpoint}
	p, err := sess.mgr.CurrentPolicy()
	if err != nil {
		data.Error = "could not read the policy"
		log.Printf("adminweb: CurrentPolicy: %v", err)
	} else {
		data.Policy = &p
	}
	s.render(w, "policy.html", http.StatusOK, data)
}
