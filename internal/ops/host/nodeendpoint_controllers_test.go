package host

import (
	"errors"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// A NODE THAT NAMES SEVERAL CONTROLLERS HAS NO ONE ENDPOINT, and the endpoint
// commands say so rather than describe one of them: the installed endpoint is
// unknown with the reason, and nodeEndpointOf, which migrate-endpoint, receipt
// and repair-unit refuse through, answers errSeveralControllers. A list of one
// is that one endpoint, as node.server_addr is.
func TestTheEndpointToolsRefuseANodeWithSeveralControllers(t *testing.T) {
	t.Parallel()

	several := &config.Config{Node: &config.NodeConfig{
		ServerAddrs: []string{"control-a.example:7717", "control-b.example:7717"},
		TLS:         &config.NodeTLS{CertPath: "c", KeyPath: "k", CAPath: "a"},
	}}

	if _, hasNode, err := nodeEndpointOf(several); !errors.Is(err, errSeveralControllers) || !hasNode {
		t.Fatalf("nodeEndpointOf = (has node %v, %v), want errSeveralControllers", hasNode, err)
	}

	got := installedEndpoint(several, true)
	if got.known || !strings.Contains(got.why, "node.server_addrs") {
		t.Fatalf("the installed endpoint is %+v, want unknown naming node.server_addrs", got)
	}

	for name, node := range map[string]*config.NodeConfig{
		"a list of one": {ServerAddrs: []string{"control-a.example:7717"}},
		"server_addr":   {ServerAddr: "control-a.example:7717"},
	} {
		node.TLS = &config.NodeTLS{CertPath: "c", KeyPath: "k", CAPath: "a"}

		if got := installedEndpoint(&config.Config{Node: node}, true); !got.known ||
			got.value != "https://control-a.example:7717" {
			t.Fatalf("%s: the installed endpoint is %+v, want https://control-a.example:7717", name, got)
		}
	}
}
