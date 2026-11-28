package irc

import (
	"bytes"
	"testing"

	"ircord/backend/internal/storage"
)

// A Welcome that names the sending account reaches the account's devices here
// without IRC, filed under the chat it already has; the other recipients go
// to the outbox. Sending it again stores it once.
func TestWelcomeForOwnDevices(t *testing.T) {
	f := newE2EEFixture(t)
	f.passGate()
	f.w.mu.Lock()
	network := f.w.networks["home"]
	network.Account = "alice"
	f.w.networks["home"] = network
	f.w.mu.Unlock()
	if _, err := f.store.PublishE2EEIdentity("alice", bytes.Repeat([]byte{1}, 32), false); err != nil {
		t.Fatal(err)
	}
	devices := []string{"d1", "d2"}
	for _, device := range devices {
		if err := f.store.PutE2EEDevice("alice", device, 1, []byte("certificate "+device), []byte("list"), "test"); err != nil {
			t.Fatal(err)
		}
	}
	group := newTransfer()
	if err := f.store.PutE2EEChat("alice", storage.E2EEChat{Group: group, Kind: storage.E2EEChatDirect, State: storage.E2EEChatJoined, Target: "bob", Peer: "bob", Inviter: "alice", Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	w := welcomeObject{Group: group, Epoch: 1, Kind: wireDirect, Welcome: []byte("welcome")}
	for range 2 {
		if err := f.m.queueWelcome(f.c, w, []string{"bob", "Alice", "bob"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, device := range devices {
		got, err := f.store.E2EEChatWelcomes("alice", device)
		if err != nil || len(got) != 1 {
			t.Fatalf("%s: %+v %v", device, got, err)
		}
		if v := got[0]; v.Group != group || v.Sender != "alice" || v.Target != "bob" || v.Kind != storage.E2EEChatDirect || !bytes.Equal(v.Welcome, w.Welcome) || v.MsgID != "" || v.At == 0 {
			t.Fatalf("%s: %+v", device, v)
		}
	}
	chat, err := f.store.E2EEChat("alice", group)
	if err != nil || chat.State != storage.E2EEChatJoined || chat.Peer != "bob" {
		t.Fatalf("the chat changed: %+v %v", chat, err)
	}
	states, err := f.store.E2EEOutboxStates("alice", group)
	if err != nil || len(states) != 1 || states["bob"] == "" {
		t.Fatalf("outbox %+v %v", states, err)
	}
}
