package adminweb

import (
	"errors"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
)

func ticketOptionalText(value string, max int) bool {
	return len(value) <= max && !containsCredential(value)
}

func ticketDate(value string) bool {
	_, err := time.Parse("2006-01-02", value)
	return err == nil
}

func ticketEmail(value string) bool {
	addr, err := mail.ParseAddress(value)
	return err == nil && addr.Address == value && len(value) <= 254
}

func addTicketContact(r *http.Request, details map[string]any) error {
	for _, field := range []string{"company_department", "contact_name", "contact_signature"} {
		value := formValue(r, field)
		if !ticketOptionalText(value, 100) {
			return errors.New("公司或联系人信息无效")
		}
		details[field] = value
	}
	if value := formValue(r, "filled_date"); value != "" {
		if !ticketDate(value) {
			return errors.New("填写日期无效")
		}
		details["filled_date"] = value
	}
	return nil
}

func parseSoftwarePreinstall(r *http.Request, details map[string]any) error {
	rows := make([]map[string]string, 0, 10)
	for i := 1; i <= 10; i++ {
		index := strconv.Itoa(i)
		name := formValue(r, "software_name_"+index)
		version := formValue(r, "software_min_version_"+index)
		note := formValue(r, "software_note_"+index)
		if name == "" && version == "" && note == "" {
			continue
		}
		if !validTicketText(name, 200) || !ticketOptionalText(version, 100) || !ticketOptionalText(note, 500) {
			return errors.New("软件清单第 " + index + " 行需要软件名称，且不能包含密钥")
		}
		rows = append(rows, map[string]string{"name": name, "min_version": version, "note": note})
	}
	if len(rows) == 0 {
		return errors.New("请填写至少一个软件；如果没有，请在第一行填写“无”")
	}
	if len(rows) > 1 {
		for _, row := range rows {
			if row["name"] == "无" {
				return errors.New("“无”不能与其他软件同时填写")
			}
		}
	}
	details["software_items"] = rows
	return addTicketContact(r, details)
}

func parseEnvironmentChange(r *http.Request, details map[string]any) error {
	date := formValue(r, "change_application_date")
	if !ticketDate(date) {
		return errors.New("请填写有效的申请日期")
	}
	details["change_application_date"] = date
	changeType := formValue(r, "change_type")
	if changeType != "personnel" && changeType != "software" && changeType != "website" {
		return errors.New("请选择人员、软件或网站变更类型")
	}
	details["change_type"] = changeType
	action := formValue(r, "change_action")
	if action != "add" && action != "delete" && action != "modify" && action != "temporary_exception" {
		return errors.New("请选择变更动作")
	}
	details["change_action"] = action
	if desired := formValue(r, "change_desired_at"); desired != "" {
		if _, err := time.Parse("2006-01-02T15:04", desired); err != nil {
			return errors.New("期望生效时间无效")
		}
		details["change_desired_at"] = desired
	}
	urgent := formValue(r, "change_urgent")
	if urgent != "yes" && urgent != "no" {
		return errors.New("请选择是否紧急")
	}
	reason := formValue(r, "change_urgent_reason")
	if !ticketOptionalText(reason, 500) || (urgent == "yes" && reason == "") {
		return errors.New("紧急变更请填写原因，且不要包含密钥")
	}
	details["change_urgent"], details["change_urgent_reason"] = urgent, reason
	note := formValue(r, "change_application_note")
	if !validTicketText(note, 2000) {
		return errors.New("请填写申请说明，且不要包含密钥")
	}
	details["change_application_note"] = note
	rows := make([]map[string]string, 0, 10)
	for i := 1; i <= 10; i++ {
		index := strconv.Itoa(i)
		var row map[string]string
		switch changeType {
		case "personnel":
			name := formValue(r, "change_person_name_"+index)
			email := strings.ToLower(formValue(r, "change_person_email_"+index))
			number := formValue(r, "change_person_number_"+index)
			note := formValue(r, "change_person_note_"+index)
			if name == "" && email == "" && number == "" && note == "" {
				continue
			}
			if !validTicketText(name, 100) || !ticketEmail(email) || !ticketOptionalText(number, 100) || !ticketOptionalText(note, 500) {
				return errors.New("人员变更第 " + index + " 行需要姓名和工作邮箱")
			}
			row = map[string]string{"name": name, "email": email, "number": number, "note": note}
		case "software":
			name := formValue(r, "change_software_name_"+index)
			version := formValue(r, "change_software_version_"+index)
			target := formValue(r, "change_software_target_"+index)
			note := formValue(r, "change_software_note_"+index)
			if name == "" && version == "" && target == "" && note == "" {
				continue
			}
			if !validTicketText(name, 200) || !ticketOptionalText(version, 100) || !ticketOptionalText(target, 300) || !ticketOptionalText(note, 500) {
				return errors.New("软件变更第 " + index + " 行需要软件名称")
			}
			row = map[string]string{"name": name, "min_version": version, "target": target, "note": note}
		case "website":
			name := formValue(r, "change_website_name_"+index)
			target := formValue(r, "change_website_target_"+index)
			cutoff := formValue(r, "change_website_cutoff_"+index)
			note := formValue(r, "change_website_note_"+index)
			if name == "" && target == "" && cutoff == "" && note == "" {
				continue
			}
			if !validTicketText(name, 200) || !ticketOptionalText(target, 300) || !ticketOptionalText(note, 500) || (cutoff != "" && !ticketDate(cutoff)) || (action == "temporary_exception" && cutoff == "") {
				return errors.New("网站变更第 " + index + " 行需要网站名称；临时例外还需截止日期")
			}
			row = map[string]string{"name": name, "target": target, "cutoff": cutoff, "note": note}
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return errors.New("请至少填写一个变更对象")
	}
	details["change_items"] = rows
	for _, field := range []string{"change_contact_name", "change_contact_signature"} {
		value := formValue(r, field)
		if !ticketOptionalText(value, 100) {
			return errors.New("客户联系人或签名信息无效")
		}
		details[field] = value
	}
	if confirmed := formValue(r, "change_contact_date"); confirmed != "" {
		if !ticketDate(confirmed) {
			return errors.New("客户填写日期无效")
		}
		details["change_contact_date"] = confirmed
	}
	return nil
}

func ticketChangeStageLabel(stage string) string {
	labels := map[string]string{
		"need_info": "待补信息", "assessing": "评估中", "waiting_customer_approval": "待客户确认", "scheduled": "已排期", "executing": "执行中", "verifying": "待验证", "waiting_customer_result": "待客户确认结果", "closed": "已关闭", "cancelled": "已取消", "not_executing": "不执行",
	}
	if label, ok := labels[stage]; ok {
		return label
	}
	return stage
}

func ticketChangeTypeLabel(value string) string {
	switch value {
	case "personnel":
		return "人员"
	case "software":
		return "软件"
	case "website":
		return "网站封禁／放行"
	}
	return value
}

func ticketChangeActionLabel(value string) string {
	switch value {
	case "add":
		return "新增"
	case "delete":
		return "删除"
	case "modify":
		return "修改"
	case "temporary_exception":
		return "临时例外"
	}
	return value
}

func ticketConfirmationLabel(value string) string {
	switch value {
	case "completed":
		return "已按申请内容完成"
	case "partial":
		return "部分完成，遗留问题见实施记录"
	case "not_completed":
		return "未完成，需重新排期"
	}
	return value
}

func ticketField(values map[string]any, key string) string {
	if value, ok := values[key].(string); ok {
		return value
	}
	return ""
}

func parseChangeImplementation(r *http.Request) (map[string]string, error) {
	stage := formValue(r, "result_stage")
	if ticketChangeStageLabel(stage) == stage {
		return nil, errors.New("请选择有效的实施状态")
	}
	fields := map[string]int{
		"result_missing": 2000, "result_impact": 2000, "result_executor": 100, "result_verifier": 100,
		"result_rollback": 2000, "result_execution": 2000, "result_test_evidence": 2000,
		"result_actual_scope": 1000, "result_followup": 1000, "result_lead": 100,
	}
	result := map[string]string{"stage": stage}
	for field, max := range fields {
		value := formValue(r, field)
		if !ticketOptionalText(value, max) {
			return nil, errors.New("实施记录过长或包含疑似密钥")
		}
		result[field] = value
	}
	if planned := formValue(r, "result_planned_at"); planned != "" {
		if _, err := time.Parse("2006-01-02T15:04", planned); err != nil {
			return nil, errors.New("计划执行时间无效")
		}
		result["result_planned_at"] = planned
	}
	if date := formValue(r, "result_date"); date != "" {
		if !ticketDate(date) {
			return nil, errors.New("实施日期无效")
		}
		result["result_date"] = date
	}
	if stage == "waiting_customer_result" && result["result_execution"] == "" {
		return nil, errors.New("待客户确认结果时，请先填写执行结果")
	}
	return result, nil
}

func parseChangeConfirmation(r *http.Request) (map[string]string, error) {
	result := formValue(r, "confirmation_result")
	if result != "completed" && result != "partial" && result != "not_completed" {
		return nil, errors.New("请选择客户确认结果")
	}
	name := formValue(r, "confirmation_contact")
	signature := formValue(r, "confirmation_signature")
	date := formValue(r, "confirmation_date")
	if !validTicketText(name, 100) || !validTicketText(signature, 100) || !ticketDate(date) {
		return nil, errors.New("请填写联系人、签名和有效确认日期")
	}
	return map[string]string{"result": result, "contact": name, "signature": signature, "date": date}, nil
}
