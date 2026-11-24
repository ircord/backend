package irc

import (
	"encoding/json"
	"strings"
	"testing"

	"ircord/backend/internal/model"
	"ircord/backend/internal/storage"
)

// Other people's pictures reach clients in snapshot members and member events.
func TestOtherPeoplesPicturesGetDisplayAddresses(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.CreateAccount("alice", "alice"); err != nil {
		t.Fatal(err)
	}
	source := func(kind, address string) string {
		if strings.HasPrefix(address, "https://other.example/") {
			return "/media/remote/" + kind
		}
		return address
	}
	m := New(Options{Store: store, Pictures: source})
	t.Cleanup(func() { m.Close(); store.Close() })

	peer := Member{ID: "peer", Nick: "peer", DisplayName: "Peer", Status: "online", Role: "member", Color: "lavender",
		AvatarURL: "https://other.example/files/1/token/avatar", BannerURL: "https://other.example/files/2/token/banner", ProfileBackgroundURL: "https://this.example/files/3/token/background"}
	w := m.space("alice")
	w.mu.Lock()
	w.members[peer.ID] = peer
	w.emit("member.updated", peer)
	storageErr := w.storageErr
	w.mu.Unlock()
	if storageErr != nil {
		t.Fatal(storageErr)
	}

	check := func(where string, got Member) {
		t.Helper()
		if got.AvatarSrc != "/media/remote/avatar" || got.BannerSrc != "/media/remote/banner" || got.ProfileBackgroundSrc != "https://this.example/files/3/token/background" {
			t.Fatalf("%s: %+v", where, got)
		}
		if got.AvatarURL != peer.AvatarURL {
			t.Fatalf("%s changed the raw address: %q", where, got.AvatarURL)
		}
	}
	var found bool
	for _, member := range m.Snapshot("alice").Members {
		if member.ID == peer.ID {
			found = true
			check("snapshot", member)
		}
	}
	if !found {
		t.Fatal("the member is missing from the snapshot")
	}

	replay, _, valid, stop := m.Subscribe("alice", 0)
	defer stop()
	if !valid {
		t.Fatal("replay was refused")
	}
	var updates int
	for _, e := range replay {
		if e.Type != "member.updated" {
			continue
		}
		var member model.Member
		if err := json.Unmarshal(e.Data, &member); err != nil {
			t.Fatal(err)
		}
		if member.ID == peer.ID {
			updates++
			check("member.updated replay", member)
		}
	}
	if updates != 1 {
		t.Fatalf("%d member updates replayed, want 1", updates)
	}
}
