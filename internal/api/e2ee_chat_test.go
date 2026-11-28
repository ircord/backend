package api

import (
	"encoding/json"
	"testing"

	"ircord/backend/internal/irc"
	"ircord/backend/internal/storage"
)

// The encrypted chat endpoints read and change only the signed-in account's
// chats, Welcomes, commits and rejoin requests. Without an IRC connection that
// passed the self-test nothing goes out, and nothing falls back to plaintext.
func TestE2EEChatEndpointsAreAccountIsolated(t *testing.T) {
	_, server, store, _ := authenticatedApp(t, func(o *irc.Options) { o.E2EE = true })
	base := server.URL + "/api/v1/e2ee"
	aliceClient, malloryClient := cookieClient(), cookieClient()
	alice := register(t, aliceClient, server.URL)
	register(t, malloryClient, server.URL)
	id := alice.Account.ID

	var snapshot irc.Snapshot
	json.Unmarshal(request(t, aliceClient, "GET", server.URL+"/api/v1/bootstrap", nil, 200), &snapshot)
	if c := snapshot.Capabilities.Encryption; c == nil || !c.Groups || !c.AcrossServers {
		t.Fatalf("encryption capability = %+v", c)
	}

	request(t, aliceClient, "PUT", base+"/identity", map[string]any{"identityKey": b64("alice-key")}, 200)
	request(t, aliceClient, "PUT", base+"/devices/laptop", map[string]any{"generation": 1, "certificate": b64("cert")}, 204)
	group := "AAECAwQFBgcICQoLDA0ODw"
	if err := store.PutE2EEChat(id, storage.E2EEChat{Group: group, Kind: storage.E2EEChatGroup, State: storage.E2EEChatJoined, Conversation: "conv", Target: "#e2ee-000102030405060708090a0b0c0d0e0f", Inviter: alice.Username}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ObserveE2EECommit(id, group, storage.E2EEObserved{Epoch: 1, At: 1_000_000, MsgID: "m1", Sender: "bob", Commit: []byte("commit")}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddE2EEChatWelcome(id, storage.E2EEChatWelcome{Group: group, Epoch: 1, Kind: storage.E2EEChatGroup, Target: "#e2ee-000102030405060708090a0b0c0d0e0f", Sender: "bob", Welcome: []byte("w"), At: 1, MsgID: "w1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddE2EERejoin(id, group, storage.E2EERejoin{Account: "bob", Device: "phone", Certificate: []byte("c"), KeyPackage: []byte("k")}); err != nil {
		t.Fatal(err)
	}

	var chats Items[E2EEChat]
	json.Unmarshal(request(t, aliceClient, "GET", base+"/chats", nil, 200), &chats)
	if len(chats.Items) != 1 || chats.Items[0].GroupID != group || chats.Items[0].Kind != "group" || chats.Items[0].State != "joined" {
		t.Fatalf("alice's chats %+v", chats)
	}
	json.Unmarshal(request(t, malloryClient, "GET", base+"/chats", nil, 200), &chats)
	if len(chats.Items) != 0 {
		t.Fatalf("mallory sees %+v", chats)
	}
	request(t, malloryClient, "GET", base+"/chats/"+group, nil, 404)
	request(t, malloryClient, "GET", base+"/chats/"+group+"/commits", nil, 404)
	request(t, malloryClient, "GET", base+"/chats/"+group+"/rejoins", nil, 404)
	request(t, malloryClient, "DELETE", base+"/chats/"+group, nil, 404)
	request(t, malloryClient, "POST", base+"/chats/"+group+"/decline", nil, 404)
	request(t, malloryClient, "POST", base+"/chats/"+group+"/commits", map[string]any{"epoch": 1, "commit": b64("x")}, 404)

	var commits Items[E2EEChatCommit]
	json.Unmarshal(request(t, aliceClient, "GET", base+"/chats/"+group+"/commits?from=1", nil, 200), &commits)
	if len(commits.Items) != 1 || commits.Items[0].Sender != "bob" || commits.Items[0].Position.MsgID != "m1" || string(commits.Items[0].Commit) != "commit" {
		t.Fatalf("commits %+v", commits)
	}
	var welcomes Items[E2EEChatWelcome]
	json.Unmarshal(request(t, aliceClient, "GET", base+"/devices/laptop/chat-welcomes", nil, 200), &welcomes)
	if len(welcomes.Items) != 1 || welcomes.Items[0].Sender != "bob" {
		t.Fatalf("welcomes %+v", welcomes)
	}
	json.Unmarshal(request(t, malloryClient, "GET", base+"/devices/laptop/chat-welcomes", nil, 200), &welcomes)
	if len(welcomes.Items) != 0 {
		t.Fatal("mallory reads alice's Welcomes")
	}
	request(t, malloryClient, "DELETE", base+"/chat-welcomes/1", nil, 404)
	var rejoins Items[E2EERejoin]
	json.Unmarshal(request(t, aliceClient, "GET", base+"/chats/"+group+"/rejoins", nil, 200), &rejoins)
	if len(rejoins.Items) != 1 || rejoins.Items[0].DeviceID != "phone" {
		t.Fatalf("rejoins %+v", rejoins)
	}
	request(t, malloryClient, "DELETE", base+"/rejoins/1", nil, 404)
	request(t, aliceClient, "DELETE", base+"/rejoins/1", nil, 204)

	// Nothing goes out without an IRC connection that passed the self-test.
	request(t, aliceClient, "POST", base+"/chats/"+group+"/commits", map[string]any{"epoch": 1, "commit": b64("x")}, 400)
	request(t, aliceClient, "POST", base+"/chats", map[string]any{"groupId": "AQECAwQFBgcICQoLDA0ODw", "kind": "group", "members": []string{"bob"}, "epoch": 1}, 400)
	request(t, aliceClient, "POST", base+"/chats", map[string]any{"groupId": "g", "kind": "direct", "members": []string{"bob"}, "epoch": 1}, 422)
	request(t, aliceClient, "GET", base+"/accounts/someone-elsewhere", nil, 404)

	// The signed device list only moves forward.
	request(t, aliceClient, "PUT", base+"/device-list", map[string]any{"list": b64("l1"), "counter": 2}, 200)
	request(t, aliceClient, "PUT", base+"/device-list", map[string]any{"list": b64("l0"), "counter": 1}, 409)
	request(t, malloryClient, "PUT", base+"/device-list", map[string]any{"list": b64("l1"), "counter": 2}, 409)
	var own E2EEOwnDirectory
	json.Unmarshal(request(t, aliceClient, "GET", base+"/identity", nil, 200), &own)
	if own.DeviceList == nil || own.DeviceList.Counter != 2 || own.Elsewhere {
		t.Fatalf("own directory %+v", own)
	}
	var mallorys E2EEOwnDirectory
	json.Unmarshal(request(t, malloryClient, "GET", base+"/identity", nil, 200), &mallorys)
	if mallorys.Identity != nil || mallorys.DeviceList != nil || mallorys.Elsewhere {
		t.Fatalf("mallory's directory %+v", mallorys)
	}

	// Leaving forgets the chat with its commits.
	request(t, aliceClient, "DELETE", base+"/chats/"+group, nil, 204)
	request(t, aliceClient, "GET", base+"/chats/"+group, nil, 404)
}
