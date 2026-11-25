package storage

import (
	"context"
	"database/sql"
	"errors"
)

func (s *Store) VoiceRoomCatalog(ctx context.Context, network string) ([]byte, error) {
	var payload []byte
	err := s.DB.QueryRowContext(ctx, "SELECT payload FROM voice_room_catalogs WHERE network=?", network).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return payload, err
}

func (s *Store) SaveVoiceRoomCatalog(ctx context.Context, network string, payload []byte) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO voice_room_catalogs(network,payload) VALUES(?,?)
		ON CONFLICT(network) DO UPDATE SET payload=excluded.payload`, network, payload)
	return err
}
