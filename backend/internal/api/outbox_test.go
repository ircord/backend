package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"ircord/backend/internal/irc"
)

func TestErgoOutboxBrowser(t *testing.T) {
	addr := os.Getenv("IRCORD_ERGO_ADDR")
	if addr == "" || os.Getenv("IRCORD_OUTBOX_BROWSER") != "1" {
		t.Skip("build the frontend and set IRCORD_OUTBOX_BROWSER=1 and IRCORD_ERGO_ADDR")
	}
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	app, server, store, _ := authenticatedApp(t)
	client := cookieClient()
	account := register(t, client, server.URL)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	uiPort := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	ui := fmt.Sprintf("http://127.0.0.1:%d", uiPort)
	app.options.Origins = []string{ui}

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	room := "#outbox" + suffix
	var network irc.Network
	if err = json.Unmarshal(request(t, client, "POST", server.URL+"/api/v1/networks",
		irc.NetworkConfig{ID: "outbox-test", Name: "Outbox fixture", Host: host, Port: port,
			Nick: "sender" + suffix, AutoJoin: []string{room}}, 200), &network); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err = app.Manager.WaitConnected(ctx, account.Account.ID, network.ID); err != nil {
		t.Fatal(err)
	}
	var conversation string
	markerWait(t, func() bool {
		for _, c := range app.Manager.Snapshot(account.Account.ID).Conversations {
			if c.Target == room && c.Joined {
				conversation = c.ID
				return true
			}
		}
		return false
	})
	input := irc.SendInput{ClientID: "held-by-backend", ConversationID: conversation, Kind: "message", Text: "Saved without an IRC receipt"}
	if _, _, err = store.BeginCommand(account.Account.ID, input); err != nil {
		t.Fatal(err)
	}
	if err = store.UncertainCommand(account.Account.ID, input.ClientID, "No IRC confirmation."); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(map[string]any{
		"api": server.URL, "ui": ui, "port": uiPort, "username": account.Username,
		"password": account.Password, "room": room, "conversation": conversation,
	})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.Abs("../../../frontend")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, "bun", "scripts/verify-outbox-browser.mjs")
	command.Dir = dir
	command.Env = append(os.Environ(), "IRCORD_OUTBOX_TEST="+string(config))
	output, err := command.CombinedOutput()
	t.Log(strings.ReplaceAll(string(output), account.Password, "[disposable password]"))
	if err != nil {
		t.Fatal(err)
	}
}

func TestOutboxExposesRetainedPayloadOnlyToItsOwner(t *testing.T) {
	_, server, store, _ := authenticatedApp(t)
	client, other := cookieClient(), cookieClient()
	account := register(t, client, server.URL)
	register(t, other, server.URL)
	input := irc.SendInput{ClientID: "retained", ConversationID: "gone", Kind: "message", Text: "private retained text", ReplyToID: "parent"}
	if _, _, err := store.QueueCommand(account.Account.ID, input, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.FailCommand(account.Account.ID, input.ClientID, "Channel is unavailable."); err != nil {
		t.Fatal(err)
	}
	var page OutboxPage
	if err := json.Unmarshal(request(t, client, "GET", server.URL+"/api/v1/outbox", nil, 200), &page); err != nil ||
		len(page.Items) != 1 || page.Items[0].Payload.Text != input.Text || page.Items[0].Payload.ReplyToID != "parent" || page.Items[0].State != "failed" {
		t.Fatalf("retained payload was not available on reload: %v %v", page, err)
	}
	otherPage := OutboxPage{}
	if err := json.Unmarshal(request(t, other, "GET", server.URL+"/api/v1/outbox", nil, 200), &otherPage); err != nil || len(otherPage.Items) != 0 {
		t.Fatalf("another account saw private sends: %v %v", otherPage, err)
	}
	request(t, other, "POST", server.URL+"/api/v1/commands/retained/retry", map[string]bool{"acceptDuplicate": true}, 404)
	request(t, other, "DELETE", server.URL+"/api/v1/commands/retained", nil, 404)
	request(t, client, "DELETE", server.URL+"/api/v1/commands/retained", nil, 204)
	entry, err := store.OutboxEntry(account.Account.ID, input.ClientID)
	if err != nil || entry.State != "discarded" || entry.Payload.Text != "" {
		t.Fatalf("discard did not keep only the idempotency record: %v %v", entry, err)
	}
}
