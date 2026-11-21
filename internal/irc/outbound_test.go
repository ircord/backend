package irc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"ircord/backend/internal/model"
	"ircord/backend/internal/storage"
)

func TestOutboundCannotSwitchEndpoints(t *testing.T) {
	plan := func(int) stubAttempt { return stubAttempt{welcome: true} }
	first, second := newStubServer(t, plan), newStubServer(t, plan)
	store := restoreStore(t)
	m := New(Options{Store: store, AllowPrivate: true})
	defer m.Close()
	cfg := lifecycleConfig(first.port())
	if _, err := m.Connect("alice", cfg); err != nil {
		t.Fatal(err)
	}
	waitConnected(t, m, "net")
	conv, err := m.Direct("alice", "net", "bob")
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.connection("alice", "net")
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.ready = false
	c.mu.Unlock()
	input := SendInput{ClientID: "original", ConversationID: conv.ID, Text: "Private to the original network",
		Endpoint: &model.SendEndpoint{Host: cfg.Host, Port: cfg.Port, TLS: cfg.TLS}}
	if err := m.Send("alice", input); err != nil {
		t.Fatal(err)
	}
	if _, err := m.UpdateNetwork("alice", "net", lifecycleConfig(second.port())); err != nil {
		t.Fatal(err)
	}
	var entry storage.OutboxEntry
	eventually(t, func() bool {
		entry, err = store.OutboxEntry("alice", input.ClientID)
		return err == nil && entry.State == "failed"
	})
	if !entry.AttemptedAt.IsZero() || entry.Payload.Text != input.Text {
		t.Fatalf("original message was attempted or lost: %v", entry)
	}
	if err := m.RetrySend("alice", input.ClientID, true); !errors.Is(err, errSendRouteChanged) {
		t.Fatalf("retry on the other network was allowed: %v", err)
	}
	input.ClientID = "saved-only-in-browser"
	if err := m.Send("alice", input); !errors.Is(err, errSendRouteChanged) {
		t.Fatalf("a browser copy for the original network was accepted: %v", err)
	}
	secondConnection, err := m.connection("alice", "net")
	if err != nil {
		t.Fatal(err)
	}
	secondConnection.mu.Lock()
	secondConnection.ready = false
	secondConnection.mu.Unlock()
	input.ClientID, input.Endpoint.Port = "queued-at-removal", second.port()
	if err := m.Send("alice", input); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteNetwork("alice", "net"); err != nil {
		t.Fatal(err)
	}
	entries, err := store.UnresolvedCommands("alice", time.Time{}, "", 10)
	if err != nil || len(entries) != 2 || entries[1].State != "failed" || entries[1].Payload.Text != input.Text || !entries[1].AttemptedAt.IsZero() {
		t.Fatalf("network removal lost or attempted the queued send: %v %v", entries, err)
	}
}

func TestOutboundUsesLabeledEchoAfterServerChangesText(t *testing.T) {
	stub := newStubServer(t, func(int) stubAttempt { return stubAttempt{welcome: true} })
	store := restoreStore(t)
	m := New(Options{Store: store, AllowPrivate: true})
	defer m.Close()
	if _, err := m.Connect("alice", lifecycleConfig(stub.port())); err != nil {
		t.Fatal(err)
	}
	waitConnected(t, m, "net")
	conv, err := m.Direct("alice", "net", "bob")
	if err != nil {
		t.Fatal(err)
	}
	c, err := m.connection("alice", "net")
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	client := c.client
	for _, capability := range []string{"message-tags", "echo-message", "labeled-response"} {
		c.caps[capability] = true
	}
	c.mu.Unlock()
	input := SendInput{ClientID: "formatted", ConversationID: conv.ID, Text: "\x02some text\x0f"}
	if err = m.Send("alice", input); err != nil {
		t.Fatal(err)
	}
	var label string
	eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		p := c.pending[input.ClientID]
		label = p.label
		return !p.at.IsZero() && label != ""
	})
	c.receive(client, girc.Event{Command: "PRIVMSG", Source: &girc.Source{Name: c.currentNick(), Ident: "alice"},
		Params: []string{"bob", "some text"}, Tags: girc.Tags{"label": label, "msgid": "modified"}})
	command, err := store.Command("alice", input.ClientID)
	if err != nil || command.State != "sent" {
		t.Fatalf("labeled send was not confirmed: %v %v", command, err)
	}
	message, err := store.Message("alice", conv.ID, command.MessageID)
	if err != nil || message.Text != "some text" || !message.Receipt {
		t.Fatalf("server's version was not retained: %v %v", message, err)
	}

	input.ClientID, input.Text = "unknown", "keep this even if the echo is lost"
	if err = m.Send("alice", input); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		p := c.pending[input.ClientID]
		if p.at.IsZero() {
			return false
		}
		label = p.label
		p.at = time.Now().Add(-16 * time.Second)
		c.pending[input.ClientID] = p
		return true
	})
	eventually(t, func() bool {
		entry, err := store.OutboxEntry("alice", input.ClientID)
		return err == nil && entry.State == "uncertain" && entry.Payload.Text == input.Text
	})
	if err := m.RetrySend("alice", input.ClientID, false); !errors.Is(err, ErrUncertain) {
		t.Fatalf("an ambiguous send was retried without consent: %v", err)
	}
	if err := m.RetrySend("alice", input.ClientID, true); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		p := c.pending[input.ClientID]
		return !p.at.IsZero() && p.label != "" && p.label != label
	})
}

func TestErgoOutboundQueuedBeforeWriteResumesAfterRestart(t *testing.T) {
	f := newMetadataFixture(t, "outbox")
	f.connect(f.bobSide, "bob", f.config(f.bob, ""), false)
	f.connect(f.aliceSide, "alice", f.config(f.alice, ""), false)
	profileChecked(t, f.aliceSide, "alice")
	conv, err := f.aliceSide.Direct("alice", "net", f.bob)
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.aliceSide.connection("alice", "net")
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.ready = false
	c.mu.Unlock()
	input := SendInput{ClientID: "before-write", ConversationID: conv.ID, Text: "Survives a restart before the first write"}
	if err = f.aliceSide.Send("alice", input); err != nil {
		t.Fatal(err)
	}
	entry, err := f.store.OutboxEntry("alice", input.ClientID)
	if err != nil || entry.State != "queued" || !entry.AttemptedAt.IsZero() {
		t.Fatalf("message was not durably queued: %v %v", entry, err)
	}
	f.restartAlice()
	eventually(t, func() bool {
		entry, err = f.store.OutboxEntry("alice", input.ClientID)
		return err == nil && entry.State == "sent"
	})
	msg, err := f.store.Message("alice", conv.ID, entry.MessageID)
	if err != nil || !msg.Receipt || msg.UpstreamID == "" || msg.Text != input.Text {
		t.Fatalf("message lacked an IRC receipt: %v %v", msg, err)
	}
	if err = f.aliceSide.Send("alice", input); err != nil {
		t.Fatal(err)
	}
	flushed(t, f.aliceSide, "alice")
	var copies int
	if err := f.store.DB.QueryRow("SELECT count(*) FROM messages WHERE owner='alice' AND text=?", input.Text).Scan(&copies); err != nil || copies != 1 {
		t.Fatalf("same client ID made duplicate messages: %d %v", copies, err)
	}
}

func TestErgoOutboundChecksRetainedReceiptWithoutReconnect(t *testing.T) {
	f := newMetadataFixture(t, "ack")
	f.connect(f.bobSide, "bob", f.config(f.bob, ""), false)
	f.connect(f.aliceSide, "alice", f.config(f.alice, ""), false)
	conv, err := f.aliceSide.Direct("alice", "net", f.bob)
	if err != nil {
		t.Fatal(err)
	}
	input := SendInput{ClientID: "lost-live-receipt", ConversationID: conv.ID, Text: "The server retained this before the echo was lost"}
	if err = f.aliceSide.Send("alice", input); err != nil {
		t.Fatal(err)
	}
	var message Message
	eventually(t, func() bool {
		command, e := f.store.Command("alice", input.ClientID)
		if e != nil || command.State != "sent" {
			return false
		}
		message, e = f.store.Message("alice", conv.ID, command.MessageID)
		return e == nil && message.UpstreamID != ""
	})
	// Model an echo that was lost locally after Ergo accepted and retained it.
	w := f.aliceSide.space("alice")
	w.mu.Lock()
	delete(w.messages, conv.ID)
	err = f.store.RemoveMessage("alice", conv.ID, message)
	if err == nil {
		_, err = f.store.DB.Exec("UPDATE outbox SET state='uncertain',message_id=NULL WHERE owner='alice' AND client_id=?", input.ClientID)
	}
	w.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := f.aliceSide.CheckSend(ctx, "alice", input.ClientID); err != nil {
		t.Fatal(err)
	}
	entry, err := f.store.OutboxEntry("alice", input.ClientID)
	if err != nil || entry.State != "sent" {
		lines, _, _, stop, traceErr := f.aliceSide.SubscribeTrace("alice", "net", "")
		if traceErr == nil {
			defer stop()
			for _, line := range lines {
				switch line.Command {
				case "CHATHISTORY", "BATCH", "PRIVMSG", "FAIL":
					t.Log(line.Command, line.Source, line.Params, line.Tags)
				}
			}
		}
		t.Fatalf("history did not confirm the retained message: %v %v", entry, err)
	}
	got, err := f.store.Message("alice", conv.ID, entry.MessageID)
	if err != nil || got.UpstreamID != message.UpstreamID || !got.Receipt {
		t.Fatalf("did not recover the original server receipt: %v %v", got, err)
	}
}
