package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"ircord/backend/internal/appdata"
	"ircord/backend/internal/model"
)

func appRead[T any](t *testing.T, client *http.Client, method, path string, body any, status int) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(request(t, client, method, path, body, status), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestAppSettingsRequireAccount(t *testing.T) {
	_, server, _, _ := authenticatedApp(t)
	base := server.URL + "/api/v1/app/"
	for _, route := range []struct{ method, path string }{
		{"GET", "appearance"}, {"PUT", "appearance"}, {"GET", "preferences"}, {"PUT", "preferences"},
		{"GET", "library"}, {"PATCH", "library"}, {"POST", "library/gifs"}, {"DELETE", "library/gifs/g1"},
		{"GET", "themes"}, {"PATCH", "themes"}, {"PUT", "themes/t1"}, {"DELETE", "themes/t1"},
	} {
		request(t, server.Client(), route.method, base+route.path, nil, 401)
	}
	client := cookieClient()
	register(t, client, server.URL)
	req, _ := http.NewRequest(http.MethodPut, base+"preferences", strings.NewReader(`{"preferences":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://unrelated.invalid")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("foreign origin returned %d", resp.StatusCode)
	}
}

func TestAppSettingsUseTheExistingAccountDocuments(t *testing.T) {
	_, server, _, _ := authenticatedApp(t)
	alice, bob := cookieClient(), cookieClient()
	register(t, alice, server.URL)
	register(t, bob, server.URL)
	api := server.URL + "/api/v1/"
	base := api + "app/"
	if got := appRead[appdata.StoredAppearance](t, alice, "GET", base+"appearance", nil, 200); got.Settings != nil || got.UpdatedAt != nil {
		t.Fatal("new account has appearance settings")
	}
	legacy := appRead[AppDocument](t, alice, "GET", api+"app-data/appearance", nil, 200)
	legacy.Data = json.RawMessage(`{"settings":{"interfaceSize":17,"coloredNames":false,"accentCustom":"#AABBCC"}}`)
	request(t, alice, "PUT", api+"app-data/appearance", legacy, 200)
	got := appRead[appdata.StoredAppearance](t, alice, "GET", base+"appearance", nil, 200).Settings
	if got == nil || *got.InterfaceScale != 120 || *got.ColoredNames || *got.AccentCustom != "#aabbcc" || got.Mode != "system" {
		t.Fatalf("legacy appearance was not normalized: %#v", got)
	}
	request(t, alice, "PUT", base+"appearance", json.RawMessage(`{"settings":{"messageSize":99}}`), 422)
	request(t, alice, "PUT", base+"appearance", json.RawMessage(`{"settings":{"mode":"dark","animateMedia":false}}`), 200)
	reloaded := appRead[AppDocument](t, alice, "GET", api+"app-data/appearance", nil, 200)
	if saved := appdata.ReadAppearance(reloaded.Data).Settings; saved.Mode != "dark" || *saved.AnimateMedia {
		t.Fatal("the old API does not see the new save")
	}
	preferences := appRead[appdata.StoredPreferences](t, alice, "PUT", base+"preferences", json.RawMessage(
		`{"preferences":{"showNsfw":true,"speech":{"baseUrl":"https://name:credential@private.invalid/v1","model":"voice"}}}`,
	), 200)
	if preferences.Preferences == nil || !preferences.Preferences.ShowNSFW || !*preferences.Preferences.RegisterChannels ||
		preferences.Preferences.Speech.BaseURL != "" {
		t.Fatalf("incorrect preference defaults or unsafe speech URL: %#v", preferences)
	}
	request(t, alice, "PUT", base+"preferences", json.RawMessage(`{"preferences":{"speech":{"apiKey":"secret"}}}`), 422)
	if got := appRead[appdata.StoredPreferences](t, bob, "GET", base+"preferences", nil, 200); got.Preferences != nil {
		t.Fatal("preferences were visible to another account")
	}
	if got := appRead[appdata.StoredAppearance](t, bob, "GET", base+"appearance", nil, 200); got.Settings != nil {
		t.Fatal("appearance was visible to another account")
	}
}

func TestAppPickerLibraryAtomicUpdates(t *testing.T) {
	_, server, _, _ := authenticatedApp(t)
	alice, bob := cookieClient(), cookieClient()
	register(t, alice, server.URL)
	register(t, bob, server.URL)
	base := server.URL + "/api/v1/app/library"
	gif := model.GIF{
		ID: "g1", Slug: "wave", Title: "Wave", URL: "https://static.klipy.com/a.gif",
		PreviewURL: "https://static.klipy.com/a.webp", StillURL: "https://static.klipy.com/a.png",
		Width: 24, Height: 24, Provider: "klipy",
	}
	request(t, alice, "POST", base+"/gifs", gif, 200)
	invalid := gif
	invalid.PreviewURL = "https://unrelated.invalid/a.gif"
	request(t, alice, "POST", base+"/gifs", invalid, 400)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			req, err := http.NewRequest(http.MethodPatch, base, strings.NewReader(`{"uses":{"first":2,"second":2},"recent":["first","second"],"skinTone":"3"}`))
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			res, err := alice.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Errorf("emoji update returned %d", res.StatusCode)
			}
		})
	}
	workers.Wait()
	got := appRead[appdata.Library](t, alice, "GET", base, nil, 200)
	if got.SkinTone != "3" || len(got.GIFs) != 1 || len(got.EmojiUsage) != 2 ||
		got.EmojiUsage[0].ID != "second" || got.EmojiUsage[0].Count != 4 || got.EmojiUsage[1].Count != 4 {
		t.Fatalf("library update lost data: %+v", got)
	}
	other := appRead[appdata.Library](t, bob, "DELETE", base+"/gifs/g1", nil, 200)
	if len(other.GIFs) != 0 || len(other.EmojiUsage) != 0 {
		t.Fatal("library was visible to another account")
	}
	got = appRead[appdata.Library](t, alice, "DELETE", base+"/gifs/g1", nil, 200)
	if len(got.GIFs) != 0 || len(got.EmojiUsage) != 2 {
		t.Fatalf("removing a GIF changed other data: %+v", got)
	}
}

func TestAppThemesConflictAndLimits(t *testing.T) {
	_, server, _, _ := authenticatedApp(t)
	alice, bob := cookieClient(), cookieClient()
	register(t, alice, server.URL)
	register(t, bob, server.URL)
	base := server.URL + "/api/v1/app/themes"
	newTheme := func(name string, expected *string, force bool) map[string]any {
		return map[string]any{"theme": map[string]any{"name": name, "background": nil}, "expected": expected, "force": force}
	}
	first := appRead[appdata.Themes](t, alice, "PUT", base+"/t1", newTheme("Night", nil, false), 200)
	version := first.Themes[0].UpdatedAt
	next := appRead[appdata.Themes](t, alice, "PUT", base+"/t1", newTheme("Day", &version, false), 200)
	if next.Themes[0].UpdatedAt == version {
		t.Fatal("a successful edit reused the old version")
	}
	conflict := appRead[ThemeConflict](t, alice, "PUT", base+"/t1", newTheme("Stale", &version, false), 412)
	if conflict.Data.Theirs.Name != "Day" || conflict.Data.Theirs.UpdatedAt != next.Themes[0].UpdatedAt {
		t.Fatal("theme conflict omitted the current theme")
	}
	request(t, alice, "PUT", base+"/t1", newTheme("Force", &version, true), 200)
	request(t, alice, "PUT", base+"/t1", map[string]any{"theme": map[string]any{"css": strings.Repeat("ä", (32<<10)+1)}, "force": true}, 400)
	request(t, alice, "PATCH", base, json.RawMessage(`{"customCss":false}`), 200)
	current := appRead[appdata.Themes](t, alice, "GET", base, nil, 200)
	if current.CustomCSS || len(current.Themes) != 1 || current.Themes[0].Name != "Force" {
		t.Fatalf("theme settings were lost: %+v", current)
	}
	other := appRead[appdata.Themes](t, bob, "DELETE", base+"/t1", nil, 200)
	if !other.CustomCSS || len(other.Themes) != 0 {
		t.Fatal("theme data was visible to another account")
	}
	got := appRead[appdata.Themes](t, alice, "DELETE", base+"/t1", nil, 200)
	if got.CustomCSS || len(got.Themes) != 0 {
		t.Fatal("delete did not preserve the CSS preference")
	}
}
