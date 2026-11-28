package irc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lrstanley/girc"
	"ircord/backend/internal/model"
	"ircord/backend/internal/storage"
)

var errSendRouteChanged = serviceError(409, "The network changed since this message was saved. Copy the message to send it to the new network.")

type outboundWrite struct {
	clientID, attempt, target, text string
	expectReply                     bool
}

// Send durably queues an owned conversation's message before trying IRC.
func (m *Manager) Send(owner string, input SendInput) error {
	if err := normalizeSendInput(&input); err != nil {
		return err
	}
	store := m.options.Store
	if store == nil || owner == partyControlOwner {
		return m.dispatchSend(owner, input, nil)
	}
	if err := m.partyAccess(owner, input.ConversationID); err != nil {
		return err
	}
	if input.Encrypted == nil {
		if err := m.refusePlaintext(owner, input.ConversationID); err != nil {
			return err
		}
	} else if !m.E2EEEnabled() {
		return ErrEncryptionOff
	}
	command, err := m.queueSend(owner, input)
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrCommandIdentityConflict):
			return ErrClientIDConflict
		case errors.Is(err, storage.ErrOutboxFull):
			return ErrBusy
		default:
			return err
		}
	}
	switch command.State {
	case "discarded", "pending", "sent", "unconfirmed":
		return nil
	case "uncertain":
		return ErrUncertain
	case "failed":
		return m.RetrySend(owner, input.ClientID, false)
	default:
		return m.tryQueuedSend(owner, input, nil)
	}
}

func (m *Manager) queueSend(owner string, input SendInput) (storage.Command, error) {
	w := m.space(owner)
	w.mu.Lock()
	defer w.mu.Unlock()
	cfg, err := w.configuredSendRoute(input.ConversationID)
	if err != nil {
		return storage.Command{}, err
	}
	if !matchesSendEndpoint(input.Endpoint, cfg) {
		return storage.Command{}, errSendRouteChanged
	}
	command, _, err := w.store.QueueCommand(owner, input, outboundRoute(cfg))
	return command, err
}

func (w *workspace) configuredSendRoute(conversation string) (NetworkConfig, error) {
	conv := w.conversations[conversation]
	cfg, ok := w.configs[conv.NetworkID]
	if w.deleted || !ok || conv.Kind != "channel" && conv.Kind != "direct" {
		return NetworkConfig{}, ErrNotFound
	}
	return cfg, nil
}

func outboundRoute(cfg NetworkConfig) string {
	method, account := "", cfg.SASLUser
	if cfg.Auth != nil {
		method, account = cfg.Auth.Method, cfg.Auth.Account
		if account == "" && method == "nickserv" {
			account = cfg.Nick
		}
	}
	return fmt.Sprintf("%q:%d:%t:%q:%q", strings.ToLower(cfg.Host), cfg.Port, cfg.TLS, method, strings.ToLower(account))
}

func matchesSendEndpoint(endpoint *model.SendEndpoint, cfg NetworkConfig) bool {
	return endpoint == nil || strings.EqualFold(endpoint.Host, cfg.Host) && endpoint.Port == cfg.Port && endpoint.TLS == cfg.TLS
}

func (m *Manager) outboundConversation(owner, conversation string) (Conversation, *connection, error) {
	w := m.space(owner)
	w.mu.Lock()
	defer w.mu.Unlock()
	conv, ok := w.conversations[conversation]
	if w.deleted || !ok || conv.Kind != "channel" && conv.Kind != "direct" {
		return Conversation{}, nil, ErrNotFound
	}
	return conv, w.connections[conv.NetworkID], nil
}

func (m *Manager) tryQueuedSend(owner string, input SendInput, expected *connection) error {
	err := m.dispatchSend(owner, input, expected)
	if errors.Is(err, ErrBusy) || errors.Is(err, ErrDisconnected) || errors.Is(err, ErrStorage) {
		if expected == nil {
			_, c, _ := m.outboundConversation(owner, input.ConversationID)
			if c != nil {
				c.wakeOutbox(false)
			}
		}
		return nil
	}
	if err != nil {
		// Dispatch did not reach the writer. A different attempt may have
		// already started, so change only the queued state.
		if e := m.options.Store.FailQueuedCommand(owner, input.ClientID, err.Error()); e != nil {
			return ErrStorage
		}
	}
	return err
}

func (m *Manager) RetrySend(owner, clientID string, acceptDuplicate bool) error {
	store := m.options.Store
	if store == nil {
		return ErrUnsupported
	}
	entry, err := store.OutboxEntry(owner, clientID)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return ErrStorage
	}
	if entry.State == "uncertain" || entry.State == "unconfirmed" {
		if !acceptDuplicate {
			return ErrUncertain
		}
	} else if entry.State != "failed" {
		return nil
	}
	if err = m.partyAccess(owner, entry.ConversationID); err != nil {
		return err
	}
	w := m.space(owner)
	w.mu.Lock()
	cfg, err := w.configuredSendRoute(entry.ConversationID)
	if err != nil {
		w.mu.Unlock()
		return err
	}
	route := outboundRoute(cfg)
	if (entry.Route != "" && entry.Route != route) || !matchesSendEndpoint(entry.Payload.Endpoint, cfg) {
		w.mu.Unlock()
		return errSendRouteChanged
	}
	requeued, err := store.RequeueCommand(owner, clientID, route, acceptDuplicate)
	w.mu.Unlock()
	if err != nil {
		return ErrStorage
	}
	if !requeued {
		current, err := store.Command(owner, clientID)
		if err != nil {
			return ErrStorage
		}
		if current.State == entry.State {
			return ErrBusy
		}
		return nil
	}
	return m.tryQueuedSend(owner, entry.Payload, nil)
}

func (m *Manager) CheckSend(ctx context.Context, owner, clientID string) error {
	if m.options.Store == nil {
		return ErrUnsupported
	}
	entry, err := m.options.Store.OutboxEntry(owner, clientID)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return ErrStorage
	}
	if entry.State != "uncertain" && entry.State != "unconfirmed" {
		return nil
	}
	_, c, err := m.outboundConversation(owner, entry.ConversationID)
	if err != nil || c == nil {
		return nil
	}
	c.checkSend(ctx, entry)
	return nil
}

func (m *Manager) DiscardSend(owner, clientID string) error {
	store := m.options.Store
	if store == nil {
		return ErrUnsupported
	}
	removed, err := store.DiscardCommand(owner, clientID)
	if err != nil {
		return ErrStorage
	}
	if removed {
		return nil
	}
	command, err := store.Command(owner, clientID)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return ErrNotFound
	case err != nil:
		return ErrStorage
	case command.State == "discarded":
		return nil
	default:
		return ErrBusy
	}
}

func (c *connection) wakeOutbox(check bool) {
	if c.outboxWake == nil {
		return
	}
	c.mu.Lock()
	c.outboxScan = true
	c.outboxCheck = c.outboxCheck || check
	c.mu.Unlock()
	select {
	case c.outboxWake <- struct{}{}:
	default:
	}
}

func (c *connection) runOutbox() {
	defer c.manager.wg.Done()
	defer c.workers.Done()
	store := c.owner.store
	if store == nil {
		return
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.outboxWake:
		case <-tick.C:
		}
		c.mu.Lock()
		if !c.ready || !c.transportAliveLocked() {
			c.mu.Unlock()
			continue
		}
		scan, check := c.outboxScan, c.outboxCheck
		c.outboxScan, c.outboxCheck = false, false
		c.mu.Unlock()
		if scan {
			entries, err := store.NetworkCommands(c.owner.ownerID, c.config.ID, "queued", storage.MaxQueuedCommands)
			if err != nil || len(entries) > 0 {
				c.mu.Lock()
				c.outboxScan = true
				c.mu.Unlock()
			}
			for _, entry := range entries {
				if c.ctx.Err() != nil {
					break
				}
				_ = c.manager.tryQueuedSend(c.owner.ownerID, entry.Payload, c)
			}
		}
		if check && c.hasAll("draft/chathistory", "batch", "server-time") {
			entries, _ := store.RecentUncertainCommands(c.owner.ownerID, c.config.ID, 8)
			for _, entry := range entries {
				c.checkSend(c.ctx, entry)
			}
		}
	}
}

func (c *connection) checkSend(ctx context.Context, entry storage.OutboxEntry) {
	if ctx.Err() != nil || !c.hasAll("draft/chathistory", "batch", "server-time") ||
		(entry.Route != "" && entry.Route != outboundRoute(c.config)) ||
		c.manager.partyAccess(c.owner.ownerID, entry.ConversationID) != nil {
		return
	}
	conv, active, err := c.manager.outboundConversation(c.owner.ownerID, entry.ConversationID)
	if err != nil || active != c || conv.Kind == "channel" && !conv.Joined {
		return
	}
	at := entry.AttemptedAt
	if at.IsZero() {
		at = entry.CreatedAt
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	target := c.target(conv)
	result, err := c.syncHistoryPoint(ctx, target, "AROUND", historyPoint{At: at}, 100)
	if err == nil && result.Count == 0 && ctx.Err() == nil {
		// Ergo returns an empty AROUND before the first retained message.
		// An exclusive AFTER with a small cushion can retrieve that message.
		_, _ = c.syncHistoryPoint(ctx, target, "AFTER", historyPoint{At: at.Add(-time.Second)}, 100)
	}
}

func (c *connection) startOutboundWriteLocked(send outboundWrite) bool {
	p, ok := c.pending[send.clientID]
	if !ok || p.attempt != send.attempt {
		return false
	}
	if store := c.owner.store; store != nil {
		started, err := store.StartCommand(c.owner.ownerID, send.clientID)
		if err != nil {
			slog.Warn("Could not record an IRC send attempt", "error", err)
		}
		if !started {
			return false
		}
	}
	p.at = time.Now()
	c.pending[send.clientID] = p
	return true
}

func (c *connection) finishOutboundWrite(send outboundWrite, written, total int) {
	if written == total && send.expectReply {
		return
	}
	c.mu.Lock()
	p, ok := c.pending[send.clientID]
	if !ok || p.attempt != send.attempt {
		c.mu.Unlock()
		return
	}
	complete := written == total && c.ready && c.transportAliveLocked()
	if !complete {
		delete(c.pending, send.clientID)
	}
	c.mu.Unlock()
	if complete {
		c.localOutboundEcho(send.clientID, send.target, send.text, false)
		c.wakeOutbox(true)
	} else if !p.at.IsZero() {
		c.owner.mu.Lock()
		c.owner.commandUncertain(send.clientID, "IRC disconnected before confirming this message.")
		c.owner.mu.Unlock()
		c.wakeOutbox(true)
	} else {
		c.wakeOutbox(false)
	}
}

func (c *connection) localOutboundEcho(clientID, target, wireText string, confirmed bool) {
	c.messageWithReceipt(girc.Event{Command: "PRIVMSG", Source: &girc.Source{Name: c.currentNick()},
		Params: []string{target, wireText}, Tags: girc.Tags{"+ircord/client-id": clientID}, Timestamp: time.Now()}, false, !confirmed)
}

func (c *connection) pendingForLabelLocked(label string) (string, pending, bool) {
	if strings.HasPrefix(label, "send:") {
		for key, p := range c.pending {
			if p.label == label && !p.at.IsZero() {
				return key, p, true
			}
		}
	}
	return "", pending{}, false
}

func (c *connection) acknowledgeOutbound(label string) {
	c.mu.Lock()
	key, p, found := c.pendingForLabelLocked(label)
	hasEcho := c.caps["echo-message"]
	c.mu.Unlock()
	if !found || hasEcho {
		return
	}
	conv, active, err := c.manager.outboundConversation(c.owner.ownerID, p.input.ConversationID)
	if err != nil || active != c {
		return
	}
	text := p.input.Text
	if p.input.Kind == "action" {
		text = "\x01ACTION " + text + "\x01"
	}
	c.localOutboundEcho(key, c.target(conv), text, true)
}

// A labeled echo identifies the live attempt. A retained client ID identifies
// the durable command after a restart. The caller already verified the sender.
func (c *connection) matchOutboundEcho(e girc.Event, msg Message, historical bool) (string, pending, bool) {
	label := c.eventLabel(e)
	stableID := e.Tags["+ircord/client-id"]
	c.mu.Lock()
	key, p, found := c.pendingForLabelLocked(label)
	if found && p.input.ConversationID != msg.ConversationID {
		found = false
	}
	if !found && stableID != "" {
		key = stableID
		p, found = c.pending[key]
		found = found && !p.at.IsZero() && p.input.ConversationID == msg.ConversationID
	}
	if !found && label == "" && stableID == "" && !historical {
		// Old servers can echo without tags. Avoid guessing when identical
		// messages are in flight together.
		for candidateID, candidate := range c.pending {
			if candidate.at.IsZero() || candidate.input.ConversationID != msg.ConversationID ||
				candidate.input.Text != msg.Text || candidate.input.Kind != msg.Kind {
				continue
			}
			if found {
				found = false
				break
			}
			key, p, found = candidateID, candidate, true
		}
	}
	if found {
		delete(c.pending, key)
	}
	c.mu.Unlock()
	if found {
		return key, p, false
	}
	if stableID == "" || c.owner.store == nil {
		return "", pending{}, false
	}
	saved, err := c.owner.store.OutboxEntry(c.owner.ownerID, stableID)
	if err != nil || saved.ConversationID != msg.ConversationID || saved.Route != "" && saved.Route != outboundRoute(c.config) {
		return "", pending{}, false
	}
	switch saved.State {
	case "queued":
		if saved.AttemptedAt.IsZero() {
			return "", pending{}, false
		}
	case "pending", "uncertain", "unconfirmed":
	default:
		return "", pending{}, false
	}
	return stableID, pending{input: saved.Payload, messageID: saved.MessageID}, true
}
