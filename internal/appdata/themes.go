package appdata

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
)

const ThemeCSSLimit = 64 << 10
const ThemeCountLimit = 20

var backgroundPath = regexp.MustCompile("^/(files/[A-Za-z0-9_-]{1,80}/[A-Za-z0-9_-]{1,200}/[A-Za-z0-9._~%$&+,:;=@-]{1,400}|api/v1/uploads/[A-Za-z0-9-]{1,80})$")

type ThemeBase struct {
	Dark  string `json:"dark" required:"false" maxLength:"63"`
	Light string `json:"light" required:"false" maxLength:"63"`
}

type ThemePalette struct {
	Red    string `json:"red" pattern:"^#[0-9a-fA-F]{6}$"`
	Orange string `json:"orange" pattern:"^#[0-9a-fA-F]{6}$"`
	Yellow string `json:"yellow" pattern:"^#[0-9a-fA-F]{6}$"`
	Green  string `json:"green" pattern:"^#[0-9a-fA-F]{6}$"`
	Cyan   string `json:"cyan" pattern:"^#[0-9a-fA-F]{6}$"`
	Blue   string `json:"blue" pattern:"^#[0-9a-fA-F]{6}$"`
	Purple string `json:"purple" pattern:"^#[0-9a-fA-F]{6}$"`
	Pink   string `json:"pink" pattern:"^#[0-9a-fA-F]{6}$"`
}

type ThemeSeeds struct {
	Canvas  string       `json:"canvas" pattern:"^#[0-9a-fA-F]{6}$"`
	Surface string       `json:"surface" pattern:"^#[0-9a-fA-F]{6}$"`
	Overlay string       `json:"overlay" pattern:"^#[0-9a-fA-F]{6}$"`
	Ink     string       `json:"ink" pattern:"^#[0-9a-fA-F]{6}$"`
	Muted   string       `json:"muted" pattern:"^#[0-9a-fA-F]{6}$"`
	Accent  string       `json:"accent" pattern:"^#[0-9a-fA-F]{6}$"`
	Palette ThemePalette `json:"palette"`
}

type ThemeColors struct {
	Dark  *ThemeSeeds `json:"dark,omitempty"`
	Light *ThemeSeeds `json:"light,omitempty"`
}

type ThemeBackground struct {
	URL          *string  `json:"url" required:"false" maxLength:"800" doc:"An upload path on this instance. Null if an imported theme needs an image."`
	Placement    string   `json:"placement" required:"false" enum:"conversation,app"`
	Fit          string   `json:"fit" required:"false" enum:"cover,contain,tile"`
	Position     string   `json:"position" required:"false" enum:"center,top,bottom,left,right"`
	Dim          *int     `json:"dim" required:"false" minimum:"0" maximum:"90"`
	Blur         *int     `json:"blur" required:"false" minimum:"0" maximum:"20"`
	PanelOpacity *int     `json:"panelOpacity" required:"false" minimum:"40" maximum:"100"`
	Tones        []string `json:"tones" required:"false" minItems:"2" maxItems:"2" doc:"Two measured image colors, dark then light, or null."`
}

type Theme struct {
	Format     int              `json:"format" required:"false" minimum:"1"`
	ID         string           `json:"id" required:"false" pattern:"^[A-Za-z0-9_-]{1,40}$" doc:"The save route takes the ID from the path."`
	Name       string           `json:"name" required:"false" maxLength:"60"`
	Base       ThemeBase        `json:"base" required:"false"`
	Colors     ThemeColors      `json:"colors" required:"false"`
	Background *ThemeBackground `json:"background" required:"false"`
	CSS        string           `json:"css" required:"false" doc:"Custom CSS up to 64 KiB. Clients filter CSS before applying it."`
	MadeWith   string           `json:"madeWith" required:"false" maxLength:"40"`
	UpdatedAt  string           `json:"updatedAt" required:"false" maxLength:"40" doc:"Version of the stored theme. The server sets it on save."`
}

func (Theme) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	return nullableObject(schema, "background")
}

type Themes struct {
	Format    int     `json:"format" enum:"1"`
	CustomCSS bool    `json:"customCss" doc:"The account's opt-in to using its stored custom CSS."`
	Themes    []Theme `json:"themes" maxItems:"20" nullable:"false"`
	UpdatedAt *string `json:"updatedAt" format:"date-time"`
}

func text(value string, limit int) string {
	for ix := range value {
		if limit == 0 {
			return value[:ix]
		}
		limit--
	}
	return value
}

func limitBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit]
}

func (s *ThemeSeeds) clean() *ThemeSeeds {
	if s == nil {
		return nil
	}
	for _, field := range []*string{
		&s.Canvas, &s.Surface, &s.Overlay, &s.Ink, &s.Muted, &s.Accent, &s.Palette.Red,
		&s.Palette.Orange, &s.Palette.Yellow, &s.Palette.Green, &s.Palette.Cyan, &s.Palette.Blue,
		&s.Palette.Purple, &s.Palette.Pink,
	} {
		if !hexColor.MatchString(*field) {
			return nil
		}
		*field = strings.ToLower(*field)
	}
	return s
}

func (b *ThemeBackground) clean() *ThemeBackground {
	if b == nil {
		return nil
	}
	if b.URL != nil {
		address, err := url.Parse(*b.URL)
		b.URL = nil
		if err == nil && address.RawQuery == "" && !address.ForceQuery && address.Fragment == "" &&
			backgroundPath.MatchString(address.EscapedPath()) {
			b.URL = pointer(address.EscapedPath())
		}
	}
	b.Placement = choice(b.Placement, "conversation", "app")
	b.Fit = choice(b.Fit, "cover", "contain", "tile")
	b.Position = choice(b.Position, "center", "top", "bottom", "left", "right")
	b.Dim = clamp(b.Dim, 0, 90, 55)
	b.Blur = clamp(b.Blur, 0, 20, 0)
	b.PanelOpacity = clamp(b.PanelOpacity, 40, 100, 85)
	if len(b.Tones) == 2 && hexColor.MatchString(b.Tones[0]) && hexColor.MatchString(b.Tones[1]) {
		b.Tones[0] = strings.ToLower(b.Tones[0])
		b.Tones[1] = strings.ToLower(b.Tones[1])
	} else {
		b.Tones = nil
	}
	return b
}

func (t Theme) Clean() Theme {
	t.Format = max(1, t.Format)
	t.Name = strings.TrimSpace(text(t.Name, 60))
	if t.Name == "" {
		t.Name = "Untitled theme"
	}
	if !schemeID.MatchString(t.Base.Dark) {
		t.Base.Dark = "ircord-dark"
	}
	if !schemeID.MatchString(t.Base.Light) {
		t.Base.Light = "ircord-light"
	}
	t.Colors.Dark = t.Colors.Dark.clean()
	t.Colors.Light = t.Colors.Light.clean()
	t.Background = t.Background.clean()
	t.CSS = limitBytes(t.CSS, ThemeCSSLimit)
	t.MadeWith = text(t.MadeWith, 40)
	t.UpdatedAt = text(t.UpdatedAt, 40)
	return t
}

func ReadThemes(raw json.RawMessage) Themes {
	var saved Themes
	saved.CustomCSS = true
	decode(raw, &saved)
	library := Themes{Format: 1, Themes: []Theme{}, CustomCSS: saved.CustomCSS, UpdatedAt: saved.UpdatedAt}
	seen := map[string]bool{}
	for _, theme := range saved.Themes {
		if len(library.Themes) == ThemeCountLimit {
			break
		}
		if themeID.MatchString(theme.ID) && !seen[theme.ID] {
			seen[theme.ID] = true
			library.Themes = append(library.Themes, theme.Clean())
		}
	}
	return library
}
