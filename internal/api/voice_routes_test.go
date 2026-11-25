package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"ircord/backend/internal/storage"
	"ircord/backend/internal/voice"
)

func TestVoiceRoomManagementRequiresServerOwnership(t *testing.T) {
	app, server, store, _ := authenticatedApp(t)
	client := cookieClient()
	account := register(t, client, server.URL)
	rooms := server.URL + "/api/v1/voice/rooms"
	payload := map[string]any{"name": "Example", "bitrateKbps": 128}
	request(t, cookieClient(), "GET", rooms, nil, 401)
	request(t, client, "POST", rooms, payload, 403)
	request(t, client, "PATCH", rooms+"/example", payload, 403)
	request(t, client, "DELETE", rooms+"/example", nil, 403)
	if err := store.GrantOwner(account.Account.ID, storage.OwnerFromOperator, "operator"); err != nil {
		t.Fatal(err)
	}
	request(t, client, "POST", rooms, payload, 503)

	sfu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/twirp/livekit.RoomService/DeleteRoom" {
			t.Error("unexpected media request", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer sfu.Close()
	public, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	service, err := voice.New(voice.Config{URL: sfu.URL, Key: "fixture-key", Secret: strings.Repeat("s", 32)}, public, nil, store.SessionActive)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.OpenCatalog(t.Context(), store, ""); err != nil {
		t.Fatal(err)
	}
	app.options.Voice = service
	var created voice.Room
	if err := json.Unmarshal(request(t, client, "POST", rooms, payload, 201), &created); err != nil || created.ID == "" {
		t.Fatalf("created room: %+v, %v", created, err)
	}
	member := cookieClient()
	register(t, member, server.URL)
	var visible []voice.Room
	if err := json.Unmarshal(request(t, member, "GET", rooms, nil, 200), &visible); err != nil || len(visible) != 1 || visible[0].ID != created.ID {
		t.Fatalf("signed-in account with no IRC membership did not see the room: %+v, %v", visible, err)
	}
	resource := rooms + "/" + created.ID
	request(t, member, "PATCH", resource, payload, 403)
	request(t, client, "POST", rooms, payload, 409)
	var changed voice.Room
	if err := json.Unmarshal(request(t, client, "PATCH", resource, map[string]any{"name": "Renamed", "bitrateKbps": 160}, 200), &changed); err != nil || changed.Name != "Renamed" || changed.BitrateKbps != 160 {
		t.Fatalf("updated room: %+v, %v", changed, err)
	}
	request(t, client, "DELETE", resource, nil, 204)
	request(t, client, "PATCH", resource, payload, 404)
}
