package api

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/danielgtaylor/huma/v2"
	"ircord/backend/internal/storage"
)

// The instance-wide game high score tables (Hash Field, Ircoid): one table per game, shared by
// every account on the instance. A client submits its own score, so the server bounds what it
// accepts and how often. Every game uses the same bounds.
const (
	scoreMax     = 10_000_000
	scoreMaxName = 24
	// scoreGap is the shortest time between two submissions from one account to one game; no
	// game can end faster.
	scoreGap = 10 * time.Second
	// scoreHourly caps one account's submissions to one game per hour.
	scoreHourly = 20
	// scoreAccounts bounds one limiter's memory.
	scoreAccounts = 10_000
)

// scoreLimiter spaces and caps one game's submissions per account.
type scoreLimiter struct {
	mu       sync.Mutex
	accounts map[string][]time.Time
}

// allow records a submission for the account when it is within the limits.
func (l *scoreLimiter) allow(account string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.accounts == nil {
		l.accounts = map[string][]time.Time{}
	}
	hour := now.Add(-time.Hour)
	recent := l.accounts[account][:0]
	for _, at := range l.accounts[account] {
		if at.After(hour) {
			recent = append(recent, at)
		}
	}
	if len(recent) > 0 && now.Sub(recent[len(recent)-1]) < scoreGap || len(recent) >= scoreHourly {
		l.accounts[account] = recent
		return false
	}
	if _, known := l.accounts[account]; !known && len(l.accounts) >= scoreAccounts {
		for key, times := range l.accounts {
			if len(times) == 0 || !times[len(times)-1].After(hour) {
				delete(l.accounts, key)
			}
		}
		if len(l.accounts) >= scoreAccounts {
			return false
		}
	}
	l.accounts[account] = append(recent, now)
	return true
}

// The reasons a submitted name is refused.
const (
	scoreNameLength     = "enter a name of 1 to 24 characters"
	scoreNameCharacters = "that name has characters the table can't show"
)

// scoreRune reports whether a name may contain r: letters, marks, numbers, punctuation,
// symbols and spaces, plus the zero-width joiner that builds emoji sequences. Controls, other
// format characters (bidi overrides), private use, unassigned code points and U+FFFD, which
// stands for invalid input, are refused rather than shown.
func scoreRune(r rune) bool {
	if r == unicode.ReplacementChar {
		return false
	}
	return r == '\u200d' || unicode.IsSpace(r) || unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Zs)
}

// scoreName trims a submitted name and folds runs of whitespace. It returns the name, or the
// reason it is refused.
func scoreName(raw string) (string, string) {
	for _, r := range raw {
		if !scoreRune(r) {
			return "", scoreNameCharacters
		}
	}
	name := strings.Join(strings.Fields(raw), " ")
	if name == "" || len([]rune(name)) > scoreMaxName {
		return "", scoreNameLength
	}
	return name, ""
}

// scoreSubmission is one game's table behind the shared submission rules.
type scoreSubmission struct {
	// notScore is the problem reported for a score the game cannot produce.
	notScore string
	limiter  *scoreLimiter
	add      func(store *storage.Store, name string, score int64) (int64, []storage.GameScore, error)
}

func scoresUnavailable() error {
	return huma.Error503ServiceUnavailable("high scores are not available")
}

// submitScore validates and enters a score. It returns whether it placed, its rank (0 when it did
// not place) and the table after the submission.
func (s *Server) submitScore(ctx context.Context, game scoreSubmission, rawName string, score int64) (bool, int, []storage.GameScore, error) {
	if s.options.Store == nil {
		return false, 0, nil, scoresUnavailable()
	}
	name, problem := scoreName(rawName)
	if problem != "" {
		return false, 0, nil, huma.Error422UnprocessableEntity(problem)
	}
	if score <= 0 || score > scoreMax || score%10 != 0 {
		return false, 0, nil, huma.Error422UnprocessableEntity(game.notScore)
	}
	if !game.limiter.allow(owner(ctx), time.Now()) {
		return false, 0, nil, huma.Error429TooManyRequests("too many scores; try again shortly")
	}
	id, rows, err := game.add(s.options.Store, name, score)
	if err != nil {
		return false, 0, nil, scoresUnavailable()
	}
	rank := 0
	for i, row := range rows {
		if row.ID == id {
			rank = i + 1
		}
	}
	return id != 0, rank, rows, nil
}
