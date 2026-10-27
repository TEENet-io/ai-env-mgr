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
	"account-add":     {"account", "新增员工账号", true},
	"account-toggle":  {"account", "停用或恢复账号", true},
	"account-delete":  {"account", "员工离职／删除账号", true},
	"account-edit":    {"account", "修改员工信息", true},
	"desktop-problem": {"desktop", "远程桌面使用问题", false},
	"software-add":    {"desktop", "安装软件", true},
	"software-change": {"desktop", "更新或卸载软件", true},
	"website":         {"desktop", "调整网站访问限制", true},
	"ai-problem":      {"ai", "AI 使用遇到问题", false},
	"quota":           {"ai", "额度用完，申请增加", true},
	"tool-problem":    {"tool", "工具使用遇到问题", false},
	"other-problem":   {"other", "其他问题", false},
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
			return "", nil, false, errors.New("数量和费用应为有效的非负数，追加数量必须大于零")
		}
		if payload["currency"] != "USD" && payload["currency"] != "CNY" {
			return "", nil, false, errors.New("请选择方案币种")
		}
		if _, err := time.Parse("2006-01-02", payload["valid_until"]); err != nil {
			return "", nil, false, errors.New("请填写有效期结束日期")
		}
		return kind, payload, strings.Trim(payload["cost"], "0.") != "", nil
	default:
		return "", nil, false, errors.New("请选择账号删除或额度追加方案")
	}
}

func dailyText(r *http.Request, name string, max int, required bool) (string, error) {
	value := formValue(r, name)
	if len(value) > max || containsCredential(value) || (required && value == "") {
		return "", fmt.Errorf("%s 未填写、过长或包含疑似密钥", name)
	}
	return value, nil
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
		return repo.Employee{}, errors.New("请选择当前员工名单中的人员")
	}
	return employee, nil
}

func dailyTargets(r *http.Request, options map[string]repo.Employee) ([]map[string]string, string, error) {
	ids := r.PostForm["employee_ids"]
	if len(ids) == 0 || len(ids) > len(options) {
		return nil, "", errors.New("请至少选择一位适用员工")
	}
	seen := make(map[string]bool, len(ids))
	targets := make([]map[string]string, 0, len(ids))
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		employee, ok := options[id]
		if !ok || seen[id] {
			return nil, "", errors.New("适用员工无效或重复")
		}
		seen[id] = true
		targets = append(targets, map[string]string{"id": id, "name": employee.Name, "email": employee.Email})
		names = append(names, employee.Name)
	}
	return targets, strings.Join(names, "、"), nil
}

func parseDailyTicketSubmission(r *http.Request, employees []repo.Employee) (kind, title, description, priority, key string, details []byte, err error) {
	requestKind := formValue(r, "request_kind")
	spec, ok := dailyRequests[requestKind]
	if !ok {
		err = errors.New("请选择工单事项")
		return
	}
	key = formValue(r, "request_key")
	if len(key) != 32 {
		err = errors.New("表单无效，请刷新")
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
				err = fmt.Errorf("第 %d 位员工需要有效姓名和工作邮箱", i)
				return
			}
			if seenEmail[email] {
				err = fmt.Errorf("第 %d 位员工的工作邮箱重复", i)
				return
			}
			seenEmail[email] = true
			rows = append(rows, map[string]string{"name": name, "email": email, "number": number})
			lines = append(lines, name+" <"+email+">")
		}
		if len(rows) == 0 {
			err = errors.New("请至少填写一位员工")
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
		data["target_employee"] = map[string]string{"id": employee.ID, "name": employee.Name, "email": employee.Email}
		title += " · " + employee.Name
		lines = append(lines, "员工："+employee.Name+" <"+employee.Email+">")
		if requestKind == "account-toggle" {
			action := formValue(r, "account_action")
			if action != "disable" && action != "restore" {
				err = errors.New("请选择停用或恢复")
				return
			}
			data["action"] = action
			lines = append(lines, "操作："+map[string]string{"disable": "停用", "restore": "恢复"}[action])
		}
		if requestKind == "account-edit" {
			changes := map[string]string{}
			for field, current := range map[string]string{"name": employee.Name, "email": employee.Email, "number": employee.ExternalID} {
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
							err = errors.New("新工作邮箱无效")
							return
						}
					}
					changes[field] = value
					lines = append(lines, field+"："+current+" → "+value)
				}
			}
			if len(changes) == 0 {
				err = errors.New("请填写至少一项不同于现有值的修改")
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
		lines = append(lines, "适用员工："+names)
		var software string
		software, err = dailyText(r, "software", 200, true)
		if err != nil {
			return
		}
		data["software"] = software
		title += " · " + software
		lines = append(lines, "软件："+software)
		if requestKind == "software-change" {
			action := formValue(r, "software_action")
			if action != "update" && action != "uninstall" {
				err = errors.New("请选择更新或卸载")
				return
			}
			data["action"] = action
			lines = append(lines, "操作："+map[string]string{"update": "更新", "uninstall": "卸载"}[action])
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
			err = errors.New("请选择封禁、解除封禁或临时放行")
			return
		}
		data["action"] = action
		selected := r.PostForm["sites"]
		allowed := map[string]bool{"ChatGPT": true, "Claude": true, "Gemini": true, "Grok": true, "Perplexity": true, "Microsoft Copilot": true, "DeepSeek": true, "通义千问": true, "Kimi": true, "豆包": true, "文心一言": true, "智谱清言": true, "MiniMax": true, "腾讯元宝": true, "讯飞星火": true}
		seen := map[string]bool{}
		for _, site := range selected {
			if !allowed[site] || seen[site] {
				err = errors.New("网站选择无效或重复")
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
			err = errors.New("请至少选择或填写一个网站")
			return
		}
		data["sites"] = selected
		if other != "" {
			data["other_site"] = other
			selected = append(selected, other)
		}
		lines = append(lines, "操作："+map[string]string{"block": "封禁", "unblock": "解除封禁", "temporary": "临时放行"}[action], "网站："+strings.Join(selected, "、"))
		if action == "temporary" {
			expires := formValue(r, "expires")
			if _, parseErr := time.Parse("2006-01-02T15:04", expires); parseErr != nil {
				err = errors.New("临时放行需要有效的截止日期和时间")
				return
			}
			data["expires"] = expires
			lines = append(lines, "截止："+expires)
		}
	case "quota":
		lines = append(lines, "申请追加当前登录人员的 AI 使用额度；具体数量、单位、有效期和费用待服务团队提出方案。")
	case "desktop-problem", "ai-problem", "tool-problem", "other-problem":
		var problem string
		problem, err = dailyText(r, "problem", 3000, true)
		if err != nil {
			return
		}
		data["problem"] = problem
		lines = append(lines, problem)
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
		value, fieldErr := dailyText(r, field, 1000, false)
		if fieldErr != nil {
			err = fieldErr
			return
		}
		if value != "" {
			if field == "desired_at" {
				if _, parseErr := time.Parse("2006-01-02T15:04", value); parseErr != nil {
					err = errors.New("希望处理时间无效")
					return
				}
			}
			data[field] = value
			lines = append(lines, map[string]string{"note": "补充说明：", "desired_at": "希望处理时间："}[field]+value)
		}
	}
	description = strings.Join(lines, "\n")
	if !validTicketText(title, 160) || !validTicketText(description, 10000) {
		err = errors.New("工单内容无效、过长或包含疑似密钥")
		return
	}
	details, err = json.Marshal(data)
	return
}
