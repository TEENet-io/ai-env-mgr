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
	Employees, Departments []skillOption
	TrialLabel             string
}
type skillEvent struct {
	repo.AuditEvent
	ActorName, ActionLabel, Change string
}
type skillDeliveryRow struct{ Hostname, User, Expected, Actual, State, LastSync string }
type skillsPage struct {
	AdminPage                       *pageData
	Name, CSRF, Base, Notice, Error string
	Editable                        bool
	Version                         int
	Rows                            []skillRow
	Employees                       []skillOption
	Edit                            *companyskills.Skill
	Description, When, Steps        string
	Attachments                     []string
	Events                          []skillEvent
	Deliveries                      []skillDeliveryRow
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
	if rest, ok := strings.CutPrefix(body, "## 何时使用\n"); ok {
		if a, b, found := strings.Cut(rest, "\n## 操作步骤\n"); found {
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
		http.Error(w, "公司 Skill 由客户经理管理", http.StatusForbidden)
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
		http.Error(w, "读取 Skill 失败", 500)
		return
	}
	roster, err := s.dbm.store.Employees().List(r.Context(), repo.EmployeeFilter{})
	if err != nil {
		http.Error(w, "读取员工失败", 500)
		return
	}
	p.Version, p.Notice, p.Error = version, r.URL.Query().Get("msg"), r.URL.Query().Get("err")
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
	for _, skill := range catalog.Skills {
		row := skillRow{Skill: skill}
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
		if r.URL.Query().Get("edit") == skill.Name {
			copy := skill
			p.Edit = &copy
			bundle, err := skill.Bundle()
			if err != nil {
				http.Error(w, "Skill 版本损坏", 500)
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
	events, _, err := s.dbm.store.Audit().Search(r.Context(), repo.AuditFilter{TargetType: "company_skill", Limit: 50})
	if err != nil {
		http.Error(w, "读取 Skill 操作记录失败", 500)
		return
	}
	labels := map[string]string{"skill.save": "保存新版本", "skill.assign": "修改派发范围", "skill.trial": "派发试用", "skill.rollback": "使用历史版本", "skill.revoke": "全部撤回"}
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
				label = "已停用的员工"
			}
			recipients = append(recipients, "试用："+label)
		}
		for _, id := range after.Employees {
			label := people[id]
			if label == "" {
				label = "已停用的员工"
			}
			recipients = append(recipients, label)
		}
		for _, department := range after.Departments {
			recipients = append(recipients, "部门："+department)
		}
		if len(recipients) == 0 {
			recipients = append(recipients, "未派发")
		}
		actor := detail["actor_name"]
		if actor == "" {
			actor = event.ActorID
		}
		p.Events = append(p.Events, skillEvent{AuditEvent: event, ActorName: actor, ActionLabel: labels[event.Action], Change: fmt.Sprintf("v%d · %s", after.Current, strings.Join(recipients, "；"))})
	}
	devices, err := s.dbm.store.Devices().List(r.Context(), repo.DeviceFilter{})
	if err != nil {
		http.Error(w, "读取设备失败", 500)
		return
	}
	for _, device := range devices {
		binding, err := s.dbm.store.Bindings().Open(r.Context(), device.ID)
		if errors.Is(err, repo.ErrNotFound) {
			continue
		}
		if err != nil {
			http.Error(w, "读取设备绑定失败", 500)
			return
		}
		e, err := s.dbm.store.Employees().ByID(r.Context(), binding.EmployeeID)
		if err != nil {
			http.Error(w, "读取绑定员工失败", 500)
			return
		}
		plan, err := catalog.Desired(e)
		if err != nil {
			http.Error(w, "计算派发清单失败", 500)
			return
		}
		row := skillDeliveryRow{Hostname: device.Hostname, User: e.Name + " · " + e.WindowsUser, State: "等待 Agent 同步"}
		var expected []string
		for _, skill := range plan.Skills {
			expected = append(expected, fmt.Sprintf("%s v%d", skill.Name, skill.Version))
		}
		row.Expected = strings.Join(expected, ", ")
		report, err := s.dbm.store.Reports().Get(r.Context(), device.ID)
		if err != nil && !errors.Is(err, repo.ErrNotFound) {
			http.Error(w, "读取派发结果失败", 500)
			return
		}
		var actual model.Status
		if err == nil && json.Unmarshal(report.Report, &actual) == nil {
			row.LastSync = actual.LastSync
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
					row.State = map[string]string{"failed": "同步失败，请联系 TEENet", "restart_pending": "等待关闭 Codex，刷新 Skill", "deferred": "等待员工登录云电脑"}[actual.ManagedSkills.State]
					if row.State == "" {
						row.State = "等待 Agent 同步"
					}
				} else if matches {
					row.State = "已同步（最近上报）"
				}
			} else {
				row.State = "Agent 尚未上报 Skill 能力"
			}
		}
		p.Deliveries = append(p.Deliveries, row)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, "skills.html", p); err != nil {
		s.events.Ops("error", "skills_render", "render skills failed", map[string]any{"error": err.Error()})
	}
}

func parseSkillPost(w http.ResponseWriter, r *http.Request, csrf string) error {
	if r.Method != http.MethodPost {
		return errors.New("请使用表单提交")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 6<<20)
	if err := parseUpload(r); err != nil {
		return errors.New("表单或附件过大，最大 6 MiB")
	}
	if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(csrf)) != 1 {
		return errors.New("表单已过期，请刷新页面")
	}
	return nil
}

func skillChange(r *http.Request, catalog companyskills.Catalog, actorType, actor string) (companyskills.Change, error) {
	version, err := strconv.Atoi(r.PostFormValue("version"))
	if err != nil || version < 0 {
		return companyskills.Change{}, errors.New("缺少页面版本，请刷新")
	}
	c := companyskills.Change{Action: r.PostFormValue("action"), Name: strings.TrimSpace(r.PostFormValue("name")), Title: r.PostFormValue("title"), TrialEmployee: r.PostFormValue("trial"), Employees: r.PostForm["employees"], Departments: r.PostForm["departments"], ExpectedVersion: version, ActorType: actorType, ActorID: actor}
	c.Revision, _ = strconv.Atoi(r.PostFormValue("revision"))
	if c.Action != "save" {
		return c, nil
	}
	if c.Name == "" {
		id := make([]byte, 6)
		if _, err := rand.Read(id); err != nil {
			return c, err
		}
		c.Name = "teenet-" + hex.EncodeToString(id)
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
	} else {
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
	if err == nil {
		var c companyskills.Change
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
	http.Redirect(w, r, base+"?msg="+url.QueryEscape("已保存，等待在线 Agent 同步；更新或撤回会关闭相关员工的 Codex 以刷新 Skill。"), http.StatusSeeOther)
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
		http.Error(w, "公司 Skill 由客户经理管理", http.StatusForbidden)
		return
	}
	s.applySkillChange(w, r, "/customer/skills", csrf, "customer", c.ID, c.Name)
}
