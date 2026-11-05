package config

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"gopkg.in/yaml.v3"
)

// MaxEgressEntries bounds one tier's allowlist. Each entry is compared with
// every name a guest asks for, and the guests of a tier live under it for as
// long as it stands; a longer list is a list nobody is reading.
const MaxEgressEntries = 256

// TierNetwork is what a tier asks of the network its guests run on.
type TierNetwork struct {
	// Egress restricts where the tier's guests may connect. Nil is open, which
	// is what a tier always was.
	Egress *TierEgress `yaml:"egress,omitempty"`
}

// TierEgress is a tier's allowlist, enforced by the node on the host (ADR-027):
// a guest resolves only what it allows and connects only to what those names
// answered and the prefixes it names. Present means listed, so an empty Allow
// is GitHubRunnerEndpoints and the node's own services and nothing else.
type TierEgress struct {
	Allow []string `yaml:"allow" json:"allow"`
}

// UnmarshalYAML reads the block by hand, for two refusals a struct cannot make.
//
// `egress:` WITH NOTHING AFTER IT DECODES TO A NIL POINTER, which is open: the
// operator who wrote the key meant to restrict the tier, and a job running open
// under it is the one outcome the key exists to prevent.
//
// AND A CUSTOM DECODER LOSES THE PARSE'S KNOWN-FIELDS CHECK, so a misspelled
// `allowed:` would otherwise read as a list with nothing on it.
func (n *TierNetwork) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.AliasNode {
		value = value.Alias
	}

	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: network must be a mapping", value.Line)
	}

	for i := 0; i+1 < len(value.Content); i += 2 {
		key, val := value.Content[i], value.Content[i+1]
		if val.Kind == yaml.AliasNode {
			val = val.Alias
		}

		if key.Value != "egress" {
			return fmt.Errorf("line %d: network has no key %q", key.Line, key.Value)
		}

		if val.Kind != yaml.MappingNode {
			return fmt.Errorf("line %d: network.egress must be a mapping with an allow list, and "+
				"leaving it empty would not restrict the tier; write `allow: []` for only GitHub's "+
				"runner endpoints, or remove the key for an open tier", key.Line)
		}

		egress := &TierEgress{}
		if err := egress.decode(val); err != nil {
			return err
		}

		n.Egress = egress
	}

	return nil
}

func (e *TierEgress) decode(value *yaml.Node) error {
	for i := 0; i+1 < len(value.Content); i += 2 {
		key, val := value.Content[i], value.Content[i+1]
		if key.Value != "allow" {
			return fmt.Errorf("line %d: network.egress has no key %q", key.Line, key.Value)
		}

		if err := val.Decode(&e.Allow); err != nil {
			return fmt.Errorf("line %d: network.egress.allow must be a list of names and prefixes: %w",
				key.Line, err)
		}
	}

	return nil
}

// EgressEntry is one allowlist entry as written, parsed: a name, or an IPv4
// prefix when Prefix is valid.
type EgressEntry struct {
	// Name is lower case, with no terminal dot. With Wildcard it is the domain
	// the entry allows every name below, and not the domain itself.
	Name     string
	Wildcard bool
	Prefix   netip.Prefix
}

// ParseEgressEntry reads one allowlist entry: `example.com` allows that name
// alone, `*.example.com` every name below it and not the name itself, and
// `203.0.113.0/24` or `203.0.113.7` those addresses. A node parses what the wire
// sent it with this too, so an entry it cannot read refuses the launch.
//
// IPv6 IS REFUSED BY NAME: a listed guest has none (ADR-027), so an entry for it
// could only ever read as a promise.
func ParseEgressEntry(raw string) (EgressEntry, error) {
	if raw == "" {
		return EgressEntry{}, errors.New("an allowlist entry is empty")
	}

	if strings.Contains(raw, "/") {
		return parseEgressPrefix(raw)
	}

	if addr, err := netip.ParseAddr(raw); err == nil {
		if !addr.Is4() {
			return EgressEntry{}, egressIPv6Refused(raw)
		}

		return EgressEntry{Prefix: netip.PrefixFrom(addr, addr.BitLen())}, nil
	}

	wildcard := strings.HasPrefix(raw, "*.")
	name := strings.ToLower(strings.TrimPrefix(raw, "*."))

	// REFUSED RATHER THAN CANONICALIZED, for node.go's reason: an address the
	// operator wrote with a root dot says something they may have meant to
	// differ, and a name is compared here without one.
	if strings.HasSuffix(name, ".") {
		return EgressEntry{}, fmt.Errorf("allowlist entry %q ends in a dot; write it without", raw)
	}

	if !isHostname(name) {
		return EgressEntry{}, fmt.Errorf("allowlist entry %q is not a name, a `*.` and a name, or an "+
			"IPv4 address or prefix", raw)
	}

	labels := strings.Split(name, ".")

	// A LAST LABEL OF DIGITS IS AN ADDRESS MISTYPED, never a name DNS can answer:
	// `203.0.113` would otherwise read as one and allow nothing.
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return EgressEntry{}, fmt.Errorf("allowlist entry %q is not a whole address, and no name "+
			"ends in digits", raw)
	}

	if wildcard && len(labels) < 2 {
		return EgressEntry{}, fmt.Errorf("allowlist entry %q allows a whole top-level domain, which "+
			"is not a restriction anyone can read; name the domains under it", raw)
	}

	return EgressEntry{Name: name, Wildcard: wildcard}, nil
}

func parseEgressPrefix(raw string) (EgressEntry, error) {
	prefix, err := netip.ParsePrefix(raw)
	if err != nil {
		return EgressEntry{}, fmt.Errorf("allowlist entry %q is not an address prefix", raw)
	}

	if !prefix.Addr().Is4() {
		return EgressEntry{}, egressIPv6Refused(raw)
	}

	if prefix.Bits() == 0 {
		return EgressEntry{}, fmt.Errorf("allowlist entry %q allows every address, which is not a list; "+
			"remove network.egress for an open tier", raw)
	}

	if masked := prefix.Masked(); masked != prefix {
		return EgressEntry{}, fmt.Errorf("allowlist entry %q has host bits set; write %s", raw, masked)
	}

	return EgressEntry{Prefix: prefix}, nil
}

func egressIPv6Refused(raw string) error {
	return fmt.Errorf("allowlist entry %q is IPv6, and a listed tier's guests have no IPv6: "+
		"the host refuses every IPv6 packet they send", raw)
}

// GitHubRunnerEndpoints is what every allowlist includes, because a runner that
// cannot reach them cannot take a job at all, and a list that forgot one would
// read as a queue nobody can explain.
//
// GITHUB'S OWN LIST, read from its self-hosted runner reference on 2026-11-04:
// the domains it gives for essential operations, downloading actions, job
// summaries, logs, artifacts and caches, runner updates, OIDC tokens and release
// assets. It lists Packages, LFS, Dependabot and VNet too, which a job may not
// need, and those a tier names itself.
//
// *.blob.core.windows.net IS EVERY AZURE STORAGE ACCOUNT, and so a way out for a
// job that holds a key to one: GitHub keeps logs, artifacts and caches there, and
// names no narrower host. ADR-027 records the limit.
func GitHubRunnerEndpoints() []string {
	return []string{
		"github.com",
		"api.github.com",
		"codeload.github.com",
		"*.actions.githubusercontent.com",
		"*.blob.core.windows.net",
		"objects.githubusercontent.com",
		"objects-origin.githubusercontent.com",
		"github-releases.githubusercontent.com",
		"github-registry-files.githubusercontent.com",
		"release-assets.githubusercontent.com",
	}
}

// EgressErrors reports an allowlist this tier cannot have.
//
// ONLY FIRECRACKER ENFORCES ONE, on every provider a tier lists: placed on any
// other backend the job would run open, and a tier spanning several is placed on
// whichever has room. `billet check` and alloc.New read this too.
func (t Tier) EgressErrors(where string) []error {
	if t.Network == nil || t.Network.Egress == nil {
		return nil
	}

	var errs []error

	for _, p := range t.AcceptableProviders() {
		if p != ProviderFirecracker {
			errs = append(errs, fmt.Errorf("%s: network.egress is enforced on the host, on a guest's "+
				"tap, and provider %s has none, so a job placed there would run open; only "+
				"firecracker tiers may have one", where, p))
		}
	}

	allow := t.Network.Egress.Allow
	if len(allow) > MaxEgressEntries {
		errs = append(errs, fmt.Errorf("%s: network.egress.allow has %d entries, over the %d a tier may have",
			where, len(allow), MaxEgressEntries))

		return errs
	}

	for _, raw := range allow {
		if _, err := ParseEgressEntry(raw); err != nil {
			errs = append(errs, fmt.Errorf("%s: network.egress: %w", where, err))
		}
	}

	return errs
}

// EgressOf is the tier's allowlist, or nil for an open tier.
func (t Tier) EgressOf() *TierEgress {
	if t.Network == nil {
		return nil
	}

	return t.Network.Egress
}

// NeedsEgressAwareNode reports whether a launch of this tier needs a node that
// reads network.egress. An older node would ignore the block and run the job
// open, so one is refused it (nodeapi.VersionEgress).
func (t Tier) NeedsEgressAwareNode() bool { return t.EgressOf() != nil }
