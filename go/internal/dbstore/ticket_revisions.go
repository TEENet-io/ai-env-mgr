package dbstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type TicketRequestRevision struct {
	Number      int
	Title       string
	Description string
	CreatedAt   time.Time
}

func (s TicketStore) RequestRevisions(ctx context.Context, ticketID, requesterID string) ([]TicketRequestRevision, error) {
	query := `select r.request_version,r.title,r.description,r.created_at
		from ticket_request_revisions r join tickets t on t.id=r.ticket_id where r.ticket_id=$1`
	args := []any{ticketID}
	if requesterID != "" {
		query += ` and t.requester_id=$2`
		args = append(args, requesterID)
	}
	query += ` order by r.request_version desc`
	rows, err := s.db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var revisions []TicketRequestRevision
	for rows.Next() {
		var revision TicketRequestRevision
		if err := rows.Scan(&revision.Number, &revision.Title, &revision.Description, &revision.CreatedAt); err != nil {
			return nil, err
		}
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

// UpdateDailyRequest keeps the original number and an immutable snapshot of
// every customer-submitted request. Reading or assigning a ticket is not the
// processing boundary; only a transition out of "new" locks its request.
func (s TicketStore) UpdateDailyRequest(ctx context.Context, ticketID, requesterID string, version int, kind, title, description string, details []byte) error {
	var next map[string]any
	if err := json.Unmarshal(details, &next); err != nil {
		return err
	}
	requestKind, ok := next["request_kind"].(string)
	if !ok || requestKind == "" {
		return errors.New("missing daily request kind")
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var owner, status, previousKind string
		var current, requestVersion int
		err := tx.QueryRow(ctx, `select coalesce(requester_id::text,''),status,version,request_version,
			coalesce(details->>'request_kind','') from tickets where id=$1 for update`, ticketID).
			Scan(&owner, &status, &current, &requestVersion, &previousKind)
		if errors.Is(err, pgx.ErrNoRows) || owner != requesterID {
			return ErrTicketNotFound
		}
		if err != nil {
			return err
		}
		if current != version || status != "new" {
			return ErrTicketConflict
		}
		if previousKind != requestKind {
			return errors.New("cannot change the request kind")
		}
		if _, err := tx.Exec(ctx, `update tickets set type=$2,title=$3,description=$4,details=$5,
			request_version=request_version+1,version=version+1,updated_at=now() where id=$1`,
			ticketID, kind, title, description, details); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into ticket_request_revisions
			(ticket_id,request_version,title,description,details,actor_id)
			values($1,$2,$3,$4,$5,$6)`, ticketID, requestVersion+1, title, description, details, requesterID); err != nil {
			return err
		}
		body := fmt.Sprintf("客户重新提交申请（第 %d 版）", requestVersion+1)
		if _, err := tx.Exec(ctx, `insert into ticket_events(ticket_id,actor_type,actor_id,kind,visibility,body)
			values($1,'customer',$2,'comment','public',$3)`, ticketID, requesterID, body); err != nil {
			return err
		}
		return ticketAudit(ctx, tx, "customer", requesterID, "ticket_request_revised", ticketID, map[string]string{"request_kind": requestKind})
	})
}
