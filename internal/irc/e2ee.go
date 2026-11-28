package irc

import (
	"fmt"
	"strings"
	"time"

	"github.com/lrstanley/girc"
	"ircord/backend/internal/model"
)

// e2eeTag carries one end-to-end encrypted event on an ordinary PRIVMSG, as
// "<version>.<class>.<payload>": version 1, class m (message) or a (auxiliary),
// and the client's MLS message in base64url. The line's body is the fixed
// placeholder, so plain IRC clients see that something arrived they can't read.
// The backend never decrypts or inspects the payload.
const e2eeTag = "+ircord/e2ee"

// The self-test sends one TAGMSG tagged "t.<probe>" to our own nick and waits
// for the echo. The probe is as long as the longest encrypted tag, so a network
// that truncates or drops long client tags fails the test.
const (
	e2eeProbePrefix = "t."
	e2eeProbeLength = model.MaxEncryptedPayload + len("1.m.") - len(e2eeProbePrefix)
)

// e2eeProbeTimeout is how long the self-test waits for its echo.
var e2eeProbeTimeout = 20 * time.Second

type e2eeTestState int

const (
	e2eeUntested e2eeTestState = iota
	e2eeTesting
	e2eePassed
	e2eeFailed
)

var (
	// ErrEncryptionOff is returned when the operator has not turned encryption on.
	ErrEncryptionOff error = notFoundError("End-to-end encryption is not turned on for this server.")
	// ErrPlaintextRefused stops plaintext from entering an encrypted conversation.
	ErrPlaintextRefused = serviceError(409, "This conversation is end-to-end encrypted. Send an encrypted message, or turn off encryption first.")
)

// E2EEEnabled reports whether end-to-end encryption is turned on now. It needs
// durable storage, which keeps the key directory and the group log.
func (m *Manager) E2EEEnabled() bool { return m.e2ee.Load() && m.options.Store != nil }

// E2EELocked reports whether the operator's configuration fixes the mode.
func (m *Manager) E2EELocked() bool { return m.options.E2EELocked }

// SetE2EE turns end-to-end encryption on or off while the server runs. Turning
// it off deletes nothing: keys, groups and encrypted history stay, so devices
// that hold keys can still read earlier messages, but no encrypted message,
// group or key change is accepted until it is turned on again.
//
// Each connected network recomputes its features, so clients see the change in
// network.updated. Turning it on starts the self-test on eligible connections.
func (m *Manager) SetE2EE(on bool) {
	if m.e2ee.Swap(on) == on {
		return
	}
	m.mu.Lock()
	spaces := make([]*workspace, 0, len(m.owners))
	for _, w := range m.owners {
		spaces = append(spaces, w)
	}
	m.mu.Unlock()
	for _, w := range spaces {
		w.mu.Lock()
		connections := make([]*connection, 0, len(w.connections))
		for _, c := range w.connections {
			connections = append(connections, c)
		}
		w.mu.Unlock()
		for _, c := range connections {
			c.mu.Lock()
			client := c.client
			c.mu.Unlock()
			if client == nil {
				continue
			}
			c.callbackFromAttempt(client, func() {
				c.refreshFeatures(client)
				c.probeEncryption(client)
			})
		}
	}
}

// EncryptionCapability is the application capability clients see in bootstrap.
func (m *Manager) EncryptionCapability() *model.EncryptionCapability {
	if !m.E2EEEnabled() {
		return nil
	}
	return &model.EncryptionCapability{Experimental: true, MaxPayload: model.MaxEncryptedPayload, Groups: true, AcrossServers: true}
}

func kickEvent(channel, account string) *girc.Event {
	return &girc.Event{Command: "KICK", Params: []string{channel, account, "Removed from the encrypted group"}}
}

func encryptedClassCode(class string) string {
	if class == model.EncryptedAuxiliary {
		return "a"
	}
	return "m"
}

// validPayload accepts unpadded base64url within the size bound. The payload is
// otherwise opaque: the backend checks size and alphabet, never content.
func validPayload(payload string) bool {
	if payload == "" || len(payload) > model.MaxEncryptedPayload {
		return false
	}
	for i := 0; i < len(payload); i++ {
		b := payload[i]
		if !(b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-' || b == '_') {
			return false
		}
	}
	return true
}

func encryptedTagValue(e *model.Encrypted) string {
	return fmt.Sprintf("%d.%s.%s", e.Version, encryptedClassCode(e.Class), e.Payload)
}

// parseEncryptedTag reads an +ircord/e2ee value. Self-test probes and unknown
// versions or classes are not encrypted messages.
func parseEncryptedTag(value string) *model.Encrypted {
	version, rest, ok := strings.Cut(value, ".")
	if !ok || version != "1" {
		return nil
	}
	code, payload, ok := strings.Cut(rest, ".")
	if !ok || !validPayload(payload) {
		return nil
	}
	e := &model.Encrypted{Version: 1, Payload: payload}
	switch code {
	case "m":
		e.Class = model.EncryptedMessage
	case "a":
		e.Class = model.EncryptedAuxiliary
	default:
		return nil
	}
	return e
}

// normalizeEncryptedInput checks an encrypted send. Everything that would
// otherwise travel in the clear (text, action, reply, GIF, uploads) belongs
// inside the ciphertext, so the input carries only the payload.
func normalizeEncryptedInput(input *SendInput) error {
	e := input.Encrypted
	if e.Version == 0 {
		e.Version = 1
	}
	if e.Class == "" {
		e.Class = model.EncryptedMessage
	}
	if e.Version != 1 || e.Class != model.EncryptedMessage && e.Class != model.EncryptedAuxiliary || !validPayload(e.Payload) {
		return fmt.Errorf("invalid encrypted message (version 1, class message or auxiliary, at most %d base64url characters)", model.MaxEncryptedPayload)
	}
	if input.Kind != "" && input.Kind != "message" || input.Text != "" && input.Text != model.EncryptedPlaceholder || input.ReplyToID != "" || input.GIF != nil || len(input.Attachments) > 0 {
		return fmt.Errorf("an encrypted message carries its text, replies, GIFs and files inside the encrypted payload")
	}
	if !token(input.ClientID, 100) {
		return fmt.Errorf("invalid clientId")
	}
	input.Kind, input.Text = "message", model.EncryptedPlaceholder
	return nil
}

// e2eeEligibleLocked reports whether this connection may run the self-test and
// carry encrypted messages. c.mu is held. Phase 1 encrypts direct messages
// between accounts of this server, which meet on its home network.
func (c *connection) e2eeEligibleLocked(policy string) bool {
	if c.manager == nil || !c.manager.E2EEEnabled() || c.config.ID != c.manager.options.Home.ID || !c.manager.matchesHomeEndpoint(c.config) {
		return false
	}
	return c.caps["message-tags"] && c.caps["echo-message"] && c.caps["account-tag"] && tagAllowed(policy, e2eeTag[1:])
}

// probeEncryption starts the self-test once per connection attempt, as soon as
// the connection is eligible. A network that strips the tag without saying so
// never echoes it, and the feature stays off until the next connection.
func (c *connection) probeEncryption(client *girc.Client) {
	w := c.owner
	w.mu.Lock()
	policy := w.networks[c.config.ID].ISupport["CLIENTTAGDENY"]
	w.mu.Unlock()
	nick := c.currentNick()
	c.mu.Lock()
	if c.client != client || !c.ready || c.e2eeTest != e2eeUntested || !c.e2eeEligibleLocked(policy) {
		c.mu.Unlock()
		return
	}
	probe := id()
	for len(probe) < e2eeProbeLength {
		probe += "x"
	}
	probe = probe[:e2eeProbeLength]
	c.e2eeTest, c.e2eeProbe = e2eeTesting, probe
	err := c.enqueueLocked(&girc.Event{Command: "TAGMSG", Params: []string{nick}, Tags: girc.Tags{e2eeTag: e2eeProbePrefix + probe}})
	if err != nil {
		// The connection is going away; the next attempt tests again. Until then
		// encrypted sends fail with a reason instead of waiting for a test that
		// never runs.
		c.e2eeTest, c.e2eeProbe = e2eeFailed, ""
	}
	c.mu.Unlock()
	if err != nil {
		return
	}
	time.AfterFunc(e2eeProbeTimeout, func() {
		c.mu.Lock()
		expired := c.client == client && c.e2eeTest == e2eeTesting && c.e2eeProbe == probe
		if expired {
			c.e2eeTest = e2eeFailed
		}
		c.mu.Unlock()
		if expired {
			c.callbackFromAttempt(client, func() { c.refreshFeatures(client) })
		}
	})
}

// encryptionProbe consumes the echo of our own self-test TAGMSG. Only an exact,
// complete copy of the probe from our own nick passes the test.
func (c *connection) encryptionProbe(client *girc.Client, e girc.Event) {
	value, ok := e.Tags[e2eeTag]
	if !ok || !strings.HasPrefix(value, e2eeProbePrefix) || e.Source == nil {
		return
	}
	self := c.fold(e.Source.Name) == c.fold(c.currentNick())
	c.mu.Lock()
	passed := self && c.client == client && c.e2eeTest == e2eeTesting && value == e2eeProbePrefix+c.e2eeProbe
	if passed {
		c.e2eeTest = e2eePassed
	}
	c.mu.Unlock()
	if passed {
		c.refreshFeatures(client)
		if c.manager != nil {
			c.manager.e2eeOnline(c)
		}
	}
}

// refusePlaintext stops a plaintext send into a conversation whose account has
// bound it to an encrypted group. Turning encryption off is an explicit action.
//
// With the operator switch off nothing is refused: encrypted sends and turning
// encryption off are unavailable then, so a refusal would leave the
// conversation unusable.
func (m *Manager) refusePlaintext(owner, conversation string) error {
	store := m.options.Store
	if !m.E2EEEnabled() {
		return nil
	}
	encrypted, err := store.E2EEConversationEncrypted(owner, conversation)
	if err != nil {
		return ErrStorage
	}
	if encrypted {
		return ErrPlaintextRefused
	}
	return nil
}

// refuseRawPlaintext applies the same rule to raw PRIVMSG and NOTICE commands,
// so /msg and /quote cannot bypass it. Every target of the command is checked.
func (c *connection) refuseRawPlaintext(event *girc.Event) error {
	if event.Command != "PRIVMSG" && event.Command != "NOTICE" || len(event.Params) == 0 {
		return nil
	}
	for _, target := range strings.Split(event.Params[0], ",") {
		target = strings.TrimLeft(target, "@+%~&")
		if target == "" {
			continue
		}
		if err := c.manager.refusePlaintext(c.owner.ownerID, c.conversationID(target)); err != nil {
			return err
		}
	}
	return nil
}

// E2EEDirect checks that conversation is one of the account's direct
// conversations on the home network, where encryption works in phase 1. It
// returns the peer's account as the network last reported it, or "" while that
// is unknown.
func (m *Manager) E2EEDirect(owner, conversation string) (string, error) {
	if m.partyAccess(owner, conversation) != nil {
		return "", ErrNotFound
	}
	w := m.space(owner)
	w.mu.Lock()
	defer w.mu.Unlock()
	conv, ok := w.conversations[conversation]
	if !ok || w.deleted || conv.Kind != "direct" || conv.Party != nil || conv.NetworkID != m.options.Home.ID {
		return "", ErrNotFound
	}
	if cfg, ok := w.configs[conv.NetworkID]; !ok || !m.matchesHomeEndpoint(cfg) {
		return "", ErrNotFound
	}
	c := w.connections[conv.NetworkID]
	if c == nil {
		return "", nil
	}
	member, known := c.nicks[c.fold(conv.Name)]
	if !known {
		return "", nil
	}
	if account := w.networkMember(conv.NetworkID, member).Account; account != "*" {
		return account, nil
	}
	return "", nil
}

// encryptedOnly reports a message whose readable content lives only on devices.
func encryptedOnly(msg Message) bool { return msg.Encrypted != nil }

// auxiliary reports an encrypted reaction, edit, delete or control event. These
// never count as unread, mention anyone, or reach the inbox or push.
func auxiliary(msg Message) bool {
	return msg.Encrypted != nil && msg.Encrypted.Class == model.EncryptedAuxiliary
}

// errEncryptionUnavailable stops an encrypted send on a connection that failed,
// or has not yet finished, the capability gate. Nothing falls back to plaintext.
var errEncryptionUnavailable = fmt.Errorf("%w: this network can't carry encrypted messages right now", ErrUnsupported)
