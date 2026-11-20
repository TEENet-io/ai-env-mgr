package worker

// The group's cards are in simplified Chinese: the group is our own people,
// and that is what they read. Everything a customer sees stays traditional;
// this file is the one exception the console's text check allows.

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/notify"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// teamTitles is what each customer action is called in the group.
var teamTitles = map[string]struct{ icon, text, color string }{
	ScenarioNewTicket:       {"🆕", "新工单", "blue"},
	ScenarioCustomerReply:   {"💬", "客户回复", "orange"},
	ScenarioResubmitted:     {"📝", "客户重新提交申请", "orange"},
	ScenarioPlanAccepted:    {"✅", "客户已确认方案", "green"},
	ScenarioPlanRejected:    {"↩️", "客户拒绝方案", "orange"},
	ScenarioChangeAccepted:  {"✅", "客户已确认变更结果", "green"},
	ScenarioResolved:        {"✅", "客户确认已解决", "green"},
	ScenarioNotResolved:     {"⚠️", "客户反馈未解决", "red"},
	ScenarioWithdrawn:       {"🗑️", "客户撤回工单", "grey"},
	ScenarioCancelRequested: {"🛑", "客户申请撤销", "red"},
}

// teamTypes is each ticket kind's name in simplified Chinese, keyed like
// TicketNoticeEvent.TypeKey; a kind not listed shows the portal's name.
var teamTypes = map[string]string{
	"account-add": "AI 工作间账号 · 新增", "account-toggle": "AI 工作间账号 · 停用或恢复",
	"account-delete": "AI 工作间账号 · 离职／删除", "account-edit": "AI 工作间账号 · 修改信息",
	"desktop-problem": "远程桌面 · 使用问题", "software-add": "远程桌面 · 安装软件",
	"software-change": "远程桌面 · 更新或卸载", "website": "远程桌面 · 网站访问",
	"ai-problem": "AI 模型与使用额度 · 使用问题", "quota": "AI 模型与使用额度 · 额度追加",
	"tool-problem": "定制工具 · 使用问题", "other-problem": "其他问题",
	"general": "一般事务", "general:employee": "一般事务 · 员工操作", "general:website": "一般事务 · 网站封禁",
	"general:software": "一般事务 · 软件预装", "general:environment": "一般事务 · 客户环境变更",
	"bug": "Bug 提交", "onboarding": "新员工开户", "offboarding": "员工离职",
	"access": "权限或额度调整", "other": "故障与其他",
}

var teamStatus = map[string]string{
	"new": "待受理", "working": "处理中", "waiting_customer": "待客户补充", "done": "已完成", "closed": "已关闭",
}

var teamPriority = map[string]string{"urgent": "紧急", "normal": "一般", "low": "低"}

// TeamMessage is the group's card for one customer action. It names the
// ticket and who acted, never the body: the body may name employees and
// the card leaves for a third-party chat service.
func (h TicketNotify) TeamMessage(e repo.TicketNoticeEvent) notify.Message {
	t, ok := teamTitles[e.Scenario]
	if !ok {
		t = teamTitles[ScenarioCustomerReply]
	}
	title := fmt.Sprintf("%s %s #%d", t.icon, t.text, e.Number)
	if h.Company != "" {
		title += " · " + h.Company
	}
	actor := e.ActorName
	if actor == "" {
		actor = e.RequesterName
	}
	kind := teamTypes[e.TypeKey]
	if kind == "" {
		kind = e.TypeLabel
	}
	fields := []notify.Field{{Label: "标题", Value: e.Title}, {Label: "类型", Value: kind}}
	if e.Scenario == ScenarioNewTicket {
		fields = append(fields, notify.Field{Label: "提交人", Value: actor})
		if p := teamPriority[e.Priority]; p != "" {
			fields = append(fields, notify.Field{Label: "优先级", Value: p})
		}
	} else {
		fields = append(fields, notify.Field{Label: "操作人", Value: actor})
		if s := teamStatus[e.Status]; s != "" {
			fields = append(fields, notify.Field{Label: "当前状态", Value: s})
		}
	}
	fields = append(fields, notify.Field{Label: "时间", Value: e.At.In(hongKong).Format("2006-01-02 15:04")})
	m := notify.Message{Title: title, Fields: fields, Color: t.color, Button: "打开工单", At: e.At}
	if h.BaseURL != "" {
		m.URL = strings.TrimSuffix(h.BaseURL, "/") + "/tickets/detail?id=" + url.QueryEscape(e.TicketID)
	}
	return m
}
