package dbstore

import (
	"context"
	"errors"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type credentialRepo struct{ q querier }

const credentialColumns = `id, employee_id, epoch, purpose, ciphertext, key_version,
	content_sha256, created_at, retired_at`

func scanCredential(row scanner) (repo.Credential, error) {
	var c repo.Credential
	var retiredAt *time.Time
	err := row.Scan(&c.ID, &c.EmployeeID, &c.Epoch, &c.Purpose, &c.Ciphertext,
		&c.KeyVersion, &c.ContentSHA256, &c.CreatedAt, &retiredAt)
	if err != nil {
		return repo.Credential{}, err
	}
	c.RetiredAt = retiredAt
	return c, nil
}

func (r credentialRepo) Live(ctx context.Context, employeeID, purpose string) (repo.Credential, error) {
	c, err := scanCredential(r.q.QueryRow(ctx,
		`select `+credentialColumns+` from credential_versions
		  where employee_id = $1 and purpose = $2 and retired_at is null`,
		employeeID, purpose))
	if err != nil {
		return repo.Credential{}, mapError(err, "read credential")
	}
	return c, nil
}

func (r credentialRepo) ByID(ctx context.Context, id string) (repo.Credential, error) {
	c, err := scanCredential(r.q.QueryRow(ctx,
		`select `+credentialColumns+` from credential_versions where id = $1`, id))
	if err != nil {
		return repo.Credential{}, mapError(err, "read credential")
	}
	return c, nil
}

// Store saves a new credential and retires the one it replaces.
//
// Two live credentials for one purpose would leave the exporter free to
// deliver either, and "which token does that machine actually have" would stop
// having an answer. The partial unique index enforces that, which is also why
// the retirement is a separate statement that has to run first: both rows in
// one statement are checked against the index before the retirement is
// visible, and the insert is refused.
//
// Call this inside InTx, with the grant and the task it belongs to. On its own
// there is a moment between the two statements in which the employee has no
// live credential -- harmless for a reader, but not something to rely on.
func (r credentialRepo) Store(ctx context.Context, n repo.NewCredential) (repo.Credential, error) {
	if n.EmployeeID == "" || n.Purpose == "" {
		return repo.Credential{}, errors.New("store credential: employee and purpose are required")
	}
	if len(n.Ciphertext) == 0 {
		return repo.Credential{}, errors.New("store credential: nothing to store")
	}
	if n.KeyVersion == "" {
		// Without the key version the row cannot be opened again, ever. It is
		// cheaper to refuse it than to find out during an incident.
		return repo.Credential{}, errors.New("store credential: the master key version is required")
	}
	if n.Epoch <= 0 {
		return repo.Credential{}, errors.New("store credential: an epoch is required")
	}

	if _, err := r.Retire(ctx, n.EmployeeID, n.Purpose); err != nil {
		return repo.Credential{}, err
	}
	c, err := scanCredential(r.q.QueryRow(ctx,
		`insert into credential_versions
		   (employee_id, epoch, purpose, ciphertext, key_version, content_sha256)
		 values ($1, $2, $3, $4, $5, $6)
		 returning `+credentialColumns,
		n.EmployeeID, n.Epoch, n.Purpose, n.Ciphertext, n.KeyVersion, n.ContentSHA256))
	if err != nil {
		return repo.Credential{}, mapError(err, "store credential")
	}
	return c, nil
}

// Retire ends the live credential without issuing a replacement, which is what
// offboarding does. Having none is not an error: the caller is asking for a
// state, not for an event.
func (r credentialRepo) Retire(ctx context.Context, employeeID, purpose string) (int, error) {
	tag, err := r.q.Exec(ctx,
		`update credential_versions set retired_at = now()
		  where employee_id = $1 and purpose = $2 and retired_at is null`,
		employeeID, purpose)
	if err != nil {
		return 0, mapError(err, "retire credential")
	}
	return int(tag.RowsAffected()), nil
}
