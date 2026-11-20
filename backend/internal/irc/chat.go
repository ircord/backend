package irc

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lrstanley/girc"
	"ircord/backend/internal/model"
)

type pending struct {
	input       SendInput
	attachments []model.Attachment
	messageID   string
	label       string
	attempt     string
	at          time.Time
}

func (c *connection) message(e girc.Event) { c.messageWithHistory(e, false) }

func (c *connection) messageWithHistory(e girc.Event, forceHistory bool, refs ...string) {
	c.messageWithReceipt(e, forceHistory, false, refs...)
}

func (c *connection) messageWithReceipt(e girc.Event, forceHistory, unconfirmed bool, refs ...string) {
	if e.Source == nil || len(e.Params) < 2 {
		return
	}
	// CTCP queries and replies other than ACTION are protocol traffic: the client library answers
	// queries, and profile lookups read replies directly. Neither belongs in a conversation.
	if ctcp := girc.DecodeCTCP(&e); ctcp != nil && ctcp.Command != girc.CTCP_ACTION {
		return
	}
	c.mu.Lock()
	historical := forceHistory || c.historyBatches[e.Tags["batch"]]
	ref := e.Tags["batch"]
	if len(refs) > 0 {
		ref = refs[0]
	}
	position := int64(0)
	req := c.historyResponses[ref]
	if req != nil && req.sub == "BEFORE" && req.orderBase != 0 {
		position = req.orderBase + int64(req.result.Count)
	}
	// Catch-up after a disconnect or restart (CHATHISTORY AFTER the stored
	// watermark) recovers messages that were missed, unlike scrolling back
	// (BEFORE) or a new conversation's first fill (LATEST).
	recovered := historical && req != nil && req.sub == "AFTER"
	c.mu.Unlock()
	target := e.Params[0]
	if !c.partyMessageAllowed(target, e.Tags["account"], historical) {
		return
	}
	if !girc.IsValidChannel(target) && c.fold(target) == c.fold(c.currentNick()) {
		target = e.Source.Name
	}
	w := c.owner
	w.mu.Lock()
	defer w.mu.Unlock()
	conv := c.conversation(target)
	if conv.Party != nil && c.owner.ownerID != partyControlOwner && !strings.EqualFold(e.Tags["account"], c.config.SASLUser) {
		if party, err := c.manager.options.Store.Party(conv.Party.ID); err == nil {
			if !party.ConversionPending && party.State != "converted" {
				return
			}
		} else if !slices.ContainsFunc(conv.Party.Members, func(member model.PartyMember) bool {
			return strings.EqualFold(member.ServicesAccount, e.Tags["account"])
		}) {
			return
		}
	}
	upstream := e.Tags["msgid"]
	messageID := id()
	if upstream != "" {
		messageID = c.config.ID + ":" + upstream
	}
	lookupKey := messageID
	if upstream != "" {
		lookupKey = upstream
	}
	existing, known := c.lookupMessage(conv.ID, lookupKey)
	if known && existing.ClientID != "" {
		return
	}
	sender := c.member(e.Source.Name, e.Tags["account"])
	host := c.memberHostForDisplay(sender.ID == w.self.ID, e.Source.Host)
	if e.Source.Ident != "" && (sender.Username != e.Source.Ident || sender.Host != host) {
		sender.Username = e.Source.Ident
		sender.Host = host
		if sender.ID == w.self.ID {
			w.saveNetworkSelf(c.config.ID, sender)
		} else {
			w.members[sender.ID] = sender
			w.emit("member.updated", sender)
		}
	}
	at := e.Timestamp
	if parsed, err := time.Parse(time.RFC3339Nano, e.Tags["time"]); err == nil {
		at = parsed
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	msg := Message{Position: position, ID: messageID, ConversationID: conv.ID, SenderID: sender.ID, Text: e.Last(), CreatedAt: at, Kind: "message", Reactions: []Reaction{}, UpstreamID: upstream}
	if encoded := e.Tags["+ircord/gif"]; len(encoded) > 0 && len(encoded) <= 2500 {
		if raw, err := base64.RawURLEncoding.DecodeString(encoded); err == nil {
			var gif GIF
			if json.Unmarshal(raw, &gif) == nil && validGIF(&gif) && gif.URL == msg.Text {
				msg.GIF = &gif
			}
		}
	}
	if strings.HasPrefix(msg.Text, "\x01ACTION ") && strings.HasSuffix(msg.Text, "\x01") {
		msg.Kind = "action"
		msg.Text = strings.TrimSuffix(strings.TrimPrefix(msg.Text, "\x01ACTION "), "\x01")
	}
	c.readAttachments(&msg, e.Tags)
	if e.Command == "NOTICE" {
		msg.Kind = "notice"
	}
	if historical && strings.EqualFold(e.Source.Name, "HistServ") && strings.HasPrefix(upstream, "_") {
		msg.Kind = "system"
	}
	msg.SenderAccount = senderAccount(e)
	if c.owner.ownerID == partyControlOwner {
		if party, err := c.manager.options.Store.PartyByConversation(conv.ID); err == nil {
			if reactions, err := c.manager.options.Store.PartyReactions(party.ID, upstream); err == nil {
				msg.Reactions = reactions
			} else {
				w.storageErr = err
				return
			}
		}
	}
	if c.owner.ownerID == partyControlOwner && known {
		if existing.Deleted {
			return
		}
		msg.Edits = clone(existing.Edits)
		msg.EditedAt = existing.EditedAt
		msg.OriginalText = existing.OriginalText
		if existing.EditedAt != nil {
			msg.Text = existing.Text
		}
	}
	if original := e.Tags[editTag]; original != "" {
		switch c.incomingEdit(conv, sender, e, msg, historical) {
		case editConsumed:
			return
		case editPending:
			msg.EditOf = original
		}
	}
	needsAcknowledgment := false
	if sender.ID == w.self.ID {
		if account := e.Tags["account"]; account != "" && account != "*" {
			msg.Account = account
		}
		clientID, p, recovered := c.matchOutboundEcho(e, msg, historical)
		if clientID != "" {
			msg.ClientID = clientID
			msg.Unconfirmed = unconfirmed
			msg.Receipt = !unconfirmed
			needsAcknowledgment = recovered
			// The controller may materialize the canonical party row before this
			// account's echo arrives. That echo still owns its outbox acknowledgement.
			if conv.Party != nil {
				needsAcknowledgment = true
			}
			msg.GIF = p.input.GIF
			if len(p.input.Attachments) == 0 {
				msg.SetAttachments(nil)
			} else {
				attachments := p.attachments
				if len(attachments) == 0 {
					attachments, _ = c.manager.uploadAttachments(w.ownerID, c.config.ID, p.input)
				}
				if len(attachments) > 0 {
					msg.SetAttachments(attachments)
				}
			}
			if upstream == "" || conv.Party == nil && p.messageID != "" && w.store != nil {
				if p.messageID != "" {
					msg.ID = p.messageID
				}
			}
			if p.input.ReplyToID != "" {
				if prior, ok := c.lookupMessage(conv.ID, p.input.ReplyToID); ok {
					msg.ReplyTo = &Reply{ID: prior.ID, Nick: w.networkMember(c.config.ID, prior.SenderID).Nick, Text: prior.Text}
				}
			}
		}
	}
	if known && !needsAcknowledgment {
		return
	}
	msg.Historical = historical
	msg.Mention = sender.ID != w.self.ID && msg.Kind != "system" && msg.Kind != "event" && (conv.Kind == "direct" || w.policy.Mentioned(msg.Text, w.recipient(conv, c.currentNick(), msg)))
	reply := e.Tags["+reply"]
	if reply == "" {
		reply = e.Tags["+draft/reply"]
	}
	// Only a reply tag naming a message IRCord has stored counts as a reply to you.
	replyToSelf := false
	if reply != "" && msg.ReplyTo == nil {
		if prior, ok := c.lookupMessage(conv.ID, reply); ok {
			msg.ReplyTo = &Reply{ID: prior.ID, Nick: w.networkMember(c.config.ID, prior.SenderID).Nick, Text: prior.Text}
			replyToSelf = prior.SenderID == w.self.ID && !prior.Deleted
		}
	}
	if w.store != nil && upstream != "" {
		if reactions, err := w.store.Reactions(w.ownerID, conv.ID, upstream); err == nil {
			msg.Reactions = reactions
		} else {
			w.storageErr = err
			return
		}
	}
	replaced := false
	for i, old := range w.messages[conv.ID] {
		if old.ID == msg.ID {
			w.messages[conv.ID][i] = msg
			replaced = true
			break
		}
	}
	if !replaced {
		w.messages[conv.ID] = append(w.messages[conv.ID], msg)
	}
	sort.SliceStable(w.messages[conv.ID], func(i, j int) bool { return w.messages[conv.ID][i].CreatedAt.Before(w.messages[conv.ID][j].CreatedAt) })
	if len(w.messages[conv.ID]) > 500 {
		w.messages[conv.ID] = append([]Message(nil), w.messages[conv.ID][len(w.messages[conv.ID])-500:]...)
	}
	w.boundCache(conv.ID)
	if !historical && sender.ID != w.self.ID && !w.policy.Ignored(sender.Nick, sender.Username, sender.Host) {
		conv.UnreadCount++
		if msg.Mention {
			conv.MentionCount++
		}
		w.conversations[conv.ID] = conv
		w.emitCounts(conv)
	}
	if historical && w.store != nil && !needsAcknowledgment {
		inserted, err := w.store.HistoryInsert(w.ownerID, msg)
		if err != nil {
			w.storageErr = err
		} else if inserted && recovered {
			c.manager.recordInbox(w, conv, msg, sender, c.currentNick(), replyToSelf)
		}
	} else {
		w.emit("message.created", msg)
		if !historical {
			c.manager.notifyMessage(w, conv, msg, sender, c.currentNick(), replyToSelf)
			c.manager.recordInbox(w, conv, msg, sender, c.currentNick(), replyToSelf)
		}
	}
	if w.storageErr == nil {
		c.applyPendingEdits(conv, msg, historical)
	}
}

func validGIF(g *GIF) bool {
	if g == nil {
		return true
	}
	if g.Provider != "klipy" || len(g.ID) > 100 || !clean(g.Slug, 180) || len(g.Title) > 200 || g.Width < 1 || g.Height < 1 || g.Width > 8192 || g.Height > 8192 {
		return false
	}
	// Klipy media hosts are kept in step with klipyMediaHosts in frontend/shared/utils/gif-hosts.ts.
	for _, raw := range []string{g.URL, g.PreviewURL, g.StillURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || len(raw) > 4096 || !slices.Contains([]string{"static.klipy.com", "media.klipy.com", "cdn.klipy.com"}, u.Hostname()) {
			return false
		}
	}
	return true
}

func normalizeSendInput(input *SendInput) error {
	if input.Kind == "" {
		input.Kind = "message"
	}
	if input.Kind != "message" && input.Kind != "action" {
		return fmt.Errorf("invalid message kind")
	}
	if !token(input.ClientID, 100) || strings.TrimSpace(input.Text) == "" || utf8.RuneCountInString(input.Text) > 4000 || len(input.Text) > 4096 || strings.ContainsAny(input.Text, "\r\x00") || !validGIF(input.GIF) || (input.GIF != nil && input.Text != input.GIF.URL) {
		return fmt.Errorf("invalid message (maximum 4000 characters / 4096 UTF-8 bytes)")
	}
	return nil
}

func (m *Manager) dispatchSend(owner string, input SendInput, expected *connection) error {
	if e := m.partyAccess(owner, input.ConversationID); e != nil {
		return e
	}
	w := m.space(owner)
	w.mu.Lock()
	if w.storageErr != nil {
		w.mu.Unlock()
		return ErrStorage
	}
	conv, ok := w.conversations[input.ConversationID]
	c := w.connections[conv.NetworkID]
	if !ok {
		w.mu.Unlock()
		return ErrNotFound
	}
	if c == nil || expected != nil && expected != c {
		w.mu.Unlock()
		return ErrDisconnected
	}
	if c.creatingTarget(c.target(conv)) {
		w.mu.Unlock()
		return serviceError(409, "Channel creation is still confirming privacy. Wait before sending a message.")
	}
	if conv.Party != nil {
		own, err := m.options.Store.PartyCandidate(owner)
		if err != nil || !strings.EqualFold(w.networks[conv.NetworkID].Account, own.Username) {
			w.mu.Unlock()
			return serviceError(403, "Reconnect your account before sending to this party.")
		}
	}
	attachments, attachmentErr := m.uploadAttachments(owner, conv.NetworkID, input)
	if attachmentErr != nil {
		w.mu.Unlock()
		return attachmentErr
	}
	for _, msg := range w.messages[conv.ID] {
		if w.store == nil && msg.ClientID == input.ClientID {
			if msg.Text != input.Text || msg.Kind != input.Kind || !equal(msg.AllAttachments(), attachments) || (w.store == nil && (input.GIF != nil || input.ReplyToID != "")) {
				w.mu.Unlock()
				return ErrClientIDConflict
			}
			w.emit("message.created", msg)
			w.mu.Unlock()
			return nil
		}
	}
	if !canSpeak(conv, w.networks[conv.NetworkID].ISupport, w.self.ID) {
		w.mu.Unlock()
		return fmt.Errorf("this channel is moderated; voice or operator privileges are required")
	}
	tags := girc.Tags{"+ircord/client-id": input.ClientID}
	if input.ReplyToID != "" {
		found := false
		replyMessages := w.messages[conv.ID]
		if w.store != nil {
			if saved, e := w.store.Message(owner, conv.ID, input.ReplyToID); e == nil {
				replyMessages = append([]Message{saved}, replyMessages...)
			}
		}
		for _, msg := range replyMessages {
			if msg.ID == input.ReplyToID && addressable(msg) {
				tags["+reply"] = msg.UpstreamID
				found = true
				break
			}
		}
		if !found {
			w.mu.Unlock()
			return fmt.Errorf("reply target is unavailable in this conversation")
		}
	}
	isupport := clone(w.networks[conv.NetworkID].ISupport)
	w.mu.Unlock()
	messageID := ""
	if w.store != nil {
		command, err := w.store.Command(owner, input.ClientID)
		if err != nil {
			return ErrStorage
		}
		if command.State != "queued" {
			return nil
		}
		if command.Route != outboundRoute(c.config) || !matchesSendEndpoint(input.Endpoint, c.config) {
			return errSendRouteChanged
		}
		messageID = command.MessageID
	}
	if messageID == "" {
		messageID = id()
	}
	attempt := id()
	c.mu.Lock()
	if !c.ready || conv.Kind == "channel" && !conv.Joined {
		c.mu.Unlock()
		return ErrDisconnected
	}
	features := c.features(isupport)
	if input.ReplyToID != "" && !features.Replies {
		c.mu.Unlock()
		return fmt.Errorf("%w: replies are disabled by this network", ErrUnsupported)
	}
	if previous, ok := c.pending[input.ClientID]; ok {
		if !equal(previous.input, input) {
			c.mu.Unlock()
			return ErrClientIDConflict
		}
		c.mu.Unlock()
		return nil
	}
	if len(c.pending) >= 4 || len(c.queue) > 8 {
		c.mu.Unlock()
		return ErrBusy
	}
	if !c.caps["message-tags"] {
		tags = nil
		if input.ReplyToID != "" {
			c.mu.Unlock()
			return ErrUnsupported
		}
	}
	label := ""
	if c.caps["labeled-response"] && tags != nil {
		label = "send:" + attempt
		tags["label"] = label
	}
	wireText := input.Text
	if input.Kind == "action" {
		wireText = "\x01ACTION " + wireText + "\x01"
	}
	if input.GIF != nil && tags != nil && tagAllowed(isupport["CLIENTTAGDENY"], "ircord/gif") {
		raw, _ := json.Marshal(input.GIF)
		encoded := base64.RawURLEncoding.EncodeToString(raw)
		if len(encoded) <= 2500 {
			tags["+ircord/gif"] = encoded
		}
	}
	if !tagAllowed(isupport["CLIENTTAGDENY"], "ircord/client-id") {
		delete(tags, "+ircord/client-id")
	}
	if err := addAttachmentTags(tags, attachments, isupport["CLIENTTAGDENY"]); err != nil {
		c.mu.Unlock()
		return err
	}
	events, eventErr := messageEvents(features, c.target(conv), wireText, tags)
	if eventErr != nil {
		c.mu.Unlock()
		return eventErr
	}
	if len(attachments) > 1 {
		for _, event := range events {
			if event.LenOpts(false) > features.MessageLineBytes || tagBytes(event.Tags) > maxSendTagBytes {
				c.mu.Unlock()
				return fmt.Errorf("upload gallery exceeds this network's message limits; send a smaller batch")
			}
		}
	}
	if err := c.enqueueLocked(events...); err != nil {
		c.mu.Unlock()
		return err
	}
	c.pending[input.ClientID] = pending{input: clone(input), attachments: slices.Clone(attachments), messageID: messageID, label: label, attempt: attempt}
	if c.outboundWrites == nil {
		c.outboundWrites = make(map[*girc.Event]outboundWrite)
	}
	c.outboundWrites[events[0]] = outboundWrite{
		clientID: input.ClientID, attempt: attempt, target: c.target(conv), text: wireText,
		expectReply: c.caps["echo-message"] || label != "",
	}
	c.mu.Unlock()
	m.endBriefAway(owner, conv.NetworkID, c)
	return nil
}
