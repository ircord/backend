package api

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
	"ircord/backend/internal/storage"
)

// The Hash Field high score table (Settings → About). The shared rules are in scores.go.

type HashFieldEntry struct {
	Rank  int    `json:"rank" doc:"Place in the table, starting at 1"`
	Name  string `json:"name"`
	Score int64  `json:"score"`
}
type HashFieldTable struct {
	Items []HashFieldEntry `json:"items" doc:"The top entries, best first"`
}
type HashFieldResult struct {
	Placed bool             `json:"placed" doc:"Whether the score made the table"`
	Rank   int              `json:"rank" doc:"The new entry's place, or 0 when it did not place"`
	Items  []HashFieldEntry `json:"items" doc:"The table after the submission, best first"`
}
type HashFieldScoreInput struct {
	Body struct {
		Name  string `json:"name" minLength:"1" maxLength:"64" doc:"Name shown in the table; trimmed, at most 24 characters"`
		Score int64  `json:"score" minimum:"10" maximum:"10000000" multipleOf:"10" doc:"Final score of one game"`
	}
}

func hashFieldEntries(rows []storage.GameScore) []HashFieldEntry {
	items := make([]HashFieldEntry, len(rows))
	for i, row := range rows {
		items[i] = HashFieldEntry{Rank: i + 1, Name: row.Name, Score: row.Score}
	}
	return items
}

func (s *Server) registerHashField() {
	game := scoreSubmission{notScore: "that is not a Hash Field score", limiter: &s.hashField, add: (*storage.Store).AddHashFieldScore}
	huma.Register(s.API, huma.Operation{OperationID: "get-hash-field-scores", Method: "GET", Path: "/api/v1/hash-field/scores", Summary: "List Hash Field high scores", Description: "The instance-wide top 10 of the Hash Field game in Settings → About."}, func(ctx context.Context, _ *struct{}) (*Output[HashFieldTable], error) {
		if s.options.Store == nil {
			return nil, scoresUnavailable()
		}
		rows, err := s.options.Store.HashFieldScores()
		if err != nil {
			return nil, scoresUnavailable()
		}
		return &Output[HashFieldTable]{Body: HashFieldTable{Items: hashFieldEntries(rows)}}, nil
	})
	huma.Register(s.API, huma.Operation{OperationID: "submit-hash-field-score", Method: "POST", Path: "/api/v1/hash-field/scores", Summary: "Submit a Hash Field score", Description: "Enters a finished game's score into the instance-wide top 10. A score that does not place is not stored. One account may submit once every 10 seconds and 20 times an hour."}, func(ctx context.Context, in *HashFieldScoreInput) (*Output[HashFieldResult], error) {
		placed, rank, rows, err := s.submitScore(ctx, game, in.Body.Name, in.Body.Score)
		if err != nil {
			return nil, err
		}
		return &Output[HashFieldResult]{Body: HashFieldResult{Placed: placed, Rank: rank, Items: hashFieldEntries(rows)}}, nil
	})
}
