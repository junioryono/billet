package initconfig

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/junioryono/billet/internal/config"
)

// JoinResult is a generation turned into a node that joins a control plane
// somewhere else.
type JoinResult struct {
	// Node is this machine's billet.yaml: no server, no github, dialling
	// ServerAddr with a certificate, contributing the ceiling the generation
	// measured.
	Node string
	// ControlPlane is what the control plane needs for this node to be useful:
	// its tiers, its nodes: policy, and how far to raise the deployment ceiling.
	// Printed, never written, because it belongs in another machine's file.
	ControlPlane string
}

// Join turns a single-machine generation into a node-only config for a control
// plane at serverAddr, with its certificate bundle under tlsDir.
//
// A TRANSFORM OF Generate's OUTPUT, not a second generator, so what the machine
// was measured to hold, and the tiers that fit it, have one author. What moves:
// the measured ceiling leaves server.max_vcpu/max_memory, which a node-only file
// does not have, for node.max_vcpu/max_memory. Left unset there, a node
// contributes its WHOLE machine, more than the ceiling the control plane was
// sized for, and nothing says so; a hand-edited join lost it exactly that way.
func Join(generated, serverAddr, tlsDir string) (JoinResult, error) {
	serverAddr = strings.TrimSpace(serverAddr)
	if serverAddr == "" {
		return JoinResult{}, errors.New("--join: the control plane's node-wire address is empty")
	}
	if strings.TrimSpace(tlsDir) == "" {
		return JoinResult{}, errors.New("--join: no directory for the node's certificate bundle")
	}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(generated), &doc); err != nil {
		return JoinResult{}, fmt.Errorf("the generated config is not YAML: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return JoinResult{}, errors.New("the generated config is not a mapping")
	}
	root := doc.Content[0]

	server := mappingValue(root, "server")
	node := mappingValue(root, "node")
	if server == nil || node == nil {
		return JoinResult{}, errors.New("the generated config has no server and node to split")
	}
	maxVCPU := mappingValue(server, "max_vcpu")
	maxMemory := mappingValue(server, "max_memory")
	if maxVCPU == nil || maxMemory == nil {
		return JoinResult{}, errors.New("the generated server block names no ceiling to carry to the node")
	}

	for _, key := range []string{"server", "github", "targets"} {
		deleteKey(root, key)
	}

	setScalar(node, "server_addr", serverAddr)
	setScalar(node, "max_vcpu", maxVCPU.Value)
	setScalar(node, "max_memory", maxMemory.Value)
	deleteKey(node, "tls")
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "tls",
			HeadComment: "The bundle from `billet ca issue <name>` on the control plane, or the\n" +
				"enrollment ceremony (`billet ca token`, then `billet node --enroll`)."},
		&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			scalar("cert"), scalar(filepath.Join(tlsDir, "node.crt")),
			scalar("key"), scalar(filepath.Join(tlsDir, "node.key")),
			scalar("ca"), scalar(filepath.Join(tlsDir, "ca.crt")),
		}},
	)

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return JoinResult{}, fmt.Errorf("render the node config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return JoinResult{}, fmt.Errorf("render the node config: %w", err)
	}

	// PROVED LIKE EVERY GENERATION: a join that billet itself would refuse to load
	// is a file the operator has to debug on a machine that cannot yet ask anyone.
	if _, err := config.Parse("joined billet.yaml", out.Bytes()); err != nil {
		return JoinResult{}, fmt.Errorf("the node config this join renders does not load: %w", err)
	}

	cp, err := controlPlaneHalf(root, maxVCPU.Value, maxMemory.Value)
	if err != nil {
		return JoinResult{}, err
	}

	return JoinResult{Node: out.String(), ControlPlane: cp}, nil
}

// controlPlaneHalf is the text an operator adds to the control plane's config.
func controlPlaneHalf(root *yaml.Node, maxVCPU, maxMemory string) (string, error) {
	var b strings.Builder

	fmt.Fprintf(&b, "Raise server.max_vcpu by %s and server.max_memory by %s: the deployment\n"+
		"ceiling caps every node, so a node it does not count is room nothing hands out.\n", maxVCPU, maxMemory)

	for _, key := range []string{"tiers", "nodes"} {
		v := mappingValue(root, key)
		if v == nil {
			continue
		}
		var out bytes.Buffer
		enc := yaml.NewEncoder(&out)
		enc.SetIndent(2)
		if err := enc.Encode(&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{scalar(key), v}}); err != nil {
			return "", fmt.Errorf("render the control plane's %s: %w", key, err)
		}
		if err := enc.Close(); err != nil {
			return "", fmt.Errorf("render the control plane's %s: %w", key, err)
		}
		fmt.Fprintf(&b, "\nAdd to the control plane's %s (a restart reads it):\n%s", key, out.String())
	}

	return b.String(), nil
}

func scalar(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }

func deleteKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)

			return
		}
	}
}

// setScalar replaces key's value in a mapping, or appends it.
func setScalar(m *yaml.Node, key, value string) {
	if v := mappingValue(m, key); v != nil {
		*v = yaml.Node{Kind: yaml.ScalarNode, Value: value}

		return
	}
	m.Content = append(m.Content, scalar(key), scalar(value))
}
