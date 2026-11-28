package storage

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestE2EEObservedCommitsInPositionOrder(t *testing.T) {
	s := e2eeStore(t, "alice", "bob")
	if _, err := s.ObserveE2EECommit("alice", "g", E2EEObserved{Epoch: 1, At: 1, MsgID: "a", Sender: "bob", Commit: []byte("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a commit for an unknown chat: %v", err)
	}
	if err := s.PutE2EEChat("alice", E2EEChat{Group: "g", Kind: E2EEChatGroup, State: E2EEChatJoined, Target: "#e2ee-x"}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []E2EEObserved{
		{Epoch: 2, At: 50, MsgID: "e", Sender: "bob", Commit: []byte("2")},
		{Epoch: 1, At: 20, MsgID: "b", Sender: "bob", Commit: []byte("1b")},
		{Epoch: 1, At: 20, MsgID: "a", Sender: "alice", Commit: []byte("1a")},
		{Epoch: 1, At: 10, MsgID: "z", Sender: "bob", Commit: []byte("1z")},
	} {
		if fresh, err := s.ObserveE2EECommit("alice", "g", v); err != nil || !fresh {
			t.Fatalf("observe %+v: %v %v", v, fresh, err)
		}
	}
	// A copy already seen changes nothing.
	if fresh, err := s.ObserveE2EECommit("alice", "g", E2EEObserved{Epoch: 1, At: 10, MsgID: "z", Sender: "bob", Commit: []byte("1z")}); err != nil || fresh {
		t.Fatalf("duplicate: %v %v", fresh, err)
	}
	log, err := s.E2EEObservedCommits("alice", "g", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	order := ""
	for _, v := range log {
		order += string(v.Commit) + " "
	}
	if order != "1z 1a 1b 2 " {
		t.Fatalf("order %q", order)
	}
	if !log[0].Before(log[1]) || log[1].Before(log[0]) || !log[1].Before(log[2]) {
		t.Fatal("Before disagrees with the stored order")
	}
	if latest, err := s.E2EELatestObservedEpoch("alice", "g"); err != nil || latest != 2 {
		t.Fatalf("latest %d %v", latest, err)
	}
	// bob's account sees nothing of alice's chat.
	if log, _ := s.E2EEObservedCommits("bob", "g", 0, 10); len(log) != 0 {
		t.Fatal("observed log leaked across accounts")
	}
	// Leaving the chat forgets its log.
	if err = s.DeleteE2EEChat("alice", "g"); err != nil {
		t.Fatal(err)
	}
	if log, _ := s.E2EEObservedCommits("alice", "g", 0, 10); len(log) != 0 {
		t.Fatal("log kept after leaving")
	}
}

func TestE2EEChatBindsTheConversation(t *testing.T) {
	s := e2eeStore(t, "alice")
	chat := E2EEChat{Group: "g", Kind: E2EEChatDirect, State: E2EEChatInvited, Conversation: "conv", Target: "bob", Peer: "bob", Inviter: "bob", Transfer: "t"}
	if err := s.PutE2EEChat("alice", chat); err != nil {
		t.Fatal(err)
	}
	if encrypted, err := s.E2EEConversationEncrypted("alice", "conv"); err != nil || encrypted {
		t.Fatalf("an invitation binds the conversation: %v %v", encrypted, err)
	}
	chat.State = E2EEChatJoined
	if err := s.PutE2EEChat("alice", chat); err != nil {
		t.Fatal(err)
	}
	if encrypted, err := s.E2EEConversationEncrypted("alice", "conv"); err != nil || !encrypted {
		t.Fatalf("joined chat: %v %v", encrypted, err)
	}
	got, err := s.E2EEChat("alice", "g")
	if err != nil || got.Transfer != "t" || got.Inviter != "bob" || got.State != E2EEChatJoined {
		t.Fatalf("chat %+v %v", got, err)
	}
}

func TestE2EEChatWelcomesReachEveryDevice(t *testing.T) {
	s := e2eeStore(t, "alice")
	publishDevice(t, s, "alice", "one")
	identity, _ := s.E2EEIdentity("alice")
	if err := s.PutE2EEDevice("alice", "two", identity.Generation, []byte("cert"), nil, ""); err != nil {
		t.Fatal(err)
	}
	w := E2EEChatWelcome{Group: "g", Epoch: 1, Kind: E2EEChatGroup, Target: "#e2ee-x", Sender: "bob", Welcome: []byte("welcome"), At: 5, MsgID: "m"}
	if n, err := s.AddE2EEChatWelcome("alice", w); err != nil || n != 2 {
		t.Fatalf("stored for %d devices: %v", n, err)
	}
	// The same Welcome again, after a lost receipt, is kept once.
	if n, err := s.AddE2EEChatWelcome("alice", w); err != nil || n != 2 {
		t.Fatalf("again: %d %v", n, err)
	}
	one, _ := s.E2EEChatWelcomes("alice", "one")
	two, _ := s.E2EEChatWelcomes("alice", "two")
	if len(one) != 1 || len(two) != 1 || !bytes.Equal(one[0].Welcome, []byte("welcome")) || one[0].Sender != "bob" {
		t.Fatalf("welcomes %+v %+v", one, two)
	}
	if err := s.DeleteE2EEChatWelcome("alice", one[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteE2EEChatWelcome("alice", one[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted twice: %v", err)
	}
	// Removing a device removes its Welcomes.
	if err := s.DeleteE2EEDevice("alice", "two"); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.E2EEChatWelcomes("alice", "two"); len(left) != 0 {
		t.Fatal("a removed device kept its Welcome")
	}
}

func TestE2EEOutboxRetriesUntilTheReceipt(t *testing.T) {
	s := e2eeStore(t, "alice")
	if err := s.AddE2EEOutbox("alice", "g", "transfer", []string{"bob", "carol"}, []byte("w")); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	due, err := s.DueE2EEOutbox("alice", now, 10)
	if err != nil || len(due) != 2 {
		t.Fatalf("due %d %v", len(due), err)
	}
	if owners, _ := s.DueE2EEOutboxOwners(now); len(owners) != 0 {
		t.Fatalf("still due right after an attempt: %v", owners)
	}
	// The second attempt waits a minute, the third five more.
	if due, _ = s.DueE2EEOutbox("alice", now.Add(59*time.Second), 10); len(due) != 0 {
		t.Fatal("retried within a minute")
	}
	if due, _ = s.DueE2EEOutbox("alice", now.Add(61*time.Second), 10); len(due) != 2 {
		t.Fatal("not retried after a minute")
	}
	if due, _ = s.DueE2EEOutbox("alice", now.Add(4*time.Minute), 10); len(due) != 0 {
		t.Fatal("retried too soon")
	}
	// Seeing bob online makes his Welcome due at once.
	if woke, err := s.WakeE2EEOutbox("alice", "BOB"); err != nil || !woke {
		t.Fatalf("wake %v %v", woke, err)
	}
	if owners, _ := s.DueE2EEOutboxOwners(time.Now()); len(owners) != 1 || owners[0] != "alice" {
		t.Fatalf("owners %v", owners)
	}
	// bob's receipt ends his retries; carol declines.
	if group, err := s.ConfirmE2EEOutbox("alice", "transfer", "bob", false); err != nil || group != "g" {
		t.Fatalf("confirm %q %v", group, err)
	}
	if _, err := s.ConfirmE2EEOutbox("alice", "transfer", "carol", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmE2EEOutbox("alice", "transfer", "mallory", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a receipt from someone else: %v", err)
	}
	states, _ := s.E2EEOutboxStates("alice", "g")
	if states["bob"] != E2EEOutboxDelivered || states["carol"] != E2EEOutboxDeclined {
		t.Fatalf("states %v", states)
	}
	if due, _ = s.DueE2EEOutbox("alice", now.Add(48*time.Hour), 10); len(due) != 0 {
		t.Fatal("a confirmed Welcome is still retried")
	}
}

func TestE2EEDeviceListCounterOnlyGoesUp(t *testing.T) {
	s := e2eeStore(t, "alice")
	if _, err := s.PutE2EEDeviceList("alice", []byte("l1"), 1); !errors.Is(err, ErrE2EEConflict) {
		t.Fatalf("without an identity: %v", err)
	}
	publishDevice(t, s, "alice", "one")
	if changed, err := s.PutE2EEDeviceList("alice", []byte("l1"), 1); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if changed, err := s.PutE2EEDeviceList("alice", []byte("l1"), 1); err != nil || changed {
		t.Fatalf("the same list again: %v %v", changed, err)
	}
	if _, err := s.PutE2EEDeviceList("alice", []byte("old"), 1); !errors.Is(err, ErrE2EEConflict) {
		t.Fatalf("a different list with the same counter: %v", err)
	}
	if changed, err := s.PutE2EEDeviceList("alice", []byte("l3"), 3); err != nil || !changed {
		t.Fatal(changed, err)
	}
	list, counter, err := s.E2EEDeviceList("alice")
	if err != nil || counter != 3 || string(list) != "l3" {
		t.Fatalf("list %q %d %v", list, counter, err)
	}
	// Starting fresh clears the list with the old identity.
	if _, err = s.PublishE2EEIdentity("alice", []byte("new"), true); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.E2EEDeviceList("alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after a reset: %v", err)
	}
}

func TestE2EERejoinsAreBounded(t *testing.T) {
	s := e2eeStore(t, "alice")
	if err := s.PutE2EEChat("alice", E2EEChat{Group: "g", Kind: E2EEChatGroup, State: E2EEChatJoined, Target: "#e2ee-x"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < E2EEMaxRejoins; i++ {
		if err := s.AddE2EERejoin("alice", "g", E2EERejoin{Account: "bob", Device: string(rune('a' + i)), Certificate: []byte("c"), KeyPackage: []byte("k")}); err != nil {
			t.Fatal(err)
		}
	}
	// The same device asking again replaces its earlier request.
	if err := s.AddE2EERejoin("alice", "g", E2EERejoin{Account: "bob", Device: "a", Certificate: []byte("c2"), KeyPackage: []byte("k2")}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddE2EERejoin("alice", "g", E2EERejoin{Account: "carol", Device: "new", Certificate: []byte("c"), KeyPackage: []byte("k")}); !errors.Is(err, ErrE2EELimit) {
		t.Fatalf("over the limit: %v", err)
	}
	rejoins, err := s.E2EERejoins("alice", "g")
	if err != nil || len(rejoins) != E2EEMaxRejoins || string(rejoins[len(rejoins)-1].Certificate) != "c2" {
		t.Fatalf("%d rejoins %v", len(rejoins), err)
	}
}
