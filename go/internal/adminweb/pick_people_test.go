package adminweb

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// With the custom tool's list, an AI workspace account is opened for
// somebody picked from it (AIC-209 #2, AIC-204 #8).

const (
	amyID  = "00000000-0000-4000-8000-000000000002"
	bobID  = "00000000-0000-4000-8000-000000000003"
	opsID  = "00000000-0000-4000-8000-000000000004"
	goneID = "00000000-0000-4000-8000-000000000005"
)

func newDirectoryServer(t *testing.T) *Server {
	t.Helper()
	s, _ := newDatabaseServerWith(t, func(o *Options) {
		o.Database.DirectoryURL = "https://tool.example.com/internal/directory/accounts"
		o.Database.DirectoryToken = "t"
	})
	if _, err := s.dbm.db.Pool().Exec(context.Background(), `insert into directory_accounts (id, username, name, email, role, status) values
		($1, 'amy', 'Amy Chan', 'amy@bippo.example', 'employee', 'active'),
		($2, 'bob', '', 'bob@bippo.example', 'employee', 'active'),
		($3, 'wei', 'Wei', 'wei@teenet.example', 'ops', 'active'),
		($4, 'cat', 'Cat', 'cat@bippo.example', 'employee', 'disabled')`, amyID, bobID, opsID, goneID); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOnboardingPicksAPersonFromTheList(t *testing.T) {
	s := newDirectoryServer(t)
	h := s.Handler()
	session := signedIn(t, s)

	page := dbGet(t, h, "/users", session).Body.String()
	for _, want := range []string{`name="directoryID"`, "Amy Chan · amy@bippo.example", "bob · bob@bippo.example"} {
		if !strings.Contains(page, want) {
			t.Errorf("the form lacks %q", want)
		}
	}
	for _, unwanted := range []string{`name="email"`, "wei@teenet.example", "cat@bippo.example"} {
		if strings.Contains(page, unwanted) {
			t.Errorf("the form offers %q", unwanted)
		}
	}

	onboard := func(fields url.Values) *httptest.ResponseRecorder {
		t.Helper()
		form := url.Values{"csrf": {csrfFrom(t, s, session, "/users")}, "budget": {"50"}, "models": {"claude-4.5-sonnet"}}
		for k, v := range fields {
			form[k] = v
		}
		return dbPost(t, h, "/users/onboard", form, session)
	}
	// What the form says about the person is not read: the list is.
	rec := onboard(url.Values{"windowsUser": {"work2"}, "directoryID": {amyID}, "name": {"Someone"}, "email": {"x@evil.example"}})
	if !strings.Contains(rec.Header().Get("Location"), "ok=1") {
		t.Fatalf("onboard = %q", rec.Header().Get("Location"))
	}
	e, err := s.dbm.store.Employees().ByWindowsUser(context.Background(), "work2")
	if err != nil || e.DirectoryID != amyID || e.Name != "Amy Chan" || e.Email != "amy@bippo.example" {
		t.Fatalf("employee = %+v %v", e, err)
	}

	for name, fields := range map[string]url.Values{
		"nobody picked":     {"windowsUser": {"work3"}, "name": {"Bob"}, "email": {"bob@bippo.example"}},
		"picked twice":      {"windowsUser": {"work3"}, "directoryID": {amyID}},
		"an ops account":    {"windowsUser": {"work3"}, "directoryID": {opsID}},
		"a disabled person": {"windowsUser": {"work3"}, "directoryID": {goneID}},
		// work2 is amy's: picking bob with it must not take her account over.
		"somebody else's Windows user": {"windowsUser": {"work2"}, "directoryID": {bobID}},
	} {
		if rec := onboard(fields); !strings.Contains(rec.Header().Get("Location"), "err=") {
			t.Errorf("%s: accepted (%q)", name, rec.Header().Get("Location"))
		}
	}
	if _, err := s.dbm.store.Employees().ByWindowsUser(context.Background(), "work3"); err == nil {
		t.Error("a refused onboarding opened an account")
	}
	if e, _ := s.dbm.store.Employees().ByWindowsUser(context.Background(), "work2"); e.DirectoryID != amyID || e.Name != "Amy Chan" {
		t.Errorf("amy's account was taken over: %+v", e)
	}

	page = dbGet(t, h, "/users", session).Body.String()
	if strings.Contains(page, "Amy Chan · amy@bippo.example") {
		t.Error("amy is still offered after getting an account")
	}
	if strings.Contains(page, "未關聯定製工具") {
		t.Error("a linked account is shown as not linked")
	}
	if _, err := s.dbm.store.Employees().Create(context.Background(), repo.NewEmployee{WindowsUser: "work9", Name: "Old"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dbGet(t, h, "/users", session).Body.String(), "未關聯定製工具") {
		t.Error("an account opened before the list is not marked")
	}
}

func TestAnAccountRequestPicksPeopleFromTheList(t *testing.T) {
	s := newDirectoryServer(t)
	h := s.Handler()
	manager := customerCookieFor(t, s, "lead@bippo.example", "manager")

	page := dbGet(t, h, "/customer?view=new", manager).Body.String()
	if !strings.Contains(page, `name="directory_ids" value="`+amyID+`"`) || strings.Contains(page, `name="employee_name_1"`) {
		t.Fatal("the request form does not pick from the list")
	}
	if strings.Contains(page, opsID) || strings.Contains(page, goneID) {
		t.Error("the request form offers ops or disabled accounts")
	}

	raise := func(ids ...string) *httptest.ResponseRecorder {
		t.Helper()
		key, err := randomTicketKey()
		if err != nil {
			t.Fatal(err)
		}
		return postTicketValues(t, h, "/customer/ticket/create", manager, url.Values{
			"csrf": {s.csrfFor(manager.Value)}, "request_key": {key}, "request_kind": {"account-add"},
			"directory_ids": ids, "employee_name_1": {"Typed"}, "work_email_1": {"typed@example.com"},
		})
	}
	rec := raise(amyID, bobID)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("raise: %d %s", rec.Code, rec.Body)
	}
	id := strings.SplitN(rec.Header().Get("Location"), "id=", 2)[1]
	var details []byte
	if err := s.dbm.db.Pool().QueryRow(context.Background(), `select details from tickets where id = $1`, id).Scan(&details); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Employees []map[string]string `json:"employees"`
	}
	if err := json.Unmarshal(details, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Employees) != 2 || got.Employees[0]["directory_id"] != amyID || got.Employees[0]["name"] != "Amy Chan" ||
		got.Employees[1]["name"] != "bob" || got.Employees[1]["email"] != "bob@bippo.example" {
		t.Errorf("employees = %+v", got.Employees)
	}

	for name, ids := range map[string][]string{"nobody": nil, "ops": {opsID}, "disabled": {goneID}, "unknown": {"00000000-0000-4000-8000-000000000099"}} {
		if rec := raise(ids...); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
}

func postTicketValues(t *testing.T, h http.Handler, path string, cookie *http.Cookie, fields url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, values := range fields {
		for _, v := range values {
			w.WriteField(k, v)
		}
	}
	w.Close()
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A linked account's name and email are the tool's (AIC-204 #9): a request
// cannot change them, and the detail page keeps the name the sync wrote.
func TestNamesAreChangedInTheToolNotHere(t *testing.T) {
	s := newDirectoryServer(t)
	h := s.Handler()
	ctx := context.Background()
	emp, err := s.dbm.store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work2", Name: "Amy Chan", Email: "amy@bippo.example", DirectoryID: amyID})
	if err != nil {
		t.Fatal(err)
	}
	manager := customerCookieFor(t, s, "lead@bippo.example", "manager")
	if page := dbGet(t, h, "/customer?view=new", manager).Body.String(); strings.Contains(page, `name="new_name"`) || strings.Contains(page, `name="new_email"`) {
		t.Error("the edit request still offers name and email")
	}
	edit := func(fields url.Values) int {
		t.Helper()
		key, _ := randomTicketKey()
		form := url.Values{"csrf": {s.csrfFor(manager.Value)}, "request_key": {key}, "request_kind": {"account-edit"}, "target_employee": {emp.ID}}
		for k, v := range fields {
			form[k] = v
		}
		return postTicketValues(t, h, "/customer/ticket/create", manager, form).Code
	}
	if code := edit(url.Values{"new_name": {"Amy C"}}); code != http.StatusBadRequest {
		t.Errorf("a name change was accepted: %d", code)
	}
	if code := edit(url.Values{"new_email": {"amy2@bippo.example"}}); code != http.StatusBadRequest {
		t.Errorf("an email change was accepted: %d", code)
	}
	if code := edit(url.Values{"new_number": {"E-102"}}); code != http.StatusSeeOther {
		t.Errorf("a number change was refused: %d", code)
	}

	session := signedIn(t, s)
	if page := dbGet(t, h, "/users/detail?user=work2", session).Body.String(); strings.Contains(page, `name="name"`) {
		t.Error("the detail page lets the name be typed")
	}
	rec := dbPost(t, h, "/users/profile", url.Values{"csrf": {csrfFrom(t, s, session, "/users")}, "windowsUser": {"work2"},
		"name": {"Someone"}, "department": {"研發"}, "email": {"amy@bippo.example"}, "version": {"1"}}, session)
	if !strings.Contains(rec.Header().Get("Location"), "ok=1") {
		t.Fatalf("profile = %q", rec.Header().Get("Location"))
	}
	after, _ := s.dbm.store.Employees().ByID(ctx, emp.ID)
	if after.Name != "Amy Chan" || after.Department != "研發" {
		t.Errorf("after the save: %+v", after)
	}
}
