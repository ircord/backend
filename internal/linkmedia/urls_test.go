package linkmedia

import (
	"slices"
	"testing"
)

func TestURLsReadMaskedLinksAsTheirAddress(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"masked", "see [docs](https://x.test/a) now", []string{"https://x.test/a"}},
		{"text is the address", "[https://x.test/a](https://x.test/a)", []string{"https://x.test/a"}},
		{"adjacent", "[a](https://a.test/1)[b](https://b.test/2)", []string{"https://a.test/1", "https://b.test/2"}},
		{"parentheses", "[w](https://en.wikipedia.org/wiki/Go_(language))", []string{"https://en.wikipedia.org/wiki/Go_(language)"}},
		{"bare keeps its rules", "(https://x.test/a), https://y.test/b's.", []string{"https://x.test/a", "https://y.test/b"}},
		{"private address", "[a](http://127.0.0.1/x)", nil},
	} {
		if got := URLs(tc.text); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestAllowsTheCanonicalSpellingOfAMaskedLink(t *testing.T) {
	for _, tc := range []struct {
		text, raw string
		want      bool
	}{
		{"[a](https://x.test)", "https://x.test", true},
		{"[a](https://x.test)", "https://x.test/", true},
		{"[a](HTTPS://X.test/p)", "https://x.test/p", true},
		{"[a](https://x.test)", "https://x.test/other", false},
		// A bare link is requested as typed.
		{"https://x.test", "https://x.test/", false},
		{"https://x.test", "https://x.test", true},
		{"[a](http://127.0.0.1/x)", "http://127.0.0.1/x", false},
	} {
		if got := Allows(tc.text, tc.raw); got != tc.want {
			t.Errorf("Allows(%q, %q) = %v, want %v", tc.text, tc.raw, got, tc.want)
		}
	}
}
