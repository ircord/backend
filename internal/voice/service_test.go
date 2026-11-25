package voice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
)

type testSFU struct {
	livekit.RoomService
	participants []*livekit.ParticipantInfo
	removals     []string
}

func TestVoiceRequiresHTTPSOutsideLoopback(t *testing.T) {
	public, _ := url.Parse("http://chat.example.test:3000")
	config := Config{URL: "http://sfu.internal:7880", Key: "fixture-key", Secret: strings.Repeat("s", 32), Channels: "#talk"}
	if _, err := New(config, public, nil, nil); err == nil {
		t.Fatal("voice accepted a public origin that would send credentials in plaintext")
	}
	public, _ = url.Parse("http://127.0.0.1:3000")
	if _, err := New(config, public, nil, nil); err != nil {
		t.Fatalf("local voice setup was rejected: %v", err)
	}
}

func (s *testSFU) ListParticipants(context.Context, *livekit.ListParticipantsRequest) (*livekit.ListParticipantsResponse, error) {
	return &livekit.ListParticipantsResponse{Participants: s.participants}, nil
}

func (s *testSFU) RemoveParticipant(_ context.Context, request *livekit.RoomParticipantIdentity) (*livekit.RemoveParticipantResponse, error) {
	s.removals = append(s.removals, request.Identity)
	return &livekit.RemoveParticipantResponse{}, nil
}

func TestVoiceAdmissionAndRevocation(t *testing.T) {
	var upstreamRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		if r.URL.Path != "/rtc/v1/validate" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("unexpected upstream path or forwarded account credentials")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	app, _ := url.Parse("https://chat.example.test")
	active := map[string]bool{"alice-session": true, "bob-session": true}
	service, err := New(Config{URL: upstream.URL, Key: "fixture-key", Secret: strings.Repeat("s", 32), Channels: "#talk"}, app, nil, func(_ string, session string) bool { return active[session] })
	if err != nil {
		t.Fatal(err)
	}
	sfu := &testSFU{}
	service.client = sfu
	id := service.Rooms()[0].ID
	room := service.rooms[id]
	service.reconcile(t.Context(), id, room)
	alice := User{ID: "alice", Name: "alice", Session: "alice-session"}
	bob := User{ID: "bob", Name: "bob", Session: "bob-session"}
	if _, err := service.Join(bob, "unknown-room", false); err != ErrRoom {
		t.Fatalf("an unknown room ID was accepted: %v", err)
	}
	if _, err := service.Join(User{ID: "other", Session: "expired"}, id, false); err != ErrRoom {
		t.Fatal("an expired login could join voice")
	}
	credential, err := service.Join(alice, id, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Join(alice, id, false); err != ErrSession {
		t.Fatal("second session for the same account was accepted")
	}
	verifier, err := auth.ParseAPIToken(credential.Token)
	if err != nil {
		t.Fatal("invalid issued credential")
	}
	_, claims, err := verifier.Verify(service.secret)
	if err != nil || claims.Video.Room != room.name || !claims.Video.RoomJoin || claims.Video.RoomAdmin ||
		claims.Video.CanPublishData == nil || *claims.Video.CanPublishData ||
		len(claims.Video.CanPublishSources) != 1 || claims.Video.CanPublishSources[0] != "microphone" {
		t.Fatal("voice permission scope is incorrect")
	}
	request := func(token, origin string) int {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/voice/rtc/v1/validate?access_token="+url.QueryEscape(token), nil)
		r.Header.Set("Origin", origin)
		r.Header.Set("Cookie", "ircord_session=fixture")
		r.Header.Set("Authorization", "Bearer fixture")
		w := httptest.NewRecorder()
		service.Signal(w, r)
		return w.Code
	}
	if got := request("", app.String()); got != http.StatusUnauthorized {
		t.Fatalf("expected anonymous signaling to fail, got %d", got)
	}
	if got := request(credential.Token, "https://different.example.test"); got != http.StatusForbidden {
		t.Fatalf("expected foreign origin to fail, got %d", got)
	}
	service.Leave(t.Context(), bob, credential.ID)
	if got := request(credential.Token, app.String()); got != http.StatusOK {
		t.Fatalf("expected the active room credential to work, got %d", got)
	}
	sfu.participants = []*livekit.ParticipantInfo{{Identity: credential.ID, State: livekit.ParticipantInfo_ACTIVE}}
	service.reconcile(t.Context(), id, room)
	if rooms := service.Rooms(); len(rooms) != 1 || len(rooms[0].Participants) != 1 || rooms[0].Participants[0] != "alice" {
		t.Fatal("the server-wide roster did not include the confirmed participant")
	}
	if _, err := service.Join(bob, id, false); err != nil {
		t.Fatalf("a second signed-in account could not join the same room: %v", err)
	}
	active[alice.Session] = false
	if got := request(credential.Token, app.String()); got != http.StatusForbidden || upstreamRequests.Load() != 1 {
		t.Fatalf("expected signed-out signaling to fail before contacting upstream, got %d", got)
	}
	service.reconcile(t.Context(), id, room)
	if len(sfu.removals) != 1 || sfu.removals[0] != credential.ID {
		t.Fatal("sign-out did not terminate the voice session")
	}
}

func TestEchoCredentialsStayInPrivateRoom(t *testing.T) {
	public, _ := url.Parse("https://chat.example.test")
	config := Config{URL: "http://sfu.internal:7880", Key: "fixture-key", Secret: strings.Repeat("s", 32), Channels: "#talk"}
	service, err := New(config, public, nil, func(_, _ string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	sfu := &testSFU{}
	service.client = sfu
	id := service.Rooms()[0].ID
	room := service.rooms[id]
	service.reconcile(t.Context(), id, room)
	alice := User{ID: "alice", Name: "alice", Session: "alice-session"}
	if _, err := service.Join(User{ID: "bob"}, id, true); err != ErrRoom {
		t.Fatal("an unsigned-in account could start a microphone test")
	}
	credential, err := service.Join(alice, id, true)
	if err != nil {
		t.Fatal(err)
	}
	current := service.sessions[alice.ID]
	if current.room == room.name || current.listener == current.identity || current.listener == "" {
		t.Fatal("expected two identities in a private room")
	}
	for _, participant := range []struct {
		id, token string
		publish   bool
	}{{current.identity, credential.Token, true}, {current.listener, credential.PlaybackToken, false}} {
		verifier, err := auth.ParseAPIToken(participant.token)
		if err != nil || verifier.Identity() != participant.id {
			t.Fatal("invalid microphone test credential")
		}
		_, claims, err := verifier.Verify(service.secret)
		if err != nil {
			t.Fatal(err)
		}
		grant := claims.Video
		if grant.Room != current.room || grant.CanPublish == nil || *grant.CanPublish != participant.publish ||
			grant.CanSubscribe == nil || *grant.CanSubscribe == participant.publish || grant.CanPublishData == nil || *grant.CanPublishData {
			t.Fatal("incorrect microphone test permissions")
		}
	}
	service.reconcile(t.Context(), id, room)
	if !service.Rooms()[0].Available || len(service.Rooms()[0].Participants) != 0 {
		t.Fatal("private test updated the live channel")
	}
	current.created = time.Now().Add(-echoLifetime)
	service.expireEchoes(t.Context())
	if len(service.identities) != 0 || len(service.sessions) != 0 || len(sfu.removals) != 2 || current.ctx.Err() == nil {
		t.Fatal("both microphone test sessions must expire together")
	}
}
