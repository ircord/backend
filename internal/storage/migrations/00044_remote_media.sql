-- +goose Up
-- Profile pictures hosted on other IRCord servers, copied here so clients draw
-- them from this server. key is the capability in the /media/remote/{key} address.
-- sha names the content-addressed file; it is empty until the first fetch succeeds
-- and again after the picture is evicted. Times are Unix nanoseconds, zero when unset.
CREATE TABLE IF NOT EXISTS remote_media (
    key TEXT PRIMARY KEY,
    url TEXT NOT NULL,
    kind TEXT NOT NULL,
    sha TEXT NOT NULL DEFAULT '',
    content_type TEXT NOT NULL DEFAULT '',
    size INTEGER NOT NULL DEFAULT 0,
    fetched_ns INTEGER NOT NULL DEFAULT 0,
    expires_ns INTEGER NOT NULL DEFAULT 0,
    failed_ns INTEGER NOT NULL DEFAULT 0,
    accessed_ns INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS remote_media_accessed ON remote_media(accessed_ns) WHERE sha != '';

-- No down migration: the table only caches pictures that can be fetched again.
