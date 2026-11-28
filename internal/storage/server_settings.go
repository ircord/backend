package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ServerSetting is one saved server-wide setting. A setting that was never saved
// has an empty Value and a zero UpdatedAt.
type ServerSetting struct {
	Value     string
	UpdatedAt time.Time
	UpdatedBy string
}

// SettingEncryption is the end-to-end encryption mode: "off" or "experimental".
const SettingEncryption = "encryption"

// ServerSetting reads a server-wide setting; a setting that was never saved
// returns the zero value.
func (s *Store) ServerSetting(ctx context.Context, name string) (ServerSetting, error) {
	var v ServerSetting
	var ns int64
	err := s.DB.QueryRowContext(ctx, "SELECT value, updated_ns, updated_by FROM server_settings WHERE name=?", name).Scan(&v.Value, &ns, &v.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return ServerSetting{}, nil
	}
	if err != nil {
		return ServerSetting{}, err
	}
	v.UpdatedAt = time.Unix(0, ns).UTC()
	return v, nil
}

// SaveServerSetting stores a server-wide setting and who changed it.
func (s *Store) SaveServerSetting(ctx context.Context, name, value, by string) error {
	_, err := s.DB.ExecContext(ctx, "INSERT INTO server_settings(name, value, updated_ns, updated_by) VALUES(?,?,?,?) ON CONFLICT(name) DO UPDATE SET value=excluded.value, updated_ns=excluded.updated_ns, updated_by=excluded.updated_by", name, value, time.Now().UnixNano(), by)
	return err
}
