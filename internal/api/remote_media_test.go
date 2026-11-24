package api

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ircord/backend/internal/irc"
	"ircord/backend/internal/model"
	"ircord/backend/internal/remotemedia"
	"ircord/backend/internal/storage"
)

func TestRemotePicturesAreServedFromThisServer(t *testing.T) {
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data.Bytes())
	}))
	defer other.Close()

	var cache *remotemedia.Service
	app, server, _, _ := authenticatedApp(t, func(o *irc.Options) {
		var err error
		cache, err = remotemedia.New(remotemedia.Options{DB: o.Store.DB, Dir: filepath.Join(t.TempDir(), "remote-media"), Key: o.Store.DeriveKey("remote-media/v1"), PublicURL: "https://this.example", Client: other.Client()})
		if err != nil {
			t.Fatal(err)
		}
		o.Pictures = cache.Src
	})
	app.options.RemoteMedia = cache
	client := cookieClient()
	account := register(t, client, server.URL)

	remote := other.URL + "/files/1/token/avatar"
	var got model.Member
	body := request(t, client, "PATCH", server.URL+"/api/v1/profile", map[string]any{"avatarUrl": remote, "bannerUrl": remote + "?banner", "profileBackgroundUrl": "https://this.example/files/2/token/background"}, 200)
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.AvatarURL != remote {
		t.Fatalf("the raw address changed: %q", got.AvatarURL)
	}
	if !strings.HasPrefix(got.AvatarSrc, "/media/remote/") || !strings.HasPrefix(got.BannerSrc, "/media/remote/") || got.AvatarSrc == got.BannerSrc {
		t.Fatalf("remote pictures were not given cached addresses: %+v", got)
	}
	if got.ProfileBackgroundSrc != got.ProfileBackgroundURL {
		t.Fatalf("a picture on this server changed: %q", got.ProfileBackgroundSrc)
	}
	snapshot := app.Manager.Snapshot(account.Account.ID)
	if snapshot.User.AvatarSrc != got.AvatarSrc || snapshot.User.BannerSrc != got.BannerSrc {
		t.Fatalf("the snapshot disagrees: %+v", snapshot.User)
	}

	// Anyone holding the address can load it, with no session, from this server alone.
	res, err := http.Get(server.URL + got.AvatarSrc)
	if err != nil {
		t.Fatal(err)
	}
	served, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !bytes.Equal(served, data.Bytes()) || res.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("picture: %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("Content-Security-Policy") != "sandbox; default-src 'none'" || res.Header.Get("Cache-Control") != "private, max-age=3600" {
		t.Fatalf("headers: %v", res.Header)
	}
	head, err := http.Head(server.URL + got.AvatarSrc)
	if err != nil || head.StatusCode != 200 {
		t.Fatalf("HEAD: %v %v", head, err)
	}
	head.Body.Close()
	for _, key := range []string{strings.Repeat("0", 32), "nope", strings.Repeat("a", 31)} {
		res, err := http.Get(server.URL + "/media/remote/" + key)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 404 {
			t.Fatalf("key %q gave %d", key, res.StatusCode)
		}
	}
}

func TestPictureSourcesInPeopleAndPartyProjections(t *testing.T) {
	source := func(kind, address string) string { return "/media/remote/" + kind }
	app := &Server{options: Options{}}
	if app.pictures() != nil {
		t.Fatal("pictures should be unset without a cache")
	}
	person := model.Person{AvatarURL: "https://other.example/a"}.WithPictures(source)
	candidate := model.PartyCandidate{AvatarURL: "https://other.example/a"}.WithPictures(source)
	if person.AvatarSrc != "/media/remote/avatar" || candidate.AvatarSrc != "/media/remote/avatar" {
		t.Fatalf("%+v %+v", person, candidate)
	}
	if empty := (model.Person{}).WithPictures(source); empty.AvatarSrc != "" {
		t.Fatal("a person without a picture got an address")
	}
	if none := (model.Person{AvatarURL: "https://other.example/a"}).WithPictures(nil); none.AvatarSrc != "" {
		t.Fatal("an address was set without a source")
	}
	party, _ := app.partyView(model.Party{Members: []model.PartyMember{{PartyCandidate: model.PartyCandidate{AvatarURL: "https://other.example/a"}}}})
	if party.Members[0].AvatarSrc != "" {
		t.Fatal("no cache must not set addresses")
	}
	store, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	app.options.RemoteMedia, err = remotemedia.New(remotemedia.Options{DB: store.DB, Dir: t.TempDir(), Key: store.DeriveKey("remote-media/v1"), PublicURL: "https://this.example"})
	if err != nil {
		t.Fatal(err)
	}
	members := []model.PartyMember{{PartyCandidate: model.PartyCandidate{AvatarURL: "https://other.example/a"}}}
	view, _ := app.partyView(model.Party{Members: members})
	if !strings.HasPrefix(view.Members[0].AvatarSrc, "/media/remote/") || members[0].AvatarSrc != "" {
		t.Fatalf("party members: %+v, stored: %+v", view.Members[0], members[0])
	}
}
