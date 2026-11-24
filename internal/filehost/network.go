package filehost

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// This file is the IRCord side of an IRC network's shared file host: the
// draft/FILEHOST standard, plus deletion, which the standard lacks. A server
// that serves the host stores what it receives like its own uploads. A server
// that uses a host passes message uploads on, with the person's IRC login, and
// asks it to delete them later.

// Credentials returns the IRC login of owner's connection to a network.
type Credentials func(owner, network string) (user, password string, err error)

// ErrRemoteRefused reports that a network file host will not delete a file for
// this login, or that no login is available to ask it. Repeating the request
// does not help.
var ErrRemoteRefused = errors.New("the network file host refused to delete the file")

// UseCredentials sets how the service finds the login that deletes a file on a
// network's file host. Without it such files cannot be deleted.
func (s *Service) UseCredentials(f Credentials) { s.credentials.Store(&f) }

func (s *Service) creds() Credentials {
	if f := s.credentials.Load(); f != nil {
		return *f
	}
	return func(string, string) (string, string, error) { return "", "", errors.New("no IRC login is available") }
}

// listed reports whether the operator trusts the host with IRC logins
// (IRCORD_FILEHOST_TRUSTED_HOSTS). Only these hosts are known to speak the
// IRCord deletion request, and they are the only ones a message upload goes to
// when the server prefers the network's host.
func (s *Service) listed(host string) bool {
	for _, h := range s.options.TrustedHosts {
		if h = strings.TrimSpace(h); h != "" && strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

func (s *Service) anyListed() bool {
	for _, h := range s.options.TrustedHosts {
		if strings.TrimSpace(h) != "" {
			return true
		}
	}
	return false
}

// deletableUpstream reports whether the stored file id, at raw on a network's
// file host, may be sent a deletion request: the operator trusts the host with
// the IRC login, and the host said when the file was stored that it deletes
// files on request. Trust alone is not enough: a host trusted only for uploads,
// such as an IRC bouncer's, does not answer IRCord's request.
func (s *Service) deletableUpstream(id, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || !s.listed(u.Hostname()) {
		return false
	}
	var can bool
	return s.options.Store.DB.QueryRow("SELECT can_delete FROM file_remote WHERE id=?", id).Scan(&can) == nil && can
}

// Network chooses the network's own file host for a message upload. It reports
// false when the upload should stay on this server instead: the network
// advertises no host, the host is not trusted, there is no IRC login to send it,
// or the host has switched its upload door off. A host that is trusted but not
// answering is reported as unavailable rather than skipped, so a file the
// person meant to put there does not land on this server's own storage.
func (s *Service) Network(ctx context.Context, advertised, user, password string) (Provider, bool) {
	u, err := url.Parse(advertised)
	if advertised == "" || user == "" || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || !s.listed(u.Hostname()) {
		return Provider{}, false
	}
	status, types, deletes, err := s.discover(ctx, u)
	switch {
	case err == nil && status >= 200 && status < 300:
		return s.upstream(u, types, deletes, user, password), true
	case err == nil && (status == http.StatusNotFound || status == http.StatusMethodNotAllowed || status == http.StatusGone || status == http.StatusNotImplemented):
		return Provider{}, false
	}
	return Provider{Provider: "upstream", Types: []string{}, Reason: "The network's file host is unavailable. Try again shortly."}, true
}

// Receive stores a file that someone uploaded to this server's network file
// host. The file belongs to owner, counts against the owner's quota, and is
// kept until deleted: the sending server records the message, so nothing here
// would ever show a use for it.
func (s *Service) Receive(ctx context.Context, owner, name string, data []byte) (Upload, error) {
	upload, err := s.Upload(ctx, owner, "", name, data, s.LocalProvider())
	if err != nil {
		return Upload{}, err
	}
	if err = s.markUsed(owner, upload.ID); err != nil {
		_ = s.Delete(ctx, owner, upload.ID)
		return Upload{}, unavailable("file metadata is unavailable")
	}
	return upload, nil
}

// DeleteReceived deletes the file at the address id and token, when owner
// uploaded it. ErrNotFound means no such file or a wrong token, ErrForbidden a
// file another account owns, and any other error a fault of this server's
// storage. Only the token is checked, so the stored file is not opened.
func (s *Service) DeleteReceived(ctx context.Context, owner, id, token string) error {
	r, err := s.Record("", id)
	if err != nil {
		return ErrNotFound
	}
	hash := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(hash[:], r.TokenHash) != 1 || r.Backend == "upstream" || r.Backend == "avatar" {
		return ErrNotFound
	}
	if r.Owner != owner {
		return ErrForbidden
	}
	return s.Delete(ctx, owner, id)
}

// AcceptPost lists the types the host takes, for the Accept-Post header.
func (s *Service) AcceptPost() string { return strings.Join(s.options.Types, ", ") }

// deleteRemote asks a network's file host to delete the file at raw, as owner's
// login on that network. A file that is already gone counts as deleted.
func (s *Service) deleteRemote(ctx context.Context, creds Credentials, owner, network, id, raw string) error {
	if !s.deletableUpstream(id, raw) {
		return fmt.Errorf("%w: the host is not trusted", ErrRemoteRefused)
	}
	user, password, err := creds(owner, network)
	if err != nil || user == "" {
		return fmt.Errorf("%w: no IRC login is available", ErrRemoteRefused)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, raw, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(user, password)
	res, err := s.options.Client.Do(req)
	if err != nil {
		return fmt.Errorf("the network file host is unavailable")
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusNoContent, res.StatusCode == http.StatusOK, res.StatusCode == http.StatusNotFound, res.StatusCode == http.StatusGone:
		return nil
	case res.StatusCode == http.StatusUnauthorized, res.StatusCode == http.StatusForbidden, res.StatusCode == http.StatusMethodNotAllowed, res.StatusCode == http.StatusNotImplemented:
		// Another file host, such as an IRC bouncer's, that has no deletion request.
		return fmt.Errorf("%w (%d)", ErrRemoteRefused, res.StatusCode)
	}
	return fmt.Errorf("the network file host could not delete the file (%d)", res.StatusCode)
}

// removeUpstream deletes the stored file's copy on the network's file host. It
// returns an error only when trying again later may succeed. A host that is no
// longer trusted, or refuses, leaves the file there and counts as done.
func (s *Service) removeUpstream(ctx context.Context, creds Credentials, owner, id string) (gone bool, err error) {
	network, raw, err := s.upstreamTarget(ctx, id)
	if err != nil {
		return false, err
	}
	if !s.deletableUpstream(id, raw) {
		return false, nil
	}
	err = s.deleteRemote(ctx, creds, owner, network, id, raw)
	if errors.Is(err, ErrRemoteRefused) {
		slog.Warn("The network file host kept a file that was deleted here", "file", id, "reason", err)
		return false, nil
	}
	return err == nil, err
}

// upstreamTarget returns the network and address of a file kept on a file host.
// A record without metadata names no address, and nothing can be done for it.
func (s *Service) upstreamTarget(ctx context.Context, id string) (network, raw string, err error) {
	err = s.options.Store.DB.QueryRowContext(ctx, "SELECT m.network,coalesce(json_extract(m.payload,'$.url'),'') FROM file_metadata m WHERE m.id=?", id).Scan(&network, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return network, raw, err
}

// upstreamNeedle is the text a message sent with a file on a network's file
// host contains, which the unused-upload sweep looks for in stored messages: the
// stored address without its scheme. It makes no assumption about how the host
// lays out its addresses.
func upstreamNeedle(raw, fallback string) string {
	if rest, ok := strings.CutPrefix(raw, "https://"); ok && rest != "" {
		return rest
	}
	return fallback
}

// refusal turns a file host's answer to an upload into the error shown to the
// person. The host's own sentence, such as its quota message, is kept.
func refusal(res *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	text := string(raw)
	if strings.Contains(res.Header.Get("Content-Type"), "json") {
		var problem struct{ Detail, Title string }
		if json.Unmarshal(raw, &problem) == nil {
			text = problem.Detail
			if text == "" {
				text = problem.Title
			}
		}
	}
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > 300 {
		text = string(runes[:300])
	}
	switch {
	case text == "":
		return fmt.Errorf("upstream upload was rejected (%d)", res.StatusCode)
	case res.StatusCode == http.StatusRequestEntityTooLarge:
		return quotaError{text}
	}
	return fmt.Errorf("%s", text)
}
