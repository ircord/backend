package app

import (
	"slices"
	"testing"
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
