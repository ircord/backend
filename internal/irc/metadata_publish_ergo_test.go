package irc

import (
	"net"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"ircord/backend/internal/activity"
)

func TestErgoMetadataRetriesLatestAfterRateLimit(t *testing.T) {
	if os.Getenv("IRCORD_METADATA_INTEGRATION") != "1" && os.Getenv("IRCORD_ERGO_ADDR") == "" {
		t.Skip("set IRCORD_METADATA_INTEGRATION=1 or run make test-integration")
	}
	host, port, _ := isolatedErgo(t, func(cfg map[string]any) {
		cfg["metadata"].(map[string]any)["client-throttle"] = map[string]any{
			"enabled": true, "duration": "5s", "max-attempts": 2,
		}
	})
	t.Setenv("IRCORD_ERGO_ADDR", net.JoinHostPort(host, strconv.Itoa(port)))
	f := newMetadataFixture(t, "thrtl")
	f.connect(f.bobSide, "bob", f.config(f.bob, ""), true)
	f.connect(f.aliceSide, "alice", f.config(f.alice, ""), true)
	profileChecked(t, f.aliceSide, "alice")
	eventually(t, func() bool { return f.peer().Nick == f.alice })
	c, err := f.aliceSide.connection("alice", "net")
	if err != nil {
		t.Fatal(err)
	}
	_, updates, _, unsubscribe := f.aliceSide.Subscribe("alice", 0)
	defer unsubscribe()

	router := activity.New(activity.Options{Source: f.aliceSide})
	defer router.Close()
	openPhone := func(page string) func() {
		t.Helper()
		sub, stop := router.SubscribeClient("alice", "phone", "tab", page)
		t.Cleanup(stop)
		binding := (<-sub.Updates).ConnectionID
		if !router.ReportState("alice", "phone", activity.Report{
			Tab: "tab", Incarnation: page, ConnectionID: binding, Sequence: 1,
			Signal: "engagement", Active: true, FormFactor: activity.Phone,
		}) {
			t.Fatal("activity report was rejected")
		}
		return stop
	}
	setName := func(name string) {
		t.Helper()
		if _, err := f.aliceSide.ApplyProfilePatch("alice", "net", ProfilePatch{DisplayName: &name}, nil); err != nil {
			t.Fatal(err)
		}
	}
	closePhone := openPhone("phone-before-reload")
	setName("Current")
	eventually(t, func() bool {
		peer := f.peer()
		return peer.DisplayName == "Current" && peer.Device == activity.Phone &&
			c.publishedProfile()["display-name"] == "Current"
	})
	closePhone()
	openPhone("phone-after-reload")
	setName("Rejected")
	var retryAt time.Time
	eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		retryAt = c.metadataWrites.retryAt
		return !retryAt.IsZero()
	})
	if got := c.publishedProfile()["display-name"]; got != "Current" {
		t.Fatal("publication history contains a rejected value", got)
	}
	setName("Latest")
	f.aliceSide.SetDevice("alice", activity.Tablet)
	eventually(t, func() bool {
		peer := f.peer()
		return peer.DisplayName == "Latest" && peer.Device == activity.Tablet &&
			c.publishedProfile()["display-name"] == "Latest"
	})

	c.owner.mu.Lock()
	trace := slices.Clone(c.owner.trace["net"])
	c.owner.mu.Unlock()
	var writes [][]string
	throttled := 0
	for _, line := range trace {
		if line.Command == "FAIL" && len(line.Params) >= 6 && line.Params[0] == "METADATA" && line.Params[1] == "RATE_LIMITED" {
			throttled++
			if line.Params[3] != "display-name" {
				t.Fatal("the reload spent the metadata allowance", line.Params)
			}
		}
		if line.Kind == "outgoing" && line.Command == "METADATA" && len(line.Params) >= 3 && line.Params[1] == "SET" {
			writes = append(writes, line.Params[2:])
			if last := line.Params[len(line.Params)-1]; (last == "Latest" || last == activity.Tablet) && line.Time.Before(retryAt) {
				t.Fatal("a write passed the retry deadline", line.Params)
			}
		}
	}
	want := [][]string{{"display-name", "Current"}, {deviceKey, activity.Phone}, {"display-name", "Rejected"}, {"display-name", "Latest"}, {deviceKey, activity.Tablet}}
	// Either initial write may win the writer's first tick.
	if len(writes) >= 2 && writes[0][0] == deviceKey {
		want[0], want[1] = want[1], want[0]
	}
	if throttled != 1 || !slices.EqualFunc(writes, want, slices.Equal) {
		t.Fatalf("got %v with %d rejections, want %v with one rejection", writes, throttled, want)
	}
	for {
		select {
		case e, open := <-updates:
			if !open || e.Type == "command.error" {
				t.Fatal("unexpected command error or subscription close", e.Message)
			}
		default:
			return
		}
	}
}
