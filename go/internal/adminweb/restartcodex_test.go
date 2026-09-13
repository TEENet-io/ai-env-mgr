package adminweb

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/admincore"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// bindInStore seeds a binding the way BindMachine would, without a roster.
func bindInStore(t *testing.T, fs *fakeStore, machine, user string) {
	t.Helper()
	out, err := json.Marshal(model.Binding{User: user, BoundAt: "2026-09-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	fs.objects[ossclient.BindingKey(machine)] = out
}

func TestRestartCodexPostWritesTheRequestAndSaysWhenItWillRun(t *testing.T) {
	fs := newFakeStore() // its policy sets a 15-minute interval
	bindInStore(t, fs, "pc1", "work1")
	s := newTestServer(t, fs)
	cookie := signIn(t, s)

	rec := post(t, s, "/machines/restart-codex", cookie, url.Values{
		"csrf":    {csrfOf(t, s, cookie)},
		"machine": {"pc1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST returned %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/overview") {
		t.Errorf("redirected to %q, want /overview", loc)
	}

	// Nothing happens on the machine until its next sync, and the notice is
	// the only thing that tells the administrator so.
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	msg := u.Query().Get("msg")
	if !strings.Contains(msg, "下个同步周期") || !strings.Contains(msg, "15") {
		t.Errorf("notice = %q, want it to name the wait and the interval", msg)
	}

	var b model.Binding
	if err := json.Unmarshal(fs.objects[ossclient.BindingKey("pc1")], &b); err != nil {
		t.Fatal(err)
	}
	if b.RestartCodex == "" {
		t.Error("no nonce reached the binding, so no agent would act")
	}
	if b.User != "work1" {
		t.Errorf("the binding lost its user: %+v", b)
	}
}

// There is nobody to restart, and the machine may not even exist. Saying so
// beats writing a binding that would assign the box to whoever came along.
func TestRestartCodexOnAnUnboundMachineReportsAnError(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	cookie := signIn(t, s)

	rec := post(t, s, "/machines/restart-codex", cookie, url.Values{
		"csrf":    {csrfOf(t, s, cookie)},
		"machine": {"nobodys-pc"},
	})
	loc := rec.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/overview" {
		t.Errorf("redirected to %q, want /overview", loc)
	}
	if !strings.Contains(u.Query().Get("err"), "not bound") {
		t.Errorf("the operator was not told why nothing happened: %q", loc)
	}
	if u.Query().Get("ok") == "1" {
		t.Error("a failed request must not report success")
	}
}

// The action changes what a machine does to somebody's running session, so it
// is gated like every other one: no session, no token, nothing happens.
func TestRestartCodexRefusesWithoutSessionOrCSRF(t *testing.T) {
	fs := newFakeStore()
	bindInStore(t, fs, "pc1", "work1")
	s := newTestServer(t, fs)

	// No session at all.
	rec := post(t, s, "/machines/restart-codex", nil, url.Values{"machine": {"pc1"}})
	if rec.Header().Get("Location") != "/" {
		t.Errorf("an anonymous POST was not sent to sign in: %q", rec.Header().Get("Location"))
	}

	// Signed in, but with a token from nowhere.
	cookie := signIn(t, s)
	rec = post(t, s, "/machines/restart-codex", cookie, url.Values{
		"csrf": {"not-the-token"}, "machine": {"pc1"},
	})
	if !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Error("a bad CSRF token was accepted")
	}
	var b model.Binding
	_ = json.Unmarshal(fs.objects[ossclient.BindingKey("pc1")], &b)
	if b.RestartCodex != "" {
		t.Error("a rejected POST still wrote a request")
	}
}

// A request that has been issued has two states, and they mean opposite
// things to whoever is waiting: still on its way, or carried out. A row that
// showed neither would leave the button with no feedback at all.
func TestOverviewShowsBothRestartStates(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	fresh := time.Now().UTC().Format(time.RFC3339)

	machines := []admincore.MachineState{
		// Asked, not yet acted on: the agent's status carries no nonce.
		{Machine: "pc-pending", Bound: true,
			Binding: model.Binding{User: "work1", RestartCodex: "nonce-new"},
			Status:  model.Status{Machine: "pc-pending", LastSync: fresh, AgentVersion: "1.2.7"}},
		// Asked and done: the agent reports the same nonce back.
		{Machine: "pc-done", Bound: true,
			Binding: model.Binding{User: "work2", RestartCodex: "nonce-old"},
			Status: model.Status{Machine: "pc-done", LastSync: fresh, AgentVersion: "1.2.7",
				CodexRestartNonce: "nonce-old", CodexRestartAt: fresh,
				CodexRestartNote: "killed 1 process"}},
		// Never asked: no line at all.
		{Machine: "pc-quiet", Bound: true,
			Binding: model.Binding{User: "work3"},
			Status:  model.Status{Machine: "pc-quiet", LastSync: fresh, AgentVersion: "1.2.7"}},
	}
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, "overview.html", pageData{
		CSRF: "t", Nav: "overview", Machines: machines, Fleet: summariseFleet(machines),
	}); err != nil {
		t.Fatalf("overview.html: %v", err)
	}
	body := buf.String()

	if !strings.Contains(body, `action="/machines/restart-codex"`) {
		t.Error("the overview offers no way to ask for a restart")
	}
	if !strings.Contains(body, "待执行") {
		t.Error("a pending request is not shown as pending")
	}
	if !strings.Contains(body, "已关闭") || !strings.Contains(body, "killed 1 process") {
		t.Error("a completed request does not report what it did")
	}
	// Three bound machines, three buttons, but only two rows have a state.
	if n := strings.Count(body, "待执行"); n != 1 {
		t.Errorf("待执行 appears %d times, want 1 -- only one machine is waiting", n)
	}
	if n := strings.Count(body, "已关闭"); n != 1 {
		t.Errorf("已关闭 appears %d times, want 1", n)
	}
}

// An unbound machine has no employee, so there is no session to end and the
// button would only produce an error.
func TestOverviewOffersNoRestartForAnUnboundMachine(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	machines := []admincore.MachineState{
		{Machine: "pc-unbound", Unbound: true,
			Status: model.Status{Machine: "pc-unbound", LastSync: time.Now().UTC().Format(time.RFC3339)}},
	}
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, "overview.html", pageData{
		CSRF: "t", Nav: "overview", Machines: machines, Fleet: summariseFleet(machines),
	}); err != nil {
		t.Fatalf("overview.html: %v", err)
	}
	if strings.Contains(buf.String(), "/machines/restart-codex") {
		t.Error("an unbound machine offers a restart button that could only fail")
	}
}

// The administrator who has just reissued somebody's token is on their page,
// not on the machine list, and that is exactly the moment the employee's
// Codex needs ending.
func TestUserPageOffersARestartForEachBoundMachine(t *testing.T) {
	s := newTestServer(t, newFakeStore())

	withMachine := accountRow{WindowsUser: "work1", Enabled: true, OnRoster: true,
		Machines: []string{"pc1"}}
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, "user.html", pageData{
		CSRF: "t", Nav: "users", Account: &withMachine,
	}); err != nil {
		t.Fatalf("user.html: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, `action="/machines/restart-codex"`) {
		t.Error("the employee page offers no restart button")
	}
	if !strings.Contains(body, `name="machine" value="pc1"`) {
		t.Error("the form does not name the machine to act on")
	}

	// Nobody bound: nothing to press.
	noMachine := accountRow{WindowsUser: "work2", Enabled: true, OnRoster: true}
	buf.Reset()
	if err := s.tpl.ExecuteTemplate(&buf, "user.html", pageData{
		CSRF: "t", Nav: "users", Account: &noMachine,
	}); err != nil {
		t.Fatalf("user.html: %v", err)
	}
	if strings.Contains(buf.String(), "/machines/restart-codex") {
		t.Error("an employee with no machine is offered a restart anyway")
	}
}

// "已保存" is a lie for a request that has not reached the machine yet. The
// notice replaces it, and the ordinary actions keep the standard one.
func TestNoticeReplacesTheDefaultSavedLine(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, "overview.html", pageData{
		CSRF: "t", Nav: "overview", OK: true, Notice: "已下发，Agent 下个同步周期执行（当前间隔 15 分钟）",
	}); err != nil {
		t.Fatalf("overview.html: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, "已下发") {
		t.Error("the notice was not rendered")
	}
	if strings.Contains(body, "已保存") {
		t.Error("the default line is shown alongside the notice, saying two different things")
	}

	buf.Reset()
	if err := s.tpl.ExecuteTemplate(&buf, "overview.html", pageData{
		CSRF: "t", Nav: "overview", OK: true,
	}); err != nil {
		t.Fatalf("overview.html: %v", err)
	}
	if !strings.Contains(buf.String(), "已保存") {
		t.Error("an ordinary action lost its confirmation")
	}
}
