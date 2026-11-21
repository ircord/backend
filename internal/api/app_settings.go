package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"ircord/backend/internal/appdata"
	"ircord/backend/internal/irc"
	"ircord/backend/internal/model"
	"ircord/backend/internal/storage"
)

const (
	appearanceDocument  = "appearance.updated"
	preferencesDocument = "client-preferences.updated"
	libraryDocument     = "library.updated"
	themesDocument      = "themes.updated"
)

func appOperation(method, path, id, summary, description string, maxBytes int64) huma.Operation {
	statuses := []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusServiceUnavailable}
	if method != http.MethodGet {
		statuses = append(statuses, http.StatusBadRequest, http.StatusConflict, http.StatusRequestEntityTooLarge)
	}
	return huma.Operation{
		Method: method, Path: "/api/v1/app/" + path, OperationID: id,
		Summary: summary, Description: description, MaxBodyBytes: maxBytes, Errors: statuses,
	}
}

func readAppData[T any](s *Server, ctx context.Context, kind string, read func(json.RawMessage) T) (*Output[T], error) {
	raw, err := s.Manager.AppData(owner(ctx), kind)
	return result(read(raw), err)
}

// Apply the user's operation again when a different device changes the
// document. Only a successful compare-and-swap sends a revision event.
func updateAppData[T any](s *Server, ctx context.Context, kind string, apply func(json.RawMessage) (T, error)) (*Output[T], error) {
	account := owner(ctx)
	for range 5 {
		if err := ctx.Err(); err != nil {
			return nil, commandFailure(err)
		}
		raw, err := s.Manager.AppData(account, kind)
		if err != nil {
			return nil, commandFailure(err)
		}
		value, err := apply(raw)
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(value)
		if err != nil {
			return nil, huma.Error500InternalServerError("Could not encode the saved data.")
		}
		if len(data) > storage.AppDataLimit(kind) {
			return nil, huma.Error413RequestEntityTooLarge("The saved data exceeds the account limit.")
		}
		if _, err := s.Manager.SaveAppDataCAS(account, kind, irc.AppRevision(raw), data); !errors.Is(err, irc.ErrConflict) {
			return result(value, err)
		}
	}
	return nil, commandFailure(irc.ErrConflict)
}

func (s *Server) registerAppSettings() {
	huma.Register(s.API, appOperation("GET", "appearance", "get-app-appearance", "Read account appearance",
		"Returns the signed-in account's appearance. Settings and update time are null before the first save.", 0),
		func(ctx context.Context, _ *struct{}) (*Output[appdata.StoredAppearance], error) {
			return readAppData(s, ctx, appearanceDocument, appdata.ReadAppearance)
		})
	huma.Register(s.API, appOperation("PUT", "appearance", "save-app-appearance", "Save account appearance",
		"Replaces the signed-in account's appearance. Omitted settings use their defaults.", 16<<10),
		func(ctx context.Context, in *struct {
			Body struct {
				Settings appdata.Appearance `json:"settings"`
			}
		}) (*Output[appdata.StoredAppearance], error) {
			settings, stamp := in.Body.Settings.Clean(), appdata.Timestamp(time.Now())
			return updateAppData(s, ctx, appearanceDocument, func(_ json.RawMessage) (appdata.StoredAppearance, error) {
				return appdata.StoredAppearance{Settings: &settings, UpdatedAt: &stamp}, nil
			})
		})
	huma.Register(s.API, appOperation("GET", "preferences", "get-app-preferences", "Read account preferences",
		"Returns the signed-in account's preferences. Preferences and update time are null before the first save.", 0),
		func(ctx context.Context, _ *struct{}) (*Output[appdata.StoredPreferences], error) {
			return readAppData(s, ctx, preferencesDocument, appdata.ReadPreferences)
		})
	huma.Register(s.API, appOperation("PUT", "preferences", "save-app-preferences", "Save account preferences",
		"Replaces the signed-in account's preferences. Omitted preferences use their defaults. Device secrets do not belong in this document.", 4<<20),
		func(ctx context.Context, in *struct {
			Body struct {
				Preferences appdata.ClientPreferences `json:"preferences"`
			}
		}) (*Output[appdata.StoredPreferences], error) {
			preferences, stamp := in.Body.Preferences.Clean(), appdata.Timestamp(time.Now())
			return updateAppData(s, ctx, preferencesDocument, func(_ json.RawMessage) (appdata.StoredPreferences, error) {
				return appdata.StoredPreferences{Preferences: &preferences, UpdatedAt: &stamp}, nil
			})
		})
	s.registerPickerLibrary()
	s.registerThemeLibrary()
}

func (s *Server) registerPickerLibrary() {
	huma.Register(s.API, appOperation("GET", "library", "get-app-library", "Read the picker library",
		"Returns the signed-in account's saved GIFs, emoji usage and skin tone.", 0),
		func(ctx context.Context, _ *struct{}) (*Output[appdata.Library], error) {
			return readAppData(s, ctx, libraryDocument, func(raw json.RawMessage) appdata.Library {
				return appdata.ReadLibrary(raw, time.Now())
			})
		})
	huma.Register(s.API, appOperation("PATCH", "library", "update-app-library", "Update emoji usage and skin tone",
		"Adds emoji picks to the signed-in account's counts and sets its default skin tone when supplied.", 64<<10),
		func(ctx context.Context, in *struct{ Body appdata.LibraryPatch }) (*Output[appdata.Library], error) {
			return editPicker(s, ctx, func(library *appdata.Library, now time.Time) { library.Apply(in.Body, now) })
		})
	huma.Register(s.API, appOperation("POST", "library/gifs", "save-app-gif", "Save a GIF to the picker library",
		"Saves a Klipy GIF for the signed-in account. Saving the same ID moves it to the front. The 200 most recent favorites are retained.", 16<<10),
		func(ctx context.Context, in *struct{ Body SavePickerGIF }) (*Output[appdata.Library], error) {
			gif := model.GIF(in.Body)
			if !appdata.ValidSavedGIF(gif) {
				return nil, huma.Error400BadRequest("This GIF cannot be saved.")
			}
			return editPicker(s, ctx, func(library *appdata.Library, now time.Time) { library.SaveGIF(gif, now) })
		})
	huma.Register(s.API, appOperation("DELETE", "library/gifs/{id}", "delete-app-gif", "Remove a saved GIF",
		"Removes the GIF from the signed-in account's library. An absent ID leaves the library contents as they are.", 0),
		func(ctx context.Context, in *struct {
			ID string `path:"id" minLength:"1" maxLength:"100" doc:"ID of the saved GIF."`
		}) (*Output[appdata.Library], error) {
			return editPicker(s, ctx, func(library *appdata.Library, _ time.Time) { library.DeleteGIF(in.ID) })
		})
}

// Keep Huma's request schema link out of GIFs embedded in chat messages and events.
type SavePickerGIF model.GIF

func editPicker(s *Server, ctx context.Context, edit func(*appdata.Library, time.Time)) (*Output[appdata.Library], error) {
	return updateAppData(s, ctx, libraryDocument, func(raw json.RawMessage) (appdata.Library, error) {
		now := time.Now()
		library := appdata.ReadLibrary(raw, now)
		edit(&library, now)
		stamp := appdata.Timestamp(now)
		library.UpdatedAt = &stamp
		return library, nil
	})
}

type ThemeConflict struct {
	huma.ErrorModel
	Data struct {
		Theirs appdata.Theme `json:"theirs" doc:"The current saved theme."`
	} `json:"data"`
}

func (s *Server) registerThemeLibrary() {
	huma.Register(s.API, appOperation("GET", "themes", "get-app-themes", "Read the theme library",
		"Returns the signed-in account's themes and custom CSS preference.", 0),
		func(ctx context.Context, _ *struct{}) (*Output[appdata.Themes], error) {
			return readAppData(s, ctx, themesDocument, appdata.ReadThemes)
		})
	huma.Register(s.API, appOperation("PATCH", "themes", "update-app-themes", "Set the custom CSS preference",
		"Sets whether the signed-in account uses its theme CSS. The themes remain saved.", 4096),
		func(ctx context.Context, in *struct {
			Body struct {
				CustomCSS bool `json:"customCss"`
			}
		}) (*Output[appdata.Themes], error) {
			return editThemes(s, ctx, func(library *appdata.Themes, _ time.Time) error {
				library.CustomCSS = in.Body.CustomCSS
				return nil
			})
		})
	save := appOperation("PUT", "themes/{id}", "save-app-theme", "Save a personal theme",
		"Creates a theme or replaces the theme with this ID. A changed version returns 412 with the current theme unless force is true.", 512<<10)
	schema := s.API.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[ThemeConflict](), true, "ThemeConflict")
	save.Responses = map[string]*huma.Response{
		"412": {Description: "The theme changed since the edit began.", Content: map[string]*huma.MediaType{
			"application/problem+json": {Schema: schema},
		}},
	}
	huma.Register(s.API, save, s.saveAppTheme)
	huma.Register(s.API, appOperation("DELETE", "themes/{id}", "delete-app-theme", "Remove a saved theme",
		"Removes a theme from the signed-in account. An absent ID leaves the library contents as they are.", 0),
		func(ctx context.Context, in *struct {
			ID string `path:"id" pattern:"^[A-Za-z0-9_-]{1,40}$" doc:"ID of the personal theme."`
		}) (*Output[appdata.Themes], error) {
			return editThemes(s, ctx, func(library *appdata.Themes, _ time.Time) error {
				library.Themes = slices.DeleteFunc(library.Themes, func(theme appdata.Theme) bool { return theme.ID == in.ID })
				return nil
			})
		})
}

type saveAppThemeInput struct {
	ID   string `path:"id" pattern:"^[A-Za-z0-9_-]{1,40}$" doc:"ID of the personal theme."`
	Body struct {
		Theme    appdata.Theme `json:"theme"`
		Expected *string       `json:"expected" required:"false" maxLength:"40" doc:"The updatedAt value the editor loaded. Null for a new theme."`
		Force    bool          `json:"force,omitempty" doc:"Replace the theme even when its saved version changed."`
	}
}

func (s *Server) saveAppTheme(ctx context.Context, in *saveAppThemeInput) (*Output[appdata.Themes], error) {
	if len(in.Body.Theme.CSS) > appdata.ThemeCSSLimit {
		return nil, huma.Error400BadRequest("A theme can have up to 64 KiB of CSS.")
	}
	return editThemes(s, ctx, func(library *appdata.Themes, now time.Time) error {
		theme := in.Body.Theme.Clean()
		theme.ID = in.ID
		theme.UpdatedAt = appdata.Timestamp(now)
		index := slices.IndexFunc(library.Themes, func(item appdata.Theme) bool { return item.ID == in.ID })
		if index < 0 {
			if len(library.Themes) == appdata.ThemeCountLimit {
				return huma.Error400BadRequest("You can keep up to 20 themes. Delete one to add another.")
			}
			library.Themes = append(library.Themes, theme)
			return nil
		}
		stored := library.Themes[index]
		if !in.Body.Force && (in.Body.Expected == nil || *in.Body.Expected != stored.UpdatedAt) {
			conflict := &ThemeConflict{ErrorModel: huma.ErrorModel{
				Status: http.StatusPreconditionFailed, Title: http.StatusText(http.StatusPreconditionFailed),
				Detail: "This theme was changed on another device.",
			}}
			conflict.Data.Theirs = stored
			return conflict
		}
		if previous, err := time.Parse(time.RFC3339Nano, stored.UpdatedAt); err == nil &&
			!now.Truncate(time.Millisecond).After(previous) {
			theme.UpdatedAt = appdata.Timestamp(previous.Add(time.Millisecond))
		}
		library.Themes[index] = theme
		return nil
	})
}

func editThemes(s *Server, ctx context.Context, edit func(*appdata.Themes, time.Time) error) (*Output[appdata.Themes], error) {
	return updateAppData(s, ctx, themesDocument, func(raw json.RawMessage) (appdata.Themes, error) {
		now := time.Now()
		library := appdata.ReadThemes(raw)
		if err := edit(&library, now); err != nil {
			return library, err
		}
		stamp := appdata.Timestamp(now)
		library.UpdatedAt = &stamp
		return library, nil
	})
}
