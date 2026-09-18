package adminweb

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/config"
	"github.com/TEENet-io/ai-env-mgr/internal/ecdclient"
	"github.com/TEENet-io/ai-env-mgr/internal/litellm"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/slsclient"
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
	// Notice replaces the standard "已保存" for an action whose outcome needs
	// explaining -- see requirePostNotice.
	Notice string
	Nav    string // which nav entry to mark active

	Machines []admincore.MachineState
	Fleet    *fleetSummary
	Users    []model.UserEntry // the roster, for the bind forms on other pages
	Policy   *model.Policy

	// The overview draws three independent sections, so each reports its own
	// failure. A single Error field would let one dead section hide the two
	// that still have something to say.
	MachinesError string
	PolicyError   string
	Stats         []admincore.CollectStat
	Machine       string // the machine a log belongs to
	Log           string
	Notes         *admincore.MachineState // that machine's own errors and warnings
	Job           *job                    // a publish in flight, or the last one

	// Account pages.
	Accounts      []accountRow
	Account       *accountRow            // the detail page's subject
	Audit         []admincore.AuditEntry // that account's history, newest first
	QuotaDefaults litellm.Quota          // pre-fills the onboarding form

	// SLS says the deployment has a log project, which is what decides
	// whether the nav offers the log page. Set centrally in render.
	SLS bool
	// Logs is the log page's own data; nil on every other page.
	Logs *logsPage

	// Gateway panel: what the gateway offers.
	GatewayURL     string
	GatewayEnabled bool
	GatewayModels  []litellm.Model
	// GatewayUnusable is why the panel cannot act. Only rendered when
	// GatewayURL is set: with no gateway configured at all the panel says so
	// in its own words rather than reporting a missing address as a fault.
	GatewayUnusable string
	// Probe is the last hour of gateway probes, shown on the overview when
	// the deployment has both a gateway and a log project. nil means "not
	// asked" -- which is not the same as "asked and found nothing", so the
	// two fields below carry why an answer is missing or partial.
	Probe           *probeHealth
	ProbeNotice     string // the SLS query failed, and this is what to do
	ProbeIncomplete bool   // the index was still catching up, so counts are low
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
		Notice:   r.URL.Query().Get("msg"),
	}
}

func (s *Server) render(w http.ResponseWriter, name string, code int, data pageData) {
	// Set here rather than in newPage: the nav is drawn by every page, so the
	// flag that decides one of its entries should not be something a new
	// handler can forget.
	data.SLS = s.opts.SLSProject != ""
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
		http.Redirect(w, r, "/overview", http.StatusSeeOther)
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
	// One writer for the whole console: each session's Manager logs its audit
	// copy through it, and it is safe to share.
	mgr := &admincore.Manager{Store: st, Events: s.events}

	// The same credentials serve the cloud desktop lookup. A key without ECD
	// permission simply makes that lookup fail and be logged; everything else
	// works, so sign-in is never blocked on it.
	var cloud *cloudLookup
	if s.opts.ECDRegion != "" {
		if client, cerr := ecdclient.New(cfg.AccessKeyID, cfg.AccessKeySecret, s.opts.ECDRegion); cerr == nil {
			cloud = newCloudLookup(client)
		}
	}

	// The same AccessKey reads the unified log. Built here so it never has to
	// be stored anywhere: it lives in the session and dies with it. A key
	// without log permission simply makes the log page say so.
	var sls *slsclient.Client
	if s.opts.SLSProject != "" {
		sls = slsclient.New(s.opts.SLSEndpoint, s.opts.SLSProject, cfg.AccessKeyID, cfg.AccessKeySecret)
	}

	id, err := s.sessions.create(mgr, cfg.Bucket, cfg.Endpoint, cloud, sls)
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
	http.Redirect(w, r, "/overview", http.StatusSeeOther)
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

// overviewBudget bounds the two remote calls on this page that accept a
// context: the gateway's model list and the SLS probe window.
//
// It does NOT bound the rest. LoadUsers, CurrentPolicy and CollectMachines
// take no context, and neither does cloud.annotate -- which is the slowest
// thing here when it goes wrong, because it holds the lookup's mutex across
// a DescribeDesktops call, so a slow platform stalls every concurrent render
// of this page rather than just one. Giving those a deadline means changing
// signatures down in admincore, which is a larger change than this page
// deserves; until then the honest statement is that this budget covers the
// gateway and the probe, and the page can still outlast it.
const overviewBudget = 10 * time.Second

// handleOverview is the console's front page: the fleet, the policy every
// machine is reading, and what the gateway is offering, in that order.
//
// The three used to be three pages, which meant three sign-in-to-answer round
// trips to learn whether anything was wrong. They are read independently and
// they fail independently: a dead gateway costs its own panel and nothing
// else.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "overview")
	// A publish started from /rollout keeps running while the operator wanders
	// back here, and the machine table below is exactly what it is changing.
	// Without this the page's refresh directive has nothing to trigger on.
	data.Job = s.jobs.snapshot()

	ctx, cancel := context.WithTimeout(r.Context(), overviewBudget)
	defer cancel()

	if us, err := sess.mgr.LoadUsers(); err == nil {
		data.Users = us.Users // the bind form offers the roster
	}

	// The policy is read once and used twice: for its own panel, and for the
	// Codex column, which compares each machine against the published target.
	if p, err := sess.mgr.CurrentPolicy(); err != nil {
		data.PolicyError = "could not read the policy"
		log.Printf("adminweb: CurrentPolicy: %v", err)
	} else {
		data.Policy = &p
	}

	// Same freshness window the CLI uses (cmd/admin/status_cmd.go), so the two
	// front ends never disagree about whether a machine is alive.
	if machines, err := sess.mgr.CollectMachines(freshAfter); err != nil {
		data.MachinesError = "could not list machines"
		log.Printf("adminweb: CollectMachines: %v", err)
	} else {
		// Only reaches the platform for machines whose own report is
		// ambiguous; a fleet that is reporting normally makes no calls.
		sess.cloud.annotate(machines)
		data.Machines = machines
		data.Fleet = summariseFleet(machines)
	}

	s.loadGatewayPanel(ctx, &data)
	// The probe bar is the log page's, reused rather than reimplemented: two
	// renderings of "is the gateway up" would eventually disagree.
	//
	// Skipped without a gateway to probe: the panel does not draw the bar
	// then, so the queries would be two SLS round trips spent on nothing.
	if s.opts.GatewayURL != "" && s.opts.SLSProject != "" && sess.sls != nil {
		scratch := &logsPage{}
		s.loadProbe(ctx, sess, scratch)
		// Why the bar is missing or wrong matters as much as the bar: an SLS
		// query that failed and an hour with no probes both leave OKs and
		// Fails at zero, and only one of them means nobody is watching.
		data.Probe = scratch.Probe
		data.ProbeNotice = scratch.Notice
		data.ProbeIncomplete = scratch.Incomplete
	}

	s.render(w, "overview.html", http.StatusOK, data)
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
	// A failed read is shown, not papered over: the form would otherwise
	// prefill with zeros and look like somebody had configured them.
	if q, err := sess.mgr.LoadQuotaDefaults(); err == nil {
		data.QuotaDefaults = q
	} else {
		data.QuotaDefaults = admincore.DefaultQuota
		data.Error = "读取开户默认额度失败，下面显示的是内置默认值：" + err.Error()
		log.Printf("adminweb: LoadQuotaDefaults: %v", err)
	}
	s.render(w, "settings.html", http.StatusOK, data)
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "overview")
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
		sess.cloud.annotate(machines)
		data.Machines = machines // so the operator can see what is actually installed
		data.Fleet = summariseFleet(machines)
	}
	s.render(w, "rollout.html", http.StatusOK, data)
}
