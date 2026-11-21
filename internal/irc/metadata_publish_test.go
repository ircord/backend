package irc

import (
	"slices"
	"testing"
	"time"

	"github.com/lrstanley/girc"
)

func TestMetadataRetryUsesNewestValue(t *testing.T) {
	for _, labeled := range []bool{false, true} {
		m, _ := storedProfileNetworks(t, false)
		c := profileConnection(m, "alice", "one")
		c.caps["labeled-response"] = labeled
		answerProfileCheck(c, map[string]string{"status": "Current"})
		c.queueMetadataLocked("status", "First")
		c.queueMetadataLocked(deviceKey, "phone")
		write := c.nextMetadataLocked(time.Now())
		if write == nil || !slices.Equal(write.Params, []string{"*", "SET", "status", "First"}) {
			t.Fatal("unexpected first write", write)
		}
		if c.nextMetadataLocked(time.Now()) != nil {
			t.Fatal("a second write passed the in-flight command")
		}
		reply := *girc.ParseEvent(":irc.test FAIL METADATA RATE_LIMITED onenick status 3 :Rate limited")
		if labeled {
			reply.Tags = girc.Tags{"label": "raw:other"}
			if c.metadataWriteReply(reply) {
				t.Fatal("a raw command's failure was consumed")
			}
		}
		reply.Tags = write.Tags
		before := time.Now()
		if !c.metadataWriteReply(reply) {
			t.Fatal("managed retryable failure was not consumed")
		}
		retryAt := c.metadataWrites.retryAt
		if retryAt.Before(before.Add(3 * time.Second)) {
			t.Fatal("the server asked for a longer delay")
		}
		if got := c.publishedProfile()["status"]; got != "Current" {
			t.Fatal("a rejected value replaced the last confirmed value", got)
		}
		c.queueMetadataLocked("status", "Latest")
		if c.nextMetadataLocked(retryAt.Add(-time.Millisecond)) != nil {
			t.Fatal("a metadata write passed the retry deadline")
		}
		write = c.nextMetadataLocked(retryAt)
		if write == nil || write.Last() != "Latest" {
			t.Fatal("the retry lost the latest value", write)
		}
		reply = *girc.ParseEvent(":irc.test 761 onenick onenick status * :Latest")
		reply.Tags = write.Tags
		if !c.metadataWriteReply(reply) || c.publishedProfile()["status"] != "Latest" {
			t.Fatal("the server-confirmed retry was not recorded")
		}
		c.queueMetadataLocked("status", "Latest")
		write = c.nextMetadataLocked(retryAt)
		if write == nil || !slices.Equal(write.Params, []string{"*", "SET", deviceKey, "phone"}) {
			t.Fatal("the acknowledged status was resent or the device was lost", write)
		}
	}
}

func TestExpiringStatusCancelsAnUnsentOrThrottledWrite(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		m, _ := storedProfileNetworks(t, false)
		c := profileConnection(m, "alice", "one")
		c.queueMetadataLocked("status", "Temporary")
		if rejected {
			c.nextMetadataLocked(time.Now())
			if !c.metadataWriteReply(*girc.ParseEvent(":irc.test FAIL METADATA RATE_LIMITED onenick status 3 :Rate limited")) {
				t.Fatal("the rejection was not handled")
			}
		}
		c.expireProfileMetadata(Member{StatusText: "Temporary"}, []string{"status"})
		if got := c.nextMetadataLocked(time.Now().Add(time.Minute)); got != nil {
			t.Fatal("an expired status was sent", got)
		}
	}
}

func TestExpiringInFlightStatusClearsAfterConfirmation(t *testing.T) {
	m, _ := storedProfileNetworks(t, false)
	c := profileConnection(m, "alice", "one")
	c.queueMetadataLocked("status", "Temporary")
	c.nextMetadataLocked(time.Now())
	c.expireProfileMetadata(Member{StatusText: "Temporary"}, []string{"status"})
	if !c.metadataWriteReply(*girc.ParseEvent(":irc.test 761 onenick onenick status * :Temporary")) {
		t.Fatal("the set was not confirmed")
	}
	clear := c.nextMetadataLocked(time.Now())
	if clear == nil || !slices.Equal(clear.Params, []string{"*", "SET", "status"}) {
		t.Fatal("the server accepted a status that expired before the reply", clear)
	}
	if !c.metadataWriteReply(*girc.ParseEvent(":irc.test 766 onenick onenick status :Key deleted")) || c.publishedProfile()["status"] != "" {
		t.Fatal("the clear was not confirmed")
	}
}
