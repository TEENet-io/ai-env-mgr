package worker

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/notify"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// TaskTicketNotify tells our group what customers did on their tickets, and
// tells customers what we did on theirs.
const (
	TaskTicketNotify  = "ticket_notify"
	EveryTicketNotify = time.Minute
)

// Scenarios: what a notice is about. Team ones are about the customer's
// action, customer ones about ours.
const (
	ScenarioNewTicket       = "new_ticket"
	ScenarioCustomerReply   = "customer_reply"
	ScenarioPlanAccepted    = "plan_accepted"
	ScenarioPlanRejected    = "plan_rejected"
	ScenarioChangeAccepted  = "change_accepted"
	ScenarioResolved        = "resolved"
	ScenarioNotResolved     = "not_resolved"
	ScenarioWithdrawn       = "withdrawn"
	ScenarioResubmitted     = "resubmitted"
	ScenarioCancelRequested = "cancel_requested"

	ScenarioTeamReply         = "team_reply"
	ScenarioNeedInfo          = "need_info"
	ScenarioConfirmPlan       = "confirm_plan"
	ScenarioPlanAwaitsManager = "plan_awaits_manager"
	ScenarioConfirmChange     = "confirm_change"
	ScenarioDone              = "done"
	ScenarioClosed            = "closed"
)

// A customer's mail waits until a ticket has been quiet for MailQuiet, so a
// reply and the status change after it are one mail, but never longer than
// MailHold after the first thing waiting.
const (
	MailQuiet = 2 * time.Minute
	MailHold  = 5 * time.Minute
	// noticeLease is how long a run holds what it is sending; a run that
	// died mid-send leaves it to the next one after this.
	noticeLease = 5 * time.Minute
)

// TicketNotify runs every minute. Each event is decided once per audience;
// what is pending is then taken under a lease, sent, and on failure retried
// by a later run.
type TicketNotify struct {
	Notices repo.TicketNotices
	// Team is the group's channel, read on every run; nil when none is set.
	Team func(ctx context.Context) (notify.Channel, error)
	// Customer is a mail channel to one address, or nil when no mail relay
	// is configured; then customers are not mailed and nothing piles up.
	Customer func(ctx context.Context, to string) (notify.Channel, error)
	BaseURL  string // the console's public address, for links
	Company  string // shown in titles; may be empty
	MaxTries int    // default 10
	Now      func() time.Time
}

func (h TicketNotify) Run(ctx context.Context, _ repo.Task) (Result, error) {
	maxTries := h.MaxTries
	if maxTries <= 0 {
		maxTries = 10
	}
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	undecided, err := h.Notices.UndecidedTicketEvents(ctx, 200)
	if err != nil {
		return Result{}, err
	}
	var decisions []repo.TicketNoticeDecision
	for _, e := range undecided {
		team, customer := ClassifyTicketEvent(e)
		decisions = append(decisions,
			repo.TicketNoticeDecision{EventID: e.EventID, Audience: repo.NoticeTeam, Scenario: team},
			repo.TicketNoticeDecision{EventID: e.EventID, Audience: repo.NoticeCustomer, Scenario: customer})
	}
	if err := h.Notices.DecideTicketNotices(ctx, decisions); err != nil {
		return Result{}, err
	}
	// The two directions do not hold each other up: a broken robot must
	// not stop customers' mail, nor the other way round.
	team := h.sendTeam(ctx, maxTries)
	customer := h.sendCustomers(ctx, now, maxTries)
	note := fmt.Sprintf("decided %d event(s); team %s; customer %s", len(undecided), team, customer)
	if team.err != nil && customer.err != nil {
		return Result{}, fmt.Errorf("%s", note)
	}
	return Result{Note: note}, nil
}

type tally struct {
	sent, failed, skipped int
	err                   error
}

func (t tally) String() string {
	s := fmt.Sprintf("%d sent %d failed %d skipped", t.sent, t.failed, t.skipped)
	if t.err != nil {
		s += " (" + t.err.Error() + ")"
	}
	return s
}

// ClassifyTicketEvent decides what the event means to each audience; an
// empty scenario is "nothing to say". Internal notes, assignments and links
// never leave the console. Buttons are told apart by the action the console
// recorded for them, never by the words of a comment.
func ClassifyTicketEvent(e repo.TicketNoticeEvent) (team, customer string) {
	if e.Visibility != "public" {
		return "", ""
	}
	changed := e.ToStatus != "" && e.ToStatus != e.FromStatus
	switch e.ActorType {
	case "customer":
		switch {
		case e.Kind == "created":
			return ScenarioNewTicket, ""
		case e.Action == "plan_accepted":
			return ScenarioPlanAccepted, ""
		case e.Action == "plan_rejected":
			return ScenarioPlanRejected, ""
		case e.Action == "change_confirmed":
			return ScenarioChangeAccepted, ""
		case e.Action == "request_revised":
			return ScenarioResubmitted, ""
		case e.Action == "cancel_requested":
			return ScenarioCancelRequested, ""
		case changed && e.FromStatus == "done" && e.ToStatus == "new":
			return ScenarioNotResolved, ""
		case changed && e.FromStatus == "done" && e.ToStatus == "closed":
			return ScenarioResolved, ""
		case changed && e.ToStatus == "closed":
			return ScenarioWithdrawn, ""
		case e.Kind == "comment":
			return ScenarioCustomerReply, ""
		}
	case "admin":
		switch {
		case e.Action == "plan_sent":
			return "", ScenarioConfirmPlan
		case e.Action == "plan_sent_manager":
			return "", ScenarioPlanAwaitsManager
		case e.Action == "change_result_ready":
			return "", ScenarioConfirmChange
		case e.Kind != "comment" && e.Kind != "status":
			return "", ""
		case changed && e.ToStatus == "waiting_customer":
			return "", ScenarioNeedInfo
		case changed && e.ToStatus == "done":
			return "", ScenarioDone
		case changed && e.ToStatus == "closed":
			return "", ScenarioClosed
		case e.Kind == "comment":
			return "", ScenarioTeamReply
		}
	}
	return "", ""
}

func (h TicketNotify) sendTeam(ctx context.Context, maxTries int) (t tally) {
	pending, err := h.Notices.PendingTicketNotices(ctx, repo.NoticeTeam, 50)
	if err != nil || len(pending) == 0 {
		t.err = err
		return t
	}
	var channel notify.Channel
	if h.Team != nil {
		if channel, err = h.Team(ctx); err != nil {
			t.err = err
			return t
		}
	}
	if channel == nil {
		// No group configured: what is waiting is let go, so a group set
		// up next month is not flooded with this month.
		ids := eventIDs(pending)
		t.skipped = len(ids)
		t.err = h.Notices.SkipTicketNotices(ctx, repo.NoticeTeam, ids, "no group channel configured")
		return t
	}
	for _, e := range pending {
		got, err := h.Notices.ClaimTicketNotices(ctx, repo.NoticeTeam, []int64{e.EventID}, noticeLease)
		if err != nil {
			t.err = err
			return t
		}
		if len(got) == 0 {
			continue // another run has it
		}
		sendErr := channel.Send(ctx, h.TeamMessage(e))
		errText := ""
		if sendErr != nil {
			errText = sendErr.Error()
			t.failed++
		} else {
			t.sent++
		}
		if err := h.Notices.FinishTicketNotices(ctx, repo.NoticeTeam, got, sendErr == nil, errText, maxTries); err != nil {
			t.err = err
			return t
		}
	}
	return t
}

func (h TicketNotify) sendCustomers(ctx context.Context, now time.Time, maxTries int) (t tally) {
	pending, err := h.Notices.PendingTicketNotices(ctx, repo.NoticeCustomer, 200)
	if err != nil || len(pending) == 0 {
		t.err = err
		return t
	}
	byTicket := map[string][]repo.TicketNoticeEvent{}
	var order []string
	for _, e := range pending {
		if _, seen := byTicket[e.TicketID]; !seen {
			order = append(order, e.TicketID)
		}
		byTicket[e.TicketID] = append(byTicket[e.TicketID], e)
	}
	for _, ticketID := range order {
		events := byTicket[ticketID]
		first, latest := events[0], events[len(events)-1]
		if reason := h.cannotMail(latest); reason != "" {
			ids := eventIDs(events)
			t.skipped += len(ids)
			if err := h.Notices.SkipTicketNotices(ctx, repo.NoticeCustomer, ids, reason); err != nil {
				t.err = err
			}
			continue
		}
		if now.Sub(latest.At) < MailQuiet && now.Sub(first.At) < MailHold {
			continue // more may follow; wait to send one mail
		}
		mailer, err := h.Customer(ctx, latest.RequesterEmail)
		if err != nil {
			t.err = err
			continue
		}
		if mailer == nil {
			ids := eventIDs(events)
			t.skipped += len(ids)
			if err := h.Notices.SkipTicketNotices(ctx, repo.NoticeCustomer, ids, "mail is not configured"); err != nil {
				t.err = err
			}
			continue
		}
		got, err := h.Notices.ClaimTicketNotices(ctx, repo.NoticeCustomer, eventIDs(events), noticeLease)
		if err != nil {
			t.err = err
			continue
		}
		if len(got) == 0 {
			continue
		}
		claimed := keep(events, got)
		sendErr := mailer.Send(ctx, h.CustomerMessage(claimed))
		errText := ""
		if sendErr != nil {
			errText = sendErr.Error()
			t.failed++
		} else {
			t.sent++
		}
		if err := h.Notices.FinishTicketNotices(ctx, repo.NoticeCustomer, got, sendErr == nil, errText, maxTries); err != nil {
			t.err = err
		}
	}
	return t
}

// cannotMail is why the ticket's requester cannot be mailed, or "".
func (h TicketNotify) cannotMail(e repo.TicketNoticeEvent) string {
	switch {
	case h.Customer == nil:
		return "mail is not configured"
	case !strings.HasPrefix(h.BaseURL, "https://"):
		return "the console has no public https address for links"
	case e.RequesterEmail == "":
		return "the requester has no e-mail address"
	case !e.RequesterActive:
		return "the requester can no longer log in"
	}
	return ""
}

func eventIDs(events []repo.TicketNoticeEvent) []int64 {
	ids := make([]int64, len(events))
	for i, e := range events {
		ids[i] = e.EventID
	}
	return ids
}

func keep(events []repo.TicketNoticeEvent, ids []int64) []repo.TicketNoticeEvent {
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []repo.TicketNoticeEvent
	for _, e := range events {
		if want[e.EventID] {
			out = append(out, e)
		}
	}
	return out
}

type customerMail struct{ subject, lead string }

var customerMails = map[string]customerMail{
	ScenarioDone:              {"工單 #%d 已處理完成，請確認", "您的工單「%s」已處理完成，請登入確認結果："},
	ScenarioClosed:            {"工單 #%d 已關閉", "您的工單「%s」已關閉，詳情請登入查看："},
	ScenarioConfirmPlan:       {"工單 #%d 請確認處理方案", "我們已為您的工單「%s」提出處理方案，確認後才會執行，請登入查看："},
	ScenarioPlanAwaitsManager: {"工單 #%d 的處理方案待貴司管理人確認", "我們已為您的工單「%s」提出處理方案，正等待貴司帳號管理聯絡人確認，確認後才會執行。詳情請登入查看："},
	ScenarioConfirmChange:     {"工單 #%d 請確認變更結果", "您的工單「%s」的變更已實施，請登入確認結果："},
	ScenarioNeedInfo:          {"工單 #%d 需要您補充資料", "處理您的工單「%s」需要更多資料，請登入補充："},
	ScenarioTeamReply:         {"工單 #%d 有新回覆", "您的工單「%s」有新回覆，請登入查看："},
}

// mailRank: when several things happened since the last mail, the mail is
// about the weightiest. Among events of one rank the latest wins, so a
// ticket that was done and then closed says it is closed.
var mailRank = map[string]int{
	ScenarioDone: 3, ScenarioClosed: 3,
	ScenarioConfirmPlan: 2, ScenarioPlanAwaitsManager: 2, ScenarioConfirmChange: 2, ScenarioNeedInfo: 2,
	ScenarioTeamReply: 1,
}

// CustomerMessage is one mail about everything waiting for one ticket, in
// traditional Chinese like the portal. It says that something happened and
// where to look; what was said stays behind the login.
func (h TicketNotify) CustomerMessage(events []repo.TicketNoticeEvent) notify.Message {
	pick := ScenarioTeamReply
	for _, e := range events {
		if mailRank[e.Scenario] >= mailRank[pick] {
			pick = e.Scenario
		}
	}
	mail, ok := customerMails[pick]
	if !ok {
		mail = customerMails[ScenarioTeamReply]
	}
	e := events[len(events)-1]
	prefix := "AI 工作間"
	if h.Company != "" {
		prefix = h.Company + " AI 工作間"
	}
	link := strings.TrimSuffix(h.BaseURL, "/") + "/customer/ticket?id=" + url.QueryEscape(e.TicketID)
	name := e.RequesterName
	if name == "" {
		name = "客戶"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s 您好：\n\n", name)
	fmt.Fprintf(&b, mail.lead+"\n%s\n\n", e.Title, link)
	b.WriteString("此郵件由系統自動發送，請勿直接回覆。\nTEENet 運維團隊\n")
	subject := fmt.Sprintf("[%s] "+mail.subject, prefix, e.Number)
	return notify.Message{Title: subject, MailSubject: subject, MailBody: b.String(), URL: link, At: e.At}
}

var hongKong = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Hong_Kong"); err == nil {
		return loc
	}
	return time.FixedZone("HKT", 8*3600)
}()
