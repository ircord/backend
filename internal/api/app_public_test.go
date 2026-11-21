package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

func TestAppPublicRoutesAndOpenAPI(t *testing.T) {
	app, server, _, _ := authenticatedApp(t)
	fetches := 0
	app.appEmoji.mu.Lock()
	app.appEmoji.fetch = func(context.Context) ([]AppEmoji, bool) {
		fetches++
		return nil, false
	}
	app.appEmoji.mu.Unlock()
	for range 2 {
		catalog := appRead[AppEmojiCatalog](t, server.Client(), "GET", server.URL+appEmojiPath, nil, 200)
		if len(catalog.Items) < 2 || catalog.Items[0].ID != "workspace:ircord" || catalog.Items[1].Source != "7tv" {
			t.Fatal("public catalog must retain the workspace and offline 7TV entries")
		}
	}
	app.appEmoji.mu.Lock()
	if fetches != 1 {
		t.Errorf("catalog cache fetched %d times", fetches)
	}
	app.appEmoji.mu.Unlock()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, method := range []string{"GET", "HEAD"} {
		req, _ := http.NewRequest(method, server.URL+appSpeechRoot+"v1/models/backbone/shard-010.bin", nil)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 302 || !strings.HasSuffix(res.Header.Get("Location"), "/web/backbone/shard-010.bin") ||
			res.Header.Get("Referrer-Policy") != "no-referrer" || res.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("incorrect speech redirect for %s: %d %v", method, res.StatusCode, res.Header)
		}
	}
	request(t, client, "GET", server.URL+appSpeechRoot+"v1/models/zac/shard-005.bin", nil, 404)
	request(t, client, "GET", server.URL+appSpeechRoot+"v2/models/backbone/shard-001.bin", nil, 404)

	spec := app.API.OpenAPI()
	if len(spec.Paths[appEmojiPath].Get.Security) != 0 ||
		len(spec.Paths[appSpeechRoot+"{version}/models/{model}/{file}"].Head.Security) != 0 {
		t.Fatal("OpenAPI marks a public read private")
	}
	for path, methods := range spec.Paths {
		if !strings.HasPrefix(path, "/api/v1/app/") {
			continue
		}
		for _, op := range []*huma.Operation{methods.Get, methods.Head, methods.Put, methods.Patch, methods.Post, methods.Delete} {
			if op == nil || publicAppRead(op.Method, path) {
				continue
			}
			if len(op.Security) == 0 || op.Security[0]["sessionCookie"] == nil || op.Summary == "" || op.Description == "" {
				t.Fatalf("private operation is missing documentation: %s %s", op.Method, path)
			}
		}
	}
	conflict := spec.Paths["/api/v1/app/themes/{id}"].Put.Responses["412"].Content["application/problem+json"].Schema
	if !strings.HasSuffix(conflict.Ref, "/ThemeConflict") {
		t.Fatalf("OpenAPI did not document the conflict response: %+v", conflict)
	}
}

func TestSevenTVCatalogValidatesMedia(t *testing.T) {
	data := `{"emotes":[{"id":"abcd","name":"Wave","data":{"host":{"url":"//cdn.7tv.app/emote/abcd","files":[{"name":"1x.webp"},{"name":"2x.webp","static_name":"2x_static.webp"}]}}}]}`
	items, ok := decodeSevenTV([]byte(data))
	if !ok || len(items) != 1 || items[0].URL != "https://cdn.7tv.app/emote/abcd/2x.webp" ||
		items[0].StaticURL != "https://cdn.7tv.app/emote/abcd/2x_static.webp" {
		t.Fatalf("catalog did not preserve the animated and still images: %+v", items)
	}
	for _, pair := range [][2]string{{"cdn.7tv.app", "unrelated.invalid"}, {"2x_static.webp", "../other.webp"}} {
		if _, ok := decodeSevenTV([]byte(strings.Replace(data, pair[0], pair[1], 1))); ok {
			t.Fatal("catalog accepted untrusted media")
		}
	}
}
