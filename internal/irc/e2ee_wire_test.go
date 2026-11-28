package irc

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/lrstanley/girc"
)

func TestHandshakeLineRoundTrip(t *testing.T) {
	transfer := newTransfer()
	if !validTransfer(transfer) {
		t.Fatalf("transfer %q", transfer)
	}
	data := make([]byte, handshakeChunkBytes)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	line := handshakeLine{kind: kindCommit, transfer: transfer, index: 31, count: 32, data: data}
	tag := line.tag()
	if len(tag) > handshakeTagBudget {
		t.Fatalf("a full line is %d characters, over %d", len(tag), handshakeTagBudget)
	}
	parsed, ok := parseHandshakeLine(tag)
	if !ok || parsed.kind != line.kind || parsed.transfer != transfer || parsed.index != 31 || parsed.count != 32 || !bytes.Equal(parsed.data, data) {
		t.Fatalf("parsed %+v %v", parsed, ok)
	}
	// Phase 1 parsers never take a handshake line for a message.
	if parseEncryptedTag(tag) != nil {
		t.Fatal("a handshake line parsed as an encrypted message")
	}
	for _, bad := range []string{
		"1.m.payload",
		"t.probe",
		"h.x." + transfer + ".0.1.AAAA",
		"h.c.short.0.1.AAAA",
		"h.c." + transfer + ".1.1.AAAA",
		"h.c." + transfer + ".0.33.AAAA",
		"h.c." + transfer + ".00.1.AAAA",
		"h.c." + transfer + ".0.1.",
		"h.c." + transfer + ".0.1.AA=A",
		"h.c." + transfer + ".0.1",
	} {
		if _, ok := parseHandshakeLine(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestChunkObject(t *testing.T) {
	object := make([]byte, 60<<10)
	lines, err := chunkObject(kindWelcome, newTransfer(), object)
	if err != nil || len(lines) != 22 {
		t.Fatalf("%d lines, %v", len(lines), err)
	}
	total := 0
	for i, line := range lines {
		if line.index != i || line.count != 22 {
			t.Fatalf("line %d: %+v", i, line)
		}
		total += len(line.data)
	}
	if total != len(object) {
		t.Fatalf("%d bytes", total)
	}
	if _, err = chunkObject(kindWelcome, newTransfer(), make([]byte, handshakeMaxObject+1)); err == nil {
		t.Fatal("an object over the limit was cut")
	}
	if _, err = chunkObject(kindWelcome, newTransfer(), nil); err == nil {
		t.Fatal("an empty object was cut")
	}
}

func TestHandshakeObjects(t *testing.T) {
	group := newTransfer()
	dir := E2EEDirectory{Account: "alice", IdentityKey: []byte("key"), Generation: 2, DeviceList: []byte("list"), ListCounter: 7, Devices: []E2EEDirectoryDevice{{ID: "dev1", Certificate: []byte("cert"), KeyPackage: []byte("kp"), LastResort: true}, {ID: "dev2", Certificate: []byte("c2")}}}
	if got, ok := decodeDirectory(dir.encode()); !ok || got.Account != "alice" || got.Generation != 2 || got.ListCounter != 7 || len(got.Devices) != 2 || !got.Devices[0].LastResort || string(got.Devices[1].Certificate) != "c2" {
		t.Fatalf("directory %+v %v", got, ok)
	}
	if got, ok := decodeDirectoryRequest(directoryRequest{Account: "bob", Claim: true}.encode()); !ok || got.Account != "bob" || !got.Claim {
		t.Fatalf("request %+v %v", got, ok)
	}
	commit := commitObject{Group: group, Entries: []commitEntry{{Epoch: 3, Commit: []byte("c"), At: 42, MsgID: "m", Sender: "alice"}}, More: true}
	if got, ok := decodeCommit(commit.encode()); !ok || got.Group != group || len(got.Entries) != 1 || got.Entries[0].At != 42 || !got.More {
		t.Fatalf("commit %+v %v", got, ok)
	}
	welcome := welcomeObject{Group: group, Epoch: 1, Kind: wireGroup, Channel: groupChannel(group), Welcome: []byte("w")}
	if got, ok := decodeWelcome(welcome.encode()); !ok || got.Channel != groupChannel(group) || got.Kind != wireGroup {
		t.Fatalf("welcome %+v %v", got, ok)
	}
	// A group Welcome must name its own group's channel, and a direct one none.
	if _, ok := decodeWelcome(welcomeObject{Group: group, Epoch: 1, Kind: wireGroup, Channel: "#elsewhere", Welcome: []byte("w")}.encode()); ok {
		t.Fatal("a Welcome naming another channel was accepted")
	}
	if _, ok := decodeWelcome(welcomeObject{Group: group, Epoch: 1, Kind: wireDirect, Channel: groupChannel(group), Welcome: []byte("w")}.encode()); ok {
		t.Fatal("a direct Welcome naming a channel was accepted")
	}
	if got, ok := decodeReceipt(receiptObject{Group: group, Declined: true}.encode()); !ok || !got.Declined {
		t.Fatalf("receipt %+v %v", got, ok)
	}
	if got, ok := decodeLogRequest(logRequest{Group: group, From: 9}.encode()); !ok || got.From != 9 {
		t.Fatalf("log %+v %v", got, ok)
	}
	if got, ok := decodeRejoin(rejoinObject{Group: group, Device: "dev", Certificate: []byte("c"), KeyPackage: []byte("k")}.encode()); !ok || got.Device != "dev" {
		t.Fatalf("rejoin %+v %v", got, ok)
	}
	if got, ok := decodeNotice(noticeObject{Reason: noticeIdentity}.encode()); !ok || got.Reason != noticeIdentity {
		t.Fatalf("notice %+v %v", got, ok)
	}
	// The wrong kind, a newer version, trailing bytes and truncation are refused.
	if _, ok := decodeCommit(receiptObject{Group: group}.encode()); ok {
		t.Fatal("a receipt decoded as a commit")
	}
	newer := receiptObject{Group: group}.encode()
	newer[0] = handshakeVersion + 1
	if _, ok := decodeReceipt(newer); ok {
		t.Fatal("a newer version decoded")
	}
	if _, ok := decodeReceipt(append(receiptObject{Group: group}.encode(), 0)); ok {
		t.Fatal("trailing bytes decoded")
	}
	encoded := dir.encode()
	for n := 0; n < len(encoded); n++ {
		if _, ok := decodeDirectory(encoded[:n]); ok {
			t.Fatalf("a directory cut at %d bytes decoded", n)
		}
	}
}

func TestGroupChannel(t *testing.T) {
	group := "AAECAwQFBgcICQoLDA0ODw"
	if channel := groupChannel(group); channel != "#e2ee-000102030405060708090a0b0c0d0e0f" || !isGroupChannel(channel) {
		t.Fatalf("channel %q", channel)
	}
	for _, bad := range []string{"short", newTransfer() + "A", "AAECAwQFBgcICQoLDA0ODx"} {
		if groupChannel(bad) != "" {
			t.Errorf("%q has a channel", bad)
		}
	}
	if isGroupChannel("#e2ee-xyz") || isGroupChannel("#general") {
		t.Fatal("not a group channel")
	}
	if !encryptedGroupConversation(Conversation{Kind: "channel", Name: "e2ee-000102030405060708090a0b0c0d0e0f"}) {
		t.Fatal("a group's channel is not treated as encrypted")
	}
}

func handshakeEvent0(account, nick, target string, line handshakeLine, at time.Time, msgid string) girc.Event {
	tags := girc.Tags{e2eeTag: line.tag(), "time": at.UTC().Format("2006-01-02T15:04:05.000Z"), "msgid": msgid}
	if account != "" {
		tags["account"] = account
	}
	return girc.Event{Command: "TAGMSG", Source: &girc.Source{Name: nick}, Params: []string{target}, Tags: tags}
}

// Chunks come back together whole, only from one account, and a transfer
// missing a chunk is dropped after a minute.
func TestHandshakeReassembly(t *testing.T) {
	c := &connection{config: NetworkConfig{Nick: "me"}}
	object := make([]byte, 3*handshakeChunkBytes-10)
	if _, err := rand.Read(object); err != nil {
		t.Fatal(err)
	}
	transfer := newTransfer()
	lines, _ := chunkObject(kindCommit, transfer, object)
	now := time.Now()
	take := func(e girc.Event) (handshakeEvent, bool) {
		ev, ok, finish := c.takeHandshake(e, now)
		finish()
		return ev, ok
	}
	// Lines without an account tag never count.
	for _, line := range lines {
		if _, ok := take(handshakeEvent0("", "alice", "#g", line, now, "x"+line.tag()[:4])); ok {
			t.Fatal("an object without account tags completed")
		}
	}
	// Two accounts sending under one transfer never combine.
	if _, ok := take(handshakeEvent0("alice", "alice", "#g", lines[0], now, "a0")); ok {
		t.Fatal("completed early")
	}
	if _, ok := take(handshakeEvent0("mallory", "mallory", "#g", lines[1], now, "m1")); ok {
		t.Fatal("chunks of two accounts combined")
	}
	if _, ok := take(handshakeEvent0("mallory", "mallory", "#g", lines[2], now, "m2")); ok {
		t.Fatal("chunks of two accounts combined")
	}
	if _, ok := take(handshakeEvent0("alice", "alice", "#g", lines[1], now, "a1")); ok {
		t.Fatal("completed early")
	}
	ev, ok := take(handshakeEvent0("alice", "alice", "#g", lines[2], now.Add(time.Millisecond), "a2"))
	if !ok || !bytes.Equal(ev.object, object) || ev.account != "alice" || ev.msgid != "a2" || ev.at != now.Add(time.Millisecond).Truncate(time.Millisecond).UnixNano() || ev.target != "#g" || ev.ours {
		t.Fatalf("assembled %+v %v", ev, ok)
	}
	// The same transfer again is not taken twice.
	for _, line := range lines {
		if _, ok := take(handshakeEvent0("alice", "alice", "#g", line, now, "again")); ok {
			t.Fatal("a completed transfer was taken again")
		}
	}

	// A transfer missing a chunk is dropped once it is a minute old.
	other := newTransfer()
	parts, _ := chunkObject(kindCommit, other, object)
	take(handshakeEvent0("alice", "alice", "#g", parts[0], now, "p0"))
	take(handshakeEvent0("alice", "alice", "#g", parts[1], now, "p1"))
	c.handshake.mu.Lock()
	for _, p := range c.handshake.partial {
		p.started = now.Add(-2 * handshakeIncomplete)
	}
	c.handshake.mu.Unlock()
	now = now.Add(time.Second)
	if _, ok := take(handshakeEvent0("alice", "alice", "#g", parts[2], now, "p2")); ok {
		t.Fatal("a transfer completed after its earlier chunks expired")
	}
	if !strings.HasPrefix(lines[0].tag(), handshakePrefix) {
		t.Fatal("prefix")
	}
}

// The echo of a tracked transfer releases its sender with the position of the
// last chunk, and is marked as ours.
func TestHandshakeEcho(t *testing.T) {
	c := &connection{config: NetworkConfig{Nick: "me"}}
	transfer := newTransfer()
	object := []byte("commit")
	lines, _ := chunkObject(kindCommit, transfer, object)
	c.handshake.sent = map[string]*outgoingTransfer{string(kindCommit) + transfer: {created: time.Now(), count: 1, echoed: make([]bool, 1), done: make(chan struct{})}}
	out := c.handshake.sent[string(kindCommit)+transfer]
	at := time.Now()
	ev, ok, finish := c.takeHandshake(handshakeEvent0("me", "me", "bob", lines[0], at, "echo"), at)
	if !ok || !ev.ours {
		t.Fatalf("echo %+v %v", ev, ok)
	}
	select {
	case <-out.done:
		t.Fatal("released before the object was handled")
	default:
	}
	finish()
	select {
	case <-out.done:
	default:
		t.Fatal("not released")
	}
	if out.msgid != "echo" || out.at != ev.at {
		t.Fatalf("position %+v", out)
	}
}
