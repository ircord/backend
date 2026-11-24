// Package remotemedia caches profile pictures hosted on other IRCord servers so
// that clients draw them from this server and never contact the other host.
//
// A picture's address on this server is /media/remote/{key}. The key is an HMAC of
// the picture's kind and address under a key derived from the instance key, so only
// addresses this server has chosen to show can be fetched: it is not an open proxy.
package remotemedia

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // registered for image.DecodeConfig
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/webp"
	"golang.org/x/sync/singleflight"

	"ircord/backend/internal/emotes"
	"ircord/backend/internal/mediafetch"
	"ircord/backend/internal/model"
)

// PathPrefix is where cached pictures are served, relative to this server's origin.
const PathPrefix = "/media/remote/"

const (
	avatarLimit = 256 << 10
	bannerLimit = 2 << 20
	maxSide     = 2048
	keyLength   = 32
)

var (
	ErrNotFound = errors.New("remote picture unavailable")
	errType     = errors.New("remote picture is not a PNG, JPEG, WebP or GIF image")
	errSize     = errors.New("remote picture is too large")
	errRedirect = errors.New("remote picture redirects")
)

type Options struct {
	DB *sql.DB
	// Dir holds the cached picture files, named by content.
	Dir string
	// Key is the HMAC key that makes addresses unguessable.
	Key []byte
	// PublicURL is this server's own address; pictures on it are not cached.
	PublicURL string
	// Client fetches remote pictures. The default refuses private destinations.
	Client *http.Client
	Now    func() time.Time

	// Zero values select the defaults: 24 h, 1 h, 64 MiB, 5000 pictures, 4 fetches.
	TTL, FailureTTL time.Duration
	MaxBytes        int64
	MaxEntries      int
	MaxFetches      int
	FetchTimeout    time.Duration
}

type Service struct {
	options    Options
	origin     string
	client     http.Client
	slots      chan struct{}
	flight     singleflight.Group
	mu         sync.Mutex // serializes storing and evicting
	registered sync.Map
}

func New(o Options) (*Service, error) {
	if o.DB == nil || o.Dir == "" || len(o.Key) == 0 {
		return nil, errors.New("remote media needs a database, directory and key")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.TTL <= 0 {
		o.TTL = 24 * time.Hour
	}
	if o.FailureTTL <= 0 {
		o.FailureTTL = time.Hour
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = 64 << 20
	}
	if o.MaxEntries <= 0 {
		o.MaxEntries = 5000
	}
	if o.MaxFetches <= 0 {
		o.MaxFetches = 4
	}
	if o.FetchTimeout <= 0 {
		o.FetchTimeout = 10 * time.Second
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, err
	}
	s := &Service{options: o, slots: make(chan struct{}, o.MaxFetches)}
	if u, err := url.Parse(o.PublicURL); err == nil {
		s.origin = originOf(u)
	}
	if o.Client != nil {
		s.client = *o.Client
	} else {
		s.client = http.Client{Transport: &http.Transport{
			Proxy: nil, DialContext: mediafetch.DialContext, MaxIdleConns: 20, MaxIdleConnsPerHost: 2,
			TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 32768,
		}}
	}
	// A redirect could lead anywhere, so none is followed whatever client is used.
	s.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return s, nil
}

func originOf(u *url.URL) string {
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port == "" || (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		return strings.ToLower(u.Scheme) + "://" + host
	}
	return strings.ToLower(u.Scheme) + "://" + host + ":" + port
}

func limit(kind string) int64 {
	if kind == model.PictureAvatar {
		return avatarLimit
	}
	return bannerLimit
}

func validKind(kind string) bool {
	return kind == model.PictureAvatar || kind == model.PictureBanner || kind == model.PictureBackground
}

func (s *Service) key(kind, address string) string {
	mac := hmac.New(sha256.New, s.options.Key)
	mac.Write([]byte(kind + "\x00" + address))
	return hex.EncodeToString(mac.Sum(nil))[:keyLength]
}

// Src returns the address clients draw for a profile picture: the same address
// when it is on this server, a cached-copy address for another HTTPS host, and
// empty for anything else.
func (s *Service) Src(kind, address string) string {
	if address == "" || !validKind(kind) {
		return ""
	}
	if strings.HasPrefix(address, "/") && !strings.HasPrefix(address, "//") {
		return address
	}
	u, err := url.Parse(address)
	if err != nil || u.Hostname() == "" || u.User != nil {
		return ""
	}
	if s.origin != "" && originOf(u) == s.origin {
		return address
	}
	if u.Scheme != "https" {
		return ""
	}
	key := s.key(kind, address)
	if _, done := s.registered.Load(key); !done {
		_, err = s.options.DB.Exec("INSERT OR IGNORE INTO remote_media(key,url,kind) VALUES(?,?,?)", key, address, kind)
		if err != nil {
			return ""
		}
		s.registered.Store(key, struct{}{})
	}
	return PathPrefix + key
}

type entry struct {
	key, url, kind, sha, contentType   string
	fetched, expires, failed, accessed int64
}

func (s *Service) load(key string) (entry, error) {
	var e entry
	err := s.options.DB.QueryRow("SELECT key,url,kind,sha,content_type,fetched_ns,expires_ns,failed_ns,accessed_ns FROM remote_media WHERE key=?", key).
		Scan(&e.key, &e.url, &e.kind, &e.sha, &e.contentType, &e.fetched, &e.expires, &e.failed, &e.accessed)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	return e, err
}

func (s *Service) file(sha string) string { return filepath.Join(s.options.Dir, sha) }

// Open returns the cached picture for key, fetching or refreshing it when needed.
func (s *Service) Open(ctx context.Context, key string) (string, io.ReadCloser, error) {
	if len(key) != keyLength || strings.Trim(key, "0123456789abcdef") != "" { // gitleaks:allow -- hexadecimal validation alphabet
		return "", nil, ErrNotFound
	}
	e, err := s.load(key)
	if err != nil {
		return "", nil, err
	}
	now := s.options.Now().UnixNano()
	switch {
	case e.sha != "" && now < e.expires:
	case e.failed != 0 && now-e.failed < int64(s.options.FailureTTL):
		// The source failed recently: serve what is kept, otherwise report it unavailable.
	default:
		_, err, _ = s.flight.Do(key, func() (any, error) { return nil, s.refresh(e) })
		if e, err = s.load(key); err != nil {
			return "", nil, err
		}
	}
	if e.sha == "" {
		return "", nil, ErrNotFound
	}
	f, err := os.Open(s.file(e.sha))
	if err != nil {
		return "", nil, ErrNotFound
	}
	if now-e.accessed > int64(time.Minute) {
		_, _ = s.options.DB.Exec("UPDATE remote_media SET accessed_ns=? WHERE key=?", now, key)
	}
	return e.contentType, f, nil
}

// refresh fetches the picture and records the outcome. A failure keeps any older copy.
func (s *Service) refresh(e entry) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.options.FetchTimeout)
	defer cancel()
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	data, contentType, err := s.fetch(ctx, e.kind, e.url)
	now := s.options.Now().UnixNano()
	if err != nil {
		_, _ = s.options.DB.Exec("UPDATE remote_media SET failed_ns=? WHERE key=?", now, e.key)
		return err
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.write(sha, data); err != nil {
		_, _ = s.options.DB.Exec("UPDATE remote_media SET failed_ns=? WHERE key=?", now, e.key)
		return err
	}
	_, err = s.options.DB.Exec("UPDATE remote_media SET sha=?,content_type=?,size=?,fetched_ns=?,expires_ns=?,failed_ns=0,accessed_ns=? WHERE key=?",
		sha, contentType, len(data), now, now+int64(s.options.TTL), now, e.key)
	if err != nil {
		return err
	}
	if e.sha != "" && e.sha != sha {
		s.release(e.sha)
	}
	s.evict()
	return nil
}

func (s *Service) write(sha string, data []byte) error {
	path := s.file(sha)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	tmp, err := os.CreateTemp(s.options.Dir, ".fetch-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// release removes a picture file that no entry uses any more.
func (s *Service) release(sha string) {
	var n int
	if s.options.DB.QueryRow("SELECT COUNT(*) FROM remote_media WHERE sha=?", sha).Scan(&n) == nil && n == 0 {
		_ = os.Remove(s.file(sha))
	}
}

// evict drops the least recently used pictures until the cache is within its limits.
// The entry itself stays, so its address keeps working and the picture is fetched again.
func (s *Service) evict() {
	for {
		var count int
		var size int64
		if s.options.DB.QueryRow("SELECT COUNT(*),COALESCE(SUM(size),0) FROM remote_media WHERE sha!=''").Scan(&count, &size) != nil {
			return
		}
		if count <= s.options.MaxEntries && size <= s.options.MaxBytes || count <= 1 {
			return
		}
		var key, sha string
		if s.options.DB.QueryRow("SELECT key,sha FROM remote_media WHERE sha!='' ORDER BY accessed_ns,fetched_ns,key LIMIT 1").Scan(&key, &sha) != nil {
			return
		}
		if _, err := s.options.DB.Exec("UPDATE remote_media SET sha='',content_type='',size=0,fetched_ns=0,expires_ns=0,failed_ns=0 WHERE key=?", key); err != nil {
			return
		}
		s.release(sha)
	}
}

func (s *Service) fetch(ctx context.Context, kind, address string) ([]byte, string, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return nil, "", errors.New("remote picture address is not HTTPS")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; picture-cache)")
	req.Header.Set("Accept", "image/png,image/jpeg,image/webp,image/gif")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, "", errRedirect
	case resp.StatusCode != http.StatusOK:
		return nil, "", fmt.Errorf("remote picture status %d", resp.StatusCode)
	}
	max := limit(kind)
	if resp.ContentLength > max {
		return nil, "", errSize
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > max {
		return nil, "", errSize
	}
	return validate(data)
}

// validate trusts the bytes, not the response headers, and reduces a GIF to a still.
func validate(data []byte) ([]byte, string, error) {
	contentType, _, _ := strings.Cut(http.DetectContentType(data), ";")
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return nil, "", errType
	}
	var cfg image.Config
	var err error
	if contentType == "image/webp" {
		cfg, err = webp.DecodeConfig(bytes.NewReader(data))
	} else {
		cfg, _, err = image.DecodeConfig(bytes.NewReader(data))
	}
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, "", errors.New("remote picture could not be read")
	}
	if cfg.Width > maxSide || cfg.Height > maxSide {
		return nil, "", errors.New("remote picture is too large in pixels")
	}
	if contentType == "image/gif" {
		still, err := emotes.GIFStill(data, cfg.Width, cfg.Height)
		if err != nil {
			return nil, "", errors.New("remote picture could not be read")
		}
		return still, "image/png", nil
	}
	return data, contentType, nil
}
