package storage

import (
	"database/sql"
	"time"
)

// ScoreTableSize is how many entries each instance-wide game high score table keeps.
const ScoreTableSize = 10

// The game high score tables. Each has the columns id, name, score and created_ns and an index
// on (score DESC, id ASC). The names are constants because they are spliced into the queries.
const (
	hashFieldTable = "hash_field_scores"
	ircoidTable    = "ircoid_scores"
)

// GameScore is one entry of an instance-wide game high score table.
type GameScore struct {
	ID    int64
	Name  string
	Score int64
}

type queryer interface {
	Query(string, ...any) (*sql.Rows, error)
}

// gameScores returns a table, best first. Equal scores keep the order they were set in.
func gameScores(db queryer, table string) ([]GameScore, error) {
	rows, err := db.Query("SELECT id, name, score FROM "+table+" ORDER BY score DESC, id ASC LIMIT ?", ScoreTableSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []GameScore{}
	for rows.Next() {
		var v GameScore
		if err := rows.Scan(&v.ID, &v.Name, &v.Score); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

// addGameScore enters a score into a table and drops whatever falls out of the top entries. It
// returns the new entry's id, or 0 when the score did not place, and the table. A score must
// beat an existing entry to push it out: ties keep the earlier entry.
func (s *Store) addGameScore(table, name string, score int64) (int64, []GameScore, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec("INSERT INTO "+table+"(name, score, created_ns) VALUES(?,?,?)", name, score, time.Now().UnixNano())
	if err != nil {
		return 0, nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, nil, err
	}
	if _, err = tx.Exec("DELETE FROM "+table+" WHERE id NOT IN (SELECT id FROM "+table+" ORDER BY score DESC, id ASC LIMIT ?)", ScoreTableSize); err != nil {
		return 0, nil, err
	}
	entries, err := gameScores(tx, table)
	if err != nil {
		return 0, nil, err
	}
	if err = tx.Commit(); err != nil {
		return 0, nil, err
	}
	for _, entry := range entries {
		if entry.ID == id {
			return id, entries, nil
		}
	}
	return 0, entries, nil
}

// HashFieldScores returns the Hash Field table (Settings → About), best first.
func (s *Store) HashFieldScores() ([]GameScore, error) {
	return gameScores(s.DB, hashFieldTable)
}

// AddHashFieldScore enters a score into the Hash Field table; see addGameScore.
func (s *Store) AddHashFieldScore(name string, score int64) (int64, []GameScore, error) {
	return s.addGameScore(hashFieldTable, name, score)
}

// IrcoidScores returns the Ircoid table, best first.
func (s *Store) IrcoidScores() ([]GameScore, error) {
	return gameScores(s.DB, ircoidTable)
}

// AddIrcoidScore enters a score into the Ircoid table; see addGameScore.
func (s *Store) AddIrcoidScore(name string, score int64) (int64, []GameScore, error) {
	return s.addGameScore(ircoidTable, name, score)
}
