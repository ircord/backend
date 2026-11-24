package app

import (
	"fmt"
	"strings"

	"ircord/backend/internal/auth"
)

// Message uploads from this server can go to the IRC network's own file host
// instead of this server's storage.
const (
	messageUploadsLocal   = "local"
	messageUploadsNetwork = "network"
)

// fileHostPolicy is the operator's choices about the IRC network's file host,
// the draft/FILEHOST standard.
type fileHostPolicy struct {
	// serve makes this server the network's file host (IRCORD_FILEHOST_SERVE).
	// It needs file storage and the associated IRC server, which checks logins.
	serve bool
	// accounts limits who may upload to it (IRCORD_FILEHOST_ACCOUNTS). Empty
	// means every account on the network, within its normal quota.
	accounts []string
	// messageUploads is where this server sends the files people attach to
	// messages (IRCORD_MESSAGE_UPLOADS): local, or network.
	messageUploads string
}

func parseFileHostPolicy(get func(string) string) (fileHostPolicy, error) {
	p := fileHostPolicy{messageUploads: messageUploadsLocal}
	switch value := strings.TrimSpace(get("IRCORD_FILEHOST_SERVE")); value {
	case "", "false":
	case "true":
		p.serve = true
	default:
		return fileHostPolicy{}, fmt.Errorf("IRCORD_FILEHOST_SERVE must be true or false")
	}
	for _, name := range strings.Split(get("IRCORD_FILEHOST_ACCOUNTS"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			if problem := auth.UsernameProblem(name); problem != "" {
				return fileHostPolicy{}, fmt.Errorf("IRCORD_FILEHOST_ACCOUNTS names %q, which cannot be an account: %s", name, problem)
			}
			p.accounts = append(p.accounts, name)
		}
	}
	if value := strings.TrimSpace(get("IRCORD_MESSAGE_UPLOADS")); value != "" {
		if value != messageUploadsLocal && value != messageUploadsNetwork {
			return fileHostPolicy{}, fmt.Errorf("IRCORD_MESSAGE_UPLOADS must be local or network")
		}
		p.messageUploads = value
	}
	return p, nil
}
