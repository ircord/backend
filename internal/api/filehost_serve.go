package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"ircord/backend/internal/auth"
	"ircord/backend/internal/filehost"
	"ircord/backend/internal/irc"
	"ircord/backend/internal/storage"
)

// This file serves the IRC network's file host: the draft/FILEHOST standard
// (OPTIONS and POST at an address the IRC server advertises), plus a DELETE
// that the standard lacks. People sign in with their IRC login, which the
// associated IRC server checks. The file belongs to this server's account of
// the same name and counts against that account's quota.

const (
	// A login is tried against this server's IRC server at most this many
	// times per window with a wrong answer, from one address and account, or
	// from one address in all.
	fileHostPairFailures    = 10
	fileHostAddressFailures = 100
	fileHostFailureWindow   = 15 * time.Minute
	fileHostTrackedFailures = 10000
)

// loginFailures counts logins to the file host that have not yet proved right.
type loginFailures struct {
	mu     sync.Mutex
	counts map[string]failure
}

type failure struct {
	count int
	until time.Time
}

// begin counts a login attempt before it is checked, as sign-in does, so that
// attempts made at the same moment cannot all pass a limit that one of them
// would use up. It reports false when the address, or the address and account
// together, have used up their wrong logins. When too many keys are tracked,
// the one that expires soonest is dropped, so a flood of new keys cannot
// refuse everyone.
func (l *loginFailures) begin(address, account string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.counts == nil {
		l.counts = map[string]failure{}
	}
	// An account that has used up its attempts is refused without touching the
	// address budget, so one account with a wrong password cannot lock out the
	// others behind the same address, such as every user of a relaying server.
	if f, ok := l.counts["pair "+address+" "+account]; ok && !now.After(f.until) && f.count >= fileHostPairFailures {
		return false
	}
	within := true
	for key, max := range map[string]int{"address " + address: fileHostAddressFailures, "pair " + address + " " + account: fileHostPairFailures} {
		if _, tracked := l.counts[key]; !tracked && len(l.counts) >= fileHostTrackedFailures {
			oldest, first := "", time.Time{}
			for k, f := range l.counts {
				if now.After(f.until) {
					delete(l.counts, k)
				} else if oldest == "" || f.until.Before(first) {
					oldest, first = k, f.until
				}
			}
			if len(l.counts) >= fileHostTrackedFailures {
				delete(l.counts, oldest)
			}
		}
		f := l.counts[key]
		if f.until.IsZero() || now.After(f.until) {
			f = failure{until: now.Add(fileHostFailureWindow)}
		}
		f.count++
		l.counts[key] = f
		within = within && f.count <= max
	}
	return within
}

// proved gives an attempt back once its login turned out right, or could not
// be checked at all.
func (l *loginFailures) proved(address, account string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, key := range []string{"address " + address, "pair " + address + " " + account} {
		if f, ok := l.counts[key]; ok && f.count > 0 {
			f.count--
			l.counts[key] = f
		}
	}
}

// loginKey is the name that a login is counted under. It folds case and the
// IRC characters {}| and []\ together, as the IRC server does, so one account has
// one limit.
func loginKey(name string) string {
	return strings.NewReplacer("[", "{", "]", "}", "\\", "|", "~", "^").Replace(strings.ToLower(name))
}

// clientAddress is what logins are limited by: the client's address, or its
// /64 for IPv6, as for sign-in.
func (s *Server) clientAddress(r *http.Request) string {
	if s.options.Auth != nil {
		return s.options.Auth.ClientLimitKey(r)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// fileHostAccount signs a request in with its Basic login and returns the
// account on this server that owns what the request uploads or deletes. It
// answers the request itself when it cannot. Only an upload is held to the list
// of accounts that may upload: anyone may delete their own files.
func (s *Server) fileHostAccount(w http.ResponseWriter, r *http.Request, upload bool) (string, bool) {
	user, password, ok := r.BasicAuth()
	if !ok || user == "" || len(user) > 100 {
		w.Header().Set("WWW-Authenticate", `Basic realm="IRCord file host", charset="UTF-8"`)
		writeProblem(w, "Sign in with your IRC login to use the file host.", http.StatusUnauthorized)
		return "", false
	}
	// Only names that can be accounts here are sent on. The IRC server folds names its
	// own way, so other spellings of one name would each get their own limit.
	if auth.UsernameProblem(user) != "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="IRCord file host", charset="UTF-8"`)
		writeProblem(w, "Wrong IRC login.", http.StatusUnauthorized)
		return "", false
	}
	address, name := s.clientAddress(r), loginKey(user)
	if !s.fileHostFailures.begin(address, name) {
		w.Header().Set("Retry-After", "900")
		writeProblem(w, "Too many failed logins. Try again later.", http.StatusTooManyRequests)
		return "", false
	}
	valid, err := s.options.Ergo.CheckPassword(r.Context(), user, password)
	if err != nil {
		s.fileHostFailures.proved(address, name)
		writeProblem(w, "The IRC server's logins are unavailable. Try again shortly.", http.StatusServiceUnavailable)
		return "", false
	}
	if !valid {
		w.Header().Set("WWW-Authenticate", `Basic realm="IRCord file host", charset="UTF-8"`)
		writeProblem(w, "Wrong IRC login.", http.StatusUnauthorized)
		return "", false
	}
	s.fileHostFailures.proved(address, name)
	if upload && len(s.options.FileHost.Accounts) > 0 && !slices.ContainsFunc(s.options.FileHost.Accounts, func(a string) bool { return strings.EqualFold(a, user) }) {
		writeProblem(w, "This file host is not open to your account.", http.StatusForbidden)
		return "", false
	}
	account, _, err := s.options.Store.AccountByUsername(user)
	if errors.Is(err, storage.ErrNotFound) {
		writeProblem(w, "You have no account on this server. Sign in here once, then try again.", http.StatusForbidden)
		return "", false
	}
	if err != nil {
		writeProblem(w, "Account storage is unavailable. Try again shortly.", http.StatusServiceUnavailable)
		return "", false
	}
	return account.ID, true
}

func (s *Server) registerFileHost(router chi.Router) {
	if s.options.Files == nil {
		return
	}
	// The contract is documented on every server; only one that its operator set
	// up as the network's file host answers it.
	const only = " Only a server set up as the IRC network's file host answers this; others reply 404."
	api := s.API.OpenAPI()
	api.AddOperation(&huma.Operation{OperationID: "filehost-options", Method: "OPTIONS", Path: "/files/", Summary: "Describe the network file host", Description: "The IRC network's file host (draft/FILEHOST). Answers without a login. Accept-Post lists the accepted types. Allow lists DELETE: senders read it to learn that this host deletes files on request, which the standard has no word for." + only, Responses: map[string]*huma.Response{"204": {Description: "Accept-Post names the accepted media types."}}})
	api.AddOperation(&huma.Operation{OperationID: "filehost-upload", Method: "POST", Path: "/files/", Summary: "Upload a file to the network file host", Description: "The body is the file. Authorization is HTTP Basic with the IRC account name and password, which the associated IRC server checks. The file belongs to this server's account of the same name and counts against its upload quota. Content-Disposition names the file. 201 returns the file's address in Location. 401 is a wrong login, 403 an account that may not upload here, 413 a file that is too large or over quota (the body says which), 415 a type this server does not accept, and 429 too many wrong logins or too many uploads at once." + only, RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{"application/octet-stream": {Schema: &huma.Schema{Type: "string", Format: "binary"}}}}, Responses: map[string]*huma.Response{"201": {Description: "Stored. Location is the file's address."}, "400": {Description: "No file, or an invalid file name."}, "401": {Description: "Missing or wrong IRC login."}, "403": {Description: "The account may not upload here."}, "413": {Description: "File too large, or the upload quota is reached."}, "415": {Description: "File type not accepted."}, "429": {Description: "Too many wrong logins, or too many uploads at once."}, "503": {Description: "Logins or storage are unavailable."}}})
	api.AddOperation(&huma.Operation{OperationID: "filehost-delete", Method: "DELETE", Path: "/files/{id}/{token}/{filename}", Summary: "Delete a file from the network file host", Description: "An IRCord addition to the standard. Authorization is HTTP Basic with the IRC login of the account that uploaded the file. 204 when deleted, 404 when there is no such file or the token is wrong, and 403 when another account owns it. The list of accounts that may upload does not apply: an account can always delete its own files. A server that is not the network's file host replies 405.", Parameters: []*huma.Param{
		{Name: "id", In: "path", Required: true, Schema: &huma.Schema{Type: "string"}},
		{Name: "token", In: "path", Required: true, Schema: &huma.Schema{Type: "string"}},
		{Name: "filename", In: "path", Required: true, Schema: &huma.Schema{Type: "string"}},
	}, Responses: map[string]*huma.Response{"204": {Description: "Deleted."}, "401": {Description: "Missing or wrong IRC login."}, "403": {Description: "The file belongs to another account, or this server has no account for the login."}, "404": {Description: "No such file, or a wrong token."}, "429": {Description: "Too many wrong logins."}, "502": {Description: "The file could not be deleted from storage. Try again."}, "503": {Description: "Logins or accounts are unavailable."}}})
	if !s.options.FileHost.Serve || s.options.Ergo == nil || s.options.Store == nil {
		return
	}
	router.Options("/files/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Post", s.options.Files.AcceptPost())
		w.Header().Set("Allow", "OPTIONS, POST, DELETE")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})
	router.Post("/files/", s.hostUpload)
	router.Delete("/files/{id}/{token}/*", s.hostDelete)
}

func (s *Server) hostUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	account, ok := s.fileHostAccount(w, r, true)
	if !ok {
		return
	}
	release, ok := s.uploads.acquire(account)
	if !ok {
		writeProblem(w, "upload capacity reached; try again shortly", http.StatusTooManyRequests)
		return
	}
	defer release()
	local := s.options.Files.LocalProvider()
	if !local.Available {
		writeProblem(w, local.Reason, http.StatusServiceUnavailable)
		return
	}
	// A file relayed over a slow line takes longer than the server's usual limit for reading a request.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(irc.UploadTimeout))
	if r.ContentLength > local.MaxBytes {
		writeProblem(w, "file exceeds the upload limit", http.StatusRequestEntityTooLarge)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, local.MaxBytes))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeProblem(w, "file exceeds the upload limit", http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		writeProblem(w, "the upload was interrupted", http.StatusBadRequest)
		return
	}
	if len(data) == 0 {
		writeProblem(w, "file is required", http.StatusBadRequest)
		return
	}
	name := "file"
	if _, params, err := mime.ParseMediaType(r.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
		name = params["filename"]
	}
	upload, err := s.options.Files.Receive(r.Context(), account, name, data)
	switch {
	case errors.Is(err, filehost.ErrQuota):
		writeProblem(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	case errors.Is(err, filehost.ErrType):
		writeProblem(w, err.Error(), http.StatusUnsupportedMediaType)
		return
	case errors.Is(err, filehost.ErrSize):
		writeProblem(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	case errors.Is(err, filehost.ErrUnavailable):
		writeProblem(w, err.Error(), http.StatusServiceUnavailable)
		return
	case err != nil:
		writeProblem(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Location", upload.URL)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(upload)
}

func (s *Server) hostDelete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	account, ok := s.fileHostAccount(w, r, false)
	if !ok {
		return
	}
	err := s.options.Files.DeleteReceived(r.Context(), account, chi.URLParam(r, "id"), chi.URLParam(r, "token"))
	switch {
	case errors.Is(err, filehost.ErrUnavailable):
		writeProblem(w, "Storage is unavailable. Try again shortly.", http.StatusServiceUnavailable)
	case errors.Is(err, filehost.ErrNotFound):
		writeProblem(w, "No such file.", http.StatusNotFound)
	case errors.Is(err, filehost.ErrForbidden):
		writeProblem(w, "This file belongs to another account.", http.StatusForbidden)
	case err != nil:
		writeProblem(w, "The file could not be deleted. Try again shortly.", http.StatusBadGateway)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
