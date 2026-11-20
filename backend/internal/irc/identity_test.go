package irc

import (
	"context"
	"ircord/backend/internal/storage"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lrstanley/girc"
)

func TestNicknameValidationAcrossConnectionAndCommands(t *testing.T) {
	for _, nick := range []string{"Alice", "alice-123", "[alice]", "_alice", "`alice", strings.Repeat("a", 32)} {
		if err := validateNickname(nick, 32); err != nil {
			t.Fatal(nick, err)
		}
	}
	for _, nick := range []string{"", "Alice Smith", " Alice", "alice\n", "9alice", "-alice", "alice@example", "alice😀", "?alice", strings.Repeat("a", 33)} {
		if _, err := normalizeConfig(NetworkConfig{Nick: nick}); err == nil {
			t.Fatalf("connection accepted invalid nickname %q", nick)
		}
		if err := validateNickname(nick, 32); err == nil {
			t.Fatalf("identity endpoint accepted invalid nickname %q", nick)
		}
	}
	for _, line := range []string{"NICK Alice Smith", "NICK :Alice Smith", "NICK alice@example", "NICK 9alice"} {
		if _, err := rawEvent(line); err == nil {
			t.Fatalf("raw/slash command accepted invalid nickname: %s", line)
		}
	}
	if _, err := normalizeConfig(NetworkConfig{Nick: "Alice", AltNick: "Alice Smith"}); err == nil {
		t.Fatal("invalid alternate nickname accepted")
	}
}

func TestRealnameAndProfileNamePrecedence(t *testing.T) {
	member := Member{Nick: "alice", DisplayName: "alice", NameSource: "nick"}
	applyRealname(&member, "Alice Smith")
	if member.DisplayName != "Alice Smith" || member.NameSource != "realname" {
		t.Fatal(member)
	}
	applyRealname(&member, "Alice Jones")
	if member.DisplayName != "Alice Jones" {
		t.Fatal("realname update was lost", member)
	}
	member.Nick = "alice_new"
	resolveMemberName(&member)
	if member.DisplayName != "Alice Jones" {
		t.Fatal("nickname overwrote realname", member)
	}
	member.DisplayName = "Custom profile"
	member.NameSource = "profile"
	applyRealname(&member, "Another real name")
	if member.DisplayName != "Custom profile" {
		t.Fatal("realname overwrote profile", member)
	}
	member.NameSource = "nick"
	resolveMemberName(&member)
	if member.DisplayName != "Another real name" {
		t.Fatal("clearing profile lost realname fallback", member)
	}
	applyRealname(&member, "IRCord")
	if member.DisplayName != "alice_new" {
		t.Fatal("placeholder did not fall back to nickname", member)
	}
}

func profileName(name string) *string { return &name }

func TestProfileNameStartsUnsetAndIsOnlyExplicit(t *testing.T) {
	m := New(Options{AllowPrivate: true})
	defer m.Close()
	user := m.Snapshot("alice").User
	if user.DisplayName == "You" || user.NameSource == "profile" {
		t.Fatal("fresh account manufactured a profile name", user)
	}
	// Saving other fields without a name keeps it unset.
	if user, _ = m.SetProfile("alice", Profile{Bio: "about"}); user.NameSource == "profile" || user.Bio != "about" {
		t.Fatal("unrelated save set a profile name", user)
	}
	if user, _ = m.SetProfile("alice", Profile{DisplayName: profileName("  Alice  ")}); user.DisplayName != "Alice" || user.NameSource != "profile" {
		t.Fatal("explicit name not saved", user)
	}
	// Omitting the name keeps the explicit one.
	if user, _ = m.SetProfile("alice", Profile{Bio: "about"}); user.DisplayName != "Alice" || user.NameSource != "profile" {
		t.Fatal("omitted name cleared the profile", user)
	}
	if user, _ = m.SetProfile("alice", Profile{DisplayName: profileName("")}); user.NameSource == "profile" || user.DisplayName != user.Nick {
		t.Fatal("clearing did not restore the nickname fallback", user)
	}
	if _, err := m.SetProfile("alice", Profile{DisplayName: profileName("bad\nname")}); err == nil {
		t.Fatal("invalid name accepted")
	}
}

func TestStoredSelfNamesWithoutSource(t *testing.T) {
	for _, tc := range []struct{ in, want, source string }{
		{"You", "alice", "nick"},
		{"", "alice", "nick"},
		{"alice", "alice", "nick"},
		{"Alice Smith", "Alice Smith", "profile"},
	} {
		v := Member{Nick: "alice", DisplayName: tc.in}
		normalizeSelfName(&v)
		if v.DisplayName != tc.want || v.NameSource != tc.source {
			t.Fatal(tc.in, v)
		}
	}
	v := Member{Nick: "alice", DisplayName: "You", NameSource: "profile"}
	normalizeSelfName(&v)
	if v.DisplayName != "You" {
		t.Fatal("an explicit name with a source was rewritten at load", v)
	}
}

func TestPushProfileSharesOnlyExplicitNames(t *testing.T) {
	c := &connection{ctx: context.Background(), ready: true, caps: map[string]bool{"draft/metadata-3": true}}
	shared := func(p Member) string {
		c.pushProfileKeys(p, []string{"display-name"})
		return queuedProfile(c)["display-name"]
	}
	if value := shared(Member{Nick: "alice", DisplayName: "alice", NameSource: "nick"}); value != "" {
		t.Fatal("nickname fallback shared as a profile name", value)
	}
	if value := shared(Member{Nick: "alice", DisplayName: "Alice", NameSource: "profile"}); value != "Alice" {
		t.Fatal("explicit name not shared", value)
	}
}

func TestPeerClearsDisplayName(t *testing.T) {
	m := New(Options{AllowPrivate: true})
	defer m.Close()
	w := m.space("alice")
	c := &connection{owner: w, config: NetworkConfig{ID: "net"}, nicks: map[string]string{}}
	event := func(line string) girc.Event { return *girc.ParseEvent(line) }
	c.metadata(event(":irc.test METADATA bob display-name * :Bobby"))
	c.metadata(event(":irc.test METADATA bob display-name *"))
	found := false
	for _, v := range m.Snapshot("alice").Members {
		if v.Nick == "bob" {
			found = true
			if v.DisplayName != "bob" || v.NameSource != "nick" {
				t.Fatal("cleared peer profile name was kept", v)
			}
		}
	}
	if !found {
		t.Fatal("peer metadata was ignored")
	}
}

func TestLoadReleasesUnsourcedSelfPlaceholder(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.CreateAccount("alice", "alice-load-test"); err != nil {
		t.Fatal(err)
	}
	// A self profile written by an earlier version, after migrations already ran.
	if _, err = store.Commit("alice", "profile.updated", Member{ID: "alice", Nick: "alice", DisplayName: "You"}); err != nil {
		t.Fatal(err)
	}
	m := New(Options{Store: store})
	defer m.Close()
	if user := m.Snapshot("alice").User; user.DisplayName != "alice" || user.NameSource != "nick" {
		t.Fatal("stored placeholder was loaded as a name", user)
	}
}

func takeMetadataSync(t *testing.T, c *connection) *girc.Event {
	t.Helper()
	select {
	case events := <-c.queue:
		if len(events) != 1 || events[0].Command != "METADATA" || len(events[0].Params) != 2 || events[0].Params[1] != "SYNC" {
			t.Fatalf("expected one metadata sync, got %v", events)
		}
		c.mu.Lock()
		c.startMetadataSyncWriteLocked(events[0], time.Now())
		c.mu.Unlock()
		return events[0]
	case <-time.After(time.Second):
		t.Fatal("metadata sync not queued")
		return nil
	}
}

func TestMetadataSyncLaterRetriesARequestedTargetAFewTimes(t *testing.T) {
	c := &connection{ctx: context.Background(), ready: true, queue: make(chan []*girc.Event, 8), metadataSubscribed: true}
	c.requestMetadataSync("#room")
	if e := takeMetadataSync(t, c); e.Params[0] != "#room" {
		t.Fatal("sync not requested", e)
	}
	for range metadataRetries {
		before := time.Now()
		c.retryMetadataSync(*girc.ParseEvent(":irc.test 774 me #room 2 :Try again later"))
		c.requestMetadataSync("#ROOM")
		if len(c.metadataSyncQueue) != 1 || len(c.queue) != 0 || c.metadataSyncQueue[0].due.Before(before.Add(2*time.Second)) {
			t.Fatal("duplicate sync bypassed the server delay")
		}
		c.dispatchMetadataSyncLocked(c.metadataSyncQueue[0].due)
		if e := takeMetadataSync(t, c); e.Params[0] != "#room" {
			t.Fatal("postponed sync was not retried", e)
		}
	}
	c.retryMetadataSync(*girc.ParseEvent(":irc.test 774 me #room 2 :Try again later"))
	c.dispatchMetadataSyncLocked(time.Now().Add(time.Minute))
	if len(c.queue) != 0 || len(c.metadataRequested) != 0 {
		t.Fatal("postponed sync retried without limit")
	}
	// A target IRCord never asked about, such as Ergo's "*ALL" after SUB, is not requested.
	c.retryMetadataSync(*girc.ParseEvent(":irc.test 774 * *ALL 0 :Try again later"))
	if len(c.queue) != 0 {
		t.Fatal("unrequested target was synced")
	}
}

func TestMetadataSyncWaitsForSubscription(t *testing.T) {
	c := &connection{ctx: context.Background(), ready: true, queue: make(chan []*girc.Event, 8)}
	// Before SUB the server would reply with no keys, which would read as every value cleared.
	c.requestMetadataSync("#room")
	if len(c.queue) != 0 || len(c.metadataRequested) != 0 {
		t.Fatal("sync requested before subscribing")
	}
}

func TestMetadataSyncMissingReplyDoesNotBlockNextTarget(t *testing.T) {
	c := &connection{ctx: context.Background(), ready: true, queue: make(chan []*girc.Event, 8), metadataSubscribed: true}
	c.requestMetadataSync("#first", "#second")
	first := takeMetadataSync(t, c)
	if first.Params[0] != "#first" || len(c.queue) != 0 {
		t.Fatal("expected the first target to be the only active lookup")
	}
	c.dispatchMetadataSyncLocked(c.metadataSyncActive.deadline)
	if next := takeMetadataSync(t, c); next.Params[0] != "#second" || c.metadataRequested["#first"] != nil {
		t.Fatal("the unanswered lookup blocked the next target")
	}
}

func TestPeerJoinSyncsDeduplicateAndShareOneQueueSlot(t *testing.T) {
	c := &connection{ctx: context.Background(), ready: true, queue: make(chan []*girc.Event, 8), metadataSubscribed: true}
	c.queueJoinSync("#small", "alice")
	c.queueJoinSync("#other", "alice")
	c.queueJoinSync("#small", "bob")
	for i := range metadataJoinBurst + 1 {
		c.queueJoinSync("#busy", "user"+strconv.Itoa(i))
	}
	targets := map[string]bool{}
	for i := range 3 {
		target := takeMetadataSync(t, c).Params[0]
		targets[target] = true
		ref := strconv.Itoa(i)
		c.batch(girc.Event{Params: []string{"+" + ref, "metadata", target}})
		c.requestMetadataSync(strings.ToUpper(target))
		if len(c.queue) != 0 {
			t.Fatal("sent another lookup before the batch ended")
		}
		if err := c.enqueue(&girc.Event{Command: "PRIVMSG", Params: []string{"#small", "hello"}}); err != nil {
			t.Fatal(err)
		}
		c.batch(girc.Event{Params: []string{"-" + ref}})
		if events := <-c.queue; len(events) != 1 || events[0].Command != "PRIVMSG" {
			t.Fatalf("chat did not keep its queue position: %v", events)
		}
	}
	if len(c.queue) != 0 || len(c.metadataRequested) != 0 || !targets["alice"] || !targets["bob"] || !targets["#busy"] {
		t.Fatal("joins did not coalesce into unique user and channel syncs", targets)
	}
}
