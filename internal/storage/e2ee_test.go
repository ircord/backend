package storage

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func e2eeStore(t *testing.T, owners ...string) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, owner := range owners {
		if _, err = s.CreateUser(owner, owner, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// publishDevice gives owner an identity and one published device.
func publishDevice(t *testing.T, s *Store, owner, device string) {
	t.Helper()
	identity, err := s.PublishE2EEIdentity(owner, []byte(owner+"-key"), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PutE2EEDevice(owner, device, identity.Generation, []byte("cert"), nil, ""); err != nil {
		t.Fatal(err)
	}
}

func TestE2EEIdentityResetRemovesTheOldIdentity(t *testing.T) {
	s := e2eeStore(t, "alice", "bob")
	first, err := s.PublishE2EEIdentity("alice", []byte("key-1"), false)
	if err != nil || first.Generation != 1 {
		t.Fatalf("first identity = %+v, %v", first, err)
	}
	if again, err := s.PublishE2EEIdentity("alice", []byte("key-1"), false); err != nil || again.Generation != 1 {
		t.Fatalf("publishing the same key again = %+v, %v", again, err)
	}
	if _, err = s.PublishE2EEIdentity("alice", []byte("key-2"), false); !errors.Is(err, ErrE2EEConflict) {
		t.Fatalf("a different key without reset: %v", err)
	}
	if err = s.PutE2EEDevice("alice", "laptop", 1, []byte("cert"), []byte("resort"), "Firefox on Linux"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddE2EEKeyPackages("alice", "laptop", [][]byte{[]byte("kp1")}); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateE2EEGroup(E2EEGroup{ID: "g", Epoch: 1, Members: []string{"bob", "alice"}, CreatedBy: "bob"}, []E2EEWelcomeInput{{Owner: "alice", Device: "laptop", Welcome: []byte("w")}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AddE2EEApproval("alice", "laptop", "phone", []byte("hello")); err != nil {
		t.Fatal(err)
	}

	reset, err := s.PublishE2EEIdentity("alice", []byte("key-2"), true)
	if err != nil || reset.Generation != 2 || string(reset.IdentityKey) != "key-2" {
		t.Fatalf("reset = %+v, %v", reset, err)
	}
	if devices, _ := s.E2EEDevices("alice"); len(devices) != 0 {
		t.Fatalf("devices survived a reset: %+v", devices)
	}
	if welcomes, _ := s.E2EEWelcomes("alice", "laptop"); len(welcomes) != 0 {
		t.Fatal("Welcomes survived a reset")
	}
	if approvals, _ := s.E2EEApprovals("alice", "phone"); len(approvals) != 0 {
		t.Fatal("approval messages survived a reset")
	}
	var packages int
	if err = s.DB.QueryRow("SELECT count(*) FROM e2ee_key_packages WHERE owner='alice'").Scan(&packages); err != nil || packages != 0 {
		t.Fatalf("%d key packages survived a reset (%v)", packages, err)
	}
	if err = s.PutE2EEDevice("alice", "laptop", 1, []byte("cert"), nil, ""); !errors.Is(err, ErrE2EEConflict) {
		t.Fatalf("a device certificate for the old identity: %v", err)
	}
	if err = s.PutE2EEDevice("alice", "phone", 2, []byte("cert"), nil, ""); err != nil {
		t.Fatal(err)
	}
	if err = s.PutE2EEDevice("bob", "phone", 1, []byte("cert"), nil, ""); !errors.Is(err, ErrE2EEConflict) {
		t.Fatalf("a device before any identity: %v", err)
	}
}

func TestE2EEKeyPackagesAreSingleUseWithALastResort(t *testing.T) {
	s := e2eeStore(t, "alice")
	if _, err := s.PublishE2EEIdentity("alice", []byte("key"), false); err != nil {
		t.Fatal(err)
	}
	if err := s.PutE2EEDevice("alice", "a", 1, []byte("cert-a"), []byte("resort-a"), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.PutE2EEDevice("alice", "b", 1, []byte("cert-b"), nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddE2EEKeyPackages("alice", "a", [][]byte{[]byte("a1"), []byte("a2")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddE2EEKeyPackages("alice", "b", [][]byte{[]byte("b1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddE2EEKeyPackages("alice", "missing", [][]byte{[]byte("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("key packages for an unknown device: %v", err)
	}
	claims := func() string {
		t.Helper()
		identity, got, err := s.ClaimE2EEKeyPackages("alice")
		if err != nil || string(identity.IdentityKey) != "key" {
			t.Fatalf("claim identity %q: %v", identity.IdentityKey, err)
		}
		out := ""
		for _, c := range got {
			out += fmt.Sprintf("%s=%s/%v ", c.Device, c.KeyPackage, c.LastResort)
		}
		return out
	}
	for i, want := range []string{"a=a1/false b=b1/false ", "a=a2/false ", "a=resort-a/true ", "a=resort-a/true "} {
		if got := claims(); got != want {
			t.Fatalf("claim %d = %q, want %q", i+1, got, want)
		}
	}
	big := make([][]byte, E2EEMaxKeyPackages+1)
	for i := range big {
		big[i] = []byte("k")
	}
	if _, err := s.AddE2EEKeyPackages("alice", "b", big); !errors.Is(err, ErrE2EELimit) {
		t.Fatalf("more than %d key packages: %v", E2EEMaxKeyPackages, err)
	}
	for i := 0; i < E2EEMaxDevices-2; i++ {
		if err := s.PutE2EEDevice("alice", fmt.Sprint("d", i), 1, []byte("c"), nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutE2EEDevice("alice", "one-too-many", 1, []byte("c"), nil, ""); !errors.Is(err, ErrE2EELimit) {
		t.Fatalf("device %d: %v", E2EEMaxDevices+1, err)
	}
	if err := s.PutE2EEDevice("alice", "a", 1, []byte("cert-a2"), nil, "renamed"); err != nil {
		t.Fatalf("updating an existing device at the limit: %v", err)
	}
	if err := s.DeleteE2EEDevice("alice", "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteE2EEDevice("alice", "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting a deleted device: %v", err)
	}
}

// Concurrent commits for one epoch: exactly one wins each round, every loser is
// told the new epoch, and the log ends up with one commit per epoch, in order.
func TestE2EEGroupLogOrdersConcurrentCommits(t *testing.T) {
	s := e2eeStore(t, "alice", "bob")
	if err := s.CreateE2EEGroup(E2EEGroup{ID: "g", Epoch: 1, Members: []string{"alice", "bob"}, CreatedBy: "alice"}, nil); err != nil {
		t.Fatal(err)
	}
	const writers, rounds = 8, 5
	for round := 0; round < rounds; round++ {
		epoch := int64(1 + round)
		var wg sync.WaitGroup
		var mu sync.Mutex
		won, lost := 0, 0
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				owner := []string{"alice", "bob"}[i%2]
				next, err := s.AppendE2EECommit(owner, "g", epoch, []byte(fmt.Sprintf("commit %d by %d", epoch, i)), []byte(fmt.Sprintf("info %d", i)), nil)
				var conflict E2EEEpochConflict
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil && next == epoch+1:
					won++
				case errors.As(err, &conflict) && conflict.Epoch == epoch+1:
					lost++
				default:
					t.Errorf("commit %d by %d: next %d, %v", epoch, i, next, err)
				}
			}(i)
		}
		wg.Wait()
		if won != 1 || lost != writers-1 {
			t.Fatalf("round %d: %d won, %d lost", round, won, lost)
		}
	}
	commits, err := s.E2EECommits("bob", "g", 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != rounds {
		t.Fatalf("log has %d commits, want %d", len(commits), rounds)
	}
	for i, c := range commits {
		if c.Epoch != int64(1+i) {
			t.Fatalf("commit %d has epoch %d", i, c.Epoch)
		}
	}
	if later, _ := s.E2EECommits("alice", "g", 4, 100); len(later) != 2 || later[0].Epoch != 4 {
		t.Fatalf("commits from epoch 4 = %+v", later)
	}
	g, err := s.E2EEGroup("alice", "g")
	if err != nil || g.Epoch != 1+rounds || len(g.GroupInfo) == 0 {
		t.Fatalf("group after the commits = %+v, %v", g, err)
	}
	if err = s.CreateE2EEGroup(E2EEGroup{ID: "g", Epoch: 0, Members: []string{"alice"}, CreatedBy: "alice"}, nil); !errors.Is(err, ErrE2EEConflict) {
		t.Fatalf("reusing a group ID: %v", err)
	}
}

// Two replacement groups racing for one conversation after a reset: exactly one
// becomes current, and the other side is told which.
func TestE2EEReplacementGroupsRace(t *testing.T) {
	s := e2eeStore(t, "alice", "bob")
	for _, id := range []string{"old", "a", "b"} {
		if err := s.CreateE2EEGroup(E2EEGroup{ID: id, Epoch: 1, Members: []string{"alice", "bob"}, CreatedBy: "alice"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.BindE2EEConversation("alice", "alice-dm", "bob", "old", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.BindE2EEConversation("bob", "bob-dm", "alice", "old", ""); err != nil {
		t.Fatalf("the peer joining the current group: %v", err)
	}
	var conflict E2EEGroupConflict
	if err := s.BindE2EEConversation("bob", "bob-dm", "alice", "a", ""); !errors.As(err, &conflict) || conflict.Current != "old" {
		t.Fatalf("a different group without replaces: %v", err)
	}
	for attempt := 0; attempt < 20; attempt++ {
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, side := range []struct{ owner, conv, peer, group string }{{"alice", "alice-dm", "bob", "a"}, {"bob", "bob-dm", "alice", "b"}} {
			wg.Add(1)
			go func(i int, owner, conv, peer, group string) {
				defer wg.Done()
				errs[i] = s.BindE2EEConversation(owner, conv, peer, group, "old")
			}(i, side.owner, side.conv, side.peer, side.group)
		}
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("attempt %d: both or neither replacement won: %v", attempt, errs)
		}
		state, err := s.E2EEConversationState("alice", "alice-dm")
		if err != nil {
			t.Fatal(err)
		}
		winner := map[bool]string{true: "a", false: "b"}[errs[0] == nil]
		loser := errs[1]
		if errs[1] == nil {
			loser = errs[0]
		}
		if state.Group != winner || !errors.As(loser, &conflict) || conflict.Current != winner {
			t.Fatalf("attempt %d: current %q, loser %v", attempt, state.Group, loser)
		}
		if bob, _ := s.E2EEConversationState("bob", "bob-dm"); bob.Group != winner {
			t.Fatalf("the two sides disagree: %q and %q", state.Group, bob.Group)
		}
		// Put the old group back for the next attempt.
		if err = s.BindE2EEConversation("alice", "alice-dm", "bob", "old", winner); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.TurnOffE2EE("bob", "bob-dm", "a"); !errors.As(err, &conflict) {
		t.Fatalf("turning off a group that is no longer current: %v", err)
	}
	if err := s.TurnOffE2EE("bob", "bob-dm", "old"); err != nil {
		t.Fatal(err)
	}
	for _, side := range [][2]string{{"alice", "alice-dm"}, {"bob", "bob-dm"}} {
		if encrypted, err := s.E2EEConversationEncrypted(side[0], side[1]); err != nil || encrypted {
			t.Fatalf("%s still encrypted after turning off (%v)", side[0], err)
		}
	}
	// Turning it on again takes each side's own client: alice binding doesn't
	// lock bob's conversation, until bob's client joins the group too.
	if err := s.BindE2EEConversation("alice", "alice-dm", "bob", "a", ""); err != nil {
		t.Fatalf("turning encryption on again: %v", err)
	}
	if encrypted, _ := s.E2EEConversationEncrypted("alice", "alice-dm"); !encrypted {
		t.Fatal("alice's own binding did not take effect")
	}
	if encrypted, _ := s.E2EEConversationEncrypted("bob", "bob-dm"); encrypted {
		t.Fatal("alice locked bob's conversation again on her own")
	}
	if err := s.BindE2EEConversation("bob", "bob-dm", "alice", "a", ""); err != nil {
		t.Fatal(err)
	}
	if encrypted, _ := s.E2EEConversationEncrypted("bob", "bob-dm"); !encrypted {
		t.Fatal("bob's binding did not take effect")
	}
	if _, _, err := s.ClaimE2EEKeyPackages("bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claiming from an account without an identity: %v", err)
	}
}

// Every read and write is scoped to the account: a group is invisible to
// non-members, Welcomes, approvals and conversation bindings to other accounts.
func TestE2EEAccountIsolation(t *testing.T) {
	s := e2eeStore(t, "alice", "bob", "mallory")
	for _, owner := range []string{"bob", "mallory"} {
		publishDevice(t, s, owner, map[string]string{"bob": "phone", "mallory": "d"}[owner])
	}
	if err := s.CreateE2EEGroup(E2EEGroup{ID: "g", Epoch: 1, Members: []string{"alice", "bob"}, CreatedBy: "alice"}, []E2EEWelcomeInput{{Owner: "bob", Device: "phone", Welcome: []byte("w")}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateE2EEGroup(E2EEGroup{ID: "x", Epoch: 1, Members: []string{"alice"}, CreatedBy: "alice"}, []E2EEWelcomeInput{{Owner: "mallory", Device: "d", Welcome: []byte("w")}}); !errors.Is(err, ErrE2EEWelcomeTarget) {
		t.Fatalf("a Welcome for a non-member: %v", err)
	}
	if err := s.CreateE2EEGroup(E2EEGroup{ID: "y", Epoch: 1, Members: []string{"alice", "bob"}, CreatedBy: "alice"}, []E2EEWelcomeInput{{Owner: "bob", Device: "unpublished", Welcome: []byte("w")}}); !errors.Is(err, ErrE2EEWelcomeTarget) {
		t.Fatalf("a Welcome for a device that was never published: %v", err)
	}
	if _, err := s.E2EEGroup("alice", "y"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused group creation left the group behind: %v", err)
	}
	if _, err := s.E2EEGroup("mallory", "g"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a non-member read the group: %v", err)
	}
	if _, err := s.E2EECommits("mallory", "g", 0, 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a non-member read the log: %v", err)
	}
	if _, err := s.AppendE2EECommit("mallory", "g", 1, []byte("c"), nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a non-member committed: %v", err)
	}
	if _, err := s.AppendE2EECommit("alice", "g", 1, []byte("c"), nil, []E2EEWelcomeInput{{Owner: "mallory", Device: "d", Welcome: []byte("w")}}); !errors.Is(err, ErrE2EEWelcomeTarget) {
		t.Fatalf("a commit with a Welcome for a non-member: %v", err)
	}
	if g, _ := s.E2EEGroup("alice", "g"); g.Epoch != 1 {
		t.Fatalf("a refused commit moved the epoch to %d", g.Epoch)
	}
	if got, _ := s.E2EEWelcomes("mallory", "phone"); len(got) != 0 {
		t.Fatal("another account read bob's Welcome")
	}
	welcomes, _ := s.E2EEWelcomes("bob", "phone")
	if len(welcomes) != 1 {
		t.Fatalf("bob's Welcomes = %+v", welcomes)
	}
	if err := s.DeleteE2EEWelcome("mallory", welcomes[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another account removed bob's Welcome: %v", err)
	}
	if err := s.DeleteE2EEWelcome("bob", welcomes[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.BindE2EEConversation("mallory", "m-dm", "alice", "g", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a non-member bound a conversation to the group: %v", err)
	}
	if err := s.BindE2EEConversation("alice", "a-dm", "mallory", "g", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a group bound to a pair it doesn't belong to: %v", err)
	}
	if err := s.BindE2EEConversation("alice", "a-dm", "bob", "g", ""); err != nil {
		t.Fatal(err)
	}
	if encrypted, _ := s.E2EEConversationEncrypted("mallory", "a-dm"); encrypted {
		t.Fatal("another account's conversation ID reads as encrypted")
	}
	if err := s.TurnOffE2EE("mallory", "a-dm", "g"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another account turned off alice's encryption: %v", err)
	}
	id, err := s.AddE2EEApproval("alice", "laptop", "phone", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.E2EEApprovals("mallory", "phone"); len(got) != 0 {
		t.Fatal("another account read alice's approval message")
	}
	if err = s.DeleteE2EEApproval("mallory", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another account removed alice's approval message: %v", err)
	}
	if got, _ := s.E2EEApprovals("alice", "phone"); len(got) != 1 || string(got[0].Payload) != "hello" {
		t.Fatalf("alice's approval messages = %+v", got)
	}
	for i := 1; i < E2EEMaxPendingRelay; i++ {
		if _, err = s.AddE2EEApproval("alice", "laptop", "phone", []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.AddE2EEApproval("alice", "laptop", "phone", []byte("x")); !errors.Is(err, ErrE2EELimit) {
		t.Fatalf("approval message over the limit: %v", err)
	}
	if _, err = s.AddE2EEApproval("bob", "laptop", "phone", []byte("x")); err != nil {
		t.Fatalf("one account's full relay blocked another: %v", err)
	}
}

// Deleting an account removes its directory entries and memberships.
func TestE2EEAccountDeletion(t *testing.T) {
	s := e2eeStore(t, "alice", "bob")
	if _, err := s.PublishE2EEIdentity("alice", []byte("key"), false); err != nil {
		t.Fatal(err)
	}
	if err := s.PutE2EEDevice("alice", "d", 1, []byte("cert"), nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateE2EEGroup(E2EEGroup{ID: "g", Epoch: 1, Members: []string{"alice", "bob"}, CreatedBy: "alice"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.BindE2EEConversation("bob", "bob-dm", "alice", "g", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddE2EEKeyPackages("alice", "d", [][]byte{[]byte("kp")}); err != nil {
		t.Fatal(err)
	}
	// A group of alice's own devices only: nobody is left once she is gone.
	if err := s.CreateE2EEGroup(E2EEGroup{ID: "solo", Epoch: 1, Members: []string{"alice"}, CreatedBy: "alice"}, []E2EEWelcomeInput{{Owner: "alice", Device: "d", Welcome: []byte("w")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendE2EECommit("alice", "solo", 1, []byte("c"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddE2EEApproval("alice", "d", "e", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDeleting("alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAccount("alice", false); err != nil {
		t.Fatal(err)
	}
	for table, where := range map[string]string{
		"e2ee_identities":    "owner='alice'",
		"e2ee_devices":       "owner='alice'",
		"e2ee_group_members": "owner='alice'",
		"e2ee_pairs":         "owner_a='alice' OR owner_b='alice'",
		"e2ee_conversations": "owner='alice' OR peer='alice'",
		"e2ee_key_packages":  "owner='alice'",
		"e2ee_welcomes":      "owner='alice'",
		"e2ee_approvals":     "owner='alice'",
		"e2ee_groups":        "id='solo'",
		"e2ee_commits":       "group_id='solo'",
	} {
		var n int
		if err := s.DB.QueryRow("SELECT count(*) FROM " + table + " WHERE " + where).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s keeps %d rows of a deleted account (%v)", table, n, err)
		}
	}
	if g, err := s.E2EEGroup("bob", "g"); err != nil || len(g.Members) != 1 {
		t.Fatalf("the shared group should stay for bob: %+v (%v)", g, err)
	}
}
