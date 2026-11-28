package api

import (
	"encoding/json"
	"strings"
	"testing"

	"ircord/backend/internal/irc"
	"ircord/backend/internal/storage"
)

// Server owners switch encryption on and off without a restart. The mode is
// saved and audited; turning it off refuses new encrypted work but keeps every
// key and group, so turning it on again finds them unchanged.
func TestOwnersSwitchEncryptionAtRunTime(t *testing.T) {
	app, server, store, _ := authenticatedApp(t)
	ownerClient, otherClient := cookieClient(), cookieClient()
	owner := register(t, ownerClient, server.URL)
	register(t, otherClient, server.URL)
	base := server.URL + administrationPrefix + "/encryption"
	setting := func() EncryptionSetting {
		t.Helper()
		var out EncryptionSetting
		if err := json.Unmarshal(request(t, ownerClient, "GET", base, nil, 200), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	capability := func() bool {
		t.Helper()
		var snapshot irc.Snapshot
		json.Unmarshal(request(t, otherClient, "GET", server.URL+"/api/v1/bootstrap", nil, 200), &snapshot)
		return snapshot.Capabilities.Encryption != nil
	}

	// Only owners read or change the mode.
	request(t, ownerClient, "GET", base, nil, 403)
	request(t, ownerClient, "PUT", base, map[string]string{"mode": "experimental"}, 403)
	if err := store.GrantOwner(owner.Account.ID, storage.OwnerFromOperator, "operator"); err != nil {
		t.Fatal(err)
	}
	request(t, otherClient, "GET", base, nil, 403)
	request(t, otherClient, "PUT", base, map[string]string{"mode": "experimental"}, 403)
	request(t, cookieClient(), "GET", base, nil, 401)

	if s := setting(); s.Mode != "off" || s.Locked || s.UpdatedAt != nil || capability() {
		t.Fatalf("starting setting = %+v", s)
	}
	request(t, ownerClient, "PUT", base, map[string]string{"mode": "on"}, 422)

	// On: the capability appears and the key directory opens, with no restart.
	var on EncryptionSetting
	json.Unmarshal(request(t, ownerClient, "PUT", base, map[string]string{"mode": "experimental", "reason": "trying it out"}, 200), &on)
	if on.Mode != "experimental" || on.UpdatedBy != owner.Username || on.UpdatedAt == nil || !app.Manager.E2EEEnabled() || !capability() {
		t.Fatalf("after turning on = %+v", on)
	}
	if saved, err := store.ServerSetting(t.Context(), storage.SettingEncryption); err != nil || saved.Value != "experimental" {
		t.Fatalf("saved mode = %+v, %v", saved, err)
	}
	var identity E2EEIdentity
	json.Unmarshal(request(t, otherClient, "PUT", server.URL+"/api/v1/e2ee/identity", map[string]any{"identityKey": b64("key")}, 200), &identity)

	// Off: new encrypted work is refused, but nothing is deleted.
	request(t, ownerClient, "PUT", base, map[string]string{"mode": "off"}, 200)
	if app.Manager.E2EEEnabled() || capability() {
		t.Fatal("encryption still offered after turning it off")
	}
	if !strings.Contains(string(request(t, otherClient, "GET", server.URL+"/api/v1/e2ee/identity", nil, 404)), "not turned on") {
		t.Fatal("the key directory stayed open with encryption off")
	}
	request(t, otherClient, "PUT", server.URL+"/api/v1/e2ee/identity", map[string]any{"identityKey": b64("key")}, 404)

	// On again: the identity published earlier is still there.
	request(t, ownerClient, "PUT", base, map[string]string{"mode": "experimental"}, 200)
	var own E2EEOwnDirectory
	json.Unmarshal(request(t, otherClient, "GET", server.URL+"/api/v1/e2ee/identity", nil, 200), &own)
	if own.Identity == nil || own.Identity.Generation != identity.Generation {
		t.Fatalf("identity after switching off and on = %+v", own.Identity)
	}

	var audit struct{ Items []storage.OwnerEvent }
	json.Unmarshal(request(t, ownerClient, "GET", server.URL+administrationPrefix+"/audit", nil, 200), &audit)
	saves, withReason := 0, false
	for _, e := range audit.Items {
		if e.Action == "encryption-mode" && e.Actor == owner.Username && e.Outcome == "saved" {
			saves++
			withReason = withReason || e.Target == "experimental" && e.Reason == "trying it out"
		}
	}
	if saves != 3 || !withReason {
		t.Fatalf("audit = %+v", audit.Items)
	}
}

// When IRCORD_E2EE fixes the mode, owners see it and why, but can't change it.
func TestLockedEncryptionMode(t *testing.T) {
	app, server, store, _ := authenticatedApp(t, func(o *irc.Options) { o.E2EE, o.E2EELocked = true, true })
	client := cookieClient()
	owner := register(t, client, server.URL)
	if err := store.GrantOwner(owner.Account.ID, storage.OwnerFromOperator, "operator"); err != nil {
		t.Fatal(err)
	}
	base := server.URL + administrationPrefix + "/encryption"
	var s EncryptionSetting
	json.Unmarshal(request(t, client, "GET", base, nil, 200), &s)
	if s.Mode != "experimental" || !s.Locked || !strings.Contains(s.LockReason, "IRCORD_E2EE") {
		t.Fatalf("locked setting = %+v", s)
	}
	if !strings.Contains(string(request(t, client, "PUT", base, map[string]string{"mode": "off"}, 409)), "IRCORD_E2EE") {
		t.Fatal("the refusal did not name the lock")
	}
	if !app.Manager.E2EEEnabled() {
		t.Fatal("a refused change turned encryption off")
	}
	if saved, err := store.ServerSetting(t.Context(), storage.SettingEncryption); err != nil || saved.Value != "" {
		t.Fatalf("a refused change was saved: %+v, %v", saved, err)
	}
}
