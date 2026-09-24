package dbstore

import (
	"context"
	"errors"
)

type bundleRepo struct{ q querier }

func (r bundleRepo) Put(ctx context.Context, employeeID string, epoch int, zip []byte, etag string) error {
	if employeeID == "" || len(zip) == 0 || etag == "" {
		return errors.New("store credential bundle: employee, bytes and etag are required")
	}
	_, err := r.q.Exec(ctx,
		`insert into credential_bundles (employee_id, epoch, zip, etag) values ($1, $2, $3, $4)
		 on conflict (employee_id, epoch) do update set zip = excluded.zip, etag = excluded.etag, created_at = now()`,
		employeeID, epoch, zip, etag)
	return mapError(err, "store credential bundle")
}

func (r bundleRepo) Live(ctx context.Context, employeeID string) ([]byte, string, error) {
	var zip []byte
	var etag string
	err := r.q.QueryRow(ctx,
		`select b.zip, b.etag from credential_bundles b
		   join employees e on e.id = b.employee_id and e.auth_epoch = b.epoch
		  where b.employee_id = $1`, employeeID).Scan(&zip, &etag)
	if err != nil {
		return nil, "", mapError(err, "read credential bundle")
	}
	return zip, etag, nil
}

func (r bundleRepo) LiveETag(ctx context.Context, employeeID string) (string, error) {
	var etag string
	err := r.q.QueryRow(ctx,
		`select b.etag from credential_bundles b
		   join employees e on e.id = b.employee_id and e.auth_epoch = b.epoch
		  where b.employee_id = $1`, employeeID).Scan(&etag)
	if err != nil {
		return "", mapError(err, "read credential bundle etag")
	}
	return etag, nil
}

func (r bundleRepo) Purge(ctx context.Context, employeeID string) error {
	_, err := r.q.Exec(ctx, `delete from credential_bundles where employee_id = $1`, employeeID)
	return mapError(err, "purge credential bundles")
}
