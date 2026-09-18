package dbstore

import (
	"context"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type legacyRepo struct{ q querier }

// Record maps an old name to the row it became. Recording it twice for the
// same name is fine and expected: the importer runs repeatedly, and the second
// run must not fail on the work the first one did.
func (r legacyRepo) Record(ctx context.Context, kind, legacyKey, newID string) error {
	if _, err := r.q.Exec(ctx,
		`insert into legacy_id_map (kind, legacy_key, new_id) values ($1, $2, $3)
		 on conflict (kind, legacy_key) do update set new_id = excluded.new_id`,
		kind, legacyKey, newID); err != nil {
		return mapError(err, "record the old name")
	}
	return nil
}

func (r legacyRepo) Lookup(ctx context.Context, kind, legacyKey string) (string, error) {
	var id string
	if err := r.q.QueryRow(ctx,
		`select new_id from legacy_id_map where kind = $1 and legacy_key = $2`,
		kind, legacyKey).Scan(&id); err != nil {
		return "", mapError(err, "look up the old name")
	}
	return id, nil
}

var _ repo.LegacyIDs = legacyRepo{}
