// Package api adapts the IRC engine to the browser contract. Identity is injected
// by the host application; an IRC nickname or request body is never an owner ID.
package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"ircord/backend/internal/activity"
	"ircord/backend/internal/admin"
	"ircord/backend/internal/auth"
	"ircord/backend/internal/buildinfo"
	"ircord/backend/internal/emotes"
	"ircord/backend/internal/ergo"
	"ircord/backend/internal/filehost"
	"ircord/backend/internal/imagevariant"
	"ircord/backend/internal/irc"
	"ircord/backend/internal/linkmedia"
	"ircord/backend/internal/model"
	"ircord/backend/internal/push"
	"ircord/backend/internal/remotemedia"
	"ircord/backend/internal/storage"
)

type Identity func(http.ResponseWriter, *http.Request) (string, error)
type Options struct {
	Ergo   *ergo.Client
	Admin  *admin.Verifier
	Build  buildinfo.Info
	Push   *push.Service
	Files  *filehost.Service
	Emotes *emotes.Service
	// RemoteMedia caches pictures hosted on other servers; nil leaves them unproxied.
	RemoteMedia *remotemedia.Service
	Media       *linkmedia.Resolver
	PublicURL   string
	Auth        *auth.Service
	// Activity routes alerts across the account's devices; nil leaves alerts to each client.
	Activity *activity.Router
	Store    *storage.Store
	Identity Identity
	Origins  []string
	// DisableCustomCSS stops personal themes from applying their custom CSS on this instance.
	DisableCustomCSS bool
	// FileHost makes this server the IRC network's file host.
	FileHost FileHostOptions
	// NetworkMessageUploads sends the files people attach to messages to the
	// network's own file host, when it advertises one this server trusts.
	NetworkMessageUploads bool
}

// FileHostOptions configures serving the IRC network's file host (draft/FILEHOST).
type FileHostOptions struct {
	// Serve accepts uploads at /files/ from any login the associated IRC server accepts.
	// It needs Options.Ergo and Options.Files with storage.
	Serve bool
	// Accounts limits who may upload, by account name. Empty allows every
	// account on the network that also has an account on this server.
	Accounts []string
}

type Server struct {
	startedAt time.Time
	Manager   *irc.Manager
	Handler   http.Handler
	API       huma.API
	options   Options

	socketsMu sync.Mutex
	sockets   map[string]int
	uploads   uploadSlots
	// fileHostFailures counts wrong logins to the network file host.
	fileHostFailures loginFailures
	// filehostConfig finds a network connection's file host and IRC login.
	filehostConfig func(owner, network string) (host, advertised, user, password string, err error)
	offlineFiles   chan struct{}
	offlineHTTP    *http.Client
	offlineCache   *offlineCache
	// emoteSlots bounds concurrent emote uploads and imports.
	emoteSlots chan struct{}
	hashField  scoreLimiter
	ircoid     scoreLimiter
	linkMedia  linkMediaCache
	images     imagevariant.Cache
	guideFacts guideFacts
	appEmoji   appEmojiCache
	// encryptionMu keeps the saved and the running encryption mode in step.
	encryptionMu sync.Mutex
}

func New(manager *irc.Manager, options Options) *Server {
	if options.Identity == nil && options.Auth == nil {
		panic("API requires an identity provider")
	}
	if options.Build.Version == "" {
		options.Build = buildinfo.Current()
	}
	s := &Server{Manager: manager, options: options, sockets: map[string]int{}, startedAt: time.Now().UTC(), filehostConfig: manager.FilehostConfig}
	if options.Activity != nil && options.Store != nil {
		options.Activity.SetDevices(options.Store)
	}
	router := chi.NewRouter()
	router.Use(s.middleware)
	config := huma.DefaultConfig("IRCord API", "0.1.0")
	config.DocsPath = "/docs"
	config.OpenAPIPath = "/openapi"
	config.DocsRenderer = huma.DocsRendererScalar
	config.DocsRendererConfig = map[string]any{"theme": "purple", "proxyUrl": "", "defaultHttpClient": map[string]string{"targetKey": "js", "clientKey": "fetch"}}
	s.API = humachi.New(router, config)
	s.registerAdmin()
	s.registerAdministration()
	s.registerChannelGuide()
	s.registerAuth()
	s.registerCommands()
	s.registerModeration()
	s.registerNetworkAPI()
	s.registerServices()
	s.registerAppData()
	s.registerAppSettings()
	s.registerAppPublic()
	s.registerChangelog()
	s.registerNotifications()
	s.registerActivity()
	s.registerSaved()
	s.registerLinks()
	s.registerInbox()
	s.registerHashField()
	s.registerIrcoid()
	s.registerE2EE()
	s.registerConnectionAPI(router)
	s.registerFiles(router)
	s.registerFileHost(router)
	s.registerEmotes(router)
	s.registerRemoteMedia(router)
	s.registerLinkMedia(router)
	s.registerConversations()
	s.registerParties()
	s.registerPeople()
	s.registerSearch()
	s.registerProfile()
	s.registerEvents(router)
	s.registerOffline(router)
	router.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if s.options.Store != nil && s.options.Store.DB.PingContext(r.Context()) != nil {
			writeProblem(w, "storage unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	huma.Get(s.API, "/api/v1/bootstrap", func(ctx context.Context, _ *struct{}) (*Output[irc.Snapshot], error) {
		accountID := owner(ctx)
		if err := manager.StorageError(accountID); err != nil {
			return result(irc.Snapshot{}, err)
		}
		snapshot := manager.Snapshot(accountID)
		snapshot.Capabilities = s.capabilities(accountID)
		return result(snapshot, nil)
	})
	s.finalizeSpec()
	s.Handler = router
	return s
}

func (s *Server) capabilities(accountID string) model.Capabilities {
	capabilities := model.Capabilities{
		Themes:     &model.ThemeCapability{CustomCSS: !s.options.DisableCustomCSS},
		Parties:    s.Manager.PartyCapability(accountID),
		Moderation: s.Manager.ModerationCapability(accountID),
		Encryption: s.Manager.EncryptionCapability(),
	}
	if s.options.Files != nil {
		p := s.options.Files.LocalProvider()
		if p.Available {
			capabilities.Upload = &model.UploadCapability{MaxBytes: p.MaxBytes, Types: p.Types}
		}
	}
	return capabilities
}
