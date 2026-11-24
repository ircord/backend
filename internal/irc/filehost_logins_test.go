package irc

import (
	"errors"
	"testing"
)

// The logins a deletion needs survive dropping the owner, and a retry gets the same ones.
func TestFilehostLoginsOutliveTheOwnersWorkspace(t *testing.T) {
	m := New(Options{})
	defer m.Close()
	w := m.space("owner")
	w.configs["net"] = NetworkConfig{ID: "net", Host: "irc.example.org", SASLUser: "alice", SASLPassword: "secret"}
	logins := m.FilehostLogins("owner")
	if err := m.DropOwner("owner"); err != nil {
		t.Fatal(err)
	}
	// The workspace is gone; asking again must not read it.
	for attempt := 0; attempt < 2; attempt++ {
		user, password, err := m.FilehostLogins("owner")("owner", "net")
		if err != nil || user != "alice" || password != "secret" {
			t.Fatalf("attempt %d: %q %q %v", attempt, user, password, err)
		}
	}
	if _, _, err := logins("owner", "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a network the owner never had: %v", err)
	}
	m.ForgetFilehostLogins("owner")
	if _, _, err := m.FilehostLogins("owner")("owner", "net"); err == nil {
		t.Fatal("logins kept after they were forgotten")
	}
}
