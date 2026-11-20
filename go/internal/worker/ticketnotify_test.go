package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/notify"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestClassifyTicketEvent(t *testing.T) {
	ev := func(actor, kind, vis, action, from, to string) repo.TicketNoticeEvent {
		return repo.TicketNoticeEvent{ActorType: actor, Kind: kind, Visibility: vis, Action: action, FromStatus: from, ToStatus: to}
	}
	cases := []struct {
		name           string
		e              repo.TicketNoticeEvent
		team, customer string
	}{
		{"new ticket", ev("customer", "created", "public", "", "", ""), ScenarioNewTicket, ""},
		{"customer supplies info", ev("customer", "comment", "public", "", "waiting_customer", "working"), ScenarioCustomerReply, ""},
		{"plan accepted", ev("customer", "comment", "public", "plan_accepted", "waiting_customer", "working"), ScenarioPlanAccepted, ""},
		{"plan rejected", ev("customer", "comment", "public", "plan_rejected", "", ""), ScenarioPlanRejected, ""},
		{"change confirmed", ev("customer", "comment", "public", "change_confirmed", "", ""), ScenarioChangeAccepted, ""},
		{"resubmitted", ev("customer", "comment", "public", "request_revised", "", ""), ScenarioResubmitted, ""},
		{"asks to cancel", ev("customer", "comment", "public", "cancel_requested", "working", "working"), ScenarioCancelRequested, ""},
		{"resolved", ev("customer", "status", "public", "", "done", "closed"), ScenarioResolved, ""},
		{"not resolved", ev("customer", "comment", "public", "", "done", "new"), ScenarioNotResolved, ""},
		{"withdrawn", ev("customer", "status", "public", "", "new", "closed"), ScenarioWithdrawn, ""},
		{"our reply", ev("admin", "comment", "public", "", "new", "working"), "", ScenarioTeamReply},
		{"we need info", ev("admin", "comment", "public", "", "working", "waiting_customer"), "", ScenarioNeedInfo},
		{"plan sent", ev("admin", "comment", "public", "plan_sent", "", ""), "", ScenarioConfirmPlan},
		{"plan for a manager", ev("admin", "comment", "public", "plan_sent_manager", "", ""), "", ScenarioPlanAwaitsManager},
		{"change ready", ev("admin", "comment", "public", "change_result_ready", "", ""), "", ScenarioConfirmChange},
		{"done", ev("admin", "status", "public", "", "working", "done"), "", ScenarioDone},
		{"closed by us", ev("admin", "status", "public", "", "working", "closed"), "", ScenarioClosed},
		{"taken without a word", ev("admin", "status", "public", "", "new", "working"), "", ""},
		{"internal note", ev("admin", "comment", "internal", "", "", ""), "", ""},
		{"assignment", ev("admin", "assignment", "internal", "", "", ""), "", ""},
		{"ticket we raised", ev("admin", "created", "public", "", "", ""), "", ""},
	}
	for _, c := range cases {
		team, customer := ClassifyTicketEvent(c.e)
		if team != c.team || customer != c.customer {
			t.Errorf("%s: got (%q,%q), want (%q,%q)", c.name, team, customer, c.team, c.customer)
		}
	}
	// Words alone change nothing: a customer typing the system's sentence
	// is a reply, not an accepted plan.
	typed := ev("customer", "comment", "public", "", "", "")
	typed.Body = "客戶已確認具體方案；服務團隊可以繼續處理。"
	if team, _ := ClassifyTicketEvent(typed); team != ScenarioCustomerReply {
		t.Fatalf("typed sentence classified as %q", team)
	}
}

func TestTicketMessagesCarryNoBody(t *testing.T) {
	h := TicketNotify{BaseURL: "https://console.example/", Company: "Bippo"}
	at := time.Date(2026, 11, 20, 6, 32, 0, 0, time.UTC)
	e := repo.TicketNoticeEvent{EventID: 1, TicketID: "t-1", Number: 1024, Title: "新同事 Amy 開戶", TypeLabel: "AI 工作間帳號 · 新增", TypeKey: "account-add",
		Priority: "normal", ActorName: "陳小姐", Body: "身份證號碼 A123", At: at, Scenario: ScenarioNewTicket}
	m := h.TeamMessage(e)
	if m.Title != "🆕 新工单 #1024 · Bippo" || m.Color != "blue" || m.URL != "https://console.example/tickets/detail?id=t-1" {
		t.Fatalf("team message = %+v", m)
	}
	text := m.Text()
	for _, want := range []string{"标题：新同事 Amy 開戶", "类型：AI 工作间账号 · 新增", "提交人：陳小姐", "优先级：一般", "时间：2026-11-20 14:32"} {
		if !strings.Contains(text, want) {
			t.Errorf("team text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "A123") {
		t.Fatal("the body reached the group")
	}
	e.TypeKey = "something-new"
	if !strings.Contains(h.TeamMessage(e).Text(), "类型：AI 工作間帳號 · 新增") {
		t.Fatal("an unknown kind should fall back to the portal's name")
	}

	e.RequesterName = "陳小姐"
	at2 := func(scenario string, id int64) repo.TicketNoticeEvent {
		c := e
		c.EventID, c.Scenario = id, scenario
		return c
	}
	mail := h.CustomerMessage([]repo.TicketNoticeEvent{at2(ScenarioTeamReply, 1), at2(ScenarioDone, 2)})
	if mail.MailSubject != "[Bippo AI 工作間] 工單 #1024 已處理完成，請確認" {
		t.Fatalf("subject = %q", mail.MailSubject)
	}
	if !strings.HasPrefix(mail.MailBody, "陳小姐 您好：") || !strings.Contains(mail.MailBody, "https://console.example/customer/ticket?id=t-1") ||
		strings.Contains(mail.MailBody, "A123") {
		t.Fatalf("body =\n%s", mail.MailBody)
	}
	// Done and then closed: the mail says closed, not "please confirm".
	if s := h.CustomerMessage([]repo.TicketNoticeEvent{at2(ScenarioDone, 1), at2(ScenarioClosed, 2)}).MailSubject; !strings.Contains(s, "已關閉") {
		t.Fatalf("done then closed = %q", s)
	}
	if s := h.CustomerMessage([]repo.TicketNoticeEvent{at2(ScenarioPlanAwaitsManager, 1)}).MailBody; !strings.Contains(s, "帳號管理聯絡人確認") {
		t.Fatalf("manager plan mail = %q", s)
	}
}

type fakeNotices struct {
	events  []repo.TicketNoticeEvent
	decided map[int64]map[string]string // event -> audience -> scenario
	state   map[string]string           // "audience/id" -> pending|held|sent|failed|skipped
	reason  map[string]string
}

func newFakeNotices(events ...repo.TicketNoticeEvent) *fakeNotices {
	return &fakeNotices{events: events, decided: map[int64]map[string]string{}, state: map[string]string{}, reason: map[string]string{}}
}

func key(id int64, audience string) string { return fmt.Sprintf("%s/%d", audience, id) }

func (f *fakeNotices) UndecidedTicketEvents(_ context.Context, _ int) ([]repo.TicketNoticeEvent, error) {
	var out []repo.TicketNoticeEvent
	for _, e := range f.events {
		if f.decided[e.EventID] == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeNotices) DecideTicketNotices(_ context.Context, ds []repo.TicketNoticeDecision) error {
	for _, d := range ds {
		if f.decided[d.EventID] == nil {
			f.decided[d.EventID] = map[string]string{}
		}
		f.decided[d.EventID][d.Audience] = d.Scenario
		f.state[key(d.EventID, d.Audience)] = map[bool]string{true: "skipped", false: "pending"}[d.Scenario == ""]
	}
	return nil
}

func (f *fakeNotices) PendingTicketNotices(_ context.Context, audience string, _ int) ([]repo.TicketNoticeEvent, error) {
	var out []repo.TicketNoticeEvent
	for _, e := range f.events {
		if f.state[key(e.EventID, audience)] == "pending" {
			e.Scenario = f.decided[e.EventID][audience]
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeNotices) ClaimTicketNotices(_ context.Context, audience string, ids []int64, _ time.Duration) ([]int64, error) {
	var got []int64
	for _, id := range ids {
		if f.state[key(id, audience)] == "pending" {
			f.state[key(id, audience)] = "held"
			got = append(got, id)
		}
	}
	return got, nil
}

func (f *fakeNotices) FinishTicketNotices(_ context.Context, audience string, ids []int64, sent bool, _ string, _ int) error {
	for _, id := range ids {
		if f.state[key(id, audience)] != "held" {
			return fmt.Errorf("finishing %s, which was not claimed", key(id, audience))
		}
		f.state[key(id, audience)] = map[bool]string{true: "sent", false: "pending"}[sent]
	}
	return nil
}

func (f *fakeNotices) SkipTicketNotices(_ context.Context, audience string, ids []int64, reason string) error {
	for _, id := range ids {
		f.state[key(id, audience)] = "skipped"
		f.reason[key(id, audience)] = reason
	}
	return nil
}

type recorder struct {
	sent []notify.Message
	err  error
}

func (r *recorder) Name() string { return "rec" }
func (r *recorder) Send(_ context.Context, m notify.Message) error {
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, m)
	return nil
}

func TestTicketNotifyRun(t *testing.T) {
	start := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	now := start
	base := repo.TicketNoticeEvent{TicketID: "t-1", Number: 7, Title: "Codex 打不開", RequesterEmail: "amy@example.com", RequesterActive: true, Visibility: "public"}
	at := func(e repo.TicketNoticeEvent, id int64, actor, kind, from, to string, when time.Time) repo.TicketNoticeEvent {
		e.EventID, e.ActorType, e.Kind, e.FromStatus, e.ToStatus, e.At = id, actor, kind, from, to, when
		return e
	}
	store := newFakeNotices(at(base, 1, "customer", "created", "", "", start))
	team, mail := &recorder{}, &recorder{}
	var mailedTo []string
	h := TicketNotify{Notices: store, BaseURL: "https://c.example", Now: func() time.Time { return now },
		Team: func(context.Context) (notify.Channel, error) { return team, nil },
		Customer: func(_ context.Context, to string) (notify.Channel, error) {
			mailedTo = append(mailedTo, to)
			return mail, nil
		}}
	run := func() {
		t.Helper()
		if _, err := h.Run(context.Background(), repo.Task{}); err != nil {
			t.Fatal(err)
		}
	}
	run()
	if len(team.sent) != 1 || !strings.Contains(team.sent[0].Title, "新工单 #7") {
		t.Fatalf("team = %+v", team.sent)
	}

	// We reply, and a minute later take the ticket to waiting: one mail,
	// sent once the ticket has been quiet for two minutes.
	now = start.Add(time.Minute)
	store.events = append(store.events, at(base, 2, "admin", "comment", "new", "working", now))
	run()
	now = start.Add(2 * time.Minute)
	store.events = append(store.events, at(base, 3, "admin", "comment", "working", "waiting_customer", now))
	run()
	now = start.Add(3 * time.Minute)
	run()
	if len(mail.sent) != 0 {
		t.Fatal("mailed before the ticket went quiet")
	}
	now = start.Add(4 * time.Minute)
	run()
	if len(mail.sent) != 1 || len(mailedTo) != 1 || mailedTo[0] != "amy@example.com" || !strings.Contains(mail.sent[0].MailSubject, "需要您補充資料") {
		t.Fatalf("mail = %+v to %v", mail.sent, mailedTo)
	}

	// A ticket that never goes quiet is still mailed after five minutes.
	busy := base
	busy.TicketID = "t-2"
	for i := int64(0); i < 6; i++ {
		now = start.Add(time.Duration(10+i) * time.Minute)
		store.events = append(store.events, at(busy, 10+i, "admin", "comment", "working", "working", now))
		run()
	}
	if len(mail.sent) != 2 {
		t.Fatalf("busy ticket mails = %d, want one after the hold", len(mail.sent))
	}

	// A robot that refuses keeps the notice for the next run, and does not
	// hold up the customers' mail.
	team.err = errors.New("down")
	now = start.Add(30 * time.Minute)
	store.events = append(store.events, at(base, 40, "customer", "comment", "waiting_customer", "working", now))
	run()
	if store.state[key(40, repo.NoticeTeam)] != "pending" {
		t.Fatalf("refused notice state = %s", store.state[key(40, repo.NoticeTeam)])
	}
	team.err = nil
	run()
	if len(team.sent) != 2 || !strings.Contains(team.sent[1].Title, "客户回复") {
		t.Fatalf("retried team = %+v", team.sent)
	}
}

func TestTicketNotifySkipsWithReasons(t *testing.T) {
	now := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	reply := repo.TicketNoticeEvent{EventID: 1, TicketID: "t", ActorType: "admin", Kind: "comment", Visibility: "public", RequesterEmail: "a@example.com", RequesterActive: true, At: old}
	created := repo.TicketNoticeEvent{EventID: 2, TicketID: "u", ActorType: "customer", Kind: "created", Visibility: "public", At: old}
	cases := []struct {
		name    string
		h       TicketNotify
		mutate  func(*repo.TicketNoticeEvent)
		reason  string
		teamWhy string
	}{
		{"no relay, no group", TicketNotify{BaseURL: "https://c", Team: func(context.Context) (notify.Channel, error) { return nil, nil },
			Customer: func(context.Context, string) (notify.Channel, error) { return nil, nil }}, nil, "mail is not configured", "no group channel configured"},
		{"no public address", TicketNotify{Customer: func(context.Context, string) (notify.Channel, error) { return &recorder{}, nil }},
			nil, "the console has no public https address for links", "no group channel configured"},
		{"disabled contact", TicketNotify{BaseURL: "https://c", Customer: func(context.Context, string) (notify.Channel, error) { return &recorder{}, nil }},
			func(e *repo.TicketNoticeEvent) { e.RequesterActive = false }, "the requester can no longer log in", "no group channel configured"},
	}
	for _, c := range cases {
		r := reply
		if c.mutate != nil {
			c.mutate(&r)
		}
		store := newFakeNotices(r, created)
		c.h.Notices, c.h.Now = store, func() time.Time { return now }
		if _, err := c.h.Run(context.Background(), repo.Task{}); err != nil {
			t.Fatal(err)
		}
		if got := store.reason[key(1, repo.NoticeCustomer)]; store.state[key(1, repo.NoticeCustomer)] != "skipped" || got != c.reason {
			t.Errorf("%s: customer %s %q, want skipped %q", c.name, store.state[key(1, repo.NoticeCustomer)], got, c.reason)
		}
		if got := store.reason[key(2, repo.NoticeTeam)]; got != c.teamWhy {
			t.Errorf("%s: team reason %q", c.name, got)
		}
	}
}
