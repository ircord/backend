package api

import (
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"ircord/backend/internal/model"
)

// registerRemoteMedia serves copies of profile pictures hosted on other servers.
// The key in the address is the capability, as with /files/, because image
// loaders send no session.
func (s *Server) registerRemoteMedia(router chi.Router) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		s.API.OpenAPI().AddOperation(&huma.Operation{
			OperationID: map[string]string{http.MethodGet: "get", http.MethodHead: "head"}[method] + "-remote-picture", Method: method, Path: "/media/remote/{key}",
			Summary:     "Show a profile picture from another server",
			Description: "Member, person and party projections give this address as avatarSrc, bannerSrc and profileBackgroundSrc when the picture is hosted on another server. This server fetches the picture once, checks it, keeps a copy for a day, and serves it from its own address, so viewers never contact the other server. Only addresses this server issued can be fetched. GIFs are served as a still frame.",
			Parameters:  []*huma.Param{{Name: "key", In: "path", Required: true, Schema: &huma.Schema{Type: "string"}}},
			Responses: map[string]*huma.Response{
				"200": {Description: "A PNG, JPEG, WebP or still image", Content: map[string]*huma.MediaType{"image/png": {Schema: &huma.Schema{Type: "string", Format: "binary"}}}},
				"404": {Description: "The key is unknown or the picture is unavailable; show the person's initial instead"},
			},
		})
	}
	router.Get("/media/remote/{key}", s.remotePicture)
	router.Head("/media/remote/{key}", s.remotePicture)
}

// pictures chooses the address clients draw for a profile picture. Without a
// cache, only pictures on this server's own address are shown.
func (s *Server) pictures() model.PictureSource {
	if s.options.RemoteMedia == nil {
		return nil
	}
	return s.options.RemoteMedia.Src
}

func (s *Server) remotePicture(w http.ResponseWriter, r *http.Request) {
	if s.options.RemoteMedia == nil {
		http.NotFound(w, r)
		return
	}
	contentType, body, err := s.options.RemoteMedia.Open(r.Context(), chi.URLParam(r, "key"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, body)
}

// partyView sets the display addresses on a copy of the party's members.
func (s *Server) partyView(p model.Party) (model.Party, error) {
	p.Members = append([]model.PartyMember(nil), p.Members...)
	for i := range p.Members {
		p.Members[i].PartyCandidate = p.Members[i].PartyCandidate.WithPictures(s.pictures())
	}
	return p, nil
}

func (s *Server) partyResult(p model.Party, err error) (*Output[model.Party], error) {
	p, _ = s.partyView(p)
	return result(p, err)
}

func (s *Server) memberResult(m model.Member, err error) (*Output[model.Member], error) {
	return result(m.WithPictures(s.pictures()), err)
}
