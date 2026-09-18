package dbstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type grantRepo struct{ q querier }

const grantColumns = `id, employee_id, epoch, gateway, external_user, key_alias, models,
	desired, actual, credential_id, reconciled_at, last_error, created_at, updated_at`

func scanGrant(row scanner) (repo.Grant, error) {
	var g repo.Grant
	var credentialID *string
	var reconciledAt *time.Time
	err := row.Scan(&g.ID, &g.EmployeeID, &g.Epoch, &g.Gateway, &g.ExternalUser, &g.KeyAlias,
		&g.Models, &g.Desired, &g.Actual, &credentialID, &reconciledAt,
		&g.LastError, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		return repo.Grant{}, err
	}
	g.CredentialID = derefString(credentialID)
	g.ReconciledAt = reconciledAt
	return g, nil
}

func (r grantRepo) Active(ctx context.Context, gateway, employeeID string) (repo.Grant, error) {
	g, err := scanGrant(r.q.QueryRow(ctx,
		`select `+grantColumns+` from gateway_grants
		  where gateway = $1 and employee_id = $2 and desired = 'active'`,
		defaultGateway(gateway), employeeID))
	if err != nil {
		return repo.Grant{}, mapError(err, "read gateway grant")
	}
	return g, nil
}

func (r grantRepo) ByKeyAlias(ctx context.Context, gateway, keyAlias string) (repo.Grant, error) {
	g, err := scanGrant(r.q.QueryRow(ctx,
		`select `+grantColumns+` from gateway_grants where gateway = $1 and key_alias = $2`,
		defaultGateway(gateway), keyAlias))
	if err != nil {
		return repo.Grant{}, mapError(err, "read gateway grant")
	}
	return g, nil
}

func (r grantRepo) ByEmployee(ctx context.Context, employeeID string) ([]repo.Grant, error) {
	return r.query(ctx, "list gateway grants",
		`select `+grantColumns+` from gateway_grants
		  where employee_id = $1 order by epoch desc, created_at desc`, employeeID)
}

// Create records the intent to grant. It does not call anything: the gateway
// call is a task, committed with this row, and what the gateway actually did
// comes back through RecordActual.
//
// A second active grant for the same employee is refused by a partial unique
// index. Two live tokens for one person means revoking "the" token leaves the
// other one working, which is the shape of a leak nobody sees.
func (r grantRepo) Create(ctx context.Context, n repo.NewGrant) (repo.Grant, error) {
	if n.EmployeeID == "" || n.ExternalUser == "" || n.KeyAlias == "" {
		return repo.Grant{}, errors.New("create gateway grant: employee, gateway user and key alias are all required")
	}
	if n.Epoch <= 0 {
		return repo.Grant{}, errors.New("create gateway grant: an epoch is required")
	}
	models := n.Models
	if models == nil {
		models = []string{}
	}
	g, err := scanGrant(r.q.QueryRow(ctx,
		`insert into gateway_grants
		   (employee_id, epoch, gateway, external_user, key_alias, models, desired, credential_id)
		 values ($1, $2, $3, $4, $5, $6, 'active', $7)
		 returning `+grantColumns,
		n.EmployeeID, n.Epoch, defaultGateway(n.Gateway), n.ExternalUser, n.KeyAlias,
		models, nullable(n.CredentialID)))
	if err != nil {
		return repo.Grant{}, mapError(err, "create gateway grant")
	}
	return g, nil
}

func (r grantRepo) Revoke(ctx context.Context, id string) (repo.Grant, error) {
	// Revoking an already-revoked grant is not an error: this runs from a
	// task, and a task runs at least once.
	g, err := scanGrant(r.q.QueryRow(ctx,
		`update gateway_grants set desired = 'revoked', updated_at = now()
		  where id = $1
		  returning `+grantColumns, id))
	if err != nil {
		return repo.Grant{}, mapError(err, "revoke gateway grant")
	}
	return g, nil
}

// RecordActual stores what the gateway was observed to hold.
//
// It is deliberately separate from the intent: between asking and knowing
// there is a call that can time out, and a single state column would have to
// claim either that the change happened or that it did not.
func (r grantRepo) RecordActual(ctx context.Context, id, actual, reason string) (repo.Grant, error) {
	switch actual {
	case repo.ActualUnknown, repo.ActualActive, repo.ActualRevoked, repo.ActualMissing:
	default:
		return repo.Grant{}, fmt.Errorf("record gateway state: %q is not an observed state", actual)
	}
	g, err := scanGrant(r.q.QueryRow(ctx,
		`update gateway_grants
		    set actual = $2, reconciled_at = now(), last_error = left($3, 2000), updated_at = now()
		  where id = $1
		  returning `+grantColumns, id, actual, reason))
	if err != nil {
		return repo.Grant{}, mapError(err, "record gateway state")
	}
	return g, nil
}

// NeedsReconcile lists the grants worth asking the gateway about: never
// checked, checked too long ago, or where what we last saw does not match what
// we asked for.
//
// The last case is the one that matters. A grant we believe is revoked but
// which the gateway still serves is a token nobody thinks exists.
func (r grantRepo) NeedsReconcile(ctx context.Context, gateway string, staleAfter time.Duration, limit int) ([]repo.Grant, error) {
	if staleAfter <= 0 {
		staleAfter = time.Hour
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.q.Query(ctx,
		`select `+grantColumns+` from gateway_grants
		  where gateway = $1
		    and (actual = 'unknown'
		         or reconciled_at is null
		         or reconciled_at < now() - $2::interval
		         or (desired = 'active' and actual <> 'active')
		         or (desired = 'revoked' and actual not in ('revoked', 'missing')))
		  order by reconciled_at nulls first
		  limit $3`,
		defaultGateway(gateway), staleAfter.String(), limit)
	if err != nil {
		return nil, mapError(err, "list grants to reconcile")
	}
	defer rows.Close()
	out := []repo.Grant{}
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, mapError(err, "scan gateway grant")
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, "list grants to reconcile")
	}
	return out, nil
}

func (r grantRepo) query(ctx context.Context, what, sql string, args ...any) ([]repo.Grant, error) {
	rows, err := r.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError(err, what)
	}
	defer rows.Close()
	out := []repo.Grant{}
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, mapError(err, what)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, what)
	}
	return out, nil
}

// defaultGateway lets callers leave the field empty while there is only one.
// Naming it in the row from the start is what keeps a second gateway from
// being a migration.
func defaultGateway(name string) string {
	if name == "" {
		return "litellm"
	}
	return name
}
