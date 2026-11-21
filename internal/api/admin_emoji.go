package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"ircord/backend/internal/emotes"
	"ircord/backend/internal/storage"
)

// EmojiManagement is who may manage the shared emoji catalog. Unrestricted, every signed-in
// account manages every entry; restricted, only the managers and server owners do.
type EmojiManagement struct {
	Restricted bool                `json:"restricted"`
	Managers   []storage.AdminUser `json:"managers" nullable:"false"`
}
type EmojiManagementUpdate struct {
	Restricted bool     `json:"restricted"`
	Managers   []string `json:"managers" nullable:"false" maxItems:"200" doc:"IRCord account IDs, as returned by the user list or identity lookup"`
}

func (s *Server) emojiManagement(ctx context.Context) (EmojiManagement, error) {
	policy := s.options.Emotes.Policy()
	out := EmojiManagement{Restricted: policy.Restricted, Managers: []storage.AdminUser{}}
	for _, id := range policy.Managers {
		user, err := s.options.Store.AdminUser(ctx, id)
		// A deleted account drops out of the list.
		if errors.Is(err, storage.ErrNotFound) || err == nil && user.ID != id {
			continue
		}
		if err != nil {
			return out, huma.Error503ServiceUnavailable("account storage is unavailable")
		}
		out.Managers = append(out.Managers, user)
	}
	return out, nil
}

func (s *Server) registerEmojiAdministration() {
	if s.options.Emotes == nil {
		return
	}
	huma.Register(s.API, huma.Operation{OperationID: "administration-emoji", Method: http.MethodGet, Path: administrationPrefix + "/emoji", Summary: "Who may manage the emoji catalog"}, func(ctx context.Context, _ *struct{}) (*Output[EmojiManagement], error) {
		if _, err := s.requireOwner(ctx); err != nil {
			return nil, err
		}
		return s.emojiManagementOutput(ctx)
	})
	huma.Register(s.API, huma.Operation{OperationID: "administration-update-emoji", Method: http.MethodPut, Path: administrationPrefix + "/emoji", Summary: "Limit emoji management to a list of accounts, or open it to everyone", MaxBodyBytes: 32768}, func(ctx context.Context, in *struct{ Body EmojiManagementUpdate }) (*Output[EmojiManagement], error) {
		actor, err := s.requireOwner(ctx)
		if err != nil {
			return nil, err
		}
		for _, id := range in.Body.Managers {
			user, err := s.options.Store.AdminUser(ctx, id)
			if errors.Is(err, storage.ErrNotFound) || err == nil && user.ID != id {
				return nil, huma.Error422UnprocessableEntity("One of the chosen accounts doesn't exist.")
			}
			if err != nil {
				return nil, huma.Error503ServiceUnavailable("account storage is unavailable")
			}
		}
		if err = s.options.Emotes.SetPolicy(emotes.Policy{Restricted: in.Body.Restricted, Managers: in.Body.Managers}); err != nil {
			return nil, emoteError(err)
		}
		outcome := "everyone"
		if in.Body.Restricted {
			outcome = "managers: " + strconv.Itoa(len(s.options.Emotes.Policy().Managers))
		}
		s.audit(actor, "emoji-management", "", "", outcome)
		return s.emojiManagementOutput(ctx)
	})
}

func (s *Server) emojiManagementOutput(ctx context.Context) (*Output[EmojiManagement], error) {
	m, err := s.emojiManagement(ctx)
	if err != nil {
		return nil, err
	}
	return &Output[EmojiManagement]{Body: m}, nil
}
