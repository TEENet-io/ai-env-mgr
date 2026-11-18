package adminweb

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/companyskills"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"gopkg.in/yaml.v3"
)

type skillOption struct {
	Value, Label string
	Selected     bool
}
type skillRow struct {
	companyskills.Skill
	Employees, Departments                     []skillOption
	TrialLabel                                 string
	Description, Audience, Assignment, Updated string
	RecipientCount, FileCount                  int
	History                                    []companyskills.Revision
}
type skillEvent struct {
	repo.AuditEvent
	ActorName, ActionLabel, Change string
}
type skillDeliveryRow struct{ Hostname, User, Expected, Actual, State, LastSync, Status, Tone string }
type skillsPage struct {
	AdminPage                            *pageData
	Name, CSRF, Base, Notice, Error      string
	Editable                             bool
	Version                              int
	Rows                                 []skillRow
	Employees                            []skillOption
	Edit                                 *companyskills.Skill
	Description, When, Steps             string
	Attachments                          []string
	Events                               []skillEvent
	Deliveries                           []skillDeliveryRow
	View, DetailTab, Mode, Query, Filter string
	Creating                             bool
	Selected                             *skillRow
	Total, ActiveCount, DraftCount       int
	Content                              string
}

func skillFields(bundle model.ManagedSkill) (description, when, steps string) {
	text := strings.ReplaceAll(string(bundle.Files["SKILL.md"]), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return "", "", text
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return "", "", text
	}
	var header struct {
		Description string `yaml:"description"`
	}
	_ = yaml.Unmarshal([]byte(text[4:4+end]), &header)
	body := strings.TrimSpace(text[4+end+5:])
	when, steps = "", body
	if rest, ok := strings.CutPrefix(body, companyskills.WhenHeading+"\n"); ok {
		if a, b, found := strings.Cut(rest, "\n"+companyskills.StepsHeading+"\n"); found {
			when, steps = strings.TrimSpace(a), strings.TrimSpace(b)
		}
	}
	return header.Description, when, steps
}

func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request, sess *session) {
	adminPage := newPage(sess, r, "skills")
	p := skillsPage{AdminPage: &adminPage, Name: sess.actor, CSRF: sess.csrf, Base: "/skills", Editable: sess.admin == nil || allowed(sess.admin.Role, "POST", "/skills/change")}
	s.serveSkills(w, r, p)
}

func (s *Server) handleCustomerSkills(w http.ResponseWriter, r *http.Request) {
	c, csrf, ok := s.customerRequired(w, r)
	if !ok {
		return
	}
	if c.Role != "manager" {
		http.Error(w, "公司 Skill 由客戶經理管理", http.StatusForbidden)
		return
	}
	s.serveSkills(w, r, skillsPage{Name: c.Name, CSRF: csrf, Base: "/customer/skills", Editable: true})
}

func (s *Server) serveSkills(w http.ResponseWriter, r *http.Request, p skillsPage) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	catalog, version, err := companyskills.Load(r.Context(), s.dbm.store.Settings())
	if err != nil {
		http.Error(w, "讀取 Skill 失敗", 500)
		return
	}
	roster, err := s.dbm.store.Employees().List(r.Context(), repo.EmployeeFilter{})
	if err != nil {
		http.Error(w, "讀取員工失敗", 500)
		return
	}
	p.Version, p.Notice, p.Error = version, r.URL.Query().Get("msg"), r.URL.Query().Get("err")
	p.View, p.DetailTab, p.Mode = "library", "content", "manual"
	if view := r.URL.Query().Get("view"); view == "deliveries" || view == "events" {
		p.View = view
	}
	if tab := r.URL.Query().Get("tab"); tab == "audience" || tab == "versions" {
		p.DetailTab = tab
	}
	p.Query, p.Filter = strings.TrimSpace(r.URL.Query().Get("q")), r.URL.Query().Get("filter")
	p.Creating = r.URL.Query().Get("new") != ""
	if r.URL.Query().Get("mode") == "import" || r.URL.Query().Get("new") == "import" {
		p.Mode = "import"
	}
	if !p.Editable && (p.Creating || r.URL.Query().Get("edit") != "") {
		http.Error(w, "當前賬號只能查看 Skill", http.StatusForbidden)
		return
	}
	departments := map[string]bool{}
	for _, e := range roster {
		if !e.Active() || e.Deleted() {
			continue
		}
		p.Employees = append(p.Employees, skillOption{Value: e.ID, Label: e.Name + " · " + e.WindowsUser + " · " + e.Department})
		if e.Department != "" {
			departments[e.Department] = true
		}
	}
	contains := func(values []string, want string) bool {
		for _, v := range values {
			if v == want {
				return true
			}
		}
		return false
	}
	selectedName := r.URL.Query().Get("edit")
	if selectedName == "" {
		selectedName = r.URL.Query().Get("skill")
	}
	for _, skill := range catalog.Skills {
		row := skillRow{Skill: skill}
		bundle, err := skill.Bundle()
		if err != nil {
			http.Error(w, "Skill 版本損壞", 500)
			return
		}
		row.Description, _, _ = skillFields(bundle)
		row.FileCount = len(bundle.Files)
		row.Assignment, row.Audience = "draft", "未派發"
		if skill.TrialEmployee != "" {
			row.Assignment, row.Audience = "trial", "試用中"
		} else if len(skill.Employees)+len(skill.Departments) > 0 {
			row.Assignment, row.Audience = "assigned", "已派發"
		}
		if row.Assignment == "draft" {
			p.DraftCount++
		} else {
			p.ActiveCount++
		}
		row.History = append([]companyskills.Revision(nil), skill.Revisions...)
		sort.Slice(row.History, func(i, j int) bool { return row.History[i].Bundle.Version > row.History[j].Bundle.Version })
		for _, revision := range skill.Revisions {
			if revision.Bundle.Version == skill.Current {
				row.Updated = revision.At.Local().Format("2006-01-02 15:04")
			}
		}
		for _, employee := range roster {
			if employee.Active() && !employee.Deleted() && (skill.TrialEmployee == employee.ID || contains(skill.Employees, employee.ID) || employee.Department != "" && contains(skill.Departments, employee.Department)) {
				row.RecipientCount++
			}
		}
		for _, option := range p.Employees {
			if option.Value == skill.TrialEmployee {
				row.TrialLabel = option.Label
			}
		}
		for _, option := range p.Employees {
			option.Selected = contains(skill.Employees, option.Value)
			row.Employees = append(row.Employees, option)
		}
		for department := range departments {
			row.Departments = append(row.Departments, skillOption{Value: department, Label: department, Selected: contains(skill.Departments, department)})
		}
		sort.Slice(row.Departments, func(i, j int) bool { return row.Departments[i].Value < row.Departments[j].Value })
		p.Rows = append(p.Rows, row)
		if selectedName == skill.Name {
			copy := row
			p.Selected, p.Content = &copy, string(bundle.Files["SKILL.md"])
		}
		if r.URL.Query().Get("edit") == skill.Name {
			copy := skill
			p.Edit = &copy
			bundle, err := skill.Bundle()
			if err != nil {
				http.Error(w, "Skill 版本損壞", 500)
				return
			}
			p.Description, p.When, p.Steps = skillFields(bundle)
			for name := range bundle.Files {
				if name != "SKILL.md" {
					p.Attachments = append(p.Attachments, name)
				}
			}
			sort.Strings(p.Attachments)
		}
	}
	p.Total = len(p.Rows)
	if (r.URL.Query().Get("skill") != "" || r.URL.Query().Get("edit") != "") && p.Selected == nil {
		http.NotFound(w, r)
		return
	}
	if p.Selected != nil || p.Creating {
		p.View = "library"
	}
	if p.Selected == nil && !p.Creating {
		visible := make([]skillRow, 0, len(p.Rows))
		for _, row := range p.Rows {
			matches := p.Query == "" || strings.Contains(strings.ToLower(row.Title+" "+row.Name+" "+row.Description), strings.ToLower(p.Query))
			if matches && (p.Filter == "" || p.Filter == row.Assignment || p.Filter == "active" && row.Assignment != "draft") {
				visible = append(visible, row)
			}
		}
		p.Rows = visible
	}
	events, _, err := s.dbm.store.Audit().Search(r.Context(), repo.AuditFilter{TargetType: "company_skill", Limit: 50})
	if err != nil {
		http.Error(w, "讀取 Skill 操作記錄失敗", 500)
		return
	}
	labels := map[string]string{"skill.save": "保存新版本", "skill.assign": "修改派發範圍", "skill.trial": "派發試用", "skill.rollback": "使用歷史版本", "skill.revoke": "全部撤回"}
	people := map[string]string{}
	for _, e := range roster {
		people[e.ID] = e.Name + " · " + e.WindowsUser
	}
	for _, event := range events {
		var detail map[string]string
		_ = json.Unmarshal(event.Detail, &detail)
		var after struct {
			Current                int    `json:"version"`
			Trial                  string `json:"trial"`
			Employees, Departments []string
		}
		_ = json.Unmarshal(event.After, &after)
		var recipients []string
		if after.Trial != "" {
			label := people[after.Trial]
			if label == "" {
				label = "已停用的員工"
			}
			recipients = append(recipients, "試用："+label)
		}
		for _, id := range after.Employees {
			label := people[id]
			if label == "" {
				label = "已停用的員工"
			}
			recipients = append(recipients, label)
		}
		for _, department := range after.Departments {
			recipients = append(recipients, "部門："+department)
		}
		if len(recipients) == 0 {
			recipients = append(recipients, "未派發")
		}
		actor := detail["actor_name"]
		if actor == "" {
			actor = event.ActorID
		}
		p.Events = append(p.Events, skillEvent{AuditEvent: event, ActorName: actor, ActionLabel: labels[event.Action], Change: fmt.Sprintf("v%d · %s", after.Current, strings.Join(recipients, "；"))})
	}
	devices, err := s.dbm.store.Devices().List(r.Context(), repo.DeviceFilter{})
	if err != nil {
		http.Error(w, "讀取設備失敗", 500)
		return
	}
	for _, device := range devices {
		binding, err := s.dbm.store.Bindings().Open(r.Context(), device.ID)
		if errors.Is(err, repo.ErrNotFound) {
			continue
		}
		if err != nil {
			http.Error(w, "讀取設備綁定失敗", 500)
			return
		}
		e, err := s.dbm.store.Employees().ByID(r.Context(), binding.EmployeeID)
		if err != nil {
			http.Error(w, "讀取綁定員工失敗", 500)
			return
		}
		plan, err := catalog.Desired(e)
		if err != nil {
			http.Error(w, "計算派發清單失敗", 500)
			return
		}
		row := skillDeliveryRow{Hostname: device.Hostname, User: e.Name + " · " + e.WindowsUser, State: "等待 Agent 同步", Status: "待同步", Tone: "warn"}
		var expected []string
		for _, skill := range plan.Skills {
			expected = append(expected, fmt.Sprintf("%s v%d", skill.Name, skill.Version))
		}
		row.Expected = strings.Join(expected, ", ")
		report, err := s.dbm.store.Reports().Get(r.Context(), device.ID)
		if err != nil && !errors.Is(err, repo.ErrNotFound) {
			http.Error(w, "讀取派發結果失敗", 500)
			return
		}
		var actual model.Status
		if err == nil && json.Unmarshal(report.Report, &actual) == nil {
			row.LastSync = actual.LastSync
			if last, err := time.Parse(time.RFC3339, actual.LastSync); err == nil {
				row.LastSync = last.Local().Format("01-02 15:04:05")
			}
			if actual.ManagedSkills != nil {
				var versions []string
				matches := actual.ManagedSkills.User == plan.User && len(actual.ManagedSkills.Skills) == len(plan.Skills)
				for _, installed := range actual.ManagedSkills.Skills {
					versions = append(versions, fmt.Sprintf("%s v%d", installed.Name, installed.Version))
					found := false
					for _, want := range plan.Skills {
						found = found || installed.Name == want.Name && installed.Version == want.Version && installed.SHA256 == want.SHA256
					}
					matches = matches && found
				}
				row.Actual = strings.Join(versions, ", ")
				if actual.ManagedSkills.State != "applied" {
					row.State = map[string]string{"failed": "同步失敗，請聯繫 TEENet", "restart_pending": "舊版 Agent 等待刷新，請升級 Agent", "refresh_pending": "文件已同步，等待 Codex 刷新", "deferred": "等待員工登錄雲電腦"}[actual.ManagedSkills.State]
					if actual.ManagedSkills.State == "refresh_pending" && !matches {
						row.State = "等待 Agent 同步最新版本"
					}
					if row.State == "" {
						row.State = "等待 Agent 同步"
					}
					if actual.ManagedSkills.State == "failed" {
						row.Status, row.Tone = "同步失敗", "bad"
					}
					if actual.ManagedSkills.State == "refresh_pending" && matches {
						row.Status = "等待刷新"
					}
				} else if matches {
					row.State = "文件已同步，當前無需等待刷新（最近上報）"
					row.Status, row.Tone = "已同步", "ok"
				}
			} else {
				row.State = "Agent 尚未上報 Skill 能力"
				row.Status = "待升級 Agent"
			}
		}
		if p.View != "deliveries" || p.Query == "" || strings.Contains(strings.ToLower(row.User+" "+row.Hostname), strings.ToLower(p.Query)) {
			p.Deliveries = append(p.Deliveries, row)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, "skills.html", p); err != nil {
		s.events.Ops("error", "skills_render", "render skills failed", map[string]any{"error": err.Error()})
	}
}

func parseSkillPost(w http.ResponseWriter, r *http.Request, csrf string) error {
	if r.Method != http.MethodPost {
		return errors.New("請使用表單提交")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 6<<20)
	if err := parseUpload(r); err != nil {
		return errors.New("表單或附件過大，最大 6 MiB")
	}
	if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(csrf)) != 1 {
		return errors.New("表單已過期，請刷新頁面")
	}
	return nil
}

func skillChange(r *http.Request, catalog companyskills.Catalog, actorType, actor string) (companyskills.Change, error) {
	version, err := strconv.Atoi(r.PostFormValue("version"))
	if err != nil || version < 0 {
		return companyskills.Change{}, errors.New("缺少頁面版本，請刷新")
	}
	c := companyskills.Change{Action: r.PostFormValue("action"), Name: strings.TrimSpace(r.PostFormValue("name")), Title: r.PostFormValue("title"), TrialEmployee: r.PostFormValue("trial"), Employees: r.PostForm["employees"], Departments: r.PostForm["departments"], ExpectedVersion: version, ActorType: actorType, ActorID: actor}
	c.Revision, _ = strconv.Atoi(r.PostFormValue("revision"))
	if c.Action != "save" {
		return c, nil
	}
	var bundle model.ManagedSkill
	if r.MultipartForm != nil && len(r.MultipartForm.File["bundle"]) > 0 && r.MultipartForm.File["bundle"][0].Filename != "" {
		file, err := r.MultipartForm.File["bundle"][0].Open()
		if err != nil {
			return c, err
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, (5<<20)+1))
		if err != nil {
			return c, err
		}
		if len(data) > 5<<20 {
			return c, errors.New("ZIP 最大 5 MiB")
		}
		bundle, err = companyskills.FromZIP(c.Name, data)
		if err != nil {
			return c, err
		}
		c.Name = bundle.Name
	} else {
		if c.Name == "" {
			id := make([]byte, 6)
			if _, err := rand.Read(id); err != nil {
				return c, err
			}
			c.Name = "teenet-" + hex.EncodeToString(id)
		}
		attachments := map[string][]byte{}
		for _, skill := range catalog.Skills {
			if skill.Name == c.Name {
				existing, err := skill.Bundle()
				if err != nil {
					return c, err
				}
				for path, data := range existing.Files {
					if path != "SKILL.md" {
						attachments[path] = data
					}
				}
			}
		}
		for _, path := range r.PostForm["remove_files"] {
			delete(attachments, path)
		}
		if r.MultipartForm != nil {
			for _, header := range r.MultipartForm.File["templates"] {
				file, err := header.Open()
				if err != nil {
					return c, err
				}
				data, readErr := io.ReadAll(io.LimitReader(file, companyskills.MaxFileBytes+1))
				closeErr := file.Close()
				if readErr != nil {
					return c, readErr
				}
				if closeErr != nil {
					return c, closeErr
				}
				attachments["references/"+header.Filename] = data
			}
		}
		bundle, err = companyskills.FromForm(c.Name, r.PostFormValue("description"), r.PostFormValue("when"), r.PostFormValue("steps"), attachments)
		if err != nil {
			return c, err
		}
	}
	for _, existing := range catalog.Skills {
		if r.PostFormValue("intent") == "create" && existing.Name == c.Name {
			return c, errors.New("這個 Skill 已存在，請進入該 Skill 的“上傳新版”更新內容")
		}
	}
	if r.PostFormValue("intent") == "update" {
		found := false
		for _, existing := range catalog.Skills {
			found = found || existing.Name == c.Name
		}
		if !found {
			return c, errors.New("要更新的 Skill 已不存在，請刷新清單")
		}
	}
	c.Bundle = bundle
	return c, nil
}

func (s *Server) applySkillChange(w http.ResponseWriter, r *http.Request, base, csrf, actorType, actor, actorName string) {
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	if err := parseSkillPost(w, r, csrf); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	catalog, _, err := companyskills.Load(r.Context(), s.dbm.store.Settings())
	var c companyskills.Change
	if err == nil {
		c, err = skillChange(r, catalog, actorType, actor)
		c.ActorName = actorName
		if err == nil {
			err = companyskills.Apply(r.Context(), s.dbm.store, c)
		}
	}
	if err != nil {
		http.Redirect(w, r, base+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	s.dbm.hub.WakeAll()
	messages := map[string]string{"save": "內容已保存爲新版本，原有派發範圍保持不變。", "assign": "派發範圍已保存，在線電腦將自動同步。", "trial": "已派發給試用員工，在線電腦將自動同步。", "rollback": "已切換到所選歷史版本，已派發的員工將同步到此版本。", "revoke": "已撤回全部派發，內容和歷史版本已保留。"}
	if c.Action == "save" && r.PostFormValue("intent") == "create" {
		messages["save"] = "Skill 已創建，可在“派發範圍”設置員工和部門。"
		if c.TrialEmployee != "" {
			messages["save"] = "Skill 已創建並派發給試用員工，在線電腦將自動同步。"
		}
	}
	tab := map[string]string{"assign": "audience", "trial": "audience", "revoke": "audience", "rollback": "versions"}[c.Action]
	values := url.Values{"skill": {c.Name}, "msg": {messages[c.Action]}}
	if tab != "" {
		values.Set("tab", tab)
	}
	http.Redirect(w, r, base+"?"+values.Encode(), http.StatusSeeOther)
}

func (s *Server) handleSkillChange(w http.ResponseWriter, r *http.Request, sess *session) {
	s.applySkillChange(w, r, "/skills", sess.csrf, "admin", sess.actor, sess.actor)
}
func (s *Server) handleCustomerSkillChange(w http.ResponseWriter, r *http.Request) {
	c, csrf, ok := s.customerRequired(w, r)
	if !ok {
		return
	}
	if c.Role != "manager" {
		http.Error(w, "公司 Skill 由客戶經理管理", http.StatusForbidden)
		return
	}
	s.applySkillChange(w, r, "/customer/skills", csrf, "customer", c.ID, c.Name)
}
