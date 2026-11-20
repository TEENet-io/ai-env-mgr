package dbstore

import (
	"context"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

func TestTicketNoticesDecideClaimSendRetry(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s := db.Tickets()
	c, err := s.CreateContact(ctx, "amy@example.com", "Amy", "hash")
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := s.Create(ctx, c.ID, "notice-1", "onboarding", "New hire Ben", "secret body", "urgent", []byte(`{"schema_version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	undecided, err := s.UndecidedTicketEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(undecided) != 1 {
		t.Fatalf("undecided = %d, want the creation", len(undecided))
	}
	e := undecided[0]
	if e.Kind != "created" || e.ActorType != "customer" || e.Number != ticket.Number || e.Title != "New hire Ben" || e.TypeKey != "onboarding" ||
		e.RequesterEmail != "amy@example.com" || !e.RequesterActive || e.ActorName != "Amy" || e.Priority != "urgent" {
		t.Fatalf("event = %+v", e)
	}
	if err := s.DecideTicketNotices(ctx, []repo.TicketNoticeDecision{
		{EventID: e.EventID, Audience: repo.NoticeTeam, Scenario: "new_ticket"},
		{EventID: e.EventID, Audience: repo.NoticeCustomer},
	}); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.UndecidedTicketEvents(ctx, 10); len(again) != 0 {
		t.Fatalf("decided event still undecided: %d", len(again))
	}
	if pending, _ := s.PendingTicketNotices(ctx, repo.NoticeCustomer, 10); len(pending) != 0 {
		t.Fatal("a skipped notice is pending")
	}
	pending, err := s.PendingTicketNotices(ctx, repo.NoticeTeam, 10)
	if err != nil || len(pending) != 1 || pending[0].Scenario != "new_ticket" {
		t.Fatalf("pending = %+v, %v", pending, err)
	}

	// One run takes it; a second run at the same time gets nothing and does
	// not see it as pending while the lease holds.
	ids := []int64{e.EventID}
	if got, err := s.ClaimTicketNotices(ctx, repo.NoticeTeam, ids, time.Minute); err != nil || len(got) != 1 {
		t.Fatalf("first claim = %v, %v", got, err)
	}
	if got, _ := s.ClaimTicketNotices(ctx, repo.NoticeTeam, ids, time.Minute); len(got) != 0 {
		t.Fatal("a held notice was taken twice")
	}
	if pending, _ := s.PendingTicketNotices(ctx, repo.NoticeTeam, 10); len(pending) != 0 {
		t.Fatal("a held notice is listed as pending")
	}
	// A run that died: the lease runs out and the notice is free again.
	if _, err := db.Pool().Exec(ctx, `update ticket_notices set lease_until=now()-interval '1 second' where event_id=$1`, e.EventID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ClaimTicketNotices(ctx, repo.NoticeTeam, ids, time.Minute); len(got) != 1 {
		t.Fatal("an expired lease was not taken over")
	}

	// Two refusals with a limit of two: the notice gives up.
	for i := 0; i < 2; i++ {
		if err := s.FinishTicketNotices(ctx, repo.NoticeTeam, ids, false, "robot down", 2); err != nil {
			t.Fatal(err)
		}
	}
	if pending, _ := s.PendingTicketNotices(ctx, repo.NoticeTeam, 10); len(pending) != 0 {
		t.Fatal("notice still pending after its last try")
	}
	var state, lastErr string
	if err := db.Pool().QueryRow(ctx, `select state,last_error from ticket_notices where event_id=$1 and audience='team'`, e.EventID).Scan(&state, &lastErr); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || lastErr != "robot down" {
		t.Fatalf("state = %s %q", state, lastErr)
	}

	// A plan from us carries its action; skipping it records why.
	var adminID string
	if err := db.Pool().QueryRow(ctx, `insert into admin_principals(username,password_hash,role) values('notice_operator','hash','admin') returning id`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddEventAction(ctx, ticket.ID, "", "admin", adminID, "comment", "public", "we are on it", "working", "", 1); err != nil {
		t.Fatal(err)
	}
	undecided, _ = s.UndecidedTicketEvents(ctx, 10)
	if len(undecided) != 1 || undecided[0].ActorType != "admin" || undecided[0].ToStatus != "working" || undecided[0].Action != "" {
		t.Fatalf("reply = %+v", undecided)
	}
	reply := undecided[0].EventID
	_ = s.DecideTicketNotices(ctx, []repo.TicketNoticeDecision{
		{EventID: reply, Audience: repo.NoticeTeam}, {EventID: reply, Audience: repo.NoticeCustomer, Scenario: "team_reply"}})
	if err := s.SkipTicketNotices(ctx, repo.NoticeCustomer, []int64{reply}, "mail is not configured"); err != nil {
		t.Fatal(err)
	}
	var sentAt *time.Time
	if err := db.Pool().QueryRow(ctx, `select state,last_error,sent_at from ticket_notices where event_id=$1 and audience='customer'`, reply).Scan(&state, &lastErr, &sentAt); err != nil {
		t.Fatal(err)
	}
	if state != "skipped" || lastErr != "mail is not configured" || sentAt != nil {
		t.Fatalf("skipped notice = %s %q %v", state, lastErr, sentAt)
	}
}

func TestProposalAndRevisionEventsCarryTheirAction(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	s := db.Tickets()
	c, err := s.CreateContact(ctx, "quota@example.com", "Quota", "hash")
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := s.Create(ctx, c.ID, "notice-plan", "access", "More quota", "please", "normal", []byte(`{"schema_version":1,"request_kind":"quota"}`))
	if err != nil {
		t.Fatal(err)
	}
	var adminID string
	if err := db.Pool().QueryRow(ctx, `insert into admin_principals(username,password_hash,role) values('plan_operator','hash','admin') returning id`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProposal(ctx, ticket.ID, adminID, 1, "quota", map[string]string{"amount": "10"}, false); err != nil {
		t.Fatal(err)
	}
	// A customer typing the system's sentence is still an ordinary comment.
	if err := s.AddEvent(ctx, ticket.ID, c.ID, "customer", c.ID, "comment", "public", "客戶已確認具體方案；服務團隊可以繼續處理。", "working", 2); err != nil {
		t.Fatal(err)
	}
	events, err := s.UndecidedTicketEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, e := range events {
		actions[e.ActorType+":"+e.Action] = true
	}
	if !actions["admin:plan_sent"] || !actions["customer:"] || actions["customer:plan_accepted"] {
		t.Fatalf("actions = %v", actions)
	}
}

func TestTicketNoticesUpgradeLeavesHistoryAlone(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var writable bool
	if err := db.Pool().QueryRow(ctx, `select has_table_privilege('aienv_app','ticket_notices','UPDATE')`).Scan(&writable); err != nil {
		t.Fatal(err)
	}
	if !writable {
		t.Fatal("the console cannot record deliveries")
	}
	// Back to the schema before notices, with tickets that already happened.
	if err := db.MigrateDown(ctx); err != nil {
		t.Fatal(err)
	}
	s := db.Tickets()
	c, err := s.CreateContact(ctx, "old@example.com", "Old", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `insert into tickets(requester_id,request_key,type,title,description,priority,details)
		values($1,'old-1','other','Old ticket','old','normal','{}')`, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, `insert into ticket_events(ticket_id,actor_type,actor_id,kind,visibility)
		select id,'customer',$1,'created','public' from tickets where request_key='old-1'`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var rows, skipped int
	if err := db.Pool().QueryRow(ctx, `select count(*),count(*) filter (where state='skipped') from ticket_notices`).Scan(&rows, &skipped); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || skipped != 2 {
		t.Fatalf("history rows = %d, skipped = %d; want both audiences skipped", rows, skipped)
	}
	if undecided, _ := s.UndecidedTicketEvents(ctx, 10); len(undecided) != 0 {
		t.Fatal("an old event is news after the upgrade")
	}
	// What happens after the upgrade is news.
	if _, err := s.Create(ctx, c.ID, "new-1", "other", "New ticket", "new", "normal", []byte(`{"schema_version":1}`)); err != nil {
		t.Fatal(err)
	}
	if undecided, _ := s.UndecidedTicketEvents(ctx, 10); len(undecided) != 1 || undecided[0].Title != "New ticket" {
		t.Fatalf("new event after upgrade = %+v", undecided)
	}
}
