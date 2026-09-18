package dbstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type policyRepo struct{ q querier }

// Qualified with the table, because Current joins policy_current and both
// tables carry a version column.
const policyColumns = `policy_versions.version, policy_versions.content, policy_versions.note,
	policy_versions.created_by, policy_versions.created_at`

func scanPolicy(row scanner) (repo.PolicyVersion, error) {
	var p repo.PolicyVersion
	if err := row.Scan(&p.Version, &p.Content, &p.Note, &p.CreatedBy, &p.CreatedAt); err != nil {
		return repo.PolicyVersion{}, err
	}
	return p, nil
}

func (r policyRepo) Current(ctx context.Context) (repo.PolicyVersion, error) {
	p, err := scanPolicy(r.q.QueryRow(ctx,
		`select `+policyColumns+`
		   from policy_versions
		   join policy_current on policy_current.version = policy_versions.version`))
	if err != nil {
		return repo.PolicyVersion{}, mapError(err, "read current policy")
	}
	return p, nil
}

func (r policyRepo) ByVersion(ctx context.Context, version int64) (repo.PolicyVersion, error) {
	p, err := scanPolicy(r.q.QueryRow(ctx,
		`select `+policyColumns+` from policy_versions where version = $1`, version))
	if err != nil {
		return repo.PolicyVersion{}, mapError(err, "read policy")
	}
	return p, nil
}

// Publish stores a new version and points the fleet at it.
//
// The insert and the switch are one statement so that a version can never
// exist without the pointer having been considered, and so that two
// administrators publishing at once end with one of the two current rather
// than with a pointer to neither.
func (r policyRepo) Publish(ctx context.Context, content []byte, note, by string) (repo.PolicyVersion, error) {
	if !json.Valid(content) {
		// The column is jsonb and would reject it anyway; saying so here names
		// the caller's mistake instead of PostgreSQL's parser position.
		return repo.PolicyVersion{}, errors.New("publish policy: content is not valid JSON")
	}
	p, err := scanPolicy(r.q.QueryRow(ctx,
		`with published as (
		   insert into policy_versions (content, note, created_by)
		   values ($1::jsonb, $2, $3)
		   returning version, content, note, created_by, created_at
		 ), pointed as (
		   insert into policy_current (singleton, version, updated_at, updated_by)
		   select true, version, now(), $3 from published
		   on conflict (singleton) do update
		     set version = excluded.version, updated_at = now(), updated_by = excluded.updated_by
		 )
		 select version, content, note, created_by, created_at from published`,
		content, note, by))
	if err != nil {
		return repo.PolicyVersion{}, mapError(err, "publish policy")
	}
	return p, nil
}

// Rollback makes an older version current again.
//
// It does not copy the old content into a new version: the history should read
// as what happened -- "we went back to 7" -- rather than as a fresh decision
// that happens to look identical.
func (r policyRepo) Rollback(ctx context.Context, version int64, by string) (repo.PolicyVersion, error) {
	target, err := r.ByVersion(ctx, version)
	if err != nil {
		return repo.PolicyVersion{}, fmt.Errorf("roll back policy: %w", err)
	}
	if _, err := r.q.Exec(ctx,
		`insert into policy_current (singleton, version, updated_at, updated_by)
		 values (true, $1, now(), $2)
		 on conflict (singleton) do update
		   set version = excluded.version, updated_at = now(), updated_by = excluded.updated_by`,
		version, by); err != nil {
		return repo.PolicyVersion{}, mapError(err, "roll back policy")
	}
	return target, nil
}

func (r policyRepo) List(ctx context.Context, limit int) ([]repo.PolicyVersion, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.q.Query(ctx,
		`select `+policyColumns+` from policy_versions order by version desc limit $1`, limit)
	if err != nil {
		return nil, mapError(err, "list policies")
	}
	defer rows.Close()
	out := []repo.PolicyVersion{}
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, mapError(err, "scan policy")
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, "list policies")
	}
	return out, nil
}
