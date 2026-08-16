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
	Fixed    bool // the OSS location is baked in, so sign-in only asks for the key
	CSRF     string
	Error    string
	OK       bool
	Nav      string // which nav entry to mark active

	Machines []admincore.MachineState
	Fleet    *fleetSummary
	Users    []model.UserEntry
	Policy   *model.Policy
	Stats    []admincore.CollectStat
	Machine  string // the machine a log belongs to
	Log      string
	Notes    *admincore.MachineState // that machine's own errors and warnings
	Job      *job                    // a publish in flight, or the last one
	Files    []admincore.StagedFile
	Link     string // a freshly minted download link
	LinkName string

	PendingUser string // an employee sign-in waiting for the pasted callback
	PendingTool string
	AuthURL     string
}

// newPage seeds the fields every page needs, including the notices carried
// through the redirect that follows a POST.
func newPage(sess *session, r *http.Request, nav string) pageData {
	return pageData{
		Bucket:   sess.bucket,
		Endpoint: sess.endpoint,
		CSRF:     sess.csrf,
		Nav:      nav,
		Error:    r.URL.Query().Get("err"),
		OK:       r.URL.Query().Get("ok") == "1",
	}
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
	s.render(w, "login.html", http.StatusOK, s.loginPage(""))
}

// loginPage seeds the sign-in form, telling it whether the OSS location is
// fixed by this build or still has to be asked for.
func (s *Server) loginPage(errMsg string) pageData {
	return pageData{
		Bucket:   s.opts.Bucket,
		Endpoint: s.opts.Endpoint,
		Fixed:    s.opts.Bucket != "" && s.opts.Endpoint != "",
		Error:    errMsg,
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.limiter.allow(s.clientKey(r)) {
		s.render(w, "login.html", http.StatusTooManyRequests, s.loginPage(errRateLimited.Error()))
		return
	}
	if err := r.ParseForm(); err != nil {
		s.render(w, "login.html", http.StatusBadRequest, s.loginPage("could not read the form"))
		return
	}
	cfg := config.Config{
		Endpoint:        strings.TrimSpace(r.PostFormValue("endpoint")),
		Bucket:          strings.TrimSpace(r.PostFormValue("bucket")),
		AccessKeyID:     strings.TrimSpace(r.PostFormValue("accessKeyId")),
		AccessKeySecret: strings.TrimSpace(r.PostFormValue("accessKeySecret")),
	}
	// A baked-in location wins over anything posted. The form does not even
	// show these fields then, so a value arriving in them was not typed by an
	// operator, and honouring it would let the console be aimed elsewhere.
	if s.opts.Bucket != "" {
		cfg.Bucket = s.opts.Bucket
	}
	if s.opts.Endpoint != "" {
		cfg.Endpoint = s.opts.Endpoint
	}
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKeyID == "" || cfg.AccessKeySecret == "" {
		s.render(w, "login.html", http.StatusBadRequest, s.loginPage("fill in every field"))
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
		s.render(w, "login.html", http.StatusUnauthorized, s.loginPage("could not reach the bucket with those credentials"))
		return
	}
	mgr := &admincore.Manager{Store: st}

	id, err := s.sessions.create(mgr, cfg.Bucket, cfg.Endpoint)
	if err != nil {
		s.render(w, "login.html", http.StatusServiceUnavailable, s.loginPage(err.Error()))
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
	data := newPage(sess, r, "machines")
	if us, err := sess.mgr.LoadUsers(); err == nil {
		data.Users = us.Users // the bind form offers the roster
	}
	// Same freshness window the CLI uses (cmd/admin/status_cmd.go), so the two
	// front ends never disagree about whether a machine is alive.
	machines, err := sess.mgr.CollectMachines(freshAfter)
	if err != nil {
		data.Error = "could not list machines"
		log.Printf("adminweb: CollectMachines: %v", err)
	} else {
		data.Machines = machines
		data.Fleet = summariseFleet(machines)
	}
	s.render(w, "machines.html", http.StatusOK, data)
}

func (s *Server) handlePolicy(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "policy")
	p, err := sess.mgr.CurrentPolicy()
	if err != nil {
		data.Error = "could not read the policy"
		log.Printf("adminweb: CurrentPolicy: %v", err)
	} else {
		data.Policy = &p
	}
	s.render(w, "policy.html", http.StatusOK, data)
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "users")
	us, err := sess.mgr.LoadUsers()
	if err != nil {
		data.Error = "could not read the roster"
		log.Printf("adminweb: LoadUsers: %v", err)
	} else {
		data.Users = us.Users
	}
	s.render(w, "users.html", http.StatusOK, data)
}

func (s *Server) handleSites(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "sites")
	p, err := sess.mgr.CurrentPolicy()
	if err != nil {
		data.Error = "could not read the policy"
		log.Printf("adminweb: CurrentPolicy: %v", err)
	} else {
		data.Policy = &p
	}
	s.render(w, "sites.html", http.StatusOK, data)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "settings")
	p, err := sess.mgr.CurrentPolicy()
	if err != nil {
		data.Error = "could not read the policy"
		log.Printf("adminweb: CurrentPolicy: %v", err)
	} else {
		data.Policy = &p
	}
	// Collection stats are informational; a failure here should not hide the
	// settings themselves.
	if stats, _, err := sess.mgr.CollectStats(); err == nil {
		data.Stats = stats
	}
	s.render(w, "settings.html", http.StatusOK, data)
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "machines")
	machine := strings.TrimSpace(r.URL.Query().Get("machine"))
	data.Machine = machine
	// The machine's own errors and warnings, so the page explains the state
	// rather than leaving the reader to find it in the log.
	if machine != "" {
		if all, err := sess.mgr.CollectMachines(freshAfter); err == nil {
			for i := range all {
				if strings.EqualFold(all[i].Machine, machine) {
					data.Notes = &all[i]
					break
				}
			}
		}
	}
	if machine == "" {
		data.Error = "no machine given"
	} else if b, err := sess.mgr.FetchLog(machine); err != nil {
		data.Error = "no log uploaded for this machine yet"
	} else {
		data.Log = string(b)
	}
	s.render(w, "log.html", http.StatusOK, data)
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "files")
	files, err := sess.mgr.ListFiles()
	if err != nil {
		data.Error = "could not list staged files"
		log.Printf("adminweb: ListFiles: %v", err)
	} else {
		data.Files = files
	}
	// A link is minted on demand rather than listed for every file: each one is
	// a URL that downloads the object without any credential, so they should be
	// created when wanted and left to expire.
	if name := strings.TrimSpace(r.URL.Query().Get("link")); name != "" {
		if url, err := sess.mgr.LinkFile(name, 24*time.Hour); err != nil {
			data.Error = "could not create a link for " + name
		} else {
			data.Link, data.LinkName = url, name
		}
	}
	s.render(w, "files.html", http.StatusOK, data)
}

// handleRollout is the page for the two actions that reach every machine.
func (s *Server) handleRollout(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "rollout")
	data.Job = s.jobs.snapshot()
	p, err := sess.mgr.CurrentPolicy()
	if err != nil {
		data.Error = "could not read the policy"
		log.Printf("adminweb: CurrentPolicy: %v", err)
	} else {
		data.Policy = &p
	}
	if machines, err := sess.mgr.CollectMachines(freshAfter); err == nil {
		data.Machines = machines // so the operator can see what is actually installed
		data.Fleet = summariseFleet(machines)
	}
	s.render(w, "rollout.html", http.StatusOK, data)
}

func (s *Server) handleEmployeeLogin(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "employee-login")
	if us, err := sess.mgr.LoadUsers(); err == nil {
		data.Users = us.Users
	}
	s.pendingMu.Lock()
	if p := s.pending[sess.csrf]; p != nil && time.Since(p.started) <= employeeLoginTTL {
		data.PendingUser, data.PendingTool, data.AuthURL = p.user, p.tool, p.authURL
	}
	s.pendingMu.Unlock()
	s.render(w, "employeelogin.html", http.StatusOK, data)
}
