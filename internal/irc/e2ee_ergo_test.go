package irc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"ircord/backend/internal/model"
	"ircord/backend/internal/storage"
)

func e2eeErgoManager(t *testing.T, host string, port int, owners ...string) (*Manager, *storage.Store) {
	t.Helper()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range owners {
		if _, err = store.CreateUser(owner, owner, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	m := New(Options{Store: store, AllowPrivate: true, ReconnectMin: time.Hour, E2EE: true, Home: NetworkConfig{ID: "home", Host: host, Port: port}})
	t.Cleanup(func() { m.Close(); store.Close() })
	return m, store
}

func e2eeErgoConnect(t *testing.T, m *Manager, owner string, v NetworkConfig) {
	t.Helper()
	if _, err := m.Connect(owner, v); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.WaitConnected(ctx, owner, v.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return m.Snapshot(owner).Networks[0].Features.Encryption })
}

func encryptedIn(m *Manager, owner, network, nick string) (Message, bool) {
	for _, conv := range m.Snapshot(owner).Conversations {
		if conv.NetworkID != network || conv.Kind != "direct" || conv.Name != nick {
			continue
		}
		for _, msg := range page(m, owner, conv.ID) {
			if msg.Encrypted != nil {
				return msg, true
			}
		}
	}
	return Message{}, false
}

// A full-size encrypted message survives send, echo, delivery and CHATHISTORY
// replay through Ergo unchanged, and the self-test turns encryption on there.
func TestErgoEncryptedDirectMessage(t *testing.T) {
	host, port := ergo(t)
	suffix := id()[:8]
	alice, bob := "xa"+suffix, "xb"+suffix
	password := id()
	registerErgoAccount(t, host, port, alice, password)
	registerErgoAccount(t, host, port, bob, password)
	m, _ := e2eeErgoManager(t, host, port, alice, bob)
	e2eeErgoConnect(t, m, alice, NetworkConfig{ID: "home", Name: "home", Host: host, Port: port, Nick: alice, SASLUser: alice, SASLPassword: password})
	e2eeErgoConnect(t, m, bob, NetworkConfig{ID: "home", Name: "home", Host: host, Port: port, Nick: bob, SASLUser: bob, SASLPassword: password})

	raw := make([]byte, model.MaxEncryptedPayload*3/4)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	if len(payload) != model.MaxEncryptedPayload {
		t.Fatalf("payload is %d characters", len(payload))
	}
	dm, err := m.Direct(alice, "home", bob)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Send(alice, SendInput{ClientID: id(), ConversationID: dm.ID, Encrypted: &model.Encrypted{Payload: payload}}); err != nil {
		t.Fatal(err)
	}
	var received Message
	eventually(t, func() bool { received, _ = encryptedIn(m, bob, "home", alice); return received.Encrypted != nil })
	if received.Encrypted.Payload != payload || received.Encrypted.Class != model.EncryptedMessage || received.Text != model.EncryptedPlaceholder || received.SenderAccount != alice {
		t.Fatalf("bob received %+v", received)
	}
	var echoed Message
	eventually(t, func() bool {
		echoed, _ = encryptedIn(m, alice, "home", bob)
		return echoed.Encrypted != nil && echoed.Receipt
	})
	if echoed.Encrypted.Payload != payload || echoed.Text != model.EncryptedPlaceholder {
		t.Fatalf("alice's echo %+v", echoed)
	}

	// An auxiliary event takes the same path and keeps its class.
	if err = m.Send(bob, SendInput{ClientID: id(), ConversationID: received.ConversationID, Encrypted: &model.Encrypted{Class: model.EncryptedAuxiliary, Payload: "reaction"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		for _, msg := range page(m, alice, dm.ID) {
			if msg.Encrypted != nil && msg.Encrypted.Class == model.EncryptedAuxiliary && msg.Encrypted.Payload == "reaction" {
				return true
			}
		}
		return false
	})

	// A new device of bob's starts with an empty store and replays the direct
	// conversation from Ergo's history.
	replay, _ := e2eeErgoManager(t, host, port, bob)
	e2eeErgoConnect(t, replay, bob, NetworkConfig{ID: "home", Name: "home", Host: host, Port: port, Nick: bob + "_", SASLUser: bob, SASLPassword: password})
	var replayed Message
	eventually(t, func() bool { replayed, _ = encryptedIn(replay, bob, "home", alice); return replayed.Encrypted != nil })
	if replayed.Encrypted.Payload != payload || replayed.Text != model.EncryptedPlaceholder || !replayed.Historical {
		t.Fatalf("replayed %+v", replayed)
	}
}

// Plaintext into a bound conversation is refused before it reaches Ergo, also
// through raw commands.
func TestErgoPlaintextRefused(t *testing.T) {
	host, port := ergo(t)
	suffix := id()[:8]
	alice, bob := "ya"+suffix, "yb"+suffix
	password := id()
	registerErgoAccount(t, host, port, alice, password)
	m, store := e2eeErgoManager(t, host, port, alice, bob)
	e2eeErgoConnect(t, m, alice, NetworkConfig{ID: "home", Name: "home", Host: host, Port: port, Nick: alice, SASLUser: alice, SASLPassword: password})
	dm, err := m.Direct(alice, "home", bob)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.CreateE2EEGroup(storage.E2EEGroup{ID: "g" + suffix, Epoch: 1, Members: []string{alice, bob}, CreatedBy: alice}, nil); err != nil {
		t.Fatal(err)
	}
	if err = store.BindE2EEConversation(alice, dm.ID, bob, "g"+suffix, ""); err != nil {
		t.Fatal(err)
	}
	if err = m.Send(alice, SendInput{ClientID: id(), ConversationID: dm.ID, Text: "secret"}); !errors.Is(err, ErrPlaintextRefused) {
		t.Fatalf("plaintext send: %v", err)
	}
	if err = m.Raw(alice, "home", "PRIVMSG "+bob+" :secret"); !errors.Is(err, ErrPlaintextRefused) {
		t.Fatalf("raw PRIVMSG: %v", err)
	}
}
