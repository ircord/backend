package api

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
	"ircord/backend"
)

type BackendChangelog struct {
	Version  string            `json:"version" doc:"Version of the backend serving this request."`
	Releases []backend.Release `json:"releases" doc:"Release notes embedded in this backend build, newest first."`
}

func (s *Server) registerChangelog() {
	version := s.options.Build.Version
	changelog := BackendChangelog{Version: version, Releases: backend.Releases(version)}
	huma.Get(s.API, "/api/v1/changelog", func(_ context.Context, _ *struct{}) (*Output[BackendChangelog], error) {
		return result(changelog, nil)
	})
}
