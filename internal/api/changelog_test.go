package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"ircord/backend/internal/buildinfo"
)

func TestBackendChangelogReportsRunningVersion(t *testing.T) {
	base, _, _, _ := authenticatedApp(t)
	options := base.options
	options.Build = buildinfo.Info{Version: "v0.7.3"}
	server := httptest.NewServer(New(base.Manager, options).Handler)
	defer server.Close()

	client := cookieClient()
	url := server.URL + "/api/v1/changelog"
	request(t, client, "GET", url, nil, 401)
	register(t, client, server.URL)
	var got BackendChangelog
	if err := json.Unmarshal(request(t, client, "GET", url, nil, 200), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != options.Build.Version || len(got.Releases) == 0 ||
		got.Releases[0].Version != got.Version || got.Releases[0].Notes == "" {
		t.Fatalf("release history does not describe the running backend: %#v", got)
	}
}
