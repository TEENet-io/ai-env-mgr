package adminweb

import (
	"bytes"
	"crypto/sha256"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/companyskills"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestCompanySkillTemplatesAndRoles(t *testing.T) {
	s := newTestServer(t, newFakeStore())
	bundle, err := companyskills.FromForm("bippo-demo", "整理清单", "每月", "读取模板后输出结果", nil)
	if err != nil {
		t.Fatal(err)
	}
	skill := companyskills.Skill{Name: bundle.Name, Title: "续保客户清单", Current: 1, Revisions: []companyskills.Revision{{Bundle: bundle, By: "TEENet", At: time.Now()}}}
	p := skillsPage{Base: "/customer/skills", CSRF: "token", Editable: true, Version: 3, Edit: &skill, Rows: []skillRow{{Skill: skill, Employees: []skillOption{{Value: "employee-1", Label: "张三 · work1"}}}}}
	var out bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&out, "skills.html", p); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`action="/customer/skills/change"`, `name="version" value="3"`, `name="bundle"`, `name="employees"`, `name="revision"`, `name="action" value="revoke"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %s", want)
		}
	}
	p.Editable = false
	out.Reset()
	if err := s.tpl.ExecuteTemplate(&out, "skills.html", p); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), `action="/customer/skills/change"`) {
		t.Fatal("viewer offered a mutation")
	}
	if allowed(repo.RoleViewer, "POST", "/skills/change") || !allowed(repo.RoleOperator, "POST", "/skills/change") {
		t.Fatal("skill role boundary")
	}
	if dir := os.Getenv("ADMINWEB_PREVIEW_DIR"); dir != "" {
		p.Editable = true
		out.Reset()
		if err := s.tpl.ExecuteTemplate(&out, "skills.html", p); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "skills.html"), out.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCustomerSkillLifecycleAndAuthorization(t *testing.T) {
	s, _ := newDatabaseServer(t)
	h := s.Handler()
	ctx := t.Context()
	contactCookie := func(email, role string) *http.Cookie {
		t.Helper()
		contact, err := s.dbm.db.Tickets().CreateContactWithRole(ctx, email, "测试经理", "test-hash", role)
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
	e, err := s.dbm.store.Employees().Create(ctx, repo.NewEmployee{WindowsUser: "work1", Name: "张三", Department: "业务部"})
	if err != nil {
		t.Fatal(err)
	}
	csrf := s.csrfFor(manager.Value)
	form := url.Values{"csrf": {csrf}, "version": {"0"}, "action": {"save"}, "name": {"bippo-demo"}, "title": {"续保清单"}, "description": {"整理客户续保信息"}, "when": {"每月"}, "steps": {"先读取模板，再汇总客户资料"}, "trial": {e.ID}}
	post := func() {
		t.Helper()
		rec := dbPost(t, h, "/customer/skills/change", form, manager)
		if rec.Code != 303 || strings.Contains(rec.Header().Get("Location"), "err=") {
			t.Fatalf("save: %d %s %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
		}
	}
	post()
	page := dbGet(t, h, "/customer/skills?edit=bippo-demo", manager)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "先读取模板，再汇总客户资料") {
		t.Fatalf("saved page: %d %s", page.Code, page.Body.String())
	}
	form.Set("version", "1")
	form.Set("action", "assign")
	form["departments"] = []string{"业务部"}
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
	if err != nil || len(events) != 3 || events[0].ActorType != "customer" || !strings.Contains(string(events[0].Detail), "测试经理") {
		t.Fatalf("audit: %+v %v", events, err)
	}
}
