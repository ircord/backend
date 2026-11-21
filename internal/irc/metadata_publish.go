package irc

import (
	"strconv"
	"strings"
	"time"

	"github.com/lrstanley/girc"
)

const metadataReplyTimeout = 10 * time.Second

// A connection keeps one desired value per supported key and sends one SET at
// a time. The server throttles all of our own keys together. All fields are
// protected by c.mu.
type metadataPublisher struct {
	known    map[string]string
	pending  map[string]string
	inFlight *metadataWrite
	retryAt  time.Time
}

type metadataWrite struct {
	key, value, label string
	deadline          time.Time
	expired           bool
}

func (c *connection) queueMetadataLocked(key, value string) {
	if c.metadataWrites.pending == nil {
		c.metadataWrites.pending = make(map[string]string)
	}
	c.metadataWrites.pending[key] = value
	c.wakeMetadataLocked(time.Now())
}

// The writer also polls on its maintenance tick, so a backoff or a missing
// reply never blocks chat or needs its own timer.
func (c *connection) wakeMetadataLocked(now time.Time) {
	p := &c.metadataWrites
	if write := p.inFlight; write != nil && !now.Before(write.deadline) {
		delete(p.known, write.key)
		p.inFlight = nil
	}
	if len(p.pending) > 0 && p.inFlight == nil && !now.Before(p.retryAt) && c.profileCheckDone && c.ready && c.caps["draft/metadata-3"] {
		select {
		case c.metadataWake <- struct{}{}:
		default:
		}
	}
}

func (c *connection) nextMetadataLocked(now time.Time) *girc.Event {
	p := &c.metadataWrites
	if !c.ready || !c.profileCheckDone || !c.caps["draft/metadata-3"] || p.inFlight != nil || now.Before(p.retryAt) {
		return nil
	}
	for _, key := range subscribedKeys() {
		value, pending := p.pending[key]
		if !pending {
			continue
		}
		delete(p.pending, key)
		if known, ok := p.known[key]; ok && known == value {
			continue
		}
		params := []string{"*", "SET", key}
		if value != "" {
			params = append(params, value)
		}
		event := &girc.Event{Command: "METADATA", Params: params}
		write := &metadataWrite{key: key, value: value, deadline: now.Add(metadataReplyTimeout)}
		if c.caps["labeled-response"] {
			write.label = "metadata:" + id()
			event.Tags = girc.Tags{"label": write.label}
		}
		p.inFlight = write
		return event
	}
	return nil
}

// metadataWriteReply handles our own SET reply before the regular metadata
// observer can mistake it for another client's edit or show a retryable error.
func (c *connection) metadataWriteReply(e girc.Event) bool {
	if len(e.Params) < 3 || e.Command == "FAIL" && e.Params[0] != "METADATA" {
		return false
	}
	label := c.eventLabel(e)
	internal := strings.HasPrefix(label, "metadata:")
	if e.Command == "FAIL" && label != "" && !internal {
		return false
	}
	nick := c.fold(c.currentNick())
	own := func(target string) bool { return c.fold(target) == nick }
	key, value := e.Params[2], ""
	rateLimited := e.Command == "FAIL" && e.Params[1] == "RATE_LIMITED"
	var retry time.Duration
	switch e.Command {
	case "761", "766":
		if !own(e.Params[0]) || !own(e.Params[1]) {
			return false
		}
		if e.Command == "761" {
			if len(e.Params) < 5 {
				return false
			}
			value = e.Params[4]
		}
	case "FAIL":
		if rateLimited {
			if len(e.Params) < 6 || e.Params[2] != "*" && !own(e.Params[2]) {
				return false
			}
			key = e.Params[3]
			if seconds, err := strconv.ParseUint(e.Params[4], 10, 32); err == nil && seconds > 0 {
				retry = time.Duration(seconds) * time.Second
			}
		} else if label == "" {
			// Without a label, errors with different context shapes can also
			// come from GET, SYNC or an explicit raw command.
			return false
		}
	default:
		return false
	}

	c.owner.mu.Lock()
	defer c.owner.mu.Unlock()
	c.mu.Lock()
	p := &c.metadataWrites
	write := p.inFlight
	if write == nil || label != write.label ||
		(rateLimited || e.Command != "FAIL") && key != write.key ||
		e.Command != "FAIL" && value != write.value {
		if e.Command != "FAIL" && !internal && c.profileCheckDone && (isMetadataKey(key) || key == deviceKey) {
			c.learnMetadataLocked(key, value)
		}
		c.mu.Unlock()
		return false
	}
	p.inFlight = nil
	if retry > 0 {
		p.retryAt = time.Now().Add(retry)
		if _, newer := p.pending[key]; !newer && !write.expired {
			p.pending[key] = write.value
		}
	} else if e.Command != "FAIL" {
		c.learnMetadataLocked(key, value)
		if _, newer := p.pending[key]; !newer && write.expired && value != "" {
			c.queueMetadataLocked(key, "")
		}
	}
	c.wakeMetadataLocked(time.Now())
	c.mu.Unlock()
	if e.Command == "FAIL" {
		return retry > 0
	}
	if isMetadataKey(key) {
		c.recordPublished(map[string]string{key: value})
	}
	return true
}

// The workspace has already saved the expiration. Cancel an unsent status,
// then clear any value this backend shared. A write in flight needs a clear
// only if it is accepted.
func (c *connection) expireProfileMetadata(before Member, keys []string) {
	published, values := c.publishedProfile(), profileValues(before)
	c.mu.Lock()
	defer c.mu.Unlock()
	p := &c.metadataWrites
	for _, key := range keys {
		_, queued := p.pending[key]
		delete(p.pending, key)
		if !c.ready || !c.profileCheckDone || !c.caps["draft/metadata-3"] {
			continue
		}
		if write := p.inFlight; write != nil && write.key == key {
			write.expired = true
		}
		last, owned := published[key]
		current, observed := p.known[key]
		if owned && last != "" && (last == values[key] || queued) && (!observed || current == last) {
			c.queueMetadataLocked(key, "")
		}
	}
}

func (c *connection) learnMetadataLocked(key, value string) {
	if c.metadataWrites.known == nil {
		c.metadataWrites.known = make(map[string]string)
	}
	c.metadataWrites.known[key] = value
}

// A broadcast of the last observed value can arrive after a local edit.
// A different value is another client's edit unless our SET is in flight.
func (c *connection) observeOwnMetadata(key, value string, changed bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := &c.metadataWrites
	last, known := p.known[key]
	c.learnMetadataLocked(key, value)
	_, pending := p.pending[key]
	preserve := p.inFlight != nil && p.inFlight.key == key ||
		pending && (key == deviceKey || known && last == value)
	if changed && !preserve {
		delete(p.pending, key)
	}
	return preserve
}
