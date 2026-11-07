// Package alerts decides which jobs a rule of `server.alerts` is about and delivers
// what it says to a generic JSON webhook or a Slack incoming webhook.
//
// IT WRITES NO LEDGER. The control plane's loop (internal/server/alerts.go) reads
// jobs and records decisions through the allocator, and this package supplies what
// is decided about a job and how a notice reaches its destination, so a slow
// destination can hold no transaction and a test of a rule needs no database.
package alerts

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strings"
)

// URL is a destination's address, which is its credential: a Slack incoming webhook
// authenticates by the path, so whoever holds the URL can post to the channel.
//
// ITS OWN TYPE, NOT A string, so every rendering path redacts it, as state.DSN does.
// Each of the five methods covers a path the others do not, and they are on VALUE
// receivers so a URL reached through a field is covered too. The one reader that
// needs the text, the sender, asks for it with string(u).
type URL string

const redactedURL = "[redacted alert destination url]"

// String redacts the address.
func (URL) String() string { return redactedURL }

// GoString covers %#v, which does not consult String.
func (u URL) GoString() string { return u.String() }

// Format makes every verb safe, not only the ones fmt.Stringer covers.
func (u URL) Format(s fmt.State, _ rune) {
	//nolint:errcheck // fmt.State has no error channel; a failed write to it is the caller's output problem.
	io.WriteString(s, u.String())
}

// MarshalJSON keeps the address out of anything that serializes one.
func (u URL) MarshalJSON() ([]byte, error) {
	out, err := json.Marshal(u.String())
	if err != nil {
		return nil, fmt.Errorf("alerts: marshal a redacted url: %w", err)
	}

	return out, nil
}

// LogValue is what slog asks for before falling back to reflection.
func (u URL) LogValue() slog.Value { return slog.StringValue(u.String()) }

// ParseURL checks an address read from the environment or a file. A signed request
// is not what protects it: the address is the secret, so plain http would send it
// past everything on the way, and only a literal loopback host may have it.
//
// NOTHING OF THE VALUE IS QUOTED, in any refusal: url.Parse's own error quotes what
// it could not parse, and a value that fails here may be a credential with a typo.
func ParseURL(raw string) (URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("the address is empty")
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("the address is not a URL")
	}

	switch {
	case parsed.Host == "":
		return "", errors.New("the address names no host")
	case parsed.User != nil:
		return "", errors.New("the address carries a user name or password")
	case parsed.Fragment != "":
		return "", errors.New("the address carries a fragment, which is never sent")
	case parsed.Scheme == "https":
		return URL(raw), nil
	case parsed.Scheme == "http" && isLoopback(parsed.Hostname()):
		return URL(raw), nil
	case parsed.Scheme == "http":
		return "", errors.New("the address is plain http to another host, which would send the credential in the clear")
	default:
		return "", errors.New("the address is neither https nor http to a loopback host")
	}
}

// isLoopback is a literal loopback address. A name is not one, `localhost` included:
// validation and the dial would resolve it separately.
func isLoopback(host string) bool {
	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}
