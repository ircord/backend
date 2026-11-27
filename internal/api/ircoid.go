package api

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
	"ircord/backend/internal/storage"
)

// The Ircoid high score table, kept apart from Hash Field's because the games score on
// different scales. The shared rules are in scores.go.

type IrcoidEntry struct {
	Rank  int    `json:"rank" doc:"Place in the table, starting at 1"`
	Name  string `json:"name"`
	Score int64  `json:"score"`
}
type IrcoidTable struct {
	Items []IrcoidEntry `json:"items" doc:"The top entries, best first"`
}
type IrcoidResult struct {
	Placed bool          `json:"placed" doc:"Whether the score made the table"`
	Rank   int           `json:"rank" doc:"The new entry's place, or 0 when it did not place"`
	Items  []IrcoidEntry `json:"items" doc:"The table after the submission, best first"`
}
type IrcoidScoreInput struct {
	Body struct {
		Name  string `json:"name" minLength:"1" maxLength:"64" doc:"Name shown in the table; trimmed, at most 24 characters"`
		Score int64  `json:"score" minimum:"10" maximum:"10000000" multipleOf:"10" doc:"Final score of one game"`
	}
}

func ircoidEntries(rows []storage.GameScore) []IrcoidEntry {
	items := make([]IrcoidEntry, len(rows))
	for i, row := range rows {
		items[i] = IrcoidEntry{Rank: i + 1, Name: row.Name, Score: row.Score}
	}
	return items
}

func (s *Server) registerIrcoid() {
	game := scoreSubmission{notScore: "that is not an Ircoid score", limiter: &s.ircoid, add: (*storage.Store).AddIrcoidScore}
	huma.Register(s.API, huma.Operation{OperationID: "get-ircoid-scores", Method: "GET", Path: "/api/v1/ircoid/scores", Summary: "List Ircoid high scores", Description: "The instance-wide top 10 of the Ircoid game."}, func(ctx context.Context, _ *struct{}) (*Output[IrcoidTable], error) {
		if s.options.Store == nil {
			return nil, scoresUnavailable()
		}
		rows, err := s.options.Store.IrcoidScores()
		if err != nil {
			return nil, scoresUnavailable()
		}
		return &Output[IrcoidTable]{Body: IrcoidTable{Items: ircoidEntries(rows)}}, nil
	})
	huma.Register(s.API, huma.Operation{OperationID: "submit-ircoid-score", Method: "POST", Path: "/api/v1/ircoid/scores", Summary: "Submit an Ircoid score", Description: "Enters a finished game's score into the instance-wide top 10. A score that does not place is not stored. One account may submit once every 10 seconds and 20 times an hour."}, func(ctx context.Context, in *IrcoidScoreInput) (*Output[IrcoidResult], error) {
		placed, rank, rows, err := s.submitScore(ctx, game, in.Body.Name, in.Body.Score)
		if err != nil {
			return nil, err
		}
		return &Output[IrcoidResult]{Body: IrcoidResult{Placed: placed, Rank: rank, Items: ircoidEntries(rows)}}, nil
	})
}
