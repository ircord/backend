package app

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func policyEnv(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestFileHostPolicyDefaults(t *testing.T) {
	p, err := parseFileHostPolicy(policyEnv(nil))
	if err != nil || p.serve || len(p.accounts) != 0 || p.messageUploads != "local" {
		t.Fatalf("defaults must serve nothing and keep message uploads local: %+v %v", p, err)
	}
}

func TestFileHostPolicySettings(t *testing.T) {
	p, err := parseFileHostPolicy(policyEnv(map[string]string{
		"IRCORD_FILEHOST_SERVE": "true", "IRCORD_FILEHOST_ACCOUNTS": " alice, bob ,,", "IRCORD_MESSAGE_UPLOADS": "network",
	}))
	if err != nil || !p.serve || !slices.Equal(p.accounts, []string{"alice", "bob"}) || p.messageUploads != "network" {
		t.Fatalf("%+v %v", p, err)
	}
	for name, value := range map[string]string{"IRCORD_FILEHOST_SERVE": "yes", "IRCORD_MESSAGE_UPLOADS": "remote"} {
		if _, err = parseFileHostPolicy(policyEnv(map[string]string{name: value})); err == nil {
			t.Fatalf("%s=%s accepted", name, value)
		}
	}
}

// Serving the file host takes an IRC login for the account of the same name, which is only the
// same person when accounts are linked to the IRC server.
func TestFileHostNeedsLinkedAccounts(t *testing.T) {
	for name, settings := range map[string]map[string]string{
		"without Ergo":                    {"IRCORD_FILEHOST_SERVE": "true"},
		"with Ergo accounts switched off": {"IRCORD_FILEHOST_SERVE": "true", "IRCORD_ERGO_ACCOUNTS": "false", "IRCORD_ERGO_SERVER": "irc.example.com:6697", "IRCORD_ERGO_API_URL": "https://127.0.0.1:1", "IRCORD_ERGO_API_TOKEN": strings.Repeat("ab", 32)},
	} {
		t.Run(name, func(t *testing.T) {
			cleanEnvironment(t)
			t.Setenv("IRCORD_DATA_DIR", filepath.Join(t.TempDir(), "state"))
			for k, v := range settings {
				t.Setenv(k, v)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := Run(ctx)
			if err == nil || !strings.Contains(err.Error(), "IRCORD_FILEHOST_SERVE") {
				t.Fatalf("started, or failed for another reason: %v", err)
			}
		})
	}
}
