package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"ircord/backend/internal/voice"
)

type voiceRoomInput struct {
	ID   string `path:"id" maxLength:"128"`
	Body voice.RoomSettings
}

func voiceRoomError(err error) error {
	switch {
	case errors.Is(err, voice.ErrRoomSettings):
		return huma.Error422UnprocessableEntity(err.Error())
	case errors.Is(err, voice.ErrRoom):
		return huma.Error404NotFound(err.Error())
	case errors.Is(err, voice.ErrRoomExists), errors.Is(err, voice.ErrRoomLimit):
		return huma.Error409Conflict(err.Error())
	default:
		return huma.Error503ServiceUnavailable(voice.ErrUnavailable.Error())
	}
}

func voiceUser(ctx context.Context) voice.User {
	p := principal(ctx)
	return voice.User{ID: p.Account.ID, Name: p.Account.Username, Session: p.Session.ID}
}

func (s *Server) saveVoiceRoom(ctx context.Context, id string, settings voice.RoomSettings) (*Output[voice.Room], error) {
	actor, err := s.requireOwner(ctx)
	if err != nil {
		return nil, err
	}
	if s.options.Voice == nil {
		return nil, voiceRoomError(voice.ErrUnavailable)
	}
	room, err := s.options.Voice.SaveRoom(ctx, voiceUser(ctx), id, settings)
	if err != nil {
		return nil, voiceRoomError(err)
	}
	action := "update-voice-room"
	if id == "" {
		action = "create-voice-room"
	}
	s.audit(actor, action, room.ID, "", "ok")
	return result(room, nil)
}

func (s *Server) registerVoice(router chi.Router) {
	// Explicitly route only LiveKit's signaling endpoints. Its control API stays private.
	if s.options.Voice != nil {
		for _, suffix := range []string{"/rtc", "/rtc/validate", "/rtc/v1", "/rtc/v1/validate"} {
			router.Get("/voice"+suffix, s.options.Voice.Signal)
		}
	}
	huma.Register(s.API, huma.Operation{OperationID: "list-voice-rooms", Method: http.MethodGet, Path: "/api/v1/voice/rooms"}, func(ctx context.Context, _ *struct{}) (*Output[[]voice.Room], error) {
		if s.options.Voice == nil {
			return result([]voice.Room{}, nil)
		}
		if user := voiceUser(ctx); user.ID == "" || user.Session == "" {
			return nil, huma.Error401Unauthorized("Sign in to use voice.")
		}
		return result(s.options.Voice.Rooms(), nil)
	})
	huma.Register(s.API, huma.Operation{OperationID: "create-voice-room", Method: http.MethodPost, Path: "/api/v1/voice/rooms", DefaultStatus: http.StatusCreated}, func(ctx context.Context, in *struct{ Body voice.RoomSettings }) (*Output[voice.Room], error) {
		return s.saveVoiceRoom(ctx, "", in.Body)
	})
	huma.Register(s.API, huma.Operation{OperationID: "update-voice-room", Method: http.MethodPatch, Path: "/api/v1/voice/rooms/{id}"}, func(ctx context.Context, in *voiceRoomInput) (*Output[voice.Room], error) {
		return s.saveVoiceRoom(ctx, in.ID, in.Body)
	})
	huma.Register(s.API, huma.Operation{OperationID: "delete-voice-room", Method: http.MethodDelete, Path: "/api/v1/voice/rooms/{id}", DefaultStatus: http.StatusNoContent}, func(ctx context.Context, in *struct {
		ID string `path:"id" maxLength:"128"`
	}) (*struct{}, error) {
		actor, err := s.requireOwner(ctx)
		if err != nil {
			return nil, err
		}
		if s.options.Voice == nil {
			return nil, voiceRoomError(voice.ErrUnavailable)
		}
		if err := s.options.Voice.DeleteRoom(ctx, voiceUser(ctx), in.ID); err != nil {
			return nil, voiceRoomError(err)
		}
		s.audit(actor, "delete-voice-room", in.ID, "", "ok")
		return nil, nil
	})
	huma.Register(s.API, huma.Operation{OperationID: "join-voice-room", Method: http.MethodPost, Path: "/api/v1/voice/rooms/{id}/join"}, func(ctx context.Context, in *struct {
		ID   string `path:"id" maxLength:"128"`
		Echo bool   `query:"echo" doc:"Open a private microphone test for the requesting account."`
	}) (*Output[voice.Credential], error) {
		if s.options.Voice == nil {
			return nil, huma.Error503ServiceUnavailable("Voice is unavailable on this server.")
		}
		credential, err := s.options.Voice.Join(voiceUser(ctx), in.ID, in.Echo)
		switch {
		case errors.Is(err, voice.ErrRoom):
			return nil, huma.Error404NotFound(err.Error())
		case errors.Is(err, voice.ErrSession), errors.Is(err, voice.ErrFull), errors.Is(err, voice.ErrEchoBusy):
			return nil, huma.Error409Conflict(err.Error())
		case err != nil:
			return nil, huma.Error503ServiceUnavailable("Voice is temporarily unavailable.")
		}
		return result(credential, nil)
	})
	huma.Register(s.API, huma.Operation{OperationID: "leave-voice-room", Method: http.MethodDelete, Path: "/api/v1/voice/sessions/{id}", DefaultStatus: http.StatusNoContent}, func(ctx context.Context, in *struct {
		ID string `path:"id" maxLength:"64"`
	}) (*struct{}, error) {
		if s.options.Voice != nil {
			s.options.Voice.Leave(ctx, voiceUser(ctx), in.ID)
		}
		return nil, nil
	})
}
