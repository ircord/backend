package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ircord/backend/internal/auth"
	"ircord/backend/internal/ergo"
	"ircord/backend/internal/filehost"
	"ircord/backend/internal/irc"
	"ircord/backend/internal/storage"
)

// networkHost is a server set up as the IRC network's file host, with a fake
// associated IRC server that knows the passwords in logins.
type networkHost struct {
	server *httptest.Server
	store  *storage.Store
	files  *filehost.Service
	logins map[string]string
	checks int
}

func (h *networkHost) files_() string { return h.server.URL + "/files/" }

func newNetworkHost(t *testing.T, serve bool, accounts []string, tune func(*filehost.Options)) *networkHost {
	t.Helper()
	h := &networkHost{logins: map[string]string{"alice": "alice-secret", "bob": "bob-secret", "carol": "carol-secret"}}
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ AccountName, Passphrase string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if r.URL.Path != "/v1/check_auth" {
			t.Errorf("unexpected IRC server request %s", r.URL.Path)
		}
		h.checks++
		want, known := h.logins[in.AccountName]
		_ = json.NewEncoder(w).Encode(map[string]any{"success": known && want == in.Passphrase})
	}))
	t.Cleanup(vendor.Close)
	associated, err := ergo.New(ergo.Options{Server: "irc.example.com:6697", URL: vendor.URL, Token: strings.Repeat("cd", 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(associated.Close)
	h.store, err = storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.store.Close() })
	// Alice and bob have accounts on this server; carol only has an IRC account.
	for _, name := range []string{"alice", "bob"} {
		if _, err = h.store.CreateAccount("id-"+name, name); err != nil {
			t.Fatal(err)
		}
	}
	h.server = httptest.NewUnstartedServer(nil)
	options := filehost.Options{Store: h.store, Blob: filehost.Local{Root: filepath.Join(t.TempDir(), "files")}, PublicURL: "https://" + h.server.Listener.Addr().String()}
	if tune != nil {
		tune(&options)
	}
	h.files, err = filehost.New(options)
	if err != nil {
		t.Fatal(err)
	}
	manager := irc.New(irc.Options{Store: h.store, Files: h.files})
	sessions := auth.New(h.store, auth.Options{Registration: true})
	app := New(manager, Options{Store: h.store, Files: h.files, Auth: sessions, Ergo: associated, FileHost: FileHostOptions{Serve: serve, Accounts: accounts}})
	h.server.Config.Handler = app.Handler
	h.server.StartTLS()
	t.Cleanup(func() { sessions.Close(); manager.Close(); h.server.Close() })
	return h
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func (h *networkHost) do(t *testing.T, method, url, user, password string, body []byte, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	res, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func (h *networkHost) post(t *testing.T, user, password string, body []byte) *http.Response {
	return h.do(t, "POST", h.files_(), user, password, body, http.Header{"Content-Type": {"image/png"}, "Content-Disposition": {`attachment; filename="pixel.png"`}})
}

func detail(t *testing.T, res *http.Response) string {
	t.Helper()
	var problem struct{ Detail string }
	raw, _ := io.ReadAll(res.Body)
	_ = json.Unmarshal(raw, &problem)
	return problem.Detail
}

func TestNetworkFileHostDescribesItself(t *testing.T) {
	h := newNetworkHost(t, true, nil, nil)
	res := h.do(t, "OPTIONS", h.files_(), "", "", nil, nil)
	if res.StatusCode != 204 || !strings.Contains(res.Header.Get("Accept-Post"), "image/*") {
		t.Fatalf("discovery: %d %q", res.StatusCode, res.Header.Get("Accept-Post"))
	}
	if h.checks != 0 {
		t.Fatal("discovery checked a login")
	}
}

func TestNetworkFileHostIsOffUnlessServed(t *testing.T) {
	h := newNetworkHost(t, false, nil, nil)
	if res := h.do(t, "OPTIONS", h.files_(), "", "", nil, nil); res.StatusCode < 400 {
		t.Fatalf("discovery answered while switched off: %d", res.StatusCode)
	}
	if res := h.post(t, "alice", "alice-secret", pngBytes(t, 2, 2)); res.StatusCode < 400 {
		t.Fatalf("upload accepted while switched off: %d", res.StatusCode)
	}
}

func TestNetworkFileHostUploadAndDownload(t *testing.T) {
	h := newNetworkHost(t, true, nil, nil)
	data := pngBytes(t, 3, 2)
	res := h.post(t, "alice", "alice-secret", data)
	if res.StatusCode != 201 {
		t.Fatalf("upload: %d %s", res.StatusCode, detail(t, res))
	}
	location := res.Header.Get("Location")
	if !strings.HasPrefix(location, h.server.URL+"/files/") || !strings.HasSuffix(location, "/pixel.png") {
		t.Fatalf("location %q", location)
	}
	got := h.do(t, "GET", location, "", "", nil, nil)
	body, _ := io.ReadAll(got.Body)
	if got.StatusCode != 200 || !bytes.Equal(body, data) {
		t.Fatalf("download: %d", got.StatusCode)
	}
	// The sending server records the message, so this one never sees a use for
	// the file; arriving through the file host must keep it.
	var owner string
	var expires int64
	err := h.store.DB.QueryRow("SELECT f.owner,u.expires_ns FROM files f JOIN file_uses u ON u.id=f.id").Scan(&owner, &expires)
	if err != nil || owner != "id-alice" || expires != 0 {
		t.Fatalf("owner %q, use %d: %v", owner, expires, err)
	}
	if removed, err := h.files.SweepUnused(context.Background(), time.Nanosecond); err != nil || removed != 0 {
		t.Fatalf("sweep removed a file received through the host: %d %v", removed, err)
	}
}

func TestNetworkFileHostRefusals(t *testing.T) {
	data := pngBytes(t, 2, 2)
	t.Run("no login", func(t *testing.T) {
		h := newNetworkHost(t, true, nil, nil)
		res := h.post(t, "", "", data)
		if res.StatusCode != 401 || !strings.HasPrefix(res.Header.Get("WWW-Authenticate"), "Basic") {
			t.Fatalf("%d", res.StatusCode)
		}
	})
	t.Run("wrong password", func(t *testing.T) {
		h := newNetworkHost(t, true, nil, nil)
		if res := h.post(t, "alice", "wrong", data); res.StatusCode != 401 {
			t.Fatalf("%d", res.StatusCode)
		}
	})
	t.Run("an IRC account with no account here", func(t *testing.T) {
		h := newNetworkHost(t, true, nil, nil)
		res := h.post(t, "carol", "carol-secret", data)
		if res.StatusCode != 403 || !strings.Contains(detail(t, res), "no account") {
			t.Fatalf("%d", res.StatusCode)
		}
	})
	t.Run("an account outside the allowed list", func(t *testing.T) {
		h := newNetworkHost(t, true, []string{"Alice"}, nil)
		if res := h.post(t, "bob", "bob-secret", data); res.StatusCode != 403 {
			t.Fatalf("unlisted: %d", res.StatusCode)
		}
		if res := h.post(t, "alice", "alice-secret", data); res.StatusCode != 201 {
			t.Fatalf("listed, other case: %d", res.StatusCode)
		}
	})
	t.Run("a type that is not accepted", func(t *testing.T) {
		h := newNetworkHost(t, true, nil, func(o *filehost.Options) { o.Types = []string{"image/png"} })
		if res := h.post(t, "alice", "alice-secret", []byte("plain text")); res.StatusCode != 415 {
			t.Fatalf("%d", res.StatusCode)
		}
	})
	t.Run("a file over the size limit", func(t *testing.T) {
		h := newNetworkHost(t, true, nil, func(o *filehost.Options) { o.MaxBytes = 64 })
		if res := h.post(t, "alice", "alice-secret", pngBytes(t, 40, 40)); res.StatusCode != 413 {
			t.Fatalf("%d", res.StatusCode)
		}
	})
	t.Run("no file", func(t *testing.T) {
		h := newNetworkHost(t, true, nil, nil)
		if res := h.post(t, "alice", "alice-secret", nil); res.StatusCode != 400 {
			t.Fatalf("%d", res.StatusCode)
		}
	})
	t.Run("past the account's quota, with the reason", func(t *testing.T) {
		h := newNetworkHost(t, true, nil, func(o *filehost.Options) { o.QuotaFiles = 1 })
		if res := h.post(t, "alice", "alice-secret", data); res.StatusCode != 201 {
			t.Fatalf("first: %d", res.StatusCode)
		}
		res := h.post(t, "alice", "alice-secret", data)
		if text := detail(t, res); res.StatusCode != 413 || !strings.Contains(text, "limit of 1 uploaded files") {
			t.Fatalf("second: %d %q", res.StatusCode, text)
		}
		if res = h.post(t, "bob", "bob-secret", data); res.StatusCode != 201 {
			t.Fatalf("another account shares the quota: %d", res.StatusCode)
		}
	})
}

func TestNetworkFileHostLimitsWrongLogins(t *testing.T) {
	h := newNetworkHost(t, true, nil, nil)
	data := pngBytes(t, 2, 2)
	for i := 0; i < fileHostPairFailures; i++ {
		if res := h.post(t, "alice", "wrong", data); res.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, res.StatusCode)
		}
	}
	checks := h.checks
	// Even the right password is refused now, without asking the IRC server.
	if res := h.post(t, "alice", "alice-secret", data); res.StatusCode != 429 || h.checks != checks {
		t.Fatalf("after the limit: %d, %d checks", res.StatusCode, h.checks-checks)
	}
	// Another account from the same address is not locked out by one account's failures.
	if res := h.post(t, "bob", "bob-secret", data); res.StatusCode != 201 {
		t.Fatalf("other account: %d", res.StatusCode)
	}
}

func TestNetworkFileHostDeletion(t *testing.T) {
	h := newNetworkHost(t, true, nil, nil)
	location := h.post(t, "alice", "alice-secret", pngBytes(t, 2, 2)).Header.Get("Location")
	if res := h.do(t, "DELETE", location, "", "", nil, nil); res.StatusCode != 401 {
		t.Fatalf("without login: %d", res.StatusCode)
	}
	if res := h.do(t, "DELETE", location, "bob", "bob-secret", nil, nil); res.StatusCode != 404 {
		t.Fatalf("another account's login: %d", res.StatusCode)
	}
	if res := h.do(t, "DELETE", strings.TrimSuffix(location, "/pixel.png")[:len(location)-len("/pixel.png")-3]+"abc/pixel.png", "alice", "alice-secret", nil, nil); res.StatusCode != 404 {
		t.Fatalf("wrong token: %d", res.StatusCode)
	}
	if res := h.do(t, "GET", location, "", "", nil, nil); res.StatusCode != 200 {
		t.Fatalf("a refused deletion removed the file: %d", res.StatusCode)
	}
	if res := h.do(t, "DELETE", location, "alice", "alice-secret", nil, nil); res.StatusCode != 204 {
		t.Fatalf("owner: %d", res.StatusCode)
	}
	if res := h.do(t, "GET", location, "", "", nil, nil); res.StatusCode != 404 {
		t.Fatalf("deleted file still served: %d", res.StatusCode)
	}
	if res := h.do(t, "DELETE", location, "alice", "alice-secret", nil, nil); res.StatusCode != 404 {
		t.Fatalf("deleting twice: %d", res.StatusCode)
	}
}

// sender is a server that passes message uploads on to a network file host.
func newSender(t *testing.T, h *networkHost, trusted ...string) (*filehost.Service, *storage.Store) {
	t.Helper()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err = store.CreateAccount("sender", "alice"); err != nil {
		t.Fatal(err)
	}
	files, err := filehost.New(filehost.Options{Store: store, Client: h.server.Client(), TrustedHosts: trusted})
	if err != nil {
		t.Fatal(err)
	}
	files.UseCredentials(func(owner, network string) (string, string, error) {
		if owner != "sender" || network != "net" {
			return "", "", errors.New("no login")
		}
		return "alice", "alice-secret", nil
	})
	return files, store
}

func TestMessageUploadReachesTheNetworkFileHostAndIsDeletedThere(t *testing.T) {
	h := newNetworkHost(t, true, nil, nil)
	host := strings.TrimPrefix(h.server.URL, "https://")
	host = host[:strings.LastIndex(host, ":")]
	sender, store := newSender(t, h, host)
	ctx := context.Background()

	provider, ok := sender.Network(ctx, h.files_(), "alice", "alice-secret")
	if !ok || !provider.Available || !provider.CanDelete || provider.Provider != "upstream" {
		t.Fatalf("provider %+v %v", provider, ok)
	}
	upload, err := sender.Upload(ctx, "sender", "net", "pixel.png", pngBytes(t, 3, 2), provider)
	if err != nil || !strings.HasPrefix(upload.URL, h.server.URL+"/files/") {
		t.Fatalf("upload %+v %v", upload, err)
	}
	if res := h.do(t, "GET", upload.URL, "", "", nil, nil); res.StatusCode != 200 {
		t.Fatalf("viewers cannot load the file from the network host: %d", res.StatusCode)
	}
	// What the sender keeps is a record, not the bytes.
	var backend string
	if err = store.DB.QueryRow("SELECT backend FROM files WHERE id=?", upload.ID).Scan(&backend); err != nil || backend != "upstream" {
		t.Fatalf("record %q %v", backend, err)
	}
	if id := sender.DeletableUpload("sender", upload.URL); id != upload.ID {
		t.Fatalf("an author cannot delete a message's file kept on the network host: %q", id)
	}
	if err = sender.Delete(ctx, "sender", upload.ID); err != nil {
		t.Fatal(err)
	}
	if res := h.do(t, "GET", upload.URL, "", "", nil, nil); res.StatusCode != 404 {
		t.Fatalf("deleted file is still on the network host: %d", res.StatusCode)
	}
	if _, err = sender.Record("sender", upload.ID); !errors.Is(err, filehost.ErrNotFound) {
		t.Fatal("record kept after deletion")
	}
}

func TestMessageUploadPassesTheNetworkHostsRefusalOn(t *testing.T) {
	h := newNetworkHost(t, true, nil, func(o *filehost.Options) { o.QuotaFiles = 1 })
	host := strings.TrimPrefix(h.server.URL, "https://")
	sender, _ := newSender(t, h, host[:strings.LastIndex(host, ":")])
	ctx := context.Background()
	provider, _ := sender.Network(ctx, h.files_(), "alice", "alice-secret")
	if _, err := sender.Upload(ctx, "sender", "net", "one.png", pngBytes(t, 2, 2), provider); err != nil {
		t.Fatal(err)
	}
	_, err := sender.Upload(ctx, "sender", "net", "two.png", pngBytes(t, 2, 3), provider)
	if !errors.Is(err, filehost.ErrQuota) || !strings.Contains(err.Error(), "limit of 1 uploaded files") {
		t.Fatalf("the host's quota sentence was lost: %v", err)
	}
	provider.Password = "wrong"
	_, err = sender.Upload(ctx, "sender", "net", "three.png", pngBytes(t, 2, 4), provider)
	if err == nil || !strings.Contains(err.Error(), "Wrong IRC login") {
		t.Fatalf("the host's refusal was lost: %v", err)
	}
}

func TestMessageUploadStaysLocalUnlessTheNetworkHostIsTrustedAndOpen(t *testing.T) {
	ctx := context.Background()
	t.Run("host not in the trusted list", func(t *testing.T) {
		h := newNetworkHost(t, true, nil, nil)
		sender, _ := newSender(t, h)
		if _, ok := sender.Network(ctx, h.files_(), "alice", "alice-secret"); ok {
			t.Fatal("a host nobody trusted with the login was chosen")
		}
	})
	host := func(h *networkHost) string {
		v := strings.TrimPrefix(h.server.URL, "https://")
		return v[:strings.LastIndex(v, ":")]
	}
	t.Run("nothing advertised, or no login to send", func(t *testing.T) {
		h := newNetworkHost(t, true, nil, nil)
		sender, _ := newSender(t, h, host(h))
		if _, ok := sender.Network(ctx, "", "alice", "alice-secret"); ok {
			t.Fatal("no advertised host")
		}
		if _, ok := sender.Network(ctx, h.files_(), "", ""); ok {
			t.Fatal("anonymous upload to the network host")
		}
	})
	t.Run("the host has switched its door off", func(t *testing.T) {
		h := newNetworkHost(t, false, nil, nil)
		sender, _ := newSender(t, h, host(h))
		if _, ok := sender.Network(ctx, h.files_(), "alice", "alice-secret"); ok {
			t.Fatal("did not fall back to local storage")
		}
	})
	t.Run("the host is down", func(t *testing.T) {
		h := newNetworkHost(t, true, nil, nil)
		sender, _ := newSender(t, h, host(h))
		url := h.files_()
		h.server.Close()
		p, ok := sender.Network(ctx, url, "alice", "alice-secret")
		if !ok || p.Available || p.Reason == "" {
			t.Fatalf("a host that is down must fail with a reason, not move the file: %+v %v", p, ok)
		}
	})
}

func TestUnusedFilesOnTheNetworkHostAreSweptAndUsedOnesKept(t *testing.T) {
	h := newNetworkHost(t, true, nil, nil)
	host := strings.TrimPrefix(h.server.URL, "https://")
	sender, _ := newSender(t, h, host[:strings.LastIndex(host, ":")])
	ctx := context.Background()
	provider, _ := sender.Network(ctx, h.files_(), "alice", "alice-secret")
	unused, err := sender.Upload(ctx, "sender", "net", "unused.png", pngBytes(t, 2, 2), provider)
	if err != nil {
		t.Fatal(err)
	}
	used, err := sender.Upload(ctx, "sender", "net", "used.png", pngBytes(t, 2, 3), provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sender.Attachment("sender", "net", used.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	removed, err := sender.SweepUnused(ctx, time.Millisecond)
	if err != nil || removed != 1 {
		t.Fatalf("sweep removed %d: %v", removed, err)
	}
	if res := h.do(t, "GET", unused.URL, "", "", nil, nil); res.StatusCode != 404 {
		t.Fatalf("an unsent file stayed on the network host: %d", res.StatusCode)
	}
	if res := h.do(t, "GET", used.URL, "", "", nil, nil); res.StatusCode != 200 {
		t.Fatalf("a sent file was removed: %d", res.StatusCode)
	}
}

func TestDeletingAnAccountDeletesItsFilesOnTheNetworkHost(t *testing.T) {
	h := newNetworkHost(t, true, nil, nil)
	host := strings.TrimPrefix(h.server.URL, "https://")
	sender, _ := newSender(t, h, host[:strings.LastIndex(host, ":")])
	ctx := context.Background()
	provider, _ := sender.Network(ctx, h.files_(), "alice", "alice-secret")
	upload, err := sender.Upload(ctx, "sender", "net", "pixel.png", pngBytes(t, 2, 2), provider)
	if err != nil {
		t.Fatal(err)
	}
	// The logins are read before the account's connections are dropped.
	logins := func(owner, network string) (string, string, error) { return "alice", "alice-secret", nil }
	if err = sender.DeleteAccountUsing(ctx, "sender", logins); err != nil {
		t.Fatal(err)
	}
	if res := h.do(t, "GET", upload.URL, "", "", nil, nil); res.StatusCode != 404 {
		t.Fatalf("file left on the network host: %d", res.StatusCode)
	}
}

func TestMessageUploadMarkerIsValidatedAndOptional(t *testing.T) {
	_, server, _, _ := authenticatedApp(t)
	client := cookieClient()
	register(t, client, server.URL)
	post := func(query string) int {
		var body bytes.Buffer
		body.WriteString("--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"p.png\"\r\nContent-Type: image/png\r\n\r\n")
		body.Write(pngBytes(t, 2, 2))
		body.WriteString("\r\n--b--\r\n")
		req, _ := http.NewRequest("POST", server.URL+"/api/v1/uploads"+query, &body)
		req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}
	if got := post(""); got != 201 {
		t.Fatalf("no marker: %d", got)
	}
	if got := post("?use=message"); got != 201 {
		t.Fatalf("a message upload with nothing to prefer stays on this server: %d", got)
	}
	if got := post("?use=profile"); got != 400 {
		t.Fatalf("unknown marker: %d", got)
	}
}
