package voice

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/twitchtv/twirp"
)

const (
	maxVoiceRooms  = 32
	defaultBitrate = 48
	serverCatalog  = "server"
)

var (
	ErrRoomExists   = errors.New("A voice channel with this name already exists.")
	ErrRoomLimit    = errors.New("This server has reached its voice channel limit.")
	ErrRoomSettings = errors.New("Enter a channel name and a supported audio bitrate.")
)

type roomCatalog interface {
	VoiceRoomCatalog(context.Context, string) ([]byte, error)
	SaveVoiceRoomCatalog(context.Context, string, []byte) error
}

type roomDefinition struct {
	MediaRoom   string `json:"mediaRoom"`
	Name        string `json:"name"`
	BitrateKbps int    `json:"bitrateKbps"`
}

type RoomSettings struct {
	Name        string `json:"name" minLength:"1" maxLength:"60"`
	BitrateKbps int    `json:"bitrateKbps" enum:"24,32,48,64,96,128,160,192"`
}

// OpenCatalog runs before serving requests. Import the legacy IRC catalog once and
// give its rooms fresh media identities, so pre-migration media never mixes with
// the server-wide rooms. Preserve the display names and audio settings.
func (s *Service) OpenCatalog(ctx context.Context, store roomCatalog, legacyScope string) error {
	s.catalog = store
	payload, err := store.VoiceRoomCatalog(ctx, serverCatalog)
	if err != nil {
		return err
	}
	if payload != nil {
		return s.loadCatalog(payload, false)
	}
	if legacyScope != "" {
		legacy, err := store.VoiceRoomCatalog(ctx, legacyScope)
		if err != nil {
			return err
		}
		if legacy != nil {
			if err := s.loadCatalog(legacy, true); err != nil {
				return err
			}
		}
	}
	return s.saveCatalog(ctx, s.rooms)
}

func (s *Service) loadCatalog(payload []byte, migrate bool) error {
	var saved map[string]roomDefinition
	if err := json.Unmarshal(payload, &saved); err != nil {
		return err
	}
	rooms := make(map[string]*roomState, len(saved))
	for id, entry := range saved {
		if migrate {
			id = rand.Text()
			entry.MediaRoom = "ircord-" + id
		}
		rooms[id] = &roomState{name: entry.MediaRoom, displayName: entry.Name, bitrateKbps: entry.BitrateKbps}
	}
	s.rooms = rooms
	return nil
}

func (s *Service) saveCatalog(ctx context.Context, rooms map[string]*roomState) error {
	saved := make(map[string]roomDefinition, len(rooms))
	for id, room := range rooms {
		saved[id] = roomDefinition{MediaRoom: room.name, Name: room.displayName, BitrateKbps: room.bitrateKbps}
	}
	payload, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	return s.catalog.SaveVoiceRoomCatalog(ctx, serverCatalog, payload)
}

func (room *roomState) view(id string) Room {
	return Room{ID: id, Name: room.displayName, BitrateKbps: room.bitrateKbps,
		Participants: append([]string{}, room.participants...), Capacity: capacity, Available: room.ready}
}

func (s *Service) SaveRoom(ctx context.Context, user User, id string, input RoomSettings) (Room, error) {
	if s.catalog == nil {
		return Room{}, ErrUnavailable
	}
	if !s.validSession(user) {
		return Room{}, ErrRoom
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || utf8.RuneCountInString(input.Name) > 60 || strings.ContainsFunc(input.Name, unicode.IsControl) ||
		!slices.Contains([]int{24, 32, 48, 64, 96, 128, 160, 192}, input.BitrateKbps) {
		return Room{}, ErrRoomSettings
	}
	s.mu.Lock()
	room, err := s.saveRoom(ctx, id, input)
	s.mu.Unlock()
	if err == nil {
		s.notify()
	}
	return room, err
}

func (s *Service) saveRoom(ctx context.Context, id string, input RoomSettings) (Room, error) {
	current := s.rooms[id]
	if id != "" && current == nil {
		return Room{}, ErrRoom
	}
	if id == "" && len(s.rooms) >= maxVoiceRooms {
		return Room{}, ErrRoomLimit
	}
	if current == nil || !strings.EqualFold(current.displayName, input.Name) {
		for _, room := range s.rooms {
			if strings.EqualFold(room.displayName, input.Name) {
				return Room{}, ErrRoomExists
			}
		}
	}
	var next roomState
	if current == nil {
		id = rand.Text()
		next.name = "ircord-" + id
	} else {
		next = *current
	}
	next.displayName, next.bitrateKbps = input.Name, input.BitrateKbps
	rooms := maps.Clone(s.rooms)
	rooms[id] = &next
	if err := s.saveCatalog(ctx, rooms); err != nil {
		return Room{}, ErrUnavailable
	}
	s.rooms = rooms
	return next.view(id), nil
}

func (s *Service) DeleteRoom(ctx context.Context, user User, id string) error {
	if s.catalog == nil {
		return ErrUnavailable
	}
	if !s.validSession(user) {
		return ErrRoom
	}
	s.mu.Lock()
	ended, err := s.deleteRoom(ctx, id)
	s.mu.Unlock()
	var work sync.WaitGroup
	for _, current := range ended {
		if current.listener != "" {
			work.Go(func() { s.removeSession(ctx, current) })
		}
	}
	work.Wait()
	if err == nil || len(ended) > 0 {
		s.notify()
	}
	return err
}

func (s *Service) deleteRoom(ctx context.Context, id string) ([]*session, error) {
	room := s.rooms[id]
	if room == nil {
		return nil, ErrRoom
	}
	if ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	// Revoke signaling before the media server closes the room. On failure,
	// reconciliation cleans up the remaining connections and reopens the saved room.
	room.ready = false
	var ended []*session
	for _, current := range s.sessions {
		if current.roomID == id {
			ended = append(ended, current)
			s.drop(current)
		}
	}
	rpc, err := s.roomRPC(ctx, &auth.VideoGrant{RoomCreate: true})
	if err != nil {
		return ended, ErrUnavailable
	}
	if _, err = s.client.DeleteRoom(rpc, &livekit.DeleteRoomRequest{Room: room.name}); err != nil {
		var failure twirp.Error
		if !errors.As(err, &failure) || failure.Code() != twirp.NotFound {
			return ended, ErrUnavailable
		}
	}
	rooms := maps.Clone(s.rooms)
	delete(rooms, id)
	if err = s.saveCatalog(ctx, rooms); err != nil {
		return ended, ErrUnavailable
	}
	s.rooms = rooms
	return ended, nil
}
