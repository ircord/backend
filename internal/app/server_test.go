package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ircord/backend/internal/storage"
)

func cleanEnvironment(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "IRCORD_") {
			t.Setenv(name, "")
		}
	}
}

func TestInvalidConfigurationDoesNotOpenStorage(t *testing.T) {
	for _, settings := range []map[string]string{
		{"IRCORD_PUBLIC_URL": "ftp://example.invalid"},
		{"IRCORD_TRUSTED_PROXIES": "not-a-network"},
		{"IRCORD_REQUIRE_REGISTRATION_KEY": "true"},
		{"IRCORD_NETWORK_PASSWORDS": "{}", "IRCORD_NETWORK_PASSWORDS_FILE": "missing"},
		{"IRCORD_LINKMEDIA_TIMEOUT": "invalid"},
		{"IRCORD_FILE_STORAGE": "unknown"},
	} {
		t.Run(fmtSettings(settings), func(t *testing.T) {
			cleanEnvironment(t)
			dataDir := filepath.Join(t.TempDir(), "state")
			t.Setenv("IRCORD_DATA_DIR", dataDir)
			for name, value := range settings {
				t.Setenv(name, value)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := Run(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("invalid configuration did not fail before startup: %v", err)
			}
			if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid configuration acquired storage: %v", err)
			}
		})
	}
}

// Names alone keep configuration values out of test output.
func fmtSettings(settings map[string]string) string {
	for name := range settings {
		return name
	}
	return "defaults"
}

func TestHTTPShutdownClosesListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listening := make(chan string, 1)
	server := &http.Server{Addr: "127.0.0.1:0", Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ready") }), BaseContext: func(listener net.Listener) context.Context {
		listening <- listener.Addr().String()
		return context.Background()
	}}
	done := make(chan error, 1)
	go func() { done <- serve(ctx, server) }()
	var address string
	select {
	case address = <-listening:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not start")
	}
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Get("http://" + address)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal(response.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not return")
	}
	if conn, err := net.DialTimeout("tcp", address, time.Second); err == nil {
		conn.Close()
		t.Fatal("listener survived shutdown")
	}
}

func TestRunServesWhileCredentialRecoveryWaits(t *testing.T) {
	cleanEnvironment(t)
	dataDir := t.TempDir()
	store, err := storage.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	err = store.SaveCredentialOperation("recover", []byte(`{"Kind":"signup","Owner":"recover","Username":"recover","Password":"test-password"}`))
	store.Close()
	if err != nil {
		t.Fatal(err)
	}
	recoveryStarted := make(chan struct{}, 1)
	releaseRecovery := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ns/saregister" {
			http.NotFound(w, r)
			return
		}
		recoveryStarted <- struct{}{}
		select {
		case <-releaseRecovery:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(releaseRecovery)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	t.Setenv("IRCORD_DATA_DIR", dataDir)
	t.Setenv("IRCORD_LISTEN", address)
	t.Setenv("IRCORD_ERGO_SERVER", "127.0.0.1:1")
	t.Setenv("IRCORD_ERGO_API_URL", upstream.URL)
	t.Setenv("IRCORD_ERGO_API_TOKEN", strings.Repeat("a", 32))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("backend did not stop")
		}
	}()
	select {
	case <-recoveryStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("pending credentials were not retried at startup")
	}
	// The associated API call has a two-second timeout and is still pending.
	checkCtx, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	request, err := http.NewRequestWithContext(checkCtx, http.MethodGet, "http://"+address+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{}
	defer client.CloseIdleConnections()
	for checkCtx.Err() == nil {
		response, err := client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("backend health check returned %d", response.StatusCode)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("backend did not serve requests while credential recovery was pending")
}

func TestHTTPBindFailureReturns(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := serve(ctx, &http.Server{Addr: listener.Addr().String()}); err == nil || ctx.Err() != nil {
		t.Fatalf("listener failure was not reported immediately: %v", err)
	}
}
