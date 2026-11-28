package irc

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lrstanley/girc"
)

// The handshake transport of one connection: it paces outgoing handshake lines
// behind the person's own messages, recognizes the echoes of what it sent, and
// puts incoming chunks back together. See e2ee_wire.go for the line format.

var (
	// Handshake lines go out at most handshakeBurst at once and then
	// handshakeRate a second, which is Ergo's default allowance. Tests raise it.
	handshakeBurst = 5.0
	handshakeRate  = 2.0
	// handshakeIncomplete is how long a transfer may wait for its missing chunks.
	handshakeIncomplete = time.Minute
	// handshakeRemember is how long a completed transfer is remembered, so a
	// transfer sent again after a reconnect is not taken twice.
	handshakeRemember = 10 * time.Minute
)

const (
	handshakeMaxPartial = 64
	handshakeMaxQueued  = 4096
	handshakeMaxTargets = 4
)

type handshakeTransport struct {
	mu        sync.Mutex
	partial   map[string]*partialTransfer
	done      map[string]time.Time
	sent      map[string]*outgoingTransfer
	lines     []pacedLine
	monitored map[string]bool
	running   bool
	tokens    float64
	refill    time.Time
}

type pacedLine struct {
	event  *girc.Event
	client *girc.Client
}

type partialTransfer struct {
	nick    string
	target  string
	count   int
	chunks  [][]byte
	got     int
	started time.Time
	at      int64
	msgid   string
}

// outgoingTransfer follows one object this connection sent until the server
// echoed every chunk. The echo of the last chunk is the object's position.
type outgoingTransfer struct {
	created time.Time
	count   int
	echoed  []bool
	seen    int
	at      int64
	msgid   string
	arrived time.Time
	done    chan struct{}
}

// handshakeEvent is a complete object as it arrived. account is the IRC
// account the network attached to every one of its lines; at and msgid are the
// server's stamp on its last chunk. ours marks the echo of a transfer this
// connection sent.
type handshakeEvent struct {
	kind     byte
	transfer string
	account  string
	nick     string
	target   string
	at       int64
	msgid    string
	arrived  time.Time
	ours     bool
	object   []byte
}

func serverTime(tag string) (int64, bool) {
	t, err := time.Parse(time.RFC3339Nano, tag)
	if err != nil {
		return 0, false
	}
	return t.UnixNano(), true
}

// targetLimit is how many names one TAGMSG may carry here. A name beyond the
// server's limit is dropped without an error, so stay within it.
func (c *connection) targetLimit() int {
	c.owner.mu.Lock()
	support := c.owner.networks[c.config.ID].ISupport
	c.owner.mu.Unlock()
	limit := 1
	for _, entry := range strings.Split(support["TARGMAX"], ",") {
		name, value, _ := strings.Cut(entry, ":")
		if strings.EqualFold(name, "TAGMSG") {
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				limit = n
			} else if value == "" {
				limit = handshakeMaxTargets
			}
		}
	}
	return min(limit, handshakeMaxTargets)
}

// sendHandshake queues one object for targets, several names a line where the
// server allows it. With track set, the returned transfer reports the echo.
func (c *connection) sendHandshake(kind byte, transfer string, targets []string, object []byte, track bool) (*outgoingTransfer, error) {
	chunks, err := chunkObject(kind, transfer, object)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, errors.New("a handshake object needs a target")
	}
	// A connection still completing its registration keeps the lines until it
	// is ready: an answer to a line that arrived early is not lost.
	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return nil, ErrDisconnected
	}
	limit := c.targetLimit()
	var lines []pacedLine
	for start := 0; start < len(targets); start += limit {
		group := strings.Join(targets[start:min(len(targets), start+limit)], ",")
		for _, chunk := range chunks {
			lines = append(lines, pacedLine{client: client, event: &girc.Event{Command: "TAGMSG", Params: []string{group}, Tags: girc.Tags{e2eeTag: chunk.tag()}}})
		}
	}
	t := &c.handshake
	t.mu.Lock()
	if len(t.lines)+len(lines) > handshakeMaxQueued {
		t.mu.Unlock()
		return nil, ErrBusy
	}
	if t.sent == nil {
		t.sent = map[string]*outgoingTransfer{}
	}
	now := time.Now()
	for k, o := range t.sent {
		if now.Sub(o.created) > handshakeRemember {
			delete(t.sent, k)
		}
	}
	// Every transfer this connection sends is remembered, tracked or not, so
	// its echoes are never taken for someone else's objects.
	key := string(kind) + transfer
	out := t.sent[key]
	if out == nil || out.count != len(chunks) {
		out = &outgoingTransfer{created: now, count: len(chunks), echoed: make([]bool, len(chunks)), done: make(chan struct{})}
		t.sent[key] = out
	}
	if !track {
		out = nil
	}
	t.lines = append(t.lines, lines...)
	start := !t.running
	t.running = true
	t.mu.Unlock()
	if start {
		go c.pumpHandshake()
	}
	return out, nil
}

// forgetSent drops the record of a transfer this connection sent.
func (c *connection) forgetSent(kind byte, transfer string) {
	t := &c.handshake
	t.mu.Lock()
	delete(t.sent, string(kind)+transfer)
	t.mu.Unlock()
}

// pumpHandshake moves paced lines into the connection's queue, behind lines
// that are already waiting there.
func (c *connection) pumpHandshake() {
	t := &c.handshake
	sleep := func(d time.Duration) bool {
		select {
		case <-c.ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	for {
		t.mu.Lock()
		if len(t.lines) == 0 || c.ctx.Err() != nil {
			t.lines, t.running = nil, false
			t.mu.Unlock()
			return
		}
		now := time.Now()
		if t.refill.IsZero() {
			t.tokens = handshakeBurst
		} else {
			t.tokens = min(handshakeBurst, t.tokens+now.Sub(t.refill).Seconds()*handshakeRate)
		}
		t.refill = now
		if t.tokens < 1 {
			wait := time.Duration((1 - t.tokens) / handshakeRate * float64(time.Second))
			t.mu.Unlock()
			if !sleep(wait) {
				return
			}
			continue
		}
		line := t.lines[0]
		t.mu.Unlock()
		for waited := 0; len(c.queue) > 0 && waited < 40; waited++ {
			if !sleep(50 * time.Millisecond) {
				return
			}
		}
		c.mu.Lock()
		current, ready := c.client, c.ready
		var err error
		switch {
		case current != line.client:
			err = ErrDisconnected
		case !ready:
			err = ErrBusy
		default:
			err = c.enqueueLocked(line.event)
		}
		c.mu.Unlock()
		if errors.Is(err, ErrBusy) {
			if !sleep(100 * time.Millisecond) {
				return
			}
			continue
		}
		t.mu.Lock()
		if err != nil {
			// The connection went away: its lines are dropped. Every sender
			// sends its object again under the same transfer.
			kept := t.lines[:0]
			for _, l := range t.lines {
				if l.client != line.client {
					kept = append(kept, l)
				}
			}
			t.lines = kept
		} else {
			t.lines = t.lines[1:]
			t.tokens--
		}
		t.mu.Unlock()
	}
}

// receiveHandshake takes one handshake line. A line without the network's
// account tag or the server's stamp is dropped, and chunks from different
// accounts never combine: the account is part of the transfer's key.
func (c *connection) receiveHandshake(client *girc.Client, e girc.Event) {
	c.mu.Lock()
	current := c.client == client
	c.mu.Unlock()
	if !current {
		return
	}
	ev, complete, finish := c.takeHandshake(e, time.Now())
	if complete && c.manager != nil {
		c.manager.e2eeReceive(c, ev)
	}
	finish()
}

// takeHandshake adds one line to its transfer. It returns the object once the
// transfer is complete, and a function that releases a sender waiting for the
// echo, which runs after the object was handled.
func (c *connection) takeHandshake(e girc.Event, now time.Time) (handshakeEvent, bool, func()) {
	nothing := func() {}
	value, ok := e.Tags[e2eeTag]
	if !ok || !strings.HasPrefix(value, handshakePrefix) || e.Source == nil || len(e.Params) == 0 {
		return handshakeEvent{}, false, nothing
	}
	line, ok := parseHandshakeLine(value)
	if !ok {
		return handshakeEvent{}, false, nothing
	}
	account := e.Tags["account"]
	at, stamped := serverTime(e.Tags["time"])
	msgid := e.Tags["msgid"]
	if account == "" || account == "*" || !clean(account, wireMaxName) || !stamped || msgid == "" || len(msgid) > wireMaxMsgID {
		return handshakeEvent{}, false, nothing
	}
	nick := e.Source.Name
	ours := c.fold(nick) == c.fold(c.currentNick())
	key := c.fold(account) + " " + string(line.kind) + line.transfer
	t := &c.handshake
	t.mu.Lock()
	if t.partial == nil {
		t.partial, t.done = map[string]*partialTransfer{}, map[string]time.Time{}
	}
	for k, p := range t.partial {
		if now.Sub(p.started) > handshakeIncomplete {
			delete(t.partial, k)
		}
	}
	for k, at := range t.done {
		if now.Sub(at) > handshakeRemember {
			delete(t.done, k)
		}
	}
	var sent *outgoingTransfer
	if ours {
		sent = t.sent[string(line.kind)+line.transfer]
	}
	if sent != nil && line.count == sent.count && !sent.echoed[line.index] {
		sent.echoed[line.index] = true
		sent.seen++
		if line.index == line.count-1 {
			sent.at, sent.msgid, sent.arrived = at, msgid, now
		}
	}
	finish := func() {
		if sent != nil && sent.seen == sent.count {
			select {
			case <-sent.done:
			default:
				close(sent.done)
			}
		}
	}
	// A Welcome sent again after its receipt was lost is taken again, so the
	// receipt goes out again; storage keeps it once.
	if _, completed := t.done[key]; completed && line.kind != kindWelcome {
		t.mu.Unlock()
		return handshakeEvent{}, false, finish
	}
	p := t.partial[key]
	if p == nil {
		if len(t.partial) >= handshakeMaxPartial {
			t.mu.Unlock()
			return handshakeEvent{}, false, finish
		}
		p = &partialTransfer{nick: nick, target: e.Params[0], count: line.count, chunks: make([][]byte, line.count), started: now}
		t.partial[key] = p
	}
	if p.count != line.count || !strings.EqualFold(p.nick, nick) {
		// One transfer, two shapes or two senders: not an object to trust.
		delete(t.partial, key)
		t.mu.Unlock()
		return handshakeEvent{}, false, finish
	}
	if p.chunks[line.index] == nil {
		p.chunks[line.index] = line.data
		p.got++
	}
	if line.index == line.count-1 {
		p.at, p.msgid = at, msgid
	}
	if p.got < p.count {
		t.mu.Unlock()
		return handshakeEvent{}, false, finish
	}
	delete(t.partial, key)
	t.done[key] = now
	object := make([]byte, 0, p.count*handshakeChunkBytes)
	for _, chunk := range p.chunks {
		object = append(object, chunk...)
	}
	t.mu.Unlock()
	ev := handshakeEvent{kind: line.kind, transfer: line.transfer, account: account, nick: p.nick, target: p.target, at: p.at, msgid: p.msgid, arrived: now, ours: sent != nil, object: object}
	return ev, true, finish
}

// monitor asks the server to say when these accounts come online, so Welcomes
// waiting for them go out at once instead of at their next retry.
func (c *connection) monitor(nicks []string) {
	c.owner.mu.Lock()
	value, supported := c.owner.networks[c.config.ID].ISupport["MONITOR"]
	c.owner.mu.Unlock()
	if !supported {
		return
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit <= 0 || limit > 100 {
		limit = 100
	}
	t := &c.handshake
	t.mu.Lock()
	if t.monitored == nil {
		t.monitored = map[string]bool{}
	}
	add := []string{}
	for _, nick := range nicks {
		if folded := c.fold(nick); !t.monitored[folded] {
			t.monitored[folded] = true
			add = append(add, nick)
		}
	}
	reset := len(t.monitored) > limit
	if reset {
		t.monitored = map[string]bool{}
		for _, nick := range add {
			t.monitored[c.fold(nick)] = true
		}
	}
	t.mu.Unlock()
	if len(add) == 0 || len(add) > limit {
		return
	}
	if reset {
		_ = c.enqueue(&girc.Event{Command: "MONITOR", Params: []string{"C"}})
	}
	_ = c.enqueue(&girc.Event{Command: "MONITOR", Params: []string{"+", strings.Join(add, ",")}})
}

// monitorOnline wakes the Welcomes waiting for accounts that came online
// (RPL_MONONLINE).
func (c *connection) monitorOnline(e girc.Event) {
	if c.manager == nil || c.manager.options.Store == nil || len(e.Params) < 2 {
		return
	}
	store := c.manager.options.Store
	woke := false
	for _, entry := range strings.Split(e.Last(), ",") {
		nick, _, _ := strings.Cut(entry, "!")
		if nick == "" {
			continue
		}
		if ok, err := store.WakeE2EEOutbox(c.owner.ownerID, nick); err == nil && ok {
			woke = true
		}
	}
	if woke {
		c.manager.net.wakeOutbox()
	}
}

// awaitEcho waits until the server echoed every chunk of a tracked transfer.
func (c *connection) awaitEcho(ctx context.Context, out *outgoingTransfer) error {
	select {
	case <-out.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return ErrDisconnected
	}
}
