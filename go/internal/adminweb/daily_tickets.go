package adminweb

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/dbstore"
	"github.com/TEENet-io/ai-env-mgr/internal/directory"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// Daily requests are the post-deployment customer service catalogue. The
// existing general/bug database types remain in place for older tickets;
// request_kind carries the more precise, versioned customer-facing type.
type dailyRequestSpec struct {
	Category string
	Label    string
	Change   bool
}

var dailyRequests = map[string]dailyRequestSpec{
	"account-add":     {"account", "新增 AI 工作間帳號", true},
	"account-toggle":  {"account", "停用或恢復帳號", true},
	"account-delete":  {"account", "員工離職／刪除帳號", true},
	"account-edit":    {"account", "修改員工信息", true},
	"desktop-problem": {"desktop", "遠程桌面使用問題", false},
	"software-add":    {"desktop", "安裝軟件", true},
	"software-change": {"desktop", "更新或卸載軟件", true},
	"website":         {"desktop", "調整網站訪問限制", true},
	"ai-problem":      {"ai", "AI 使用遇到問題", false},
	"quota":           {"ai", "額度用完，申請增加", true},
	"tool-problem":    {"tool", "工具使用遇到問題", false},
	"other-problem":   {"other", "其他問題", false},
}

// contactMayRequest reports whether a customer contact may raise this kind of
// request. A manager may raise every kind; anyone else everything about using
// the service, but nothing about accounts or quota.
func contactMayRequest(role, requestKind string) bool {
	if role == "manager" {
		return true
	}
	return dailyRequests[requestKind].Category != "account" && requestKind != "quota"
}

// needsWorkspace is a kind about a remote desktop or the AI in it: only
// somebody with an AI workspace account has those to ask about (AIC-204 #10).
func needsWorkspace(requestKind string) bool {
	category := dailyRequests[requestKind].Category
	return category == "desktop" || category == "ai"
}

var proposalDecimal = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,4})?$`)

func parseDailyProposal(r *http.Request) (string, map[string]string, bool, error) {
	kind := formValue(r, "proposal_kind")
	payload := map[string]string{}
	switch kind {
	case "account-delete":
		for _, field := range []string{"deletion_scope", "data_plan"} {
			value, err := dailyText(r, field, 2000, true)
			if err != nil {
				return "", nil, false, err
			}
			payload[field] = value
		}
		return kind, payload, true, nil
	case "quota":
		for _, field := range []string{"amount", "unit", "valid_until", "cost", "currency"} {
			value, err := dailyText(r, field, 100, true)
			if err != nil {
				return "", nil, false, err
			}
			payload[field] = value
		}
		if !proposalDecimal.MatchString(payload["amount"]) || !proposalDecimal.MatchString(payload["cost"]) || strings.Trim(payload["amount"], "0.") == "" {
			return "", nil, false, errors.New("數量和費用應為有效的非負數，追加數量必須大於零")
		}
		if payload["currency"] != "USD" && payload["currency"] != "CNY" {
			return "", nil, false, errors.New("請選擇方案幣種")
		}
		if _, err := time.Parse("2006-01-02", payload["valid_until"]); err != nil {
			return "", nil, false, errors.New("請填寫有效期結束日期")
		}
		return kind, payload, strings.Trim(payload["cost"], "0.") != "", nil
	default:
		return "", nil, false, errors.New("請選擇帳號刪除或額度追加方案")
	}
}

func dailyText(r *http.Request, name string, max int, required bool) (string, error) {
	value := formValue(r, name)
	if len(value) > max || containsCredential(value) || (required && value == "") {
		return "", fmt.Errorf("%s 未填寫、過長或包含疑似密鑰", name)
	}
	return value, nil
}

// employeeName is what to call somebody who may have no name on file: the
// email, then the Windows user, which every employee has.
func employeeName(e repo.Employee) string {
	switch {
	case e.Name != "":
		return e.Name
	case e.Email != "":
		return e.Email
	}
	return e.WindowsUser
}

// employeeLabel identifies somebody in one line, with their status.
func employeeLabel(e repo.Employee) string {
	label := employeeName(e)
	if e.Name != "" && e.Email != "" {
		label += " · " + e.Email
	}
	if !e.Active() {
		label += "（已停用）"
	}
	return label
}

func dailyEmployeeOptions(employees []repo.Employee) map[string]repo.Employee {
	options := make(map[string]repo.Employee, len(employees))
	for _, employee := range employees {
		if !employee.Deleted() {
			options[employee.ID] = employee
		}
	}
	return options
}

func dailyVisibleEmployees(employees []repo.Employee) []repo.Employee {
	visible := make([]repo.Employee, 0, len(employees))
	for _, employee := range employees {
		if !employee.Deleted() {
			visible = append(visible, employee)
		}
	}
	return visible
}

func dailyAuthorizedEmployees(employees []repo.Employee, contact dbstore.CustomerContact) []repo.Employee {
	visible := dailyVisibleEmployees(employees)
	if contact.Role == "manager" {
		return visible
	}
	self := make([]repo.Employee, 0, 1)
	for _, employee := range visible {
		if strings.EqualFold(employee.Email, contact.Email) {
			self = append(self, employee)
		}
	}
	if len(self) != 1 {
		return nil
	}
	return self
}

func dailyEmployee(r *http.Request, options map[string]repo.Employee) (repo.Employee, error) {
	employee, ok := options[formValue(r, "target_employee")]
	if !ok {
		return repo.Employee{}, errors.New("請選擇當前員工名單中的人員")
	}
	return employee, nil
}

func dailyTargets(r *http.Request, options map[string]repo.Employee) ([]map[string]string, string, error) {
	ids := r.PostForm["employee_ids"]
	if len(ids) == 0 || len(ids) > len(options) {
		return nil, "", errors.New("請至少選擇一位適用員工")
	}
	seen := make(map[string]bool, len(ids))
	targets := make([]map[string]string, 0, len(ids))
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		employee, ok := options[id]
		if !ok || seen[id] {
			return nil, "", errors.New("適用員工無效或重複")
		}
		seen[id] = true
		targets = append(targets, map[string]string{"id": id, "name": employeeName(employee), "email": employee.Email})
		names = append(names, employeeName(employee))
	}
	return targets, strings.Join(names, "、"), nil
}

// dailyPickedPeople reads who a new AI workspace account is asked for, from
// the people the form offered. Each row keeps the name and email the tool had
// when the request was made.
func dailyPickedPeople(r *http.Request, unopened []directory.Account) ([]map[string]string, []string, error) {
	offered := make(map[string]directory.Account, len(unopened))
	for _, a := range unopened {
		offered[a.ID] = a
	}
	ids := r.PostForm["directory_ids"]
	if len(ids) == 0 {
		return nil, nil, errors.New("請至少選擇一位員工")
	}
	if len(ids) > 50 {
		return nil, nil, errors.New("一次最多選擇 50 位員工")
	}
	rows := make([]map[string]string, 0, len(ids))
	var lines []string
	seen := map[string]bool{}
	for _, id := range ids {
		a, ok := offered[id]
		if !ok {
			return nil, nil, errors.New("所選員工已經有 AI 工作間帳號，或已不在定製工具名單裡，請刷新後重選")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		rows = append(rows, map[string]string{"directory_id": a.ID, "username": a.Username, "name": a.DisplayName(), "email": a.Email})
		line := a.DisplayName()
		if a.Email != "" {
			line += " <" + a.Email + ">"
		}
		lines = append(lines, line)
	}
	return rows, lines, nil
}

// dailyDateTime reads a time typed as 2026-11-05 14:30 (the form uses a plain
// text box, since a date box shows the format of the browser's language) and
// returns it in the format stored. Empty is ok; the caller decides if it may be.
func dailyDateTime(value string) (string, bool) {
	if value == "" {
		return "", true
	}
	t, err := time.Parse("2006-01-02 15:04", strings.Replace(value, "T", " ", 1))
	if err != nil {
		return "", false
	}
	return t.Format("2006-01-02T15:04"), true
}

// parseDailyTicketSubmission reads the request form. unopened is the custom
// tool's people without an AI workspace account when the list comes from the
// tool: a new account is then asked for by picking them, not by typing a
// name; nil keeps the typed rows.
func parseDailyTicketSubmission(r *http.Request, employees []repo.Employee, unopened []directory.Account) (kind, title, description, priority, key string, details []byte, err error) {
	requestKind := formValue(r, "request_kind")
	spec, ok := dailyRequests[requestKind]
	if !ok {
		err = errors.New("請選擇工單事項")
		return
	}
	key = formValue(r, "request_key")
	if len(key) != 32 {
		err = errors.New("表單無效，請刷新")
		return
	}
	if spec.Change {
		kind = "general"
	} else {
		kind = "bug"
	}
	priority = "normal"
	title = spec.Label
	data := map[string]any{"schema_version": 4, "request_kind": requestKind, "category": spec.Category}
	options := dailyEmployeeOptions(employees)
	var lines []string

	switch requestKind {
	case "account-add":
		if unopened != nil {
			var rows []map[string]string
			if rows, lines, err = dailyPickedPeople(r, unopened); err != nil {
				return
			}
			data["employees"] = rows
			title += " · " + rows[0]["name"]
			break
		}
		rows := make([]map[string]string, 0, 10)
		seenEmail := map[string]bool{}
		for i := 1; i <= 10; i++ {
			suffix := strconv.Itoa(i)
			name := formValue(r, "employee_name_"+suffix)
			email := strings.ToLower(formValue(r, "work_email_"+suffix))
			number := formValue(r, "employee_number_"+suffix)
			if name == "" && email == "" && number == "" {
				continue
			}
			address, parseErr := mail.ParseAddress(email)
			if !validTicketText(name, 100) || parseErr != nil || address.Address != email || len(email) > 254 || len(number) > 100 || containsCredential(number) {
				err = fmt.Errorf("第 %d 位員工需要有效姓名和工作郵箱", i)
				return
			}
			if seenEmail[email] {
				err = fmt.Errorf("第 %d 位員工的工作郵箱重複", i)
				return
			}
			seenEmail[email] = true
			rows = append(rows, map[string]string{"name": name, "email": email, "number": number})
			lines = append(lines, name+" <"+email+">")
		}
		if len(rows) == 0 {
			err = errors.New("請至少填寫一位員工")
			return
		}
		data["employees"] = rows
		title += " · " + rows[0]["name"]
	case "account-toggle", "account-delete", "account-edit":
		var employee repo.Employee
		employee, err = dailyEmployee(r, options)
		if err != nil {
			return
		}
		data["target_employee"] = map[string]string{"id": employee.ID, "name": employeeName(employee), "email": employee.Email}
		title += " · " + employeeName(employee)
		lines = append(lines, "員工："+employeeLabel(employee))
		if requestKind == "account-toggle" {
			action := formValue(r, "account_action")
			if action != "disable" && action != "restore" {
				err = errors.New("請選擇停用或恢復")
				return
			}
			data["action"] = action
			lines = append(lines, "操作："+map[string]string{"disable": "停用", "restore": "恢復"}[action])
		}
		if requestKind == "account-edit" {
			changes := map[string]string{}
			editable := map[string]string{"name": employee.Name, "email": employee.Email, "number": employee.ExternalID}
			if unopened != nil {
				// Names and emails are the custom tool's to change.
				if formValue(r, "new_name") != "" || formValue(r, "new_email") != "" {
					err = errors.New("姓名和工作郵箱以定製工具為準，請在定製工具裡修改")
					return
				}
				editable = map[string]string{"number": employee.ExternalID}
			}
			for field, current := range editable {
				max := 100
				if field == "email" {
					max = 254
				}
				value, fieldErr := dailyText(r, "new_"+field, max, false)
				if fieldErr != nil {
					err = fieldErr
					return
				}
				if value != "" && value != current {
					if field == "email" {
						address, parseErr := mail.ParseAddress(value)
						if parseErr != nil || address.Address != value {
							err = errors.New("新工作郵箱無效")
							return
						}
					}
					changes[field] = value
					lines = append(lines, field+"："+current+" → "+value)
				}
			}
			if len(changes) == 0 {
				err = errors.New("請填寫至少一項不同於現有值的修改")
				return
			}
			data["changes"] = changes
		}
	case "software-add", "software-change":
		var targets []map[string]string
		var names string
		targets, names, err = dailyTargets(r, options)
		if err != nil {
			return
		}
		data["target_employees"] = targets
		lines = append(lines, "適用員工："+names)
		var software string
		software, err = dailyText(r, "software", 200, true)
		if err != nil {
			return
		}
		data["software"] = software
		title += " · " + software
		lines = append(lines, "軟件："+software)
		if requestKind == "software-change" {
			action := formValue(r, "software_action")
			if action != "update" && action != "uninstall" {
				err = errors.New("請選擇更新或卸載")
				return
			}
			data["action"] = action
			lines = append(lines, "操作："+map[string]string{"update": "更新", "uninstall": "卸載"}[action])
		}
		version, versionErr := dailyText(r, "min_version", 100, false)
		if versionErr != nil {
			err = versionErr
			return
		}
		if version != "" {
			data["min_version"] = version
			lines = append(lines, "最低版本："+version)
		}
	case "website":
		action := formValue(r, "website_action")
		if action != "block" && action != "unblock" && action != "temporary" {
			err = errors.New("請選擇封禁、解除封禁或臨時放行")
			return
		}
		data["action"] = action
		selected := r.PostForm["sites"]
		allowed := map[string]bool{"ChatGPT": true, "Claude": true, "Gemini": true, "Grok": true, "Perplexity": true, "Microsoft Copilot": true, "DeepSeek": true, "通義千問": true, "Kimi": true, "豆包": true, "文心一言": true, "智譜清言": true, "MiniMax": true, "騰訊元寶": true, "訊飛星火": true}
		seen := map[string]bool{}
		for _, site := range selected {
			if !allowed[site] || seen[site] {
				err = errors.New("網站選擇無效或重複")
				return
			}
			seen[site] = true
		}
		var other string
		other, err = dailyText(r, "other_site", 200, false)
		if err != nil {
			return
		}
		if len(selected) == 0 && other == "" {
			err = errors.New("請至少選擇或填寫一個網站")
			return
		}
		data["sites"] = selected
		if other != "" {
			data["other_site"] = other
			selected = append(selected, other)
		}
		lines = append(lines, "操作："+map[string]string{"block": "封禁", "unblock": "解除封禁", "temporary": "臨時放行"}[action], "網站："+strings.Join(selected, "、"))
		if action == "temporary" {
			expires, ok := dailyDateTime(formValue(r, "expires"))
			if !ok || expires == "" {
				err = errors.New("臨時放行需要有效的截止日期和時間，例如 2026-11-05 14:30")
				return
			}
			data["expires"] = expires
			lines = append(lines, "截止："+expires)
		}
	case "quota":
		var targets []map[string]string
		var names string
		targets, names, err = dailyTargets(r, options)
		if err != nil {
			return
		}
		data["target_employees"] = targets
		lines = append(lines, "適用員工："+names, "申請追加以上員工的 AI 使用額度；具體數量、單位、有效期和費用待服務團隊提出方案。")
	case "desktop-problem", "ai-problem", "tool-problem", "other-problem":
		var problem string
		problem, err = dailyText(r, "problem", 3000, true)
		if err != nil {
			return
		}
		data["problem"] = problem
		lines = append(lines, problem)
		if requestKind == "desktop-problem" {
			occurred, occurredErr := dailyText(r, "occurred_at", 100, false)
			if occurredErr != nil {
				err = occurredErr
				return
			}
			if occurred != "" {
				data["occurred_at"] = occurred
				lines = append(lines, "大約發生時間："+occurred)
			}
		}
		if requestKind == "tool-problem" {
			tool, toolErr := dailyText(r, "tool", 200, true)
			if toolErr != nil {
				err = toolErr
				return
			}
			data["tool"] = tool
			lines = append([]string{"工具：" + tool}, lines...)
		}
		if len([]rune(problem)) <= 40 {
			title = problem
		}
	}

	for _, field := range []string{"note", "desired_at"} {
		// The design asks for no handling time on a problem report.
		if field == "desired_at" && !spec.Change {
			continue
		}
		value, fieldErr := dailyText(r, field, 1000, false)
		if fieldErr != nil {
			err = fieldErr
			return
		}
		if value != "" {
			if field == "desired_at" {
				normalized, ok := dailyDateTime(value)
				if !ok {
					err = errors.New("希望處理時間無效，例如 2026-11-05 14:30")
					return
				}
				value = normalized
			}
			data[field] = value
			lines = append(lines, map[string]string{"note": "補充說明：", "desired_at": "希望處理時間："}[field]+value)
		}
	}
	description = strings.Join(lines, "\n")
	if !validTicketText(title, 160) || !validTicketText(description, 10000) {
		err = errors.New("工單內容無效、過長或包含疑似密鑰")
		return
	}
	details, err = json.Marshal(data)
	return
}
