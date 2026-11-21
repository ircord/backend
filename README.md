# IRCord backend

The Go backend connects IRCord clients to IRC networks and serves the HTTP and
WebSocket interfaces.

Use the latest stable Go release. Run `go test -race ./...` and `go vet ./...`
from this directory. Run `docker build .` to build the container.

For local development with the frontend, use the
[development environment](https://github.com/ircord/dev-environment). Run
`make backend` there to start the backend with live reload.

Generate `openapi.json` with `go run ./cmd/openapi > openapi.json` after changing
the HTTP or WebSocket contract. Refresh and check both clients with
`make api-generate` and `make api-check` in the development environment.

Push a version tag in this repository to build and publish
`ghcr.io/ircord/ircord-backend`.

The backend embeds `CHANGELOG.md` in the binary. Signed-in clients can read
the running version and its release notes from `GET /api/v1/changelog`.
