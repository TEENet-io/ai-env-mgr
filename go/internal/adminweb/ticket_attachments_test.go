package adminweb

import (
	"bytes"
	"crypto/sha256"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/authn"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

var (
	testPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	testPDF = []byte("%PDF-1.4\n%test")
)

type testFile struct {
	name string
	data []byte
}

func createAdminTicket(t *testing.T, h http.Handler, cookie *http.Cookie, csrf string, fields map[string]string, files ...testFile) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("csrf", csrf)
	key, err := randomTicketKey()
	if err != nil {
		t.Fatal(err)
	}
	w.WriteField("request_key", key)
	for k, v := range fields {
		w.WriteField(k, v)
	}
	for _, f := range files {
		part, _ := w.CreateFormFile("files", f.name)
		part.Write(f.data)
	}
	w.Close()
	req := httptest.NewRequest(http.MethodPost, "/tickets/create", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestTicketFilesFollowTheDesign(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	csrf := csrfFrom(t, s, cookie, "/settings")
	problem := func(kind string) map[string]string {
		fields := map[string]string{"request_kind": kind, "problem": "连不上"}
		if kind == "tool-problem" {
			fields["tool"] = "不确定"
		}
		return fields
	}

	// An image on every problem kind, and it comes back as a download.
	for _, kind := range []string{"desktop-problem", "ai-problem", "tool-problem", "other-problem"} {
		if rec := createAdminTicket(t, h, cookie, csrf, problem(kind), testFile{"截图.png", testPNG}); rec.Code != http.StatusSeeOther {
			t.Fatalf("%s with an image: %d %s", kind, rec.Code, rec.Body)
		}
	}
	// PDF only on "other".
	if rec := createAdminTicket(t, h, cookie, csrf, problem("ai-problem"), testFile{"a.pdf", testPDF}); rec.Code != http.StatusBadRequest {
		t.Fatalf("PDF on a screenshot-only kind: %d", rec.Code)
	}
	rec := createAdminTicket(t, h, cookie, csrf, problem("other-problem"), testFile{`..\..\report.pdf`, testPDF})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("PDF on other: %d %s", rec.Code, rec.Body)
	}
	detail := rec.Header().Get("Location")
	id, _ := url.ParseQuery(strings.SplitN(detail, "?", 2)[1])

	// The type comes from the content, not the name.
	if rec := createAdminTicket(t, h, cookie, csrf, problem("other-problem"), testFile{"x.png", []byte("<html><script>alert(1)</script>")}); rec.Code != http.StatusBadRequest {
		t.Fatalf("HTML named .png: %d", rec.Code)
	}
	if rec := createAdminTicket(t, h, cookie, csrf, problem("other-problem"), testFile{"big.png", append(append([]byte{}, testPNG...), make([]byte, maxTicketFileSize)...)}); rec.Code != http.StatusBadRequest {
		t.Fatalf("over 5 MB: %d", rec.Code)
	}
	six := []testFile{{"1.png", testPNG}, {"2.png", testPNG}, {"3.png", testPNG}, {"4.png", testPNG}, {"5.png", testPNG}, {"6.png", testPNG}}
	if rec := createAdminTicket(t, h, cookie, csrf, problem("other-problem"), six...); rec.Code != http.StatusBadRequest {
		t.Fatalf("six files: %d", rec.Code)
	}
	// A change request takes no files.
	if rec := createAdminTicket(t, h, cookie, csrf, map[string]string{"request_kind": "quota"}, testFile{"a.png", testPNG}); rec.Code != http.StatusBadRequest {
		t.Fatalf("a file on a change request: %d", rec.Code)
	}

	page := dbGet(t, h, detail, cookie).Body.String()
	if !strings.Contains(page, "report.pdf") || !strings.Contains(page, "/tickets/file?id=") {
		t.Fatalf("the detail page lists no file: %s", firstLine(page, "文件"))
	}
	var fileID string
	if i := strings.Index(page, "file="); i >= 0 {
		fileID = page[i+5 : i+5+32]
	}
	got := dbGet(t, h, "/tickets/file?id="+url.QueryEscape(id.Get("id"))+"&file="+fileID, cookie)
	if got.Code != 200 || !bytes.Equal(got.Body.Bytes(), testPDF) {
		t.Fatalf("download: %d", got.Code)
	}
	if ct, cd := got.Header().Get("Content-Type"), got.Header().Get("Content-Disposition"); ct != "application/pdf" || !strings.HasPrefix(cd, "attachment;") || strings.Contains(cd, "..") {
		t.Fatalf("download headers: %q %q", ct, cd)
	}
	for _, query := range []string{"&file=", "&file=" + strings.Repeat("0", 32), "&file=../../x"} {
		if rec := dbGet(t, h, "/tickets/file?id="+url.QueryEscape(id.Get("id"))+query, cookie); rec.Code != http.StatusNotFound {
			t.Fatalf("%q: %d", query, rec.Code)
		}
	}
	// Nobody signed in gets it.
	req := httptest.NewRequest(http.MethodGet, "/tickets/file?id="+url.QueryEscape(id.Get("id"))+"&file="+fileID, nil)
	anon := httptest.NewRecorder()
	h.ServeHTTP(anon, req)
	if anon.Code == 200 {
		t.Fatal("a file was served without a session")
	}
}

func TestToolReportsChooseFromTheList(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	cookie := signedIn(t, s)
	csrf := csrfFrom(t, s, cookie, "/settings")
	report := func(tool string) int {
		return createAdminTicket(t, h, cookie, csrf, map[string]string{"request_kind": "tool-problem", "tool": tool, "problem": "打不开"}).Code
	}
	if code := report("新闻监控"); code != http.StatusBadRequest {
		t.Fatalf("a tool off the list: %d", code)
	}
	if code := report("不确定"); code != http.StatusSeeOther {
		t.Fatalf("unsure: %d", code)
	}
	if rec := dbPost(t, h, "/settings/ticket-tools", url.Values{"csrf": {csrf}, "version": {"0"}, "names": {"新闻监控\n\n新闻监控\n 销售报表助手 \n不确定"}}, cookie); strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Fatal(rec.Header().Get("Location"))
	}
	got, _, _ := repo.LoadTicketToolSettings(t.Context(), s.dbm.store.Settings())
	if len(got.Names) != 2 || got.Names[1] != "销售报表助手" {
		t.Fatalf("stored %v", got.Names)
	}
	if code := report("新闻监控"); code != http.StatusSeeOther {
		t.Fatalf("a listed tool: %d", code)
	}
	form := dbGet(t, h, "/tickets/new", cookie).Body.String()
	if !strings.Contains(form, `<option value="销售报表助手">`) || !strings.Contains(form, `<option value="不确定">`) {
		t.Fatal("the form does not offer the list")
	}
}

func TestEveryEmployeeCanBeToldApart(t *testing.T) {
	cases := []struct {
		e           repo.Employee
		name, label string
	}{
		{repo.Employee{Name: "张三", Email: "z@example.com", WindowsUser: "z", Status: repo.StatusActive}, "张三", "张三 · z@example.com"},
		{repo.Employee{Email: "z@example.com", WindowsUser: "z", Status: repo.StatusActive}, "z@example.com", "z@example.com"},
		{repo.Employee{WindowsUser: "z", Status: repo.StatusActive}, "z", "z"},
		{repo.Employee{WindowsUser: "z", Status: repo.StatusOffboarded}, "z", "z（已停用）"},
	}
	for _, c := range cases {
		if got := employeeName(c.e); got != c.name {
			t.Errorf("name %q, want %q", got, c.name)
		}
		if got := employeeLabel(c.e); got != c.label {
			t.Errorf("label %q, want %q", got, c.label)
		}
	}
}

func TestProblemReportsTakeNoHandlingTime(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(url.Values{
		"request_kind": {"desktop-problem"}, "request_key": {strings.Repeat("a", 32)}, "problem": {"连不上"},
		"occurred_at": {"今天上午"}, "desired_at": {"2026-11-05 14:30"},
	}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.ParseForm()
	_, _, description, _, _, details, err := parseDailyTicketSubmission(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(details), "desired_at") || !strings.Contains(description, "大约发生时间：今天上午") {
		t.Fatalf("%s / %s", details, description)
	}
}

func TestChangeRequestTimesAreTypedAndStoredOneWay(t *testing.T) {
	for in, want := range map[string]string{"": "", "2026-11-05 14:30": "2026-11-05T14:30", "2026-11-05T14:30": "2026-11-05T14:30"} {
		if got, ok := dailyDateTime(in); !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	for _, bad := range []string{"05/11/2026 14:30", "2026-13-05 14:30", "明天"} {
		if _, ok := dailyDateTime(bad); ok {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func customerCookieFor(t *testing.T, s *Server, email, role string) *http.Cookie {
	t.Helper()
	ctx := t.Context()
	hash, err := authn.HashPassword("a-long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	contact, err := s.dbm.db.Tickets().CreateContactWithRole(ctx, email, email, hash, role)
	if err != nil {
		t.Fatal(err)
	}
	token := "customer-session-" + email
	sum := sha256.Sum256([]byte(token))
	if err := s.dbm.db.Tickets().CreateSession(ctx, contact.ID, sum[:], time.Hour, time.Hour); err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: customerCookie, Value: token}
}

func postTicketForm(t *testing.T, h http.Handler, path string, cookie *http.Cookie, fields map[string]string, files ...testFile) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		w.WriteField(k, v)
	}
	for _, f := range files {
		part, _ := w.CreateFormFile("files", f.name)
		part.Write(f.data)
	}
	w.Close()
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestATicketsFilesFollowWhoMaySeeIt(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	owner := customerCookieFor(t, s, "owner@example.com", "employee")
	other := customerCookieFor(t, s, "other@example.com", "employee")
	manager := customerCookieFor(t, s, "manager@example.com", "manager")
	key, err := randomTicketKey()
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]string{"csrf": s.csrfFor(owner.Value), "request_key": key, "request_kind": "desktop-problem", "problem": "连不上"}
	rec := postTicketForm(t, h, "/customer/ticket/create", owner, fields, testFile{"first.png", testPNG})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	id := strings.SplitN(rec.Header().Get("Location"), "id=", 2)[1]
	page := dbGet(t, h, "/customer/ticket?id="+id, owner).Body.String()
	i := strings.Index(page, "file=")
	if i < 0 {
		t.Fatal("the customer's ticket page lists no file")
	}
	file := "/customer/ticket/file?id=" + id + "&file=" + page[i+5:i+5+32]
	if rec := dbGet(t, h, file, owner); rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), testPNG) {
		t.Fatalf("the owner's download: %d", rec.Code)
	}
	if rec := dbGet(t, h, file, other); rec.Code != http.StatusNotFound {
		t.Fatalf("another employee's download: %d", rec.Code)
	}
	// A manager contact sees the company's tickets, and so their files.
	if rec := dbGet(t, h, file, manager); rec.Code != 200 {
		t.Fatalf("a manager's download: %d", rec.Code)
	}

	// Editing keeps the files already there and adds to them, up to the limit.
	edit := map[string]string{"csrf": s.csrfFor(owner.Value), "id": strings.ReplaceAll(id, "%2D", "-"), "version": "1", "request_kind": "desktop-problem", "request_key": key, "problem": "还是连不上"}
	rec = postTicketForm(t, h, "/customer/ticket/edit", owner, edit, testFile{"second.png", testPNG})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body)
	}
	page = dbGet(t, h, "/customer/ticket?id="+id, owner).Body.String()
	if !strings.Contains(page, "first.png") || !strings.Contains(page, "second.png") {
		t.Fatal("an edit lost a file")
	}
	edit["version"] = "2"
	rec = postTicketForm(t, h, "/customer/ticket/edit", owner, edit, testFile{"3.png", testPNG}, testFile{"4.png", testPNG}, testFile{"5.png", testPNG}, testFile{"6.png", testPNG})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("six files across edits: %d", rec.Code)
	}
}
