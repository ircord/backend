package voice

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/twitchtv/twirp"
	"ircord/backend/internal/storage"
)

type catalogSFU struct {
	testSFU
	secret  string
	deleted string
}

func (s *catalogSFU) DeleteRoom(ctx context.Context, req *livekit.DeleteRoomRequest) (*livekit.DeleteRoomResponse, error) {
	headers, _ := twirp.HTTPRequestHeaders(ctx)
	verifier, err := auth.ParseAPIToken(strings.TrimPrefix(headers.Get("Authorization"), "Bearer "))
	if err != nil {
		return nil, err
	}
	_, grants, err := verifier.Verify(s.secret)
	if err != nil || grants.Video == nil || !grants.Video.RoomCreate || grants.Video.RoomJoin {
		return nil, errors.New("missing internal room management grant")
	}
	s.deleted = req.Room
	return &livekit.DeleteRoomResponse{}, nil
}

func TestRoomCatalogMigratesIRCLinkedRoomsOnceAndPreservesServerWideEdits(t *testing.T) {
	ctx := t.Context()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	public, _ := url.Parse("https://chat.example.test")
	cfg := Config{URL: "http://sfu.internal:7880", Key: "fixture-key", Secret: strings.Repeat("s", 32), Channels: "#legacy"}
	const legacyScope = "irc.example.test:6697/true"
	if err := store.SaveVoiceRoomCatalog(ctx, legacyScope, []byte(`{"#legacy":{"mediaRoom":"ircord-old","name":"Legacy voice","bitrateKbps":160}}`)); err != nil {
		t.Fatal(err)
	}
	start := func(scope string) *Service {
		t.Helper()
		service, err := New(cfg, public, nil, func(_, _ string) bool { return true })
		if err != nil {
			t.Fatal(err)
		}
		if err := service.OpenCatalog(ctx, store, scope); err != nil {
			t.Fatal(err)
		}
		return service
	}
	owner := User{ID: "owner", Name: "owner", Session: "owner-session"}
	service := start(legacyScope)
	sfu := &catalogSFU{secret: cfg.Secret}
	service.client = sfu
	list := service.Rooms()
	if len(list) != 1 || list[0].Name != "Legacy voice" || list[0].BitrateKbps != 160 || list[0].ID == "#legacy" || service.rooms[list[0].ID].name == "ircord-old" {
		t.Fatalf("legacy migration did not preserve settings and change identity: %+v", list)
	}
	legacyID := list[0].ID
	if _, err := service.SaveRoom(ctx, User{}, "", RoomSettings{Name: "Private", BitrateKbps: 48}); err != ErrRoom {
		t.Fatalf("accepted a missing session: %v", err)
	}
	if _, err := service.SaveRoom(ctx, owner, "", RoomSettings{Name: "Next", BitrateKbps: 99}); err != ErrRoomSettings {
		t.Fatalf("accepted unsupported bitrate: %v", err)
	}
	next, err := service.SaveRoom(ctx, owner, "", RoomSettings{Name: "Next", BitrateKbps: 128})
	if err != nil {
		t.Fatal(err)
	}
	mediaRoom := service.rooms[next.ID].name
	if _, err := service.SaveRoom(ctx, owner, "", RoomSettings{Name: "next", BitrateKbps: 48}); err != ErrRoomExists {
		t.Fatalf("accepted duplicate room: %v", err)
	}
	if _, err := service.SaveRoom(ctx, owner, next.ID, RoomSettings{Name: "Renamed", BitrateKbps: 192}); err != nil {
		t.Fatal(err)
	}
	if service.rooms[next.ID].name != mediaRoom {
		t.Fatal("renaming changed the media room")
	}
	legacyMediaRoom := service.rooms[legacyID].name
	service.reconcile(ctx, legacyID, service.rooms[legacyID])
	credential, err := service.Join(owner, legacyID, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteRoom(ctx, owner, legacyID); err != nil || sfu.deleted != legacyMediaRoom {
		t.Fatalf("legacy room did not delete: %v", err)
	}
	response := httptest.NewRecorder()
	service.Signal(response, httptest.NewRequest(http.MethodGet, "/voice/rtc/v1/validate?access_token="+url.QueryEscape(credential.Token), nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("deleted room still accepted a signaling token: %d", response.Code)
	}
	service = start(legacyScope)
	rooms := service.Rooms()
	if len(rooms) != 1 || rooms[0].ID != next.ID || rooms[0].Name != "Renamed" || rooms[0].BitrateKbps != 192 || service.rooms[next.ID].name != mediaRoom {
		t.Fatalf("catalog after restart: %+v", rooms)
	}
	if changedNetwork := start("replacement.example.test:6697/true").Rooms(); len(changedNetwork) != 1 || changedNetwork[0].ID != next.ID {
		t.Fatalf("changing the IRC network changed the server catalog: %+v", changedNetwork)
	}
}
