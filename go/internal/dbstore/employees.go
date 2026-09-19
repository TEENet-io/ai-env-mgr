package dbstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type employeeRepo struct{ q querier }

// employeeColumns is the select list every read here shares, in the order
// scanEmployee expects. One list means a column added in a migration is added
// in one place, not in six that must agree.
const employeeColumns = `id, windows_user, coalesce(external_id, ''), name, department,
	codex_account, status, auth_epoch, version,
	created_at, updated_at, offboarded_at, deleted_at`

type scanner interface{ Scan(dest ...any) error }

func scanEmployee(row scanner) (repo.Employee, error) {
	var e repo.Employee
	var status string
	var offboardedAt, deletedAt *time.Time
	err := row.Scan(&e.ID, &e.WindowsUser, &e.ExternalID, &e.Name, &e.Department,
		&e.CodexAccount, &status, &e.AuthEpoch, &e.Version,
		&e.CreatedAt, &e.UpdatedAt, &offboardedAt, &deletedAt)
	if err != nil {
		return repo.Employee{}, err
	}
	e.Status = repo.EmployeeStatus(status)
	e.OffboardedAt = offboardedAt
	e.DeletedAt = deletedAt
	return e, nil
}

func (r employeeRepo) ByID(ctx context.Context, id string) (repo.Employee, error) {
	e, err := scanEmployee(r.q.QueryRow(ctx,
		`select `+employeeColumns+` from employees where id = $1`, id))
	if err != nil {
		return repo.Employee{}, mapError(err, "read employee")
	}
	return e, nil
}

func (r employeeRepo) ByWindowsUser(ctx context.Context, windowsUser string) (repo.Employee, error) {
	e, err := scanEmployee(r.q.QueryRow(ctx,
		`select `+employeeColumns+` from employees
		  where windows_user = $1 and deleted_at is null`,
		repo.NormalizeWindowsUser(windowsUser)))
	if err != nil {
		return repo.Employee{}, mapError(err, "read employee")
	}
	return e, nil
}

func (r employeeRepo) List(ctx context.Context, filter repo.EmployeeFilter) ([]repo.Employee, error) {
	rows, err := r.q.Query(ctx,
		`select `+employeeColumns+` from employees
		 where ($1 or status = 'active') and ($2 or deleted_at is null)
		 order by windows_user`, filter.IncludeOffboarded, filter.IncludeDeleted)
	if err != nil {
		return nil, mapError(err, "list employees")
	}
	defer rows.Close()

	// An empty roster is an empty slice, not nil: callers range over it and
	// templates count it, and nil vs empty is a distinction nobody wants.
	out := []repo.Employee{}
	for rows.Next() {
		e, err := scanEmployee(rows)
		if err != nil {
			return nil, mapError(err, "scan employee")
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, "list employees")
	}
	return out, nil
}

func (r employeeRepo) Create(ctx context.Context, n repo.NewEmployee) (repo.Employee, error) {
	user := repo.NormalizeWindowsUser(n.WindowsUser)
	if user == "" {
		return repo.Employee{}, errors.New("create employee: windows user must not be empty")
	}
	e, err := scanEmployee(r.q.QueryRow(ctx,
		`insert into employees (windows_user, external_id, name, department, codex_account)
		 values ($1, nullif($2, ''), $3, $4, $5)
		 returning `+employeeColumns,
		user, n.ExternalID, n.Name, n.Department, n.CodexAccount))
	if err != nil {
		return repo.Employee{}, mapError(err, "create employee")
	}
	return e, nil
}

func (r employeeRepo) UpdateProfile(ctx context.Context, id string, version int, p repo.Profile) (repo.Employee, error) {
	e, err := scanEmployee(r.q.QueryRow(ctx,
		`update employees
		    set name = $3, department = $4, codex_account = $5,
		        external_id = nullif($6, ''), version = version + 1, updated_at = now()
		  where id = $1 and version = $2
		  returning `+employeeColumns,
		id, version, p.Name, p.Department, p.CodexAccount, p.ExternalID))
	if err == nil {
		return e, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return repo.Employee{}, r.explainMiss(ctx, id, version, "update employee")
	}
	return repo.Employee{}, mapError(err, "update employee")
}

func (r employeeRepo) Offboard(ctx context.Context, id string, version int) (repo.Employee, error) {
	// Raising auth_epoch is what makes this stick: a provisioning task that
	// was already in flight comes back for an epoch that no longer exists and
	// is dropped, instead of handing a working token to somebody who left.
	e, err := scanEmployee(r.q.QueryRow(ctx,
		`update employees
		    set status = 'offboarded', offboarded_at = now(),
		        auth_epoch = auth_epoch + 1, version = version + 1, updated_at = now()
		  where id = $1 and version = $2 and status = 'active'
		  returning `+employeeColumns,
		id, version))
	if err == nil {
		return e, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// Offboarding somebody who has already left is not an error. The
		// console offers a retry for a partial offboard, and that retry must
		// not fail on the one step that did succeed.
		if current, lookupErr := r.ByID(ctx, id); lookupErr == nil && !current.Active() {
			return current, nil
		}
		return repo.Employee{}, r.explainMiss(ctx, id, version, "offboard employee")
	}
	return repo.Employee{}, mapError(err, "offboard employee")
}

func (r employeeRepo) Reopen(ctx context.Context, id string, version int) (repo.Employee, error) {
	// The epoch goes up again rather than back: whatever was issued before the
	// departure stays dead, and the person comes back with new credentials.
	e, err := scanEmployee(r.q.QueryRow(ctx,
		`update employees
		    set status = 'active', offboarded_at = null,
		        auth_epoch = auth_epoch + 1, version = version + 1, updated_at = now()
		  where id = $1 and version = $2 and status = 'offboarded'
		  returning `+employeeColumns,
		id, version))
	if err == nil {
		return e, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if current, lookupErr := r.ByID(ctx, id); lookupErr == nil && current.Active() {
			return current, nil
		}
		return repo.Employee{}, r.explainMiss(ctx, id, version, "reopen employee")
	}
	return repo.Employee{}, mapError(err, "reopen employee")
}

func (r employeeRepo) Delete(ctx context.Context, id string, version int) (repo.Employee, error) {
	e, err := scanEmployee(r.q.QueryRow(ctx,
		`update employees
		    set deleted_at = now(), version = version + 1, updated_at = now()
		  where id = $1 and version = $2 and status = 'offboarded' and deleted_at is null
		  returning `+employeeColumns,
		id, version))
	if err == nil {
		return e, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		current, lookupErr := r.ByID(ctx, id)
		switch {
		case lookupErr != nil:
			return repo.Employee{}, fmt.Errorf("delete employee: %w", lookupErr)
		case current.Deleted():
			return current, nil
		case current.Active():
			return repo.Employee{}, errors.New("delete employee: the account is still open; close it first")
		}
		return repo.Employee{}, r.explainMiss(ctx, id, version, "delete employee")
	}
	return repo.Employee{}, mapError(err, "delete employee")
}

func (r employeeRepo) BumpAuthEpoch(ctx context.Context, id string, version int) (repo.Employee, error) {
	e, err := scanEmployee(r.q.QueryRow(ctx,
		`update employees
		    set auth_epoch = auth_epoch + 1, version = version + 1, updated_at = now()
		  where id = $1 and version = $2
		  returning `+employeeColumns,
		id, version))
	if err == nil {
		return e, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return repo.Employee{}, r.explainMiss(ctx, id, version, "re-issue employee credentials")
	}
	return repo.Employee{}, mapError(err, "re-issue employee credentials")
}

// explainMiss says why an update matched no row: the employee is gone, or
// somebody else changed it first. "Nothing happened" is the least useful thing
// a console can tell an administrator who just pressed save.
func (r employeeRepo) explainMiss(ctx context.Context, id string, version int, what string) error {
	current, err := r.ByID(ctx, id)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if current.Version != version {
		return fmt.Errorf("%s: %w (read version %d, now %d)", what, repo.ErrConflict, version, current.Version)
	}
	return fmt.Errorf("%s: employee is %s", what, current.Status)
}

func (r employeeRepo) SetModels(ctx context.Context, id string, models []string) error {
	// The employee has to exist even when the list is empty: with nothing to
	// insert there is no foreign key to violate, and "granted no models to
	// nobody" would report success.
	var exists bool
	if err := r.q.QueryRow(ctx,
		`select exists (select 1 from employees where id = $1)`, id).Scan(&exists); err != nil {
		return mapError(err, "set models")
	}
	if !exists {
		return fmt.Errorf("set models: %w", repo.ErrNotFound)
	}

	// One statement, so the grant list is never briefly empty for a reader.
	if _, err := r.q.Exec(ctx,
		`with wanted as (select distinct unnest($2::text[]) as model),
		     removed as (
		       delete from employee_models
		        where employee_id = $1 and model not in (select model from wanted)
		     )
		insert into employee_models (employee_id, model)
		select $1, model from wanted
		on conflict (employee_id, model) do nothing`,
		id, models); err != nil {
		return mapError(err, "set models")
	}
	return nil
}

func (r employeeRepo) Models(ctx context.Context, id string) ([]string, error) {
	rows, err := r.q.Query(ctx,
		`select model from employee_models where employee_id = $1 order by model`, id)
	if err != nil {
		return nil, mapError(err, "read models")
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, mapError(err, "scan model")
		}
		out = append(out, model)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, "read models")
	}
	return out, nil
}
