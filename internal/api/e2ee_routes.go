package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"ircord/backend/internal/irc"
	"ircord/backend/internal/storage"
)

// The end-to-end encryption API (ircord/issues#19, experimental). The server
// keeps a key directory, an ordered group log per MLS group, the binding of
// each account's direct conversations to their current group, and a short-lived
// relay for device approval. Every key, certificate, key package, commit,
// Welcome, group info and relay message is opaque: the server checks who may
// read or write it and how large it is, and never what it contains. It never
// holds a private key.

// Size bounds of the opaque values, in bytes after base64 decoding.
const (
	e2eeMaxIdentityKey = 1024
	e2eeMaxCertificate = 4096
	e2eeMaxKeyPackage  = 4096
	e2eeMaxMLSObject   = 64 << 10
	e2eeMaxUpload      = 100
	e2eeMaxWelcomes    = 64
)

const (
	e2eeIDPattern = "^[A-Za-z0-9_-]+$"
	e2eeDeviceDoc = "Client-chosen device ID, for example derived from the device key: 1 to 64 base64url characters."
	e2eeGroupDoc  = "The MLS group ID in base64url, chosen by the creating client: 1 to 128 characters."
)

type E2EEIdentity struct {
	IdentityKey []byte    `json:"identityKey" doc:"The account's identity public key, as its client encoded it."`
	Generation  int64     `json:"generation" doc:"Increases with every reset. Device certificates name the generation they were made under."`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type E2EEDevice struct {
	DeviceID    string    `json:"deviceId"`
	Certificate []byte    `json:"certificate" doc:"The device certificate, signed by the identity key. Clients check the signature; the server does not."`
	Label       string    `json:"label,omitempty" doc:"A name for the device that only the account's own devices see, such as Firefox on Linux."`
	KeyPackages int       `json:"keyPackages" doc:"Single-use key packages in stock."`
	LastResort  bool      `json:"lastResort" doc:"A reusable last-resort key package is stored."`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type E2EEOwnDirectory struct {
	Identity   *E2EEIdentity   `json:"identity,omitempty" doc:"Absent until the account's first device publishes an identity key."`
	Devices    []E2EEDevice    `json:"devices" nullable:"false"`
	DeviceList *E2EEDeviceList `json:"deviceList,omitempty" doc:"The newest signed device list your devices published."`
	Elsewhere  bool            `json:"elsewhere" doc:"Encryption for this account is set up on another IRCord server, and this one has no keys for it. Encrypted chats are readable only through that server; this server refuses to set up a second identity. This is the server's last answer: it asks again in the background and sends e2ee.changed when it finds keys elsewhere."`
}

type E2EEDeviceList struct {
	List    []byte `json:"list" doc:"The list of the account's current device IDs, signed by its identity key. Clients check the signature; the server does not."`
	Counter int64  `json:"counter" doc:"Goes up with every new list. Clients refuse a list older than the newest one they have seen."`
}

type E2EEDeviceListInput struct {
	Body E2EEDeviceList
}

type E2EEIdentityInput struct {
	Body struct {
		IdentityKey []byte `json:"identityKey" doc:"The identity public key."`
		Reset       bool   `json:"reset,omitempty" doc:"Start fresh: replace a different existing identity key. This removes every device, key package, waiting Welcome and approval message of the old identity."`
	}
}

type E2EEDevicePath struct {
	DeviceID string `path:"deviceId" minLength:"1" maxLength:"64" pattern:"^[A-Za-z0-9_-]+$" doc:"Client-chosen device ID, for example derived from the device key: 1 to 64 base64url characters."`
}

type E2EEDeviceInput struct {
	E2EEDevicePath
	Body struct {
		Generation  int64  `json:"generation" minimum:"1" doc:"The identity generation the certificate was made under. A certificate for an older generation is refused."`
		Certificate []byte `json:"certificate"`
		LastResort  []byte `json:"lastResort,omitempty" doc:"A reusable key package handed out when the device has no single-use package left. Omit to keep the stored one."`
		Label       string `json:"label,omitempty" maxLength:"100"`
	}
}

type E2EEKeyPackagesInput struct {
	E2EEDevicePath
	Body struct {
		KeyPackages [][]byte `json:"keyPackages" minItems:"1" maxItems:"100" nullable:"false"`
	}
}

type E2EEStock struct {
	KeyPackages int `json:"keyPackages" doc:"Single-use key packages now in stock for the device."`
}

type E2EEAccountPath struct {
	Account string `path:"account" minLength:"1" maxLength:"100" doc:"The person's account name on this server's home network."`
}

type E2EEPeerDevice struct {
	DeviceID    string `json:"deviceId"`
	Certificate []byte `json:"certificate"`
	KeyPackages int    `json:"keyPackages"`
	LastResort  bool   `json:"lastResort"`
}

type E2EEPeerDirectory struct {
	Account     string           `json:"account"`
	IdentityKey []byte           `json:"identityKey"`
	Generation  int64            `json:"generation"`
	Devices     []E2EEPeerDevice `json:"devices" nullable:"false"`
	DeviceList  *E2EEDeviceList  `json:"deviceList,omitempty" doc:"The person's newest signed device list, when their devices published one."`
	Source      string           `json:"source" enum:"server,network" doc:"server: this IRCord server holds the person's keys. network: the person's own IRCord server answered over IRC, so the keys passed through it and the IRC network; compare security codes to be sure."`
}

type E2EEClaimedPackage struct {
	DeviceID    string `json:"deviceId"`
	Certificate []byte `json:"certificate"`
	KeyPackage  []byte `json:"keyPackage"`
	LastResort  bool   `json:"lastResort" doc:"The device had no single-use package left; this is its reusable last-resort package."`
}

type E2EEClaim struct {
	Account     string               `json:"account"`
	IdentityKey []byte               `json:"identityKey"`
	Generation  int64                `json:"generation"`
	Items       []E2EEClaimedPackage `json:"items" nullable:"false" doc:"One key package for each device that has one. Single-use packages are removed from the directory."`
	DeviceList  *E2EEDeviceList      `json:"deviceList,omitempty"`
	Source      string               `json:"source" enum:"server,network" doc:"Where the keys came from; see the person's directory."`
}

type E2EEWelcomeInput struct {
	Account  string `json:"account" minLength:"1" maxLength:"100" doc:"Member account the device belongs to."`
	DeviceID string `json:"deviceId" minLength:"1" maxLength:"64" pattern:"^[A-Za-z0-9_-]+$"`
	Welcome  []byte `json:"welcome"`
}

type E2EEGroupPath struct {
	GroupID string `path:"groupId" minLength:"1" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$" doc:"The MLS group ID in base64url, chosen by the creating client: 1 to 128 characters."`
}

type E2EECreateGroupInput struct {
	Body struct {
		GroupID   string             `json:"groupId" minLength:"1" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$" doc:"The MLS group ID in base64url, chosen by the creating client: 1 to 128 characters."`
		Members   []string           `json:"members" minItems:"1" maxItems:"32" nullable:"false" doc:"Account names allowed to read and extend the group log, including your own. A direct conversation's group has exactly the two people."`
		Epoch     int64              `json:"epoch" minimum:"0" doc:"The epoch the creator reached, usually 1 after the commit that added the other devices."`
		GroupInfo []byte             `json:"groupInfo,omitempty"`
		Welcomes  []E2EEWelcomeInput `json:"welcomes,omitempty" maxItems:"64" nullable:"false"`
	}
}

type E2EEGroup struct {
	GroupID   string    `json:"groupId"`
	Epoch     int64     `json:"epoch" doc:"The current epoch. The next commit must name it."`
	Members   []string  `json:"members" nullable:"false"`
	GroupInfo []byte    `json:"groupInfo,omitempty" doc:"The newest group info, which an approved new device uses to join by external commit."`
	CreatedBy string    `json:"createdBy,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type E2EECommitInput struct {
	E2EEGroupPath
	Body struct {
		Epoch     int64              `json:"epoch" minimum:"0" doc:"The epoch the commit was made in. It must be the group's current epoch."`
		Commit    []byte             `json:"commit"`
		GroupInfo []byte             `json:"groupInfo,omitempty" doc:"The group info after the commit; omit to keep the stored one."`
		Welcomes  []E2EEWelcomeInput `json:"welcomes,omitempty" maxItems:"64" nullable:"false"`
	}
}

type E2EEEpoch struct {
	Epoch int64 `json:"epoch" doc:"The group's epoch after the commit."`
}

type E2EECommitsInput struct {
	E2EEGroupPath
	From  int64 `query:"from" minimum:"0" doc:"The epoch this device is at; the commit made in that epoch comes first."`
	Limit int   `query:"limit" default:"100" minimum:"1" maximum:"100"`
}

type E2EECommit struct {
	Epoch     int64     `json:"epoch" doc:"The epoch the commit was made in."`
	Sender    string    `json:"sender,omitempty" doc:"Account that posted it. Clients trust the MLS sender, not this."`
	Commit    []byte    `json:"commit"`
	CreatedAt time.Time `json:"createdAt"`
}

type E2EEWelcome struct {
	ID        int64     `json:"id"`
	GroupID   string    `json:"groupId"`
	Epoch     int64     `json:"epoch"`
	Welcome   []byte    `json:"welcome"`
	CreatedAt time.Time `json:"createdAt"`
}

type E2EEItemPath struct {
	ID int64 `path:"id" minimum:"1"`
}

type E2EEConversationPath struct {
	ID string `path:"id" maxLength:"200" doc:"Your direct conversation's ID."`
}

type E2EEConversation struct {
	ConversationID string `json:"conversationId"`
	Encrypted      bool   `json:"encrypted" doc:"The backend refuses plaintext into this conversation."`
	GroupID        string `json:"groupId,omitempty" doc:"The conversation's current group, shared by both people."`
	Peer           string `json:"peer,omitempty" doc:"The other person's account name."`
}

type E2EEBindInput struct {
	E2EEConversationPath
	Body struct {
		GroupID  string `json:"groupId" minLength:"1" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$"`
		Peer     string `json:"peer" minLength:"1" maxLength:"100" doc:"The other person's account name. The group must have exactly you and them as members."`
		Replaces string `json:"replaces,omitempty" maxLength:"128" doc:"The current group this one replaces, after one of you started fresh. Omit when starting encryption or joining the current group."`
	}
}

type E2EETurnOffInput struct {
	E2EEConversationPath
	GroupID string `query:"groupId" minLength:"1" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$" doc:"The group you are turning off; must still be current."`
}

type E2EEApprovalInput struct {
	Body struct {
		FromDevice string `json:"fromDevice" minLength:"1" maxLength:"64" pattern:"^[A-Za-z0-9_-]+$"`
		ToDevice   string `json:"toDevice" minLength:"1" maxLength:"64" pattern:"^[A-Za-z0-9_-]+$"`
		Payload    []byte `json:"payload" doc:"A handshake message encrypted to the receiving device."`
	}
}

type E2EEApprovalID struct {
	ID int64 `json:"id"`
}

type E2EEApproval struct {
	ID         int64     `json:"id"`
	FromDevice string    `json:"fromDevice"`
	ToDevice   string    `json:"toDevice"`
	Payload    []byte    `json:"payload"`
	CreatedAt  time.Time `json:"createdAt"`
}

// e2eeStore returns the store when the operator turned encryption on.
func (s *Server) e2eeStore() (*storage.Store, error) {
	if !s.Manager.E2EEEnabled() {
		return nil, commandFailure(irc.ErrEncryptionOff)
	}
	return s.options.Store, nil
}

// e2eeFailure translates directory and group log errors at the HTTP boundary.
func e2eeFailure(err error) error {
	var epoch storage.E2EEEpochConflict
	var group storage.E2EEGroupConflict
	switch {
	case err == nil:
		return nil
	case errors.As(err, &epoch):
		return problem(409, fmt.Sprintf("The group has moved on to epoch %d. Apply its newer commits, then try again.", epoch.Epoch), problemResourceConflict)
	case errors.As(err, &group):
		if group.Current == "" {
			return problem(409, "The conversation has no current group to replace. Start encryption without replaces.", problemResourceConflict)
		}
		return problem(409, "The conversation already has another current group. Join it, or replace it by naming it.", problemResourceConflict)
	case errors.Is(err, storage.ErrE2EEConflict):
		return problem(409, "The encryption state changed. Read it again, then try again.", problemResourceConflict)
	case errors.Is(err, storage.ErrE2EEWelcomeTarget):
		return huma.Error422UnprocessableEntity("each Welcome must be for a published device of a group member")
	case errors.Is(err, storage.ErrE2EELimit):
		return problem(409, "The limit for this account is reached. Remove unused devices or wait for pending items to expire.", problemResourceConflict)
	case errors.Is(err, storage.ErrNotFound):
		return huma.Error404NotFound("Not found.")
	default:
		return commandFailure(irc.ErrStorage)
	}
}

func tooLarge(name string, limit int) error {
	return huma.Error422UnprocessableEntity(fmt.Sprintf("%s must be 1 to %d bytes", name, limit))
}

func blobOK(b []byte, limit int) bool { return len(b) > 0 && len(b) <= limit }

// e2eeOwners resolves account names on this server to account IDs. An unknown
// name is not found, so the directory can't be used to tell accounts apart.
func e2eeOwner(store *storage.Store, name string) (string, error) {
	account, _, err := store.AccountByUsername(strings.TrimSpace(name))
	if err != nil {
		return "", err
	}
	return account.ID, nil
}

func (s *Server) e2eeWelcomes(store *storage.Store, members map[string]bool, in []E2EEWelcomeInput) ([]storage.E2EEWelcomeInput, error) {
	out := make([]storage.E2EEWelcomeInput, 0, len(in))
	for _, w := range in {
		if !blobOK(w.Welcome, e2eeMaxMLSObject) {
			return nil, tooLarge("welcome", e2eeMaxMLSObject)
		}
		id, err := e2eeOwner(store, w.Account)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, e2eeFailure(err)
		}
		if err != nil || !members[id] {
			return nil, huma.Error422UnprocessableEntity("each Welcome must be for a device of a group member")
		}
		out = append(out, storage.E2EEWelcomeInput{Owner: id, Device: w.DeviceID, Welcome: w.Welcome})
	}
	return out, nil
}

func (s *Server) e2eeConversation(ctx context.Context, store *storage.Store, conversation string) (E2EEConversation, error) {
	if _, err := s.Manager.E2EEDirect(owner(ctx), conversation); err != nil {
		return E2EEConversation{}, commandFailure(err)
	}
	out := E2EEConversation{ConversationID: conversation}
	state, err := store.E2EEConversationState(owner(ctx), conversation)
	if errors.Is(err, storage.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return E2EEConversation{}, e2eeFailure(err)
	}
	names, err := store.E2EEAccountNames([]string{state.Peer})
	if err != nil {
		return E2EEConversation{}, e2eeFailure(err)
	}
	out.Encrypted, out.GroupID, out.Peer = state.Group != "", state.Group, names[state.Peer]
	return out, nil
}

func (s *Server) registerE2EE() {
	const tag = "End-to-end encryption"
	op := func(id, method, path, summary, description string) huma.Operation {
		return huma.Operation{OperationID: id, Method: method, Path: path, Summary: summary, Description: description + " Experimental; answers 404 while the server operator has not turned encryption on.", Tags: []string{tag}}
	}
	noContent := func(o huma.Operation) huma.Operation { o.DefaultStatus = 204; return o }

	huma.Register(s.API, op("get-e2ee-directory", "GET", "/api/v1/e2ee/identity", "Read your encryption identity and devices",
		"Your identity public key and every device published under it, with each device's key package stock."),
		func(ctx context.Context, _ *struct{}) (*Output[E2EEOwnDirectory], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			out := E2EEOwnDirectory{Devices: []E2EEDevice{}}
			identity, err := store.E2EEIdentity(owner(ctx))
			if err == nil {
				out.Identity = &E2EEIdentity{IdentityKey: identity.IdentityKey, Generation: identity.Generation, UpdatedAt: identity.UpdatedAt}
			} else if !errors.Is(err, storage.ErrNotFound) {
				return nil, e2eeFailure(err)
			}
			devices, err := store.E2EEDevices(owner(ctx))
			if err != nil {
				return nil, e2eeFailure(err)
			}
			for _, d := range devices {
				out.Devices = append(out.Devices, E2EEDevice{DeviceID: d.ID, Certificate: d.Certificate, Label: d.Label, KeyPackages: d.KeyPackages, LastResort: d.LastResort, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt})
			}
			out.DeviceList, err = e2eeDeviceList(store, owner(ctx))
			if err != nil {
				return nil, e2eeFailure(err)
			}
			if out.Identity == nil {
				out.Elsewhere = s.Manager.E2EEElsewhereHint(owner(ctx))
			}
			return &Output[E2EEOwnDirectory]{Body: out}, nil
		})

	huma.Register(s.API, op("put-e2ee-device-list", "PUT", "/api/v1/e2ee/device-list", "Publish your signed device list",
		"Stores the list of your current device IDs, signed by your identity key, and tells your encrypted chats that it changed. The counter must go up; publishing the same list again changes nothing. Others see the list in your directory and refuse an older one, so a signed-out device can't be passed off as current."),
		func(ctx context.Context, in *E2EEDeviceListInput) (*Output[E2EEDeviceList], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			if !blobOK(in.Body.List, e2eeMaxCertificate) {
				return nil, tooLarge("list", e2eeMaxCertificate)
			}
			if in.Body.Counter < 1 {
				return nil, huma.Error422UnprocessableEntity("counter must be at least 1")
			}
			changed, err := store.PutE2EEDeviceList(owner(ctx), in.Body.List, in.Body.Counter)
			if errors.Is(err, storage.ErrE2EEConflict) {
				return nil, problem(409, "Your devices already published a newer list, or this account has no identity yet. Read your directory, then try again.", problemResourceConflict)
			}
			if err != nil {
				return nil, e2eeFailure(err)
			}
			if changed {
				s.Manager.E2EEDeviceListChanged(owner(ctx))
			}
			return &Output[E2EEDeviceList]{Body: in.Body}, nil
		})

	huma.Register(s.API, op("publish-e2ee-identity", "PUT", "/api/v1/e2ee/identity", "Publish your identity key, or start fresh",
		"The first device of an account publishes its identity public key. Publishing the same key again changes nothing. A different key is refused unless reset is set: then the old identity, its devices, key packages, waiting Welcomes and approval messages are removed, and peers see the new key."),
		func(ctx context.Context, in *E2EEIdentityInput) (*Output[E2EEIdentity], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			if !blobOK(in.Body.IdentityKey, e2eeMaxIdentityKey) {
				return nil, tooLarge("identityKey", e2eeMaxIdentityKey)
			}
			before, err := store.E2EEIdentity(owner(ctx))
			if errors.Is(err, storage.ErrNotFound) {
				// One account, one identity: a second IRCord server would look
				// like an identity change to everyone.
				elsewhere, err := s.Manager.E2EEElsewhere(ctx, owner(ctx))
				if err != nil {
					return nil, commandFailure(err)
				}
				if elsewhere {
					return nil, commandFailure(irc.ErrE2EEElsewhere)
				}
			} else if err != nil {
				return nil, e2eeFailure(err)
			}
			v, err := store.PublishE2EEIdentity(owner(ctx), in.Body.IdentityKey, in.Body.Reset)
			if err != nil {
				return nil, e2eeFailure(err)
			}
			if v.Generation != before.Generation {
				s.Manager.E2EEIdentityPublished(owner(ctx), before.Generation != 0)
			}
			return &Output[E2EEIdentity]{Body: E2EEIdentity{IdentityKey: v.IdentityKey, Generation: v.Generation, UpdatedAt: v.UpdatedAt}}, nil
		})

	huma.Register(s.API, noContent(op("put-e2ee-device", "PUT", "/api/v1/e2ee/devices/{deviceId}", "Publish a device certificate",
		"Adds or updates one of your devices under the current identity generation. A device approved before a reset is refused. An account can have 32 devices.")),
		func(ctx context.Context, in *E2EEDeviceInput) (*struct{}, error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			if !blobOK(in.Body.Certificate, e2eeMaxCertificate) {
				return nil, tooLarge("certificate", e2eeMaxCertificate)
			}
			if len(in.Body.LastResort) > e2eeMaxKeyPackage {
				return nil, tooLarge("lastResort", e2eeMaxKeyPackage)
			}
			if !cleanLabel(in.Body.Label) {
				return nil, huma.Error422UnprocessableEntity("label has characters that can't be shown")
			}
			return nil, e2eeFailure(store.PutE2EEDevice(owner(ctx), in.DeviceID, in.Body.Generation, in.Body.Certificate, in.Body.LastResort, in.Body.Label))
		})

	huma.Register(s.API, noContent(op("delete-e2ee-device", "DELETE", "/api/v1/e2ee/devices/{deviceId}", "Remove a device",
		"Removes one of your devices with its key packages, waiting Welcomes and approval messages. Your other devices then remove it from every group, which rotates the keys.")),
		func(ctx context.Context, in *E2EEDevicePath) (*struct{}, error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			if err = store.DeleteE2EEDevice(owner(ctx), in.DeviceID); err != nil {
				return nil, e2eeFailure(err)
			}
			s.Manager.E2EEDeviceListChanged(owner(ctx))
			return nil, nil
		})

	huma.Register(s.API, op("add-e2ee-key-packages", "POST", "/api/v1/e2ee/devices/{deviceId}/key-packages", "Add key packages for a device",
		"Adds single-use key packages so others can add the device to a group while it is offline. A device keeps at most 100."),
		func(ctx context.Context, in *E2EEKeyPackagesInput) (*Output[E2EEStock], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			for _, p := range in.Body.KeyPackages {
				if !blobOK(p, e2eeMaxKeyPackage) {
					return nil, tooLarge("keyPackages[]", e2eeMaxKeyPackage)
				}
			}
			stock, err := store.AddE2EEKeyPackages(owner(ctx), in.DeviceID, in.Body.KeyPackages)
			if err != nil {
				return nil, e2eeFailure(err)
			}
			return &Output[E2EEStock]{Body: E2EEStock{KeyPackages: stock}}, nil
		})

	huma.Register(s.API, op("get-e2ee-account", "GET", "/api/v1/e2ee/accounts/{account}", "Read a person's encryption directory",
		"The identity public key, signed device list and device certificates of anyone on the home network. When this server holds the person's keys it answers itself; otherwise it asks over IRC, and the person's own IRCord server answers (source network). A client starts an encrypted chat only when the person has a device, and checks each certificate against the identity key it pinned. Labels are left out. 404 means the person hasn't set up encryption or their server doesn't support encrypted chats with other servers yet; 504 means their server did not answer."),
		func(ctx context.Context, in *E2EEAccountPath) (*Output[E2EEPeerDirectory], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			id, err := e2eeOwner(store, in.Account)
			var identity storage.E2EEIdentity
			if err == nil {
				identity, err = store.E2EEIdentity(id)
			}
			if errors.Is(err, storage.ErrNotFound) {
				remote, err := s.Manager.E2EERemoteDirectory(ctx, owner(ctx), in.Account, false)
				if err != nil {
					return nil, e2eeRemoteFailure(err)
				}
				out := E2EEPeerDirectory{Account: remote.Account, IdentityKey: remote.IdentityKey, Generation: remote.Generation, Devices: []E2EEPeerDevice{}, Source: "network"}
				if len(remote.DeviceList) > 0 {
					out.DeviceList = &E2EEDeviceList{List: remote.DeviceList, Counter: remote.ListCounter}
				}
				for _, d := range remote.Devices {
					out.Devices = append(out.Devices, E2EEPeerDevice{DeviceID: d.ID, Certificate: d.Certificate})
				}
				return &Output[E2EEPeerDirectory]{Body: out}, nil
			}
			if err != nil {
				return nil, e2eeFailure(err)
			}
			devices, err := store.E2EEDevices(id)
			if err != nil {
				return nil, e2eeFailure(err)
			}
			out := E2EEPeerDirectory{Account: in.Account, IdentityKey: identity.IdentityKey, Generation: identity.Generation, Devices: []E2EEPeerDevice{}, Source: "server"}
			if out.DeviceList, err = e2eeDeviceList(store, id); err != nil {
				return nil, e2eeFailure(err)
			}
			for _, d := range devices {
				out.Devices = append(out.Devices, E2EEPeerDevice{DeviceID: d.ID, Certificate: d.Certificate, KeyPackages: d.KeyPackages, LastResort: d.LastResort})
			}
			return &Output[E2EEPeerDirectory]{Body: out}, nil
		})

	huma.Register(s.API, op("claim-e2ee-key-packages", "POST", "/api/v1/e2ee/accounts/{account}/key-packages/claim", "Claim key packages to add a person's devices",
		"Takes one key package for each of the person's devices, or that device's last-resort package when it has no other. Your own account can be claimed too, to add your other devices. For someone whose keys are on another IRCord server, the request goes over IRC; that server hands out at most 10 single-use packages a minute to one account, then only last-resort ones."),
		func(ctx context.Context, in *E2EEAccountPath) (*Output[E2EEClaim], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			id, err := e2eeOwner(store, in.Account)
			var identity storage.E2EEIdentity
			var claims []storage.E2EEClaim
			if err == nil {
				identity, claims, err = store.ClaimE2EEKeyPackages(id)
			}
			if errors.Is(err, storage.ErrNotFound) {
				remote, err := s.Manager.E2EERemoteDirectory(ctx, owner(ctx), in.Account, true)
				if err != nil {
					return nil, e2eeRemoteFailure(err)
				}
				out := E2EEClaim{Account: remote.Account, IdentityKey: remote.IdentityKey, Generation: remote.Generation, Items: []E2EEClaimedPackage{}, Source: "network"}
				if len(remote.DeviceList) > 0 {
					out.DeviceList = &E2EEDeviceList{List: remote.DeviceList, Counter: remote.ListCounter}
				}
				for _, d := range remote.Devices {
					if len(d.KeyPackage) > 0 {
						out.Items = append(out.Items, E2EEClaimedPackage{DeviceID: d.ID, Certificate: d.Certificate, KeyPackage: d.KeyPackage, LastResort: d.LastResort})
					}
				}
				return &Output[E2EEClaim]{Body: out}, nil
			}
			if err != nil {
				return nil, e2eeFailure(err)
			}
			out := E2EEClaim{Account: in.Account, IdentityKey: identity.IdentityKey, Generation: identity.Generation, Items: []E2EEClaimedPackage{}, Source: "server"}
			if out.DeviceList, err = e2eeDeviceList(store, id); err != nil {
				return nil, e2eeFailure(err)
			}
			for _, c := range claims {
				out.Items = append(out.Items, E2EEClaimedPackage{DeviceID: c.Device, Certificate: c.Certificate, KeyPackage: c.KeyPackage, LastResort: c.LastResort})
			}
			return &Output[E2EEClaim]{Body: out}, nil
		})

	groupOut := func(store *storage.Store, g storage.E2EEGroup) (*Output[E2EEGroup], error) {
		names, err := store.E2EEAccountNames(append(append([]string{}, g.Members...), g.CreatedBy))
		if err != nil {
			return nil, e2eeFailure(err)
		}
		out := E2EEGroup{GroupID: g.ID, Epoch: g.Epoch, GroupInfo: g.GroupInfo, CreatedBy: names[g.CreatedBy], CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt, Members: []string{}}
		for _, member := range g.Members {
			if name, ok := names[member]; ok {
				out.Members = append(out.Members, name)
			}
		}
		return &Output[E2EEGroup]{Body: out}, nil
	}

	huma.Register(s.API, op("create-e2ee-group", "POST", "/api/v1/e2ee/groups", "Create a group log",
		"Registers a new MLS group at the epoch its creator reached, the member accounts that may use its log, and the creator's Welcomes for the other devices. A group ID can't be reused."),
		func(ctx context.Context, in *E2EECreateGroupInput) (*Output[E2EEGroup], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			if len(in.Body.GroupInfo) > e2eeMaxMLSObject {
				return nil, tooLarge("groupInfo", e2eeMaxMLSObject)
			}
			members := map[string]bool{}
			ids := []string{}
			for _, name := range in.Body.Members {
				id, err := e2eeOwner(store, name)
				if errors.Is(err, storage.ErrNotFound) {
					return nil, huma.Error422UnprocessableEntity("each member must be an account on this server")
				}
				if err != nil {
					return nil, e2eeFailure(err)
				}
				if !members[id] {
					members[id] = true
					ids = append(ids, id)
				}
			}
			if !members[owner(ctx)] {
				return nil, huma.Error422UnprocessableEntity("members must include your own account")
			}
			welcomes, err := s.e2eeWelcomes(store, members, in.Body.Welcomes)
			if err != nil {
				return nil, err
			}
			g := storage.E2EEGroup{ID: in.Body.GroupID, Epoch: in.Body.Epoch, GroupInfo: in.Body.GroupInfo, Members: ids, CreatedBy: owner(ctx)}
			if err = store.CreateE2EEGroup(g, welcomes); err != nil {
				return nil, e2eeFailure(err)
			}
			created, err := store.E2EEGroup(owner(ctx), g.ID)
			if err != nil {
				return nil, e2eeFailure(err)
			}
			return groupOut(store, created)
		})

	huma.Register(s.API, op("get-e2ee-group", "GET", "/api/v1/e2ee/groups/{groupId}", "Read a group",
		"The group's current epoch, members and newest group info. Only member accounts can read it."),
		func(ctx context.Context, in *E2EEGroupPath) (*Output[E2EEGroup], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			g, err := store.E2EEGroup(owner(ctx), in.GroupID)
			if err != nil {
				return nil, e2eeFailure(err)
			}
			return groupOut(store, g)
		})

	huma.Register(s.API, op("post-e2ee-commit", "POST", "/api/v1/e2ee/groups/{groupId}/commits", "Add a commit to a group log",
		"Accepted only if epoch is the group's current epoch, which then advances by one. Of two commits for the same epoch exactly one is accepted; the other gets 409 and its sender applies the newer commits and tries again. Welcomes for added devices are queued with the commit."),
		func(ctx context.Context, in *E2EECommitInput) (*Output[E2EEEpoch], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			if !blobOK(in.Body.Commit, e2eeMaxMLSObject) {
				return nil, tooLarge("commit", e2eeMaxMLSObject)
			}
			if len(in.Body.GroupInfo) > e2eeMaxMLSObject {
				return nil, tooLarge("groupInfo", e2eeMaxMLSObject)
			}
			g, err := store.E2EEGroup(owner(ctx), in.GroupID)
			if err != nil {
				return nil, e2eeFailure(err)
			}
			members := map[string]bool{}
			for _, member := range g.Members {
				members[member] = true
			}
			welcomes, err := s.e2eeWelcomes(store, members, in.Body.Welcomes)
			if err != nil {
				return nil, err
			}
			epoch, err := store.AppendE2EECommit(owner(ctx), in.GroupID, in.Body.Epoch, in.Body.Commit, in.Body.GroupInfo, welcomes)
			if err != nil {
				return nil, e2eeFailure(err)
			}
			return &Output[E2EEEpoch]{Body: E2EEEpoch{Epoch: epoch}}, nil
		})

	huma.Register(s.API, op("list-e2ee-commits", "GET", "/api/v1/e2ee/groups/{groupId}/commits", "List a group's commits",
		"Commits from an epoch on, oldest first. A device that was offline applies them in this order. The server keeps a group's last 1,000 commits."),
		func(ctx context.Context, in *E2EECommitsInput) (*Output[Items[E2EECommit]], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			commits, err := store.E2EECommits(owner(ctx), in.GroupID, in.From, in.Limit)
			if err != nil {
				return nil, e2eeFailure(err)
			}
			senders := make([]string, 0, len(commits))
			for _, c := range commits {
				senders = append(senders, c.Sender)
			}
			names, err := store.E2EEAccountNames(senders)
			if err != nil {
				return nil, e2eeFailure(err)
			}
			out := Items[E2EECommit]{Items: []E2EECommit{}}
			for _, c := range commits {
				out.Items = append(out.Items, E2EECommit{Epoch: c.Epoch, Sender: names[c.Sender], Commit: c.Commit, CreatedAt: c.CreatedAt})
			}
			return &Output[Items[E2EECommit]]{Body: out}, nil
		})

	huma.Register(s.API, op("list-e2ee-welcomes", "GET", "/api/v1/e2ee/devices/{deviceId}/welcomes", "List Welcomes for a device",
		"Welcomes that add this device of yours to groups, oldest first. Remove each one after processing it. Unprocessed Welcomes expire after 30 days."),
		func(ctx context.Context, in *E2EEDevicePath) (*Output[Items[E2EEWelcome]], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			welcomes, err := store.E2EEWelcomes(owner(ctx), in.DeviceID)
			if err != nil {
				return nil, e2eeFailure(err)
			}
			out := Items[E2EEWelcome]{Items: []E2EEWelcome{}}
			for _, w := range welcomes {
				out.Items = append(out.Items, E2EEWelcome{ID: w.ID, GroupID: w.GroupID, Epoch: w.Epoch, Welcome: w.Welcome, CreatedAt: w.CreatedAt})
			}
			return &Output[Items[E2EEWelcome]]{Body: out}, nil
		})

	huma.Register(s.API, noContent(op("delete-e2ee-welcome", "DELETE", "/api/v1/e2ee/welcomes/{id}", "Remove a processed Welcome", "Removes one of your devices' Welcomes.")),
		func(ctx context.Context, in *E2EEItemPath) (*struct{}, error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			return nil, e2eeFailure(store.DeleteE2EEWelcome(owner(ctx), in.ID))
		})

	huma.Register(s.API, op("get-e2ee-conversation", "GET", "/api/v1/e2ee/conversations/{id}", "Read a direct conversation's encryption state",
		"Whether the backend refuses plaintext into your direct conversation, and its current group. Clients keep their own list of encrypted conversations and do not rely on this."),
		func(ctx context.Context, in *E2EEConversationPath) (*Output[E2EEConversation], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			out, err := s.e2eeConversation(ctx, store, in.ID)
			if err != nil {
				return nil, err
			}
			return &Output[E2EEConversation]{Body: out}, nil
		})

	huma.Register(s.API, op("bind-e2ee-conversation", "PUT", "/api/v1/e2ee/conversations/{id}", "Turn on encryption in a direct conversation",
		"Makes the group the conversation's current group for both people and binds your own conversation to it: from then on the backend refuses plaintext messages, edits, reactions and raw PRIVMSG or NOTICE into it. The other person's client binds their side when it joins the group. A conversation has one current group: binding it again changes nothing, and a different group must name the current one in replaces, else 409."),
		func(ctx context.Context, in *E2EEBindInput) (*Output[E2EEConversation], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			known, err := s.Manager.E2EEDirect(owner(ctx), in.ID)
			if err != nil {
				return nil, commandFailure(err)
			}
			if known != "" && !strings.EqualFold(known, in.Body.Peer) {
				return nil, problem(409, "The person in this conversation is logged in to a different account.", problemResourceConflict)
			}
			peer, err := e2eeOwner(store, in.Body.Peer)
			if err != nil {
				return nil, e2eeFailure(err)
			}
			if err = store.BindE2EEConversation(owner(ctx), in.ID, peer, in.Body.GroupID, in.Body.Replaces); err != nil {
				return nil, e2eeFailure(err)
			}
			out, err := s.e2eeConversation(ctx, store, in.ID)
			if err != nil {
				return nil, err
			}
			return &Output[E2EEConversation]{Body: out}, nil
		})

	huma.Register(s.API, op("turn-off-e2ee-conversation", "DELETE", "/api/v1/e2ee/conversations/{id}", "Turn off encryption in a direct conversation",
		"Ends encryption for both people if groupId is still the current group; plaintext is accepted again. The client sends its signed, encrypted notice first, so both people see who turned it off."),
		func(ctx context.Context, in *E2EETurnOffInput) (*Output[E2EEConversation], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			if _, err = s.Manager.E2EEDirect(owner(ctx), in.ID); err != nil {
				return nil, commandFailure(err)
			}
			if err = store.TurnOffE2EE(owner(ctx), in.ID, in.GroupID); err != nil {
				return nil, e2eeFailure(err)
			}
			out, err := s.e2eeConversation(ctx, store, in.ID)
			if err != nil {
				return nil, err
			}
			return &Output[E2EEConversation]{Body: out}, nil
		})

	huma.Register(s.API, op("relay-e2ee-approval", "POST", "/api/v1/e2ee/approvals", "Relay a device approval message",
		"Passes one handshake message between two devices of your account while one approves the other. Messages expire after 10 minutes; an account can have 32 waiting."),
		func(ctx context.Context, in *E2EEApprovalInput) (*Output[E2EEApprovalID], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			if !blobOK(in.Body.Payload, e2eeMaxMLSObject) {
				return nil, tooLarge("payload", e2eeMaxMLSObject)
			}
			id, err := store.AddE2EEApproval(owner(ctx), in.Body.FromDevice, in.Body.ToDevice, in.Body.Payload)
			if err != nil {
				return nil, e2eeFailure(err)
			}
			return &Output[E2EEApprovalID]{Body: E2EEApprovalID{ID: id}}, nil
		})

	huma.Register(s.API, op("list-e2ee-approvals", "GET", "/api/v1/e2ee/devices/{deviceId}/approvals", "List approval messages for a device",
		"Unexpired approval messages for this device of yours, oldest first. Remove each one after reading it."),
		func(ctx context.Context, in *E2EEDevicePath) (*Output[Items[E2EEApproval]], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			approvals, err := store.E2EEApprovals(owner(ctx), in.DeviceID)
			if err != nil {
				return nil, e2eeFailure(err)
			}
			out := Items[E2EEApproval]{Items: []E2EEApproval{}}
			for _, a := range approvals {
				out.Items = append(out.Items, E2EEApproval{ID: a.ID, FromDevice: a.FromDevice, ToDevice: a.ToDevice, Payload: a.Payload, CreatedAt: a.CreatedAt})
			}
			return &Output[Items[E2EEApproval]]{Body: out}, nil
		})

	huma.Register(s.API, noContent(op("delete-e2ee-approval", "DELETE", "/api/v1/e2ee/approvals/{id}", "Remove a read approval message", "Removes one of your account's approval messages.")),
		func(ctx context.Context, in *E2EEItemPath) (*struct{}, error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			return nil, e2eeFailure(store.DeleteE2EEApproval(owner(ctx), in.ID))
		})
}

// e2eeRemoteFailure explains a directory lookup over IRC that failed. Without
// a connection that passed the encryption self-test nothing can be asked.
func e2eeRemoteFailure(err error) error {
	var service *irc.ServiceError
	if errors.As(err, &service) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return commandFailure(err)
	}
	return huma.Error404NotFound("This server has no encryption keys for this person and can't ask other IRCord servers right now.")
}

// e2eeDeviceList returns the account's signed device list, or nil before its
// devices published one.
func e2eeDeviceList(store *storage.Store, account string) (*E2EEDeviceList, error) {
	list, counter, err := store.E2EEDeviceList(account)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &E2EEDeviceList{List: list, Counter: counter}, nil
}

// cleanLabel accepts a short device name without control characters.
func cleanLabel(label string) bool {
	for _, r := range label {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
