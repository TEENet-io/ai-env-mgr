package adminweb

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/authn"
	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
	"github.com/TEENet-io/ai-env-mgr/internal/directory"
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
	SSOTool                          string // the custom tool, when people sign in through it
	SubmitKey                        string
	AdminMode                        bool
	AdminPage                        pageData
	Rows                             []dbstore.Ticket
	Filter                           string
	Employees                        []repo.Employee
	// PickPeople is set when people come from the custom tool: a new AI
	// workspace account is asked for by picking one of Unopened.
	PickPeople bool
	Unopened   []directory.Account
	// NoWorkspace hides the desktop and AI requests from an employee
	// without an AI workspace account.
	NoWorkspace                bool
	Tools                      []string
	Ticket                     *dbstore.Ticket
	Events                     []dbstore.TicketEvent
	Revisions                  []dbstore.TicketRequestRevision
	Proposal                   *dbstore.TicketProposal
	Approvals                  []dbstore.PendingTicketApproval
	Owner                      bool
	EditTicket                 *dbstore.Ticket
	EditPayload                string
	Page                       int
	HasNext                    bool
	Workflow                   dbstore.TicketChangeWorkflow
	NewView                    bool
	Manager                    bool
	Pending, Working, Finished int
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
		"deepseek": "DeepSeek", "qwen": "通義千問", "kimi": "Kimi", "doubao": "豆包", "ernie": "文心一言", "glm": "智譜清言", "minimax": "MiniMax", "yuanbao": "騰訊元寶", "spark": "訊飛星火",
	}
	if label, ok := labels[site]; ok {
		return label
	}
	return site
}

func ticketOperationLabel(operation string) string {
	switch operation {
	case "onboard":
		return "開通 / 入職"
	case "offboard":
		return "停用 / 離職"
	case "change":
		return "信息或權限調整"
	case "other":
		return "其他員工操作"
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
				return nil, errors.New("Bug 信息過長或包含疑似密鑰")
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
			return nil, errors.New("請選擇員工操作類型")
		}
		details["employee_operation"] = operation
		for _, field := range []string{"company_department", "contact_name", "contact_signature"} {
			value := formValue(r, field)
			if len(value) > 100 || containsCredential(value) {
				return nil, errors.New("聯絡人或公司信息無效")
			}
			details[field] = value
		}
		if date := formValue(r, "filled_date"); date != "" {
			if _, err := time.Parse("2006-01-02", date); err != nil {
				return nil, errors.New("填寫日期無效")
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
				return nil, errors.New("員工名單第 " + index + " 行需要有效姓名和工作郵箱，且不得包含密鑰")
			}
			rows = append(rows, map[string]string{"name": name, "email": email, "number": number, "note": note})
		}
		if len(rows) == 0 {
			return nil, errors.New("請至少填寫一名員工")
		}
		details["employees"] = rows
	case "website":
		block := formValue(r, "block_websites")
		if block != "yes" && block != "no" {
			return nil, errors.New("請選擇是否需要網站封禁")
		}
		details["block_websites"] = block
		if block == "yes" {
			llm := formValue(r, "block_llm")
			if llm != "yes" && llm != "no" {
				return nil, errors.New("請選擇是否封禁大模型網站")
			}
			details["block_llm"] = llm
			if llm == "yes" {
				allowed := map[string]bool{"chatgpt": true, "claude": true, "gemini": true, "grok": true, "perplexity": true, "copilot": true, "deepseek": true, "qwen": true, "kimi": true, "doubao": true, "ernie": true, "glm": true, "minimax": true, "yuanbao": true, "spark": true}
				selected := r.PostForm["llm_site"]
				if len(selected) > len(allowed) {
					return nil, errors.New("大模型網站選擇無效")
				}
				for _, site := range selected {
					if !allowed[site] {
						return nil, errors.New("大模型網站選擇無效")
					}
				}
				details["llm_sites"] = selected
				for _, field := range []string{"other_international_llm_sites", "other_domestic_llm_sites"} {
					other := formValue(r, field)
					if len(other) > 300 || containsCredential(other) {
						return nil, errors.New("其他大模型網站內容無效")
					}
					details[field] = other
				}
				if len(selected) == 0 && formValue(r, "other_international_llm_sites") == "" && formValue(r, "other_domestic_llm_sites") == "" {
					return nil, errors.New("請至少勾選或填寫一個大模型網站")
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
					return nil, errors.New("其他網站第 " + index + " 行需要網站名稱或域名")
				}
				rows = append(rows, map[string]string{"name": name, "note": note})
			}
			if llm == "no" && len(rows) == 0 {
				return nil, errors.New("請填寫至少一個需要封禁的其他網站")
			}
			details["websites"] = rows
			value := formValue(r, "website_scope_note")
			if len(value) > 500 || containsCredential(value) {
				return nil, errors.New("網站封禁補充信息無效")
			}
			details["website_scope_note"] = value
		}
		for _, field := range []string{"company_department", "contact_name", "contact_signature"} {
			value := formValue(r, field)
			if len(value) > 100 || containsCredential(value) {
				return nil, errors.New("網站封禁補充信息無效")
			}
			details[field] = value
		}
		if date := formValue(r, "filled_date"); date != "" {
			if _, err := time.Parse("2006-01-02", date); err != nil {
				return nil, errors.New("填寫日期無效")
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
		return nil, errors.New("請選擇一般事務類型")
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
		page.Error = "無法讀取工單"
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
			page.Error = "無法讀取客戶聯絡人"
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
		http.Error(w, "無法讀取工單", 500)
		return
	}
	tasks, err := s.dbm.db.Tickets().TaskIDs(r.Context(), t.ID)
	if err != nil {
		http.Error(w, "無法讀取任務", 500)
		return
	}
	page := newPage(sess, r, "tickets")
	page.Tickets = &ticketPage{Ticket: &t, Events: events, Tasks: tasks, CSRF: sess.csrf}
	if t.Details["request_kind"] != nil {
		page.Tickets.Revisions, err = s.dbm.db.Tickets().RequestRevisions(r.Context(), t.ID, "")
		if err != nil {
			http.Error(w, "無法讀取申請歷史", 500)
			return
		}
		if requestKind := t.Details["request_kind"]; requestKind == "account-delete" || requestKind == "quota" {
			page.Tickets.Proposal, err = s.dbm.db.Tickets().LatestProposal(r.Context(), t.ID)
			if err != nil {
				http.Error(w, "無法讀取待確認方案", 500)
				return
			}
		}
	}
	if t.Type == "general" && t.Details["subtype"] == "environment" {
		workflow, err := s.dbm.db.Tickets().ChangeWorkflow(r.Context(), t.ID)
		if err != nil {
			http.Error(w, "無法讀取變更執行記錄", 500)
			return
		}
		page.Tickets.Workflow = workflow
	}
	if sess.admin.Role == repo.RoleAdmin {
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
		return errors.New("工單版本無效，請刷新")
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
			return errors.New("內容不能為空、超過 4000 字節，或包含疑似密鑰")
		}
		kind, visibility, next := "comment", "public", ""
		if formValue(r, "action") == "note" {
			visibility = "internal"
		}
		if formValue(r, "action") == "status" {
			kind = "status"
			next = formValue(r, "status")
			if next == "" {
				return errors.New("請選擇狀態")
			}
			if next == "done" {
				if body, err = s.withToolAccountResult(r, id, body); err != nil {
					return err
				}
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
		return errors.New("未知工單操作")
	}
}

// withToolAccountResult adds what was done to the departing employee's
// custom-tool account to the note that closes an offboarding ticket.
//
// The console cannot reach the custom tool yet, so an operator disables that
// account by hand; a ticket that could be completed without saying so is how
// a departed employee keeps a working login there. Other tickets pass
// through unchanged.
func (s *Server) withToolAccountResult(r *http.Request, id, body string) (string, error) {
	t, err := s.dbm.db.Tickets().Get(r.Context(), id, "")
	if err != nil {
		return "", err
	}
	if t.Details["request_kind"] != "account-delete" {
		return body, nil
	}
	result := formValue(r, "tool_account_result")
	if result == "" {
		return "", errors.New("離職工單完成前，請填寫定製工具帳號的處理結果")
	}
	if !validTicketText(result, 1000) {
		return "", errors.New("定製工具帳號處理結果超過 1000 字節，或包含疑似密鑰")
	}
	return body + "\n\n定製工具帳號：" + result, nil
}

func (s *Server) handleCustomerContactCreate(w http.ResponseWriter, r *http.Request, sess *session) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/tickets", http.StatusSeeOther)
		return
	}
	if s.oidcMode() {
		s.redirectWithError(w, r, "/tickets", errManagedByTool.Error())
		return
	}
	if err := r.ParseForm(); err != nil || subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.csrf)) != 1 {
		s.redirectWithError(w, r, "/tickets", "表單已過期，請刷新頁面")
		return
	}
	email := strings.ToLower(formValue(r, "email"))
	name := formValue(r, "name")
	role := formValue(r, "role")
	if role != "manager" && role != "employee" {
		s.redirectWithError(w, r, "/tickets", "請選擇客戶聯絡人權限")
		return
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || len(email) > 254 || len(name) < 1 || len(name) > 100 {
		s.redirectWithError(w, r, "/tickets", "請填寫有效郵箱和聯絡人姓名")
		return
	}
	password, err := authn.GeneratePassword(24)
	if err != nil {
		http.Error(w, "無法生成密碼", 500)
		return
	}
	hash, err := authn.HashPassword(password)
	if err != nil {
		http.Error(w, "無法生成密碼", 500)
		return
	}
	contact, err := s.dbm.db.Tickets().CreateContactWithRole(r.Context(), email, name, hash, role)
	if err != nil {
		s.redirectWithError(w, r, "/tickets", "建立聯絡人失敗，郵箱可能已存在")
		return
	}
	s.recordAccountEvent(r, sess.actor, "contact.create", "customer_contact", contact.ID, map[string]any{"email": email, "role": role})
	s.renderContactPassword(w, r, sess, email, password)
}

func (s *Server) actionCustomerContactRole(sess *session, r *http.Request) error {
	if s.oidcMode() {
		return errManagedByTool
	}
	id, role := formValue(r, "id"), formValue(r, "role")
	if err := s.dbm.db.Tickets().SetContactRole(r.Context(), id, role); err != nil {
		return err
	}
	s.recordAccountEvent(r, sess.actor, "contact.role", "customer_contact", id, map[string]any{"role": role})
	return nil
}

func (s *Server) handleCustomerContactReset(w http.ResponseWriter, r *http.Request, sess *session) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/tickets", 303)
		return
	}
	if s.oidcMode() {
		s.redirectWithError(w, r, "/tickets", errManagedByTool.Error())
		return
	}
	if err := r.ParseForm(); err != nil || subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.csrf)) != 1 {
		s.redirectWithError(w, r, "/tickets", "表單已過期，請刷新頁面")
		return
	}
	id := formValue(r, "id")
	contacts, err := s.dbm.db.Tickets().ListContacts(r.Context())
	if err != nil {
		http.Error(w, "無法讀取聯絡人", 500)
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
		http.Error(w, "無法生成密碼", 500)
		return
	}
	hash, err := authn.HashPassword(password)
	if err != nil {
		http.Error(w, "無法生成密碼", 500)
		return
	}
	if err := s.dbm.db.Tickets().SetContactPassword(r.Context(), id, "", hash); err != nil {
		http.Error(w, "無法重置密碼", 500)
		return
	}
	s.recordAccountEvent(r, sess.actor, "contact.password_reset", "customer_contact", id, map[string]any{"email": email})
	s.renderContactPassword(w, r, sess, email, password)
}

func (s *Server) renderContactPassword(w http.ResponseWriter, r *http.Request, sess *session, email, password string) {
	page := newPage(sess, r, "tickets")
	contacts, contactErr := s.dbm.db.Tickets().ListContacts(r.Context())
	rows, rowErr := s.dbm.db.Tickets().List(r.Context(), "", "", 26, 0)
	if contactErr != nil || rowErr != nil {
		page.Error = "帳號已更新，但列表暫時無法讀取"
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
		if s.oidcMode() {
			return errManagedByTool
		}
		id := formValue(r, "id")
		if id == "" {
			return errors.New("缺少聯絡人 ID")
		}
		if err := s.dbm.db.Tickets().SetContactDisabled(r.Context(), id, disable); err != nil {
			return err
		}
		action := "contact.enable"
		if disable {
			action = "contact.disable"
		}
		s.recordAccountEvent(r, sess.actor, action, "customer_contact", id, nil)
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
		http.Error(w, "表單已過期，請刷新頁面", http.StatusForbidden)
		return c, csrf, false
	}
	return c, csrf, true
}

func (s *Server) handleCustomerLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// In the oidc mode the page has only the one button; go straight to
		// the tool, which lets in at once whoever is already signed in there.
		// Not when the page has something to say: that is how a refused or
		// failed sign-in comes back, and going on again would loop.
		if s.oidcMode() && len(r.URL.Query()) == 0 {
			http.Redirect(w, r, "/oidc/start?entry="+entryCustomer, http.StatusSeeOther)
			return
		}
		csrf, err := randomTicketKey()
		if err != nil {
			http.Error(w, "登入暫不可用", 500)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: customerLoginCSRFCookie, Value: csrf, Path: "/customer/login", HttpOnly: true, Secure: s.browserUsesTLS(), SameSite: http.SameSiteStrictMode, MaxAge: 600})
		s.tpl.ExecuteTemplate(w, "customer_login.html", customerPage{CSRF: csrf, Error: r.URL.Query().Get("err"), SSOTool: s.ssoTool()})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if s.oidcMode() {
		http.Redirect(w, r, "/customer/login", 303)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", 400)
		return
	}
	csrfCookie, err := r.Cookie(customerLoginCSRFCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(csrfCookie.Value), []byte(r.PostFormValue("csrf"))) != 1 {
		http.Error(w, "登入表單已過期，請刷新頁面", 403)
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
		http.Error(w, "嘗試次數過多，請稍後再試", 429)
		return
	}
	contact, err := store.ContactByEmail(r.Context(), email)
	valid := err == nil && contact.DisabledAt == nil && authn.VerifyPassword(contact.PasswordHash, password) == nil
	if recErr := store.RecordLogin(r.Context(), email, source, valid); recErr != nil {
		http.Error(w, "sign-in unavailable", 500)
		return
	}
	if !valid {
		http.Redirect(w, r, "/customer/login?err="+url.QueryEscape("郵箱或密碼不正確"), 303)
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
	// Signing out here signs out of the tool too; otherwise the next visit
	// would sign straight back in through it.
	// A page that moves on rather than a redirect: the console's CSP has
	// form-action 'self', and a redirect at the end of a form post counts as
	// the form's target, so a 303 to the tool is blocked by the browser.
	if to := s.oidcLogoutURL(r, "/customer/login"); to != "" {
		bounce(w, to)
		return
	}
	http.Redirect(w, r, "/customer/login", 303)
}

func (s *Server) handleCustomerPassword(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.customerPost(w, r)
	if !ok {
		return
	}
	if s.oidcMode() {
		http.Error(w, errManagedByTool.Error(), http.StatusForbidden)
		return
	}
	oldPassword, newPassword := r.PostFormValue("old_password"), r.PostFormValue("new_password")
	if len(newPassword) < 16 || len(newPassword) > 128 {
		http.Error(w, "新密碼需為 16–128 個字符", 400)
		return
	}
	if authn.VerifyPassword(c.PasswordHash, oldPassword) != nil {
		http.Error(w, "當前密碼不正確", 403)
		return
	}
	hash, err := authn.HashPassword(newPassword)
	if err != nil {
		http.Error(w, "無法更新密碼", 500)
		return
	}
	if err := s.dbm.db.Tickets().SetContactPassword(r.Context(), c.ID, c.PasswordHash, hash); err != nil {
		http.Error(w, "密碼已變化，請重新登入", 409)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: customerCookie, Value: "", Path: "/customer", MaxAge: -1, HttpOnly: true, Secure: s.browserUsesTLS(), SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/customer/login?err="+url.QueryEscape("密碼已更新，請重新登入"), 303)
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
		http.Error(w, "無法讀取工單", 500)
		return
	}
	hasNext := len(rows) > 25
	if hasNext {
		rows = rows[:25]
	}
	counts, err := s.dbm.db.Tickets().Counts(r.Context(), c.ID)
	if err != nil {
		http.Error(w, "無法讀取工單概覽", 500)
		return
	}
	key, err := randomTicketKey()
	if err != nil {
		http.Error(w, "無法建立表單", 500)
		return
	}
	p := customerPage{NoWorkspace: !s.contactMay(r, c, "ai-problem"), Name: c.Name, Email: c.Email, CSRF: csrf, SubmitKey: key, Rows: rows, Page: page, HasNext: hasNext, Filter: filter, Manager: c.Role == "manager",
		NewView: r.URL.Query().Get("view") == "new" || r.URL.Query().Get("edit") != "", Pending: counts.Pending, Working: counts.Working,
		Finished: counts.Finished, Notice: r.URL.Query().Get("msg")}
	if p.Manager && !p.NewView {
		p.Approvals, err = s.dbm.db.Tickets().PendingManagerApprovals(r.Context(), 20)
		if err != nil {
			http.Error(w, "無法讀取待確認方案", 500)
			return
		}
	}
	if p.NewView {
		if editID := r.URL.Query().Get("edit"); editID != "" {
			editTicket, err := s.dbm.db.Tickets().Get(r.Context(), editID, c.ID)
			if err != nil || editTicket.Status != "new" || editTicket.Details["request_kind"] == nil {
				http.Error(w, "這張工單不能修改，請返回工單詳情查看最新狀態", http.StatusConflict)
				return
			}
			if requestKind, _ := editTicket.Details["request_kind"].(string); dailyRequests[requestKind].Category == "account" && c.Role != "manager" {
				http.Error(w, "只有客戶管理員可以修改 AI 工作間帳號申請", 403)
				return
			}
			payload, err := json.Marshal(map[string]any{"kind": editTicket.Details["request_kind"], "details": editTicket.Details})
			if err != nil {
				http.Error(w, "無法讀取申請內容", 500)
				return
			}
			p.EditTicket, p.EditPayload = &editTicket, string(payload)
		}
		p.Employees, err = s.dbm.store.Employees().List(r.Context(), repo.EmployeeFilter{IncludeOffboarded: true})
		if err != nil {
			http.Error(w, "無法讀取員工名單", 500)
			return
		}
		p.Employees = dailyAuthorizedEmployees(p.Employees, c)
		if p.Manager {
			p.Unopened = s.ticketUnopened(r)
		}
		p.PickPeople = s.usesDirectory()
		if p.Tools, err = s.ticketTools(r.Context()); err != nil {
			http.Error(w, "無法讀取工具清單", 500)
			return
		}
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
		http.Error(w, "無法建立表單", http.StatusInternalServerError)
		return
	}
	employees, err := s.dbm.store.Employees().List(r.Context(), repo.EmployeeFilter{IncludeOffboarded: true})
	if err != nil {
		http.Error(w, "無法讀取員工名單", http.StatusInternalServerError)
		return
	}
	tools, err := s.ticketTools(r.Context())
	if err != nil {
		http.Error(w, "無法讀取工具清單", http.StatusInternalServerError)
		return
	}
	adminPage := newPage(sess, r, "tickets")
	adminPage.GatewayTeamID = s.teamID()
	s.tpl.ExecuteTemplate(w, "customer_portal.html", customerPage{AdminMode: true, AdminPage: adminPage, Manager: true, NewView: true, Name: sess.admin.Username, CSRF: sess.csrf, SubmitKey: key, Employees: dailyVisibleEmployees(employees), Unopened: s.ticketUnopened(r), PickPeople: s.usesDirectory(), Tools: tools})
}

func (s *Server) handleAdminTicketCreate(w http.ResponseWriter, r *http.Request, sess *session) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !parseTicketForm(w, r, sess.csrf) {
		return
	}
	defer removeTicketForm(r)
	var kind, title, description, priority, key string
	var details []byte
	var err error
	if requestKind := formValue(r, "request_kind"); requestKind != "" {
		var employees []repo.Employee
		employees, err = s.dbm.store.Employees().List(r.Context(), repo.EmployeeFilter{IncludeOffboarded: true})
		if err == nil {
			kind, title, description, priority, key, details, err = parseDailyTicketSubmission(r, employees, s.ticketUnopened(r))
		}
		if err == nil {
			err = s.checkTicketTool(r, "")
		}
		if err == nil {
			var uploads []ticketUpload
			if uploads, err = ticketUploads(r, requestKind); err == nil {
				description, details, err = s.attachTicketFiles(r, uploads, nil, description, details)
			}
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
		http.Error(w, "建立工單失敗", http.StatusInternalServerError)
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
		err = errors.New("類型、標題或內容無效；不要填寫密碼或密鑰")
		return
	}
	key = formValue(r, "request_key")
	if len(key) != 32 {
		err = errors.New("表單無效，請刷新")
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

// customerTicketPost is customerPost for the request form, which may carry
// files and so needs the larger limit.
func (s *Server) customerTicketPost(w http.ResponseWriter, r *http.Request) (dbstore.CustomerContact, string, bool) {
	c, csrf, ok := s.customerRequired(w, r)
	if !ok {
		return c, csrf, false
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return c, csrf, false
	}
	return c, csrf, parseTicketForm(w, r, csrf)
}

func (s *Server) handleCustomerCreateTicket(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.customerTicketPost(w, r)
	if !ok {
		return
	}
	defer removeTicketForm(r)
	var kind, title, description, priority, key string
	var details []byte
	var err error
	if requestKind := formValue(r, "request_kind"); requestKind != "" {
		if !s.contactMay(r, c, requestKind) {
			http.Error(w, "當前帳號不能發起這類工單", 403)
			return
		}
		var employees []repo.Employee
		employees, err = s.dbm.store.Employees().List(r.Context(), repo.EmployeeFilter{IncludeOffboarded: true})
		if err == nil {
			kind, title, description, priority, key, details, err = parseDailyTicketSubmission(r, dailyAuthorizedEmployees(employees, c), s.ticketUnopened(r))
		}
		if err == nil {
			err = s.checkTicketTool(r, "")
		}
		if err == nil {
			var uploads []ticketUpload
			if uploads, err = ticketUploads(r, requestKind); err == nil {
				description, details, err = s.attachTicketFiles(r, uploads, nil, description, details)
			}
		}
	} else {
		if c.Role != "manager" {
			http.Error(w, "當前帳號不能提交舊版工單", 403)
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
		http.Error(w, "建立工單失敗", 500)
		return
	}
	http.Redirect(w, r, "/customer/ticket?id="+url.QueryEscape(t.ID), 303)
}

func (s *Server) handleCustomerEditTicket(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.customerTicketPost(w, r)
	if !ok {
		return
	}
	defer removeTicketForm(r)
	id := ticketID(r)
	version, err := strconv.Atoi(formValue(r, "version"))
	if err != nil || version < 1 {
		http.Error(w, "表單版本無效，請刷新", 400)
		return
	}
	employees, err := s.dbm.store.Employees().List(r.Context(), repo.EmployeeFilter{IncludeOffboarded: true})
	if err != nil {
		http.Error(w, "無法讀取員工名單", 500)
		return
	}
	requestKind := formValue(r, "request_kind")
	if dailyRequests[requestKind].Category == "account" && c.Role != "manager" {
		http.Error(w, "只有客戶管理員可以修改 AI 工作間帳號申請", 403)
		return
	}
	// A ticket raised before the limits keeps its kind; an edit cannot move
	// to a kind this contact may not raise.
	if !s.contactMay(r, c, requestKind) {
		current, getErr := s.dbm.db.Tickets().GetForContact(r.Context(), id, c.ID, c.Role)
		if getErr != nil || current.Details["request_kind"] != requestKind {
			http.Error(w, "當前帳號不能發起這類工單", 403)
			return
		}
	}
	kind, title, description, _, _, details, err := parseDailyTicketSubmission(r, dailyAuthorizedEmployees(employees, c), s.ticketUnopened(r))
	if err == nil {
		// What the version being edited carries: its files stay, and it may
		// keep a tool that has since left the list. A ticket this contact
		// cannot read carries nothing; the update below refuses it anyway.
		var previousTool string
		var kept []any
		if current, getErr := s.dbm.db.Tickets().GetForContact(r.Context(), id, c.ID, c.Role); getErr == nil && current.Details["request_kind"] == requestKind {
			previousTool, _ = current.Details["tool"].(string)
			kept, _ = current.Details["attachments"].([]any)
		}
		if err = s.checkTicketTool(r, previousTool); err == nil {
			var uploads []ticketUpload
			if uploads, err = ticketUploads(r, requestKind); err == nil {
				description, details, err = s.attachTicketFiles(r, uploads, kept, description, details)
			}
		}
	}
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := s.dbm.db.Tickets().UpdateDailyRequest(r.Context(), id, c.ID, version, kind, title, description, details); err != nil {
		if errors.Is(err, dbstore.ErrTicketConflict) {
			http.Error(w, "工單已開始處理或有更新，請刷新後重試", 409)
		} else {
			http.Error(w, "修改工單失敗", 400)
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
		http.Error(w, "無法讀取工單", 500)
		return
	}
	page := customerPage{Name: c.Name, CSRF: csrf, Ticket: &t, Events: events, Manager: c.Role == "manager", Owner: t.RequesterID == c.ID, Notice: r.URL.Query().Get("msg")}
	if t.Details["request_kind"] != nil {
		page.Revisions, err = s.dbm.db.Tickets().RequestRevisions(r.Context(), t.ID, t.RequesterID)
		if err != nil {
			http.Error(w, "無法讀取申請歷史", 500)
			return
		}
		if requestKind := t.Details["request_kind"]; requestKind == "account-delete" || requestKind == "quota" {
			page.Proposal, err = s.dbm.db.Tickets().LatestProposal(r.Context(), t.ID)
			if err != nil {
				http.Error(w, "無法讀取待確認方案", 500)
				return
			}
		}
	}
	if t.Type == "general" && t.Details["subtype"] == "environment" {
		workflow, err := s.dbm.db.Tickets().ChangeWorkflow(r.Context(), t.ID)
		if err != nil {
			http.Error(w, "無法讀取變更執行記錄", 500)
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
		http.Error(w, "請刷新工單", 400)
		return
	}
	proposalID, err := strconv.ParseInt(formValue(r, "proposal_id"), 10, 64)
	if err != nil || proposalID < 1 {
		http.Error(w, "方案編號無效", 400)
		return
	}
	if err := s.dbm.db.Tickets().DecideProposal(r.Context(), id, c.ID, version, proposalID, formValue(r, "decision")); err != nil {
		if errors.Is(err, dbstore.ErrTicketNotFound) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "方案已更新或當前不能確認，請刷新後重試", 409)
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
		http.Error(w, "請刷新工單", 400)
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
		http.Error(w, "請刷新工單", 400)
		return
	}
	t, err := s.dbm.db.Tickets().Get(r.Context(), id, c.ID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if formValue(r, "action") == "cancel" {
		if t.Status != "new" {
			http.Error(w, "工單已受理，請聯絡服務人員取消", 409)
			return
		}
		if err := s.dbm.db.Tickets().AddEvent(r.Context(), id, c.ID, "customer", c.ID, "status", "public", "客戶取消請求", "closed", version); err != nil {
			http.Error(w, "無法取消工單，請刷新後重試", 409)
			return
		}
		http.Redirect(w, r, "/customer/ticket?id="+url.QueryEscape(id), 303)
		return
	}
	if formValue(r, "action") == "request_cancel" {
		if t.Status != "working" && t.Status != "waiting_customer" {
			http.Error(w, "當前狀態不能申請撤銷；已執行的操作不能自動恢復", 409)
			return
		}
		reason := formValue(r, "body")
		if len(reason) > 1000 || containsCredential(reason) {
			http.Error(w, "撤銷原因過長或包含疑似密鑰", 400)
			return
		}
		body := "客戶申請撤銷；請確認已執行的部分和能否停止後續處理。"
		if reason != "" {
			body += "\n原因：" + reason
		}
		if err := s.dbm.db.Tickets().AddEvent(r.Context(), id, c.ID, "customer", c.ID, "comment", "public", body, "working", version); err != nil {
			http.Error(w, "工單已更新，請刷新後重試", 409)
			return
		}
		http.Redirect(w, r, "/customer/ticket?id="+url.QueryEscape(id), 303)
		return
	}
	if formValue(r, "action") == "confirm_result" {
		if t.Status != "done" || t.Details["request_kind"] == nil {
			http.Error(w, "當前沒有待確認的處理結果", 409)
			return
		}
		body := "客戶已確認處理結果符合申請，工單關閉。"
		if t.Type == "bug" {
			body = "客戶已確認問題解決，工單關閉。"
		}
		if err := s.dbm.db.Tickets().AddEvent(r.Context(), id, c.ID, "customer", c.ID, "status", "public", body, "closed", version); err != nil {
			http.Error(w, "工單已更新，請刷新後重試", 409)
			return
		}
		http.Redirect(w, r, "/customer/ticket?id="+url.QueryEscape(id), 303)
		return
	}
	body := formValue(r, "body")
	if !validTicketText(body, 4000) {
		http.Error(w, "回覆不能為空、超過 4000 字節，或包含疑似密鑰", 400)
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
			http.Error(w, "工單已更新，請刷新後重試", 409)
		} else {
			http.Error(w, "無法保存回覆", 400)
		}
		return
	}
	http.Redirect(w, r, "/customer/ticket?id="+url.QueryEscape(id), 303)
}

// ticketUnopened is what a request for a new AI workspace account may pick
// from: nil when people are not from the custom tool, and the people the
// list has without an account otherwise. A list that cannot be read offers
// nobody, rather than falling back to typed names.
func (s *Server) ticketUnopened(r *http.Request) []directory.Account {
	if !s.usesDirectory() {
		return nil
	}
	people, err := s.dbm.db.Directory().WithoutWorkspace(r.Context())
	if err != nil {
		log.Printf("adminweb: tickets: directory: %v", err)
		return []directory.Account{}
	}
	return people
}

// contactMay is contactMayRequest plus the AI workspace rule: an employee
// without an account does not raise desktop or AI requests. Managers ask
// for others and are not limited by their own account.
func (s *Server) contactMay(r *http.Request, c dbstore.CustomerContact, requestKind string) bool {
	if !contactMayRequest(c.Role, requestKind) {
		return false
	}
	// Only where people come from the custom tool: there an account and its
	// contact are tied by the tool's id. Elsewhere they meet only by email,
	// which is too loose to take requests away on.
	if c.Role == "manager" || !needsWorkspace(requestKind) || !s.usesDirectory() {
		return true
	}
	has, err := s.dbm.db.Tickets().ContactHasWorkspace(r.Context(), c.ID)
	if err != nil {
		// A request let through is a ticket somebody reads; one refused by
		// mistake is a person who cannot report a broken desktop.
		log.Printf("adminweb: tickets: workspace of %s: %v", c.ID, err)
		return true
	}
	return has
}
