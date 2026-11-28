package irc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"ircord/backend/internal/model"
	"ircord/backend/internal/storage"
)

// Two IRCord backends, each with its own database, attached to one disposable
// Ergo. Payloads are random bytes: the backend never reads them.

func e2eeNetworkErgo(t *testing.T) (string, int) {
	t.Helper()
	if os.Getenv("IRCORD_E2EE_INTEGRATION") != "1" {
		t.Skip("set IRCORD_E2EE_INTEGRATION=1 for the two-backend encryption fixture (needs Docker)")
	}
	// Restore the timings after every backend of the test has stopped.
	settle, burst, rate, wait, join, outbox, elsewhere := e2eeSettle, handshakeBurst, handshakeRate, e2eeRequestWait, e2eeJoinDelay, e2eeOutboxInterval, e2eeElsewhereWait
	t.Cleanup(func() {
		e2eeSettle, handshakeBurst, handshakeRate, e2eeRequestWait, e2eeJoinDelay, e2eeOutboxInterval, e2eeElsewhereWait = settle, burst, rate, wait, join, outbox, elsewhere
	})
	// The fixture switches off Ergo's rate limit, as the backend's other
	// fixtures do; pacing is tested on its own.
	e2eeSettle, handshakeBurst, handshakeRate = 400*time.Millisecond, 1000, 1000
	e2eeRequestWait, e2eeJoinDelay, e2eeOutboxInterval, e2eeElsewhereWait = 2*time.Second, 200*time.Millisecond, 200*time.Millisecond, 2*time.Second
	host, port, _ := isolatedErgo(t, nil)
	return host, port
}

type e2eeBackend struct {
	m     *Manager
	store *storage.Store
	dir   string
}

// startBackend opens a backend on dir. Closing it keeps dir, so the same
// backend can start again.
func startBackend(t *testing.T, host string, port int, dir string, accounts ...string) *e2eeBackend {
	t.Helper()
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		if _, _, err := store.AccountByUsername(account); err == nil {
			continue
		}
		if _, err = store.CreateUser(account, account, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	b := &e2eeBackend{m: New(Options{Store: store, AllowPrivate: true, ReconnectMin: time.Hour, E2EE: true, Home: NetworkConfig{ID: "home", Host: host, Port: port}}), store: store, dir: dir}
	t.Cleanup(b.stop)
	return b
}

func (b *e2eeBackend) stop() {
	b.m.Close()
	b.store.Close()
}

func (b *e2eeBackend) connect(t *testing.T, host string, port int, account, password string) {
	t.Helper()
	e2eeErgoConnect(t, b.m, account, NetworkConfig{ID: "home", Name: "home", Host: host, Port: port, Nick: account, SASLUser: account, SASLPassword: password})
}

// setUp gives the account an identity and one device with key packages, as a
// client would publish them.
func (b *e2eeBackend) setUp(t *testing.T, account string, packages int) string {
	t.Helper()
	if _, err := b.store.PublishE2EEIdentity(account, random(t, 32), false); err != nil {
		t.Fatal(err)
	}
	device := "d" + id()[:8]
	if err := b.store.PutE2EEDevice(account, device, 1, random(t, 300), random(t, 430), "test"); err != nil {
		t.Fatal(err)
	}
	if packages > 0 {
		list := make([][]byte, packages)
		for i := range list {
			list[i] = random(t, 430)
		}
		if _, err := b.store.AddE2EEKeyPackages(account, device, list); err != nil {
			t.Fatal(err)
		}
	}
	return device
}

func random(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func ergoAccounts(t *testing.T, host string, port int, names ...string) string {
	t.Helper()
	password := id()
	for _, name := range names {
		registerErgoAccount(t, host, port, name, password)
	}
	return password
}

// The gate of the in-band transport: two commits for one epoch, sent at the
// same instant from two backends, settle on the same winner on both, every
// round. Run with -race.
func TestErgoE2EECommitRace(t *testing.T) {
	host, port := e2eeNetworkErgo(t)
	suffix := id()[:8]
	alice, bob := "ca"+suffix, "cb"+suffix
	password := ergoAccounts(t, host, port, alice, bob)
	a := startBackend(t, host, port, t.TempDir(), alice)
	b := startBackend(t, host, port, t.TempDir(), bob)
	a.connect(t, host, port, alice, password)
	b.connect(t, host, port, bob, password)
	dmA, err := a.m.Direct(alice, "home", bob)
	if err != nil {
		t.Fatal(err)
	}
	dmB, err := b.m.Direct(bob, "home", alice)
	if err != nil {
		t.Fatal(err)
	}
	rounds := 200
	if n, err := strconv.Atoi(os.Getenv("IRCORD_E2EE_RACE_ROUNDS")); err == nil && n > 0 {
		rounds = n
	}
	type outcome struct {
		result E2EECommitResult
		err    error
	}
	slots := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for round := 0; round < rounds; round++ {
		group := newTransfer()
		if err = a.store.PutE2EEChat(alice, storage.E2EEChat{Group: group, Kind: storage.E2EEChatDirect, State: storage.E2EEChatJoined, Conversation: dmA.ID, Target: bob, Peer: bob, Epoch: 1}); err != nil {
			t.Fatal(err)
		}
		if err = b.store.PutE2EEChat(bob, storage.E2EEChat{Group: group, Kind: storage.E2EEChatDirect, State: storage.E2EEChatJoined, Conversation: dmB.ID, Target: alice, Peer: alice, Epoch: 1}); err != nil {
			t.Fatal(err)
		}
		slots <- struct{}{}
		wg.Add(1)
		go func(round int, group string) {
			defer wg.Done()
			defer func() { <-slots }()
			start := make(chan struct{})
			var fromA, fromB outcome
			var both sync.WaitGroup
			both.Add(2)
			post := func(m *Manager, owner string, into *outcome) {
				defer both.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				into.result, into.err = m.E2EEPostCommit(ctx, owner, group, E2EECommit{Epoch: 1, Commit: []byte("commit from " + owner)})
			}
			go post(a.m, alice, &fromA)
			go post(b.m, bob, &fromB)
			close(start)
			both.Wait()
			if fromA.err != nil || fromB.err != nil {
				t.Errorf("round %d: %v / %v", round, fromA.err, fromB.err)
				return
			}
			if fromA.result.Accepted == fromB.result.Accepted {
				t.Errorf("round %d: accepted on both or neither: %+v / %+v", round, fromA.result, fromB.result)
			}
			if fromA.result.Winner != fromB.result.Winner {
				t.Errorf("round %d: winners differ: %+v / %+v", round, fromA.result.Winner, fromB.result.Winner)
			}
			logA, errA := a.store.E2EEObservedCommits(alice, group, 1, 10)
			logB, errB := b.store.E2EEObservedCommits(bob, group, 1, 10)
			if errA != nil || errB != nil || len(logA) != 2 || len(logB) != 2 {
				t.Errorf("round %d: logs %d / %d (%v %v)", round, len(logA), len(logB), errA, errB)
				return
			}
			for i := range logA {
				if logA[i].At != logB[i].At || logA[i].MsgID != logB[i].MsgID || !bytes.Equal(logA[i].Commit, logB[i].Commit) || logA[i].Sender != logB[i].Sender {
					t.Errorf("round %d: candidate %d differs: %+v / %+v", round, i, logA[i], logB[i])
				}
			}
			if positionOf(logA[0].At, logA[0].MsgID) != fromA.result.Winner {
				t.Errorf("round %d: the winner is not the first candidate", round)
			}
		}(round, group)
	}
	wg.Wait()
}

// A directory request reaches the backend that holds the account, which
// answers with its devices and one key package each; beyond the rate limit
// only last-resort packages come back. An account nobody answers for is
// reported as not set up, and an account set up on another backend is
// recognized there.
func TestErgoE2EEDirectoryAcrossServers(t *testing.T) {
	host, port := e2eeNetworkErgo(t)
	suffix := id()[:8]
	alice, bob, nobody := "da"+suffix, "db"+suffix, "dn"+suffix
	password := ergoAccounts(t, host, port, alice, bob, nobody)
	a := startBackend(t, host, port, t.TempDir(), alice)
	b := startBackend(t, host, port, t.TempDir(), bob)
	a.connect(t, host, port, alice, password)
	b.connect(t, host, port, bob, password)
	device := b.setUp(t, bob, 20)
	ctx := context.Background()

	reply, err := a.m.E2EERemoteDirectory(ctx, alice, bob, false)
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := b.store.E2EEIdentity(bob)
	if reply.Account != bob || !bytes.Equal(reply.IdentityKey, identity.IdentityKey) || len(reply.Devices) != 1 || reply.Devices[0].ID != device || len(reply.Devices[0].KeyPackage) != 0 {
		t.Fatalf("directory %+v", reply)
	}
	for i := 0; i < e2eeDirectoryRate-1; i++ {
		claim, err := a.m.E2EERemoteDirectory(ctx, alice, bob, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(claim.Devices) != 1 || len(claim.Devices[0].KeyPackage) == 0 || claim.Devices[0].LastResort {
			t.Fatalf("claim %d: %+v", i, claim.Devices)
		}
	}
	limited, err := a.m.E2EERemoteDirectory(ctx, alice, bob, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited.Devices) != 1 || !limited.Devices[0].LastResort {
		t.Fatalf("over the limit: %+v", limited.Devices)
	}
	devices, _ := b.store.E2EEDevices(bob)
	if devices[0].KeyPackages != 20-(e2eeDirectoryRate-1) {
		t.Fatalf("stock %d", devices[0].KeyPackages)
	}

	// nobody is online but has no IRCord server with encryption.
	c := startBackend(t, host, port, t.TempDir(), nobody)
	c.connect(t, host, port, nobody, password)
	if _, err = a.m.E2EERemoteDirectory(ctx, alice, nobody, false); !errors.Is(err, ErrE2EENotSetUp) {
		t.Fatalf("no answer: %v", err)
	}

	// A second backend for bob, without keys, finds them on the first.
	second := startBackend(t, host, port, t.TempDir(), bob)
	second.connect(t, host, port, bob, password)
	if elsewhere, err := second.m.E2EEElsewhere(ctx, bob); err != nil || !elsewhere {
		t.Fatalf("second backend: elsewhere %v %v", elsewhere, err)
	}
	if elsewhere, err := a.m.E2EEElsewhere(ctx, alice); err != nil || elsewhere {
		t.Fatalf("alice: elsewhere %v %v", elsewhere, err)
	}
	// The second backend stays silent when someone asks for bob: only the
	// first answers, so the answer is still bob's real directory.
	a.m.net.forget(bob)
	again, err := a.m.E2EERemoteDirectory(ctx, alice, bob, false)
	if err != nil || !bytes.Equal(again.IdentityKey, identity.IdentityKey) {
		t.Fatalf("with two sessions: %+v %v", again, err)
	}
}

// A 60 KB commit crosses in 22 chunks and arrives whole.
func TestErgoE2EELargeObject(t *testing.T) {
	host, port := e2eeNetworkErgo(t)
	suffix := id()[:8]
	alice, bob := "la"+suffix, "lb"+suffix
	password := ergoAccounts(t, host, port, alice, bob)
	a := startBackend(t, host, port, t.TempDir(), alice)
	b := startBackend(t, host, port, t.TempDir(), bob)
	a.connect(t, host, port, alice, password)
	b.connect(t, host, port, bob, password)
	group := newTransfer()
	dm, _ := a.m.Direct(alice, "home", bob)
	_ = a.store.PutE2EEChat(alice, storage.E2EEChat{Group: group, Kind: storage.E2EEChatDirect, State: storage.E2EEChatJoined, Conversation: dm.ID, Target: bob, Peer: bob, Epoch: 1})
	_ = b.store.PutE2EEChat(bob, storage.E2EEChat{Group: group, Kind: storage.E2EEChatDirect, State: storage.E2EEChatJoined, Target: alice, Peer: alice, Epoch: 1})
	commit := random(t, 60<<10)
	if lines, _ := chunkObject(kindCommit, group, commitObject{Group: group, Entries: []commitEntry{{Epoch: 1, Commit: commit}}}.encode()); len(lines) != 22 {
		t.Fatalf("%d chunks", len(lines))
	}
	result, err := a.m.E2EEPostCommit(context.Background(), alice, group, E2EECommit{Epoch: 1, Commit: commit})
	if err != nil || !result.Accepted {
		t.Fatalf("%+v %v", result, err)
	}
	eventually(t, func() bool {
		log, _ := b.store.E2EEObservedCommits(bob, group, 1, 10)
		return len(log) == 1 && bytes.Equal(log[0].Commit, commit) && log[0].Sender == alice
	})
}

// A Welcome for an account whose backend is down goes out when the account
// comes online, and its receipt ends the retries.
func TestErgoE2EEWelcomeWaitsForBackend(t *testing.T) {
	host, port := e2eeNetworkErgo(t)
	suffix := id()[:8]
	alice, bob := "wa"+suffix, "wb"+suffix
	password := ergoAccounts(t, host, port, alice, bob)
	a := startBackend(t, host, port, t.TempDir(), alice)
	a.connect(t, host, port, alice, password)
	a.setUp(t, alice, 0)
	dm, err := a.m.Direct(alice, "home", bob)
	if err != nil {
		t.Fatal(err)
	}
	group := newTransfer()
	welcome := random(t, 5000)
	if _, _, err = a.m.E2EECreateChat(context.Background(), alice, E2EECreate{Group: group, Kind: storage.E2EEChatDirect, Conversation: dm.ID, Peer: bob, Epoch: 1, Welcome: welcome}); err != nil {
		t.Fatal(err)
	}
	// The first attempt finds bob offline.
	eventually(t, func() bool {
		items, _ := a.store.E2EEOutboxFor(alice, group, bob)
		return len(items) == 1 && items[0].Attempts > 0
	})
	time.Sleep(500 * time.Millisecond)

	b := startBackend(t, host, port, t.TempDir(), bob)
	device := b.setUp(t, bob, 0)
	b.connect(t, host, port, bob, password)
	eventually(t, func() bool {
		welcomes, _ := b.store.E2EEChatWelcomes(bob, device)
		return len(welcomes) == 1 && bytes.Equal(welcomes[0].Welcome, welcome) && welcomes[0].Sender == alice && welcomes[0].Kind == storage.E2EEChatDirect
	})
	eventually(t, func() bool {
		states, _ := a.store.E2EEOutboxStates(alice, group)
		return states[bob] == storage.E2EEOutboxDelivered
	})
	chat, err := b.store.E2EEChat(bob, group)
	if err != nil || chat.State != storage.E2EEChatInvited || chat.Inviter != alice || chat.Target != alice {
		t.Fatalf("bob's chat %+v %v", chat, err)
	}
	joined, err := b.m.E2EEJoinChat(context.Background(), bob, group)
	if err != nil || joined.State != storage.E2EEChatJoined || joined.Conversation == "" {
		t.Fatalf("join %+v %v", joined, err)
	}
	// From now on bob's side refuses plaintext into the conversation.
	if err = b.m.Send(bob, SendInput{ClientID: id(), ConversationID: joined.Conversation, Text: "plain"}); !errors.Is(err, ErrPlaintextRefused) {
		t.Fatalf("plaintext into the joined chat: %v", err)
	}
}

// A group lives in a registered, invite-only, secret channel. A member whose
// backend was down catches up on the commits it missed by asking another
// member, and re-enters the channel after the restart; once removed it cannot.
func TestErgoE2EEGroupChannel(t *testing.T) {
	host, port := e2eeNetworkErgo(t)
	suffix := id()[:8]
	alice, bob := "ga"+suffix, "gb"+suffix
	password := ergoAccounts(t, host, port, alice, bob)
	a := startBackend(t, host, port, t.TempDir(), alice)
	b := startBackend(t, host, port, t.TempDir(), bob)
	a.connect(t, host, port, alice, password)
	b.connect(t, host, port, bob, password)
	a.setUp(t, alice, 0)
	device := b.setUp(t, bob, 0)
	ctx := context.Background()

	group := newTransfer()
	chat, warnings, err := a.m.E2EECreateChat(ctx, alice, E2EECreate{Group: group, Kind: storage.E2EEChatGroup, Members: []string{alice, bob}, Epoch: 1, Welcome: random(t, 3000)})
	if err != nil {
		t.Fatal(err, warnings)
	}
	if len(warnings) != 0 || chat.Target != groupChannel(group) {
		t.Fatalf("created %+v %v", chat, warnings)
	}
	registration, err := a.m.ChannelRegistration(ctx, alice, chat.Conversation)
	if err != nil || !registration.Registered || registration.Founder != alice {
		t.Fatalf("registration %+v %v", registration, err)
	}
	access, err := a.m.ChannelAccess(ctx, alice, chat.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	voiced := false
	for _, entry := range access {
		voiced = voiced || entry.Account == bob && entry.Mode == "v"
	}
	if !voiced {
		t.Fatalf("access %+v", access)
	}

	eventually(t, func() bool {
		welcomes, _ := b.store.E2EEChatWelcomes(bob, device)
		return len(welcomes) == 1 && welcomes[0].Target == chat.Target && welcomes[0].Kind == storage.E2EEChatGroup
	})
	joined, err := b.m.E2EEJoinChat(ctx, bob, group)
	if err != nil {
		t.Fatal(err)
	}
	inChannel := func(backend *e2eeBackend, owner, conversation string) bool {
		for _, conv := range backend.m.Snapshot(owner).Conversations {
			if conv.ID == conversation {
				return conv.Joined
			}
		}
		return false
	}
	eventually(t, func() bool { return inChannel(b, bob, joined.Conversation) })

	post := func(epoch int64, in E2EECommit) E2EECommitResult {
		t.Helper()
		in.Epoch = epoch
		if in.Commit == nil {
			in.Commit = random(t, 900)
		}
		result, err := a.m.E2EEPostCommit(ctx, alice, group, in)
		if err != nil || !result.Accepted {
			t.Fatalf("epoch %d: %+v %v", epoch, result, err)
		}
		return result
	}
	// A plain IRC client in the channel, let in by an operator, sees the
	// placeholder of encrypted messages and no handshake line at all.
	dave := "gd" + suffix
	var plainMu sync.Mutex
	var plainTags, plainBodies []string
	plain, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { plain.Close() })
	plainConnected, plainJoined := make(chan struct{}, 1), make(chan struct{}, 1)
	go func() {
		lines := bufio.NewScanner(plain)
		lines.Buffer(make([]byte, 16<<10), 16<<10)
		for lines.Scan() {
			e := girc.ParseEvent(lines.Text())
			if e == nil {
				continue
			}
			switch e.Command {
			case "PING":
				fmt.Fprintf(plain, "PONG :%s\r\n", e.Last())
			case "001":
				plainConnected <- struct{}{}
			case "INVITE":
				fmt.Fprintf(plain, "JOIN %s\r\n", chat.Target)
			case "JOIN":
				if e.Source != nil && e.Source.Name == dave {
					plainJoined <- struct{}{}
				}
			case "TAGMSG", "PRIVMSG":
				plainMu.Lock()
				if e.Command == "TAGMSG" {
					plainTags = append(plainTags, e.String())
				} else {
					plainBodies = append(plainBodies, e.Last())
				}
				plainMu.Unlock()
			}
		}
	}()
	// No CAP negotiation: this client has no tags at all.
	fmt.Fprintf(plain, "NICK %s\r\nUSER plain 0 * :plain\r\n", dave)
	select {
	case <-plainConnected:
	case <-time.After(10 * time.Second):
		t.Fatal("the plain client did not connect")
	}
	if err = a.m.Raw(alice, "home", "INVITE "+dave+" "+chat.Target); err != nil {
		t.Fatal(err)
	}
	select {
	case <-plainJoined:
	case <-time.After(10 * time.Second):
		t.Fatal("the plain client did not join")
	}

	post(1, E2EECommit{})
	eventually(t, func() bool { log, _ := b.store.E2EEObservedCommits(bob, group, 1, 10); return len(log) == 1 })

	// Encrypted messages go into the group's channel; plaintext is refused.
	if err = a.m.Send(alice, SendInput{ClientID: id(), ConversationID: chat.Conversation, Text: "plain"}); !errors.Is(err, ErrPlaintextRefused) {
		t.Fatalf("plaintext: %v", err)
	}
	if err = a.m.Send(alice, SendInput{ClientID: id(), ConversationID: chat.Conversation, Encrypted: &model.Encrypted{Payload: "c2VjcmV0"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		for _, msg := range page(b.m, bob, joined.Conversation) {
			if msg.Encrypted != nil && msg.Encrypted.Payload == "c2VjcmV0" {
				return true
			}
		}
		return false
	})
	eventually(t, func() bool {
		plainMu.Lock()
		defer plainMu.Unlock()
		return len(plainBodies) == 1 && plainBodies[0] == model.EncryptedPlaceholder
	})
	plainMu.Lock()
	if len(plainTags) != 0 {
		t.Fatalf("the plain client received handshake lines: %v", plainTags)
	}
	plainMu.Unlock()

	// bob's backend stops; three commits happen without it.
	b.stop()
	for epoch := int64(2); epoch <= 4; epoch++ {
		post(epoch, E2EECommit{})
	}
	b = startBackend(t, host, port, b.dir)
	if err = b.m.Restore(); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return b.m.Snapshot(bob).Networks[0].Features.Encryption })
	eventually(t, func() bool { log, _ := b.store.E2EEObservedCommits(bob, group, 1, 10); return len(log) == 4 })
	logA, _ := a.store.E2EEObservedCommits(alice, group, 1, 10)
	logB, _ := b.store.E2EEObservedCommits(bob, group, 1, 10)
	for i := range logA {
		if logA[i].Epoch != logB[i].Epoch || logA[i].At != logB[i].At || logA[i].MsgID != logB[i].MsgID || !bytes.Equal(logA[i].Commit, logB[i].Commit) {
			t.Fatalf("catch-up %d: %+v / %+v", i, logA[i], logB[i])
		}
	}
	eventually(t, func() bool { return inChannel(b, bob, joined.Conversation) })

	// No handshake line became a message, an unread count or an inbox item.
	for _, msg := range page(a.m, alice, chat.Conversation) {
		if msg.Encrypted == nil {
			t.Fatalf("a handshake line reached history: %+v", msg)
		}
	}

	// alice removes bob: he is kicked and cannot come back.
	post(5, E2EECommit{Removed: []string{bob}})
	eventually(t, func() bool { return !inChannel(b, bob, joined.Conversation) })
	if _, err = b.m.Join(bob, "home", chat.Target); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if inChannel(b, bob, joined.Conversation) {
		t.Fatal("a removed member re-entered the group's channel")
	}
}
