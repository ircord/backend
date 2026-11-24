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
	"ircord/backend/internal/filehost"
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

// loginFailures counts wrong logins to the file host.
type loginFailures struct {
	mu     sync.Mutex
	counts map[string]failure
}

type failure struct {
	count int
	until time.Time
}

// blocked reports whether the address, or the address and account together,
// have used up their wrong logins.
func (l *loginFailures) blocked(address, account string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	return l.current("address "+address, now) >= fileHostAddressFailures || l.current("pair "+address+" "+account, now) >= fileHostPairFailures
}

func (l *loginFailures) current(key string, now time.Time) int {
	f, ok := l.counts[key]
	if !ok || now.After(f.until) {
		return 0
	}
	return f.count
}

// failed records a wrong login. When too many keys are tracked, the one that
// expires soonest is dropped, so a flood of new keys cannot refuse everyone.
func (l *loginFailures) failed(address, account string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.counts == nil {
		l.counts = map[string]failure{}
	}
	for _, key := range []string{"address " + address, "pair " + address + " " + account} {
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
	}
}

func (s *Server) clientAddress(r *http.Request) string {
	if s.options.Auth != nil {
		return s.options.Auth.ClientIP(r)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// fileHostAccount signs a request in with its Basic login and returns the
// account on this server that owns what the request uploads or deletes. It
// answers the request itself when it cannot.
func (s *Server) fileHostAccount(w http.ResponseWriter, r *http.Request) (string, bool) {
	user, password, ok := r.BasicAuth()
	if !ok || user == "" || len(user) > 100 {
		w.Header().Set("WWW-Authenticate", `Basic realm="IRCord file host", charset="UTF-8"`)
		writeProblem(w, "Sign in with your IRC login to use the file host.", http.StatusUnauthorized)
		return "", false
	}
	address, name := s.clientAddress(r), strings.ToLower(user)
	if s.fileHostFailures.blocked(address, name) {
		w.Header().Set("Retry-After", "900")
		writeProblem(w, "Too many failed logins. Try again later.", http.StatusTooManyRequests)
		return "", false
	}
	valid, err := s.options.Ergo.CheckPassword(r.Context(), user, password)
	if err != nil {
		writeProblem(w, "The IRC server's logins are unavailable. Try again shortly.", http.StatusServiceUnavailable)
		return "", false
	}
	if !valid {
		s.fileHostFailures.failed(address, name)
		w.Header().Set("WWW-Authenticate", `Basic realm="IRCord file host", charset="UTF-8"`)
		writeProblem(w, "Wrong IRC login.", http.StatusUnauthorized)
		return "", false
	}
	if len(s.options.FileHost.Accounts) > 0 && !slices.ContainsFunc(s.options.FileHost.Accounts, func(a string) bool { return strings.EqualFold(a, user) }) {
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
	api.AddOperation(&huma.Operation{OperationID: "filehost-options", Method: "OPTIONS", Path: "/files/", Summary: "Describe the network file host", Description: "The IRC network's file host (draft/FILEHOST). Answers without a login. Accept-Post lists the accepted types." + only, Responses: map[string]*huma.Response{"204": {Description: "Accept-Post names the accepted media types."}}})
	api.AddOperation(&huma.Operation{OperationID: "filehost-upload", Method: "POST", Path: "/files/", Summary: "Upload a file to the network file host", Description: "The body is the file. Authorization is HTTP Basic with the IRC account name and password, which the associated IRC server checks. The file belongs to this server's account of the same name and counts against its upload quota. Content-Disposition names the file. 201 returns the file's address in Location. 401 is a wrong login, 403 an account that may not upload here, 413 a file that is too large or over quota (the body says which), 415 a type this server does not accept, and 429 too many wrong logins or too many uploads at once." + only, RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{"application/octet-stream": {Schema: &huma.Schema{Type: "string", Format: "binary"}}}}, Responses: map[string]*huma.Response{"201": {Description: "Stored. Location is the file's address."}, "400": {Description: "No file, or an invalid file name."}, "401": {Description: "Missing or wrong IRC login."}, "403": {Description: "The account may not upload here."}, "413": {Description: "File too large, or the upload quota is reached."}, "415": {Description: "File type not accepted."}, "429": {Description: "Too many wrong logins, or too many uploads at once."}, "503": {Description: "Logins or storage are unavailable."}}})
	api.AddOperation(&huma.Operation{OperationID: "filehost-delete", Method: "DELETE", Path: "/files/{id}/{token}/{filename}", Summary: "Delete a file from the network file host", Description: "An IRCord addition to the standard. Authorization is HTTP Basic with the IRC login of the account that uploaded the file. 204 when deleted, 404 when no such file belongs to that account." + only, Parameters: []*huma.Param{
		{Name: "id", In: "path", Required: true, Schema: &huma.Schema{Type: "string"}},
		{Name: "token", In: "path", Required: true, Schema: &huma.Schema{Type: "string"}},
		{Name: "filename", In: "path", Required: true, Schema: &huma.Schema{Type: "string"}},
	}, Responses: map[string]*huma.Response{"204": {Description: "Deleted."}, "401": {Description: "Missing or wrong IRC login."}, "404": {Description: "No such file for this account."}, "429": {Description: "Too many wrong logins."}}})
	if !s.options.FileHost.Serve || s.options.Ergo == nil || s.options.Store == nil {
		return
	}
	router.Options("/files/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Post", s.options.Files.AcceptPost())
		w.Header().Set("Allow", "OPTIONS, POST")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})
	router.Post("/files/", s.hostUpload)
	router.Delete("/files/{id}/{token}/*", s.hostDelete)
}

func (s *Server) hostUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	account, ok := s.fileHostAccount(w, r)
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
	if r.ContentLength > local.MaxBytes {
		writeProblem(w, "file exceeds the upload limit", http.StatusRequestEntityTooLarge)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, local.MaxBytes))
	if err != nil {
		writeProblem(w, "file exceeds the upload limit", http.StatusRequestEntityTooLarge)
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
	account, ok := s.fileHostAccount(w, r)
	if !ok {
		return
	}
	err := s.options.Files.DeleteReceived(r.Context(), account, chi.URLParam(r, "id"), chi.URLParam(r, "token"))
	switch {
	case errors.Is(err, filehost.ErrNotFound):
		writeProblem(w, "No such file for this account.", http.StatusNotFound)
	case err != nil:
		writeProblem(w, "The file could not be deleted. Try again shortly.", http.StatusBadGateway)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
