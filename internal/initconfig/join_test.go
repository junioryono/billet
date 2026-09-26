package initconfig

import (
	"strconv"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// A JOINED NODE CARRIES THE CEILING IT WAS MEASURED TO HAVE. The failure this
// exists for: a single-machine generation hand-edited into a node lost its
// ceiling with the server block, so the node contributed its whole machine
// against a control plane sized for less, and nothing said so.
func TestJoinMovesTheMeasuredCeilingOntoTheNode(t *testing.T) {
	body, _, err := Generate(dockerParams())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	got, err := Join(body, "10.0.0.1:7717", "/etc/billet/tls")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	cfg, err := config.Parse("joined", []byte(got.Node))
	if err != nil {
		t.Fatalf("the joined config does not load: %v\n%s", err, got.Node)
	}

	if cfg.Server != nil {
		t.Errorf("a joined node still carries a server block")
	}
	if cfg.GitHub != nil {
		t.Errorf("a joined node still carries a github block")
	}
	if cfg.Node == nil {
		t.Fatalf("a joined config has no node block:\n%s", got.Node)
	}
	if cfg.Node.ServerAddr != "10.0.0.1:7717" {
		t.Errorf("node.server_addr = %q, want the control plane's address", cfg.Node.ServerAddr)
	}

	p := dockerParams()
	if want := CeilingVCPU(p.VCPU); cfg.Node.MaxVCPU != want {
		t.Errorf("node.max_vcpu = %d, want the measured ceiling %d", cfg.Node.MaxVCPU, want)
	}
	if want := CeilingMemory(p.Memory); cfg.Node.MaxMemory != want {
		t.Errorf("node.max_memory = %s, want the measured ceiling %s", cfg.Node.MaxMemory, want)
	}

	if cfg.Node.TLS == nil || cfg.Node.TLS.CertPath != "/etc/billet/tls/node.crt" ||
		cfg.Node.TLS.KeyPath != "/etc/billet/tls/node.key" || cfg.Node.TLS.CAPath != "/etc/billet/tls/ca.crt" {
		t.Errorf("node.tls = %+v, want the bundle under /etc/billet/tls", cfg.Node.TLS)
	}

	if len(cfg.Tiers) == 0 {
		t.Errorf("a joined node lost its tiers, which a host that boots images reads")
	}
}

// THE CONTROL PLANE'S HALF names the ceiling to add and the tiers to paste,
// because the join's whole value is that nobody derives them by hand.
func TestJoinSaysWhatTheControlPlaneNeeds(t *testing.T) {
	body, _, err := Generate(dockerParams())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	got, err := Join(body, "controller.example:7717", "/etc/billet/tls")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	p := dockerParams()
	for _, want := range []string{
		"Raise server.max_vcpu by " + strconv.Itoa(CeilingVCPU(p.VCPU)),
		"server.max_memory by " + CeilingMemory(p.Memory).String(),
		"tiers:",
		"- label:",
	} {
		if !strings.Contains(got.ControlPlane, want) {
			t.Errorf("the control plane's half does not say %q:\n%s", want, got.ControlPlane)
		}
	}
}

func TestJoinRefusesAnEmptyControlPlaneAddress(t *testing.T) {
	body, _, err := Generate(dockerParams())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if _, err := Join(body, "  ", "/etc/billet/tls"); err == nil || !strings.Contains(err.Error(), "--join") {
		t.Fatalf("Join accepted an empty control plane address: %v", err)
	}
}
