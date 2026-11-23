package msgindex

import (
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
)

func linkURLs(list []Link) []string {
	out := []string{}
	for _, l := range list {
		out = append(out, l.URL)
	}
	return out
}

func TestLinksFindsLinksInMessageText(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"several links", "read https://a.test/one and http://b.test/two?x=1 now", []string{"https://a.test/one", "http://b.test/two?x=1"}},
		{"sentence punctuation", "see https://a.test/page. Or https://b.test/x, (https://c.test/y)!", []string{"https://a.test/page", "https://b.test/x", "https://c.test/y"}},
		{"balanced parentheses stay", "https://en.wikipedia.org/wiki/Go_(programming_language).", []string{"https://en.wikipedia.org/wiki/Go_(programming_language)"}},
		{"unicode path", "→ https://例え.テスト/パス?q=猫 ok", []string{"https://例え.テスト/パス?q=猫"}},
		{"IRC formatting ends a link", "\x02https://a.test/bold\x02 and \x0304https://b.test/red\x03", []string{"https://a.test/bold", "https://b.test/red"}},
		{"no link inside code", "\x11https://b.test/mono\x11\n```\nhttps://c.test/fenced\n```\nhttps://d.test/after", []string{"https://d.test/after"}},
		{"credentials are never indexed", "https://user:secret@a.test/x https://b.test/", []string{"https://b.test/"}},
		{"not a link", "ftp://a.test/x https:// https://:80/ mailto:a@b.test", []string{}},
		// Received IRC text has no inline code; a composer's `code` arrives as monospace.
		{"stray backticks", "`https://a.test/x`", []string{"https://a.test/x"}},
		{"the same URL twice counts once", "https://a.test/x https://a.test/x", []string{"https://a.test/x"}},
	} {
		if got := linkURLs(Links(tc.text)); strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestLinksReadMaskedLinks(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"masked link", "see [docs](https://x.test) now", []string{"https://x.test"}},
		{"masked link at the end", "see [docs](https://x.test/a?b=1)", []string{"https://x.test/a?b=1"}},
		{"parenthesised url", "[Go](https://en.wikipedia.org/wiki/Go_(programming_language)) is a language", []string{"https://en.wikipedia.org/wiki/Go_(programming_language)"}},
		{"parenthesised url, bare", "https://en.wikipedia.org/wiki/Go_(programming_language) is a language", []string{"https://en.wikipedia.org/wiki/Go_(programming_language)"}},
		{"url with an unbalanced parenthesis", "[odd](https://x.test/a_(b) end", []string{"https://x.test/a_(b)"}},
		{"bare url followed by a parenthesis", "(see https://x.test/a) and https://y.test/b)", []string{"https://x.test/a", "https://y.test/b"}},
		{"masked link keeps the url exactly", "[a](https://x.test/a.)", []string{"https://x.test/a."}},
		{"two masked links", "[a](https://a.test/1) and [b](https://b.test/2)!", []string{"https://a.test/1", "https://b.test/2"}},
		{"masked and bare in order", "https://a.test/0 [b](https://b.test/1) https://c.test/2", []string{"https://a.test/0", "https://b.test/1", "https://c.test/2"}},
		{"the text is a url", "[https://a.test/shown](https://b.test/real)", []string{"https://b.test/real"}},
		{"masked link in code is ignored", "`[a](https://a.test/x)` \x11[b](https://b.test/y)\x11\n```\n[c](https://c.test/z)\n```\n[d](https://d.test/w)", []string{"https://a.test/x", "https://d.test/w"}},
		{"masked link with credentials", "[a](https://user:secret@a.test/x) [b](https://b.test/y)", []string{"https://b.test/y"}},
		{"masked link that is not http", "[a](ftp://a.test/x) [b](javascript:alert(1))", []string{}},
		{"masked link without a closing parenthesis", "[a](https://a.test/x and more", []string{"https://a.test/x"}},
	} {
		if got := linkURLs(Links(tc.text)); strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestMaskedLinksKeepTheBareLinkRules(t *testing.T) {
	long := "https://a.test/" + strings.Repeat("a", MaxLinkLength)
	if got := Links("[long](" + long + ") [ok](https://a.test/ok)"); len(got) != 1 || got[0].URL != "https://a.test/ok" {
		t.Errorf("a too long masked link is left out: %+v", got)
	}
	got := Links("[docs](https://X.Test:443/Path?q=1#top)")
	if len(got) != 1 || got[0].Identity != "https://x.test/Path?q=1" || got[0].Host != "x.test" {
		t.Errorf("masked link identity: %+v", got)
	}
	var text strings.Builder
	for i := 0; i < MaxLinksPerMessage+5; i++ {
		text.WriteString("[l](https://a.test/" + strings.Repeat("x", i+1) + ") ")
	}
	if got := Links(text.String()); len(got) != MaxLinksPerMessage {
		t.Errorf("masked links are capped: %d", len(got))
	}
}

func TestLinksAreBounded(t *testing.T) {
	var b strings.Builder
	for i := range MaxLinksPerMessage + 5 {
		b.WriteString("https://a.test/" + strings.Repeat("x", i+1) + " ")
	}
	b.WriteString("https://a.test/" + strings.Repeat("y", MaxLinkLength))
	if got := Links(b.String()); len(got) != MaxLinksPerMessage {
		t.Fatalf("links: %d", len(got))
	}
	if got := Links("https://a.test/" + strings.Repeat("y", MaxLinkLength)); len(got) != 0 {
		t.Fatalf("an overlong URL was indexed: %d", len(got))
	}
}

func TestIdentityGroupsOnlyTheSameResource(t *testing.T) {
	id := func(raw string) string {
		got := Links(raw)
		if len(got) != 1 {
			t.Fatalf("%q: %v", raw, got)
		}
		return got[0].Identity
	}
	same := [][2]string{
		{"https://A.Test/Path?q=1", "https://a.test/Path?q=1"},
		{"https://a.test:443/x", "https://a.test/x"},
		{"https://a.test/x#section", "https://a.test/x"},
		{"https://a.test", "https://a.test/"},
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ", "https://youtu.be/dQw4w9WgXcQ?t=30"},
	}
	for _, pair := range same {
		if id(pair[0]) != id(pair[1]) {
			t.Errorf("%q and %q should group", pair[0], pair[1])
		}
	}
	different := [][2]string{
		{"https://a.test/article/1", "https://a.test/article/2"},
		{"https://a.test/x?id=1", "https://a.test/x?id=2"},
		{"https://a.test/file?X-Amz-Signature=abc", "https://a.test/file?X-Amz-Signature=def"},
		{"https://a.test/x?a=1&b=2", "https://a.test/x?b=2&a=1"},
		{"https://a.test/Path", "https://a.test/path"},
		{"https://a.test/x", "http://a.test/x"},
		{"https://a.test:8443/x", "https://a.test/x"},
		{"https://news.test/", "https://blog.news.test/"},
	}
	for _, pair := range different {
		if id(pair[0]) == id(pair[1]) {
			t.Errorf("%q and %q must not group", pair[0], pair[1])
		}
	}
	// The original link, query and fragment included, is what is kept and opened.
	if got := Links("https://a.test/x?sig=1#frag")[0].URL; got != "https://a.test/x?sig=1#frag" {
		t.Fatalf("original URL: %q", got)
	}
}

func TestKindsMatchTheTimelinePreviews(t *testing.T) {
	for raw, want := range map[string]string{
		"https://a.test/page":                        KindLink,
		"https://i.imgur.com/cat.JPG":                KindImage,
		"https://a.test/cat.webp":                    KindImage,
		"https://media.tenor.com/x/cat.webp":         KindGIF,
		"https://a.test/dance.gif":                   KindGIF,
		"https://a.test/clip.mp4?token=1":            KindVideo,
		"https://a.test/song.flac":                   KindAudio,
		"https://www.youtube.com/shorts/dQw4w9WgXcQ": KindYouTube,
		"http://www.youtube.com/watch?v=dQw4w9WgXcQ": KindLink,
		"https://www.youtube.com/watch?v=tooshort":   KindLink,
	} {
		if got := Links(raw); len(got) != 1 || got[0].Kind != want {
			t.Errorf("%s: %+v, want %s", raw, got, want)
		}
	}
}

func TestHostsAreNormalizedToASCII(t *testing.T) {
	a, b := Links("https://Bücher.example/a")[0], Links("https://xn--bcher-kva.example/a")[0]
	if a.Host != "xn--bcher-kva.example" || a.Host != b.Host || a.Identity != b.Identity {
		t.Fatalf("unicode %+v, punycode %+v", a, b)
	}
	if got := NormalizeHost("WWW.Example.COM."); got != "www.example.com" {
		t.Fatalf("normalized: %q", got)
	}
}

func TestCapitalSchemesAreNotLinks(t *testing.T) {
	// The timeline links only lowercase http:// and https:// (MessageContent.vue).
	if got := Links("HTTPS://A.TEST/x Http://b.test/"); len(got) != 0 {
		t.Fatalf("links: %+v", got)
	}
}

// The timeline, server previews and the catalog share one definition of code.
func TestLinksSkipCodeLikeTheFrontend(t *testing.T) {
	raw, err := os.ReadFile("testdata/code-parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Text string
		URLs       []string
	}
	if err = json.Unmarshal(raw, &cases); err != nil || len(cases) == 0 {
		t.Fatalf("parity fixture: %v", err)
	}
	for _, tc := range cases {
		want := map[string]bool{}
		for _, u := range tc.URLs {
			want[trimTrailing(u)] = true
		}
		for _, l := range Links(tc.Text) {
			if strings.HasPrefix(l.URL, "https://") && !want[l.URL] {
				t.Errorf("%s: indexed %q, which the timeline shows as code", tc.Name, l.URL)
			}
		}
	}
}

// masked-links.json is the case list the web and desktop clients share. The
// catalogue must list the address every client links, and a request for the
// canonical spelling the clients use must name the address as typed.
func TestMaskedLinksMatchTheSharedClientCases(t *testing.T) {
	raw, err := os.ReadFile("testdata/masked-links.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Input string
		Targets     []string
		Ordinary    *string
	}
	if err = json.Unmarshal(raw, &cases); err != nil || len(cases) == 0 {
		t.Fatalf("shared cases: %v", err)
	}
	// Shapes the catalogue deliberately reads differently from the clients.
	skip := map[string]string{
		"non-canonical address": "https:host without slashes is not a link in message text",
	}
	for _, tc := range cases {
		if why, ok := skip[tc.Name]; ok {
			t.Logf("%s: skipped, %s", tc.Name, why)
			continue
		}
		want := []string{}
		for _, target := range append(tc.Targets, derefs(tc.Ordinary)...) {
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
				want = append(want, target)
			}
		}
		links := Links(tc.Input)
		for _, target := range want {
			// The catalogue never lists an address with credentials.
			if u, err := url.Parse(target); err == nil && u.User != nil {
				continue
			}
			found := false
			for _, link := range links {
				found = found || SameAddress(link.URL, target)
			}
			if !found {
				t.Errorf("%s: catalogue %q lacks the address %q the clients link", tc.Name, linkURLs(links), target)
			}
		}
		for _, link := range links {
			if strings.Contains(link.URL, "](") {
				t.Errorf("%s: %q holds the markup around a link", tc.Name, link.URL)
			}
		}
	}
}

func derefs(s *string) []string {
	if s == nil {
		return nil
	}
	return []string{*s}
}

func TestMaskedLinkShapes(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"a masked link inside another link's text wins", "[a [b](https://x.test/in) c](https://y.test/out)", []string{"https://x.test/in", "https://y.test/out"}},
		{"the address ends at a space", "[a](https://x.test/a b) c", []string{"https://x.test/a"}},
		{"adjacent masked links", "[a](https://a.test/1)[b](https://b.test/2)", []string{"https://a.test/1", "https://b.test/2"}},
		{"a masked link inside an address", "[a](https://x.test/[b](https://y.test/z))", []string{"https://x.test/[b](https://y.test/z)"}},
		{"brackets in the text", "[see [1]](https://x.test/a.)", []string{"https://x.test/a."}},
		{"escaped brackets in the text", `[a \] b](https://x.test/a.)`, []string{"https://x.test/a."}},
		{"capital scheme", "[Docs](HTTPS://X.test/up)", []string{"HTTPS://X.test/up"}},
		{"an escaped opening bracket opens nothing", `\[x](https://x.test/a)`, []string{"https://x.test/a"}},
	} {
		if got := linkURLs(Links(tc.text)); strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSameAddressReadsTheCanonicalSpelling(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"https://x.test", "https://x.test/", true},
		{"HTTPS://X.Test:443/a", "https://x.test/a", true},
		{"https://bücher.test/ä?q=ö#é", "https://xn--bcher-kva.test/%C3%A4?q=%C3%B6#%C3%A9", true},
		{"https://x.test/a b", "https://x.test/a%20b", true},
		{"https://x.test/a", "https://x.test/b", false},
		{"https://x.test/a", "http://x.test/a", false},
		{"https://x.test/a?q=1", "https://x.test/a", false},
		{"https://x.test/a#top", "https://x.test/a", false},
		{"https://u:p@x.test/", "https://u:p@x.test/", false},
	} {
		if got := SameAddress(tc.a, tc.b); got != tc.same {
			t.Errorf("SameAddress(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.same)
		}
	}
}

// Many opened, never closed links are the worst case for a scan that looks
// ahead from each one; this scan makes one pass, so the input is cheap.
func BenchmarkLinksUnclosedMaskedLinks(b *testing.B) {
	text := strings.Repeat("[a](https://", 1400)
	for i := 0; i < b.N; i++ {
		Links(text)
	}
}

func TestUnclosedMaskedLinksAreNotLinks(t *testing.T) {
	if got := Links(strings.Repeat("[a](https://", 1400)); len(got) != 0 {
		t.Errorf("unclosed links: %+v", got)
	}
}
