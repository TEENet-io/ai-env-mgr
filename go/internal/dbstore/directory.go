package dbstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TEENet-io/ai-env-mgr/internal/directory"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// DirectoryStore keeps the console's copy of the customer's list of people
// and applies what changed in it to the console's own accounts.
type DirectoryStore struct{ db *DB }

func (d *DB) Directory() DirectoryStore { return DirectoryStore{db: d} }

// directoryActor is who the audit trail says made a change the list caused.
const directoryActor = "directory-sync"

// Known is the local copy, keyed by account id.
func (s DirectoryStore) Known(ctx context.Context) (map[string]directory.Account, error) {
	rows, err := s.db.pool.Query(ctx, `select id::text, username, name, email, role, status from directory_accounts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]directory.Account{}
	for rows.Next() {
		var a directory.Account
		if err := rows.Scan(&a.ID, &a.Username, &a.Name, &a.Email, &a.Role, &a.Status); err != nil {
			return nil, err
		}
		out[a.ID] = a
	}
	return out, rows.Err()
}

// SyncState is the row the alerts read.
type SyncState struct {
	LastSuccessAt, LastAttemptAt, FailingSince *time.Time
	LastError                                  string
}

func (s DirectoryStore) State(ctx context.Context) (SyncState, error) {
	var st SyncState
	err := s.db.pool.QueryRow(ctx, `select last_success_at, last_attempt_at, failing_since, last_error
		from directory_sync_state`).Scan(&st.LastSuccessAt, &st.LastAttemptAt, &st.FailingSince, &st.LastError)
	return st, err
}

// RecordFailure notes a reading that did not happen and returns since when
// readings have been failing.
func (s DirectoryStore) RecordFailure(ctx context.Context, cause error) (time.Time, error) {
	var since time.Time
	err := s.db.pool.QueryRow(ctx, `update directory_sync_state
		set last_attempt_at = now(), last_error = $1, failing_since = coalesce(failing_since, now())
		returning failing_since`, cause.Error()).Scan(&since)
	return since, err
}

// SyncResult counts what Apply did, for the task's note.
type SyncResult struct {
	Accounts, Changes, Held, Missing int
	ContactsCreated, ContactsLinked  int
	SessionsEnded                    int
	FollowUps                        int
}

func (r SyncResult) String() string {
	return fmt.Sprintf("%d accounts, %d changes, %d held, %d missing; contacts +%d linked %d; %d sessions ended; %d follow-ups",
		r.Accounts, r.Changes, r.Held, r.Missing, r.ContactsCreated, r.ContactsLinked, r.SessionsEnded, r.FollowUps)
}

// Apply records one reading and everything it changes, in one transaction:
// the copy, the customer contacts, the sessions that must end, the audit
// trail and the alerts. Either all of it happens or none of it, so a failed
// apply is simply tried again at the next reading.
func (s DirectoryStore) Apply(ctx context.Context, list directory.List, plan directory.Plan) (SyncResult, error) {
	res := SyncResult{Accounts: len(list.Accounts), Changes: len(plan.Changes), Held: len(plan.Held), Missing: len(plan.Missing)}
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		for _, a := range plan.Store(list) {
			if _, err := tx.Exec(ctx, `insert into directory_accounts (id, username, name, email, role, status)
				values ($1, $2, $3, $4, $5, $6)
				on conflict (id) do update set
				  username = excluded.username, name = excluded.name, email = excluded.email,
				  role = excluded.role, status = excluded.status, missing_since = null,
				  updated_at = case when (directory_accounts.username, directory_accounts.name, directory_accounts.email,
				                          directory_accounts.role, directory_accounts.status)
				                       is distinct from (excluded.username, excluded.name, excluded.email, excluded.role, excluded.status)
				                    then now() else directory_accounts.updated_at end`,
				a.ID, a.Username, a.Name, a.Email, a.Role, a.Status); err != nil {
				return fmt.Errorf("store account %s: %w", a.ID, err)
			}
		}
		for _, a := range plan.Missing {
			if _, err := tx.Exec(ctx, `update directory_accounts set missing_since = coalesce(missing_since, now()) where id = $1`, a.ID); err != nil {
				return err
			}
		}
		ap := directoryApply{ctx: ctx, tx: tx, res: &res}
		for _, c := range plan.Changes {
			if err := ap.change(c); err != nil {
				return fmt.Errorf("%s %s: %w", c.Kind, c.After.ID, err)
			}
		}
		if err := ap.batchAlerts(plan); err != nil {
			return err
		}
		var held []byte
		if len(plan.Held) > 0 {
			held, _ = json.Marshal(plan.Held)
		}
		_, err := tx.Exec(ctx, `update directory_sync_state
			set last_success_at = now(), last_attempt_at = now(), last_error = '', failing_since = null, held_disables = $1`, held)
		return err
	})
	return res, err
}

type directoryApply struct {
	ctx context.Context
	tx  pgx.Tx
	res *SyncResult
}

// contactRole is the ticket system's name for a directory role. Ops
// accounts are not customers and have no contact.
func contactRole(role string) (string, bool) {
	switch role {
	case directory.RoleAdmin:
		return "manager", true
	case directory.RoleEmployee:
		return "employee", true
	}
	return "", false
}

func (ap directoryApply) change(c directory.Change) error {
	if err := ap.audit(c); err != nil {
		return err
	}
	a := c.After
	switch c.Kind {
	case directory.ChangeAdded:
		if a.Active() {
			return ap.ensureContact(a)
		}
		return nil
	case directory.ChangeRenamed:
		_, err := ap.tx.Exec(ap.ctx, `update customer_contacts set name = $2 where directory_id = $1`, a.ID, a.DisplayName())
		return err
	case directory.ChangeEmail:
		if a.Email != "" {
			tag, err := ap.tx.Exec(ap.ctx, `update customer_contacts set email = $2 where directory_id = $1
				and not exists (select 1 from customer_contacts o where o.email = $2 and o.directory_id is distinct from $1)`, a.ID, a.Email)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 && a.Active() && c.After.Role != directory.RoleOps {
				if err := ap.followUp(a, "email-conflict", fmt.Sprintf("%s 的郵箱改為 %s，但這個郵箱已屬於另一位聯絡人，工單聯絡人未更新", a.DisplayName(), a.Email)); err != nil {
					return err
				}
			}
		}
		if emp, ok, err := ap.liveEmployee(a.ID); err != nil {
			return err
		} else if ok {
			return ap.followUp(a, "email", fmt.Sprintf("%s 的郵箱由 %s 改為 %s；AI 工作間帳號 %s 和無影帳號不會自動修改，請人工同步", a.DisplayName(), c.Before.Email, a.Email, emp))
		}
		return nil
	case directory.ChangeRole:
		if c.Before.Role == directory.RoleOps || a.Role == directory.RoleOps {
			if err := ap.followUp(a, "ops-role", fmt.Sprintf("%s 的角色由 %s 改為 %s。運維權限不會自動變化，請確認這是否是預期的修改", a.DisplayName(), c.Before.Role, a.Role)); err != nil {
				return err
			}
		}
		role, ok := contactRole(a.Role)
		if !ok {
			return ap.endContact(a.ID)
		}
		if _, err := ap.tx.Exec(ap.ctx, `update customer_contacts set role = $2 where directory_id = $1`, a.ID, role); err != nil {
			return err
		}
		if a.Active() {
			return ap.ensureContact(a)
		}
		return nil
	case directory.ChangeDisabled, directory.ChangeDeleted:
		if err := ap.endContact(a.ID); err != nil {
			return err
		}
		tag, err := ap.tx.Exec(ap.ctx, `delete from admin_sessions where principal_id in
			(select id from admin_principals where directory_id = $1)`, a.ID)
		if err != nil {
			return err
		}
		ap.res.SessionsEnded += int(tag.RowsAffected())
		if c.Kind == directory.ChangeDeleted {
			return nil
		}
		if emp, ok, err := ap.liveEmployee(a.ID); err != nil {
			return err
		} else if ok {
			return ap.followUp(a, "disabled", fmt.Sprintf("%s 的定製工具帳號已%s，但 AI 工作間帳號 %s 仍在使用。請確認後關戶", a.DisplayName(), statusWord(a.Status), emp))
		}
		return nil
	case directory.ChangeReenabled:
		if _, ok := contactRole(a.Role); ok {
			if _, err := ap.tx.Exec(ap.ctx, `update customer_contacts set disabled_at = null where directory_id = $1`, a.ID); err != nil {
				return err
			}
			if err := ap.ensureContact(a); err != nil {
				return err
			}
		}
		if emp, ok, err := ap.offboardedEmployee(a.ID); err != nil {
			return err
		} else if ok {
			return ap.followUp(a, "reenabled", fmt.Sprintf("%s 的定製工具帳號已重新啟用。AI 工作間帳號 %s 不會自動恢復，需要時請重新開戶", a.DisplayName(), emp))
		}
		return nil
	}
	return fmt.Errorf("unknown change %q", c.Kind)
}

func statusWord(status string) string {
	if status == directory.StatusDeleted {
		return "刪除"
	}
	return "停用"
}

// ensureContact gives an active customer account a ticket contact: linked
// if there is one under the same email that nobody has claimed, new
// otherwise. An account with no email cannot have one -- the ticket system
// matches people by email -- and is left for a person to look at.
func (ap directoryApply) ensureContact(a directory.Account) error {
	role, ok := contactRole(a.Role)
	if !ok {
		return nil
	}
	var exists bool
	if err := ap.tx.QueryRow(ap.ctx, `select exists (select 1 from customer_contacts where directory_id = $1)`, a.ID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	if a.Email == "" {
		return ap.followUp(a, "no-email", fmt.Sprintf("%s 在定製工具裡沒有郵箱，無法建立工單聯絡人", a.DisplayName()))
	}
	tag, err := ap.tx.Exec(ap.ctx, `update customer_contacts set directory_id = $1, name = $3, role = $4
		where email = $2 and directory_id is null`, a.ID, a.Email, a.DisplayName(), role)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		ap.res.ContactsLinked++
		return nil
	}
	tag, err = ap.tx.Exec(ap.ctx, `insert into customer_contacts (email, name, password_hash, role, directory_id)
		values ($1, $2, null, $3, $4) on conflict (email) do nothing`, a.Email, a.DisplayName(), role, a.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ap.followUp(a, "email-conflict", fmt.Sprintf("%s 的郵箱 %s 已屬於另一位定製工具帳號的聯絡人，未建立工單聯絡人", a.DisplayName(), a.Email))
	}
	ap.res.ContactsCreated++
	return nil
}

// endContact disables the account's contact and ends its sessions.
func (ap directoryApply) endContact(id string) error {
	if _, err := ap.tx.Exec(ap.ctx, `update customer_contacts set disabled_at = coalesce(disabled_at, now()) where directory_id = $1`, id); err != nil {
		return err
	}
	tag, err := ap.tx.Exec(ap.ctx, `delete from customer_sessions where contact_id in
		(select id from customer_contacts where directory_id = $1)`, id)
	if err != nil {
		return err
	}
	ap.res.SessionsEnded += int(tag.RowsAffected())
	return nil
}

func (ap directoryApply) liveEmployee(id string) (string, bool, error) {
	return ap.employee(id, `status = 'active'`)
}

func (ap directoryApply) offboardedEmployee(id string) (string, bool, error) {
	return ap.employee(id, `status = 'offboarded'`)
}

func (ap directoryApply) employee(id, condition string) (string, bool, error) {
	var user string
	err := ap.tx.QueryRow(ap.ctx, `select windows_user from employees
		where directory_id = $1 and deleted_at is null and `+condition, id).Scan(&user)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return user, err == nil, err
}

func (ap directoryApply) followUp(a directory.Account, what, detail string) error {
	ap.res.FollowUps++
	_, _, err := (alertRepo{q: ap.tx}).Open(ap.ctx, repo.NewAlert{
		Kind: repo.AlertDirectoryFollowUp, Fingerprint: repo.AlertDirectoryFollowUp + ":" + what + ":" + a.ID,
		Severity: repo.SeverityWarn, SubjectType: "directory_account", SubjectID: a.ID,
		Title: "定製工具帳號變更需要人工處理：" + a.DisplayName(), Detail: detail,
	})
	return err
}

func (ap directoryApply) audit(c directory.Change) error {
	var before []byte
	if c.Kind != directory.ChangeAdded {
		before, _ = json.Marshal(c.Before)
	}
	after, _ := json.Marshal(c.After)
	_, err := (auditRepo{q: ap.tx}).Append(ap.ctx, repo.AuditEvent{
		ActorType: repo.ActorSystem, ActorID: directoryActor, Action: "directory." + c.Kind,
		TargetType: "directory_account", TargetID: c.After.ID, Before: before, After: after,
	})
	return err
}

// batchAlerts opens or resolves the alerts about the reading as a whole.
func (ap directoryApply) batchAlerts(plan directory.Plan) error {
	alerts := alertRepo{q: ap.tx}
	if _, err := alerts.Resolve(ap.ctx, repo.AlertDirectorySync, directoryActor); err != nil {
		return err
	}
	if len(plan.Held) > 0 {
		names := make([]string, 0, len(plan.Held))
		for _, c := range plan.Held {
			names = append(names, c.After.DisplayName())
		}
		if _, _, err := alerts.Open(ap.ctx, repo.NewAlert{
			Kind: repo.AlertDirectoryHeld, Fingerprint: repo.AlertDirectoryHeld, Severity: repo.SeverityCrit,
			SubjectType: "directory", Title: fmt.Sprintf("定製工具一次停用了 %d 個帳號，已暫緩處理", len(plan.Held)),
			Detail: fmt.Sprintf("超過每次 %d 個的上限，這批沒有生效：%s。請先向客戶核實。", directory.MaxDisablesPerSync, strings.Join(names, "、")),
		}); err != nil {
			return err
		}
	} else if _, err := alerts.Resolve(ap.ctx, repo.AlertDirectoryHeld, directoryActor); err != nil {
		return err
	}
	if len(plan.Missing) > 0 {
		names := make([]string, 0, len(plan.Missing))
		for _, a := range plan.Missing {
			names = append(names, a.DisplayName())
		}
		_, _, err := alerts.Open(ap.ctx, repo.NewAlert{
			Kind: repo.AlertDirectoryMissing, Fingerprint: repo.AlertDirectoryMissing, Severity: repo.SeverityWarn,
			SubjectType: "directory", Title: fmt.Sprintf("定製工具的名單裡少了 %d 個帳號", len(plan.Missing)),
			Detail: "這些帳號沒有出現在名單裡，控制台沒有對它們做任何處理：" + strings.Join(names, "、") + "。名單應該連已刪除的帳號一起返回，請檢查定製工具。",
		})
		return err
	}
	_, err := alerts.Resolve(ap.ctx, repo.AlertDirectoryMissing, directoryActor)
	return err
}

// ApplyReading is Apply for the Worker, which only reports what was done.
func (s DirectoryStore) ApplyReading(ctx context.Context, list directory.List, plan directory.Plan) (string, error) {
	res, err := s.Apply(ctx, list, plan)
	return res.String(), err
}

// ErrNotPermitted is a sign-in the list does not allow: the account is
// disabled or deleted there, or has no usable contact here.
var ErrNotPermitted = errors.New("this account may not sign in")

// CustomerForSignIn is the contact a person signing in through the custom
// tool becomes. A person new since the last reading is added to the copy and
// given a contact on the spot, so a new employee can raise a ticket before
// the next sync; one the copy knows as disabled or deleted is refused even
// if the tool let them in. The copy is not updated from the token otherwise:
// the sync owns it.
func (s DirectoryStore) CustomerForSignIn(ctx context.Context, a directory.Account) (string, error) {
	var contactID string
	err := s.db.Tx(ctx, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `select status from directory_accounts where id = $1`, a.ID).Scan(&status)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			a.Status = directory.StatusActive
			if _, err := tx.Exec(ctx, `insert into directory_accounts (id, username, name, email, role, status)
				values ($1, $2, $3, $4, $5, 'active') on conflict (id) do nothing`,
				a.ID, a.Username, a.Name, a.Email, a.Role); err != nil {
				return err
			}
			if err := (directoryApply{ctx: ctx, tx: tx, res: &SyncResult{}}).audit(directory.Change{Kind: directory.ChangeAdded, After: a}); err != nil {
				return err
			}
		case err != nil:
			return err
		case status != directory.StatusActive:
			return ErrNotPermitted
		}
		if err := (directoryApply{ctx: ctx, tx: tx, res: &SyncResult{}}).ensureContact(a); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `select id::text from customer_contacts where directory_id = $1 and disabled_at is null`, a.ID).Scan(&contactID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotPermitted
		}
		return err
	})
	return contactID, err
}

// AdminForSignIn is the console account linked to a directory account, if
// that link exists, the account is enabled, and the copy does not know the
// person as disabled. The link is the whitelist: an ops role in the tool on
// its own opens nothing here.
func (s DirectoryStore) AdminForSignIn(ctx context.Context, directoryID string) (string, error) {
	var username string
	err := s.db.pool.QueryRow(ctx, `select a.username from admin_principals a
		left join directory_accounts d on d.id = a.directory_id
		where a.directory_id = $1 and a.disabled_at is null
		  and (d.id is null or d.status = 'active')`, directoryID).Scan(&username)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotPermitted
	}
	return username, err
}

// OpsAccount is a directory account with the ops role, and the console
// account it is linked to, if any.
type OpsAccount struct {
	directory.Account
	LinkedTo string // admin_principals.username
}

// OpsAccounts lists the tool's ops accounts for the linking form.
func (s DirectoryStore) OpsAccounts(ctx context.Context) ([]OpsAccount, error) {
	rows, err := s.db.pool.Query(ctx, `select d.id::text, d.username, d.name, d.email, d.role, d.status, coalesce(a.username, '')
		from directory_accounts d left join admin_principals a on a.directory_id = d.id
		where d.role = 'ops' order by d.username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpsAccount
	for rows.Next() {
		var o OpsAccount
		if err := rows.Scan(&o.ID, &o.Username, &o.Name, &o.Email, &o.Role, &o.Status, &o.LinkedTo); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// AdminLinks maps console account ids to the directory account each is
// linked to.
func (s DirectoryStore) AdminLinks(ctx context.Context) (map[string]directory.Account, error) {
	rows, err := s.db.pool.Query(ctx, `select a.id::text, d.id::text, d.username, d.name, d.email, d.role, d.status
		from admin_principals a join directory_accounts d on d.id = a.directory_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]directory.Account{}
	for rows.Next() {
		var adminID string
		var d directory.Account
		if err := rows.Scan(&adminID, &d.ID, &d.Username, &d.Name, &d.Email, &d.Role, &d.Status); err != nil {
			return nil, err
		}
		out[adminID] = d
	}
	return out, rows.Err()
}

// LinkAdmin binds a console account to an ops account of the tool, or
// unbinds it when directoryID is empty. Only an ops account can be bound,
// and only to one console account.
func (s DirectoryStore) LinkAdmin(ctx context.Context, adminID, directoryID string) error {
	if directoryID == "" {
		_, err := s.db.pool.Exec(ctx, `update admin_principals set directory_id = null, updated_at = now() where id = $1`, adminID)
		return err
	}
	tag, err := s.db.pool.Exec(ctx, `update admin_principals set directory_id = $2, updated_at = now()
		where id = $1 and exists (select 1 from directory_accounts where id = $2 and role = 'ops')`, adminID, directoryID)
	if err != nil {
		return mapError(err, "link account")
	}
	if tag.RowsAffected() == 0 {
		return ErrNotPermitted
	}
	// A new link changes who this account is; whoever is signed in as it
	// signs in again.
	_, err = s.db.pool.Exec(ctx, `delete from admin_sessions where principal_id = $1`, adminID)
	return err
}
