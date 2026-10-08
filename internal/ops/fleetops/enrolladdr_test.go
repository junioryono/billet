package fleetops

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// WHERE AN ENROLLING MACHINE ASKS, and the fallback that used to be the only
// answer.
//
// A control plane serving enrollment on an address of its own cannot be reached
// for it at node.server_addr: the node wire refuses a connection with no
// certificate in the handshake, and a machine that is enrolling has none. So the
// flag and the config key exist, and the fallback stays because it is right for a
// control plane that has no separate address.
//
// TABLE OVER ALL THREE SOURCES, because the ORDER is the whole content: a
// resolution that reads the config before the flag looks identical in every test
// that sets only one of them.
func TestTheEnrollmentAddressPrefersTheFlagThenTheConfig(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name             string
		flag, cfgAddr    string
		serverAddr, want string
	}{
		{
			name: "the flag wins over both",
			flag: "flag.example:7718", cfgAddr: "cfg.example:7718",
			serverAddr: "wire.example:7717", want: "https://flag.example:7718",
		},
		{
			name:    "the config key wins over the node wire",
			cfgAddr: "cfg.example:7718", serverAddr: "wire.example:7717",
			want: "https://cfg.example:7718",
		},
		{
			name:       "and the node wire is the fallback",
			serverAddr: "wire.example:7717", want: "https://wire.example:7717",
		},
		{
			name: "whitespace is not an address",
			flag: "   ", cfgAddr: "\t", serverAddr: "wire.example:7717",
			want: "https://wire.example:7717",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			cfg := &config.Config{Node: &config.NodeConfig{
				ServerAddr:    c.serverAddr,
				BootstrapAddr: c.cfgAddr,
			}}

			if got := BootstrapBase(cfg, c.flag); got != c.want {
				t.Errorf("bootstrapBase = %q, want %q", got, c.want)
			}
		})
	}
}

// AND THE CONTROL PLANE PRINTS THE ADDRESS RATHER THAN LEAVING IT TO BE GUESSED.
//
// The operator is already carrying a fingerprint and a join token from the
// controller to the new machine; the address is the third thing in that hand-off,
// and getting it wrong ends in a handshake failure that names no cause. A
// wildcard says which interfaces to accept on and not which name to dial, so only
// the port is billet's to state.
func TestTheEnrollmentFlagIsPrintedOnlyWhenThereIsOne(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		listen, want string
	}{
		{listen: "", want: ""},
		{listen: "billet.example:7718", want: " --bootstrap-addr billet.example:7718"},
		{listen: "0.0.0.0:7718", want: " --bootstrap-addr <this control plane>:7718"},
		{listen: ":7718", want: " --bootstrap-addr <this control plane>:7718"},
		{listen: "[::]:7718", want: " --bootstrap-addr <this control plane>:7718"},
	} {
		t.Run(c.listen, func(t *testing.T) {
			t.Parallel()

			cfg := &config.Config{Server: &config.ServerConfig{BootstrapListen: c.listen}}
			if got := enrollAddrFlag(cfg); got != c.want {
				t.Errorf("enrollAddrFlag(%q) = %q, want %q", c.listen, got, c.want)
			}
		})
	}
}
