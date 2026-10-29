package redact

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"strings"
	"testing"
)

func TestURLKeepsOnlySchemeAndHost(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"key in the path", "https://eth-mainnet.example/v2/SECRET-KEY", "https://eth-mainnet.example"},
		{"key in the query", "http://127.0.0.1:8545/rpc?apikey=SECRET-KEY", "http://127.0.0.1:8545"},
		{"key in userinfo", "https://user:SECRET-KEY@rpc.example:8443/", "https://rpc.example:8443"},
		{"websocket with fragment", "wss://rpc.example/ws/SECRET-KEY#frag", "wss://rpc.example"},
		{"ipv6 host", "ws://[::1]:8546/SECRET-KEY", "ws://[::1]:8546"},
		{"nothing to hide", "https://rpc.example", "https://rpc.example"},
		{"ipc socket path is not a secret", "/var/run/geth.ipc", "/var/run/geth.ipc"},
		{"unparsable", "http://[::1/SECRET-KEY", "<redacted url>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := URL(tc.raw)
			if got != tc.want {
				t.Fatalf("URL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			if strings.Contains(got, "SECRET-KEY") {
				t.Fatalf("URL(%q) = %q still carries the key", tc.raw, got)
			}
		})
	}
}

func TestURLsRewritesURLsInsideMessages(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{
			"net/http transport failure",
			`Post "http://127.0.0.1:9545/rpc?apikey=SECRET-KEY": EOF`,
			`Post "http://127.0.0.1:9545": EOF`,
		},
		{
			"url followed by a colon",
			"eth rpc https://rpc.example/v2/SECRET-KEY: parse url failed",
			"eth rpc https://rpc.example: parse url failed",
		},
		{
			"two urls",
			"dial wss://a.example/SECRET-KEY, fell back to https://b.example/SECRET-KEY.",
			"dial wss://a.example, fell back to https://b.example.",
		},
		{
			"upper case scheme",
			"HTTPS://RPC.example/SECRET-KEY failed",
			"https://RPC.example failed",
		},
		{
			"unparsable url",
			`parse "http://[::1/SECRET-KEY": missing ']'`,
			`parse "<redacted url>": missing ']'`,
		},
		{"no url", "context deadline exceeded", "context deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := URLs(tc.in)
			if got != tc.want {
				t.Fatalf("URLs(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "SECRET-KEY") {
				t.Fatalf("URLs(%q) = %q still carries the key", tc.in, got)
			}
		})
	}
}

// The failure the audit reproduced: net/http strips only the password of the
// userinfo, so a key in the path or query survives into the error text.
func TestErrorHidesTheKeyOfARealTransportError(t *testing.T) {
	transport := &url.Error{Op: "Post", URL: "http://127.0.0.1:9545/rpc?apikey=SECRET-KEY", Err: io.EOF}
	wrapped := fmt.Errorf("optimism_syncStatus failed: %w", transport)
	if !strings.Contains(wrapped.Error(), "SECRET-KEY") {
		t.Fatalf("premise: the unredacted transport error %q should carry the key", wrapped)
	}

	redacted := Error(wrapped)
	if strings.Contains(redacted.Error(), "SECRET-KEY") {
		t.Fatalf("redacted error %q still carries the key", redacted)
	}
	if want := `optimism_syncStatus failed: Post "http://127.0.0.1:9545": EOF`; redacted.Error() != want {
		t.Fatalf("redacted error = %q, want %q", redacted, want)
	}
	// Redaction changes the text, not the identity of the error.
	if !errors.Is(redacted, io.EOF) {
		t.Fatal("Error broke errors.Is")
	}
	var asURL *url.Error
	if !errors.As(redacted, &asURL) || asURL != transport {
		t.Fatal("Error broke errors.As")
	}
}

func TestErrorOfNilIsNil(t *testing.T) {
	if err := Error(nil); err != nil {
		t.Fatalf("Error(nil) = %v", err)
	}
}

// Where a URL ends cannot be read off its characters: an apostrophe, a
// parenthesis, a semicolon, a comma and a bracket are all legal in a path or a
// query. A scan that stopped at one of them left the rest of the URL, and the
// key that was in it, in the text.
func TestURLsReadsAURLToItsEnd(t *testing.T) {
	const withApostrophes = "https://rpc.example/rpc?label='node'&apikey=SECRET-KEY"
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{
			"apostrophes, quoted by %q as net/http does",
			`Post "` + withApostrophes + `": EOF`,
			`Post "https://rpc.example": EOF`,
		},
		{
			"apostrophes, unquoted",
			"dial " + withApostrophes + " failed",
			"dial https://rpc.example failed",
		},
		{
			"apostrophes, between apostrophes",
			"dial '" + withApostrophes + "' failed",
			"dial 'https://rpc.example' failed",
		},
		{
			"the key last, between apostrophes",
			"dial 'https://rpc.example/rpc?apikey=SECRET-KEY' failed",
			"dial 'https://rpc.example' failed",
		},
		{
			"escaped quotes inside a %q string",
			`Post "https://rpc.example/rpc?q=\"a\"&apikey=SECRET-KEY": EOF`,
			`Post "https://rpc.example": EOF`,
		},
		{
			"an escaped backslash before the quote",
			`Post "https://rpc.example/rpc?p=a\\&apikey=SECRET-KEY": EOF`,
			`Post "https://rpc.example": EOF`,
		},
		{
			"white space inside a %q string",
			`Post "https://rpc.example/a b?apikey=SECRET-KEY": EOF`,
			`Post "https://rpc.example": EOF`,
		},
		{
			"json string",
			`{"url":"https://rpc.example/rpc?a=1\u0026label='x'\u0026apikey=SECRET-KEY","n":1}`,
			`{"url":"https://rpc.example","n":1}`,
		},
		{
			"a %q string that is never closed",
			`Post "https://rpc.example/rpc?label='x'&apikey=SECRET-KEY`,
			`Post "https://rpc.example`,
		},
		{
			"parentheses and angle brackets",
			"(" + withApostrophes + ") and <" + withApostrophes + ">",
			"(https://rpc.example) and <https://rpc.example>",
		},
		{
			"glued to the word before it",
			"endpoint=" + withApostrophes + " down; node" + withApostrophes,
			"endpoint=https://rpc.example down; nodehttps://rpc.example",
		},
		{
			"two urls in one %q string are hidden together",
			`Post "https://a.example/x?k=1,https://b.example/y?apikey=SECRET-KEY": EOF`,
			`Post "https://a.example": EOF`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := URLs(tc.in)
			if got != tc.want {
				t.Fatalf("URLs(%q)\n got  %q\n want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "SECRET-KEY") {
				t.Fatalf("URLs(%q) = %q still carries the key", tc.in, got)
			}
			if again := URLs(got); again != got {
				t.Fatalf("URLs is not idempotent: %q became %q", got, again)
			}
		})
	}
}

// A *url.Error holds the exact URL it quotes, so it is redacted as that URL and
// not read back out of the text. This holds whatever the URL contains, including
// the characters a text scan cannot tell from the end of one.
func TestErrorRedactsAURLErrorWhateverItsCharacters(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		want string // what the text names of the endpoint
	}{
		{"apostrophes", "https://rpc.example/rpc?label='node'&apikey=SECRET-KEY", "https://rpc.example"},
		{"double quotes", `https://rpc.example/rpc?label="node"&apikey=SECRET-KEY`, "https://rpc.example"},
		{"a backslash", `https://rpc.example/rpc?p=a\b&apikey=SECRET-KEY`, "https://rpc.example"},
		{"angle brackets", "https://rpc.example/rpc?a=<b>&apikey=SECRET-KEY", "https://rpc.example"},
		{"white space", "https://rpc.example/rpc?a=b c&apikey=SECRET-KEY", "https://rpc.example"},
		{"non-ASCII", "https://rpc.example/rpc?name=ノード&apikey=SECRET-KEY", "https://rpc.example"},
		{"userinfo", "https://user:SECRET-KEY@rpc.example:8443/rpc", "https://rpc.example:8443"},
		{"unparsable", "https://rpc.example/%zz?apikey=SECRET-KEY", redactedURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &url.Error{Op: "Post", URL: tc.url, Err: io.EOF}
			for wrapperName, err := range map[string]error{
				"itself":      transport,
				"wrapped":     fmt.Errorf("optimism_syncStatus failed: %w", transport),
				"joined":      errors.Join(errors.New("first"), fmt.Errorf("second: %w", transport)),
				"two wrapped": fmt.Errorf("%w; %w", errors.New("other"), fmt.Errorf("inner: %w", transport)),
			} {
				got := Error(err).Error()
				if strings.Contains(got, "SECRET-KEY") {
					t.Errorf("%s: redacted error %q still carries the key", wrapperName, got)
				}
				if !strings.Contains(got, tc.want) {
					t.Errorf("%s: redacted error %q does not name the endpoint %q", wrapperName, got, tc.want)
				}
				if !errors.Is(Error(err), io.EOF) {
					t.Errorf("%s: Error broke errors.Is", wrapperName)
				}
			}
		})
	}
}

// Error also reads the text for URLs that reached it some other way than a
// *url.Error.
func TestErrorRedactsAURLThatIsOnlyInTheText(t *testing.T) {
	err := fmt.Errorf("provider said no to https://rpc.example/v2/SECRET-KEY?label='x' (HTTP 403)")
	got := Error(err).Error()
	if want := "provider said no to https://rpc.example (HTTP 403)"; got != want {
		t.Fatalf("Error = %q, want %q", got, want)
	}
}

// The key is dropped whichever way the URL is written into a message, and
// whatever characters it holds. This builds URLs from a hostile alphabet, puts
// a distinctive key after the hostile part, and writes each into the messages a
// real error carries it in. Contexts where white space would end the URL, or
// where the writer does not escape quotes, use an alphabet without them: no
// reader can tell where such a URL ends.
func TestNoContextLeaksTheKey(t *testing.T) {
	const (
		anyChar   = "'\"\\<>()[]{};,!*+$&=%/?:@~-._#|^` aAzZ09"
		noSpace   = "'\"\\<>()[]{};,!*+$&=%/?:@~-._#|^`aAzZ09"
		hosts     = 3
		iteration = 4000
	)
	hostNames := [hosts]string{"rpc.example", "rpc.example:8545", "[::1]:8546"}
	random := rand.New(rand.NewSource(574))
	junk := func(alphabet string) string {
		n := random.Intn(9)
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[random.Intn(len(alphabet))]
		}
		return string(b)
	}
	build := func(alphabet, key string) string {
		return fmt.Sprintf("%s://%s/rpc%s?label=%s&apikey=%s%s",
			[]string{"http", "https", "ws", "wss"}[random.Intn(4)], hostNames[random.Intn(hosts)],
			junk(alphabet), junk(alphabet), key, junk(alphabet))
	}

	type context struct {
		name     string
		alphabet string
		render   func(u string) (string, error) // the text, and the error to redact if it is not plain text
	}
	contexts := []context{
		{"url.Error wrapped", anyChar, nil},
		{"%q string", anyChar, func(u string) (string, error) { return fmt.Sprintf("Post %q: EOF", u), nil }},
		{"json string", anyChar, func(u string) (string, error) {
			b, err := json.Marshal(map[string]string{"url": u, "n": "1"})
			return string(b), err
		}},
		{"unquoted", noSpace, func(u string) (string, error) { return "dial " + u + " failed", nil }},
		{"unquoted, then a comma", noSpace, func(u string) (string, error) { return "dial " + u + ", retrying.", nil }},
		{"between apostrophes", noSpace, func(u string) (string, error) { return "dial '" + u + "' failed", nil }},
		{"in parentheses", noSpace, func(u string) (string, error) { return "dial (" + u + ")", nil }},
		{"in angle brackets", noSpace, func(u string) (string, error) { return "dial <" + u + ">", nil }},
		{"after an equals sign", noSpace, func(u string) (string, error) { return "endpoint=" + u, nil }},
	}
	for _, c := range contexts {
		t.Run(c.name, func(t *testing.T) {
			for i := 0; i < iteration; i++ {
				key := fmt.Sprintf("SECRET%08x", random.Uint32())
				u := build(c.alphabet, key)
				var got string
				if c.render == nil {
					got = Error(fmt.Errorf("op failed: %w", &url.Error{Op: "Post", URL: u, Err: io.EOF})).Error()
				} else {
					text, err := c.render(u)
					if err != nil {
						t.Fatal(err)
					}
					got = URLs(text)
				}
				if strings.Contains(got, key) {
					t.Fatalf("the key survived\n url:    %q\n output: %q", u, got)
				}
				if again := URLs(got); again != got {
					t.Fatalf("URLs is not idempotent\n output: %q\n again:  %q", got, again)
				}
			}
		})
	}
}
