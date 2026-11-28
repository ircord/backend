package irc

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/lrstanley/girc"
	"ircord/backend/internal/model"
	"ircord/backend/internal/storage"
)

// Encrypted chats carried over IRC (ircord/issues#19, phase 2): direct chats
// with anyone on the home network, whichever IRCord server they use, and
// groups in their own invite-only channel.
//
// The backend is a blind helper. It answers directory requests for its own
// accounts, sends and orders commits, delivers Welcomes until their receipt
// arrives, answers log requests from other members and creates the channels
// of groups. Every MLS object stays opaque; the backend reads only the headers
// the handshake objects state in the clear.
//
// Ordering: IRC delivers lines in different orders to different members, but
// every copy of a line carries the same server time and message ID. The
// position of a commit is that pair on its last line. For each epoch the
// candidate with the earliest position comes first; devices try candidates in
// that order and apply the first one their core accepts. A backend offers a
// candidate e2eeSettle after it arrived, by which time any competitor stamped
// earlier has arrived too.

var (
	e2eeSettle         = 2 * time.Second
	e2eeRequestWait    = 8 * time.Second
	e2eeRequestTries   = 3
	e2eeEchoWait       = 20 * time.Second
	e2eeDirectoryTTL   = 5 * time.Minute
	e2eeElsewhereTTL   = 10 * time.Minute
	e2eeElsewhereWait  = 3 * time.Second
	e2eeOutboxInterval = 5 * time.Second
	e2eeJoinDelay      = 2 * time.Second
)

const (
	// e2eeDirectoryRate is how many directory and log requests one account may
	// send to one of ours a minute. Beyond it, only reusable last-resort key
	// packages are handed out, so nobody can empty a device's stock.
	e2eeDirectoryRate  = 10
	e2eeMaxCached      = 512
	e2eeMaxLogAnswer   = 60 << 10
	e2eeMaxGroupPeople = storage.E2EEMaxGroupMembers
	// encryptionKey is the public metadata value saying that an account accepts
	// encrypted chats, and with which handshake version.
	encryptionKey   = "ircord/e2ee"
	encryptionValue = "2"
)

var (
	// ErrE2EENoAnswer means nobody answered a request over IRC.
	ErrE2EENoAnswer = serviceError(504, "Their IRCord server did not answer. Try again later.")
	// ErrE2EENotSetUp means the person has no encryption set up anywhere this server can see.
	ErrE2EENotSetUp = serviceError(404, "This person hasn't set up encryption, or their IRCord server doesn't support encrypted chats with other servers yet.")
	// ErrE2EEElsewhere stops a second IRCord server from setting up encryption
	// for an account that already has it on another one.
	ErrE2EEElsewhere = serviceError(409, "Encryption for this account is set up on another IRCord server. Open that one to read encrypted chats.")
	// ErrE2EENotJoined refuses changes to a chat the account has not joined.
	ErrE2EENotJoined = serviceError(409, "Join this encrypted chat first.")
)

type e2eeCached struct {
	reply   E2EEDirectory
	expires time.Time
}

type e2eeElsewhere struct {
	value   bool
	checked time.Time
}

// e2eeNetwork is the backend's shared state for handshakes.
type e2eeNetwork struct {
	mu        sync.Mutex
	waiting   map[string]chan handshakeEvent
	cache     map[string]e2eeCached
	asked     map[string][]time.Time
	elsewhere map[string]e2eeElsewhere
	wake      chan struct{}
}

func (n *e2eeNetwork) await(transfer string) chan handshakeEvent {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.waiting == nil {
		n.waiting = map[string]chan handshakeEvent{}
	}
	ch := make(chan handshakeEvent, 16)
	n.waiting[transfer] = ch
	return ch
}

func (n *e2eeNetwork) unwait(transfer string) {
	n.mu.Lock()
	delete(n.waiting, transfer)
	n.mu.Unlock()
}

// deliver hands an answer to the request waiting for its transfer.
func (n *e2eeNetwork) deliver(ev handshakeEvent) bool {
	n.mu.Lock()
	ch := n.waiting[ev.transfer]
	n.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- ev:
	default:
	}
	return true
}

func (n *e2eeNetwork) waitingFor(transfer string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.waiting[transfer] != nil
}

// allow counts a request from one account to one of ours.
func (n *e2eeNetwork) allow(owner, requester string, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.asked == nil {
		n.asked = map[string][]time.Time{}
	}
	if len(n.asked) > 4096 {
		for k, times := range n.asked {
			if len(times) == 0 || now.Sub(times[len(times)-1]) > time.Minute {
				delete(n.asked, k)
			}
		}
	}
	key := owner + " " + strings.ToLower(requester)
	recent := n.asked[key][:0]
	for _, at := range n.asked[key] {
		if now.Sub(at) < time.Minute {
			recent = append(recent, at)
		}
	}
	allowed := len(recent) < e2eeDirectoryRate
	n.asked[key] = append(recent, now)
	return allowed
}

func (n *e2eeNetwork) cached(owner, account string) (E2EEDirectory, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	v, ok := n.cache[owner+" "+strings.ToLower(account)]
	if !ok || time.Now().After(v.expires) {
		return E2EEDirectory{}, false
	}
	return v.reply, true
}

func (n *e2eeNetwork) remember(owner, account string, reply E2EEDirectory) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cache == nil {
		n.cache = map[string]e2eeCached{}
	}
	if len(n.cache) >= e2eeMaxCached {
		for k, v := range n.cache {
			if time.Now().After(v.expires) || len(n.cache) >= e2eeMaxCached {
				delete(n.cache, k)
			}
		}
	}
	// Cached directories never carry key packages; those are single-use.
	devices := make([]E2EEDirectoryDevice, len(reply.Devices))
	for i, d := range reply.Devices {
		devices[i] = E2EEDirectoryDevice{ID: d.ID, Certificate: d.Certificate}
	}
	reply.Devices = devices
	n.cache[owner+" "+strings.ToLower(account)] = e2eeCached{reply: reply, expires: time.Now().Add(e2eeDirectoryTTL)}
}

// forget drops what every account of this backend cached about an account.
func (n *e2eeNetwork) forget(account string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	suffix := " " + strings.ToLower(account)
	for k := range n.cache {
		if strings.HasSuffix(k, suffix) {
			delete(n.cache, k)
		}
	}
}

func (n *e2eeNetwork) wakeOutbox() {
	n.mu.Lock()
	ch := n.wake
	n.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// e2eeConnection returns the account's home connection once it passed the
// encryption self-test.
func (m *Manager) e2eeConnection(owner string) (*connection, error) {
	if !m.E2EEEnabled() {
		return nil, ErrEncryptionOff
	}
	c, err := m.connection(owner, m.options.Home.ID)
	if err != nil {
		return nil, errEncryptionUnavailable
	}
	w := c.owner
	w.mu.Lock()
	policy := w.networks[c.config.ID].ISupport["CLIENTTAGDENY"]
	w.mu.Unlock()
	c.mu.Lock()
	passed := c.ready && c.e2eeTest == e2eePassed && c.e2eeEligibleLocked(policy)
	c.mu.Unlock()
	if !passed {
		return nil, errEncryptionUnavailable
	}
	return c, nil
}

// ownAccount is the IRC account this connection is signed in as.
func (c *connection) ownAccount() string {
	c.owner.mu.Lock()
	defer c.owner.mu.Unlock()
	return c.owner.networks[c.config.ID].Account
}

func (c *connection) inChannel(channel, nick string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	members := c.roster[c.foldUnlocked(channel)]
	return members != nil && members[c.foldUnlocked(nick)]
}

func (c *connection) channelMembers(channel string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []string{}
	for nick := range c.roster[c.foldUnlocked(channel)] {
		out = append(out, nick)
	}
	return out
}

// emitE2EE tells the account's clients to sync their encryption state.
func (m *Manager) emitE2EE(owner string, change model.E2EEChange) {
	if m.ctx.Err() != nil {
		return
	}
	w := m.space(owner)
	w.mu.Lock()
	w.emit("e2ee.changed", change)
	w.mu.Unlock()
}

// encryptedGroupConversation reports the channel of an encrypted group. Its
// messages notify like direct messages: the server cannot see mentions.
func encryptedGroupConversation(conv Conversation) bool {
	if conv.Kind != "channel" || conv.Party != nil {
		return false
	}
	target := conv.Target
	if target == "" {
		target = "#" + strings.TrimPrefix(conv.Name, "#")
	}
	return isGroupChannel(target)
}

// e2eeReceive handles a complete handshake object from the home network.
func (m *Manager) e2eeReceive(c *connection, ev handshakeEvent) {
	store := m.options.Store
	if store == nil || !m.E2EEEnabled() || c.config.ID != m.options.Home.ID {
		return
	}
	owner := c.owner.ownerID
	if !ev.ours {
		if woke, err := store.WakeE2EEOutbox(owner, ev.account); err == nil && woke {
			m.net.wakeOutbox()
		}
	}
	switch ev.kind {
	case kindDirectoryRequest:
		if !ev.ours {
			m.answerDirectory(c, ev)
		}
	case kindDirectory:
		if !ev.ours {
			m.net.deliver(ev)
		}
	case kindCommit:
		m.receiveCommit(c, ev)
	case kindWelcome:
		if !ev.ours {
			m.receiveWelcome(c, ev)
		}
	case kindReceipt:
		if !ev.ours {
			m.receiveReceipt(c, ev)
		}
	case kindLogRequest:
		if !ev.ours {
			m.answerLog(c, ev)
		}
	case kindRejoin:
		if !ev.ours {
			m.receiveRejoin(c, ev)
		}
	case kindNotice:
		if !ev.ours {
			if _, ok := decodeNotice(ev.object); ok {
				m.net.forget(ev.account)
				m.emitE2EE(owner, model.E2EEChange{Account: ev.account, Reason: "notice"})
			}
		}
	}
}

// answerDirectory answers a request for our account's devices. Only the
// backend that holds the account's identity answers.
func (m *Manager) answerDirectory(c *connection, ev handshakeEvent) {
	store := m.options.Store
	owner := c.owner.ownerID
	req, ok := decodeDirectoryRequest(ev.object)
	if !ok || !strings.EqualFold(req.Account, c.ownAccount()) {
		return
	}
	identity, err := store.E2EEIdentity(owner)
	if err != nil {
		return
	}
	reply := E2EEDirectory{Account: c.ownAccount(), IdentityKey: identity.IdentityKey, Generation: identity.Generation}
	if list, counter, err := store.E2EEDeviceList(owner); err == nil {
		reply.DeviceList, reply.ListCounter = list, counter
	}
	allowed := m.net.allow(owner, ev.account, time.Now())
	packages := map[string]storage.E2EEClaim{}
	if req.Claim {
		var claims []storage.E2EEClaim
		if allowed {
			_, claims, err = store.ClaimE2EEKeyPackages(owner)
		} else {
			claims, err = store.E2EELastResortPackages(owner)
		}
		if err != nil {
			return
		}
		for _, claim := range claims {
			packages[claim.Device] = claim
		}
	}
	devices, err := store.E2EEDevices(owner)
	if err != nil {
		return
	}
	for _, d := range devices {
		item := E2EEDirectoryDevice{ID: d.ID, Certificate: d.Certificate}
		if claim, ok := packages[d.ID]; ok {
			item.KeyPackage, item.LastResort = claim.KeyPackage, claim.LastResort
		}
		reply.Devices = append(reply.Devices, item)
	}
	object := reply.encode()
	for len(object) > handshakeMaxObject && len(reply.Devices) > 0 {
		reply.Devices = reply.Devices[:len(reply.Devices)-1]
		object = reply.encode()
	}
	if _, err = c.sendHandshake(kindDirectory, ev.transfer, []string{ev.nick}, object, false); err != nil {
		slog.Debug("Encryption directory answer not sent", "error", err)
	}
}

// E2EERemoteDirectory asks the network for an account's identity and devices.
// Every IRCord server the account is signed in through answers for the
// devices it holds; the first answer from that account counts. With claim set,
// each device's answer carries one key package.
func (m *Manager) E2EERemoteDirectory(ctx context.Context, owner, account string, claim bool) (E2EEDirectory, error) {
	return m.remoteDirectory(ctx, owner, account, claim, e2eeRequestTries, e2eeRequestWait)
}

func (m *Manager) remoteDirectory(ctx context.Context, owner, account string, claim bool, tries int, wait time.Duration) (E2EEDirectory, error) {
	account = strings.TrimSpace(account)
	if !serviceAccount(account) {
		return E2EEDirectory{}, ErrNotFound
	}
	c, err := m.e2eeConnection(owner)
	if err != nil {
		return E2EEDirectory{}, err
	}
	if !claim {
		if v, ok := m.net.cached(owner, account); ok {
			return v, nil
		}
	}
	transfer := newTransfer()
	answers := m.net.await(transfer)
	defer m.net.unwait(transfer)
	defer c.forgetSent(kindDirectoryRequest, transfer)
	object := directoryRequest{Account: account, Claim: claim}.encode()
	for try := 0; try < tries; try++ {
		if _, err = c.sendHandshake(kindDirectoryRequest, transfer, []string{account}, object, false); err != nil {
			return E2EEDirectory{}, err
		}
		timer := time.NewTimer(wait)
	waiting:
		for {
			select {
			case ev := <-answers:
				if ev.kind != kindDirectory || !strings.EqualFold(ev.account, account) {
					continue
				}
				reply, ok := decodeDirectory(ev.object)
				if !ok || !strings.EqualFold(reply.Account, account) {
					continue
				}
				timer.Stop()
				m.net.remember(owner, account, reply)
				return reply, nil
			case <-timer.C:
				break waiting
			case <-ctx.Done():
				timer.Stop()
				return E2EEDirectory{}, ctx.Err()
			}
		}
	}
	if m.publishesEncryption(owner, account) {
		return E2EEDirectory{}, ErrE2EENoAnswer
	}
	return E2EEDirectory{}, ErrE2EENotSetUp
}

// publishesEncryption reports whether the account's public metadata says it
// accepts encrypted chats.
func (m *Manager) publishesEncryption(owner, account string) bool {
	c, err := m.connection(owner, m.options.Home.ID)
	if err != nil {
		return false
	}
	c.mu.Lock()
	id := c.nicks[c.foldUnlocked(account)]
	c.mu.Unlock()
	w := c.owner
	w.mu.Lock()
	defer w.mu.Unlock()
	return id != "" && w.members[id].Encryption != ""
}

// E2EEElsewhere reports whether another IRCord server holds encryption keys
// for this account, while this one has none. It asks the account's other
// sessions over IRC and remembers the answer for E2EEElsewhereHint.
func (m *Manager) E2EEElsewhere(ctx context.Context, owner string) (bool, error) {
	store := m.options.Store
	if store == nil {
		return false, nil
	}
	if _, err := store.E2EEIdentity(owner); err == nil {
		return false, nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return false, ErrStorage
	}
	// Only a recent yes is taken from memory: setting up keys must not follow
	// a no that a background check has not confirmed yet.
	m.net.mu.Lock()
	known, ok := m.net.elsewhere[owner]
	m.net.mu.Unlock()
	if ok && known.value && time.Since(known.checked) < e2eeElsewhereTTL {
		return true, nil
	}
	return m.checkElsewhere(ctx, owner)
}

// checkElsewhere asks the account's other sessions for its directory.
func (m *Manager) checkElsewhere(ctx context.Context, owner string) (bool, error) {
	c, err := m.e2eeConnection(owner)
	if err != nil {
		return false, nil
	}
	account := c.ownAccount()
	if account == "" {
		return false, nil
	}
	_, err = m.remoteDirectory(ctx, owner, account, false, 1, e2eeElsewhereWait)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	value := err == nil
	m.net.mu.Lock()
	if m.net.elsewhere == nil {
		m.net.elsewhere = map[string]e2eeElsewhere{}
	}
	m.net.elsewhere[owner] = e2eeElsewhere{value: value, checked: time.Now()}
	m.net.mu.Unlock()
	return value, nil
}

// E2EEElsewhereHint answers from the last check without waiting, and starts a
// new check in the background when the last one is stale. When the answer
// turns out to be yes, the account's clients get an e2ee.changed event.
func (m *Manager) E2EEElsewhereHint(owner string) bool {
	if store := m.options.Store; store == nil {
		return false
	} else if _, err := store.E2EEIdentity(owner); err == nil {
		return false
	}
	m.net.mu.Lock()
	if m.net.elsewhere == nil {
		m.net.elsewhere = map[string]e2eeElsewhere{}
	}
	known, ok := m.net.elsewhere[owner]
	stale := !ok || time.Since(known.checked) >= e2eeElsewhereTTL
	if stale {
		// Mark the check as started, so concurrent reads start no second one.
		m.net.elsewhere[owner] = e2eeElsewhere{value: known.value, checked: time.Now()}
	}
	m.net.mu.Unlock()
	if stale {
		go func() {
			ctx, cancel := context.WithTimeout(m.ctx, 2*e2eeElsewhereWait)
			defer cancel()
			if value, err := m.checkElsewhere(ctx, owner); err == nil && value && !known.value {
				m.emitE2EE(owner, model.E2EEChange{Reason: "notice"})
			}
		}()
	}
	return known.value
}

// E2EEIdentityPublished runs after the account's identity was first published
// or reset here: it shares the public capability value and tells the
// account's encrypted chats that its keys changed.
func (m *Manager) E2EEIdentityPublished(owner string, reset bool) {
	m.net.mu.Lock()
	delete(m.net.elsewhere, owner)
	m.net.mu.Unlock()
	if c, err := m.e2eeConnection(owner); err == nil {
		c.publishEncryption()
	}
	if reset {
		m.E2EENotify(owner, noticeIdentity)
	}
}

// publishEncryption shares the public capability value of an account that has
// encryption keys here.
func (c *connection) publishEncryption() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ready && c.caps["draft/metadata-3"] {
		c.queueMetadataLocked(encryptionKey, encryptionValue)
	}
}

// E2EENotify tells every joined encrypted chat of the account that its
// devices or identity changed, so others ask for its directory again.
func (m *Manager) E2EENotify(owner string, reason byte) {
	store := m.options.Store
	if store == nil {
		return
	}
	c, err := m.e2eeConnection(owner)
	if err != nil {
		return
	}
	chats, err := store.E2EEChats(owner)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	targets := []string{}
	for _, chat := range chats {
		key := strings.ToLower(chat.Target)
		if chat.State != storage.E2EEChatJoined || seen[key] {
			continue
		}
		seen[key] = true
		targets = append(targets, chat.Target)
	}
	if len(targets) == 0 {
		return
	}
	// Channels take one line each; people several to a line.
	people := []string{}
	object := noticeObject{Reason: reason}.encode()
	for _, target := range targets {
		if strings.HasPrefix(target, "#") {
			_, _ = c.sendHandshake(kindNotice, newTransfer(), []string{target}, object, false)
		} else {
			people = append(people, target)
		}
	}
	if len(people) > 0 {
		_, _ = c.sendHandshake(kindNotice, newTransfer(), people, object, false)
	}
}

// E2EEDeviceListChanged runs after the account's signed device list changed.
func (m *Manager) E2EEDeviceListChanged(owner string) { m.E2EENotify(owner, noticeDevices) }

// receiveCommit records a commit for one of the account's chats: a live
// commit at the position of its last line, or the commits of a log answer at
// the positions their sender recorded.
func (m *Manager) receiveCommit(c *connection, ev handshakeEvent) {
	store := m.options.Store
	owner := c.owner.ownerID
	obj, ok := decodeCommit(ev.object)
	if !ok {
		return
	}
	chat, err := store.E2EEChat(owner, obj.Group)
	if err != nil {
		m.net.deliver(ev)
		return
	}
	answer := m.net.waitingFor(ev.transfer)
	live := len(obj.Entries) == 1 && obj.Entries[0].At == 0 && obj.Entries[0].MsgID == "" && !answer
	switch {
	case live && chat.Kind == storage.E2EEChatGroup:
		// Only members can send to the group's channel.
		if !strings.EqualFold(ev.target, chat.Target) {
			return
		}
	case live:
		if !ev.ours && !strings.EqualFold(ev.account, chat.Peer) && !strings.EqualFold(ev.account, c.ownAccount()) {
			return
		}
	case !answer:
		return
	}
	added := false
	for _, entry := range obj.Entries {
		v := storage.E2EEObserved{Epoch: entry.Epoch, At: entry.At, MsgID: entry.MsgID, Sender: entry.Sender, Commit: entry.Commit, Arrived: ev.arrived}
		if live {
			v.At, v.MsgID, v.Sender = ev.at, ev.msgid, ev.account
		}
		if v.MsgID == "" || v.Sender == "" || !clean(v.Sender, wireMaxName) {
			continue
		}
		fresh, err := store.ObserveE2EECommit(owner, obj.Group, v)
		if err != nil {
			slog.Warn("Recording an encrypted commit failed", "error", err)
			continue
		}
		added = added || fresh
	}
	if answer {
		m.net.deliver(ev)
	}
	if added {
		group := obj.Group
		time.AfterFunc(e2eeSettle, func() { m.emitE2EE(owner, model.E2EEChange{GroupID: group, Reason: "commit"}) })
	}
}

// receiveWelcome keeps a Welcome for the account's devices and confirms it to
// the sender's server.
func (m *Manager) receiveWelcome(c *connection, ev handshakeEvent) {
	store := m.options.Store
	owner := c.owner.ownerID
	obj, ok := decodeWelcome(ev.object)
	if !ok {
		return
	}
	if _, err := store.E2EEIdentity(owner); err != nil {
		// Another IRCord server holds this account's keys and answers.
		return
	}
	// Anyone on the network can send a Welcome; one sender cannot fill the
	// account's invitations. A sender over the limit retries later.
	if !m.net.allow(owner, "welcome "+ev.account, time.Now()) {
		return
	}
	if !m.keepWelcome(owner, obj, ev.account, ev.nick, ev.transfer, ev.at, ev.msgid) {
		return
	}
	receipt := receiptObject{Group: obj.Group}.encode()
	_, _ = c.sendHandshake(kindReceipt, ev.transfer, []string{ev.nick}, receipt, false)
}

// keepWelcome stores a Welcome for every device of the account, filed under
// its chat: an invitation for a chat the account does not have yet, or the
// chat it already has. at and msgid are the Welcome's position.
func (m *Manager) keepWelcome(owner string, obj welcomeObject, sender, nick, transfer string, at int64, msgid string) bool {
	store := m.options.Store
	kind, target := storage.E2EEChatDirect, nick
	if obj.Kind == wireGroup {
		kind, target = storage.E2EEChatGroup, obj.Channel
	}
	chat, err := store.E2EEChat(owner, obj.Group)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		chat = storage.E2EEChat{Group: obj.Group, Kind: kind, State: storage.E2EEChatInvited, Target: target, Inviter: sender, Replaces: obj.Replaces, Epoch: obj.Epoch, Transfer: transfer}
		if kind == storage.E2EEChatDirect {
			chat.Peer = sender
		}
		if err = store.PutE2EEChat(owner, chat); err != nil {
			return false
		}
	case err != nil:
		return false
	case chat.Kind != kind:
		return false
	}
	if _, err = store.AddE2EEChatWelcome(owner, storage.E2EEChatWelcome{Group: obj.Group, Epoch: obj.Epoch, Kind: kind, Replaces: obj.Replaces, Target: target, Sender: sender, Welcome: obj.Welcome, At: at, MsgID: msgid}); err != nil {
		return false
	}
	m.emitE2EE(owner, model.E2EEChange{GroupID: obj.Group, Account: sender, Reason: "welcome"})
	return true
}

func (m *Manager) receiveReceipt(c *connection, ev handshakeEvent) {
	obj, ok := decodeReceipt(ev.object)
	if !ok {
		return
	}
	owner := c.owner.ownerID
	group, err := m.options.Store.ConfirmE2EEOutbox(owner, ev.transfer, ev.account, obj.Declined)
	if err != nil || group != obj.Group {
		return
	}
	if obj.Declined {
		m.emitE2EE(owner, model.E2EEChange{GroupID: group, Account: ev.account, Reason: "declined"})
	}
}

// answerLog sends a member the commits of a chat from an epoch on, with the
// positions they were first seen at, and any Welcome we still owe them.
func (m *Manager) answerLog(c *connection, ev handshakeEvent) {
	store := m.options.Store
	owner := c.owner.ownerID
	req, ok := decodeLogRequest(ev.object)
	if !ok {
		return
	}
	chat, err := store.E2EEChat(owner, req.Group)
	if err != nil || chat.State != storage.E2EEChatJoined {
		return
	}
	if chat.Kind == storage.E2EEChatDirect && !strings.EqualFold(ev.account, chat.Peer) ||
		chat.Kind == storage.E2EEChatGroup && !c.inChannel(chat.Target, ev.nick) {
		return
	}
	if !m.net.allow(owner, ev.account, time.Now()) {
		return
	}
	commits, err := store.E2EEObservedCommits(owner, req.Group, req.From, 100)
	if err != nil {
		return
	}
	answer := commitObject{Group: req.Group, More: len(commits) == 100}
	size := 0
	for _, v := range commits {
		size += len(v.Commit) + len(v.MsgID) + len(v.Sender) + 24
		if size > e2eeMaxLogAnswer && len(answer.Entries) > 0 {
			answer.More = true
			break
		}
		answer.Entries = append(answer.Entries, commitEntry{Epoch: v.Epoch, Commit: v.Commit, At: v.At, MsgID: v.MsgID, Sender: v.Sender})
	}
	if _, err = c.sendHandshake(kindCommit, ev.transfer, []string{ev.nick}, answer.encode(), false); err != nil {
		return
	}
	welcomes, err := store.E2EEOutboxFor(owner, req.Group, ev.account)
	if err != nil {
		return
	}
	for _, item := range welcomes {
		_, _ = c.sendHandshake(kindWelcome, item.Transfer, []string{ev.nick}, item.Object, false)
	}
}

func (m *Manager) receiveRejoin(c *connection, ev handshakeEvent) {
	store := m.options.Store
	owner := c.owner.ownerID
	obj, ok := decodeRejoin(ev.object)
	if !ok {
		return
	}
	chat, err := store.E2EEChat(owner, obj.Group)
	if err != nil || chat.State != storage.E2EEChatJoined {
		return
	}
	if chat.Kind == storage.E2EEChatGroup && !strings.EqualFold(ev.target, chat.Target) ||
		chat.Kind == storage.E2EEChatDirect && !strings.EqualFold(ev.account, chat.Peer) && !strings.EqualFold(ev.account, c.ownAccount()) {
		return
	}
	if err = store.AddE2EERejoin(owner, obj.Group, storage.E2EERejoin{Account: ev.account, Device: obj.Device, Certificate: obj.Certificate, KeyPackage: obj.KeyPackage}); err != nil {
		return
	}
	m.emitE2EE(owner, model.E2EEChange{GroupID: obj.Group, Account: ev.account, Reason: "rejoin"})
}

// E2EEPosition is where the IRC server put a commit: its time and message ID.
type E2EEPosition struct {
	At    time.Time `json:"at" doc:"The IRC server's time on the commit's last line."`
	MsgID string    `json:"msgid" doc:"The IRC server's message ID on the commit's last line."`
}

func positionOf(at int64, msgid string) E2EEPosition {
	return E2EEPosition{At: time.Unix(0, at).UTC(), MsgID: msgid}
}

// E2EECommit is a change to an encrypted chat.
type E2EECommit struct {
	Epoch   int64
	Commit  []byte
	Welcome []byte
	// Added and Removed are the accounts the commit adds and removes. In a
	// group they gain or lose their standing right to enter its channel once
	// the commit has won; added accounts receive the Welcome.
	Added   []string
	Removed []string
	// Admins gain operator rights in the group's channel, Members lose them.
	Admins  []string
	Members []string
}

type E2EECommitResult struct {
	Accepted bool
	Position E2EEPosition
	Winner   E2EEPosition
	Warnings []string
}

// E2EEPostCommit sends a commit to the chat, waits for the server's echo and
// the settling time, and reports whether it came first for its epoch. Only
// then are Welcomes sent and channel rights changed.
func (m *Manager) E2EEPostCommit(ctx context.Context, owner, group string, in E2EECommit) (E2EECommitResult, error) {
	out := E2EECommitResult{Warnings: []string{}}
	store := m.options.Store
	if store == nil {
		return out, ErrEncryptionOff
	}
	chat, err := store.E2EEChat(owner, group)
	if errors.Is(err, storage.ErrNotFound) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, ErrStorage
	}
	if chat.State != storage.E2EEChatJoined {
		return out, ErrE2EENotJoined
	}
	c, err := m.e2eeConnection(owner)
	if err != nil {
		return out, err
	}
	object := commitObject{Group: group, Entries: []commitEntry{{Epoch: in.Epoch, Commit: in.Commit}}}.encode()
	if len(object) > handshakeMaxObject {
		return out, errObjectTooLarge
	}
	transfer := newTransfer()
	var sent *outgoingTransfer
	for attempt := 0; ; attempt++ {
		sent, err = c.sendHandshake(kindCommit, transfer, []string{chat.Target}, object, true)
		if err == nil {
			wait, cancel := context.WithTimeout(ctx, e2eeEchoWait)
			err = c.awaitEcho(wait, sent)
			cancel()
		}
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		// The connection dropped before the server echoed every chunk: send
		// the whole object again on the next one, under the same transfer.
		c.forgetSent(kindCommit, transfer)
		if attempt == 2 {
			return out, ErrE2EENoAnswer
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(time.Second):
		}
		if c, err = m.e2eeConnection(owner); err != nil {
			return out, err
		}
	}
	defer c.forgetSent(kindCommit, transfer)
	out.Position = positionOf(sent.at, sent.msgid)
	select {
	case <-ctx.Done():
		return out, ctx.Err()
	case <-time.After(time.Until(sent.arrived.Add(e2eeSettle))):
	}
	candidates, err := store.E2EEObservedCommits(owner, group, in.Epoch, 100)
	if err != nil {
		return out, ErrStorage
	}
	mine := storage.E2EEObserved{At: sent.at, MsgID: sent.msgid}
	winner := mine
	for _, v := range candidates {
		if v.Epoch == in.Epoch && v.Before(winner) {
			winner = v
		}
	}
	out.Winner = positionOf(winner.At, winner.MsgID)
	out.Accepted = winner.At == mine.At && winner.MsgID == mine.MsgID
	if !out.Accepted {
		return out, nil
	}
	chat.Epoch = max(chat.Epoch, in.Epoch+1)
	if err = store.PutE2EEChat(owner, chat); err != nil {
		out.Warnings = append(out.Warnings, "The commit won, but this server could not save the chat's epoch.")
	}
	if len(in.Welcome) > 0 && len(in.Added) > 0 {
		kind := wireDirect
		channel := ""
		if chat.Kind == storage.E2EEChatGroup {
			kind, channel = wireGroup, chat.Target
		}
		w := welcomeObject{Group: group, Epoch: in.Epoch + 1, Kind: kind, Channel: channel, Welcome: in.Welcome}
		if err = m.queueWelcome(c, w, in.Added); err != nil {
			out.Warnings = append(out.Warnings, "The commit won, but its Welcome could not be queued.")
		}
	}
	if chat.Kind == storage.E2EEChatGroup {
		out.Warnings = append(out.Warnings, m.applyGroupRights(ctx, owner, chat, in)...)
	}
	return out, nil
}

// applyGroupRights carries out the channel changes a winning commit justifies.
// The IRC server enforces who may make them.
func (m *Manager) applyGroupRights(ctx context.Context, owner string, chat storage.E2EEChat, in E2EECommit) []string {
	warnings := []string{}
	change := func(account, mode string, remove bool, failure string) {
		if _, err := m.SetChannelAccess(ctx, owner, chat.Conversation, account, mode, remove); err != nil {
			warnings = append(warnings, failure+" "+err.Error())
		}
	}
	c, err := m.connection(owner, m.options.Home.ID)
	own := ""
	if err == nil {
		own = c.ownAccount()
	}
	for _, account := range in.Added {
		// A commit that adds a new device of our own account keeps our rights.
		if own != "" && strings.EqualFold(account, own) {
			continue
		}
		change(account, "v", false, "Could not let "+account+" into the group's channel.")
	}
	for _, account := range in.Admins {
		change(account, "o", false, "Could not make "+account+" an admin of the group's channel.")
	}
	for _, account := range in.Members {
		change(account, "v", false, "Could not change "+account+"'s role in the group's channel.")
	}
	for _, account := range in.Removed {
		change(account, "", true, "Could not take away "+account+"'s right to enter the group's channel.")
		if err == nil {
			_ = c.enqueue(kickEvent(chat.Target, account))
		}
	}
	return warnings
}

// queueWelcome keeps a Welcome for each recipient until their server confirms
// it. The sending account's own devices here get it at once: over IRC, a line
// an account sends to itself comes back to this server only as its own echo.
// Other accounts, here or on another server, get it over IRC.
func (m *Manager) queueWelcome(c *connection, w welcomeObject, recipients []string) error {
	owner, own := c.owner.ownerID, c.ownAccount()
	clean := []string{}
	seen := map[string]bool{}
	self := false
	for _, r := range recipients {
		r = strings.TrimSpace(r)
		switch {
		case own != "" && strings.EqualFold(r, own):
			self = true
		case serviceAccount(r) && !seen[strings.ToLower(r)]:
			seen[strings.ToLower(r)] = true
			clean = append(clean, r)
		}
	}
	if len(clean) > 0 {
		if err := m.options.Store.AddE2EEOutbox(owner, w.Group, newTransfer(), clean, w.encode()); err != nil {
			return err
		}
		m.net.wakeOutbox()
	}
	if !self {
		return nil
	}
	// The account already has the chat; a direct one stays filed under the
	// other person's nick. No IRC server stamped this copy, so its position is
	// the time it was kept.
	chat, err := m.options.Store.E2EEChat(owner, w.Group)
	if err != nil {
		return err
	}
	if !m.keepWelcome(owner, w, own, chat.Target, chat.Transfer, time.Now().UnixNano(), "") {
		return errors.New("the Welcome for this account's devices was not stored")
	}
	return nil
}

// runE2EEOutbox delivers waiting Welcomes: at once, after 1, 5 and 30 minutes,
// then hourly, and at once when the recipient is next seen online.
func (m *Manager) runE2EEOutbox() {
	defer m.wg.Done()
	ticker := time.NewTicker(e2eeOutboxInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
		case <-m.net.wake:
		}
		m.deliverOutbox()
	}
}

func (m *Manager) deliverOutbox() {
	store := m.options.Store
	if store == nil || !m.E2EEEnabled() {
		return
	}
	owners, err := store.DueE2EEOutboxOwners(time.Now())
	if err != nil {
		return
	}
	for _, owner := range owners {
		c, err := m.e2eeConnection(owner)
		if err != nil {
			continue
		}
		items, err := store.DueE2EEOutbox(owner, time.Now(), 64)
		if err != nil {
			continue
		}
		byTransfer := map[string][]storage.E2EEOutboxItem{}
		order := []string{}
		for _, item := range items {
			if byTransfer[item.Transfer] == nil {
				order = append(order, item.Transfer)
			}
			byTransfer[item.Transfer] = append(byTransfer[item.Transfer], item)
		}
		for _, transfer := range order {
			group := byTransfer[transfer]
			recipients := make([]string, 0, len(group))
			for _, item := range group {
				recipients = append(recipients, item.Recipient)
			}
			if _, err = c.sendHandshake(kindWelcome, transfer, recipients, group[0].Object, false); err != nil {
				slog.Debug("Encrypted Welcome not sent", "error", err)
			}
			c.monitor(recipients)
		}
	}
}

// E2EECreate starts an encrypted chat. A direct chat binds the account's
// direct conversation; a group gets its own registered, invite-only, secret
// channel. Both send the creator's Welcome to the people in it.
type E2EECreate struct {
	Group        string
	Kind         string
	Conversation string
	Peer         string
	Members      []string
	Epoch        int64
	Welcome      []byte
	Replaces     string
}

func (m *Manager) E2EECreateChat(ctx context.Context, owner string, in E2EECreate) (storage.E2EEChat, []string, error) {
	warnings := []string{}
	store := m.options.Store
	if store == nil {
		return storage.E2EEChat{}, warnings, ErrEncryptionOff
	}
	if _, err := store.E2EEChat(owner, in.Group); err == nil {
		return storage.E2EEChat{}, warnings, serviceError(409, "This group ID is already in use. Choose a new one.")
	} else if !errors.Is(err, storage.ErrNotFound) {
		return storage.E2EEChat{}, warnings, ErrStorage
	}
	c, err := m.e2eeConnection(owner)
	if err != nil {
		return storage.E2EEChat{}, warnings, err
	}
	own := c.ownAccount()
	chat := storage.E2EEChat{Group: in.Group, Kind: in.Kind, State: storage.E2EEChatJoined, Epoch: in.Epoch, Replaces: in.Replaces, Inviter: own}
	recipients := []string{}
	welcome := welcomeObject{Group: in.Group, Epoch: in.Epoch, Replaces: in.Replaces, Welcome: in.Welcome}
	switch in.Kind {
	case storage.E2EEChatDirect:
		known, err := m.E2EEDirect(owner, in.Conversation)
		if err != nil {
			return storage.E2EEChat{}, warnings, err
		}
		if known != "" && !strings.EqualFold(known, in.Peer) {
			return storage.E2EEChat{}, warnings, serviceError(409, "The person in this conversation is logged in to a different account.")
		}
		w := c.owner
		w.mu.Lock()
		conv := w.conversations[in.Conversation]
		w.mu.Unlock()
		chat.Conversation, chat.Target, chat.Peer = in.Conversation, c.target(conv), in.Peer
		recipients = []string{in.Peer}
		welcome.Kind = wireDirect
	case storage.E2EEChatGroup:
		channel := groupChannel(in.Group)
		if channel == "" {
			return storage.E2EEChat{}, warnings, serviceError(422, "A group ID must be 16 random bytes in base64url.")
		}
		for _, member := range in.Members {
			if !strings.EqualFold(member, own) {
				recipients = append(recipients, member)
			}
		}
		result, err := m.CreateChannel(ctx, owner, m.options.Home.ID, ChannelCreate{Name: channel, Flags: "+is"})
		if err != nil {
			return storage.E2EEChat{}, warnings, err
		}
		warnings = append(warnings, result.Warnings...)
		if !result.Registered || !result.Configured {
			_ = m.PartReason(owner, result.Conversation.ID, "Encrypted group setup failed")
			return storage.E2EEChat{}, warnings, serviceError(502, "The group's channel could not be registered as invite-only and secret. Nothing was sent.")
		}
		chat.Conversation, chat.Target = result.Conversation.ID, channel
		welcome.Kind, welcome.Channel = wireGroup, channel
	default:
		return storage.E2EEChat{}, warnings, serviceError(422, "kind must be direct or group")
	}
	if err = store.PutE2EEChat(owner, chat); err != nil {
		if errors.Is(err, storage.ErrE2EELimit) {
			return storage.E2EEChat{}, warnings, serviceError(409, "You have too many encrypted chats. Leave some first.")
		}
		return storage.E2EEChat{}, warnings, ErrStorage
	}
	if chat.Kind == storage.E2EEChatGroup {
		warnings = append(warnings, m.applyGroupRights(ctx, owner, chat, E2EECommit{Added: recipients})...)
	}
	// Every device of the account but the creator's is new to the chat, so
	// the account's own devices get the Welcome too.
	if len(in.Welcome) > 0 && len(recipients) > 0 {
		if err = m.queueWelcome(c, welcome, append(recipients, own)); err != nil {
			warnings = append(warnings, "The chat was created, but its Welcome could not be queued.")
		}
	}
	created, err := store.E2EEChat(owner, in.Group)
	if err != nil {
		return storage.E2EEChat{}, warnings, ErrStorage
	}
	return created, warnings, nil
}

// E2EEJoinChat accepts an invitation: the account enters the group's channel,
// or binds its direct conversation, and fetches the commits it missed.
func (m *Manager) E2EEJoinChat(ctx context.Context, owner, group string) (storage.E2EEChat, error) {
	store := m.options.Store
	chat, err := store.E2EEChat(owner, group)
	if errors.Is(err, storage.ErrNotFound) {
		return chat, ErrNotFound
	}
	if err != nil {
		return chat, ErrStorage
	}
	if chat.State == storage.E2EEChatJoined {
		return chat, nil
	}
	if _, err = m.e2eeConnection(owner); err != nil {
		return chat, err
	}
	var conv Conversation
	if chat.Kind == storage.E2EEChatGroup {
		conv, err = m.Join(owner, m.options.Home.ID, chat.Target)
	} else {
		conv, err = m.Direct(owner, m.options.Home.ID, chat.Target)
	}
	if err != nil {
		return chat, err
	}
	chat.Conversation, chat.State = conv.ID, storage.E2EEChatJoined
	if err = store.PutE2EEChat(owner, chat); err != nil {
		return chat, ErrStorage
	}
	if chat.Kind == storage.E2EEChatGroup {
		// Our JOIN of the channel starts the catch-up; see e2eeJoined.
		return chat, nil
	}
	go func() {
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(e2eeJoinDelay):
		}
		m.e2eeCatchUp(m.ctx, owner, group)
	}()
	return chat, nil
}

// E2EEDeclineChat forgets an invitation and tells the inviter's server.
func (m *Manager) E2EEDeclineChat(owner, group string) error {
	store := m.options.Store
	chat, err := store.E2EEChat(owner, group)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return ErrStorage
	}
	if chat.State != storage.E2EEChatInvited {
		return serviceError(409, "You already joined this chat. Leave it instead.")
	}
	if c, err := m.e2eeConnection(owner); err == nil && chat.Transfer != "" && chat.Inviter != "" {
		_, _ = c.sendHandshake(kindReceipt, chat.Transfer, []string{chat.Inviter}, receiptObject{Group: group, Declined: true}.encode(), false)
	}
	if err = store.DeleteE2EEChat(owner, group); err != nil {
		return ErrStorage
	}
	return nil
}

// E2EELeaveChat leaves a chat. In a group the account parts its channel; with
// close set, the founder first removes the channel's registration. A direct
// chat accepts plaintext on this side again.
func (m *Manager) E2EELeaveChat(ctx context.Context, owner, group string, closing bool) error {
	store := m.options.Store
	chat, err := store.E2EEChat(owner, group)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return ErrStorage
	}
	if chat.Kind == storage.E2EEChatGroup && chat.Conversation != "" {
		if closing {
			if err = m.UnregisterChannel(ctx, owner, chat.Conversation); err != nil {
				return err
			}
		}
		if err = m.PartReason(owner, chat.Conversation, "Left the encrypted group"); err != nil && !errors.Is(err, ErrNotFound) {
			slog.Debug("Leaving an encrypted group's channel failed", "error", err)
		}
	}
	if err = store.DeleteE2EEChat(owner, group); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return ErrStorage
	}
	return nil
}

// E2EERequestRejoin asks the chat to add a device of the account again.
func (m *Manager) E2EERequestRejoin(owner, group, device string, certificate, keyPackage []byte) error {
	chat, err := m.options.Store.E2EEChat(owner, group)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return ErrStorage
	}
	if chat.State != storage.E2EEChatJoined {
		return ErrE2EENotJoined
	}
	c, err := m.e2eeConnection(owner)
	if err != nil {
		return err
	}
	object := rejoinObject{Group: group, Device: device, Certificate: certificate, KeyPackage: keyPackage}.encode()
	_, err = c.sendHandshake(kindRejoin, newTransfer(), []string{chat.Target}, object, false)
	return err
}

// e2eeCatchUp asks other members for a chat's commits from the epoch after the
// newest one this account saw, one member after another until one answers.
func (m *Manager) e2eeCatchUp(ctx context.Context, owner, group string) bool {
	store := m.options.Store
	chat, err := store.E2EEChat(owner, group)
	if err != nil || chat.State != storage.E2EEChatJoined {
		return false
	}
	c, err := m.e2eeConnection(owner)
	if err != nil {
		return false
	}
	own := c.ownAccount()
	asked := []string{}
	if chat.Kind == storage.E2EEChatDirect {
		asked = append(asked, chat.Target)
	} else {
		if chat.Inviter != "" && !strings.EqualFold(chat.Inviter, own) {
			asked = append(asked, chat.Inviter)
		}
		for _, nick := range c.channelMembers(chat.Target) {
			if !strings.EqualFold(nick, c.currentNick()) && !strings.EqualFold(nick, chat.Inviter) {
				asked = append(asked, nick)
			}
		}
	}
	if len(asked) > e2eeRequestTries {
		asked = asked[:e2eeRequestTries]
	}
	for _, member := range asked {
		answered := false
		for round := 0; round < 20; round++ {
			latest, err := store.E2EELatestObservedEpoch(owner, group)
			if err != nil {
				return false
			}
			from := max(chat.Epoch, latest+1)
			more, ok := m.requestLog(ctx, c, member, group, from)
			if !ok {
				break
			}
			answered = true
			if !more {
				break
			}
		}
		if answered {
			m.emitE2EE(owner, model.E2EEChange{GroupID: group, Reason: "commit"})
			return true
		}
	}
	return false
}

func (m *Manager) requestLog(ctx context.Context, c *connection, member, group string, from int64) (bool, bool) {
	transfer := newTransfer()
	answers := m.net.await(transfer)
	defer m.net.unwait(transfer)
	defer c.forgetSent(kindLogRequest, transfer)
	if _, err := c.sendHandshake(kindLogRequest, transfer, []string{member}, logRequest{Group: group, From: from}.encode(), false); err != nil {
		return false, false
	}
	timer := time.NewTimer(e2eeRequestWait)
	defer timer.Stop()
	for {
		select {
		case ev := <-answers:
			if ev.kind != kindCommit {
				continue
			}
			obj, ok := decodeCommit(ev.object)
			if !ok || obj.Group != group {
				continue
			}
			return obj.More && len(obj.Entries) > 0, true
		case <-timer.C:
			return false, false
		case <-ctx.Done():
			return false, false
		}
	}
}

// e2eeOnline runs when the account's home connection passed the self-test:
// it shares the public capability value, delivers waiting Welcomes and asks
// for the commits of every joined chat that it may have missed.
func (m *Manager) e2eeOnline(c *connection) {
	store := m.options.Store
	if store == nil || !m.E2EEEnabled() || c.config.ID != m.options.Home.ID {
		return
	}
	owner := c.owner.ownerID
	if _, err := store.E2EEIdentity(owner); err != nil {
		return
	}
	c.publishEncryption()
	m.net.wakeOutbox()
	chats, err := store.E2EEChats(owner)
	if err != nil {
		return
	}
	go func() {
		for _, chat := range chats {
			// Groups catch up once the account is back in their channel.
			if chat.State != storage.E2EEChatJoined || chat.Kind != storage.E2EEChatDirect || m.ctx.Err() != nil {
				continue
			}
			m.e2eeCatchUp(m.ctx, owner, chat.Group)
		}
	}()
}

// e2eeJoined runs on our own JOIN of a group's channel, after accepting the
// invitation or after a reconnect: the other members see us there, so they
// answer our request for the commits we missed.
func (c *connection) e2eeJoined(e girc.Event) {
	m := c.manager
	if m == nil || m.options.Store == nil || !m.E2EEEnabled() || e.Source == nil || len(e.Params) == 0 || !isGroupChannel(e.Params[0]) || c.fold(e.Source.Name) != c.fold(c.currentNick()) {
		return
	}
	owner, channel := c.owner.ownerID, e.Params[0]
	go func() {
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(e2eeJoinDelay):
		}
		chats, err := m.options.Store.E2EEChats(owner)
		if err != nil {
			return
		}
		for _, chat := range chats {
			if chat.Kind == storage.E2EEChatGroup && chat.State == storage.E2EEChatJoined && strings.EqualFold(chat.Target, channel) {
				m.e2eeCatchUp(m.ctx, owner, chat.Group)
			}
		}
	}()
}
