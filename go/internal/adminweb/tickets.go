package adminweb

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/authn"
	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

const customerCookie = "aem_customer"
const customerLoginCSRFCookie = "aem_customer_login_csrf"

type ticketPage struct {
	Rows                  []dbstore.Ticket
	Ticket                *dbstore.Ticket
	Events                []dbstore.TicketEvent
	Revisions             []dbstore.TicketRequestRevision
	Proposal              *dbstore.TicketProposal
	Contacts              []dbstore.CustomerContact
	Assignees             []repo.Admin
	Tasks                 []dbstore.TicketTaskLink
	Status, NextStatus    string
	Page                  int
	HasNext               bool
	NewEmail, NewPassword string
	CSRF                  string
	Workflow              dbstore.TicketChangeWorkflow
}

type customerPage struct {
	CSRF, Name, Email, Error, Notice string
	SubmitKey                        string
	AdminMode                        bool
	AdminPage                        pageData
	Rows                             []dbstore.Ticket
	Filter                           string
	Employees                        []repo.Employee
	Ticket                           *dbstore.Ticket
	Events                           []dbstore.TicketEvent
	Revisions                        []dbstore.TicketRequestRevision
	Proposal                         *dbstore.TicketProposal
	Approvals                        []dbstore.PendingTicketApproval
	Owner                            bool
	EditTicket                       *dbstore.Ticket
	EditPayload                      string
	Page                             int
	HasNext                          bool
	Workflow                         dbstore.TicketChangeWorkflow
	NewView                          bool
	Manager                          bool
	Pending, Working, Finished       int
}

func ticketID(r *http.Request) string { return strings.TrimSpace(r.FormValue("id")) }

func backToTicket(r *http.Request) string {
	id := ticketID(r)
	if id == "" {
		return "/tickets"
	}
	return "/tickets/detail?id=" + url.QueryEscape(id)
}

func validTicketText(text string, max int) bool {
	return len(strings.TrimSpace(text)) > 0 && len(text) <= max && !containsCredential(text)
}

var credentialPattern = regexp.MustCompile(`(?i)(-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----|\bAKIA[0-9A-Z]{16}\b|\bsk-[a-z0-9_-]{20,}\b|\bAIza[0-9A-Za-z_-]{30,}\b|\b(?:password|api[_-]?key|access[_-]?key|secret|token)\s*[:=]\s*\S+)`)

func containsCredential(text string) bool { return credentialPattern.MatchString(text) }

func ticketSiteLabel(site string) string {
	labels := map[string]string{
		"chatgpt": "ChatGPT", "claude": "Claude", "gemini": "Gemini", "grok": "Grok", "perplexity": "Perplexity", "copilot": "Microsoft Copilot",
		"deepseek": "DeepSeek", "qwen": "通义千问", "kimi": "Kimi", "doubao": "豆包", "ernie": "文心一言", "glm": "智谱清言", "minimax": "MiniMax", "yuanbao": "腾讯元宝", "spark": "讯飞星火",
	}
	if label, ok := labels[site]; ok {
		return label
	}
	return site
}

func ticketOperationLabel(operation string) string {
	switch operation {
	case "onboard":
		return "开通 / 入职"
	case "offboard":
		return "停用 / 离职"
	case "change":
		return "信息或权限调整"
	case "other":
		return "其他员工操作"
	}
	return operation
}

func randomTicketKey() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func validTicketType(kind string) bool {
	switch kind {
	case "general", "bug":
		return true
	}
	return false
}

func ticketRequestDetails(r *http.Request, kind string) ([]byte, error) {
	details := map[string]any{"schema_version": 3}
	if kind == "bug" {
		for _, field := range []string{"steps", "expected", "actual", "environment"} {
			value := formValue(r, field)
			if len(value) > 2000 || containsCredential(value) {
				return nil, errors.New("Bug 信息过长或包含疑似密钥")
			}
			details[field] = value
		}
		return json.Marshal(details)
	}
	subtype := formValue(r, "subtype")
	switch subtype {
	case "employee":
		operation := formValue(r, "employee_operation")
		if operation != "onboard" && operation != "offboard" && operation != "change" && operation != "other" {
			return nil, errors.New("请选择员工操作类型")
		}
		details["employee_operation"] = operation
		for _, field := range []string{"company_department", "contact_name", "contact_signature"} {
			value := formValue(r, field)
			if len(value) > 100 || containsCredential(value) {
				return nil, errors.New("联系人或公司信息无效")
			}
			details[field] = value
		}
		if date := formValue(r, "filled_date"); date != "" {
			if _, err := time.Parse("2006-01-02", date); err != nil {
				return nil, errors.New("填写日期无效")
			}
			details["filled_date"] = date
		}
		rows := make([]map[string]string, 0, 10)
		for i := 1; i <= 10; i++ {
			index := strconv.Itoa(i)
			name := formValue(r, "employee_name_"+index)
			email := strings.ToLower(formValue(r, "work_email_"+index))
			number := formValue(r, "employee_number_"+index)
			note := formValue(r, "employee_note_"+index)
			if name == "" && email == "" && number == "" && note == "" {
				continue
			}
			addr, err := mail.ParseAddress(email)
			if !validTicketText(name, 100) || err != nil || addr.Address != email || len(email) > 254 || len(number) > 100 || len(note) > 500 || containsCredential(number+" "+note) {
				return nil, errors.New("员工名单第 " + index + " 行需要有效姓名和工作邮箱，且不得包含密钥")
			}
			rows = append(rows, map[string]string{"name": name, "email": email, "number": number, "note": note})
		}
		if len(rows) == 0 {
			return nil, errors.New("请至少填写一名员工")
		}
		details["employees"] = rows
	case "website":
		block := formValue(r, "block_websites")
		if block != "yes" && block != "no" {
			return nil, errors.New("请选择是否需要网站封禁")
		}
		details["block_websites"] = block
		if block == "yes" {
			llm := formValue(r, "block_llm")
			if llm != "yes" && llm != "no" {
				return nil, errors.New("请选择是否封禁大模型网站")
			}
			details["block_llm"] = llm
			if llm == "yes" {
				allowed := map[string]bool{"chatgpt": true, "claude": true, "gemini": true, "grok": true, "perplexity": true, "copilot": true, "deepseek": true, "qwen": true, "kimi": true, "doubao": true, "ernie": true, "glm": true, "minimax": true, "yuanbao": true, "spark": true}
				selected := r.PostForm["llm_site"]
				if len(selected) > len(allowed) {
					return nil, errors.New("大模型网站选择无效")
				}
				for _, site := range selected {
					if !allowed[site] {
						return nil, errors.New("大模型网站选择无效")
					}
				}
				details["llm_sites"] = selected
				for _, field := range []string{"other_international_llm_sites", "other_domestic_llm_sites"} {
					other := formValue(r, field)
					if len(other) > 300 || containsCredential(other) {
						return nil, errors.New("其他大模型网站内容无效")
					}
					details[field] = other
				}
				if len(selected) == 0 && formValue(r, "other_international_llm_sites") == "" && formValue(r, "other_domestic_llm_sites") == "" {
					return nil, errors.New("请至少勾选或填写一个大模型网站")
				}
			}
			rows := make([]map[string]string, 0, 10)
			for i := 1; i <= 10; i++ {
				index := strconv.Itoa(i)
				name, note := formValue(r, "website_name_"+index), formValue(r, "website_note_"+index)
				if name == "" && note == "" {
					continue
				}
				if !validTicketText(name, 200) || len(note) > 500 || containsCredential(note) {
					return nil, errors.New("其他网站第 " + index + " 行需要网站名称或域名")
				}
				rows = append(rows, map[string]string{"name": name, "note": note})
			}
			if llm == "no" && len(rows) == 0 {
				return nil, errors.New("请填写至少一个需要封禁的其他网站")
			}
			details["websites"] = rows
			value := formValue(r, "website_scope_note")
			if len(value) > 500 || containsCredential(value) {
				return nil, errors.New("网站封禁补充信息无效")
			}
			details["website_scope_note"] = value
		}
		for _, field := range []string{"company_department", "contact_name", "contact_signature"} {
			value := formValue(r, field)
			if len(value) > 100 || containsCredential(value) {
				return nil, errors.New("网站封禁补充信息无效")
			}
			details[field] = value
		}
		if date := formValue(r, "filled_date"); date != "" {
			if _, err := time.Parse("2006-01-02", date); err != nil {
				return nil, errors.New("填写日期无效")
			}
			details["filled_date"] = date
		}
	case "software":
		if err := parseSoftwarePreinstall(r, details); err != nil {
			return nil, err
		}
	case "environment":
		if err := parseEnvironmentChange(r, details); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("请选择一般事务类型")
	}
	details["subtype"] = subtype
	return json.Marshal(details)
}

func (s *Server) handleTickets(w http.ResponseWriter, r *http.Request, sess *session) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	page := newPage(sess, r, "tickets")
	tp := &ticketPage{Status: r.URL.Query().Get("status")}
	if tp.Status != "" {
		switch tp.Status {
		case "pending", "new", "working", "waiting_customer", "done", "closed":
		default:
			tp.Status = ""
		}
	}
	tp.Page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	if tp.Page < 1 {
		tp.Page = 1
	}
	if tp.Page > 10000 {
		tp.Page = 10000
	}
	rows, err := s.dbm.db.Tickets().List(r.Context(), "", tp.Status, 26, (tp.Page-1)*25)
	if err != nil {
		page.Error = "无法读取工单"
	} else {
		tp.HasNext = len(rows) > 25
		if tp.HasNext {
			rows = rows[:25]
		}
		tp.Rows = rows
	}
	if sess.admin.Role == repo.RoleAdmin {
		contacts, err := s.dbm.db.Tickets().ListContacts(r.Context())
		if err == nil {
			tp.Contacts = contacts
		} else {
			page.Error = "无法读取客户联系人"
		}
	}
	page.Tickets = tp
	s.render(w, "tickets.html", http.StatusOK, page)
}

func (s *Server) handleTicketDetail(w http.ResponseWriter, r *http.Request, sess *session) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	t, err := s.dbm.db.Tickets().Get(r.Context(), r.URL.Query().Get("id"), "")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	events, err := s.dbm.db.Tickets().Events(r.Context(), t.ID, "")
	if err != nil {
		http.Error(w, "无法读取工单", 500)
		return
	}
	tasks, err := s.dbm.db.Tickets().TaskIDs(r.Context(), t.ID)
	if err != nil {
		http.Error(w, "无法读取任务", 500)
		return
	}
	page := newPage(sess, r, "tickets")
	page.Tickets = &ticketPage{Ticket: &t, Events: events, Tasks: tasks, CSRF: sess.csrf}
	if t.Details["request_kind"] != nil {
		page.Tickets.Revisions, err = s.dbm.db.Tickets().RequestRevisions(r.Context(), t.ID, "")
		if err != nil {
			http.Error(w, "无法读取申请历史", 500)
			return
		}
		if requestKind := t.Details["request_kind"]; requestKind == "account-delete" || requestKind == "quota" {
			page.Tickets.Proposal, err = s.dbm.db.Tickets().LatestProposal(r.Context(), t.ID)
			if err != nil {
				http.Error(w, "无法读取待确认方案", 500)
				return
			}
		}
	}
	if t.Type == "general" && t.Details["subtype"] == "environment" {
		workflow, err := s.dbm.db.Tickets().ChangeWorkflow(r.Context(), t.ID)
		if err != nil {
			http.Error(w, "无法读取变更执行记录", 500)
			return
		}
		page.Tickets.Workflow = workflow
	}
	if sess.admin.Role == repo.RoleOperator || sess.admin.Role == repo.RoleAdmin {
		admins, err := s.dbm.store.Admins().List(r.Context())
		if err == nil {
			page.Tickets.Assignees = admins
		}
	}
	s.render(w, "ticket_detail.html", http.StatusOK, page)
}

func (s *Server) actionTicket(sess *session, r *http.Request) error {
	if err := r.ParseForm(); err != nil {
		return err
	}
	id := ticketID(r)
	version, err := strconv.Atoi(formValue(r, "version"))
	if err != nil || version < 1 {
		return errors.New("工单版本无效，请刷新")
	}
	actor := sess.admin.ID
	store := s.dbm.db.Tickets()
	switch formValue(r, "action") {
	case "proposal":
		kind, payload, manager, err := parseDailyProposal(r)
		if err != nil {
			return err
		}
		return store.SaveProposal(r.Context(), id, actor, version, kind, payload, manager)
	case "change_result":
		result, err := parseChangeImplementation(r)
		if err != nil {
			return err
		}
		return store.SaveChangeImplementation(r.Context(), id, actor, version, result)
	case "reply", "note", "status":
		body := formValue(r, "body")
		if !validTicketText(body, 4000) {
			return errors.New("内容不能为空、超过 4000 字节，或包含疑似密钥")
		}
		kind, visibility, next := "comment", "public", ""
		if formValue(r, "action") == "note" {
			visibility = "internal"
		}
		if formValue(r, "action") == "status" {
			kind = "status"
			next = formValue(r, "status")
			if next == "" {
				return errors.New("请选择状态")
			}
		}
		return store.AddEvent(r.Context(), id, "", "admin", actor, kind, visibility, body, next, version)
	case "assign":
		return store.Assign(r.Context(), id, formValue(r, "assignee"), actor, version)
	case "link_subject":
		return store.LinkSubject(r.Context(), id, formValue(r, "subject"), formValue(r, "key"), actor, version)
	case "link_task":
		if _, err := store.Get(r.Context(), id, ""); err != nil {
			return err
		}
		return store.LinkTask(r.Context(), id, formValue(r, "task_kind"), formValue(r, "task_id"), actor)
	default:
		return errors.New("未知工单操作")
	}
}

func (s *Server) handleCustomerContactCreate(w http.ResponseWriter, r *http.Request, sess *session) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/tickets", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil || subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.csrf)) != 1 {
		s.redirectWithError(w, r, "/tickets", "表单已过期，请刷新页面")
		return
	}
	email := strings.ToLower(formValue(r, "email"))
	name := formValue(r, "name")
	role := formValue(r, "role")
	if role != "manager" && role != "employee" {
		s.redirectWithError(w, r, "/tickets", "请选择客户联系人权限")
		return
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || len(email) > 254 || len(name) < 1 || len(name) > 100 {
		s.redirectWithError(w, r, "/tickets", "请填写有效邮箱和联系人姓名")
		return
	}
	password, err := authn.GeneratePassword(24)
	if err != nil {
		http.Error(w, "无法生成密码", 500)
		return
	}
	hash, err := authn.HashPassword(password)
	if err != nil {
		http.Error(w, "无法生成密码", 500)
		return
	}
	if _, err := s.dbm.db.Tickets().CreateContactWithRole(r.Context(), email, name, hash, role); err != nil {
		s.redirectWithError(w, r, "/tickets", "创建联系人失败，邮箱可能已存在")
		return
	}
	s.events.Audit("customer_contact_created", "customer contact created", map[string]any{"email": email, "by": sess.actor})
	s.renderContactPassword(w, r, sess, email, password)
}

func (s *Server) actionCustomerContactRole(sess *session, r *http.Request) error {
	id, role := formValue(r, "id"), formValue(r, "role")
	if err := s.dbm.db.Tickets().SetContactRole(r.Context(), id, role); err != nil {
		return err
	}
	s.events.Audit("customer_contact_role_changed", "customer contact role changed", map[string]any{"id": id, "role": role, "by": sess.actor})
	return nil
}

func (s *Server) handleCustomerContactReset(w http.ResponseWriter, r *http.Request, sess *session) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/tickets", 303)
		return
	}
	if err := r.ParseForm(); err != nil || subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.csrf)) != 1 {
		s.redirectWithError(w, r, "/tickets", "表单已过期，请刷新页面")
		return
	}
	id := formValue(r, "id")
	contacts, err := s.dbm.db.Tickets().ListContacts(r.Context())
	if err != nil {
		http.Error(w, "无法读取联系人", 500)
		return
	}
	var email string
	for _, c := range contacts {
		if c.ID == id && c.DisabledAt == nil {
			email = c.Email
			break
		}
	}
	if email == "" {
		http.NotFound(w, r)
		return
	}
	password, err := authn.GeneratePassword(24)
	if err != nil {
		http.Error(w, "无法生成密码", 500)
		return
	}
	hash, err := authn.HashPassword(password)
	if err != nil {
		http.Error(w, "无法生成密码", 500)
		return
	}
	if err := s.dbm.db.Tickets().SetContactPassword(r.Context(), id, "", hash); err != nil {
		http.Error(w, "无法重置密码", 500)
		return
	}
	s.events.Audit("customer_contact_password_reset", "customer contact password reset", map[string]any{"id": id, "by": sess.actor})
	s.renderContactPassword(w, r, sess, email, password)
}

func (s *Server) renderContactPassword(w http.ResponseWriter, r *http.Request, sess *session, email, password string) {
	page := newPage(sess, r, "tickets")
	contacts, contactErr := s.dbm.db.Tickets().ListContacts(r.Context())
	rows, rowErr := s.dbm.db.Tickets().List(r.Context(), "", "", 26, 0)
	if contactErr != nil || rowErr != nil {
		page.Error = "账号已更新，但列表暂时无法读取"
	}
	hasNext := len(rows) > 25
	if hasNext {
		rows = rows[:25]
	}
	page.Tickets = &ticketPage{Contacts: contacts, Rows: rows, Page: 1, HasNext: hasNext, NewEmail: email, NewPassword: password}
	s.render(w, "tickets.html", 200, page)
}

func (s *Server) actionCustomerContactDisabled(disable bool) func(*session, *http.Request) error {
	return func(sess *session, r *http.Request) error {
		id := formValue(r, "id")
		if id == "" {
			return errors.New("缺少联系人 ID")
		}
		if err := s.dbm.db.Tickets().SetContactDisabled(r.Context(), id, disable); err != nil {
			return err
		}
		s.events.Audit("customer_contact_access_changed", "customer contact access changed", map[string]any{"id": id, "disabled": disable, "by": sess.actor})
		return nil
	}
}

func (s *Server) customerSession(r *http.Request) (dbstore.CustomerContact, string, bool) {
	c, err := r.Cookie(customerCookie)
	if err != nil || c.Value == "" {
		return dbstore.CustomerContact{}, "", false
	}
	sum := sha256.Sum256([]byte(c.Value))
	contact, err := s.dbm.db.Tickets().ContactBySession(r.Context(), sum[:], s.opts.IdleTTL)
	if err != nil {
		return dbstore.CustomerContact{}, "", false
	}
	return contact, s.csrfFor(c.Value), true
}

func (s *Server) customerRequired(w http.ResponseWriter, r *http.Request) (dbstore.CustomerContact, string, bool) {
	contact, csrf, ok := s.customerSession(r)
	if !ok {
		http.Redirect(w, r, "/customer/login", http.StatusSeeOther)
	}
	return contact, csrf, ok
}

func (s *Server) customerPost(w http.ResponseWriter, r *http.Request) (dbstore.CustomerContact, string, bool) {
	c, csrf, ok := s.customerRequired(w, r)
	if !ok {
		return c, csrf, false
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return c, csrf, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := r.ParseForm(); err != nil || subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(csrf)) != 1 {
		http.Error(w, "表单已过期，请刷新页面", http.StatusForbidden)
		return c, csrf, false
	}
	return c, csrf, true
}

func (s *Server) handleCustomerLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		csrf, err := randomTicketKey()
		if err != nil {
			http.Error(w, "登录暂不可用", 500)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: customerLoginCSRFCookie, Value: csrf, Path: "/customer/login", HttpOnly: true, Secure: s.browserUsesTLS(), SameSite: http.SameSiteStrictMode, MaxAge: 600})
		s.tpl.ExecuteTemplate(w, "customer_login.html", customerPage{CSRF: csrf, Error: r.URL.Query().Get("err")})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", 400)
		return
	}
	csrfCookie, err := r.Cookie(customerLoginCSRFCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(csrfCookie.Value), []byte(r.PostFormValue("csrf"))) != 1 {
		http.Error(w, "登录表单已过期，请刷新页面", 403)
		return
	}
	email := strings.ToLower(formValue(r, "email"))
	password := r.PostFormValue("password")
	if len(email) > 254 || len(password) > 1024 {
		http.Error(w, "invalid credentials", 400)
		return
	}
	store := s.dbm.db.Tickets()
	source := s.clientKey(r)
	allowed, err := store.LoginAllowed(r.Context(), email, source)
	if err != nil {
		http.Error(w, "sign-in unavailable", 500)
		return
	}
	if !allowed {
		http.Error(w, "尝试次数过多，请稍后再试", 429)
		return
	}
	contact, err := store.ContactByEmail(r.Context(), email)
	valid := err == nil && contact.DisabledAt == nil && authn.VerifyPassword(contact.PasswordHash, password) == nil
	if recErr := store.RecordLogin(r.Context(), email, source, valid); recErr != nil {
		http.Error(w, "sign-in unavailable", 500)
		return
	}
	if !valid {
		http.Redirect(w, r, "/customer/login?err="+url.QueryEscape("邮箱或密码不正确"), 303)
		return
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		http.Error(w, "sign-in unavailable", 500)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(token))
	if err := store.CreateSession(r.Context(), contact.ID, sum[:], s.opts.IdleTTL, s.opts.AbsTTL); err != nil {
		http.Error(w, "sign-in unavailable", 500)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: customerCookie, Value: token, Path: "/customer", HttpOnly: true, Secure: s.browserUsesTLS(), SameSite: http.SameSiteStrictMode})
	http.SetCookie(w, &http.Cookie{Name: customerLoginCSRFCookie, Value: "", Path: "/customer/login", MaxAge: -1, HttpOnly: true, Secure: s.browserUsesTLS(), SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/customer", 303)
}

func (s *Server) handleCustomerLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	_, _, ok := s.customerPost(w, r)
	if !ok {
		return
	}
	if c, err := r.Cookie(customerCookie); err == nil {
		sum := sha256.Sum256([]byte(c.Value))
		_ = s.dbm.db.Tickets().DeleteSession(r.Context(), sum[:])
	}
	http.SetCookie(w, &http.Cookie{Name: customerCookie, Value: "", Path: "/customer", MaxAge: -1, HttpOnly: true, Secure: s.browserUsesTLS(), SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/customer/login", 303)
}

func (s *Server) handleCustomerPassword(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.customerPost(w, r)
	if !ok {
		return
	}
	oldPassword, newPassword := r.PostFormValue("old_password"), r.PostFormValue("new_password")
	if len(newPassword) < 16 || len(newPassword) > 128 {
		http.Error(w, "新密码需为 16–128 个字符", 400)
		return
	}
	if authn.VerifyPassword(c.PasswordHash, oldPassword) != nil {
		http.Error(w, "当前密码不正确", 403)
		return
	}
	hash, err := authn.HashPassword(newPassword)
	if err != nil {
		http.Error(w, "无法更新密码", 500)
		return
	}
	if err := s.dbm.db.Tickets().SetContactPassword(r.Context(), c.ID, c.PasswordHash, hash); err != nil {
		http.Error(w, "密码已变化，请重新登录", 409)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: customerCookie, Value: "", Path: "/customer", MaxAge: -1, HttpOnly: true, Secure: s.browserUsesTLS(), SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/customer/login?err="+url.QueryEscape("密码已更新，请重新登录"), 303)
}

func (s *Server) handleCustomerHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/customer" || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	c, csrf, ok := s.customerRequired(w, r)
	if !ok {
		return
	}
	page := 1
	if n, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && n > 0 && n < 10000 {
		page = n
	}
	filter := r.URL.Query().Get("status")
	if filter != "customer_action" && filter != "active" && filter != "finished" {
		filter = ""
	}
	rows, err := s.dbm.db.Tickets().List(r.Context(), c.ID, filter, 26, (page-1)*25)
	if err != nil {
		http.Error(w, "无法读取工单", 500)
		return
	}
	hasNext := len(rows) > 25
	if hasNext {
		rows = rows[:25]
	}
	counts, err := s.dbm.db.Tickets().Counts(r.Context(), c.ID)
	if err != nil {
		http.Error(w, "无法读取工单概览", 500)
		return
	}
	key, err := randomTicketKey()
	if err != nil {
		http.Error(w, "无法创建表单", 500)
		return
	}
	p := customerPage{Name: c.Name, Email: c.Email, CSRF: csrf, SubmitKey: key, Rows: rows, Page: page, HasNext: hasNext, Filter: filter, Manager: c.Role == "manager",
		NewView: r.URL.Query().Get("view") == "new" || r.URL.Query().Get("edit") != "", Pending: counts.Pending, Working: counts.Working,
		Finished: counts.Finished, Notice: r.URL.Query().Get("msg")}
	if p.Manager && !p.NewView {
		p.Approvals, err = s.dbm.db.Tickets().PendingManagerApprovals(r.Context(), 20)
		if err != nil {
			http.Error(w, "无法读取待确认方案", 500)
			return
		}
	}
	if p.NewView {
		if editID := r.URL.Query().Get("edit"); editID != "" {
			editTicket, err := s.dbm.db.Tickets().Get(r.Context(), editID, c.ID)
			if err != nil || editTicket.Status != "new" || editTicket.Details["request_kind"] == nil {
				http.Error(w, "这张工单不能修改，请返回工单详情查看最新状态", http.StatusConflict)
				return
			}
			if requestKind, _ := editTicket.Details["request_kind"].(string); dailyRequests[requestKind].Category == "account" && c.Role != "manager" {
				http.Error(w, "只有账号管理联系人可以修改员工账号申请", 403)
				return
			}
			payload, err := json.Marshal(map[string]any{"kind": editTicket.Details["request_kind"], "details": editTicket.Details})
			if err != nil {
				http.Error(w, "无法读取申请内容", 500)
				return
			}
			p.EditTicket, p.EditPayload = &editTicket, string(payload)
		}
		p.Employees, err = s.dbm.store.Employees().List(r.Context(), repo.EmployeeFilter{IncludeOffboarded: true})
		if err != nil {
			http.Error(w, "无法读取员工名单", 500)
			return
		}
		p.Employees = dailyAuthorizedEmployees(p.Employees, c)
	}
	s.tpl.ExecuteTemplate(w, "customer_portal.html", p)
}

func (s *Server) handleAdminTicketNew(w http.ResponseWriter, r *http.Request, sess *session) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	key, err := randomTicketKey()
	if err != nil {
		http.Error(w, "无法创建表单", http.StatusInternalServerError)
		return
	}
	employees, err := s.dbm.store.Employees().List(r.Context(), repo.EmployeeFilter{IncludeOffboarded: true})
	if err != nil {
		http.Error(w, "无法读取员工名单", http.StatusInternalServerError)
		return
	}
	s.tpl.ExecuteTemplate(w, "customer_portal.html", customerPage{AdminMode: true, AdminPage: newPage(sess, r, "tickets"), Manager: true, NewView: true, Name: sess.admin.Username, CSRF: sess.csrf, SubmitKey: key, Employees: employees})
}

func (s *Server) handleAdminTicketCreate(w http.ResponseWriter, r *http.Request, sess *session) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := r.ParseForm(); err != nil || subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.csrf)) != 1 {
		http.Error(w, "表单已过期，请刷新页面", http.StatusForbidden)
		return
	}
	var kind, title, description, priority, key string
	var details []byte
	var err error
	if formValue(r, "request_kind") != "" {
		var employees []repo.Employee
		employees, err = s.dbm.store.Employees().List(r.Context(), repo.EmployeeFilter{IncludeOffboarded: true})
		if err == nil {
			kind, title, description, priority, key, details, err = parseDailyTicketSubmission(r, employees)
		}
	} else {
		kind, title, description, priority, key, details, err = parseTicketSubmission(r)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	t, err := s.dbm.db.Tickets().CreateByAdmin(r.Context(), key, kind, title, description, priority, details, sess.admin.ID)
	if err != nil {
		http.Error(w, "创建工单失败", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/tickets/detail?id="+url.QueryEscape(t.ID), http.StatusSeeOther)
}

func parseTicketSubmission(r *http.Request) (kind, title, description, priority, key string, details []byte, err error) {
	kind, title, description = formValue(r, "type"), formValue(r, "title"), formValue(r, "description")
	priority = formValue(r, "priority")
	if priority != "urgent" {
		priority = "normal"
	}
	if !validTicketType(kind) || !validTicketText(title, 160) || !validTicketText(description, 10000) {
		err = errors.New("类型、标题或内容无效；不要填写密码或密钥")
		return
	}
	key = formValue(r, "request_key")
	if len(key) != 32 {
		err = errors.New("表单无效，请刷新")
		return
	}
	details, err = ticketRequestDetails(r, kind)
	if err == nil && kind == "general" && formValue(r, "subtype") == "environment" {
		priority = "normal"
		if formValue(r, "change_urgent") == "yes" {
			priority = "urgent"
		}
	}
	return
}

func (s *Server) handleCustomerCreateTicket(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.customerPost(w, r)
	if !ok {
		return
	}
	var kind, title, description, priority, key string
	var details []byte
	var err error
	if requestKind := formValue(r, "request_kind"); requestKind != "" {
		if dailyRequests[requestKind].Category == "account" && c.Role != "manager" {
			http.Error(w, "只有账号管理联系人可以申请员工账号变更", 403)
			return
		}
		var employees []repo.Employee
		employees, err = s.dbm.store.Employees().List(r.Context(), repo.EmployeeFilter{IncludeOffboarded: true})
		if err == nil {
			kind, title, description, priority, key, details, err = parseDailyTicketSubmission(r, dailyAuthorizedEmployees(employees, c))
		}
	} else {
		if c.Role != "manager" {
			http.Error(w, "当前账号不能提交旧版工单", 403)
			return
		}
		kind, title, description, priority, key, details, err = parseTicketSubmission(r)
	}
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	t, err := s.dbm.db.Tickets().Create(r.Context(), c.ID, key, kind, title, description, priority, details)
	if err != nil {
		http.Error(w, "创建工单失败", 500)
		return
	}
	http.Redirect(w, r, "/customer/ticket?id="+url.QueryEscape(t.ID), 303)
}

func (s *Server) handleCustomerEditTicket(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.customerPost(w, r)
	if !ok {
		return
	}
	id := ticketID(r)
	version, err := strconv.Atoi(formValue(r, "version"))
	if err != nil || version < 1 {
		http.Error(w, "表单版本无效，请刷新", 400)
		return
	}
	employees, err := s.dbm.store.Employees().List(r.Context(), repo.EmployeeFilter{IncludeOffboarded: true})
	if err != nil {
		http.Error(w, "无法读取员工名单", 500)
		return
	}
	requestKind := formValue(r, "request_kind")
	if dailyRequests[requestKind].Category == "account" && c.Role != "manager" {
		http.Error(w, "只有账号管理联系人可以修改员工账号申请", 403)
		return
	}
	kind, title, description, _, _, details, err := parseDailyTicketSubmission(r, dailyAuthorizedEmployees(employees, c))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := s.dbm.db.Tickets().UpdateDailyRequest(r.Context(), id, c.ID, version, kind, title, description, details); err != nil {
		if errors.Is(err, dbstore.ErrTicketConflict) {
			http.Error(w, "工单已开始处理或有更新，请刷新后重试", 409)
		} else {
			http.Error(w, "修改工单失败", 400)
		}
		return
	}
	http.Redirect(w, r, "/customer/ticket?id="+url.QueryEscape(id), 303)
}

func (s *Server) handleCustomerTicket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	c, csrf, ok := s.customerRequired(w, r)
	if !ok {
		return
	}
	t, err := s.dbm.db.Tickets().GetForContact(r.Context(), r.URL.Query().Get("id"), c.ID, c.Role)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	events, err := s.dbm.db.Tickets().Events(r.Context(), t.ID, t.RequesterID)
	if err != nil {
		http.Error(w, "无法读取工单", 500)
		return
	}
	page := customerPage{Name: c.Name, CSRF: csrf, Ticket: &t, Events: events, Manager: c.Role == "manager", Owner: t.RequesterID == c.ID, Notice: r.URL.Query().Get("msg")}
	if t.Details["request_kind"] != nil {
		page.Revisions, err = s.dbm.db.Tickets().RequestRevisions(r.Context(), t.ID, t.RequesterID)
		if err != nil {
			http.Error(w, "无法读取申请历史", 500)
			return
		}
		if requestKind := t.Details["request_kind"]; requestKind == "account-delete" || requestKind == "quota" {
			page.Proposal, err = s.dbm.db.Tickets().LatestProposal(r.Context(), t.ID)
			if err != nil {
				http.Error(w, "无法读取待确认方案", 500)
				return
			}
		}
	}
	if t.Type == "general" && t.Details["subtype"] == "environment" {
		workflow, err := s.dbm.db.Tickets().ChangeWorkflow(r.Context(), t.ID)
		if err != nil {
			http.Error(w, "无法读取变更执行记录", 500)
			return
		}
		page.Workflow = workflow
	}
	s.tpl.ExecuteTemplate(w, "customer_ticket.html", page)
}

func (s *Server) handleCustomerDecideProposal(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.customerPost(w, r)
	if !ok {
		return
	}
	id := ticketID(r)
	version, err := strconv.Atoi(formValue(r, "version"))
	if err != nil || version < 1 {
		http.Error(w, "请刷新工单", 400)
		return
	}
	proposalID, err := strconv.ParseInt(formValue(r, "proposal_id"), 10, 64)
	if err != nil || proposalID < 1 {
		http.Error(w, "方案编号无效", 400)
		return
	}
	if err := s.dbm.db.Tickets().DecideProposal(r.Context(), id, c.ID, version, proposalID, formValue(r, "decision")); err != nil {
		if errors.Is(err, dbstore.ErrTicketNotFound) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "方案已更新或当前不能确认，请刷新后重试", 409)
		}
		return
	}
	http.Redirect(w, r, "/customer/ticket?id="+url.QueryEscape(id), 303)
}

func (s *Server) handleCustomerConfirmChange(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.customerPost(w, r)
	if !ok {
		return
	}
	id := ticketID(r)
	version, err := strconv.Atoi(formValue(r, "version"))
	if err != nil || version < 1 {
		http.Error(w, "请刷新工单", 400)
		return
	}
	if _, err := s.dbm.db.Tickets().Get(r.Context(), id, c.ID); err != nil {
		http.NotFound(w, r)
		return
	}
	confirmation, err := parseChangeConfirmation(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := s.dbm.db.Tickets().ConfirmChangeResult(r.Context(), id, c.ID, version, confirmation); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	http.Redirect(w, r, "/customer/ticket?id="+url.QueryEscape(id), http.StatusSeeOther)
}

func (s *Server) handleCustomerReply(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.customerPost(w, r)
	if !ok {
		return
	}
	id := ticketID(r)
	version, err := strconv.Atoi(formValue(r, "version"))
	if err != nil {
		http.Error(w, "请刷新工单", 400)
		return
	}
	t, err := s.dbm.db.Tickets().Get(r.Context(), id, c.ID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if formValue(r, "action") == "cancel" {
		if t.Status != "new" {
			http.Error(w, "工单已受理，请联系服务人员取消", 409)
			return
		}
		if err := s.dbm.db.Tickets().AddEvent(r.Context(), id, c.ID, "customer", c.ID, "status", "public", "客户取消请求", "closed", version); err != nil {
			http.Error(w, "无法取消工单，请刷新后重试", 409)
			return
		}
		http.Redirect(w, r, "/customer/ticket?id="+url.QueryEscape(id), 303)
		return
	}
	if formValue(r, "action") == "request_cancel" {
		if t.Status != "working" && t.Status != "waiting_customer" {
			http.Error(w, "当前状态不能申请撤销；已执行的操作不能自动恢复", 409)
			return
		}
		reason := formValue(r, "body")
		if len(reason) > 1000 || containsCredential(reason) {
			http.Error(w, "撤销原因过长或包含疑似密钥", 400)
			return
		}
		body := "客户申请撤销；请确认已执行的部分和能否停止后续处理。"
		if reason != "" {
			body += "\n原因：" + reason
		}
		if err := s.dbm.db.Tickets().AddEvent(r.Context(), id, c.ID, "customer", c.ID, "comment", "public", body, "working", version); err != nil {
			http.Error(w, "工单已更新，请刷新后重试", 409)
			return
		}
		http.Redirect(w, r, "/customer/ticket?id="+url.QueryEscape(id), 303)
		return
	}
	if formValue(r, "action") == "confirm_result" {
		if t.Status != "done" || t.Details["request_kind"] == nil {
			http.Error(w, "当前没有待确认的处理结果", 409)
			return
		}
		body := "客户已确认处理结果符合申请，工单关闭。"
		if t.Type == "bug" {
			body = "客户已确认问题解决，工单关闭。"
		}
		if err := s.dbm.db.Tickets().AddEvent(r.Context(), id, c.ID, "customer", c.ID, "status", "public", body, "closed", version); err != nil {
			http.Error(w, "工单已更新，请刷新后重试", 409)
			return
		}
		http.Redirect(w, r, "/customer/ticket?id="+url.QueryEscape(id), 303)
		return
	}
	body := formValue(r, "body")
	if !validTicketText(body, 4000) {
		http.Error(w, "回复不能为空、超过 4000 字节，或包含疑似密钥", 400)
		return
	}
	next := ""
	if t.Status == "waiting_customer" {
		next = "working"
	}
	if t.Status == "done" {
		next = "new"
	}
	if err := s.dbm.db.Tickets().AddEvent(r.Context(), id, c.ID, "customer", c.ID, "comment", "public", body, next, version); err != nil {
		if errors.Is(err, dbstore.ErrTicketConflict) {
			http.Error(w, "工单已更新，请刷新后重试", 409)
		} else {
			http.Error(w, "无法保存回复", 400)
		}
		return
	}
	http.Redirect(w, r, "/customer/ticket?id="+url.QueryEscape(id), 303)
}
