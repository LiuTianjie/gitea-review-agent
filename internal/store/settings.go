package store

import (
	"context"
	"database/sql"
	"fmt"
)

// GetSetting returns the value for key. found=false when the key is absent.
func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key=?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get setting: %w", err)
	}
	return value, true, nil
}

// SetSetting upserts a console-editable setting. isSecret flags values that
// should be redacted in the console UI.
func (s *Store) SetSetting(ctx context.Context, key, value string, isSecret bool) error {
	secret := 0
	if isSecret {
		secret = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings(key,value,is_secret,updated_at) VALUES(?,?,?,?)
		 ON CONFLICT(key) DO UPDATE SET
		   value=excluded.value,
		   is_secret=excluded.is_secret,
		   updated_at=excluded.updated_at`,
		key, value, secret, nowRFC3339())
	if err != nil {
		return fmt.Errorf("set setting: %w", err)
	}
	return nil
}

// AllSettings returns every setting as a key->value map.
func (s *Store) AllSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("all settings: %w", err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate settings: %w", err)
	}
	return out, nil
}
