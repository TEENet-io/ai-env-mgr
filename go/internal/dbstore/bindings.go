package dbstore

import (
	"context"
	"errors"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type bindingRepo struct{ q querier }

const bindingColumns = `id, device_id, employee_id, epoch, note,
	bound_at, bound_by, unbound_at, unbound_by, restart_nonce, restart_at`

func scanBinding(row scanner) (repo.Binding, error) {
	var b repo.Binding
	var unboundAt, restartAt *time.Time
	err := row.Scan(&b.ID, &b.DeviceID, &b.EmployeeID, &b.Epoch, &b.Note,
		&b.BoundAt, &b.BoundBy, &unboundAt, &b.UnboundBy, &b.RestartNonce, &restartAt)
	if err != nil {
		return repo.Binding{}, err
	}
	b.UnboundAt = unboundAt
	b.RestartAt = restartAt
	return b, nil
}

func (r bindingRepo) Open(ctx context.Context, deviceID string) (repo.Binding, error) {
	b, err := scanBinding(r.q.QueryRow(ctx,
		`select `+bindingColumns+` from device_bindings
		  where device_id = $1 and unbound_at is null`, deviceID))
	if err != nil {
		return repo.Binding{}, mapError(err, "read binding")
	}
	return b, nil
}

func (r bindingRepo) OpenByEmployee(ctx context.Context, employeeID string) ([]repo.Binding, error) {
	return r.query(ctx, "list bindings for employee",
		`select `+bindingColumns+` from device_bindings
		  where employee_id = $1 and unbound_at is null
		  order by bound_at`, employeeID)
}

func (r bindingRepo) ListOpen(ctx context.Context) ([]repo.Binding, error) {
	return r.query(ctx, "list bindings",
		`select `+bindingColumns+` from device_bindings
		  where unbound_at is null order by bound_at`)
}

// History is every binding a machine has had, newest first -- the answer to
// "who had this machine in August", which is why unbinding closes a row
// instead of deleting it.
func (r bindingRepo) History(ctx context.Context, deviceID string) ([]repo.Binding, error) {
	return r.query(ctx, "read binding history",
		`select `+bindingColumns+` from device_bindings
		  where device_id = $1 order by epoch desc`, deviceID)
}

func (r bindingRepo) HistoryByEmployee(ctx context.Context, employeeID string) ([]repo.Binding, error) {
	return r.query(ctx, "read employee's machines",
		`select `+bindingColumns+` from device_bindings
		  where employee_id = $1 order by bound_at desc`, employeeID)
}

// Bind assigns a machine to an employee.
//
// The epoch is the machine's binding count plus one, computed in the same
// statement so two concurrent binds cannot pick the same number. A machine
// that already has somebody on it is ErrDuplicate: changing hands is Unbind
// then Bind, in a transaction, because a machine left half-reassigned is
// exactly the state that leaks one employee's credentials to another.
//
// No restart request is carried over. A pending "end Codex" belongs to the
// person who has just been unassigned; acting on it would end the new
// employee's session for a reason that has nothing to do with them.
func (r bindingRepo) Bind(ctx context.Context, deviceID, employeeID, note, by string) (repo.Binding, error) {
	if deviceID == "" || employeeID == "" {
		return repo.Binding{}, errors.New("bind machine: device and employee are both required")
	}
	b, err := scanBinding(r.q.QueryRow(ctx,
		`insert into device_bindings (device_id, employee_id, epoch, note, bound_by)
		 select $1, $2,
		        coalesce((select max(epoch) from device_bindings where device_id = $1), 0) + 1,
		        $3, $4
		 returning `+bindingColumns,
		deviceID, employeeID, note, by))
	if err != nil {
		return repo.Binding{}, mapError(err, "bind machine")
	}
	return b, nil
}

func (r bindingRepo) Unbind(ctx context.Context, deviceID, by string) (repo.Binding, error) {
	b, err := scanBinding(r.q.QueryRow(ctx,
		`update device_bindings
		    set unbound_at = now(), unbound_by = $2
		  where device_id = $1 and unbound_at is null
		  returning `+bindingColumns, deviceID, by))
	if err != nil {
		return repo.Binding{}, mapError(err, "unbind machine")
	}
	return b, nil
}

func (r bindingRepo) RequestCodexRestart(ctx context.Context, deviceID, nonce string) (repo.Binding, error) {
	if nonce == "" {
		return repo.Binding{}, errors.New("restart Codex: a nonce is required")
	}
	// A machine nobody is assigned to has nobody whose Codex could be ended.
	// Reporting that, rather than succeeding at nothing, is the difference
	// between an administrator retrying and an administrator waiting.
	b, err := scanBinding(r.q.QueryRow(ctx,
		`update device_bindings
		    set restart_nonce = $2, restart_at = now()
		  where device_id = $1 and unbound_at is null
		  returning `+bindingColumns, deviceID, nonce))
	if err != nil {
		return repo.Binding{}, mapError(err, "request Codex restart")
	}
	return b, nil
}

func (r bindingRepo) query(ctx context.Context, what, sql string, args ...any) ([]repo.Binding, error) {
	rows, err := r.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError(err, what)
	}
	defer rows.Close()
	out := []repo.Binding{}
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, mapError(err, what)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, what)
	}
	return out, nil
}
