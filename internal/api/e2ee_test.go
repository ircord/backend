package api

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"ircord/backend/internal/irc"
	"ircord/backend/internal/model"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestE2EEOffByDefault(t *testing.T) {
	_, server, _, _ := authenticatedApp(t)
	alice := cookieClient()
	register(t, alice, server.URL)
	var snapshot irc.Snapshot
	json.Unmarshal(request(t, alice, "GET", server.URL+"/api/v1/bootstrap", nil, 200), &snapshot)
	if snapshot.Capabilities.Encryption != nil {
		t.Fatal("encryption capability advertised with the operator switch off")
	}
	for _, r := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/v1/e2ee/identity", nil},
		{"PUT", "/api/v1/e2ee/identity", map[string]any{"identityKey": b64("key")}},
		{"GET", "/api/v1/e2ee/accounts/someone", nil},
		{"POST", "/api/v1/e2ee/approvals", map[string]any{"fromDevice": "a", "toDevice": "b", "payload": b64("x")}},
	} {
		if !strings.Contains(string(request(t, alice, r.method, server.URL+r.path, r.body, 404)), "not turned on") {
			t.Fatalf("%s %s did not explain that encryption is off", r.method, r.path)
		}
	}
}

// Every endpoint reads and changes only what the signed-in account may: its own
// directory entries, Welcomes and approval messages, and groups it belongs to.
func TestE2EEEndpointsAreAccountIsolated(t *testing.T) {
	_, server, _, _ := authenticatedApp(t, func(o *irc.Options) { o.E2EE = true })
	base := server.URL + "/api/v1/e2ee"
	aliceClient, bobClient, malloryClient := cookieClient(), cookieClient(), cookieClient()
	alice, bob := register(t, aliceClient, server.URL), register(t, bobClient, server.URL)
	register(t, malloryClient, server.URL)

	var snapshot irc.Snapshot
	json.Unmarshal(request(t, aliceClient, "GET", server.URL+"/api/v1/bootstrap", nil, 200), &snapshot)
	if c := snapshot.Capabilities.Encryption; c == nil || !c.Experimental || c.MaxPayload != model.MaxEncryptedPayload {
		t.Fatalf("encryption capability = %+v", c)
	}
	anonymous := cookieClient()
	request(t, anonymous, "GET", base+"/identity", nil, 401)

	// Directory: alice publishes; others read only the public parts.
	var identity E2EEIdentity
	json.Unmarshal(request(t, aliceClient, "PUT", base+"/identity", map[string]any{"identityKey": b64("alice-key")}, 200), &identity)
	if identity.Generation != 1 {
		t.Fatalf("identity = %+v", identity)
	}
	request(t, aliceClient, "PUT", base+"/identity", map[string]any{"identityKey": b64("other-key")}, 409)
	request(t, aliceClient, "PUT", base+"/identity", map[string]any{"identityKey": ""}, 422)
	request(t, aliceClient, "PUT", base+"/devices/laptop", map[string]any{"generation": 1, "certificate": b64("cert"), "lastResort": b64("resort"), "label": "Firefox on Linux"}, 204)
	request(t, aliceClient, "PUT", base+"/devices/bad.id", map[string]any{"generation": 1, "certificate": b64("cert")}, 422)
	request(t, aliceClient, "PUT", base+"/devices/laptop", map[string]any{"generation": 1, "certificate": b64("cert"), "label": "bad\x07"}, 422)
	request(t, aliceClient, "POST", base+"/devices/laptop/key-packages", map[string]any{"keyPackages": []string{b64("kp1"), b64("kp2")}}, 200)
	var own E2EEOwnDirectory
	json.Unmarshal(request(t, malloryClient, "GET", base+"/identity", nil, 200), &own)
	if own.Identity != nil || len(own.Devices) != 0 {
		t.Fatalf("mallory sees a directory: %+v", own)
	}
	request(t, malloryClient, "DELETE", base+"/devices/laptop", nil, 404)
	request(t, malloryClient, "POST", base+"/devices/laptop/key-packages", map[string]any{"keyPackages": []string{b64("evil")}}, 404)
	json.Unmarshal(request(t, aliceClient, "GET", base+"/identity", nil, 200), &own)
	if own.Identity == nil || len(own.Devices) != 1 || own.Devices[0].Label != "Firefox on Linux" || own.Devices[0].KeyPackages != 2 || !own.Devices[0].LastResort {
		t.Fatalf("alice's own directory = %+v", own)
	}
	var peer E2EEPeerDirectory
	json.Unmarshal(request(t, bobClient, "GET", base+"/accounts/"+alice.Username, nil, 200), &peer)
	if string(peer.IdentityKey) != "alice-key" || len(peer.Devices) != 1 || peer.Devices[0].DeviceID != "laptop" || strings.Contains(string(request(t, bobClient, "GET", base+"/accounts/"+alice.Username, nil, 200)), "Firefox") {
		t.Fatalf("bob's view of alice = %+v", peer)
	}
	request(t, bobClient, "GET", base+"/accounts/nobody-here", nil, 404)
	request(t, bobClient, "GET", base+"/accounts/"+bob.Username, nil, 404)
	var claim E2EEClaim
	json.Unmarshal(request(t, bobClient, "POST", base+"/accounts/"+alice.Username+"/key-packages/claim", nil, 200), &claim)
	if len(claim.Items) != 1 || string(claim.Items[0].KeyPackage) != "kp1" || claim.Items[0].LastResort {
		t.Fatalf("claim = %+v", claim)
	}

	// Group log: only members read or extend it. Welcomes go to published devices.
	request(t, bobClient, "PUT", base+"/identity", map[string]any{"identityKey": b64("bob-key")}, 200)
	request(t, bobClient, "PUT", base+"/devices/phone", map[string]any{"generation": 1, "certificate": b64("bob-cert")}, 204)
	request(t, aliceClient, "POST", base+"/groups", map[string]any{"groupId": "g0", "members": []string{alice.Username, bob.Username}, "epoch": 1,
		"welcomes": []map[string]any{{"account": bob.Username, "deviceId": "tablet", "welcome": b64("w")}}}, 422)
	group := map[string]any{"groupId": "g1", "members": []string{alice.Username, bob.Username}, "epoch": 1, "groupInfo": b64("info"),
		"welcomes": []map[string]any{{"account": bob.Username, "deviceId": "phone", "welcome": b64("welcome")}}}
	var created E2EEGroup
	json.Unmarshal(request(t, aliceClient, "POST", base+"/groups", group, 200), &created)
	if created.Epoch != 1 || len(created.Members) != 2 || created.CreatedBy != alice.Username {
		t.Fatalf("group = %+v", created)
	}
	request(t, aliceClient, "POST", base+"/groups", group, 409)
	request(t, malloryClient, "POST", base+"/groups", map[string]any{"groupId": "g2", "members": []string{alice.Username, bob.Username}, "epoch": 1}, 422)
	request(t, aliceClient, "POST", base+"/groups", map[string]any{"groupId": "g3", "members": []string{alice.Username}, "epoch": 1,
		"welcomes": []map[string]any{{"account": bob.Username, "deviceId": "phone", "welcome": b64("w")}}}, 422)
	request(t, malloryClient, "GET", base+"/groups/g1", nil, 404)
	request(t, malloryClient, "GET", base+"/groups/g1/commits", nil, 404)
	request(t, malloryClient, "POST", base+"/groups/g1/commits", map[string]any{"epoch": 1, "commit": b64("evil")}, 404)
	var epoch E2EEEpoch
	json.Unmarshal(request(t, bobClient, "POST", base+"/groups/g1/commits", map[string]any{"epoch": 1, "commit": b64("c1")}, 200), &epoch)
	if epoch.Epoch != 2 {
		t.Fatalf("epoch after commit = %d", epoch.Epoch)
	}
	if body := request(t, aliceClient, "POST", base+"/groups/g1/commits", map[string]any{"epoch": 1, "commit": b64("late")}, 409); !strings.Contains(string(body), "epoch 2") {
		t.Fatalf("losing commit: %s", body)
	}
	var commits Items[E2EECommit]
	json.Unmarshal(request(t, aliceClient, "GET", base+"/groups/g1/commits?from=1", nil, 200), &commits)
	if len(commits.Items) != 1 || string(commits.Items[0].Commit) != "c1" || commits.Items[0].Sender != bob.Username {
		t.Fatalf("commits = %+v", commits)
	}

	// Welcomes belong to the addressed account.
	var welcomes Items[E2EEWelcome]
	json.Unmarshal(request(t, malloryClient, "GET", base+"/devices/phone/welcomes", nil, 200), &welcomes)
	if len(welcomes.Items) != 0 {
		t.Fatal("mallory read bob's Welcome")
	}
	json.Unmarshal(request(t, bobClient, "GET", base+"/devices/phone/welcomes", nil, 200), &welcomes)
	if len(welcomes.Items) != 1 || welcomes.Items[0].GroupID != "g1" || string(welcomes.Items[0].Welcome) != "welcome" {
		t.Fatalf("bob's Welcomes = %+v", welcomes)
	}
	welcome := "/welcomes/" + jsonNumber(welcomes.Items[0].ID)
	request(t, malloryClient, "DELETE", base+welcome, nil, 404)
	request(t, bobClient, "DELETE", base+welcome, nil, 204)

	// Approval relay: within one account only.
	var relayed E2EEApprovalID
	json.Unmarshal(request(t, aliceClient, "POST", base+"/approvals", map[string]any{"fromDevice": "laptop", "toDevice": "phone", "payload": b64("handshake")}, 200), &relayed)
	var approvals Items[E2EEApproval]
	json.Unmarshal(request(t, bobClient, "GET", base+"/devices/phone/approvals", nil, 200), &approvals)
	if len(approvals.Items) != 0 {
		t.Fatal("another account read alice's approval message")
	}
	request(t, malloryClient, "DELETE", base+"/approvals/"+jsonNumber(relayed.ID), nil, 404)
	json.Unmarshal(request(t, aliceClient, "GET", base+"/devices/phone/approvals", nil, 200), &approvals)
	if len(approvals.Items) != 1 || string(approvals.Items[0].Payload) != "handshake" {
		t.Fatalf("alice's approval messages = %+v", approvals)
	}
	request(t, aliceClient, "DELETE", base+"/approvals/"+jsonNumber(relayed.ID), nil, 204)

	// Conversation state: only your own direct conversations on the home network.
	request(t, malloryClient, "GET", base+"/conversations/home_0123", nil, 404)
	request(t, malloryClient, "PUT", base+"/conversations/home_0123", map[string]any{"groupId": "g1", "peer": alice.Username}, 404)
	request(t, malloryClient, "DELETE", base+"/conversations/home_0123?groupId=g1", nil, 404)

	// A reset replaces the identity and drops its devices.
	json.Unmarshal(request(t, aliceClient, "PUT", base+"/identity", map[string]any{"identityKey": b64("fresh-key"), "reset": true}, 200), &identity)
	if identity.Generation != 2 {
		t.Fatalf("identity after reset = %+v", identity)
	}
	json.Unmarshal(request(t, bobClient, "GET", base+"/accounts/"+alice.Username, nil, 200), &peer)
	if string(peer.IdentityKey) != "fresh-key" || len(peer.Devices) != 0 {
		t.Fatalf("bob's view of alice after her reset = %+v", peer)
	}
	request(t, aliceClient, "PUT", base+"/devices/laptop", map[string]any{"generation": 1, "certificate": b64("cert")}, 409)
}

func jsonNumber(n int64) string { b, _ := json.Marshal(n); return string(b) }
