package egress

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/junioryono/billet/internal/config"
)

// Policy is what one guest may do: open, only watched, or listed.
type Policy struct {
	listed bool
	// allow is the operator's entries as written, without the built-in ones, so a
	// restart compiles what the launch compiled under this binary's built-ins.
	allow    []string
	exact    map[string]struct{}
	below    []string
	prefixes []netip.Prefix
}

// Observe is the policy of a guest that may go anywhere and whose names are
// recorded.
func Observe() *Policy { return &Policy{} }

// Compile is the policy of a guest restricted to allow and
// config.GitHubRunnerEndpoints. An entry that cannot be read is an error, so a
// launch whose list arrived unreadable is refused and never runs open.
func Compile(allow []string) (*Policy, error) {
	p := &Policy{listed: true, allow: slices.Clone(allow), exact: map[string]struct{}{}}

	for _, raw := range append(config.GitHubRunnerEndpoints(), allow...) {
		entry, err := config.ParseEgressEntry(raw)
		if err != nil {
			return nil, fmt.Errorf("egress: %w", err)
		}

		switch {
		case entry.Prefix.IsValid():
			p.prefixes = append(p.prefixes, entry.Prefix)
		case entry.Wildcard:
			p.below = append(p.below, "."+entry.Name)
		default:
			p.exact[entry.Name] = struct{}{}
		}
	}

	return p, nil
}

// Listed reports whether the guest is restricted.
func (p *Policy) Listed() bool { return p != nil && p.listed }

// AllowsName reports whether a listed guest may resolve name, which is lower
// case with no terminal dot (see readQuestion). An open guest may resolve any.
func (p *Policy) AllowsName(name string) bool {
	if !p.Listed() {
		return true
	}

	if _, ok := p.exact[name]; ok {
		return true
	}

	for _, suffix := range p.below {
		if len(name) > len(suffix) && strings.HasSuffix(name, suffix) {
			return true
		}
	}

	return false
}
