package linkmedia

import (
	"ircord/backend/internal/mediafetch"
	"ircord/backend/internal/msgindex"
	"regexp"
	"strings"
)

var messageURL = regexp.MustCompile(`https?://[^\s<>"'\x00-\x1f]+`)

// URLs finds link tokens in retained message text for request authorization.
// Code suppression is a presentation decision made by the client; this exact
// token check prevents a request from resolving an unrelated URL or substring.
// A masked link, [text](url), is the token url, as typed.
func URLs(text string) []string {
	masked, rest := msgindex.MaskedLinks(text)
	seen := map[string]bool{}
	var out []string
	add := func(raw string) {
		if _, err := mediafetch.URL(raw); err != nil || seen[raw] {
			return
		}
		seen[raw] = true
		out = append(out, raw)
	}
	for _, raw := range messageURL.FindAllString(rest, -1) {
		add(trimLink(raw))
	}
	for _, raw := range masked {
		add(raw)
	}
	return out
}

// Allows reports whether raw is a public address that names a link in the
// message text: one of its tokens exactly, or the canonical spelling of a masked link's address, which
// is how clients show and request it (https://x.test/ for [x](https://x.test)).
func Allows(text, raw string) bool {
	if _, err := mediafetch.URL(raw); err != nil {
		return false
	}
	return Names(text, raw)
}

// Names is Allows without the public-address check, for an address the caller
// authorizes some other way.
func Names(text, raw string) bool {
	masked, rest := msgindex.MaskedLinks(text)
	for _, token := range messageURL.FindAllString(rest, -1) {
		if trimLink(token) == raw {
			return true
		}
	}
	for _, address := range masked {
		if address == raw || msgindex.SameAddress(address, raw) {
			return true
		}
	}
	return false
}

func trimLink(raw string) string {
	raw = strings.TrimRight(raw, ".,!?;:")
	for _, pair := range [][2]string{{"(", ")"}, {"[", "]"}, {"{", "}"}} {
		for strings.HasSuffix(raw, pair[1]) && strings.Count(raw, pair[1]) > strings.Count(raw, pair[0]) {
			raw = strings.TrimSuffix(raw, pair[1])
		}
	}
	return raw
}
