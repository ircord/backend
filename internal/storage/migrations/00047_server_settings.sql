-- +goose Up
-- Server-wide settings that server owners change in Administration, one row per
-- setting. The first is the end-to-end encryption mode (ircord/issues#19).
CREATE TABLE IF NOT EXISTS server_settings (
    name TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_ns INTEGER NOT NULL,
    updated_by TEXT NOT NULL
);

-- +goose Down
DROP TABLE server_settings;
