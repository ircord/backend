package api

import (
	"context"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"ircord/backend/internal/irc"
	"ircord/backend/internal/storage"
)

// Encrypted chats carried over IRC (ircord/issues#19, phase 2): direct chats
// with anyone on the home network, whichever IRCord server they use, and
// groups in their own invite-only channel. Commits, Welcomes and requests
// travel between servers as tagged IRC lines; these operations are the
// client's side of that. Every MLS object stays opaque.

type E2EEChat struct {
	GroupID        string    `json:"groupId"`
	Kind           string    `json:"kind" enum:"direct,group"`
	State          string    `json:"state" enum:"invited,joined" doc:"invited until you join; a joined chat refuses plaintext from you."`
	ConversationID string    `json:"conversationId,omitempty" doc:"Your conversation the chat is bound to: the direct conversation, or the group's channel."`
	Target         string    `json:"target" doc:"Where its commits go on IRC: the other person's nick, or the group's channel (#e2ee- and the group ID in hex)."`
	Peer           string    `json:"peer,omitempty" doc:"The other person's account in a direct chat."`
	Inviter        string    `json:"inviter,omitempty" doc:"The account whose Welcome invited you; your own for chats you started."`
	Replaces       string    `json:"replaces,omitempty" doc:"A direct chat's earlier group, which this one replaces after someone started fresh."`
	Epoch          int64     `json:"epoch" doc:"The epoch you joined at, raised as your own commits win."`
	Declined       []string  `json:"declined" nullable:"false" doc:"Accounts that declined your invitation. Their clients remove them."`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type E2EEChatPath struct {
	GroupID string `path:"groupId" minLength:"1" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$" doc:"The MLS group ID in base64url. A group's ID is 16 random bytes."`
}

type E2EECreateChatInput struct {
	Body struct {
		GroupID        string   `json:"groupId" minLength:"1" maxLength:"128" pattern:"^[A-Za-z0-9_-]+$" doc:"The MLS group ID in base64url, chosen by the creating client. A group's ID must be 16 random bytes; it also names the group's channel."`
		Kind           string   `json:"kind" enum:"direct,group"`
		ConversationID string   `json:"conversationId,omitempty" maxLength:"200" doc:"direct: your direct conversation with the person."`
		Peer           string   `json:"peer,omitempty" maxLength:"100" doc:"direct: the other person's account."`
		Members        []string `json:"members,omitempty" maxItems:"32" nullable:"false" doc:"group: the accounts you add, with or without your own. Each gets a standing right to enter the channel and the Welcome."`
		Epoch          int64    `json:"epoch" minimum:"0" doc:"The epoch the creator reached, usually 1 after the commit that added everyone."`
		Welcome        []byte   `json:"welcome,omitempty" doc:"The MLS Welcome for everyone added, which can include your account's other devices. Your account's devices on this server get it at once; a device that is not in it, such as yours, discards it. The server delivers it to each other account's IRCord server and retries until that server confirms it, for 30 days."`
		Replaces       string   `json:"replaces,omitempty" maxLength:"128" doc:"direct: the earlier group this one replaces after someone started fresh."`
	}
}

type E2EECreatedChat struct {
	Chat     E2EEChat `json:"chat"`
	Warnings []string `json:"warnings" nullable:"false" doc:"Steps that did not complete, such as a member who could not be given the right to enter."`
}

type E2EEChatCommitInput struct {
	E2EEChatPath
	Body struct {
		Epoch   int64    `json:"epoch" minimum:"0" doc:"The epoch the commit was made in."`
		Commit  []byte   `json:"commit"`
		Welcome []byte   `json:"welcome,omitempty" doc:"The Welcome for the accounts the commit adds. Sent only if the commit wins its epoch."`
		Added   []string `json:"added,omitempty" maxItems:"32" nullable:"false" doc:"Accounts the commit adds. In a group they get the right to enter once it has won. Name your own account when the commit adds a device of yours: your devices on this server get the Welcome, and your rights in the group's channel stay."`
		Removed []string `json:"removed,omitempty" maxItems:"32" nullable:"false" doc:"Accounts the commit removes. In a group they lose the right to enter and are kicked once it has won."`
		Admins  []string `json:"admins,omitempty" maxItems:"32" nullable:"false" doc:"Accounts the commit makes admins. They get operator rights in the group's channel."`
		Members []string `json:"members,omitempty" maxItems:"32" nullable:"false" doc:"Admins the commit makes ordinary members again."`
	}
}

type E2EEPosition struct {
	At    time.Time `json:"at" doc:"The IRC server's time on the commit's last line."`
	MsgID string    `json:"msgid" doc:"The IRC server's message ID on the commit's last line. Within one time, positions sort by it."`
}

type E2EEChatCommitResult struct {
	Accepted bool         `json:"accepted" doc:"Your commit came first for its epoch. Otherwise another commit won: discard yours, apply the newer commits and try again."`
	Position E2EEPosition `json:"position" doc:"Where the IRC server put your commit."`
	Winner   E2EEPosition `json:"winner" doc:"The first commit for the epoch."`
	Warnings []string     `json:"warnings" nullable:"false" doc:"Channel changes that failed after the commit won."`
}

type E2EEChatCommitsInput struct {
	E2EEChatPath
	From  int64 `query:"from" minimum:"0" doc:"The epoch this device is at; commits made in that epoch come first."`
	Limit int   `query:"limit" default:"100" minimum:"1" maximum:"100"`
}

type E2EEChatCommit struct {
	Epoch    int64        `json:"epoch" doc:"The epoch the commit was made in."`
	Sender   string       `json:"sender" doc:"The IRC account that sent it, as the network attached it. Clients check the MLS sender's certificate against it."`
	Commit   []byte       `json:"commit"`
	Position E2EEPosition `json:"position" doc:"Candidates of one epoch are listed in position order; apply the first one your core accepts."`
}

type E2EEChatWelcome struct {
	ID        int64        `json:"id"`
	GroupID   string       `json:"groupId"`
	Epoch     int64        `json:"epoch"`
	Kind      string       `json:"kind" enum:"direct,group"`
	Replaces  string       `json:"replaces,omitempty" doc:"direct: the group this one replaces. When both sides started a chat at once, the Welcome with the earlier position wins."`
	Target    string       `json:"target" doc:"The inviter's nick, or the group's channel."`
	Sender    string       `json:"sender" doc:"The IRC account that sent the Welcome."`
	Welcome   []byte       `json:"welcome"`
	Position  E2EEPosition `json:"position"`
	CreatedAt time.Time    `json:"createdAt"`
}

type E2EELeaveInput struct {
	E2EEChatPath
	Close bool `query:"close" doc:"group: you founded it and close it for everyone: its channel registration is removed before you leave."`
}

type E2EERejoinInput struct {
	E2EEChatPath
	Body struct {
		DeviceID    string `json:"deviceId" minLength:"1" maxLength:"64" pattern:"^[A-Za-z0-9_-]+$"`
		Certificate []byte `json:"certificate"`
		KeyPackage  []byte `json:"keyPackage"`
	}
}

type E2EERejoin struct {
	ID          int64     `json:"id"`
	Account     string    `json:"account" doc:"The IRC account that asked."`
	DeviceID    string    `json:"deviceId"`
	Certificate []byte    `json:"certificate"`
	KeyPackage  []byte    `json:"keyPackage"`
	CreatedAt   time.Time `json:"createdAt"`
}

func positionOut(at int64, msgid string) E2EEPosition {
	return E2EEPosition{At: time.Unix(0, at).UTC(), MsgID: msgid}
}

func e2eeChatOut(store *storage.Store, account string, v storage.E2EEChat) (E2EEChat, error) {
	out := E2EEChat{GroupID: v.Group, Kind: v.Kind, State: v.State, ConversationID: v.Conversation, Target: v.Target, Peer: v.Peer, Inviter: v.Inviter, Replaces: v.Replaces, Epoch: v.Epoch, Declined: []string{}, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}
	states, err := store.E2EEOutboxStates(account, v.Group)
	if err != nil {
		return out, err
	}
	for recipient, state := range states {
		if state == storage.E2EEOutboxDeclined {
			out.Declined = append(out.Declined, recipient)
		}
	}
	return out, nil
}

func e2eeAccounts(names []string) ([]string, error) {
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || len(name) > 100 || strings.ContainsAny(name, " ,:*!@#") {
			return nil, huma.Error422UnprocessableEntity("each account must be an IRC account name")
		}
		out = append(out, name)
	}
	return out, nil
}

func (s *Server) e2eeChat(ctx context.Context, store *storage.Store, group string) (*Output[E2EEChat], error) {
	v, err := store.E2EEChat(owner(ctx), group)
	if err != nil {
		return nil, e2eeFailure(err)
	}
	out, err := e2eeChatOut(store, owner(ctx), v)
	if err != nil {
		return nil, e2eeFailure(err)
	}
	return &Output[E2EEChat]{Body: out}, nil
}

func (s *Server) registerE2EEChats() {
	const tag = "End-to-end encryption"
	op := func(id, method, path, summary, description string) huma.Operation {
		return huma.Operation{OperationID: id, Method: method, Path: path, Summary: summary, Description: description + " Experimental; answers 404 while the server operator has not turned encryption on.", Tags: []string{tag}}
	}
	noContent := func(o huma.Operation) huma.Operation { o.DefaultStatus = 204; return o }

	huma.Register(s.API, op("list-e2ee-chats", "GET", "/api/v1/e2ee/chats", "List your encrypted chats",
		"Your encrypted direct chats and groups carried over IRC, with invitations you have not answered yet."),
		func(ctx context.Context, _ *struct{}) (*Output[Items[E2EEChat]], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			chats, err := store.E2EEChats(owner(ctx))
			if err != nil {
				return nil, e2eeFailure(err)
			}
			out := Items[E2EEChat]{Items: []E2EEChat{}}
			for _, v := range chats {
				item, err := e2eeChatOut(store, owner(ctx), v)
				if err != nil {
					return nil, e2eeFailure(err)
				}
				out.Items = append(out.Items, item)
			}
			return &Output[Items[E2EEChat]]{Body: out}, nil
		})

	huma.Register(s.API, op("create-e2ee-chat", "POST", "/api/v1/e2ee/chats", "Start an encrypted chat",
		"Starts an encrypted direct chat with anyone on the home network, or an encrypted group. A direct chat binds your direct conversation, which then refuses plaintext from you. A group gets its own registered, invite-only, secret channel named after its ID; each member gets a standing right to enter it. The Welcome goes to everyone added, in the background."),
		func(ctx context.Context, in *E2EECreateChatInput) (*Output[E2EECreatedChat], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			if len(in.Body.Welcome) > e2eeMaxMLSObject {
				return nil, tooLarge("welcome", e2eeMaxMLSObject)
			}
			members, err := e2eeAccounts(in.Body.Members)
			if err != nil {
				return nil, err
			}
			create := irc.E2EECreate{Group: in.Body.GroupID, Kind: in.Body.Kind, Conversation: in.Body.ConversationID, Peer: strings.TrimSpace(in.Body.Peer), Members: members, Epoch: in.Body.Epoch, Welcome: in.Body.Welcome, Replaces: in.Body.Replaces}
			switch in.Body.Kind {
			case storage.E2EEChatDirect:
				if create.Conversation == "" || create.Peer == "" || len(members) > 0 {
					return nil, huma.Error422UnprocessableEntity("a direct chat names conversationId and peer, and no members")
				}
				if _, err := e2eeAccounts([]string{create.Peer}); err != nil {
					return nil, err
				}
			case storage.E2EEChatGroup:
				if create.Conversation != "" || create.Peer != "" || create.Replaces != "" {
					return nil, huma.Error422UnprocessableEntity("a group names members, not conversationId, peer or replaces")
				}
			}
			chat, warnings, err := s.Manager.E2EECreateChat(ctx, owner(ctx), create)
			if err != nil {
				return nil, commandFailure(err)
			}
			out, err := e2eeChatOut(store, owner(ctx), chat)
			if err != nil {
				return nil, e2eeFailure(err)
			}
			return &Output[E2EECreatedChat]{Body: E2EECreatedChat{Chat: out, Warnings: warnings}}, nil
		})

	huma.Register(s.API, op("get-e2ee-chat", "GET", "/api/v1/e2ee/chats/{groupId}", "Read an encrypted chat", "One of your encrypted chats."),
		func(ctx context.Context, in *E2EEChatPath) (*Output[E2EEChat], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			return s.e2eeChat(ctx, store, in.GroupID)
		})

	huma.Register(s.API, op("join-e2ee-chat", "POST", "/api/v1/e2ee/chats/{groupId}/join", "Join an encrypted chat you were invited to",
		"Enters the group's channel, or binds your direct conversation with the inviter, and asks another member for the commits you missed. Joining again changes nothing."),
		func(ctx context.Context, in *E2EEChatPath) (*Output[E2EEChat], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			if _, err = s.Manager.E2EEJoinChat(ctx, owner(ctx), in.GroupID); err != nil {
				return nil, commandFailure(err)
			}
			return s.e2eeChat(ctx, store, in.GroupID)
		})

	huma.Register(s.API, noContent(op("decline-e2ee-chat", "POST", "/api/v1/e2ee/chats/{groupId}/decline", "Decline an invitation",
		"Forgets the invitation and its Welcomes, and tells the inviter's IRCord server, whose client then removes you.")),
		func(ctx context.Context, in *E2EEChatPath) (*struct{}, error) {
			if _, err := s.e2eeStore(); err != nil {
				return nil, err
			}
			return nil, commandFailure(s.Manager.E2EEDeclineChat(owner(ctx), in.GroupID))
		})

	huma.Register(s.API, noContent(op("leave-e2ee-chat", "DELETE", "/api/v1/e2ee/chats/{groupId}", "Leave an encrypted chat",
		"Leaves a group's channel, or accepts plaintext in a direct conversation again, and forgets the chat's commits on this server. Send your encrypted notice first, so the others see that you left; an admin's client then removes you from the group.")),
		func(ctx context.Context, in *E2EELeaveInput) (*struct{}, error) {
			if _, err := s.e2eeStore(); err != nil {
				return nil, err
			}
			return nil, commandFailure(s.Manager.E2EELeaveChat(ctx, owner(ctx), in.GroupID, in.Close))
		})

	huma.Register(s.API, op("post-e2ee-chat-commit", "POST", "/api/v1/e2ee/chats/{groupId}/commits", "Send a commit to an encrypted chat",
		"Sends the commit over IRC and answers once it has settled, about two seconds after the IRC server echoed it. Of the commits for one epoch, the one the server stamped first wins everywhere: accepted tells you whether that is yours. Only a winning commit's Welcome is sent and its channel changes made."),
		func(ctx context.Context, in *E2EEChatCommitInput) (*Output[E2EEChatCommitResult], error) {
			if _, err := s.e2eeStore(); err != nil {
				return nil, err
			}
			if !blobOK(in.Body.Commit, e2eeMaxMLSObject) {
				return nil, tooLarge("commit", e2eeMaxMLSObject)
			}
			if len(in.Body.Welcome) > e2eeMaxMLSObject {
				return nil, tooLarge("welcome", e2eeMaxMLSObject)
			}
			lists := [][]string{in.Body.Added, in.Body.Removed, in.Body.Admins, in.Body.Members}
			for i, list := range lists {
				clean, err := e2eeAccounts(list)
				if err != nil {
					return nil, err
				}
				lists[i] = clean
			}
			result, err := s.Manager.E2EEPostCommit(ctx, owner(ctx), in.GroupID, irc.E2EECommit{Epoch: in.Body.Epoch, Commit: in.Body.Commit, Welcome: in.Body.Welcome, Added: lists[0], Removed: lists[1], Admins: lists[2], Members: lists[3]})
			if err != nil {
				return nil, commandFailure(err)
			}
			out := E2EEChatCommitResult{Accepted: result.Accepted, Position: E2EEPosition(result.Position), Winner: E2EEPosition(result.Winner), Warnings: result.Warnings}
			return &Output[E2EEChatCommitResult]{Body: out}, nil
		})

	huma.Register(s.API, op("list-e2ee-chat-commits", "GET", "/api/v1/e2ee/chats/{groupId}/commits", "List an encrypted chat's commits",
		"Every complete commit this server saw for the chat from an epoch on: by epoch, and within an epoch in position order. Work through them in this order, together with messages, after time offline. The server keeps a chat's last 1,000 commits."),
		func(ctx context.Context, in *E2EEChatCommitsInput) (*Output[Items[E2EEChatCommit]], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			if _, err = store.E2EEChat(owner(ctx), in.GroupID); err != nil {
				return nil, e2eeFailure(err)
			}
			commits, err := store.E2EEObservedCommits(owner(ctx), in.GroupID, in.From, in.Limit)
			if err != nil {
				return nil, e2eeFailure(err)
			}
			out := Items[E2EEChatCommit]{Items: []E2EEChatCommit{}}
			for _, v := range commits {
				out.Items = append(out.Items, E2EEChatCommit{Epoch: v.Epoch, Sender: v.Sender, Commit: v.Commit, Position: positionOut(v.At, v.MsgID)})
			}
			return &Output[Items[E2EEChatCommit]]{Body: out}, nil
		})

	huma.Register(s.API, op("list-e2ee-chat-welcomes", "GET", "/api/v1/e2ee/devices/{deviceId}/chat-welcomes", "List Welcomes that arrived over IRC for a device",
		"Welcomes for encrypted chats carried over IRC, in position order. Remove each one after processing it. They expire after 30 days."),
		func(ctx context.Context, in *E2EEDevicePath) (*Output[Items[E2EEChatWelcome]], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			welcomes, err := store.E2EEChatWelcomes(owner(ctx), in.DeviceID)
			if err != nil {
				return nil, e2eeFailure(err)
			}
			out := Items[E2EEChatWelcome]{Items: []E2EEChatWelcome{}}
			for _, v := range welcomes {
				out.Items = append(out.Items, E2EEChatWelcome{ID: v.ID, GroupID: v.Group, Epoch: v.Epoch, Kind: v.Kind, Replaces: v.Replaces, Target: v.Target, Sender: v.Sender, Welcome: v.Welcome, Position: positionOut(v.At, v.MsgID), CreatedAt: v.CreatedAt})
			}
			return &Output[Items[E2EEChatWelcome]]{Body: out}, nil
		})

	huma.Register(s.API, noContent(op("delete-e2ee-chat-welcome", "DELETE", "/api/v1/e2ee/chat-welcomes/{id}", "Remove a processed Welcome", "Removes one of your devices' Welcomes that arrived over IRC.")),
		func(ctx context.Context, in *E2EEItemPath) (*struct{}, error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			return nil, e2eeFailure(store.DeleteE2EEChatWelcome(owner(ctx), in.ID))
		})

	huma.Register(s.API, noContent(op("request-e2ee-rejoin", "POST", "/api/v1/e2ee/chats/{groupId}/rejoin", "Ask to be added to a chat again",
		"For a device that fell out of step with a chat: sends its certificate and a fresh key package to the chat. One of your own devices, or else an admin's, removes the stale device and adds it again.")),
		func(ctx context.Context, in *E2EERejoinInput) (*struct{}, error) {
			if _, err := s.e2eeStore(); err != nil {
				return nil, err
			}
			if !blobOK(in.Body.Certificate, e2eeMaxCertificate) {
				return nil, tooLarge("certificate", e2eeMaxCertificate)
			}
			if !blobOK(in.Body.KeyPackage, e2eeMaxKeyPackage) {
				return nil, tooLarge("keyPackage", e2eeMaxKeyPackage)
			}
			return nil, commandFailure(s.Manager.E2EERequestRejoin(owner(ctx), in.GroupID, in.Body.DeviceID, in.Body.Certificate, in.Body.KeyPackage))
		})

	huma.Register(s.API, op("list-e2ee-rejoins", "GET", "/api/v1/e2ee/chats/{groupId}/rejoins", "List requests to be added again",
		"Devices of other members, or of your own account, that asked to be added to the chat again. They expire after 7 days; remove each one once handled."),
		func(ctx context.Context, in *E2EEChatPath) (*Output[Items[E2EERejoin]], error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			if _, err = store.E2EEChat(owner(ctx), in.GroupID); err != nil {
				return nil, e2eeFailure(err)
			}
			rejoins, err := store.E2EERejoins(owner(ctx), in.GroupID)
			if err != nil {
				return nil, e2eeFailure(err)
			}
			out := Items[E2EERejoin]{Items: []E2EERejoin{}}
			for _, v := range rejoins {
				out.Items = append(out.Items, E2EERejoin{ID: v.ID, Account: v.Account, DeviceID: v.Device, Certificate: v.Certificate, KeyPackage: v.KeyPackage, CreatedAt: v.CreatedAt})
			}
			return &Output[Items[E2EERejoin]]{Body: out}, nil
		})

	huma.Register(s.API, noContent(op("delete-e2ee-rejoin", "DELETE", "/api/v1/e2ee/rejoins/{id}", "Remove a handled rejoin request", "Removes one of your chats' rejoin requests.")),
		func(ctx context.Context, in *E2EEItemPath) (*struct{}, error) {
			store, err := s.e2eeStore()
			if err != nil {
				return nil, err
			}
			return nil, e2eeFailure(store.DeleteE2EERejoin(owner(ctx), in.ID))
		})
}
