package dbstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type TicketProposal struct {
	ID              int64
	Kind            string
	Payload         map[string]any
	RequiresManager bool
	Decision        string
	CreatedAt       time.Time
}

func (p *TicketProposal) Awaiting() bool { return p != nil && p.Decision == "" }

type PendingTicketApproval struct {
	TicketID      string
	Number        int64
	Title         string
	RequesterName string
	ProposalID    int64
}

func (s TicketStore) GetForContact(ctx context.Context, id, contactID, role string) (Ticket, error) {
	if role != "manager" {
		return s.Get(ctx, id, contactID)
	}
	ticket, err := s.Get(ctx, id, "")
	if err != nil {
		return Ticket{}, err
	}
	if ticket.RequesterID == "" {
		return Ticket{}, ErrTicketNotFound
	}
	return ticket, nil
}

func (s TicketStore) LatestProposal(ctx context.Context, ticketID string) (*TicketProposal, error) {
	var proposal TicketProposal
	var payload []byte
	err := s.db.pool.QueryRow(ctx, `select p.id,p.kind,p.payload,p.requires_manager,p.created_at,
		coalesce(d.decision,'') from ticket_proposals p
		left join ticket_proposal_decisions d on d.proposal_id=p.id
		where p.ticket_id=$1 order by p.id desc limit 1`, ticketID).
		Scan(&proposal.ID, &proposal.Kind, &payload, &proposal.RequiresManager, &proposal.CreatedAt, &proposal.Decision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(payload, &proposal.Payload); err != nil {
		return nil, err
	}
	return &proposal, nil
}

func (s TicketStore) PendingManagerApprovals(ctx context.Context, limit int) ([]PendingTicketApproval, error) {
	rows, err := s.db.pool.Query(ctx, `select t.id,t.number,t.title,c.name,p.id
		from ticket_proposals p join tickets t on t.id=p.ticket_id
		join customer_contacts c on c.id=t.requester_id
		left join ticket_proposal_decisions d on d.proposal_id=p.id
		where p.requires_manager and d.id is null and t.status='waiting_customer'
		and p.id=(select max(p2.id) from ticket_proposals p2 where p2.ticket_id=t.id)
		order by p.created_at desc limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var approvals []PendingTicketApproval
	for rows.Next() {
		var approval PendingTicketApproval
		if err := rows.Scan(&approval.TicketID, &approval.Number, &approval.Title, &approval.RequesterName, &approval.ProposalID); err != nil {
			return nil, err
		}
		approvals = append(approvals, approval)
	}
	return approvals, rows.Err()
}

func (s TicketStore) SaveProposal(ctx context.Context, ticketID, adminID string, version int, kind string, payload map[string]string, requiresManager bool) error {
	if kind != "quota" && kind != "account-delete" {
		return errors.New("unsupported proposal kind")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var requestKind, status string
		var current int
		err := tx.QueryRow(ctx, `select coalesce(details->>'request_kind',''),status,version
			from tickets where id=$1 for update`, ticketID).Scan(&requestKind, &status, &current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTicketNotFound
		}
		if err != nil {
			return err
		}
		if requestKind != kind || status == "done" || status == "closed" || current != version {
			return ErrTicketConflict
		}
		if _, err := tx.Exec(ctx, `insert into ticket_proposals(ticket_id,kind,payload,requires_manager,admin_id)
			values($1,$2,$3,$4,$5)`, ticketID, kind, data, requiresManager, adminID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update tickets set status='waiting_customer',version=version+1,updated_at=now() where id=$1`, ticketID); err != nil {
			return err
		}
		body := "服务团队已提交具体方案，等待客户确认；申请本身尚未执行。"
		if requiresManager {
			body = "服务团队已提交具体方案，等待账号管理联系人确认；申请本身尚未执行。"
		}
		if _, err := tx.Exec(ctx, `insert into ticket_events(ticket_id,actor_type,actor_id,kind,visibility,body)
			values($1,'admin',$2,'comment','public',$3)`, ticketID, adminID, body); err != nil {
			return err
		}
		return ticketAudit(ctx, tx, "admin", adminID, "ticket_proposal_sent", ticketID, map[string]string{"kind": kind})
	})
}

func (s TicketStore) DecideProposal(ctx context.Context, ticketID, contactID string, version int, proposalID int64, decision string) error {
	if decision != "approved" && decision != "rejected" {
		return errors.New("invalid proposal decision")
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var owner, status string
		var current int
		err := tx.QueryRow(ctx, `select coalesce(requester_id::text,''),status,version
			from tickets where id=$1 for update`, ticketID).Scan(&owner, &status, &current)
		if errors.Is(err, pgx.ErrNoRows) || owner == "" {
			return ErrTicketNotFound
		}
		if err != nil {
			return err
		}
		if current != version || status != "waiting_customer" {
			return ErrTicketConflict
		}
		var role string
		if err := tx.QueryRow(ctx, `select role from customer_contacts where id=$1 and disabled_at is null`, contactID).Scan(&role); err != nil {
			return ErrTicketNotFound
		}
		var latestID int64
		var requiresManager bool
		err = tx.QueryRow(ctx, `select id,requires_manager from ticket_proposals
			where ticket_id=$1 order by id desc limit 1`, ticketID).Scan(&latestID, &requiresManager)
		if err != nil || latestID != proposalID {
			return ErrTicketConflict
		}
		if requiresManager && role != "manager" || !requiresManager && owner != contactID && role != "manager" {
			return ErrTicketNotFound
		}
		var decided bool
		if err := tx.QueryRow(ctx, `select exists(select 1 from ticket_proposal_decisions where proposal_id=$1)`, proposalID).Scan(&decided); err != nil {
			return err
		}
		if decided {
			return ErrTicketConflict
		}
		if _, err := tx.Exec(ctx, `insert into ticket_proposal_decisions(proposal_id,contact_id,decision)
			values($1,$2,$3)`, proposalID, contactID, decision); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update tickets set status='working',version=version+1,updated_at=now() where id=$1`, ticketID); err != nil {
			return err
		}
		body := "客户已确认具体方案；服务团队可以继续处理。"
		if decision == "rejected" {
			body = "客户未同意具体方案；服务团队需重新评估，不能按此方案执行。"
		}
		if _, err := tx.Exec(ctx, `insert into ticket_events(ticket_id,actor_type,actor_id,kind,visibility,body)
			values($1,'customer',$2,'comment','public',$3)`, ticketID, contactID, body); err != nil {
			return err
		}
		return ticketAudit(ctx, tx, "customer", contactID, "ticket_proposal_decided", ticketID, map[string]string{"decision": decision, "proposal_id": fmt.Sprint(proposalID)})
	})
}
