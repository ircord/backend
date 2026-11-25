-- +goose Up
CREATE TABLE IF NOT EXISTS voice_room_catalogs (
    network TEXT PRIMARY KEY,
    payload BLOB NOT NULL
);

-- +goose Down
DROP TABLE voice_room_catalogs;
