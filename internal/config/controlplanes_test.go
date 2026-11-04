package config

import (
	"slices"
	"strings"
	"testing"
)

// remoteNode is a node alone, dialling a control plane on another machine with
// the certificate that needs.
const remoteNode = `
node:
  name: host-1
  server_addr: 10.0.0.4:7717
  provider: docker
  state_dir: /var/lib/billet/node
  lock_dir: /run/billet/locks
  tls:
    cert: /etc/billet/tls/node.crt
    key: /etc/billet/tls/node.key
    ca: /etc/billet/tls/ca.crt
`

// withAddrs replaces remoteNode's server_addr with the lines given.
func withAddrs(lines string) string {
	return strings.Replace(remoteNode, "  server_addr: 10.0.0.4:7717\n", lines, 1)
}

// EVERY CONTROLLER IN ONE KEY, in the order given, and server_addr is that
// list of one: what the node dials is the same question either way.
func TestANodeNamesEveryControllerItDials(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		body string
		want []string
	}{
		"server_addrs": {
			withAddrs("  server_addrs: [control-b.example:7717, 10.0.0.4:7717]\n"),
			[]string{"control-b.example:7717", "10.0.0.4:7717"},
		},
		"a list of one": {withAddrs("  server_addrs: [10.0.0.4:7717]\n"), []string{"10.0.0.4:7717"}},
		"server_addr":   {remoteNode, []string{"10.0.0.4:7717"}},
		// NAMING NONE, beside a server on another port, so the answer can only
		// have come from the server's listen address.
		"the server's own": {
			strings.Replace(strings.Replace(validConfig, "  server_addr: 127.0.0.1:7717\n", "", 1),
				"listen: 127.0.0.1:7717", "listen: 127.0.0.1:7799", 1),
			[]string{"127.0.0.1:7799"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg, err := Load(writeConfig(t, tc.body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			if got := cfg.Node.ControlPlanes(); !slices.Equal(got, tc.want) {
				t.Fatalf("the node dials %v, want %v", got, tc.want)
			}
		})
	}
}

// A LIST THAT IS SET IS NOT FILLED FROM THE SERVER'S LISTEN ADDRESS: the
// default is for a file that names no controller at all.
func TestAControllerListIsNotFilledFromTheServer(t *testing.T) {
	t.Parallel()

	body := strings.Replace(validConfig, "  server_addr: 127.0.0.1:7717\n",
		"  server_addrs: [127.0.0.1:7717, localhost:7717]\n", 1)

	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Node.ServerAddr != "" {
		t.Fatalf("node.server_addr was filled with %q beside node.server_addrs", cfg.Node.ServerAddr)
	}

	if got := cfg.Node.ControlPlanes(); !slices.Equal(got, []string{"127.0.0.1:7717", "localhost:7717"}) {
		t.Fatalf("the node dials %v", got)
	}
}

// WHAT A LIST MAY NOT BE: both keys at once, empty, an element that is not
// host:port, the same controller twice, or a remote controller without the
// certificate every remote one needs. Each refusal names what to change.
func TestAControllerListIsRefusedWhenItCannotBeDialled(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		body, want string
	}{
		"both keys": {
			strings.Replace(remoteNode, "  server_addr: 10.0.0.4:7717\n",
				"  server_addr: 10.0.0.4:7717\n  server_addrs: [10.0.0.5:7717]\n", 1),
			"remove node.server_addr",
		},
		"an empty list": {withAddrs("  server_addrs: []\n"), "node.server_addrs is empty"},
		"not host:port": {
			withAddrs("  server_addrs: [10.0.0.4:7717, https://10.0.0.5:7717]\n"),
			"node.server_addrs[1]",
		},
		"no port": {withAddrs("  server_addrs: [10.0.0.4]\n"), "node.server_addrs[0]"},
		"more controllers than a node may name": {
			withAddrs("  server_addrs: [a:1, b:1, c:1, d:1, e:1]\n"),
			"names 5 controllers, more than the 4",
		},
		"an address longer than a host name and a port": {
			withAddrs("  server_addrs: [" + strings.Repeat("a", 255) + ":7717, 10.0.0.4:7717]\n"),
			"node.server_addrs[0] is 260 bytes",
		},
		"a controller twice": {
			withAddrs("  server_addrs: [Control.example:7717, 10.0.0.4:7717, control.example:7717]\n"),
			"node.server_addrs[2] \"control.example:7717\" repeats node.server_addrs[0]",
		},
		"a remote controller without a certificate": {
			strings.Replace(validConfig, "  server_addr: 127.0.0.1:7717\n",
				"  server_addrs: [127.0.0.1:7717, 10.0.0.5:7717]\n", 1),
			"\"10.0.0.5:7717\", which is not on this machine",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatal("the configuration was accepted")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal does not say %q: %v", tc.want, err)
			}
		})
	}
}
