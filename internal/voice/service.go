// Package voice authorizes IRCord users to join rooms on a private LiveKit server.
package voice

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/lrstanley/girc"
	"github.com/twitchtv/twirp"
)

const (
	capacity        = 8
	echoCapacity    = 4
	echoLifetime    = 40 * time.Second
	joinTimeout     = 30 * time.Second
	reconnectWindow = 15 * time.Second
)

var (
	ErrUnavailable = errors.New("Voice is temporarily unavailable.")
	ErrRoom        = errors.New("This voice channel is unavailable to your account.")
	ErrSession     = errors.New("Leave your current voice session before joining another.")
	ErrFull        = errors.New("This voice channel is full.")
	ErrEchoBusy    = errors.New("Too many microphone tests are running. Try again.")
)

type Config struct {
	URL, Key, Secret, Channels string
}

type User struct {
	ID, Name, Session string
}

type Room struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	BitrateKbps  int      `json:"bitrateKbps"`
	Participants []string `json:"participants" nullable:"false"`
	Capacity     int      `json:"capacity"`
	Available    bool     `json:"available"`
}

type Credential struct {
	ID            string `json:"id"`
	URL           string `json:"url"`
	Token         string `json:"token"`
	PlaybackToken string `json:"playbackToken,omitempty" doc:"Subscribe-only credential for the private microphone test."`
}

type session struct {
	identity, roomID string
	room, listener   string
	user             User
	created, seen    time.Time
	ctx              context.Context
	cancel           context.CancelFunc
	signals          int
}

type roomState struct {
	name         string
	displayName  string
	bitrateKbps  int
	ready        bool
	participants []string
}

type Service struct {
	key, secret, public, origin string
	notify                      func()
	sessionActive               func(owner, session string) bool
	client                      livekit.RoomService
	proxy                       *httputil.ReverseProxy
	catalog                     roomCatalog

	mu         sync.Mutex
	rooms      map[string]*roomState
	sessions   map[string]*session // account ID
	identities map[string]*session
}

func New(cfg Config, public *url.URL, notify func(), sessionActive func(string, string) bool) (*Service, error) {
	if (cfg == Config{}) {
		return nil, nil
	}
	if public.Scheme != "https" && public.Hostname() != "localhost" && !net.ParseIP(public.Hostname()).IsLoopback() {
		return nil, errors.New("voice requires HTTPS on non-loopback public addresses")
	}
	target, err := url.Parse(cfg.URL)
	if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") ||
		target.User != nil || target.RawQuery != "" || target.Fragment != "" || (target.Path != "" && target.Path != "/") {
		return nil, errors.New("IRCORD_VOICE_URL must be an internal HTTP(S) origin")
	}
	if len(cfg.Key) < 8 || len(cfg.Secret) < 32 || strings.IndexFunc(cfg.Key+cfg.Secret, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		return nil, errors.New("voice requires an API key and a secret of at least 32 printable characters")
	}
	channels := strings.FieldsFunc(cfg.Channels, func(r rune) bool { return r == ',' || r == ' ' })
	if len(channels) > maxVoiceRooms {
		return nil, errors.New("IRCORD_VOICE_CHANNELS contains too many IRC channels")
	}
	rooms := make(map[string]*roomState, len(channels))
	seen := make(map[string]bool, len(channels))
	origin := public.Scheme + "://" + public.Host
	for _, name := range channels {
		if !strings.HasPrefix(name, "#") || !girc.IsValidChannel(name) || strings.ContainsAny(name, "\r\n\x00") {
			return nil, errors.New("IRCORD_VOICE_CHANNELS contains an invalid IRC channel")
		}
		// Keep the original spelling rules for the optional environment seed list.
		if strings.IndexFunc(name, func(r rune) bool {
			return !strings.ContainsRune("#abcdefghijklmnopqrstuvwxyz0123456789_-", r)
		}) >= 0 {
			return nil, errors.New("voice channel configuration uses lowercase ASCII channel names")
		}
		if !seen[name] {
			seen[name] = true
			id := rand.Text()
			rooms[id] = &roomState{name: "ircord-" + id, displayName: name, bitrateKbps: defaultBitrate}
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(target)
			p.Out.URL.Path = strings.TrimPrefix(p.In.URL.Path, "/voice")
			p.Out.URL.RawPath = ""
			for _, header := range []string{"Cookie", "Authorization", "Origin", "Referer"} {
				p.Out.Header.Del(header)
			}
		},
		ModifyResponse: func(response *http.Response) error {
			response.Header.Del("Set-Cookie")
			response.Header.Del("Access-Control-Allow-Origin")
			response.Header.Del("Access-Control-Allow-Credentials")
			response.Header.Del("Location")
			return nil
		},
		// Upstream errors can contain the signaling URL and its access token.
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "Voice signaling is unavailable.", http.StatusBadGateway)
		},
		ErrorLog: log.New(io.Discard, "", 0),
	}
	ws := "wss"
	if public.Scheme == "http" {
		ws = "ws"
	}
	if notify == nil {
		notify = func() {}
	}
	return &Service{key: cfg.Key, secret: cfg.Secret, origin: origin, public: ws + "://" + public.Host + "/voice",
		notify: notify, sessionActive: sessionActive, client: livekit.NewRoomServiceProtobufClient(target.String(), client), proxy: proxy,
		rooms: rooms, sessions: map[string]*session{}, identities: map[string]*session{}}, nil
}

func (s *Service) Rooms() []Room {
	rooms := []Room{}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, room := range s.rooms {
		rooms = append(rooms, room.view(id))
	}
	slices.SortFunc(rooms, func(a, b Room) int {
		if comparison := strings.Compare(a.Name, b.Name); comparison != 0 {
			return comparison
		}
		return strings.Compare(a.ID, b.ID)
	})
	return rooms
}

func (s *Service) validSession(user User) bool {
	return user.ID != "" && user.Session != "" && s.sessionActive != nil && s.sessionActive(user.ID, user.Session)
}

func (s *Service) Join(user User, id string, echo bool) (Credential, error) {
	if !s.validSession(user) {
		return Credential{}, ErrRoom
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	room := s.rooms[id]
	if room == nil {
		return Credential{}, ErrRoom
	}
	if !room.ready {
		return Credential{}, ErrUnavailable
	}
	if s.sessions[user.ID] != nil {
		return Credential{}, ErrSession
	}
	count := 0
	for _, other := range s.sessions {
		if (echo && other.listener != "") || (!echo && other.listener == "" && other.roomID == id) {
			count++
		}
	}
	if echo && count >= echoCapacity {
		return Credential{}, ErrEchoBusy
	}
	if !echo && count >= capacity {
		return Credential{}, ErrFull
	}
	identity := rand.Text()
	mediaRoom, listener, playbackToken := room.name, "", ""
	if echo {
		mediaRoom = "ircord-mic-test-" + identity
		listener = rand.Text()
		var err error
		playbackToken, err = s.credential(mediaRoom, listener, user.Name, false, true)
		if err != nil {
			return Credential{}, ErrUnavailable
		}
	}
	token, err := s.credential(mediaRoom, identity, user.Name, true, !echo)
	if err != nil {
		return Credential{}, ErrUnavailable
	}
	ctx, cancel := context.WithCancel(context.Background())
	current := &session{identity: identity, roomID: id, room: mediaRoom,
		listener: listener, user: user, created: time.Now(), ctx: ctx, cancel: cancel}
	s.sessions[user.ID] = current
	s.identities[identity] = current
	if echo {
		s.identities[listener] = current
	}
	return Credential{ID: identity, URL: s.public, Token: token, PlaybackToken: playbackToken}, nil
}

func (s *Service) credential(room, identity, name string, publish, subscribe bool) (string, error) {
	grant := &auth.VideoGrant{Room: room, RoomJoin: true}
	grant.SetCanPublish(publish)
	grant.SetCanSubscribe(subscribe)
	grant.SetCanPublishData(false)
	if publish {
		grant.SetCanPublishSources([]livekit.TrackSource{livekit.TrackSource_MICROPHONE})
	}
	return auth.NewAccessToken(s.key, s.secret).SetIdentity(identity).SetName(name).SetVideoGrant(grant).SetValidFor(joinTimeout).ToJWT()
}

func (s *Service) entitled(current *session) bool {
	if current.listener != "" && time.Since(current.created) >= echoLifetime {
		return false
	}
	return s.rooms[current.roomID] != nil && s.validSession(current.user)
}

// drop invalidates every cached or refreshed token for the reservation before
// asking LiveKit to remove the participant. Callers hold s.mu.
func (s *Service) drop(current *session) {
	delete(s.sessions, current.user.ID)
	delete(s.identities, current.identity)
	delete(s.identities, current.listener)
	current.cancel()
}

func (s *Service) Leave(ctx context.Context, user User, identity string) {
	s.mu.Lock()
	current := s.identities[identity]
	if current == nil || current.user != user {
		s.mu.Unlock()
		return
	}
	s.drop(current)
	s.mu.Unlock()
	s.removeSession(ctx, current)
}

func (s *Service) admin(ctx context.Context, room string) (context.Context, error) {
	return s.roomRPC(ctx, &auth.VideoGrant{Room: room, RoomAdmin: true})
}

func (s *Service) roomRPC(ctx context.Context, grant *auth.VideoGrant) (context.Context, error) {
	token, err := auth.NewAccessToken(s.key, s.secret).SetVideoGrant(grant).SetValidFor(time.Minute).ToJWT()
	if err != nil {
		return nil, err
	}
	return twirp.WithHTTPRequestHeaders(ctx, http.Header{"Authorization": {"Bearer " + token}})
}

func (s *Service) remove(ctx context.Context, room, identity string) {
	if rpc, err := s.admin(ctx, room); err == nil {
		// Reconciliation retries interrupted removals and cleans up after crashes.
		_, _ = s.client.RemoveParticipant(rpc, &livekit.RoomParticipantIdentity{Room: room, Identity: identity})
	}
}

func (s *Service) removeSession(ctx context.Context, current *session) {
	s.remove(ctx, current.room, current.identity)
	if current.listener != "" {
		s.remove(ctx, current.room, current.listener)
	}
}

func (s *Service) expireEchoes(ctx context.Context) {
	var expired []*session
	s.mu.Lock()
	for _, current := range s.sessions {
		if current.listener != "" && !s.entitled(current) {
			s.drop(current)
			expired = append(expired, current)
		}
	}
	s.mu.Unlock()
	for _, current := range expired {
		s.removeSession(ctx, current)
	}
}

func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	defer s.closeSessions()
	for {
		var work sync.WaitGroup
		var changed atomic.Bool
		s.mu.Lock()
		rooms := maps.Clone(s.rooms)
		s.mu.Unlock()
		for id, room := range rooms {
			work.Go(func() {
				if s.reconcile(ctx, id, room) {
					changed.Store(true)
				}
			})
		}
		work.Go(func() { s.expireEchoes(ctx) })
		work.Wait()
		if changed.Load() {
			s.notify()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) closeSessions() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var work sync.WaitGroup
	s.mu.Lock()
	for _, current := range s.sessions {
		s.drop(current)
		work.Go(func() { s.removeSession(ctx, current) })
	}
	for _, room := range s.rooms {
		room.ready = false
	}
	s.mu.Unlock()
	work.Wait()
}

func (s *Service) reconcile(ctx context.Context, id string, room *roomState) bool {
	rpc, err := s.admin(ctx, room.name)
	if err != nil {
		return false
	}
	reply, err := s.client.ListParticipants(rpc, &livekit.ListParticipantsRequest{Room: room.name})
	var rpcError twirp.Error
	ready := err == nil || errors.As(err, &rpcError) && rpcError.Code() == twirp.NotFound
	observed := map[string]bool{}
	if ready {
		for _, participant := range reply.GetParticipants() {
			observed[participant.Identity] = participant.State == livekit.ParticipantInfo_ACTIVE
		}
	}
	s.mu.Lock()
	if s.rooms[id] != room {
		s.mu.Unlock()
		return false
	}
	var removed []*session
	participants := []string{}
	now := time.Now()
	for _, current := range s.sessions {
		if current.roomID != id || current.listener != "" {
			continue
		}
		if !s.entitled(current) || ready && !observed[current.identity] && (current.seen.IsZero() && now.Sub(current.created) > joinTimeout || !current.seen.IsZero() && now.Sub(current.seen) > reconnectWindow) {
			removed = append(removed, current)
			s.drop(current)
			continue
		}
		if !ready {
			continue
		}
		present := observed[current.identity]
		if present {
			current.seen = now
			participants = append(participants, current.user.Name)
		}
		delete(observed, current.identity)
	}
	slices.Sort(participants)
	available := ready && len(observed) == 0 && len(removed) == 0
	changed := room.ready != available || !slices.Equal(room.participants, participants)
	room.ready = available
	room.participants = participants
	s.mu.Unlock()
	for _, current := range removed {
		s.remove(ctx, room.name, current.identity)
		delete(observed, current.identity)
	}
	for identity := range observed {
		s.remove(ctx, room.name, identity)
	}
	return changed
}

// Signal is the sole public LiveKit entry point. The token identifies a live
// reservation whose room and login session must still be valid.
func (s *Service) Signal(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	origin := r.Header.Get("Origin")
	switch origin {
	case s.origin:
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Add("Vary", "Origin")
	case "":
	default:
		http.Error(w, "Origin rejected.", http.StatusForbidden)
		return
	}
	query, parseErr := url.ParseQuery(r.URL.RawQuery)
	tokens := query["access_token"]
	if parseErr != nil || len(tokens) != 1 || len(tokens[0]) == 0 || len(tokens[0]) > 4096 {
		http.Error(w, "Voice credential required.", http.StatusUnauthorized)
		return
	}
	verifier, err := auth.ParseAPIToken(tokens[0])
	if err != nil || verifier.APIKey() != s.key {
		http.Error(w, "Voice credential rejected.", http.StatusUnauthorized)
		return
	}
	_, grants, err := verifier.Verify(s.secret)
	if err != nil || grants.Video == nil || !grants.Video.RoomJoin || grants.Video.RoomAdmin {
		http.Error(w, "Voice credential rejected.", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	current := s.identities[verifier.Identity()]
	if current == nil || current.room != grants.Video.Room || !s.entitled(current) {
		s.mu.Unlock()
		http.Error(w, "Voice session ended.", http.StatusForbidden)
		return
	}
	if current.signals >= 3 {
		s.mu.Unlock()
		http.Error(w, "Too many voice connections.", http.StatusTooManyRequests)
		return
	}
	current.signals++
	s.mu.Unlock()
	defer func() { s.mu.Lock(); current.signals--; s.mu.Unlock() }()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(current.ctx, cancel)
	defer stop()
	s.proxy.ServeHTTP(w, r.WithContext(ctx))
}
