package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"ircord/backend/internal/irc"
	"ircord/backend/internal/storage"
)

func TestSessionReadyMatchesAccountCapabilities(t *testing.T) {
	associated, _ := fakeErgo(t)
	const networkID = "ircord"
	_, server, store, _ := authenticatedApp(t, func(options *irc.Options) {
		options.AssociatedServer = associated
		options.Home = irc.NetworkConfig{ID: networkID}
		options.Moderation = &irc.ModerationLogin{Operator: "moderator", Password: "test-password"}
	})
	for _, test := range []struct {
		name       string
		moderation *irc.ModerationCapability
	}{
		{name: "owner", moderation: &irc.ModerationCapability{NetworkID: networkID}},
		{name: "member"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := cookieClient()
			account := register(t, client, server.URL)
			if test.moderation != nil {
				if err := store.GrantOwner(account.Account.ID, storage.OwnerFromOperator, "test"); err != nil {
					t.Fatal(err)
				}
			}
			var bootstrap irc.Snapshot
			if err := json.Unmarshal(request(t, client, "GET", server.URL+"/api/v1/bootstrap", nil, 200), &bootstrap); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(bootstrap.Capabilities.Moderation, test.moderation) {
				t.Fatalf("bootstrap moderation = %+v, want %+v", bootstrap.Capabilities.Moderation, test.moderation)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			options := &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Origin": []string{server.URL}}}
			socket, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/events?sync=1", options)
			if err != nil {
				t.Fatal(err)
			}
			defer socket.CloseNow()
			_, raw, err := socket.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var ready sessionReady
			if err := json.Unmarshal(raw, &ready); err != nil {
				t.Fatal(err)
			}
			if ready.Type != "session.ready" || !ready.Sync || ready.Capabilities == nil {
				t.Fatalf("expected synchronized session.ready with capabilities, got %s", raw)
			}
			if !reflect.DeepEqual(*ready.Capabilities, bootstrap.Capabilities) {
				t.Fatalf("session.ready capabilities differ from bootstrap: moderation = %+v, want %+v", ready.Capabilities.Moderation, bootstrap.Capabilities.Moderation)
			}
		})
	}
}

func TestReplayBarrierPrecedesLiveAndHandlesEmptyReplay(t *testing.T) {
	app, server, _, _ := authenticatedApp(t)
	client := cookieClient()
	account := register(t, client, server.URL)
	owner := account.Account.ID
	for _, empty := range []bool{false, true} {
		cursor := app.Manager.Cursor(owner)
		if !empty {
			if _, err := app.Manager.SetProfile(owner, irc.Profile{Bio: "while away"}); err != nil {
				t.Fatal(err)
			}
		}
		head := app.Manager.Cursor(owner)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		socket, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/events?sync=1", &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Origin": []string{server.URL}}})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		read := func() map[string]any {
			t.Helper()
			_, raw, err := socket.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var event map[string]any
			if err = json.Unmarshal(raw, &event); err != nil {
				t.Fatal(err)
			}
			return event
		}
		if ready := read(); ready["type"] != "session.ready" || ready["sync"] != true {
			t.Fatal("missing synchronization capability")
		}
		resume, _ := json.Marshal(sessionResume{V: 1, Type: "session.resume", After: cursor, Sync: true})
		if err := socket.Write(ctx, websocket.MessageText, resume); err != nil {
			t.Fatal(err)
		}
		var barrier map[string]any
		last := cursor
		for {
			event := read()
			if event["type"] == "session.synced" {
				barrier = event
				break
			}
			seq := uint64(event["seq"].(float64))
			if seq != last+1 || seq > head {
				t.Fatal("replay is not contiguous")
			}
			last = seq
		}
		if last != head || uint64(barrier["cursor"].(float64)) != head {
			t.Fatal("barrier did not identify the completed replay")
		}
		if _, err := app.Manager.SetProfile(owner, irc.Profile{Bio: "now live"}); err != nil {
			t.Fatal(err)
		}
		if event := read(); event["type"] != "profile.updated" || uint64(event["seq"].(float64)) != head+1 {
			t.Fatal("live event did not follow the replay barrier")
		}
		socket.CloseNow()
		cancel()
	}
}
