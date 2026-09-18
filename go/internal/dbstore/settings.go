package dbstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

type settingsRepo struct{ q querier }

const settingColumns = `key, value, version, updated_at, updated_by`

func scanSetting(row scanner) (repo.Setting, error) {
	var s repo.Setting
	if err := row.Scan(&s.Key, &s.Value, &s.Version, &s.UpdatedAt, &s.UpdatedBy); err != nil {
		return repo.Setting{}, err
	}
	return s, nil
}

func (r settingsRepo) Get(ctx context.Context, key string) (repo.Setting, error) {
	s, err := scanSetting(r.q.QueryRow(ctx,
		`select `+settingColumns+` from settings where key = $1`, key))
	if err != nil {
		return repo.Setting{}, mapError(err, "read setting "+key)
	}
	return s, nil
}

func (r settingsRepo) Set(ctx context.Context, key string, value []byte, expectVersion int, by string) (repo.Setting, error) {
	if key == "" {
		return repo.Setting{}, errors.New("save setting: key must not be empty")
	}
	if !json.Valid(value) {
		return repo.Setting{}, fmt.Errorf("save setting %s: value is not valid JSON", key)
	}

	if expectVersion == 0 {
		s, err := scanSetting(r.q.QueryRow(ctx,
			`insert into settings (key, value, updated_by) values ($1, $2::jsonb, $3)
			 returning `+settingColumns, key, value, by))
		if err != nil {
			if isUniqueViolation(err) {
				// The caller believed nothing was stored. Being wrong about
				// that and overwriting anyway is how a default quota somebody
				// set on purpose goes back to the built-in one.
				return repo.Setting{}, fmt.Errorf("save setting %s: %w (a value is already stored)", key, repo.ErrConflict)
			}
			return repo.Setting{}, mapError(err, "save setting "+key)
		}
		return s, nil
	}

	s, err := scanSetting(r.q.QueryRow(ctx,
		`update settings
		    set value = $3::jsonb, version = version + 1, updated_at = now(), updated_by = $4
		  where key = $1 and version = $2
		  returning `+settingColumns, key, expectVersion, value, by))
	if err == nil {
		return s, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		current, lookupErr := r.Get(ctx, key)
		if lookupErr != nil {
			return repo.Setting{}, fmt.Errorf("save setting %s: %w", key, lookupErr)
		}
		return repo.Setting{}, fmt.Errorf("save setting %s: %w (read version %d, now %d)",
			key, repo.ErrConflict, expectVersion, current.Version)
	}
	return repo.Setting{}, mapError(err, "save setting "+key)
}

func (r settingsRepo) List(ctx context.Context) ([]repo.Setting, error) {
	rows, err := r.q.Query(ctx, `select `+settingColumns+` from settings order by key`)
	if err != nil {
		return nil, mapError(err, "list settings")
	}
	defer rows.Close()
	out := []repo.Setting{}
	for rows.Next() {
		s, err := scanSetting(rows)
		if err != nil {
			return nil, mapError(err, "scan setting")
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, "list settings")
	}
	return out, nil
}
