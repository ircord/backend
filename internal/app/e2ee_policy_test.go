package app

import "testing"

// End-to-end encryption is off unless the operator turns it on as experimental.
func TestE2EEPolicy(t *testing.T) {
	for value, want := range map[string]bool{"": false, "off": false, "OFF": false, "experimental": true, " Experimental ": true} {
		got, err := e2eePolicy(value)
		if err != nil || got != want {
			t.Errorf("IRCORD_E2EE=%q: %v, %v; want %v", value, got, err, want)
		}
	}
	for _, value := range []string{"true", "on", "1"} {
		if _, err := e2eePolicy(value); err == nil {
			t.Errorf("IRCORD_E2EE=%q accepted", value)
		}
	}
}
