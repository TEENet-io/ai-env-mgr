package dbstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type TicketChangeWorkflow struct {
	Implementation   map[string]any
	Confirmation     map[string]any
	ImplementationID int64
	ConfirmationID   int64
}

func (w TicketChangeWorkflow) AwaitingConfirmation() bool {
	return w.ImplementationID > w.ConfirmationID && w.Implementation["stage"] == "waiting_customer_result"
}

func (s TicketStore) ChangeWorkflow(ctx context.Context, ticketID string) (TicketChangeWorkflow, error) {
	rows, err := s.db.pool.Query(ctx, `select id,kind,payload from ticket_change_entries where ticket_id=$1 order by id desc`, ticketID)
	if err != nil {
		return TicketChangeWorkflow{}, err
	}
	defer rows.Close()
	var workflow TicketChangeWorkflow
	for rows.Next() {
		var id int64
		var kind string
		var raw []byte
		if err := rows.Scan(&id, &kind, &raw); err != nil {
			return TicketChangeWorkflow{}, err
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			return TicketChangeWorkflow{}, err
		}
		if kind == "implementation" && workflow.ImplementationID == 0 {
			workflow.ImplementationID, workflow.Implementation = id, payload
		}
		if kind == "confirmation" && workflow.ConfirmationID == 0 {
			workflow.ConfirmationID, workflow.Confirmation = id, payload
		}
		if workflow.ImplementationID != 0 && workflow.ConfirmationID != 0 {
			break
		}
	}
	return workflow, rows.Err()
}

func (s TicketStore) SaveChangeImplementation(ctx context.Context, ticketID, adminID string, version int, payload map[string]string) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var kind, subtype, status string
		var current int
		err := tx.QueryRow(ctx, `select type,coalesce(details->>'subtype',''),status,version from tickets where id=$1 for update`, ticketID).Scan(&kind, &subtype, &status, &current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTicketNotFound
		}
		if err != nil {
			return err
		}
		if kind != "general" || subtype != "environment" || status == "closed" {
			return errors.New("仅未关闭的环境变更申请可填写实施记录")
		}
		if current != version {
			return ErrTicketConflict
		}
		if _, err := tx.Exec(ctx, `insert into ticket_change_entries(ticket_id,kind,actor_type,actor_id,payload)
			values($1,'implementation','admin',$2,$3)`, ticketID, adminID, data); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update tickets set version=version+1,updated_at=now() where id=$1`, ticketID); err != nil {
			return err
		}
		body := "实施方更新了变更处理记录：" + payload["stage"]
		if _, err := tx.Exec(ctx, `insert into ticket_events(ticket_id,actor_type,actor_id,kind,visibility,body)
			values($1,'admin',$2,'comment','public',$3)`, ticketID, adminID, body); err != nil {
			return err
		}
		return ticketAudit(ctx, tx, "admin", adminID, "ticket_change_implementation", ticketID, map[string]string{"stage": payload["stage"]})
	})
}

func (s TicketStore) ConfirmChangeResult(ctx context.Context, ticketID, contactID string, version int, payload map[string]string) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var owner, kind, subtype, status string
		var current int
		err := tx.QueryRow(ctx, `select coalesce(requester_id::text,''),type,coalesce(details->>'subtype',''),status,version
			from tickets where id=$1 for update`, ticketID).Scan(&owner, &kind, &subtype, &status, &current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTicketNotFound
		}
		if err != nil {
			return err
		}
		if owner != contactID {
			return ErrTicketNotFound
		}
		if kind != "general" || subtype != "environment" || status == "closed" {
			return errors.New("当前工单不能确认结果")
		}
		if current != version {
			return ErrTicketConflict
		}
		var implementationID, confirmationID int64
		var stage string
		err = tx.QueryRow(ctx, `select id,coalesce(payload->>'stage','') from ticket_change_entries
			where ticket_id=$1 and kind='implementation' order by id desc limit 1`, ticketID).Scan(&implementationID, &stage)
		if errors.Is(err, pgx.ErrNoRows) || stage != "waiting_customer_result" {
			return errors.New("实施方尚未提交待确认的执行结果")
		}
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `select id from ticket_change_entries where ticket_id=$1 and kind='confirmation' order by id desc limit 1`, ticketID).Scan(&confirmationID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if confirmationID > implementationID {
			return errors.New("本次执行结果已经确认")
		}
		if _, err := tx.Exec(ctx, `insert into ticket_change_entries(ticket_id,kind,actor_type,actor_id,payload)
			values($1,'confirmation','customer',$2,$3)`, ticketID, contactID, data); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `update tickets set version=version+1,updated_at=now() where id=$1`, ticketID); err != nil {
			return err
		}
		body := fmt.Sprintf("客户已确认变更结果：%s", payload["result"])
		if _, err := tx.Exec(ctx, `insert into ticket_events(ticket_id,actor_type,actor_id,kind,visibility,body)
			values($1,'customer',$2,'comment','public',$3)`, ticketID, contactID, body); err != nil {
			return err
		}
		return ticketAudit(ctx, tx, "customer", contactID, "ticket_change_confirmation", ticketID, map[string]string{"result": payload["result"]})
	})
}
