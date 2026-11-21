package emotes

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
)

// Policy decides who manages the whole catalog. Unrestricted, every signed-in account does.
// Restricted, only the listed accounts (and server owners) do; everyone else manages what they
// added.
type Policy struct {
	Restricted bool     `json:"restricted"`
	Managers   []string `json:"managers" nullable:"false" doc:"IRCord account IDs that manage every entry while restricted."`
}

func (s *Service) loadPolicy() error {
	var raw string
	err := s.options.Store.DB.QueryRow("SELECT value FROM emote_state WHERE key='policy'").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), &s.policy)
}

func (s *Service) Policy() Policy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Policy{Restricted: s.policy.Restricted, Managers: append([]string{}, s.policy.Managers...)}
}

// SetPolicy stores p and publishes a new catalog version, since what each account may manage
// changes with it.
func (s *Service) SetPolicy(p Policy) error {
	managers := []string{}
	for _, id := range p.Managers {
		if id != "" && !slices.Contains(managers, id) {
			managers = append(managers, id)
		}
	}
	p.Managers = managers
	raw, _ := json.Marshal(p)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.options.Store.DB.Exec("INSERT INTO emote_state(key,value) VALUES('policy',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(raw)); err != nil {
		return fail(ErrProvider, "Emote storage is unavailable.")
	}
	s.policy = p
	return s.commit(nil, nil, "", nil)
}
