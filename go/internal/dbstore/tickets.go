package dbstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
	"github.com/jackc/pgx/v5"
)

var ErrTicketConflict = errors.New("ticket was changed by someone else; reload it")
var ErrTicketNotFound = errors.New("ticket not found")

type CustomerContact struct {
	ID, Email, Name, PasswordHash, Role string
	DisabledAt                          *time.Time
}

type Ticket struct {
	ID, RequesterID, RequesterName, Type, Title, Description, Status, Priority          string
	AssigneeID, AssigneeName, EmployeeID, EmployeeWindowsUser, DeviceID, DeviceHostname string
	Number                                                                              int64
	Version                                                                             int
	CreatedAt, UpdatedAt                                                                time.Time
	Details                                                                             map[string]any
	NeedsAction                                                                         bool
	ProposalPending, ProposalRequiresManager                                            bool
}

func ticketStatusLabel(status string) string {
	switch status {
	case "new":
		return "待受理"
	case "working":
		return "处理中"
	case "waiting_customer":
		return "待客户补充"
	case "done":
		return "已完成"
	case "closed":
		return "已关闭"
	default:
		return status
	}
}

func (t Ticket) StatusLabel() string {
	if t.Status == "waiting_customer" && t.ProposalPending {
		if t.ProposalRequiresManager {
			return "待联系人确认方案"
		}
		return "待确认方案"
	}
	if t.Status == "done" && t.Details["request_kind"] != nil {
		return "待确认结果"
	}
	return ticketStatusLabel(t.Status)
}
func (t Ticket) TypeLabel() string {
	if requestKind, ok := t.Details["request_kind"].(string); ok {
		if label, found := map[string]string{
			"account-add": "员工账号 · 新增员工", "account-toggle": "员工账号 · 停用或恢复", "account-delete": "员工账号 · 离职／删除",
			"account-edit": "员工账号 · 修改信息", "desktop-problem": "远程桌面 · 使用问题", "software-add": "远程桌面 · 安装软件",
			"software-change": "远程桌面 · 更新或卸载", "website": "远程桌面 · 网站访问", "ai-problem": "AI 模型与使用额度 · 使用问题",
			"quota": "AI 模型与使用额度 · 额度追加", "tool-problem": "定制工具 · 使用问题", "other-problem": "其他问题",
		}[requestKind]; found {
			return label
		}
	}
	switch t.Type {
	case "general":
		switch t.Details["subtype"] {
		case "employee":
			return "一般事务 · 员工操作"
		case "website":
			return "一般事务 · 网站封禁"
		case "software":
			return "一般事务 · 软件预装"
		case "environment":
			return "一般事务 · 客户环境变更"
		}
		return "一般事务"
	case "bug":
		return "Bug 提交"
	case "onboarding":
		return "新员工开户"
	case "offboarding":
		return "员工离职"
	case "access":
		return "权限或额度调整"
	case "other":
		return "故障与其他"
	default:
		return t.Type
	}
}

type TicketEvent struct {
	ID                                                                 int64
	ActorType, ActorName, Kind, Visibility, Body, FromStatus, ToStatus string
	CreatedAt                                                          time.Time
}

func (e TicketEvent) FromStatusLabel() string { return ticketStatusLabel(e.FromStatus) }
func (e TicketEvent) ToStatusLabel() string   { return ticketStatusLabel(e.ToStatus) }
func (e TicketEvent) KindLabel() string {
	switch e.Kind {
	case "created":
		return "创建工单"
	case "comment":
		return "回复"
	case "status":
		return "状态更新"
	case "assignment":
		return "分派"
	case "link":
		return "关联记录"
	default:
		return e.Kind
	}
}

type TicketTaskLink struct{ Kind, ID string }

type TicketCounts struct{ Pending, Working, Finished int }

type TicketStore struct{ db *DB }

func (d *DB) Tickets() TicketStore { return TicketStore{db: d} }

func (s TicketStore) Counts(ctx context.Context, requesterID string) (TicketCounts, error) {
	var counts TicketCounts
	err := s.db.pool.QueryRow(ctx, `select
		count(*) filter (where status='waiting_customer' or (status='done' and details ? 'request_kind')),
		count(*) filter (where status in ('new','working')),
		count(*) filter (where status='closed' or (status='done' and not (details ? 'request_kind')))
		from tickets where requester_id=$1`, requesterID).Scan(&counts.Pending, &counts.Working, &counts.Finished)
	return counts, err
}

func ticketAudit(ctx context.Context, tx pgx.Tx, actorType, actorID, action, ticketID string, detail map[string]string) error {
	if actorType == "customer" {
		actorType = repo.ActorSystem
		actorID = "customer:" + actorID
	}
	data, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = (auditRepo{q: tx}).Append(ctx, repo.AuditEvent{
		ActorType: actorType, ActorID: actorID, Action: action, TargetType: "ticket", TargetID: ticketID, Detail: data,
	})
	return err
}

func (s TicketStore) CreateContact(ctx context.Context, email, name, hash string) (CustomerContact, error) {
	return s.CreateContactWithRole(ctx, email, name, hash, "manager")
}

func (s TicketStore) CreateContactWithRole(ctx context.Context, email, name, hash, role string) (CustomerContact, error) {
	if role != "manager" && role != "employee" {
		return CustomerContact{}, errors.New("invalid customer contact role")
	}
	var c CustomerContact
	err := s.db.pool.QueryRow(ctx, `insert into customer_contacts(email,name,password_hash,role) values($1,$2,$3,$4)
		returning id,email,name,password_hash,role,disabled_at`, email, name, hash, role).
		Scan(&c.ID, &c.Email, &c.Name, &c.PasswordHash, &c.Role, &c.DisabledAt)
	return c, err
}

func (s TicketStore) ContactByEmail(ctx context.Context, email string) (CustomerContact, error) {
	var c CustomerContact
	err := s.db.pool.QueryRow(ctx, `select id,email,name,password_hash,role,disabled_at from customer_contacts where email=$1`, email).
		Scan(&c.ID, &c.Email, &c.Name, &c.PasswordHash, &c.Role, &c.DisabledAt)
	return c, err
}

func (s TicketStore) ContactBySession(ctx context.Context, hash []byte, idle time.Duration) (CustomerContact, error) {
	var c CustomerContact
	err := s.db.pool.QueryRow(ctx, `update customer_sessions cs set last_seen_at=now(),
		expires_at=least(cs.absolute_expires_at, now()+$2 * interval '1 second')
		from customer_contacts cc where cs.contact_id=cc.id and cs.token_sha256=$1
		and cs.expires_at>now() and cs.absolute_expires_at>now() and cc.disabled_at is null
		returning cc.id,cc.email,cc.name,cc.password_hash,cc.role,cc.disabled_at`, hash, idle.Seconds()).
		Scan(&c.ID, &c.Email, &c.Name, &c.PasswordHash, &c.Role, &c.DisabledAt)
	return c, err
}

func (s TicketStore) CreateSession(ctx context.Context, id string, hash []byte, idle, absolute time.Duration) error {
	_, err := s.db.pool.Exec(ctx, `insert into customer_sessions(token_sha256,contact_id,expires_at,absolute_expires_at)
		values($1,$2,now()+$3 * interval '1 second',now()+$4 * interval '1 second')`, hash, id, idle.Seconds(), absolute.Seconds())
	return err
}

func (s TicketStore) DeleteSession(ctx context.Context, hash []byte) error {
	_, err := s.db.pool.Exec(ctx, `delete from customer_sessions where token_sha256=$1`, hash)
	return err
}

func (s TicketStore) ListContacts(ctx context.Context) ([]CustomerContact, error) {
	rows, err := s.db.pool.Query(ctx, `select id,email,name,password_hash,role,disabled_at from customer_contacts order by name,email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CustomerContact{}
	for rows.Next() {
		var c CustomerContact
		if err := rows.Scan(&c.ID, &c.Email, &c.Name, &c.PasswordHash, &c.Role, &c.DisabledAt); err != nil {
			return nil, err
		}
		c.PasswordHash = ""
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s TicketStore) SetContactDisabled(ctx context.Context, id string, disabled bool) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var tagErr error
		var changed int64
		if disabled {
			tag, err := tx.Exec(ctx, `update customer_contacts set disabled_at=now() where id=$1`, id)
			if err == nil {
				changed = tag.RowsAffected()
			}
			tagErr = err
			if tagErr == nil {
				_, tagErr = tx.Exec(ctx, `delete from customer_sessions where contact_id=$1`, id)
			}
		} else {
			tag, err := tx.Exec(ctx, `update customer_contacts set disabled_at=null where id=$1`, id)
			if err == nil {
				changed = tag.RowsAffected()
			}
			tagErr = err
		}
		if tagErr != nil {
			return tagErr
		}
		if changed != 1 {
			return ErrTicketNotFound
		}
		return nil
	})
}

func (s TicketStore) SetContactRole(ctx context.Context, id, role string) error {
	if role != "manager" && role != "employee" {
		return errors.New("invalid customer contact role")
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `update customer_contacts set role=$2 where id=$1`, id, role)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrTicketNotFound
		}
		_, err = tx.Exec(ctx, `delete from customer_sessions where contact_id=$1`, id)
		return err
	})
}

func (s TicketStore) SetContactPassword(ctx context.Context, id, oldHash, newHash string) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		query := `update customer_contacts set password_hash=$2 where id=$1 and disabled_at is null`
		args := []any{id, newHash}
		if oldHash != "" {
			query += ` and password_hash=$3`
			args = append(args, oldHash)
		}
		tag, err := tx.Exec(ctx, query, args...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrTicketConflict
		}
		_, err = tx.Exec(ctx, `delete from customer_sessions where contact_id=$1`, id)
		return err
	})
}

// Failed password guesses are bounded by both the account and the source.
func (s TicketStore) LoginAllowed(ctx context.Context, email, source string) (bool, error) {
	var accountCount, sourceCount int
	err := s.db.pool.QueryRow(ctx, `select count(*) filter(where email=$1),count(*) filter(where source_key=$2)
		from customer_login_attempts where (email=$1 or source_key=$2)
		and at>now()-interval '15 minutes' and not succeeded`, email, source).Scan(&accountCount, &sourceCount)
	return accountCount < 8 && sourceCount < 50, err
}

func (s TicketStore) RecordLogin(ctx context.Context, email, source string, success bool) error {
	_, err := s.db.pool.Exec(ctx, `insert into customer_login_attempts(email,source_key,succeeded) values($1,$2,$3)`, email, source, success)
	return err
}

func (s TicketStore) Create(ctx context.Context, requesterID, key, kind, title, description, priority string, details []byte) (Ticket, error) {
	return s.create(ctx, requesterID, key, kind, title, description, priority, details, "customer", requesterID)
}

// CreateByAdmin creates an internal ticket with no customer owner. Only
// administrator sessions can access it; customer-owned tickets are separate.
func (s TicketStore) CreateByAdmin(ctx context.Context, key, kind, title, description, priority string, details []byte, adminID string) (Ticket, error) {
	return s.create(ctx, "", key, kind, title, description, priority, details, "admin", adminID)
}

func (s TicketStore) create(ctx context.Context, requesterID, key, kind, title, description, priority string, details []byte, actorType, actorID string) (Ticket, error) {
	var t Ticket
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `insert into tickets(requester_id,request_key,type,title,description,priority,details)
			values(nullif($1,'')::uuid,$2,$3,$4,$5,$6,$7) on conflict do nothing returning id,number`, requesterID, key, kind, title, description, priority, details).Scan(&t.ID, &t.Number)
		if errors.Is(err, pgx.ErrNoRows) {
			return tx.QueryRow(ctx, `select id,number from tickets where requester_id is not distinct from nullif($1,'')::uuid and request_key=$2`, requesterID, key).Scan(&t.ID, &t.Number)
		}
		if err != nil {
			return err
		}
		if requesterID != "" {
			var request map[string]any
			if err := json.Unmarshal(details, &request); err != nil {
				return err
			}
			if _, isDaily := request["request_kind"].(string); isDaily {
				if _, err := tx.Exec(ctx, `insert into ticket_request_revisions
					(ticket_id,request_version,title,description,details,actor_id)
					values($1,1,$2,$3,$4,$5)`, t.ID, title, description, details, requesterID); err != nil {
					return err
				}
			}
		}
		_, err = tx.Exec(ctx, `insert into ticket_events(ticket_id,actor_type,actor_id,kind,visibility,body)
			values($1,$2,$3,'created','public','')`, t.ID, actorType, actorID)
		if err != nil {
			return err
		}
		return ticketAudit(ctx, tx, actorType, actorID, "ticket_created", t.ID, map[string]string{"type": kind, "requester_id": requesterID})
	})
	return t, err
}

// An untouched new request needs triage. An active request needs a response
// when its latest public exchange is from the customer. Private notes and
// assignment changes must not clear that signal.
const ticketNeedsActionSQL = `(t.status='new' or (t.status='working' and t.requester_id is not null and
	coalesce((select te.actor_type from ticket_events te where te.ticket_id=t.id
		and te.visibility='public' and te.kind in ('created','comment','status')
		order by te.id desc limit 1),'')='customer'))`

const ticketSelect = `select t.id,t.number,coalesce(t.requester_id::text,''),coalesce(c.name,'内部工单'),t.type,t.title,t.description,t.status,t.priority,
	coalesce(t.assignee_id::text,''),coalesce(a.username,''),coalesce(t.employee_id::text,''),coalesce(e.windows_user,''),
	coalesce(t.device_id::text,''),coalesce(d.hostname,''),t.version,t.created_at,t.updated_at,t.details,` + ticketNeedsActionSQL + ` as needs_action,
	coalesce((select d.id is null from ticket_proposals p left join ticket_proposal_decisions d on d.proposal_id=p.id
		where p.ticket_id=t.id order by p.id desc limit 1),false) as proposal_pending,
	coalesce((select p.requires_manager from ticket_proposals p where p.ticket_id=t.id order by p.id desc limit 1),false) as proposal_requires_manager
	from tickets t left join customer_contacts c on c.id=t.requester_id
	left join admin_principals a on a.id=t.assignee_id
	left join employees e on e.id=t.employee_id
	left join devices d on d.id=t.device_id`

func scanTicket(row interface{ Scan(...any) error }) (Ticket, error) {
	var t Ticket
	var details []byte
	err := row.Scan(&t.ID, &t.Number, &t.RequesterID, &t.RequesterName, &t.Type, &t.Title, &t.Description,
		&t.Status, &t.Priority, &t.AssigneeID, &t.AssigneeName, &t.EmployeeID, &t.EmployeeWindowsUser, &t.DeviceID, &t.DeviceHostname, &t.Version, &t.CreatedAt, &t.UpdatedAt, &details, &t.NeedsAction, &t.ProposalPending, &t.ProposalRequiresManager)
	if err == nil {
		err = json.Unmarshal(details, &t.Details)
	}
	return t, err
}

func (s TicketStore) Get(ctx context.Context, id, requesterID string) (Ticket, error) {
	query := ticketSelect + ` where t.id=$1`
	args := []any{id}
	if requesterID != "" {
		query += ` and t.requester_id=$2`
		args = append(args, requesterID)
	}
	t, err := scanTicket(s.db.pool.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, ErrTicketNotFound
	}
	return t, err
}

func (s TicketStore) List(ctx context.Context, requesterID, status string, limit, offset int) ([]Ticket, error) {
	filter := `($2='' or t.status=$2)`
	if status == "pending" {
		filter = `($2::text='pending' and ` + ticketNeedsActionSQL + `)`
	} else if status == "customer_action" {
		filter = `($2::text='customer_action' and (t.status='waiting_customer' or
			(t.status='done' and t.details ? 'request_kind')))`
	} else if status == "active" {
		filter = `($2::text='active' and t.status in ('new','working'))`
	} else if status == "finished" {
		filter = `($2::text='finished' and (t.status='closed' or
			(t.status='done' and not (t.details ? 'request_kind'))))`
	}
	query := ticketSelect + ` where ($1='' or t.requester_id::text=$1) and ` + filter + `
		order by case when t.status in ('done','closed') then 1 else 0 end,
		case t.priority when 'urgent' then 0 else 1 end,t.updated_at desc limit $3 offset $4`
	rows, err := s.db.pool.Query(ctx, query, requesterID, status, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Ticket{}
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s TicketStore) ActionRequiredCount(ctx context.Context) (int, error) {
	var count int
	err := s.db.pool.QueryRow(ctx, `select count(*) from tickets t where `+ticketNeedsActionSQL).Scan(&count)
	return count, err
}

func (s TicketStore) Events(ctx context.Context, id, requesterID string) ([]TicketEvent, error) {
	query := `select e.id,e.actor_type,coalesce(c.name,a.username,'系统'),e.kind,e.visibility,e.body,
		coalesce(e.from_status,''),coalesce(e.to_status,''),e.created_at from ticket_events e
		left join customer_contacts c on e.actor_type='customer' and e.actor_id=c.id
		left join admin_principals a on e.actor_type='admin' and e.actor_id=a.id
		join tickets t on t.id=e.ticket_id where e.ticket_id=$1`
	args := []any{id}
	if requesterID != "" {
		query += ` and t.requester_id=$2 and e.visibility='public'`
		args = append(args, requesterID)
	}
	query += ` order by e.id`
	rows, err := s.db.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TicketEvent{}
	for rows.Next() {
		var e TicketEvent
		if err := rows.Scan(&e.ID, &e.ActorType, &e.ActorName, &e.Kind, &e.Visibility, &e.Body, &e.FromStatus, &e.ToStatus, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AddEvent locks the ticket, checks ownership and version, then records both
// the state change and its evidence atomically. A stale browser cannot silently
// overwrite another operator's work.
func (s TicketStore) AddEvent(ctx context.Context, id, requesterID, actorType, actorID, kind, visibility, body, nextStatus string, version int) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var owner, status, requestKind string
		var current int
		err := tx.QueryRow(ctx, `select coalesce(requester_id::text,''),status,version,
			coalesce(details->>'request_kind','') from tickets where id=$1 for update`, id).Scan(&owner, &status, &current, &requestKind)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTicketNotFound
		}
		if err != nil {
			return err
		}
		if requesterID != "" && owner != requesterID {
			return ErrTicketNotFound
		}
		if current != version {
			return ErrTicketConflict
		}
		if nextStatus == "" {
			nextStatus = status
		}
		if !validTicketTransition(status, nextStatus, actorType) {
			return fmt.Errorf("invalid ticket status transition")
		}
		if kind == "comment" && status == "closed" {
			return fmt.Errorf("closed ticket cannot be replied to")
		}
		if nextStatus == "done" && (requestKind == "account-delete" || requestKind == "quota") {
			var approved bool
			if err := tx.QueryRow(ctx, `select exists(
				select 1 from ticket_proposals p join ticket_proposal_decisions d on d.proposal_id=p.id
				where p.ticket_id=$1 and d.decision='approved'
				and p.id=(select max(id) from ticket_proposals where ticket_id=$1))`, id).Scan(&approved); err != nil {
				return err
			}
			if !approved {
				return errors.New("客户尚未确认账号删除或额度追加方案")
			}
		}
		if _, err := tx.Exec(ctx, `update tickets set status=$2,version=version+1,updated_at=now(),
			closed_at=case when $2 in ('done','closed') then coalesce(closed_at,now()) else null end where id=$1`, id, nextStatus); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `insert into ticket_events(ticket_id,actor_type,actor_id,kind,visibility,body,from_status,to_status)
			values($1,$2,$3,$4,$5,$6,$7,$8)`, id, actorType, actorID, kind, visibility, body, status, nextStatus)
		if err != nil {
			return err
		}
		return ticketAudit(ctx, tx, actorType, actorID, "ticket_"+kind, id, map[string]string{"from": status, "to": nextStatus, "visibility": visibility})
	})
}

func validTicketTransition(from, to, actor string) bool {
	if from == to {
		return true
	}
	if actor == "customer" {
		return (from == "waiting_customer" && to == "working") || (from == "done" && (to == "new" || to == "closed")) || (from == "new" && to == "closed")
	}
	switch from {
	case "new":
		return to == "working" || to == "waiting_customer" || to == "closed"
	case "working":
		return to == "waiting_customer" || to == "done" || to == "closed"
	case "waiting_customer":
		return to == "working" || to == "closed"
	case "done":
		return to == "new" || to == "closed"
	default:
		return false
	}
}

func (s TicketStore) Assign(ctx context.Context, id, adminID, actorID string, version int) error {
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var current int
		err := tx.QueryRow(ctx, `select version from tickets where id=$1 for update`, id).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTicketNotFound
		}
		if err != nil {
			return err
		}
		if current != version {
			return ErrTicketConflict
		}
		var assignee any
		if adminID != "" {
			assignee = adminID
		}
		if _, err := tx.Exec(ctx, `update tickets set assignee_id=$2,version=version+1,updated_at=now() where id=$1`, id, assignee); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `insert into ticket_events(ticket_id,actor_type,actor_id,kind,visibility,body)
			values($1,'admin',$2,'assignment','internal',$3)`, id, actorID, strings.TrimSpace(adminID))
		if err != nil {
			return err
		}
		return ticketAudit(ctx, tx, "admin", actorID, "ticket_assignment", id, map[string]string{"assignee_id": adminID})
	})
}

func (s TicketStore) LinkSubject(ctx context.Context, id, subject, key, actorID string, version int) error {
	if subject != "employee" && subject != "device" {
		return errors.New("unsupported ticket subject")
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var current int
		err := tx.QueryRow(ctx, `select version from tickets where id=$1 for update`, id).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTicketNotFound
		}
		if err != nil {
			return err
		}
		if current != version {
			return ErrTicketConflict
		}
		var refID string
		if subject == "employee" {
			err = tx.QueryRow(ctx, `select id from employees where windows_user=$1`, strings.ToLower(key)).Scan(&refID)
		} else {
			err = tx.QueryRow(ctx, `select id from devices where hostname=$1`, key).Scan(&refID)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("employee or device not found")
		}
		if err != nil {
			return err
		}
		if subject == "employee" {
			_, err = tx.Exec(ctx, `update tickets set employee_id=$2,version=version+1,updated_at=now() where id=$1`, id, refID)
		} else {
			_, err = tx.Exec(ctx, `update tickets set device_id=$2,version=version+1,updated_at=now() where id=$1`, id, refID)
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `insert into ticket_events(ticket_id,actor_type,actor_id,kind,visibility,body)
			values($1,'admin',$2,'link','internal',$3)`, id, actorID, subject+": "+key)
		if err != nil {
			return err
		}
		return ticketAudit(ctx, tx, "admin", actorID, "ticket_link_subject", id, map[string]string{"subject": subject, "reference_id": refID})
	})
}

func (s TicketStore) LinkTask(ctx context.Context, id, kind, taskID, actorID string) error {
	var table string
	switch kind {
	case "task":
		table = "tasks"
	case "application":
		table = "application_tasks"
	case "admin":
		table = "admin_jobs"
	default:
		return errors.New("unsupported task kind")
	}
	return s.db.Tx(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `select exists(select 1 from `+table+` where id::text=$1)`, taskID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errors.New("task not found")
		}
		tag, err := tx.Exec(ctx, `insert into ticket_task_links(ticket_id,task_kind,task_id,linked_by)
			values($1,$2,$3,$4) on conflict do nothing`, id, kind, taskID, actorID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		_, err = tx.Exec(ctx, `insert into ticket_events(ticket_id,actor_type,actor_id,kind,visibility,body)
			values($1,'admin',$2,'link','internal',$3)`, id, actorID, taskID)
		if err != nil {
			return err
		}
		return ticketAudit(ctx, tx, "admin", actorID, "ticket_link_task", id, map[string]string{"task_kind": kind, "task_id": taskID})
	})
}

func (s TicketStore) TaskIDs(ctx context.Context, id string) ([]TicketTaskLink, error) {
	rows, err := s.db.pool.Query(ctx, `select task_kind,task_id from ticket_task_links where ticket_id=$1 order by created_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TicketTaskLink{}
	for rows.Next() {
		var x TicketTaskLink
		if err := rows.Scan(&x.Kind, &x.ID); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
