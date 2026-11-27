-- +goose Up
-- The instance-wide Ircoid high score table. Only the top entries are kept; the table starts
-- with two entries.
--
-- The starting scores are each author's lines added plus lines deleted over the history of
-- ircord-gpui at commit 2a3cd5f, binary files left out, the same rule that primed the Hash Field
-- table. They are set once, here, and never refreshed:
--   git log 2a3cd5f --numstat --format='@%aN' \
--     | awk '/^@/{a=substr($0,2);next} NF==3 && $1!="-"{t[a]+=$1+$2} END{for(k in t) print t[k], k}'
CREATE TABLE IF NOT EXISTS ircoid_scores (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    score INTEGER NOT NULL,
    created_ns INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ircoid_scores_rank ON ircoid_scores(score DESC, id ASC);
INSERT INTO ircoid_scores(name, score, created_ns) VALUES
    ('Peter Petermann', 225357, 0),
    ('Michael Karbowiak', 2102, 0);
