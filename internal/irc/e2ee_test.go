package irc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"ircord/backend/internal/activity"
	"ircord/backend/internal/model"
	"ircord/backend/internal/storage"
)

var e2eeHome = NetworkConfig{ID: "home", Name: "home", Host: "irc.example", Port: 6697, TLS: true, Nick: "alice"}

type e2eeFixture struct {
	m      *Manager
	store  *storage.Store
	w      *workspace
	c      *connection
	pusher *recordingPusher
	at     time.Time
}

// newE2EEFixture signs alice (nick "alice") into the home network with
// encryption switched on and every capability the gate needs. Alerts go to a
// recording pusher.
func newE2EEFixture(t *testing.T, caps ...string) *e2eeFixture {
	t.Helper()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		if _, err = store.CreateUser(owner, owner, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	pusher := &recordingPusher{}
	router := activity.New(activity.Options{Push: pusher})
	_, stop := router.Subscribe("alice", "device", "tab")
	m := New(Options{Store: store, Router: router, E2EE: true, Home: e2eeHome})
	router.SetSource(m)
	t.Cleanup(func() { stop(); m.Close(); store.Close() })
	if caps == nil {
		caps = []string{"message-tags", "echo-message", "account-tag"}
	}
	set := map[string]bool{}
	for _, c := range caps {
		set[c] = true
	}
	w := m.space("alice")
	// An unconnected client: nothing is sent, the queue holds what would be.
	client := girc.New(girc.Config{Server: e2eeHome.Host, Port: e2eeHome.Port, Nick: "alice", User: "alice", Name: "alice"})
	c := &connection{owner: w, manager: m, client: client, ctx: context.Background(), ready: true, queue: make(chan []*girc.Event, 16), caps: set, config: e2eeHome,
		nicks: map[string]string{}, pending: map[string]pending{}, historyBatches: map[string]bool{}, historyResponses: map[string]*historyRequest{}}
	w.mu.Lock()
	w.connections["home"] = c
	w.configs["home"] = e2eeHome
	w.networks["home"] = Network{ID: "home", Name: "home", ISupport: map[string]string{}}
	w.mu.Unlock()
	return &e2eeFixture{m: m, store: store, w: w, c: c, pusher: pusher, at: time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)}
}

func (f *e2eeFixture) passGate() {
	f.c.mu.Lock()
	f.c.e2eeTest = e2eePassed
	f.c.mu.Unlock()
}

func (f *e2eeFixture) features(policy string) model.Features {
	f.c.mu.Lock()
	defer f.c.mu.Unlock()
	return f.c.features(map[string]string{"CLIENTTAGDENY": policy})
}

// direct opens alice's direct conversation with bob.
func (f *e2eeFixture) direct(t *testing.T) Conversation {
	t.Helper()
	f.w.mu.Lock()
	defer f.w.mu.Unlock()
	return f.c.conversation("bob")
}

// receive delivers a live PRIVMSG from bob to alice.
func (f *e2eeFixture) receive(t *testing.T, msgid, text string, tags ...string) {
	t.Helper()
	f.at = f.at.Add(time.Second)
	e := girc.Event{Command: "PRIVMSG", Source: &girc.Source{Name: "bob", Ident: "bob", Host: "bob.example"}, Params: []string{"alice", text},
		Tags: girc.Tags{"msgid": msgid, "time": f.at.Format(time.RFC3339Nano), "account": "bob"}}
	for i := 0; i+1 < len(tags); i += 2 {
		e.Tags[tags[i]] = tags[i+1]
	}
	f.c.message(e)
	if err := f.c.owner.storageErr; err != nil {
		t.Fatal(err)
	}
}

// bind makes alice's conversation with bob encrypted, as alice's client does
// after creating the group.
func (f *e2eeFixture) bind(t *testing.T, conversation string) {
	t.Helper()
	if err := f.store.CreateE2EEGroup(storage.E2EEGroup{ID: "g1", Epoch: 1, Members: []string{"alice", "bob"}, CreatedBy: "alice"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.store.BindE2EEConversation("alice", conversation, "bob", "g1", ""); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptedTagFormat(t *testing.T) {
	payload := strings.Repeat("A", model.MaxEncryptedPayload)
	for value, want := range map[string]*model.Encrypted{
		"1.m.abc_-09":          {Version: 1, Class: model.EncryptedMessage, Payload: "abc_-09"},
		"1.a.abc":              {Version: 1, Class: model.EncryptedAuxiliary, Payload: "abc"},
		"1.m." + payload:       {Version: 1, Class: model.EncryptedMessage, Payload: payload},
		"1.m." + payload + "A": nil,
		"1.m.":                 nil,
		"1.m.ab+c":             nil,
		"1.m.abc=":             nil,
		"1.x.abc":              nil,
		"2.m.abc":              nil,
		"t.probe":              nil,
		"":                     nil,
		"1.m":                  nil,
	} {
		got := parseEncryptedTag(value)
		if (got == nil) != (want == nil) || got != nil && *got != *want {
			t.Errorf("parseEncryptedTag(%.20q) = %+v, want %+v", value, got, want)
		}
		if got != nil && encryptedTagValue(got) != value {
			t.Errorf("encryptedTagValue does not round-trip %.20q", value)
		}
	}
}

func TestEncryptedSendCarriesOnlyThePayload(t *testing.T) {
	ok := SendInput{ClientID: "c1", ConversationID: "dm", Encrypted: &model.Encrypted{Payload: "abc"}}
	if err := normalizeSendInput(&ok); err != nil {
		t.Fatal(err)
	}
	if ok.Text != model.EncryptedPlaceholder || ok.Kind != "message" || ok.Encrypted.Version != 1 || ok.Encrypted.Class != model.EncryptedMessage {
		t.Fatalf("normalized input = %+v", ok)
	}
	for name, input := range map[string]SendInput{
		"plaintext":  {ClientID: "c", Text: "secret", Encrypted: &model.Encrypted{Payload: "abc"}},
		"action":     {ClientID: "c", Kind: "action", Encrypted: &model.Encrypted{Payload: "abc"}},
		"reply":      {ClientID: "c", ReplyToID: "x", Encrypted: &model.Encrypted{Payload: "abc"}},
		"gif":        {ClientID: "c", GIF: &GIF{}, Encrypted: &model.Encrypted{Payload: "abc"}},
		"upload":     {ClientID: "c", Attachments: []string{"u"}, Encrypted: &model.Encrypted{Payload: "abc"}},
		"no payload": {ClientID: "c", Encrypted: &model.Encrypted{}},
		"padding":    {ClientID: "c", Encrypted: &model.Encrypted{Payload: "abc="}},
		"too large":  {ClientID: "c", Encrypted: &model.Encrypted{Payload: strings.Repeat("A", model.MaxEncryptedPayload+1)}},
		"version":    {ClientID: "c", Encrypted: &model.Encrypted{Version: 2, Payload: "abc"}},
		"class":      {ClientID: "c", Encrypted: &model.Encrypted{Class: "other", Payload: "abc"}},
		"client ID":  {ClientID: "", Encrypted: &model.Encrypted{Payload: "abc"}},
	} {
		if err := normalizeSendInput(&input); err == nil {
			t.Errorf("%s: accepted %+v", name, input)
		}
	}
}

// The capability gate: encryption is offered only on the home network, with the
// operator switch on, message-tags, echo-message and account-tag negotiated, a
// CLIENTTAGDENY policy that lets +ircord/e2ee through, and a passed self-test.
func TestEncryptionCapabilityGate(t *testing.T) {
	f := newE2EEFixture(t)
	if f.features("").Encryption {
		t.Fatal("encryption offered before the self-test")
	}
	f.passGate()
	if !f.features("").Encryption {
		t.Fatal("encryption not offered after a passed self-test")
	}
	for policy, want := range map[string]bool{"*": false, "ircord/e2ee": false, "*,-ircord/e2ee": true, "typing": true} {
		if got := f.features(policy).Encryption; got != want {
			t.Errorf("CLIENTTAGDENY=%q: encryption %v, want %v", policy, got, want)
		}
	}
	for _, missing := range []string{"message-tags", "echo-message", "account-tag"} {
		g := newE2EEFixture(t)
		delete(g.c.caps, missing)
		g.passGate()
		if g.features("").Encryption {
			t.Errorf("encryption offered without %s", missing)
		}
	}
	f.c.mu.Lock()
	f.c.e2eeTest = e2eeFailed
	f.c.mu.Unlock()
	if f.features("").Encryption {
		t.Fatal("encryption offered after a failed self-test")
	}
	other := newE2EEFixture(t)
	other.c.config.ID = "elsewhere"
	other.passGate()
	if other.features("").Encryption {
		t.Fatal("encryption offered on a network other than the home network")
	}
	off := newE2EEFixture(t)
	off.m.SetE2EE(false)
	off.passGate()
	if off.features("").Encryption || off.m.EncryptionCapability() != nil {
		t.Fatal("encryption offered with the operator switch off")
	}
}

// The self-test sends one full-size tagged TAGMSG to our own nick and passes only
// on an exact echo from our own nick. A truncated tag never passes, and without
// an echo the test fails at its timeout.
func TestEncryptionSelfTest(t *testing.T) {
	f := newE2EEFixture(t)
	f.c.probeEncryption(f.c.client)
	sent := drain(f.c)
	if len(sent) != 1 || sent[0].Command != "TAGMSG" || len(sent[0].Params) != 1 || sent[0].Params[0] != "alice" {
		t.Fatalf("expected one TAGMSG to our own nick, got %v", sent)
	}
	value := sent[0].Tags[e2eeTag]
	if !strings.HasPrefix(value, e2eeProbePrefix) || len(value) != len("1.m.")+model.MaxEncryptedPayload {
		t.Fatalf("probe is %d bytes, want the longest encrypted tag", len(value))
	}
	f.c.probeEncryption(f.c.client)
	if again := drain(f.c); len(again) != 0 {
		t.Fatalf("the self-test runs once per connection, got %v", again)
	}
	echo := func(nick, value string) {
		f.c.encryptionProbe(f.c.client, girc.Event{Command: "TAGMSG", Source: &girc.Source{Name: nick}, Params: []string{"alice"}, Tags: girc.Tags{e2eeTag: value}})
	}
	echo("alice", value[:len(value)-100])
	echo("mallory", value)
	if f.c.e2eeTest != e2eeTesting {
		t.Fatal("a truncated echo or one from another nick passed the self-test")
	}
	echo("alice", value)
	if f.c.e2eeTest != e2eePassed || !f.features("").Encryption {
		t.Fatal("an exact echo did not pass the self-test")
	}
	f.w.mu.Lock()
	n := f.w.networks["home"]
	f.w.mu.Unlock()
	if !n.Features.Encryption {
		t.Fatal("the network's features were not refreshed after the self-test")
	}

	previous := e2eeProbeTimeout
	e2eeProbeTimeout = 20 * time.Millisecond
	t.Cleanup(func() { e2eeProbeTimeout = previous })
	silent := newE2EEFixture(t)
	silent.c.probeEncryption(silent.c.client)
	drain(silent.c)
	eventually(t, func() bool {
		silent.c.mu.Lock()
		defer silent.c.mu.Unlock()
		return silent.c.e2eeTest == e2eeFailed
	})
	if silent.features("").Encryption {
		t.Fatal("encryption offered when the self-test echo never came")
	}

	ineligible := newE2EEFixture(t, "message-tags", "echo-message")
	ineligible.c.probeEncryption(ineligible.c.client)
	if sent := drain(ineligible.c); len(sent) != 0 {
		t.Fatalf("self-test sent without account-tag: %v", sent)
	}
}

// An encrypted message's body is replaced by the placeholder and nothing of it
// reaches search, the link catalog, the inbox excerpt or push text.
func TestEncryptedMessageStaysOutOfIndexesInboxAndPush(t *testing.T) {
	f := newE2EEFixture(t)
	conv := f.direct(t)
	f.receive(t, "m1", "secret https://example.com/page.png @alice", e2eeTag, "1.m.cipher_text", "+ircord/gif", "eyJ9", "+draft/reply", "m0")
	stored, err := f.store.Message("alice", conv.ID, "home:m1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Text != model.EncryptedPlaceholder || stored.Encrypted == nil || stored.Encrypted.Payload != "cipher_text" || stored.GIF != nil || stored.ReplyTo != nil {
		t.Fatalf("stored encrypted message = %+v", stored)
	}
	for _, query := range []string{"encrypted", "IRCord", "secret", "cipher_text"} {
		page, err := f.store.SearchPage("alice", storage.SearchFilter{Query: query}, "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 0 {
			t.Fatalf("search for %q found an encrypted message", query)
		}
	}
	if page, err := f.store.SearchPage("alice", storage.SearchFilter{Has: []string{"link"}}, "", 10); err != nil || len(page.Items) != 0 {
		t.Fatalf("link search found an encrypted message: %v %v", page.Items, err)
	}
	if storage.MatchesSearch(stored, storage.SearchFilter{}) {
		t.Fatal("the in-memory search matches an encrypted message")
	}
	var links int
	if err = f.store.DB.QueryRow("SELECT count(*) FROM message_links WHERE owner='alice'").Scan(&links); err != nil || links != 0 {
		t.Fatalf("link catalog has %d entries for an encrypted message (%v)", links, err)
	}
	inbox, err := f.m.Inbox("alice", "all", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox.Items) != 1 || !inbox.Items[0].Encrypted || inbox.Items[0].Excerpt != "" || inbox.Items[0].Status != "available" {
		t.Fatalf("inbox = %+v", inbox.Items)
	}
	if len(f.pusher.alerts) != 1 || f.pusher.alerts[0].Body != EncryptedNotificationBody || strings.Contains(f.pusher.alerts[0].Body, "secret") {
		t.Fatalf("alerts = %+v", f.pusher.alerts)
	}
	f.w.mu.Lock()
	unread := f.w.conversations[conv.ID].UnreadCount
	f.w.mu.Unlock()
	if unread != 1 {
		t.Fatalf("unread = %d after one encrypted message", unread)
	}

	// An auxiliary event (reaction, edit, delete, control) is stored and
	// delivered, but never counts as unread, enters the inbox or alerts.
	f.receive(t, "a1", "anything", e2eeTag, "1.a.reaction")
	f.w.mu.Lock()
	unread = f.w.conversations[conv.ID].UnreadCount
	f.w.mu.Unlock()
	if unread != 1 {
		t.Fatalf("an auxiliary event counted as unread: %d", unread)
	}
	if n, err := f.store.Unread("alice", conv.ID); err != nil || n != 1 {
		t.Fatalf("stored unread count = %d (%v), want 1", n, err)
	}
	if inbox, _ = f.m.Inbox("alice", "all", "", 10); len(inbox.Items) != 1 {
		t.Fatalf("an auxiliary event entered the inbox: %+v", inbox.Items)
	}
	if len(f.pusher.alerts) != 1 {
		t.Fatalf("an auxiliary event alerted: %+v", f.pusher.alerts)
	}
	aux, err := f.store.Message("alice", conv.ID, "home:a1")
	if err != nil || aux.Encrypted == nil || aux.Encrypted.Class != model.EncryptedAuxiliary || aux.Mention {
		t.Fatalf("stored auxiliary event = %+v (%v)", aux, err)
	}

	// Plaintext still works in the same conversation, which is not bound.
	f.receive(t, "p1", "plain words https://example.com/a")
	if page, _ := f.store.SearchPage("alice", storage.SearchFilter{Query: "plain"}, "", 10); len(page.Items) != 1 {
		t.Fatalf("plaintext message not searchable: %v", page.Items)
	}

	// A plaintext edit line never rewrites an encrypted message; it stays a line of its own.
	f.receive(t, "e1", "rewritten in the clear", editTag, "m1")
	if stored, err = f.store.Message("alice", conv.ID, "home:m1"); err != nil || stored.Text != model.EncryptedPlaceholder || len(stored.Edits) != 0 || stored.EditedAt != nil {
		t.Fatalf("a plaintext edit changed an encrypted message: %+v (%v)", stored, err)
	}
	if line, err := f.store.Message("alice", conv.ID, "home:e1"); err != nil || line.Text != "rewritten in the clear" || line.EditOf != "" {
		t.Fatalf("the plaintext edit line = %+v (%v)", line, err)
	}

	// Saved-message search doesn't match the placeholder.
	if _, err = f.m.SaveMessage("alice", conv.ID, "home:m1"); err != nil {
		t.Fatal(err)
	}
	if page, err := f.m.SavedMessages("alice", "", "encrypted", 10); err != nil || len(page.Items) != 0 {
		t.Fatalf("saved search matched an encrypted message: %+v (%v)", page, err)
	}

	// Deleting an encrypted message drops its ciphertext.
	f.w.mu.Lock()
	deleted := f.c.applyRedaction(conv, stored, false)
	f.w.mu.Unlock()
	if !deleted.Deleted || deleted.Encrypted != nil {
		t.Fatalf("deleted encrypted message = %+v", deleted)
	}
	if stored, err = f.store.Message("alice", conv.ID, "home:m1"); err != nil || stored.Encrypted != nil || !stored.Deleted {
		t.Fatalf("stored deleted message = %+v (%v)", stored, err)
	}
}

// The IRC server's own search sees only placeholders of encrypted messages; none
// of them is returned, whether or not the tag survived.
func TestUpstreamSearchSkipsEncryptedMessages(t *testing.T) {
	f, conversation := startSearchFixture(t)
	finished := make(chan searchRead, 1)
	go func() {
		p, e := f.m.SearchMessagesContext(context.Background(), "alice", SearchQuery{Query: "encrypted", Conversations: []string{conversation}, Source: "upstream"})
		finished <- searchRead{p, e}
	}()
	request := searchDispatch(t, f.c)
	searchEvent(f.c, girc.Event{Command: "BATCH", Params: []string{"+b", "soju.im/search"}, Tags: girc.Tags{"label": request.Tags["label"]}})
	for i, tags := range []girc.Tags{{e2eeTag: "1.m.cipher"}, {}} {
		tags["batch"], tags["time"], tags["msgid"] = "b", f.at.Format(time.RFC3339Nano), "s"+string(rune('0'+i))
		searchEvent(f.c, girc.Event{Command: "PRIVMSG", Source: &girc.Source{Name: "bob"}, Params: []string{"#test", model.EncryptedPlaceholder}, Tags: tags})
	}
	searchEvent(f.c, girc.Event{Command: "PRIVMSG", Source: &girc.Source{Name: "bob"}, Params: []string{"#test", "encrypted backups are fine"}, Tags: girc.Tags{"batch": "b", "time": f.at.Format(time.RFC3339Nano), "msgid": "plain"}})
	searchEvent(f.c, girc.Event{Command: "BATCH", Params: []string{"-b"}})
	result := <-finished
	if result.err != nil {
		t.Fatal(result.err)
	}
	if len(result.page.Items) != 1 || result.page.Items[0].Message.Text != "encrypted backups are fine" {
		t.Fatalf("upstream search returned %+v", result.page.Items)
	}
}

// Once alice's conversation is bound to an encrypted group, the backend refuses
// plaintext sends, edits, reactions and raw PRIVMSG or NOTICE into it.
func TestPlaintextRefusedInEncryptedConversation(t *testing.T) {
	f := newE2EEFixture(t)
	f.passGate()
	conv := f.direct(t)
	f.receive(t, "m1", "hello before encryption")
	f.bind(t, conv.ID)

	if err := f.m.Send("alice", SendInput{ClientID: "c1", ConversationID: conv.ID, Text: "secret"}); !errors.Is(err, ErrPlaintextRefused) {
		t.Fatalf("plaintext send: %v", err)
	}
	if _, err := f.store.Command("alice", "c1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a refused plaintext send was queued: %v", err)
	}
	if err := f.m.dispatchSend("alice", SendInput{ClientID: "c2", ConversationID: conv.ID, Text: "queued earlier", Kind: "message"}, nil); !errors.Is(err, ErrPlaintextRefused) {
		t.Fatalf("a plaintext send queued before encryption: %v", err)
	}
	for _, line := range []string{"PRIVMSG bob :secret", "NOTICE bob :secret", "PRIVMSG carol,bob :secret", "PRIVMSG BOB :secret"} {
		if err := f.m.RawCommand("alice", "home", "r1", line); !errors.Is(err, ErrPlaintextRefused) {
			t.Fatalf("raw %q: %v", line, err)
		}
	}
	if _, err := f.m.Edit("alice", conv.ID, "home:m1", "edited"); !errors.Is(err, ErrPlaintextRefused) {
		t.Fatalf("plaintext edit: %v", err)
	}
	if _, err := f.m.ToggleReaction("alice", conv.ID, "home:m1", "👍"); !errors.Is(err, ErrPlaintextRefused) {
		t.Fatalf("plaintext reaction: %v", err)
	}
	if sent := drain(f.c); len(sent) != 0 {
		t.Fatalf("refused commands reached IRC: %v", sent)
	}
	if err := f.m.RawCommand("alice", "home", "r2", "PRIVMSG carol :hello"); err != nil {
		t.Fatalf("a raw message to someone else was refused: %v", err)
	}
	drain(f.c)

	// An encrypted send goes out as the placeholder with the payload in the tag.
	if err := f.m.Send("alice", SendInput{ClientID: "e1", ConversationID: conv.ID, Encrypted: &model.Encrypted{Payload: "cipher"}}); err != nil {
		t.Fatal(err)
	}
	sent := drain(f.c)
	if len(sent) != 1 || sent[0].Command != "PRIVMSG" || sent[0].Params[1] != model.EncryptedPlaceholder || sent[0].Tags[e2eeTag] != "1.m.cipher" || sent[0].Tags["+ircord/client-id"] != "e1" {
		t.Fatalf("encrypted send = %v", sent)
	}

	// With the operator switch off nothing is refused: the conversation must stay
	// usable when encryption can neither be used nor turned off.
	f.m.SetE2EE(false)
	if err := f.m.Send("alice", SendInput{ClientID: "c4", ConversationID: conv.ID, Text: "switch off"}); err != nil {
		t.Fatalf("plaintext with the operator switch off: %v", err)
	}
	f.m.SetE2EE(true)
	drain(f.c)

	// Turning encryption off accepts plaintext again.
	if err := f.store.TurnOffE2EE("alice", conv.ID, "g1"); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Send("alice", SendInput{ClientID: "c3", ConversationID: conv.ID, Text: "plain again"}); err != nil {
		t.Fatalf("plaintext after turning encryption off: %v", err)
	}
}

// Encrypted sends never fall back to plaintext: they need a passed gate and a
// direct conversation, and wait while a new connection's self-test runs.
func TestEncryptedSendNeedsTheGate(t *testing.T) {
	f := newE2EEFixture(t)
	conv := f.direct(t)
	send := func(client, conversation string) error {
		return f.m.Send("alice", SendInput{ClientID: client, ConversationID: conversation, Encrypted: &model.Encrypted{Payload: "cipher"}})
	}
	f.c.mu.Lock()
	f.c.e2eeTest = e2eeTesting
	f.c.mu.Unlock()
	if err := send("e1", conv.ID); err != nil {
		t.Fatalf("a send during the self-test should stay queued: %v", err)
	}
	if command, err := f.store.Command("alice", "e1"); err != nil || command.State != "queued" {
		t.Fatalf("send during the self-test: %+v %v", command, err)
	}
	f.c.mu.Lock()
	f.c.e2eeTest = e2eeFailed
	f.c.mu.Unlock()
	if err := send("e2", conv.ID); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("send after a failed self-test: %v", err)
	}
	if sent := drain(f.c); len(sent) != 0 {
		t.Fatalf("an encrypted send reached IRC without the gate: %v", sent)
	}
	f.passGate()
	f.w.mu.Lock()
	channel := f.c.conversation("#room")
	channel.Joined = true
	f.w.conversations[channel.ID] = channel
	f.w.mu.Unlock()
	if err := send("e3", channel.ID); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("encrypted send to a channel: %v", err)
	}
	f.m.SetE2EE(false)
	if err := send("e4", conv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("encrypted send with the operator switch off: %v", err)
	}
}
