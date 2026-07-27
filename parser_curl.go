package main

import (
	"fmt"
	"net/http"
	"strings"
)

// parseChromeCurl parses a curl command as copied from Chrome (or any other
// browser) devtools' "Copy as cURL (bash)" action:
//
//	curl 'https://example.com/path' \
//	  -H 'Header-Name: value' \
//	  -H 'Other-Header: value' \
//	  --data-raw '{"key":"value"}'
//
// Only the subset of curl flags devtools actually emits is understood: -H /
// --header, -X / --request, --url, -b / --cookie, and the various --data*
// flags (all treated as an opaque request body — devtools always sends a
// single raw body, never multi-field form encoding). A handful of common
// no-op-for-us boolean flags (--compressed, -k, -L, ...) are recognized and
// ignored. Anything else is silently skipped rather than rejected, since
// devtools output is stable but curl itself has hundreds of flags we'll
// never actually see here.
//
// The URL can arrive two ways: as a bare positional argument (bash/macOS/
// Linux "Copy as cURL" puts it right after `curl`) or via an explicit --url
// flag (PowerShell's "Copy as cURL" does this, since PowerShell's own
// aliasing of `curl` makes a bare leading argument ambiguous). --url wins if
// both are somehow present.
func parseChromeCurl(raw string) (*rawRequest, error) {
	tokens, err := splitShellWords(raw)
	if err != nil {
		return nil, fmt.Errorf("tokenizing curl command: %w", err)
	}
	if len(tokens) == 0 || tokens[0] != "curl" {
		return nil, fmt.Errorf("input does not start with 'curl'")
	}
	tokens = tokens[1:]

	var (
		url         string
		explicitURL string
		method      string
		body        string
		cookies     []string
	)
	header := make(http.Header)

	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		switch tok {
		case "-H", "--header":
			i++
			if i >= len(tokens) {
				return nil, fmt.Errorf("%s flag missing value", tok)
			}
			name, value, ok := strings.Cut(tokens[i], ":")
			if !ok {
				return nil, fmt.Errorf("malformed header %q", tokens[i])
			}
			header.Add(strings.TrimSpace(name), strings.TrimSpace(value))

		case "-X", "--request":
			i++
			if i >= len(tokens) {
				return nil, fmt.Errorf("%s flag missing value", tok)
			}
			method = tokens[i]

		case "--url":
			i++
			if i >= len(tokens) {
				return nil, fmt.Errorf("%s flag missing value", tok)
			}
			explicitURL = tokens[i]

		case "-b", "--cookie":
			i++
			if i >= len(tokens) {
				return nil, fmt.Errorf("%s flag missing value", tok)
			}
			// curl accepts either "name=value" pairs or a path to a cookie
			// jar file here; devtools only ever emits the former, so that's
			// the only case we handle. A jar path (no '=') is skipped rather
			// than mistaken for a literal cookie.
			if strings.Contains(tokens[i], "=") {
				cookies = append(cookies, tokens[i])
			}

		case "-d", "--data", "--data-raw", "--data-binary", "--data-ascii", "--data-urlencode":
			i++
			if i >= len(tokens) {
				return nil, fmt.Errorf("%s flag missing value", tok)
			}
			body = tokens[i]

		case "--compressed", "-k", "--insecure", "-s", "--silent", "-L", "--location":
			// Boolean flags devtools/users commonly add; no effect on the
			// resulting request shape.

		default:
			if strings.HasPrefix(tok, "-") {
				// Unrecognized flag. devtools' own "Copy as cURL" doesn't
				// emit anything beyond what's handled above, so rather than
				// guess whether this one takes a value (and risk eating the
				// URL as its argument), just leave it alone.
				continue
			}
			if url == "" {
				url = tok
			}
		}
	}

	if explicitURL != "" {
		url = explicitURL
	}
	if url == "" {
		return nil, fmt.Errorf("no URL found in curl command")
	}
	if len(cookies) > 0 {
		// Multiple -b flags (or a -b alongside a Cookie: -H, which devtools
		// won't emit but a human-edited command might) all fold into one
		// header, same as curl itself does on the wire.
		header.Set("Cookie", strings.Join(append(header.Values("Cookie"), cookies...), "; "))
	}
	if method == "" {
		if body != "" {
			method = http.MethodPost
		} else {
			method = http.MethodGet
		}
	}
	// Same reasoning as the proxyman parser: a Content-Length copied from
	// devtools is stale the moment the body is touched, and Go's
	// http.Client sets it correctly from the body anyway.
	header.Del("Content-Length")

	return &rawRequest{
		method: method,
		url:    url,
		header: header,
		body:   body,
	}, nil
}

// splitShellWords tokenizes a command line the way a POSIX shell would when
// word-splitting a curl invocation: single quotes (literal, no escapes
// inside), double quotes (backslash escapes \" \\ \$ \`), bare backslash
// escapes outside quotes, and backslash-newline line continuations (dropped
// entirely, not turned into a space). It is deliberately not a general shell
// parser — no variable expansion, no command substitution, no glob
// expansion — just enough to tokenize the curl commands devtools produces.
func splitShellWords(s string) ([]string, error) {
	var (
		words []string
		cur   strings.Builder
		has   bool // cur holds part of a word, even if it's empty so far (e.g. '')
	)
	runes := []rune(s)
	n := len(runes)

	for i := 0; i < n; {
		c := runes[i]
		switch {
		case c == '\\' && i+1 < n && runes[i+1] == '\n':
			i += 2 // line continuation: consumed, contributes nothing

		case c == '\\' && i+1 < n:
			cur.WriteRune(runes[i+1])
			has = true
			i += 2

		case c == '\'':
			i++
			for i < n && runes[i] != '\'' {
				cur.WriteRune(runes[i])
				i++
			}
			if i >= n {
				return nil, fmt.Errorf("unterminated single quote")
			}
			i++ // skip closing quote
			has = true

		case c == '"':
			i++
			for i < n && runes[i] != '"' {
				if runes[i] == '\\' && i+1 < n && strings.ContainsRune(`"\$`+"`", runes[i+1]) {
					cur.WriteRune(runes[i+1])
					i += 2
					continue
				}
				cur.WriteRune(runes[i])
				i++
			}
			if i >= n {
				return nil, fmt.Errorf("unterminated double quote")
			}
			i++ // skip closing quote
			has = true

		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if has {
				words = append(words, cur.String())
				cur.Reset()
				has = false
			}
			i++

		default:
			cur.WriteRune(c)
			has = true
			i++
		}
	}
	if has {
		words = append(words, cur.String())
	}
	return words, nil
}
