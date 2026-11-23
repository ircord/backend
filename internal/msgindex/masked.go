package msgindex

import (
	"net/url"
	"sort"
	"strings"
)

// maxMaskedAddress is the longest address a masked link takes; a longer one
// stays as typed, like the clients, which keeps scanning linear.
const maxMaskedAddress = 2048

// addressEnd reports the bytes that end an address: whitespace, control
// characters and markup delimiters, the same set that ends a bare link.
func addressEnd(c byte) bool {
	return c <= ' ' || c == 0x7f || c == '<' || c == '>' || c == '"'
}

// MaskedLinks finds the masked links, [text](url), in a text and returns their
// urls in order, exactly as typed, together with the text with each whole
// masked link blanked, so a scan for bare links does not read it again.
//
// The rules follow the clients. The text may hold balanced brackets and
// backslash-escaped ones, but no line break. The address starts with http:// or
// https:// in any case, holds no whitespace, control character or markup
// delimiter, and ends at the parenthesis matching the one that opened it, so
// an address with balanced parentheses keeps them. When the text of a link
// holds another link, the inner link wins and the outer address is read as a
// bare link. The scan is linear in the length of the text.
func MaskedLinks(text string) (urls []string, rest string) {
	found := scanMasked(text)
	if len(found) == 0 {
		return nil, text
	}
	blanked := []byte(text)
	urls = make([]string, len(found))
	for i, m := range found {
		urls[i] = text[m.urlStart:m.end]
		for j := m.open; j <= m.end; j++ {
			blanked[j] = ' '
		}
	}
	return urls, string(blanked)
}

type maskedLink struct {
	// open is the offset of "[", close of its "]", urlStart of the address and
	// end of the ")" that closes the link.
	open, close, urlStart, end int
}

func scanMasked(text string) []maskedLink {
	if !strings.Contains(text, "](") {
		return nil
	}
	n := len(text)
	// One forward pass: backslash escapes, then bracket and parenthesis pairs.
	bracketClose := make([]int, n)
	parenClose := make([]int, n)
	var brackets, parens []int
	slashes := 0
	for i := 0; i < n; i++ {
		c := text[i]
		bracketClose[i], parenClose[i] = -1, -1
		escaped := slashes%2 == 1
		if c == '\\' {
			slashes++
		} else {
			slashes = 0
		}
		switch {
		case c == '\n':
			brackets = brackets[:0]
		case escaped:
		case c == '[':
			brackets = append(brackets, i)
		case c == ']' && len(brackets) > 0:
			bracketClose[brackets[len(brackets)-1]] = i
			brackets = brackets[:len(brackets)-1]
		}
		switch {
		case addressEnd(c):
			parens = parens[:0]
		case c == '(':
			parens = append(parens, i)
		case c == ')' && len(parens) > 0:
			parenClose[parens[len(parens)-1]] = i
			parens = parens[:len(parens)-1]
		}
	}
	// Candidates: [text] followed at once by an (address) that starts with a scheme.
	var candidates []maskedLink
	for open := 0; open < n; open++ {
		closing := bracketClose[open]
		if closing < 0 || closing == open+1 || closing+1 >= n || text[closing+1] != '(' {
			continue
		}
		end := parenClose[closing+1]
		start := closing + 2
		if end < 0 || end-start > maxMaskedAddress || !hasHTTPScheme(text[start:end]) {
			continue
		}
		candidates = append(candidates, maskedLink{open, closing, start, end})
	}
	var out []maskedLink
	after := 0
	for i, c := range candidates {
		// A link whose text holds another link is an outer one: the inner wins.
		if c.open < after || (i+1 < len(candidates) && candidates[i+1].open < c.close) {
			continue
		}
		out = append(out, c)
		after = c.end + 1
	}
	return out
}

func hasHTTPScheme(address string) bool {
	head := strings.ToLower(address[:min(len(address), len("https://"))])
	return strings.HasPrefix(head, "http://") && len(address) > len("http://") ||
		strings.HasPrefix(head, "https://") && len(address) > len("https://")
}

// candidates lists the urls a text names, in order. A masked link,
// [text](url), names exactly its url: the parenthesis that closes it is not
// part of the url, and neither is its text, even when the text is a url. Every
// other url is a bare link that ends at whitespace and loses sentence
// punctuation (trimTrailing).
func candidates(text string) []string {
	type at struct {
		pos int
		raw string
	}
	var all []at
	rest := text
	if found := scanMasked(text); len(found) > 0 {
		var urls []string
		urls, rest = MaskedLinks(text)
		for i, m := range found {
			all = append(all, at{m.open, urls[i]})
		}
	}
	for _, m := range candidate.FindAllStringIndex(rest, -1) {
		all = append(all, at{m[0], trimTrailing(rest[m[0]:m[1]])})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].pos < all[j].pos })
	out := make([]string, len(all))
	for i, f := range all {
		out[i] = f.raw
	}
	return out
}

// SameAddress reports whether two spellings name the same address once
// normalized the way a browser writes it: scheme and host in lower case (a
// non-ASCII host in punycode), the default port dropped, an empty path as "/",
// and non-ASCII or unsafe characters percent-encoded. Clients show a masked
// link under that canonical spelling, so a request for it is a request for the
// address as typed.
func SameAddress(a, b string) bool {
	ca, okA := canonicalAddress(a)
	cb, okB := canonicalAddress(b)
	return okA && okB && ca == cb
}

func canonicalAddress(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	host := NormalizeHost(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && !(scheme == "http" && port == "80") && !(scheme == "https" && port == "443") {
		host += ":" + port
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	out := scheme + "://" + host + path
	if u.RawQuery != "" || strings.Contains(raw, "?") {
		out += "?" + escapeUnsafe(u.RawQuery)
	}
	if fragment := u.EscapedFragment(); fragment != "" || strings.Contains(raw, "#") {
		out += "#" + fragment
	}
	return out, true
}

// escapeUnsafe percent-encodes what a browser encodes in a query: bytes
// outside printable ASCII and the characters that would end an address.
func escapeUnsafe(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c >= 0x7f || c == '"' || c == '<' || c == '>' {
			b.WriteString("%" + strings.ToUpper(string("0123456789abcdef"[c>>4])+string("0123456789abcdef"[c&15])))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
