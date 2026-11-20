package repo

import (
	"context"
	"time"
)

// Who hears about a ticket event.
const (
	NoticeTeam     = "team"     // our chat group: the customer did something
	NoticeCustomer = "customer" // the person who raised the ticket: we did something
)

// TicketNoticeEvent is one ticket event with what a message about it needs:
// the ticket's number, title and kind, and who raised it. Body is here only
// for completeness; messages never carry it, since they leave the console.
type TicketNoticeEvent struct {
	EventID                                      int64
	TicketID, Title, TypeLabel, Status, Priority string
	// TypeKey is the ticket's kind as a stable key: the request kind of a
	// daily request ("account-add"), else "type" or "type:subtype".
	TypeKey                       string
	Number                        int64
	RequesterName, RequesterEmail string
	RequesterActive               bool // a contact who can still log in

	ActorType, ActorName, Kind, Visibility, Body string
	// Action names what a system-written event stands for ("plan_accepted");
	// empty for an ordinary comment or status change.
	Action               string
	FromStatus, ToStatus string
	At                   time.Time

	// Set on pending notices: what was decided, and how often sending failed.
	Scenario string
	Tries    int
}

// TicketNoticeDecision records whether an audience is told about an event.
// An empty Scenario means not: the event is marked skipped.
type TicketNoticeDecision struct {
	EventID            int64
	Audience, Scenario string
}

// TicketNotices is the delivery record beside the append-only ticket events.
type TicketNotices interface {
	// UndecidedTicketEvents lists recent events nobody has decided on yet.
	UndecidedTicketEvents(ctx context.Context, limit int) ([]TicketNoticeEvent, error)
	DecideTicketNotices(ctx context.Context, decisions []TicketNoticeDecision) error
	// PendingTicketNotices lists what still has to go to one audience and is
	// not held by another run, oldest first.
	PendingTicketNotices(ctx context.Context, audience string, limit int) ([]TicketNoticeEvent, error)
	// ClaimTicketNotices takes the notices for one run until the lease ends
	// and returns those it got: another run may have taken some first.
	ClaimTicketNotices(ctx context.Context, audience string, eventIDs []int64, lease time.Duration) ([]int64, error)
	// FinishTicketNotices records one attempt on claimed notices: sent, or
	// failed with errText. One that has failed maxTries times gives up.
	FinishTicketNotices(ctx context.Context, audience string, eventIDs []int64, sent bool, errText string, maxTries int) error
	// SkipTicketNotices lets notices go unsent, with the reason.
	SkipTicketNotices(ctx context.Context, audience string, eventIDs []int64, reason string) error
}
