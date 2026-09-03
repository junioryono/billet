package config

import (
	"strings"
	"testing"
)

// validTartConfig is a one-Mac node, which is the shape node.tart belongs to.
const validTartConfig = `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  max_vcpu: 8
  max_memory: 16GiB
github:
  org: acme
  app_id: 12345
  installation_id: 67890
  private_key_path: /etc/billet/app.pem
node:
  name: mac-mini-1
  server_addr: 127.0.0.1:7717
  provider: tart
  state_dir: /var/lib/billet/node
  max_vcpu: 8
  max_memory: 16GiB
tiers:
  - label: billet-4vcpu-macos-26
    provider: tart
    guest_os: macos
    node: mac-mini-1
    vcpu: 4
    memory: 8GiB
    image: ghcr.io/cirruslabs/macos-tahoe-xcode:latest
`

func loadTart(t *testing.T, body string) (*Config, error) {
	t.Helper()

	return Load(writeConfig(t, body))
}

// THE BLOCK IS REFUSED WHERE NOTHING READS IT, exactly as node.firecracker and
// node.ceph are. The shape that produces this is switching a node's provider and
// leaving the old block behind — which is precisely when a silent acceptance is
// most expensive, because the operator believes untrusted work is confined.
func TestTartBlockIsRefusedOnAnotherProvider(t *testing.T) {
	body := strings.Replace(validConfig, "  ceph:\n",
		"  tart:\n    untrusted_isolation: softnet\n  ceph:\n", 1)

	_, err := loadTart(t, body)
	if err == nil {
		t.Fatal("node.tart was accepted on a firecracker node, where nothing reads it")
	}

	if !strings.Contains(err.Error(), "only tart reads it") {
		t.Errorf("Load = %v, want the refusal to say why the block is inert", err)
	}
}

// A MISSPELLING MUST NOT READ AS "NO ISOLATION". Both are absent as far as a
// struct field is concerned, and they mean opposite things: one is an operator
// who decided not to run untrusted work, the other is an operator who believes
// they configured it.
func TestAnUnknownIsolationMechanismIsRefused(t *testing.T) {
	body := strings.Replace(validTartConfig, "  state_dir: /var/lib/billet/node",
		"  state_dir: /var/lib/billet/node\n  tart:\n    untrusted_isolation: softnett", 1)

	_, err := loadTart(t, body)
	if err == nil {
		t.Fatal("a misspelled isolation mechanism was accepted, and would read as no isolation")
	}

	if !strings.Contains(err.Error(), "softnet") {
		t.Errorf("Load = %v, want the mechanism billet does drive named", err)
	}
}

// RESOLVERS WITH NOTHING TO CONFIGURE are a block that looks configured and is
// inert — the same failure the refusals above exist for, one level down.
func TestResolversWithoutIsolationAreRefused(t *testing.T) {
	body := strings.Replace(validTartConfig, "  state_dir: /var/lib/billet/node",
		"  state_dir: /var/lib/billet/node\n  tart:\n    untrusted_dns: [1.1.1.1]", 1)

	_, err := loadTart(t, body)
	if err == nil {
		t.Fatal("resolvers were accepted on a node that never isolates a guest")
	}

	if !strings.Contains(err.Error(), "untrusted_isolation") {
		t.Errorf("Load = %v, want the missing setting named", err)
	}
}

// AN ADDRESS, NOT A NAME. This is what a guest resolves THROUGH, so a hostname
// here could only be resolved by the resolver it is meant to configure — and the
// value is interpolated into a shell script that runs inside the guest, which is
// what makes "it parses as an IP address" a safety rule rather than tidiness.
func TestAResolverThatIsNotAnAddressIsRefused(t *testing.T) {
	for _, bad := range []string{
		"dns.example.com", "1.1.1.1:53", "1.1.1.0/24", "1.1.1.1; id",
		// THE ONE THAT PARSED. netip.ParseAddr accepts an IPv6 zone and places
		// no restriction on its contents, so this is a valid address as far as
		// the parser is concerned — and it was written straight into a shell
		// script that runs with sudo inside the guest. Found by review, after
		// this file's own comment claimed the parse was what made it safe.
		"2001:db8::1%x;touch /tmp/pwned",
		"fe80::1%' ; id ; echo '",
	} {
		body := strings.Replace(validTartConfig, "  state_dir: /var/lib/billet/node",
			"  state_dir: /var/lib/billet/node\n  tart:\n    untrusted_isolation: softnet\n"+
				"    untrusted_dns: ['"+bad+"']", 1)

		if _, err := loadTart(t, body); err == nil {
			t.Errorf("untrusted_dns %q was accepted; it reaches a shell inside the guest", bad)
		}
	}
}

// THE DEFAULT IS FILLED ONLY WHERE IT IS READ. Two addresses in a rendered
// config that nothing consults is the "looks configured, is inert" shape again.
func TestResolversDefaultOnlyWhenIsolationIsOn(t *testing.T) {
	withIsolation := strings.Replace(validTartConfig, "  state_dir: /var/lib/billet/node",
		"  state_dir: /var/lib/billet/node\n  tart:\n    untrusted_isolation: softnet", 1)

	cfg, err := loadTart(t, withIsolation)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Node.Tart == nil || len(cfg.Node.Tart.UntrustedDNS) == 0 {
		t.Fatal("an isolating node got no resolvers, so its guests cannot resolve anything")
	}

	// And a node with no isolation gets none, rather than two it never uses.
	bare, err := loadTart(t, validTartConfig)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if bare.Node.Tart != nil && len(bare.Node.Tart.UntrustedDNS) > 0 {
		t.Errorf("a node that never isolates a guest was given resolvers: %v",
			bare.Node.Tart.UntrustedDNS)
	}
}

// APPLE'S HYPERVISOR HAS A FLOOR, AND A TIER UNDER IT IS A CONFIG ERROR.
//
// Measured on the reference Mac: Virtualization.framework refuses a macOS VM
// below 4GiB with LessThanMinimalResourcesError. Without this the tier loads,
// the node registers, the label advertises capacity, and every job on it dies at
// the point where the least is known about why.
func TestAMacOSTierUnderApplesFloorIsRefused(t *testing.T) {
	body := strings.Replace(validTartConfig, "memory: 8GiB", "memory: 2GiB", 1)

	_, err := loadTart(t, body)
	if err == nil {
		t.Fatal("a 2GiB macOS tier loaded; Apple's hypervisor would refuse to start it")
	}

	// The DIAGNOSTIC, not merely an error: an operator has to learn the floor is
	// Apple's rather than billet's, or the obvious next move is to look for the
	// billet setting that lowered it.
	for _, want := range []string{"guest_os macos", MinMacOSGuestMemory.String(), "2GiB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load = %v, want the refusal to mention %q", err, want)
		}
	}
}

// AND THE OTHER DIRECTION, which is the half a single-sided test would miss.
//
// The floor is a property of a macOS GUEST, not of the tart backend: an arm64
// Linux guest of the same size boots fine, so capping it would refuse a correct
// deployment. Same shape as the pair guarding the macOS licence limit, and for
// the same reason — the original limit keyed off a label and was wrong in both
// directions at once.
func TestALinuxTartTierIsNotHeldToApplesMacOSFloor(t *testing.T) {
	body := strings.Replace(validTartConfig, "memory: 8GiB", "memory: 2GiB", 1)
	body = strings.Replace(body, "guest_os: macos", "guest_os: linux", 1)

	if _, err := loadTart(t, body); err != nil {
		t.Fatalf("a 2GiB arm64 Linux guest was refused Apple's macOS floor: %v", err)
	}
}

// A macOS tier EXACTLY at the floor is accepted, because the hypervisor accepts
// it — the refusal above must be a floor rather than a threshold one above it.
func TestAMacOSTierAtApplesFloorIsAccepted(t *testing.T) {
	body := strings.Replace(validTartConfig, "memory: 8GiB",
		"memory: "+MinMacOSGuestMemory.String(), 1)

	if _, err := loadTart(t, body); err != nil {
		t.Fatalf("a macOS tier at exactly Apple's floor was refused: %v", err)
	}
}
