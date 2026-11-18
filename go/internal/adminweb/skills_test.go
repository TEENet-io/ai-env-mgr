package adminweb

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/companyskills"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestSkillImportDetectsNameAndRequiresExplicitUpdateForAnExistingSkill(t *testing.T) {
	bundle, err := companyskills.FromForm("teenet-import-check", "Import check", "", "Read the supplied template", nil)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	file, _ := writer.Create("SKILL.md")
	file.Write(bundle.Files["SKILL.md"])
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, intent        string
		existing, wantError bool
	}{
		{"create detects identity", "create", false, false},
		{"create cannot overwrite", "create", true, true},
		{"update retains identity", "update", true, false},
		{"update missing skill", "update", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			form := multipart.NewWriter(&body)
			for name, value := range map[string]string{"csrf": "test-token", "version": "0", "action": "save", "title": "Import check", "intent": tc.intent} {
				form.WriteField(name, value)
			}
			if tc.intent == "update" {
				form.WriteField("name", bundle.Name)
			}
			file, _ := form.CreateFormFile("bundle", "skill.zip")
			file.Write(archive.Bytes())
			form.Close()
			req := httptest.NewRequest("POST", "/skills/change", &body)
			req.Header.Set("Content-Type", form.FormDataContentType())
			if err := parseSkillPost(httptest.NewRecorder(), req, "test-token"); err != nil {
				t.Fatal(err)
			}
			if req.MultipartForm != nil {
				defer req.MultipartForm.RemoveAll()
			}
			catalog := companyskills.Catalog{}
			if tc.existing {
				catalog.Skills = []companyskills.Skill{{Name: bundle.Name}}
			}
			change, err := skillChange(req, catalog, "admin", "test-admin")
			if (err != nil) != tc.wantError {
				t.Fatalf("change=%+v err=%v", change, err)
			}
			if err == nil && (change.Name != bundle.Name || change.Bundle.Name != bundle.Name) {
				t.Fatal("import identity was not retained")
			}
		})
	}
}

func TestCompanySkillTemplatesAndRoles(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	bundle, err := companyskills.FromForm("bippo-demo", "根據客戶資料和業務模板，整理本月待跟進的續保清單。", "每月續保資料整理時", "讀取模板後輸出結果，標出需要人工確認的信息。", map[string][]byte{"references/客戶清單.csv": []byte("客戶,續保日期\n")})
	if err != nil {
		t.Fatal(err)
	}
	old := bundle
	bundle.Version = 2
	now := time.Now()
	skill := companyskills.Skill{Name: bundle.Name, Title: "續保客戶清單", Current: 2, Employees: []string{"employee-1"}, Departments: []string{"客戶服務部"}, Revisions: []companyskills.Revision{{Bundle: old, By: "TEENet", At: now.Add(-24 * time.Hour)}, {Bundle: bundle, By: "夢寒", At: now}}}
	people := []skillOption{{Value: "employee-1", Label: "張三 · work1 · 客戶服務部", Selected: true}, {Value: "employee-2", Label: "李四 · work2 · 業務部"}, {Value: "employee-3", Label: "王五 · work3 · 業務部"}, {Value: "employee-4", Label: "趙六 · work4 · 運營部"}, {Value: "employee-5", Label: "陳晨 · work5 · 客戶服務部"}}
	description, when, steps := skillFields(bundle)
	row := skillRow{Skill: skill, Description: description, Assignment: "assigned", Audience: "已派發", Updated: now.Format("2006-01-02 15:04"), FileCount: 2, RecipientCount: 2, Employees: people, Departments: []skillOption{{Value: "客戶服務部", Label: "客戶服務部", Selected: true}, {Value: "業務部", Label: "業務部"}, {Value: "運營部", Label: "運營部"}}, History: []companyskills.Revision{skill.Revisions[1], skill.Revisions[0]}}
	second := row
	second.Name, second.Title, second.Assignment, second.Audience = "teenet-weekly-report", "部門週報整理", "trial", "試用中"
	second.TrialLabel = "李四 · work2 · 業務部"
	third := row
	third.Name, third.Title, third.Assignment, third.Audience = "teenet-invoice-review", "發票資料覈對", "draft", "未派發"
	base := skillsPage{Base: "/customer/skills", CSRF: "token", Name: "測試經理", Editable: true, Version: 3, View: "library", DetailTab: "content", Mode: "manual", Total: 3, ActiveCount: 2, DraftCount: 1, Rows: []skillRow{row, second, third}, Employees: people, Description: description, When: when, Steps: steps, Content: string(bundle.Files["SKILL.md"]), Attachments: []string{"references/客戶清單.csv"}}
	if allowed(repo.RoleViewer, "POST", "/skills/change") || !allowed(repo.RoleOperator, "POST", "/skills/change") {
		t.Fatal("skill role boundary")
	}
	variants := []struct {
		name    string
		prepare func(*skillsPage)
		want    string
	}{
		{"skills", func(p *skillsPage) {}, "?new=import"},
		{"skill-content", func(p *skillsPage) { p.Selected = &row }, "查看 SKILL.md 完整內容"},
		{"skill-audience", func(p *skillsPage) { p.Selected = &row; p.DetailTab = "audience" }, "name=\"employees\""},
		{"skill-versions", func(p *skillsPage) { p.Selected = &row; p.DetailTab = "versions" }, "name=\"revision\""},
		{"skill-import", func(p *skillsPage) { p.Creating = true; p.Mode = "import" }, "name=\"bundle\""},
		{"skill-create", func(p *skillsPage) { p.Creating = true }, "name=\"steps\""},
		{"skill-update", func(p *skillsPage) { p.Selected = &row; p.Edit = &skill; p.Mode = "import" }, "name=\"bundle\""},
		{"skill-edit", func(p *skillsPage) { p.Selected = &row; p.Edit = &skill }, "name=\"remove_files\""},
		{"skill-deliveries", func(p *skillsPage) {
			p.View = "deliveries"
			p.Deliveries = []skillDeliveryRow{{User: "張三 · work1", Hostname: "work-desktop-01", Expected: "bippo-demo v2", Actual: "bippo-demo v2", State: "文件已同步，當前無需等待刷新（最近上報）", Status: "已同步", Tone: "ok", LastSync: now.Format("01-02 15:04:05")}, {User: "李四 · work2", Hostname: "work-desktop-02", Expected: "bippo-demo v2", Actual: "bippo-demo v1", State: "等待 Agent 同步最新版本", Status: "待同步", Tone: "warn", LastSync: now.Add(-time.Hour).Format("01-02 15:04:05")}}
		}, "員工電腦同步情況"},
		{"skill-events", func(p *skillsPage) { p.View = "events" }, "最近操作記錄"},
	}
	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			p := base
			variant.prepare(&p)
			var out bytes.Buffer
			if err := s.tpl.ExecuteTemplate(&out, "skills.html", p); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), variant.want) {
				t.Fatalf("missing %s", variant.want)
			}
			if strings.Contains(out.String(), "action=\"/customer/skills/change\"") && (!strings.Contains(out.String(), "name=\"csrf\" value=\"token\"") || !strings.Contains(out.String(), "name=\"version\" value=\"3\"")) {
				t.Fatal("mutation lost CSRF or catalog version")
			}
			if variant.name == "skill-audience" && !strings.Contains(out.String(), "name=\"action\" value=\"revoke\"") {
				t.Fatal("missing revoke action")
			}
			if dir := os.Getenv("ADMINWEB_PREVIEW_DIR"); dir != "" {
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, variant.name+".html"), out.Bytes(), 0644); err != nil {
					t.Fatal(err)
				}
				if variant.name == "skills" {
					for _, name := range []string{"app.css", "app.js", "skills.css", "skills.js"} {
						data, err := assetFS.ReadFile("assets/static/" + name)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.MkdirAll(filepath.Join(dir, "static"), 0755); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(dir, "static", name), data, 0644); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			p.Editable = false
			out.Reset()
			if err := s.tpl.ExecuteTemplate(&out, "skills.html", p); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "action=\"/customer/skills/change\"") {
				t.Fatal("viewer offered a mutation")
			}
		})
	}
}

func TestCustomerSkillLifecycleAndAuthorization(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	ctx := t.Context()
	contactCookie := func(email, role string) *http.Cookie {
		t.Helper()
		contact, err := s.dbm.db.Tickets().CreateContactWithRole(ctx, email, "測試經理", "test-hash", role)
		if err != nil {
			t.Fatal(err)
		}
		token := "test-company-skills-" + role
		sum := sha256.Sum256([]byte(token))
		if err := s.dbm.db.Tickets().CreateSession(ctx, contact.ID, sum[:], time.Hour, time.Hour); err != nil {
			t.Fatal(err)
		}
		return &http.Cookie{Name: customerCookie, Value: token}
	}
	manager := contactCookie("manager@example.com", "manager")
	employee := contactCookie("employee@example.com", "employee")
	if rec := dbGet(t, h, "/customer/skills"); rec.Code != 303 {
		t.Fatalf("unauthenticated read: %d", rec.Code)
	}
	if rec := dbGet(t, h, "/customer/skills", employee); rec.Code != 403 {
		t.Fatalf("employee read: %d", rec.Code)
	}
	if rec := dbPost(t, h, "/customer/skills/change", url.Values{}, employee); rec.Code != 403 {
		t.Fatalf("employee write: %d", rec.Code)
	}
	if rec := dbPost(t, h, "/customer/skills/change", url.Values{"csrf": {"bad"}}, manager); rec.Code != 400 {
		t.Fatalf("CSRF: %d", rec.Code)
	}
	e, err := s.dbm.store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1", Name: "張三", Department: "業務部"})
	if err != nil {
		t.Fatal(err)
	}
	csrf := s.csrfFor(manager.Value)
	form := url.Values{"csrf": {csrf}, "version": {"0"}, "action": {"save"}, "name": {"bippo-demo"}, "title": {"續保清單"}, "description": {"整理客戶續保信息"}, "when": {"每月"}, "steps": {"先讀取模板，再彙總客戶資料"}, "trial": {e.ID}}
	post := func() {
		t.Helper()
		rec := dbPost(t, h, "/customer/skills/change", form, manager)
		if rec.Code != 303 || strings.Contains(rec.Header().Get("Location"), "err=") {
			t.Fatalf("save: %d %s %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
		}
	}
	post()
	page := dbGet(t, h, "/customer/skills?edit=bippo-demo", manager)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "先讀取模板，再彙總客戶資料") {
		t.Fatalf("saved page: %d %s", page.Code, page.Body.String())
	}
	for _, path := range []string{"/customer/skills?skill=bippo-demo", "/customer/skills?skill=bippo-demo&tab=audience", "/customer/skills?skill=bippo-demo&tab=versions", "/customer/skills?new=import", "/customer/skills?view=deliveries", "/customer/skills?view=events"} {
		if rec := dbGet(t, h, path, manager); rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
	}
	if rec := dbGet(t, h, "/customer/skills?edit=teenet-missing", manager); rec.Code != http.StatusNotFound {
		t.Fatal("missing edit target must not open a create form")
	}
	form.Set("version", "1")
	form.Set("action", "assign")
	form["departments"] = []string{"業務部"}
	post()
	catalog, _, err := companyskills.Load(ctx, s.dbm.store.Settings())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := catalog.Desired(e)
	if err != nil || len(plan.Skills) != 1 {
		t.Fatal("assignment missing")
	}
	form.Set("version", "2")
	form.Set("action", "revoke")
	post()
	catalog, _, err = companyskills.Load(ctx, s.dbm.store.Settings())
	if err != nil {
		t.Fatal(err)
	}
	plan, err = catalog.Desired(e)
	if err != nil || len(plan.Skills) != 0 {
		t.Fatal("revoke did not clear audience")
	}
	events, _, err := s.dbm.store.Audit().Search(ctx, repo.AuditFilter{TargetType: "company_skill", Limit: 10})
	if err != nil || len(events) != 3 || events[0].ActorType != "customer" || !strings.Contains(string(events[0].Detail), "測試經理") {
		t.Fatalf("audit: %+v %v", events, err)
	}
}
