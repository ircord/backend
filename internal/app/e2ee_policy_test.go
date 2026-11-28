package app

import (
	"testing"

	"ircord/backend/internal/irc"
	"ircord/backend/internal/storage"
)

// Without IRCORD_E2EE the server starts in the mode owners last saved, and off
// when nobody chose one. With it, the saved mode is ignored.
func TestSavedE2EEAtStart(t *testing.T) {
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	start := func(options irc.Options) bool {
		t.Helper()
		if err := savedE2EE(t.Context(), store, &options); err != nil {
			t.Fatal(err)
		}
		return options.E2EE
	}
	if start(irc.Options{}) {
		t.Fatal("encryption on without a saved mode")
	}
	if err = store.SaveServerSetting(t.Context(), storage.SettingEncryption, "experimental", "alice"); err != nil {
		t.Fatal(err)
	}
	if !start(irc.Options{}) {
		t.Fatal("the saved mode was not applied")
	}
	if start(irc.Options{E2EELocked: true}) {
		t.Fatal("IRCORD_E2EE=off was overridden by the saved mode")
	}
}

// Unset, IRCORD_E2EE leaves the choice to server owners, starting off. Set to
// off or experimental, it fixes the mode.
func TestE2EEPolicy(t *testing.T) {
	type policy struct{ on, locked bool }
	for value, want := range map[string]policy{"": {}, "  ": {}, "off": {false, true}, "OFF": {false, true}, "experimental": {true, true}, " Experimental ": {true, true}} {
		on, locked, err := e2eePolicy(value)
		if err != nil || (policy{on, locked}) != want {
			t.Errorf("IRCORD_E2EE=%q: %v, %v, %v; want %+v", value, on, locked, err, want)
		}
	}
	for _, value := range []string{"true", "on", "1"} {
		if _, _, err := e2eePolicy(value); err == nil {
			t.Errorf("IRCORD_E2EE=%q accepted", value)
		}
	}
}
