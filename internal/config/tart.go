package config

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// TartConfig is the Apple Silicon backend's node settings.
type TartConfig struct {
	// UntrustedIsolation names the mechanism that confines a fork pull
	// request's guest, and its ABSENCE is what refuses one.
	//
	// The same rule, and the same reasoning, as
	// node.firecracker.untrusted_bridge: a tart VM is a real kernel boundary,
	// and the NETWORK is not one. Tart's default is shared NAT, where a guest
	// reaches the host and can ARP-spoof the vmnet bridge to read another
	// guest's traffic — so untrusted work runs only once its confinement has
	// been described, rather than landing on the default because nobody said
	// otherwise.
	//
	// STATED BY THE OPERATOR RATHER THAN DETECTED, because billet cannot prove
	// this from the host. What it can see of softnet is two metadata bits — a
	// setuid bit and an owner of root — which say the helper could start, not
	// what its policy then permits. Naming it here is the operator asserting
	// the mechanism is the one they want, exactly as naming a bridge is.
	UntrustedIsolation TartIsolation `yaml:"untrusted_isolation,omitempty"`

	// UntrustedDNS are the resolvers an isolated guest is given, and billet
	// gives them because billet is what took the working one away.
	//
	// MEASURED: under softnet a guest's DHCP-assigned resolver is the vmnet
	// gateway, which sits in the private address space softnet blocks. Egress
	// to public addresses keeps working and TCP/443 keeps working, so nothing
	// looks wrong — every job simply fails to resolve github.com. Public
	// resolvers are reachable under exactly the policy that broke the gateway
	// one.
	UntrustedDNS []string `yaml:"untrusted_dns,omitempty"`
}

// Normalize trims the block and fills the resolver default.
//
// EXPORTED FOR THE SAME REASON CheckFirecracker AND CheckTart ARE: the
// provider's constructor cannot assume its configuration came through Load, and
// a value trimmed for the CHECK while the caller launches with the raw one is
// the defect the ec2 and ceph blocks each shipped with once.
func (t *TartConfig) Normalize() {
	if t == nil {
		return
	}

	t.UntrustedIsolation = TartIsolation(strings.TrimSpace(string(t.UntrustedIsolation)))

	for i := range t.UntrustedDNS {
		t.UntrustedDNS[i] = strings.TrimSpace(t.UntrustedDNS[i])
	}

	// ONLY WHERE IT IS READ. Defaulting resolvers on a node that never isolates
	// a guest would put two addresses in an operator's rendered config that
	// nothing consults, which is the "looks configured, is inert" shape billet
	// refuses elsewhere.
	if t.UntrustedIsolation != "" && len(t.UntrustedDNS) == 0 {
		t.UntrustedDNS = DefaultUntrustedDNS()
	}
}

// TartIsolation is a confinement mechanism for untrusted guests.
type TartIsolation string

// IsolationSoftnet is tart's own userspace packet filter, which restricts a
// guest to public destinations and isolates guests from each other on the
// bridge. It is the only mechanism billet drives today; the type exists so a
// second one is a new value rather than a new meaning for a boolean.
const IsolationSoftnet TartIsolation = "softnet"

// DefaultUntrustedDNS is what an isolated guest resolves through when the
// operator names no resolver: Cloudflare and Google, both public, both
// reachable under the policy that blocks the gateway resolver.
//
// Two of them, from different operators, because a runner that cannot resolve
// fails every job on the host and the second one costs nothing.
func DefaultUntrustedDNS() []string { return []string{"1.1.1.1", "8.8.8.8"} }

// validateTartNode checks the Apple Silicon block, and refuses it on any other
// backend for the reason node.firecracker and node.ceph are refused: nothing
// else reads it, so it would describe an isolation policy no launch consults.
func (c *Config) validateTartNode() []error {
	if c.Node.Tart == nil {
		return nil
	}

	if c.Node.Provider != ProviderTart {
		return []error{fmt.Errorf("node.tart is set but this node's provider is %s, and only "+
			"tart reads it, so this host would describe an isolation policy that no launch "+
			"consults", c.Node.Provider)}
	}

	return CheckTart(*c.Node.Tart)
}

// CheckTart validates the Apple Silicon block.
//
// EXPORTED for the reason alloc.New re-applies config's rules: a caller that
// built a TartConfig in code never passed through Load, and a rule enforced in
// only one of the two entry points is not enforced.
func CheckTart(t TartConfig) []error {
	var errs []error

	// A MISSPELLING MUST NOT READ AS "NO ISOLATION". Both are absent as far as
	// a struct field is concerned, and the two mean opposite things: one is an
	// operator who decided not to run untrusted work, the other is an operator
	// who thought they had configured it. Only the empty string is the former.
	switch t.UntrustedIsolation {
	case "", IsolationSoftnet:
	default:
		errs = append(errs, fmt.Errorf("node.tart.untrusted_isolation is %q, and the only "+
			"mechanism billet drives is %q (tart's own userspace packet filter, which confines a "+
			"guest to public destinations and isolates guests from each other)",
			t.UntrustedIsolation, IsolationSoftnet))
	}

	if len(t.UntrustedDNS) > 0 && t.UntrustedIsolation == "" {
		errs = append(errs, errors.New("node.tart.untrusted_dns is set but "+
			"node.tart.untrusted_isolation is not, so no guest is ever isolated and nothing "+
			"reads these resolvers"))
	}

	for _, resolver := range t.UntrustedDNS {
		// AN ADDRESS, NOT A NAME. This is what a guest resolves THROUGH, so a
		// hostname here could only be resolved by the resolver it is meant to
		// configure. netip.ParseAddr refuses a port, a CIDR and a name alike.
		addr, err := netip.ParseAddr(resolver)
		if err != nil {
			errs = append(errs, fmt.Errorf("node.tart.untrusted_dns %q is not an IP address: "+
				"a guest with no working resolver cannot look one up, so this must be literal",
				resolver))

			continue
		}

		// AND NOT A ZONE, which is the part that made "it parsed as an address"
		// a weaker statement than it looks. ParseAddr accepts an IPv6 zone and
		// places NO restriction on its contents, so
		// `2001:4860:4860::8888%x;touch${IFS}/tmp/pwn` parses cleanly — and this
		// value is written into a shell script that runs inside the guest. A
		// zone is a link-local scope name for the host it is used on; it is
		// meaningless as a resolver a guest was handed, so refusing it costs
		// nothing and removes the class.
		if addr.Zone() != "" {
			errs = append(errs, fmt.Errorf("node.tart.untrusted_dns %q carries an IPv6 zone "+
				"(%q); a zone names a link on the machine that uses it, so it cannot describe "+
				"a resolver for a guest, and billet refuses it rather than passing it to one",
				resolver, addr.Zone()))
		}
	}

	return errs
}
