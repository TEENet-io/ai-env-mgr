package dbstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

var _ repo.TicketNotices = TicketStore{}

// noticeWindow bounds how far back an undecided event is still news. A
// console that was down for longer does not wake the group with old
// tickets; they are on the queue page anyway.
const noticeWindow = 24 * time.Hour

const noticeEventColumns = `e.id,e.ticket_id::text,e.actor_type,
	coalesce(nullif(e.actor_name,''),c.name,a.username,''),e.kind,e.visibility,e.body,e.action,
	coalesce(e.from_status,''),coalesce(e.to_status,''),e.created_at`

const noticeEventJoins = ` from ticket_events e
	left join customer_contacts c on e.actor_type='customer' and e.actor_id=c.id
	left join admin_principals a on e.actor_type='admin' and e.actor_id=a.id`

func (s TicketStore) UndecidedTicketEvents(ctx context.Context, limit int) ([]repo.TicketNoticeEvent, error) {
	rows, err := s.db.pool.Query(ctx, `select `+noticeEventColumns+`,'',0`+noticeEventJoins+`
		where e.created_at > $1 and not exists (select 1 from ticket_notices n where n.event_id=e.id)
		order by e.id limit $2`, time.Now().Add(-noticeWindow), limit)
	if err != nil {
		return nil, err
	}
	return s.noticeEvents(ctx, rows)
}

func (s TicketStore) PendingTicketNotices(ctx context.Context, audience string, limit int) ([]repo.TicketNoticeEvent, error) {
	rows, err := s.db.pool.Query(ctx, `select `+noticeEventColumns+`,n.scenario,n.tries`+noticeEventJoins+`
		join ticket_notices n on n.event_id=e.id and n.audience=$1
		where n.state='pending' and (n.lease_until is null or n.lease_until<now())
		order by e.id limit $2`, audience, limit)
	if err != nil {
		return nil, err
	}
	return s.noticeEvents(ctx, rows)
}

// noticeEvents reads the events, then fills in each one's ticket. The
// volume is a handful a minute, so one lookup per event is fine.
func (s TicketStore) noticeEvents(ctx context.Context, rows pgx.Rows) ([]repo.TicketNoticeEvent, error) {
	var out []repo.TicketNoticeEvent
	for rows.Next() {
		var e repo.TicketNoticeEvent
		if err := rows.Scan(&e.EventID, &e.TicketID, &e.ActorType, &e.ActorName, &e.Kind, &e.Visibility, &e.Body, &e.Action,
			&e.FromStatus, &e.ToStatus, &e.At, &e.Scenario, &e.Tries); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		t, err := s.Get(ctx, out[i].TicketID, "")
		if err != nil {
			return nil, err
		}
		out[i].Number, out[i].Title, out[i].TypeLabel = t.Number, t.Title, t.TypeLabel()
		out[i].Status, out[i].Priority, out[i].RequesterName = t.Status, t.Priority, t.RequesterName
		out[i].TypeKey = ticketTypeKey(t)
		if t.RequesterID == "" {
			continue
		}
		var disabled *time.Time
		err = s.db.pool.QueryRow(ctx, `select email,disabled_at from customer_contacts where id=$1`, t.RequesterID).
			Scan(&out[i].RequesterEmail, &disabled)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		out[i].RequesterActive = err == nil && disabled == nil
	}
	return out, nil
}

func ticketTypeKey(t Ticket) string {
	if kind, ok := t.Details["request_kind"].(string); ok && kind != "" {
		return kind
	}
	if sub, ok := t.Details["subtype"].(string); ok && sub != "" {
		return t.Type + ":" + sub
	}
	return t.Type
}

func (s TicketStore) DecideTicketNotices(ctx context.Context, decisions []repo.TicketNoticeDecision) error {
	if len(decisions) == 0 {
		return nil
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		for _, d := range decisions {
			state := "pending"
			if d.Scenario == "" {
				state = "skipped"
			}
			if _, err := tx.Exec(ctx, `insert into ticket_notices(event_id,audience,scenario,state)
				values($1,$2,$3,$4) on conflict do nothing`, d.EventID, d.Audience, d.Scenario, state); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s TicketStore) ClaimTicketNotices(ctx context.Context, audience string, eventIDs []int64, lease time.Duration) ([]int64, error) {
	if len(eventIDs) == 0 {
		return nil, nil
	}
	rows, err := s.db.pool.Query(ctx, `update ticket_notices set lease_until=now()+$3::interval
		where audience=$1 and event_id=any($2) and state='pending' and (lease_until is null or lease_until<now())
		returning event_id`, audience, eventIDs, fmt.Sprintf("%d milliseconds", lease.Milliseconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		got = append(got, id)
	}
	return got, rows.Err()
}

func (s TicketStore) FinishTicketNotices(ctx context.Context, audience string, eventIDs []int64, sent bool, errText string, maxTries int) error {
	if len(eventIDs) == 0 {
		return nil
	}
	if sent {
		_, err := s.db.pool.Exec(ctx, `update ticket_notices set state='sent',sent_at=now(),tries=tries+1,last_error='',lease_until=null
			where audience=$1 and event_id=any($2) and state='pending'`, audience, eventIDs)
		return err
	}
	_, err := s.db.pool.Exec(ctx, `update ticket_notices set tries=tries+1,last_error=$3,lease_until=null,
		state=case when tries+1>=$4 then 'failed' else 'pending' end
		where audience=$1 and event_id=any($2) and state='pending'`, audience, eventIDs, errText, maxTries)
	return err
}

func (s TicketStore) SkipTicketNotices(ctx context.Context, audience string, eventIDs []int64, reason string) error {
	if len(eventIDs) == 0 {
		return nil
	}
	_, err := s.db.pool.Exec(ctx, `update ticket_notices set state='skipped',last_error=$3,lease_until=null
		where audience=$1 and event_id=any($2) and state='pending'`, audience, eventIDs, reason)
	return err
}
