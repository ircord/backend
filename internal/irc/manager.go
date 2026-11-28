package irc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lrstanley/girc"
	"ircord/backend/internal/activity"
	ergoapi "ircord/backend/internal/ergo"
	"ircord/backend/internal/filehost"
	"ircord/backend/internal/model"
	"ircord/backend/internal/push"
	"ircord/backend/internal/storage"
)

type Options struct {
	DisableAutoRegistration bool
	AssociatedServer        *ergoapi.Client
	NetworkPasswords        map[string]string
	ServerList              ServerList
	Origin                  func(string) (string, error)
	Push                    *push.Service
	Files                   *filehost.Service
	AllowPrivate            bool
	TLSConfig               *tls.Config
	ReconnectMin            time.Duration
	MaxConnections          int
	ConnectTimeout          time.Duration
	Store                   *storage.Store
	// Pictures sets how clients draw profile pictures; nil leaves them undrawn.
	Pictures model.PictureSource
	// Home is the IRCord network (ID, host and port) where channels the user creates are
	// registered to their account.
	Home NetworkConfig
	// Router decides which devices are alerted (#84). Without one, alerts go to every device.
	Router *activity.Router
	// Moderation is the operator login on the associated server that carries out
	// server owners' deletions. Without it, server owners delete like anyone else.
	Moderation *ModerationLogin
	// E2EE is the starting state of the switch for end-to-end encryption between
	// IRCord clients on the home network. It is experimental and off by default;
	// SetE2EE changes it while the server runs.
	E2EE bool
	// E2EELocked means the operator's configuration fixes E2EE, so server owners
	// can see the mode but not change it.
	E2EELocked bool
}

// Presets returns only public operator-provided connection hints.
func (m *Manager) Presets() (bool, []NetworkPreset) {
	return m.options.ServerList.Configured, clone(m.options.ServerList.Presets)
}

type Manager struct {
	profileExpiryWake chan struct{}
	people            peopleCache
	partyPeople       peopleCache
	parties           *partyManager
	moderator         *moderator
	purges            purgeJobs
	checkSlots        chan struct{}
	listSlots         chan struct{}
	connectionCount   int
	serviceSlots      chan struct{}
	channelCreates    map[string]bool
	deletedOwners     map[string]bool
	// filehostLogins are copies of IRC logins kept while an account is deleted.
	filehostLogins map[string]func(owner, network string) (string, string, error)
	mu             sync.Mutex
	owners         map[string]*workspace
	options        Options
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	closed         bool
	// Channel-wide settings keyed by upstream network and folded channel name.
	sharedMu     sync.Mutex
	shared       map[string]ChannelSettings
	sharedLoaded bool
	// e2ee is the current encryption switch; see E2EEEnabled.
	e2ee atomic.Bool
	// net carries encrypted chats over IRC; see e2ee_chats.go.
	net e2eeNetwork
}

func New(options Options) *Manager {
	options.NetworkPasswords = clone(options.NetworkPasswords)
	if options.ReconnectMin <= 0 {
		options.ReconnectMin = time.Second
	}
	if options.MaxConnections <= 0 {
		options.MaxConnections = 1000
	}
	if options.ConnectTimeout <= 0 {
		options.ConnectTimeout = 15 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{profileExpiryWake: make(chan struct{}, 1), serviceSlots: make(chan struct{}, 16), checkSlots: make(chan struct{}, 16), listSlots: make(chan struct{}, 4), deletedOwners: map[string]bool{}, owners: map[string]*workspace{}, options: options, ctx: ctx, cancel: cancel}
	m.e2ee.Store(options.E2EE)
	m.net.wake = make(chan struct{}, 1)
	m.wg.Add(2)
	go m.runProfileExpiry()
	go m.runE2EEOutbox()
	m.partyPeople.limit = 128
	m.sharedMu.Lock()
	_ = m.loadSharedChannels() // logged; retried on first use
	m.sharedMu.Unlock()
	m.moderator = newModerator(m)
	m.parties = newPartyManager(m)
	if m.parties != nil {
		m.wg.Add(1)
		go m.parties.run()
	}
	return m
}

func (m *Manager) Connect(owner string, cfg NetworkConfig) (Network, error) {
	if owner == "" || !clean(owner, 128) {
		return Network{}, fmt.Errorf("owner is required")
	}
	w := m.space(owner)
	w.configMu.Lock()
	defer w.configMu.Unlock()
	return m.connect(w, cfg, false)
}

// connect starts a network connection with w.configMu held. A previously working
// network restored at startup retries even when its first connection attempt fails.
func (m *Manager) connect(w *workspace, cfg NetworkConfig, restored bool) (Network, error) {
	owner := w.ownerID
	cfg = m.configuredPassword(cfg)
	var normalizeErr error
	cfg, normalizeErr = normalizeConfig(cfg)
	if normalizeErr != nil {
		return Network{}, normalizeErr
	}
	if err := m.validatePartyConfig(owner, cfg); err != nil {
		return Network{}, err
	}
	if cfg.Port < 1 || cfg.Port > 65535 || cfg.Host == "" || !clean(cfg.Host, 253) || strings.ContainsAny(cfg.Host, " /\\") || !girc.IsValidNick(cfg.Nick) || !clean(cfg.Name, 100) || !clean(cfg.SASLUser, 100) || !clean(cfg.SASLPassword, 512) || !clean(cfg.ServerPassword, 512) {
		return Network{}, fmt.Errorf("invalid network configuration")
	}
	if !cfg.TLS && !m.options.AllowPrivate {
		return Network{}, fmt.Errorf("TLS is required")
	}
	if cfg.Auth != nil && cfg.Auth.Method != "none" && (cfg.Auth.Password == nil || *cfg.Auth.Password == "") {
		return Network{}, fmt.Errorf("add the authentication password before connecting")
	}
	if (cfg.SASLUser == "") != (cfg.SASLPassword == "") {
		return Network{}, fmt.Errorf("SASL username and password must be provided together")
	}
	if cfg.ID == "" {
		cfg.ID = id()
	}
	if !token(cfg.ID, 100) {
		return Network{}, fmt.Errorf("invalid network ID")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.connections[cfg.ID]; ok {
		return Network{}, fmt.Errorf("network already has an owner connection")
	}
	if w.storageErr != nil {
		return Network{}, ErrStorage
	}
	if w.deleted {
		return Network{}, ErrNotFound
	}
	if len(w.connections) >= 20 {
		return Network{}, ErrBusy
	}
	if err := m.reserveConnection(owner); err != nil {
		return Network{}, err
	}
	defer m.wg.Done()
	started := false
	defer func() {
		if !started {
			m.releaseConnection()
		}
	}()
	ctx, cancel := context.WithCancel(m.ctx)
	c := &connection{restored: restored, serviceGate: make(chan struct{}, 1), serviceBatches: map[string]string{}, createdAt: time.Now(), manager: m, owner: w, config: cfg, ctx: ctx, cancel: cancel, queue: make(chan []*girc.Event, 64), pending: map[string]pending{}, batches: map[string]*multiline{}, historyBatches: map[string]bool{}, historyGate: make(chan struct{}, 1), historyRequests: map[string]*historyRequest{}, historyResponses: map[string]*historyRequest{}, batchLabels: map[string]string{}, catching: map[string]bool{}, modeListGate: make(chan struct{}, 1), listGate: make(chan struct{}, 1), closedTargets: map[string]bool{}, desired: map[string]string{}, roster: map[string]map[string]bool{}, arrivals: map[string]bool{}, nicks: map[string]string{}, caps: map[string]bool{}, done: make(chan struct{})}
	c.metadataWake = make(chan struct{}, 1)
	n := Network{ID: cfg.ID, Name: cfg.Name, Host: cfg.Host, Port: cfg.Port, TLS: cfg.TLS, Nick: cfg.Nick, Initials: strings.ToUpper(string([]rune(cfg.Name + "I")[0])), Color: "mint", Status: "connecting", Capabilities: []string{}}
	if err := w.prepareNetworkProfile(&n, cfg); err != nil {
		cancel()
		return Network{}, err
	}
	applyConfig(&n, cfg)
	if w.store != nil {
		for _, conv := range w.conversations {
			if conv.NetworkID == cfg.ID {
				if e := w.store.BeginGap(owner, conv.ID); e != nil {
					cancel()
					return Network{}, ErrStorage
				}
			}
		}
	}
	for _, entry := range cfg.Channels {
		target := strings.Fields(entry)[0]
		if !girc.IsValidChannel(target) || !clean(target, 100) || strings.ContainsAny(target, " ,:") {
			cancel()
			return Network{}, fmt.Errorf("invalid initial channel")
		}
		c.desired[c.foldUnlocked(target)] = entry
	}
	if w.store != nil {
		if err := w.store.SaveNetwork(owner, cfg, true); err != nil {
			cancel()
			return Network{}, ErrStorage
		}
	}
	c.restoreNicks()
	w.info[n.ID] = newConnectionInfo(n.ID)
	w.networks[n.ID] = n
	w.connections[n.ID] = c
	w.configs[n.ID] = cfg
	delete(w.signedOut, n.ID)
	w.emit("network.updated", n)
	if w.storageErr != nil {
		delete(w.connections, n.ID)
		cancel()
		return Network{}, ErrStorage
	}
	c.outboxWake = make(chan struct{}, 1)
	c.workers.Add(2)
	m.wg.Add(3)
	started = true
	go c.run()
	go func() {
		defer c.workers.Done()
		c.writer()
	}()
	go c.runOutbox()
	return clone(n), nil
}

// Reserve capacity and track setup so Close waits for it and the workers it starts.
// Release the manager lock before doing any account storage work.
func (m *Manager) reserveConnection(owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("manager is closed")
	}
	if m.deletedOwners[owner] {
		return ErrNotFound
	}
	if m.connectionCount >= m.options.MaxConnections {
		return ErrBusy
	}
	m.connectionCount++
	m.wg.Add(1)
	return nil
}

func (m *Manager) releaseConnection() {
	m.mu.Lock()
	m.connectionCount--
	m.mu.Unlock()
}

func (m *Manager) connection(owner, network string) (*connection, error) {
	w := m.space(owner)
	w.mu.Lock()
	defer w.mu.Unlock()
	c := w.connections[network]
	if c == nil || w.deleted {
		return nil, ErrNotFound
	}
	return c, nil
}

func (m *Manager) Reconnect(owner, network string) error {
	w := m.space(owner)
	w.configMu.Lock()
	defer w.configMu.Unlock()
	w.mu.Lock()
	cfg, ok := w.configs[network]
	active := w.connections[network]
	w.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	if active != nil {
		if err := m.disconnect(w, network, false); err != nil {
			return err
		}
	}
	_, err := m.connect(w, cfg, false)
	return err
}

func (m *Manager) Disconnect(owner, network string) error {
	w := m.space(owner)
	w.configMu.Lock()
	defer w.configMu.Unlock()
	return m.disconnect(w, network, false)
}

// SignOut turns off every network the owner has on, because they signed out of their last
// device, and disconnects the connected ones. Those networks stay off, across restarts too,
// until SignIn; a dropped connection still reconnects by itself, a deliberate sign-out
// doesn't (#237). A network that fails to disconnect doesn't stop the others.
func (m *Manager) SignOut(owner string) error {
	w := m.space(owner)
	w.configMu.Lock()
	defer w.configMu.Unlock()
	// Networks can be on without a connection, such as one saved while the connection pool
	// was full; the next restart would connect them, so mark every network that is on.
	if w.store != nil {
		if err := w.store.SignOutEnabledNetworks(owner); err != nil {
			return ErrStorage
		}
	}
	w.mu.Lock()
	ids := make([]string, 0, len(w.connections))
	for id := range w.connections {
		ids = append(ids, id)
	}
	w.mu.Unlock()
	sort.Strings(ids)
	var errs []error
	for _, id := range ids {
		if err := m.disconnect(w, id, true); err != nil && err != ErrNotFound {
			errs = append(errs, fmt.Errorf("sign out of network %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// SignIn reconnects the networks SignOut turned off. Networks the user disconnected
// themselves stay disconnected. A network that can't connect keeps its mark for the next
// sign-in and doesn't stop the others.
func (m *Manager) SignIn(owner string) error {
	w := m.space(owner)
	w.configMu.Lock()
	defer w.configMu.Unlock()
	ids := []string{}
	if w.store != nil {
		var err error
		if ids, err = w.store.SignedOutNetworks(owner); err != nil {
			return ErrStorage
		}
	}
	w.mu.Lock()
	for id := range w.signedOut {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	w.mu.Unlock()
	sort.Strings(ids)
	var errs []error
	for _, id := range ids {
		w.mu.Lock()
		cfg, ok := w.configs[id]
		active := w.connections[id] != nil
		if !ok || active {
			delete(w.signedOut, id)
		}
		w.mu.Unlock()
		if !ok || active {
			continue
		}
		// Connect clears the mark once it succeeds; a failure, including a full connection
		// pool, leaves it so the next sign-in tries again.
		if _, err := m.connect(w, cfg, false); err != nil && err != ErrBusy {
			errs = append(errs, fmt.Errorf("reconnect network %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// disconnect runs with w.configMu held.
func (m *Manager) disconnect(w *workspace, network string, signedOut bool) error {
	w.mu.Lock()
	c := w.connections[network]
	if c == nil || w.deleted {
		w.mu.Unlock()
		return ErrNotFound
	}
	if w.store != nil {
		var err error
		if signedOut {
			err = w.store.SignOutNetwork(w.ownerID, network)
		} else {
			err = w.store.SaveNetwork(w.ownerID, w.configs[network], false)
		}
		if err != nil {
			w.mu.Unlock()
			return ErrStorage
		}
	}
	if signedOut {
		if w.signedOut == nil {
			w.signedOut = map[string]bool{}
		}
		w.signedOut[network] = true
	}
	c.mu.Lock()
	c.ready = false
	c.mu.Unlock()
	c.cancel()
	w.mu.Unlock()
	c.closeTransport()
	<-c.done
	c.workers.Wait()
	w.mu.Lock()
	removed := w.connections[network] == c
	if removed {
		n := w.networks[network]
		n.Status = "disconnected"
		n.StatusDetail = ""
		w.networks[network] = n
		w.emit("network.updated", n)
		delete(w.connections, network)
	}
	w.mu.Unlock()
	if removed {
		m.releaseConnection()
	}
	return nil
}

func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.cancel()
	spaces := make([]*workspace, 0, len(m.owners))
	for _, w := range m.owners {
		spaces = append(spaces, w)
	}
	m.mu.Unlock()
	var conns []*connection
	for _, w := range spaces {
		w.mu.Lock()
		for _, c := range w.connections {
			conns = append(conns, c)
		}
		w.mu.Unlock()
	}
	for _, c := range conns {
		c.closeTransport()
	}
	if m.moderator != nil {
		m.moderator.close()
	}
	m.wg.Wait()
	for _, w := range spaces {
		w.mu.Lock()
		w.closeTraceSubscribers()
		for ch := range w.subscribers {
			close(ch)
			delete(w.subscribers, ch)
		}
		w.mu.Unlock()
	}
}

// WaitConnected is a convenience for integration tests and service startup.
func (m *Manager) WaitConnected(ctx context.Context, owner, network string) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if n, err := m.Network(owner, network); err == nil && n.Status == "connected" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
