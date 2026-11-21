package appdata

import (
	"encoding/json"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
)

var (
	hexColor = regexp.MustCompile("^#[0-9a-fA-F]{6}$")
	schemeID = regexp.MustCompile("^[a-z0-9][a-z0-9-]{0,62}$")
	objectID = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9._:-]{0,119}$")
	themeID  = regexp.MustCompile("^[A-Za-z0-9_-]{1,40}$")
	iconURL  = regexp.MustCompile("^data:image/(png|jpeg|webp);base64,[A-Za-z0-9+/=]+$")
	keyName  = regexp.MustCompile("^[A-Za-z][A-Za-z0-9]{1,19}$")
)

func pointer[T any](value T) *T { return &value }

func choice(value, fallback string, allowed ...string) string {
	if slices.Contains(allowed, value) {
		return value
	}
	return fallback
}

func clamp(value *int, low, high, fallback int) *int {
	if value == nil {
		return &fallback
	}
	return pointer(max(low, min(high, *value)))
}

func bounded(value string, maximum int) bool {
	return value != "" && utf8.RuneCountInString(value) <= maximum
}

func color(value *string) *string {
	if value == nil || !hexColor.MatchString(*value) {
		return nil
	}
	return pointer(strings.ToLower(*value))
}

// decode reads previous document formats. A wrong field type leaves that
// field at its default. Other fields are still decoded by encoding/json.
func decode(raw json.RawMessage, target any) { _ = json.Unmarshal(raw, target) }

type Appearance struct {
	Mode           string  `json:"mode" required:"false" enum:"system,dark,light"`
	SchemeDark     string  `json:"schemeDark" required:"false" maxLength:"63" doc:"Dark scheme identifier. Defaults to ircord-dark."`
	SchemeLight    string  `json:"schemeLight" required:"false" maxLength:"63" doc:"Light scheme identifier. Defaults to ircord-light."`
	Accent         string  `json:"accent" required:"false" enum:"scheme,custom,lavender,indigo,blue,sky,teal,emerald,lime,amber,orange,rose,pink,fuchsia"`
	AccentCustom   *string `json:"accentCustom" required:"false" pattern:"^#[0-9a-fA-F]{6}$"`
	Font           string  `json:"font" required:"false" enum:"geist,inter,ibm-plex,manrope,nunito,atkinson,source-sans,system"`
	InterfaceScale *int    `json:"interfaceScale" required:"false" minimum:"85" maximum:"150" doc:"Interface scale in percent."`
	MessageFont    string  `json:"messageFont" required:"false" enum:"interface,atkinson,lexend,serif,literata,jetbrains-mono,geist-mono,atkinson-mono"`
	MessageSize    *int    `json:"messageSize" required:"false" minimum:"12" maximum:"20"`
	MessageLayout  string  `json:"messageLayout" required:"false" enum:"cozy,compact,irc"`
	GroupSpacing   *int    `json:"groupSpacing" required:"false" minimum:"0" maximum:"24" doc:"Pixels between message groups."`
	Density        string  `json:"density" required:"false" enum:"comfortable,compact"`
	Corners        string  `json:"corners" required:"false" enum:"rounded,soft,square"`
	TimeFormat     string  `json:"timeFormat" required:"false" enum:"24h,12h"`
	ColoredNames   *bool   `json:"coloredNames" required:"false"`
	AnimateMedia   *bool   `json:"animateMedia" required:"false"`
	Motion         string  `json:"motion" required:"false" enum:"system,full,reduced"`
	Theme          *string `json:"theme" required:"false" pattern:"^[A-Za-z0-9_-]{1,40}$" doc:"Saved theme ID, or null to use the scheme."`
}

type StoredAppearance struct {
	Settings  *Appearance `json:"settings" doc:"The account's appearance. Null until saved."`
	UpdatedAt *string     `json:"updatedAt" format:"date-time"`
}

// Huma references object schemas without inferring null for pointer fields.
func (StoredAppearance) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	return nullableObject(schema, "settings")
}

func nullableObject(schema *huma.Schema, name string) *huma.Schema {
	field := schema.Properties[name]
	schema.Properties[name] = &huma.Schema{
		Description: field.Description,
		AnyOf:       []*huma.Schema{{Ref: field.Ref}, {Type: "null"}},
	}
	return schema
}

func (a Appearance) Clean() Appearance {
	a.Mode = choice(a.Mode, "system", "dark", "light")
	if !schemeID.MatchString(a.SchemeDark) {
		a.SchemeDark = "ircord-dark"
	}
	if !schemeID.MatchString(a.SchemeLight) {
		a.SchemeLight = "ircord-light"
	}
	a.Accent = choice(a.Accent, "scheme", "custom", "lavender", "indigo", "blue", "sky", "teal", "emerald", "lime", "amber", "orange", "rose", "pink", "fuchsia")
	a.AccentCustom = color(a.AccentCustom)
	a.Font = choice(a.Font, "geist", "inter", "ibm-plex", "manrope", "nunito", "atkinson", "source-sans", "system")
	a.InterfaceScale = clamp(a.InterfaceScale, 85, 150, 100)
	a.MessageFont = choice(a.MessageFont, "interface", "atkinson", "lexend", "serif", "literata", "jetbrains-mono", "geist-mono", "atkinson-mono")
	a.MessageSize = clamp(a.MessageSize, 12, 20, 14)
	a.MessageLayout = choice(a.MessageLayout, "cozy", "compact", "irc")
	a.GroupSpacing = clamp(a.GroupSpacing, 0, 24, 12)
	a.Density = choice(a.Density, "comfortable", "compact")
	a.Corners = choice(a.Corners, "rounded", "soft", "square")
	a.TimeFormat = choice(a.TimeFormat, "24h", "12h")
	a.Motion = choice(a.Motion, "system", "full", "reduced")
	if a.ColoredNames == nil {
		a.ColoredNames = pointer(true)
	}
	if a.AnimateMedia == nil {
		a.AnimateMedia = pointer(true)
	}
	if a.Theme != nil && !themeID.MatchString(*a.Theme) {
		a.Theme = nil
	}
	return a
}

func ReadAppearance(raw json.RawMessage) StoredAppearance {
	var saved StoredAppearance
	decode(raw, &saved)
	if saved.Settings != nil {
		if saved.Settings.InterfaceScale == nil {
			var legacy struct {
				Settings struct {
					InterfaceSize *float64 `json:"interfaceSize"`
				} `json:"settings"`
			}
			decode(raw, &legacy)
			if size := legacy.Settings.InterfaceSize; size != nil && *size > 0 && *size < 100 {
				saved.Settings.InterfaceScale = pointer(int(*size/14*20+0.5) * 5)
			}
		}
		saved.Settings = pointer(saved.Settings.Clean())
	}
	return saved
}

type Startup struct {
	Mode      string  `json:"mode" required:"false" enum:"last,channel"`
	ChannelID *string `json:"channelId" required:"false" minLength:"1" maxLength:"120"`
}

type SpeechService struct {
	BaseURL string `json:"baseUrl" required:"false" maxLength:"500" doc:"HTTPS base URL, or HTTP on loopback. Leave empty to disable. Credentials, queries and fragments are forbidden."`
	Model   string `json:"model" required:"false" maxLength:"100"`
	Voice   string `json:"voice" required:"false" maxLength:"100"`
}

type Shortcut struct {
	Key   string `json:"key" minLength:"1" maxLength:"20"`
	Mod   bool   `json:"mod,omitempty"`
	Shift bool   `json:"shift,omitempty"`
	Alt   bool   `json:"alt,omitempty"`
}

type ClientPreferences struct {
	Startup          Startup               `json:"startup" required:"false"`
	Events           string                `json:"events" required:"false" enum:"show,collapse,hide"`
	NetworkIcons     map[string]string     `json:"networkIcons" required:"false" maxProperties:"30" doc:"Network IDs mapped to PNG, JPEG or WebP data URLs up to 64 KiB each."`
	RegisterChannels *bool                 `json:"registerChannels" required:"false" doc:"Register new home-network channels. Defaults to true."`
	AskBeforeEmbeds  bool                  `json:"askBeforeEmbeds" required:"false"`
	ShowNSFW         bool                  `json:"showNsfw" required:"false"`
	NSFWNetworks     map[string]bool       `json:"nsfwNetworks" required:"false" maxProperties:"100" doc:"Legacy opt-ins by network ID."`
	Speech           SpeechService         `json:"speech" required:"false" doc:"Speech service settings. API keys stay on the device."`
	Shortcuts        map[string][]Shortcut `json:"shortcuts" required:"false" maxProperties:"100" doc:"Keyboard overrides by action. An empty list clears the bindings for that action."`
	SidebarOrder     map[string][]string   `json:"sidebarOrder" required:"false" maxProperties:"50" doc:"Up to 200 ordered conversation IDs per sidebar section."`
	ChannelGuideSeen bool                  `json:"channelGuideSeen" required:"false"`
}

type StoredPreferences struct {
	Preferences *ClientPreferences `json:"preferences" doc:"The account's preferences. Null until saved."`
	UpdatedAt   *string            `json:"updatedAt" format:"date-time"`
}

func (StoredPreferences) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	return nullableObject(schema, "preferences")
}

func limitMap[T any](input map[string]T, maximum int, accept func(string, T) bool) map[string]T {
	output := map[string]T{}
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if len(output) == maximum {
			break
		}
		if value := input[key]; accept(key, value) {
			output[key] = value
		}
	}
	return output
}

func (p ClientPreferences) Clean() ClientPreferences {
	p.Startup.Mode = choice(p.Startup.Mode, "channel", "last")
	if p.Startup.ChannelID != nil && !bounded(*p.Startup.ChannelID, 120) {
		p.Startup.ChannelID = nil
	}
	p.Events = choice(p.Events, "collapse", "show", "hide")
	if p.RegisterChannels == nil {
		p.RegisterChannels = pointer(true)
	}
	p.NetworkIcons = limitMap(p.NetworkIcons, 30, func(id, address string) bool {
		return bounded(id, 120) && len(address) <= 64*1024 && iconURL.MatchString(address)
	})
	p.NSFWNetworks = limitMap(p.NSFWNetworks, 100, func(id string, enabled bool) bool {
		return enabled && objectID.MatchString(id)
	})
	p.Speech = p.Speech.clean()
	p.Shortcuts = limitMap(p.Shortcuts, 100, func(id string, bindings []Shortcut) bool {
		return objectID.MatchString(id) && len(bindings) <= 3 && slices.IndexFunc(bindings, func(binding Shortcut) bool {
			return !keyName.MatchString(binding.Key) &&
				(utf8.RuneCountInString(binding.Key) != 1 || strings.ContainsFunc(binding.Key, unicode.IsControl))
		}) < 0
	})
	p.SidebarOrder = limitMap(p.SidebarOrder, 50, func(section string, ids []string) bool {
		return bounded(section, 300) && ids != nil
	})
	for section, ids := range p.SidebarOrder {
		seen := map[string]bool{}
		order := []string{}
		for _, id := range ids {
			if bounded(id, 300) && !seen[id] {
				seen[id] = true
				order = append(order, id)
				if len(order) == 200 {
					break
				}
			}
		}
		p.SidebarOrder[section] = order
	}
	return p
}

func (s SpeechService) clean() SpeechService {
	s.BaseURL = strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
	u, err := url.Parse(s.BaseURL)
	if err != nil || !bounded(s.BaseURL, 500) || u.User != nil || u.Hostname() == "" ||
		strings.ContainsAny(s.BaseURL, "\u0020;,'\"\\*?#") || strings.ContainsFunc(s.BaseURL, unicode.IsSpace) {
		return SpeechService{}
	}
	host := strings.ToLower(u.Hostname())
	loopback := host == "localhost" || strings.HasSuffix(host, ".localhost") || net.ParseIP(host).IsLoopback()
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return SpeechService{}
	}
	for _, field := range []*string{&s.Model, &s.Voice} {
		if strings.ContainsFunc(*field, unicode.IsControl) {
			*field = ""
		}
		*field = strings.TrimSpace(*field)
		if !bounded(*field, 100) {
			*field = ""
		}
	}
	return s
}

func ReadPreferences(raw json.RawMessage) StoredPreferences {
	var saved StoredPreferences
	decode(raw, &saved)
	if saved.Preferences != nil {
		saved.Preferences = pointer(saved.Preferences.Clean())
	}
	return saved
}
