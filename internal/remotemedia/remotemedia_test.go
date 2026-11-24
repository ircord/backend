package remotemedia

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ircord/backend/internal/mediafetch"
	"ircord/backend/internal/model"
	"ircord/backend/internal/storage"
)

func pngBytes(t *testing.T, side int, shade uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, side, side))
	for i := range img.Pix {
		img.Pix[i] = shade
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type fixture struct {
	svc    *Service
	server *httptest.Server
	hits   atomic.Int32
	now    atomic.Int64
	body   atomic.Pointer[func(http.ResponseWriter, *http.Request)]
	dir    string
}

func (f *fixture) serve(h func(http.ResponseWriter, *http.Request)) { f.body.Store(&h) }

func (f *fixture) advance(d time.Duration) { f.now.Add(int64(d)) }

func newFixture(t *testing.T, mutate func(*Options)) *fixture {
	t.Helper()
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f := &fixture{dir: t.TempDir()}
	f.now.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		(*f.body.Load())(w, r)
	}))
	t.Cleanup(f.server.Close)
	o := Options{DB: store.DB, Dir: f.dir, Key: store.DeriveKey("remote-media/v1"), PublicURL: "https://chat.example.net", Client: f.server.Client(),
		Now: func() time.Time { return time.Unix(0, f.now.Load()) }}
	if mutate != nil {
		mutate(&o)
	}
	f.svc, err = New(o)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) key(t *testing.T, kind, path string) string {
	t.Helper()
	src := f.svc.Src(kind, f.server.URL+path)
	if !strings.HasPrefix(src, PathPrefix) {
		t.Fatalf("Src = %q, want a cached address", src)
	}
	return strings.TrimPrefix(src, PathPrefix)
}

func (f *fixture) open(key string) (string, []byte, error) {
	contentType, body, err := f.svc.Open(context.Background(), key)
	if err != nil {
		return "", nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	return contentType, data, err
}

func serveBytes(contentType string, data []byte) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(data)
	}
}

func TestSrcPassesSameOriginThrough(t *testing.T) {
	f := newFixture(t, nil)
	for _, address := range []string{
		"https://chat.example.net/files/1/token/avatar",
		"https://CHAT.example.net:443/avatars/defaults/avatar-003.png",
		"/avatars/defaults/avatar-003.png",
	} {
		if got := f.svc.Src(model.PictureAvatar, address); got != address {
			t.Fatalf("Src(%q) = %q, want it unchanged", address, got)
		}
	}
}

func TestSrcTurnsRemoteAddressIntoKey(t *testing.T) {
	f := newFixture(t, nil)
	remote := "https://other.example.org/files/1/token/avatar"
	src := f.svc.Src(model.PictureAvatar, remote)
	if !strings.HasPrefix(src, "/media/remote/") || len(src) != len("/media/remote/")+keyLength || strings.Contains(src, "other.example.org") {
		t.Fatalf("Src = %q", src)
	}
	if again := f.svc.Src(model.PictureAvatar, remote); again != src {
		t.Fatalf("Src is not deterministic: %q then %q", src, again)
	}
	if banner := f.svc.Src(model.PictureBanner, remote); banner == src {
		t.Fatal("kinds share a key")
	}
	for _, address := range []string{"", "http://other.example.org/a.png", "ftp://other.example.org/a.png", "https://user:pw@other.example.org/a.png", "javascript:alert(1)", "//other.example.org/a.png", "not a url"} {
		if got := f.svc.Src(model.PictureAvatar, address); got != "" {
			t.Fatalf("Src(%q) = %q, want empty", address, got)
		}
	}
	if got := f.svc.Src("other", remote); got != "" {
		t.Fatalf("Src with unknown kind = %q", got)
	}
	// A different instance key issues different addresses.
	other := newFixture(t, func(o *Options) { o.Key = []byte("another instance key") })
	if other.svc.Src(model.PictureAvatar, remote) == src {
		t.Fatal("keys do not depend on the instance key")
	}
}

func TestOpenRefusesUnknownKeys(t *testing.T) {
	f := newFixture(t, nil)
	f.serve(serveBytes("image/png", pngBytes(t, 8, 1)))
	_ = f.key(t, model.PictureAvatar, "/a.png")
	for _, key := range []string{"", "short", strings.Repeat("0", keyLength), strings.Repeat("g", keyLength), strings.Repeat("A", keyLength), strings.Repeat("0", keyLength+1), "../../etc/passwd"} {
		if _, _, err := f.open(key); err != ErrNotFound {
			t.Fatalf("Open(%q) error = %v, want ErrNotFound", key, err)
		}
	}
	if f.hits.Load() != 0 {
		t.Fatal("an unknown key reached the network")
	}
}

func TestOpenFetchesAndValidatesPictures(t *testing.T) {
	f := newFixture(t, nil)
	data := pngBytes(t, 16, 7)
	f.serve(serveBytes("text/plain", data)) // the bytes decide, not the header
	contentType, got, err := f.open(f.key(t, model.PictureAvatar, "/a.png"))
	if err != nil || contentType != "image/png" || !bytes.Equal(got, data) {
		t.Fatalf("Open = %q, %d bytes, %v", contentType, len(got), err)
	}
}

func TestOpenRefusesOversizePictures(t *testing.T) {
	f := newFixture(t, nil)
	big := append(pngBytes(t, 8, 1), make([]byte, avatarLimit)...)
	f.serve(serveBytes("image/png", big))
	if _, _, err := f.open(f.key(t, model.PictureAvatar, "/big.png")); err != ErrNotFound {
		t.Fatalf("an oversize avatar gave %v", err)
	}
	// The same bytes fit the larger banner limit.
	if _, _, err := f.open(f.key(t, model.PictureBanner, "/big.png")); err != nil {
		t.Fatalf("a banner of the same size gave %v", err)
	}
	f.serve(serveBytes("image/png", append(pngBytes(t, 8, 1), make([]byte, bannerLimit)...)))
	if _, _, err := f.open(f.key(t, model.PictureBackground, "/bigger.png")); err != ErrNotFound {
		t.Fatalf("an oversize background gave %v", err)
	}
	// A chunked body without a length is cut off at the limit as well.
	f.serve(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush()
		_, _ = w.Write(big)
	})
	if _, _, err := f.open(f.key(t, model.PictureAvatar, "/chunked.png")); err != ErrNotFound {
		t.Fatalf("a chunked oversize avatar gave %v", err)
	}
}

func TestOpenRefusesWrongTypes(t *testing.T) {
	f := newFixture(t, nil)
	huge := image.NewGray(image.Rect(0, 0, maxSide+1, 1))
	var tooWide bytes.Buffer
	if err := png.Encode(&tooWide, huge); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"html":    []byte("<html><script>alert(1)</script></html>"),
		"svg":     []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`),
		"empty":   nil,
		"corrupt": append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 40)...),
		"wide":    tooWide.Bytes(),
	} {
		f.serve(serveBytes("image/png", body))
		if _, _, err := f.open(f.key(t, model.PictureAvatar, "/"+name)); err != ErrNotFound {
			t.Fatalf("%s gave %v", name, err)
		}
	}
}

func TestOpenReducesGIFToStill(t *testing.T) {
	f := newFixture(t, nil)
	frame := image.NewPaletted(image.Rect(0, 0, 4, 4), color.Palette{color.Black, color.White})
	var animated bytes.Buffer
	if err := gif.EncodeAll(&animated, &gif.GIF{Image: []*image.Paletted{frame, frame}, Delay: []int{5, 5}}); err != nil {
		t.Fatal(err)
	}
	f.serve(serveBytes("image/gif", animated.Bytes()))
	contentType, data, err := f.open(f.key(t, model.PictureAvatar, "/a.gif"))
	if err != nil || contentType != "image/png" {
		t.Fatalf("Open = %q, %v", contentType, err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("the still is not a PNG: %v", err)
	}
}

func TestOpenRefusesRedirects(t *testing.T) {
	f := newFixture(t, nil)
	var target atomic.Int32
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		target.Add(1)
		serveBytes("image/png", pngBytes(t, 8, 1))(w, nil)
	}))
	defer elsewhere.Close()
	f.serve(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/a.png", http.StatusFound)
	})
	if _, _, err := f.open(f.key(t, model.PictureAvatar, "/a.png")); err != ErrNotFound {
		t.Fatalf("a redirect gave %v", err)
	}
	if target.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
}

func TestOpenRefusesPrivateDestinations(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.Client = nil }) // the default client
	f.serve(serveBytes("image/png", pngBytes(t, 8, 1)))
	// The refusal itself, not just a failed fetch: a certificate or connection error would also end in 404.
	for address, want := range map[string]error{
		"https://127.0.0.1/a.png":                  mediafetch.ErrPrivateDestination,
		"https://[::1]/a.png":                      mediafetch.ErrPrivateDestination,
		"https://10.0.0.5/a.png":                   mediafetch.ErrPrivateDestination,
		"https://169.254.169.254/latest/meta-data": mediafetch.ErrPrivateDestination,
		"https://localhost/a.png":                  mediafetch.ErrPrivateDestination,
		f.server.URL + "/a.png":                    mediafetch.ErrInvalidDestination, // a port other than 443
		"https://127.0.0.1:8443/a.png":             mediafetch.ErrInvalidDestination,
	} {
		if _, _, err := f.svc.fetch(context.Background(), model.PictureAvatar, address); !errors.Is(err, want) {
			t.Fatalf("fetch %s: %v, want %v", address, err, want)
		}
	}
	for _, address := range []string{f.server.URL + "/a.png", "https://127.0.0.1/a.png", "https://169.254.169.254/latest/meta-data", "https://10.0.0.5/a.png", "https://[::1]/a.png"} {
		src := f.svc.Src(model.PictureAvatar, address)
		if src == "" {
			continue // refused before any request
		}
		if _, _, err := f.open(strings.TrimPrefix(src, PathPrefix)); err != ErrNotFound {
			t.Fatalf("%s gave %v", address, err)
		}
	}
	if f.hits.Load() != 0 {
		t.Fatal("a private destination was contacted")
	}
}

func TestOpenCachesPicturesAndFailures(t *testing.T) {
	f := newFixture(t, nil)
	f.serve(serveBytes("image/png", pngBytes(t, 8, 1)))
	key := f.key(t, model.PictureAvatar, "/a.png")
	for range 3 {
		if _, _, err := f.open(key); err != nil {
			t.Fatal(err)
		}
	}
	if f.hits.Load() != 1 {
		t.Fatalf("%d fetches for three requests, want 1", f.hits.Load())
	}

	f.serve(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "gone", http.StatusInternalServerError) })
	missing := f.key(t, model.PictureAvatar, "/missing.png")
	before := f.hits.Load()
	for range 3 {
		if _, _, err := f.open(missing); err != ErrNotFound {
			t.Fatalf("a failing source gave %v", err)
		}
	}
	if f.hits.Load() != before+1 {
		t.Fatalf("%d fetches of a failing source, want 1", f.hits.Load()-before)
	}
	f.advance(time.Hour + time.Minute)
	_, _, _ = f.open(missing)
	if f.hits.Load() != before+2 {
		t.Fatal("a failure was not retried after its time")
	}
}

func TestOpenRefreshesAfterTTL(t *testing.T) {
	f := newFixture(t, nil)
	first, second := pngBytes(t, 8, 1), pngBytes(t, 8, 200)
	f.serve(serveBytes("image/png", first))
	key := f.key(t, model.PictureAvatar, "/a.png")
	if _, got, err := f.open(key); err != nil || !bytes.Equal(got, first) {
		t.Fatalf("first fetch: %v", err)
	}
	f.advance(23 * time.Hour)
	f.serve(serveBytes("image/png", second))
	if _, got, _ := f.open(key); !bytes.Equal(got, first) || f.hits.Load() != 1 {
		t.Fatal("a fresh copy was refetched")
	}
	f.advance(2 * time.Hour)
	if _, got, err := f.open(key); err != nil || !bytes.Equal(got, second) || f.hits.Load() != 2 {
		t.Fatalf("an expired copy was not refreshed: %v, %d fetches", err, f.hits.Load())
	}
	files, _ := os.ReadDir(f.dir)
	if len(files) != 1 {
		t.Fatalf("%d files kept, want only the current picture", len(files))
	}

	// When the refresh fails, the copy already held is still shown.
	f.advance(25 * time.Hour)
	f.serve(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusBadGateway) })
	if _, got, err := f.open(key); err != nil || !bytes.Equal(got, second) {
		t.Fatalf("a failed refresh lost the stale copy: %v", err)
	}
}

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.MaxEntries = 2 })
	f.serve(func(w http.ResponseWriter, r *http.Request) {
		shade := uint8(len(r.URL.Path))
		serveBytes("image/png", pngBytes(t, 8, shade))(w, r)
	})
	a, b, c := f.key(t, model.PictureAvatar, "/a.png"), f.key(t, model.PictureAvatar, "/bb.png"), f.key(t, model.PictureAvatar, "/ccc.png")
	for _, key := range []string{a, b} {
		f.advance(time.Minute)
		if _, _, err := f.open(key); err != nil {
			t.Fatal(err)
		}
	}
	f.advance(time.Minute)
	if _, _, err := f.open(a); err != nil { // a is now the most recently used
		t.Fatal(err)
	}
	f.advance(time.Minute)
	if _, _, err := f.open(c); err != nil {
		t.Fatal(err)
	}
	if files, _ := os.ReadDir(f.dir); len(files) != 2 {
		t.Fatalf("%d files kept, want 2", len(files))
	}
	hits := f.hits.Load()
	for _, key := range []string{a, c} {
		if _, _, err := f.open(key); err != nil {
			t.Fatal(err)
		}
	}
	if f.hits.Load() != hits {
		t.Fatal("a recently used picture was evicted")
	}
	if _, _, err := f.open(b); err != nil || f.hits.Load() != hits+1 {
		t.Fatalf("the least recently used picture was not evicted and fetched again: %v", err)
	}
}

func TestEvictsBySize(t *testing.T) {
	data := pngBytes(t, 64, 3)
	f := newFixture(t, func(o *Options) { o.MaxBytes = int64(len(data)) + 1 })
	f.serve(func(w http.ResponseWriter, r *http.Request) {
		serveBytes("image/png", pngBytes(t, 64, uint8(len(r.URL.Path))))(w, r)
	})
	a, b := f.key(t, model.PictureAvatar, "/a.png"), f.key(t, model.PictureAvatar, "/bb.png")
	for _, key := range []string{a, b} {
		f.advance(time.Minute)
		if _, _, err := f.open(key); err != nil {
			t.Fatal(err)
		}
	}
	if files, _ := os.ReadDir(f.dir); len(files) != 1 {
		t.Fatalf("%d files kept within the size limit, want 1", len(files))
	}
}

func TestOneFetchServesConcurrentRequests(t *testing.T) {
	f := newFixture(t, nil)
	release := make(chan struct{})
	f.serve(func(w http.ResponseWriter, r *http.Request) {
		<-release
		serveBytes("image/png", pngBytes(t, 8, 1))(w, r)
	})
	key := f.key(t, model.PictureAvatar, "/a.png")
	done := make(chan error, 5)
	for range 5 {
		go func() { _, _, err := f.open(key); done <- err }()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	for range 5 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if f.hits.Load() != 1 {
		t.Fatalf("%d fetches for concurrent requests, want 1", f.hits.Load())
	}
}

func TestRequestCarriesNoIdentity(t *testing.T) {
	f := newFixture(t, nil)
	f.serve(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" || r.Header.Get("Authorization") != "" || strings.Contains(r.UserAgent(), "ircord") {
			http.Error(w, "identifying request", http.StatusTeapot)
			return
		}
		serveBytes("image/png", pngBytes(t, 8, 1))(w, r)
	})
	if _, _, err := f.open(f.key(t, model.PictureAvatar, "/a.png")); err != nil {
		t.Fatal(err)
	}
}

func TestOpenDropsCopyDeletedAtSource(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		f := newFixture(t, nil)
		f.serve(serveBytes("image/png", pngBytes(t, 8, 1)))
		key := f.key(t, model.PictureAvatar, "/a.png")
		if _, _, err := f.open(key); err != nil {
			t.Fatal(err)
		}
		f.advance(25 * time.Hour)
		f.serve(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "deleted", status) })
		if _, _, err := f.open(key); err != ErrNotFound {
			t.Fatalf("status %d: a deleted picture was still served: %v", status, err)
		}
		if files, _ := os.ReadDir(f.dir); len(files) != 0 {
			t.Fatalf("status %d: %d files kept", status, len(files))
		}
		// Later requests do not bring it back, and the source is not asked again at once.
		hits := f.hits.Load()
		if _, _, err := f.open(key); err != ErrNotFound || f.hits.Load() != hits {
			t.Fatalf("status %d: %v after %d extra fetches", status, err, f.hits.Load()-hits)
		}
		// If the source has the picture again, it is fetched again after the failure time.
		f.advance(2 * time.Hour)
		f.serve(serveBytes("image/png", pngBytes(t, 8, 1)))
		if _, _, err := f.open(key); err != nil {
			t.Fatalf("status %d: a restored picture was not fetched: %v", status, err)
		}
	}
}
