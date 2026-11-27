package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIrcoidScoresAreSeparateValidatedAndRateLimited(t *testing.T) {
	_, server, _, _ := authenticatedApp(t)
	url := server.URL + "/api/v1/ircoid/scores"
	anonymous := cookieClient()
	request(t, anonymous, "GET", url, nil, 401)
	request(t, anonymous, "POST", url, map[string]any{"name": "x", "score": 100}, 401)

	alice := cookieClient()
	register(t, alice, server.URL)
	var table IrcoidTable
	json.Unmarshal(request(t, alice, "GET", url, nil, 200), &table)
	if len(table.Items) != 2 || table.Items[0] != (IrcoidEntry{1, "Peter Petermann", 225357}) || table.Items[1] != (IrcoidEntry{2, "Michael Karbowiak", 2102}) {
		t.Fatalf("unexpected seeded table: %+v", table)
	}

	for _, bad := range []struct {
		name   string
		score  int
		detail string
	}{
		{"Alice", 0, "score"},
		{"Alice", -10, "score"},
		{"Alice", 105, "score"},
		{"Alice", 10_000_010, "score"},
		{"   ", 100, scoreNameLength},
		{strings.Repeat("a", 25), 100, scoreNameLength},
		{"bad\u0007bell", 100, scoreNameCharacters},
		{"right\u202eleft", 100, scoreNameCharacters},
	} {
		body := request(t, alice, "POST", url, map[string]any{"name": bad.name, "score": bad.score}, 422)
		if !strings.Contains(string(body), bad.detail) {
			t.Fatalf("%q/%d: unexpected problem %s", bad.name, bad.score, body)
		}
	}

	// A score between the two seeds places second.
	var placed IrcoidResult
	json.Unmarshal(request(t, alice, "POST", url, map[string]any{"name": "  Alice \t Liddell ", "score": 5000}, 200), &placed)
	if !placed.Placed || placed.Rank != 2 || len(placed.Items) != 3 || placed.Items[1] != (IrcoidEntry{2, "Alice Liddell", 5000}) || placed.Items[2] != (IrcoidEntry{3, "Michael Karbowiak", 2102}) {
		t.Fatalf("score did not place: %+v", placed)
	}
	// One account cannot submit again straight away.
	request(t, alice, "POST", url, map[string]any{"name": "Alice", "score": 40000}, 429)
	// The games keep separate limits and separate tables.
	var hash HashFieldResult
	json.Unmarshal(request(t, alice, "POST", server.URL+"/api/v1/hash-field/scores", map[string]any{"name": "Alice", "score": 100}, 200), &hash)
	if !hash.Placed || len(hash.Items) != 3 || hash.Items[0].Name != "Michael Karbowiak" {
		t.Fatalf("Hash Field table was touched by Ircoid: %+v", hash)
	}

	bob := cookieClient()
	register(t, bob, server.URL)
	json.Unmarshal(request(t, bob, "GET", url, nil, 200), &table)
	if len(table.Items) != 3 || table.Items[1].Name != "Alice Liddell" {
		t.Fatalf("table is not shared: %+v", table)
	}
}
