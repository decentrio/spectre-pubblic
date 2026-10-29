// Package redact removes API keys from the text of errors that quote an RPC
// endpoint. It is a leaf package with no third-party imports, so the gRPC
// adapter can use it without taking on the Ethereum client.
package redact

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// redactedURL stands in for a URL that cannot be parsed, so there is no telling
// which part of it is a secret.
const redactedURL = "<redacted url>"

// schemeStart finds where an http, https, ws or wss URL begins in a text. It is
// not anchored to a word boundary: a URL glued to the word before it is still a
// URL, and leaving it in the text would leave its key there too.
var schemeStart = regexp.MustCompile(`(?i)(?:https?|wss?)://`)

// trailing is what can follow an unquoted URL: the punctuation of the sentence
// it sits in, or the bracket that closes it. It is not read as part of the URL
// and is written back as it was. A closing apostrophe is told apart from one
// that ends the URL by what comes before the URL: see urlEnd.
const trailing = ".,;:)]}>"

// URL reduces a URL to its scheme and host, dropping userinfo, path,
// query and fragment: that is where RPC providers put the API key
// (https://rpc.example/v2/KEY, wss://rpc.example/ws?apikey=KEY). A filesystem
// path (an IPC socket) carries no key and is returned as is.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return redactedURL
	}
	switch {
	case u.Host != "":
		return u.Scheme + "://" + u.Host
	case u.Scheme != "":
		return u.Scheme + "://"
	default:
		return raw
	}
}

// URLs applies URL to every http, https, ws or wss URL inside s.
//
// Where a URL ends is the whole difficulty, because nearly any character can be
// in one: an apostrophe, a parenthesis, a semicolon, a comma and a bracket are
// all legal in a path or a query, and a scan that stops at one leaves the rest of
// the URL, and its key, in the text. So a URL is read to its end by the context
// it is written in:
//
//   - Inside a double-quoted string, as fmt's %q and JSON write it, it runs to
//     the closing quote, and a backslash escapes the character after it. This is
//     how net/http formats a failed request: `Post "<full URL>": <cause>`.
//   - Anywhere else it runs to the next white space. Quotes and brackets are not
//     taken as its end, so a URL that holds them is not cut short; only
//     punctuation at its very end is given back (see trailing), and an
//     apostrophe only if one opened the URL.
//
// The second rule can swallow text after a URL that is not separated from it by
// white space, which hides more than it needs to, and never less.
func URLs(s string) string {
	starts := schemeStart.FindAllStringIndex(s, -1)
	if len(starts) == 0 {
		return s
	}
	var out strings.Builder
	copied := 0
	for _, loc := range starts {
		start := loc[0]
		if start < copied {
			continue // inside a URL that was already replaced
		}
		end, quoted := urlEnd(s, start)
		out.WriteString(s[copied:start])
		out.WriteString(redactSpan(s[start:end], quoted))
		copied = end
	}
	out.WriteString(s[copied:])
	return out.String()
}

// urlEnd returns where the URL that begins at start ends, and whether it was
// inside a double-quoted string.
func urlEnd(s string, start int) (end int, quoted bool) {
	if start > 0 && s[start-1] == '"' {
		for i := start; i < len(s); i++ {
			switch s[i] {
			case '\\':
				i++ // the next character is escaped, a quote included
			case '"':
				return i, true
			}
		}
		return len(s), true // never closed: hide the rest
	}
	end = start
	for end < len(s) {
		r, size := utf8.DecodeRuneInString(s[end:])
		if unicode.IsSpace(r) {
			break
		}
		end += size
	}
	closesApostrophe := start > 0 && s[start-1] == '\''
	for end > start {
		last := s[end-1]
		if strings.IndexByte(trailing, last) < 0 && !(last == '\'' && closesApostrophe) {
			break
		}
		end--
	}
	return end, false
}

// redactSpan redacts the URL in span. Inside a double-quoted string the URL is
// written escaped, so it is read unescaped, redacted, and written back escaped.
func redactSpan(span string, quoted bool) string {
	if quoted {
		if unescaped, err := strconv.Unquote(`"` + span + `"`); err == nil {
			escaped := strconv.Quote(URL(unescaped))
			return escaped[1 : len(escaped)-1]
		}
	}
	return URL(span)
}

// redactedError is an error whose text has its URLs redacted while the
// original stays reachable through Unwrap, so errors.Is and errors.As work.
type redactedError struct {
	text string
	err  error
}

func (e *redactedError) Error() string { return e.text }
func (e *redactedError) Unwrap() error { return e.err }

// Error returns err with the URLs in its text redacted, for an error that is
// about to be logged or returned from startup. It is nil for nil.
//
// An error from net/http or url.Parse is a *url.Error, which holds the exact URL
// it quotes. That URL is replaced in the text as it is, so it is redacted
// whatever characters it holds, without reading it back out of the text. The
// rest of the text is then scanned for URLs, which catches one that reached it
// some other way.
func Error(err error) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	for _, raw := range quotedURLs(err) {
		text = replaceURL(text, raw)
	}
	return &redactedError{text: URLs(text), err: err}
}

// maxDepth bounds the walk over wrapped errors, so a chain that wraps itself
// cannot loop.
const maxDepth = 64

// quotedURLs returns the URL of every *url.Error in err's tree.
func quotedURLs(err error) []string {
	var found []string
	var walk func(error, int)
	walk = func(err error, depth int) {
		if err == nil || depth > maxDepth {
			return
		}
		if urlErr, ok := err.(*url.Error); ok && urlErr.URL != "" {
			found = append(found, urlErr.URL)
		}
		switch wrapped := err.(type) {
		case interface{ Unwrap() error }:
			walk(wrapped.Unwrap(), depth+1)
		case interface{ Unwrap() []error }:
			for _, child := range wrapped.Unwrap() {
				walk(child, depth+1)
			}
		}
	}
	walk(err, 0)
	return found
}

// replaceURL replaces raw in text with its redacted form, both as it is written
// by fmt's %q (escaped) and as it is.
func replaceURL(text, raw string) string {
	redacted := URL(raw)
	if redacted == raw {
		return text
	}
	quoted, redactedQuoted := strconv.Quote(raw), strconv.Quote(redacted)
	text = strings.ReplaceAll(text, quoted[1:len(quoted)-1], redactedQuoted[1:len(redactedQuoted)-1])
	return strings.ReplaceAll(text, raw, redacted)
}
