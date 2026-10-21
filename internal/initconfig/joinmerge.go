package initconfig

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/junioryono/billet/internal/config"
)

// ContributingNode is what one registered host contributes, as the control
// plane's ledger records it.
type ContributingNode struct {
	Name   string
	VCPU   int
	Memory config.ByteSize
}

// JoinMerge is a proposal merged into a control plane's configuration.
type JoinMerge struct {
	// Body is the merged configuration.
	Body []byte
	// AddedTiers and KeptTiers are the proposal's tier labels this merge
	// added and found already there, identical; likewise for nodes:.
	AddedTiers, KeptTiers []string
	AddedNodes, KeptNodes []string
	// VCPU and Memory are the deployment ceiling before and after.
	VCPU   [2]int
	Memory [2]config.ByteSize
}

// Changed reports whether the merge changes anything.
func (m JoinMerge) Changed() bool {
	return len(m.AddedTiers) > 0 || len(m.AddedNodes) > 0 || m.VCPU[0] != m.VCPU[1] || m.Memory[0] != m.Memory[1]
}

// MergeJoin merges a joining node's proposal into a control plane's
// configuration, editing the YAML tree so the operator's comments survive.
//
// AN ENTRY ALREADY THERE IS KEPT, NEVER CHANGED. A tier the proposal names by a
// label the control plane already has is skipped when the two are identical
// and refused when they differ, naming both: a label is one scale set, and
// which definition of it is right is the operator's to decide. The same holds
// for a nodes: entry by name.
//
// THE CEILING COVERS THE FLEET, AND IS NEVER LOWERED: it becomes at least what
// every registered host other than this one contributes plus this node's
// share. That is idempotent: a second accept, or one run after the node has
// registered (when its own row is set aside as this node), raises nothing
// again, where adding the share to the ceiling would raise it each time.
func MergeJoin(cp []byte, p JoinProposal, registered []ContributingNode) (JoinMerge, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(cp, &doc); err != nil {
		return JoinMerge{}, fmt.Errorf("the control plane's config does not parse: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return JoinMerge{}, errors.New("the control plane's config is not a mapping")
	}
	root := doc.Content[0]

	server := mappingValue(root, "server")
	if server == nil || server.Kind != yaml.MappingNode {
		return JoinMerge{}, errors.New("the config has no server block, so it is not a control plane's")
	}

	var m JoinMerge

	var err error
	if m.AddedTiers, m.KeptTiers, err = mergeEntries(root, "tiers", "label", p.Tiers); err != nil {
		return JoinMerge{}, err
	}
	if m.AddedNodes, m.KeptNodes, err = mergeEntries(root, "nodes", "name", p.Nodes); err != nil {
		return JoinMerge{}, err
	}

	curVCPU, err := strconv.Atoi(scalarValue(server, "max_vcpu"))
	if err != nil {
		return JoinMerge{}, fmt.Errorf("server.max_vcpu is %q: %w", scalarValue(server, "max_vcpu"), err)
	}
	curMemory, err := config.ParseByteSize(scalarValue(server, "max_memory"))
	if err != nil {
		return JoinMerge{}, fmt.Errorf("server.max_memory: %w", err)
	}

	wantVCPU, wantMemory := p.MaxVCPU, p.MaxMemory
	for _, r := range registered {
		if p.Node != "" && r.Name == p.Node {
			continue
		}
		wantVCPU += r.VCPU
		wantMemory += r.Memory
	}

	m.VCPU = [2]int{curVCPU, max(curVCPU, wantVCPU)}
	m.Memory = [2]config.ByteSize{curMemory, max(curMemory, wantMemory)}

	if m.VCPU[1] != curVCPU {
		setScalar(server, "max_vcpu", strconv.Itoa(m.VCPU[1]))
	}
	if m.Memory[1] != curMemory {
		setScalar(server, "max_memory", m.Memory[1].String())
	}

	if !m.Changed() {
		m.Body = cp

		return m, nil
	}

	var out bytes.Buffer

	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return JoinMerge{}, fmt.Errorf("render the merged config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return JoinMerge{}, fmt.Errorf("render the merged config: %w", err)
	}

	// PROVED BEFORE IT IS RETURNED, like every file billet generates: a merge
	// the control plane would refuse to load is one it must not be handed.
	if _, err := config.Parse("the merged config", out.Bytes()); err != nil {
		return JoinMerge{}, fmt.Errorf("the config this merge renders does not load, so nothing was "+
			"changed: %w", err)
	}

	m.Body = out.Bytes()

	return m, nil
}

// mergeEntries adds each proposed entry to root's list at key, matched by the
// scalar at id: an identical entry is kept, a differing one refused.
func mergeEntries(root *yaml.Node, key, id string, proposed []*yaml.Node) ([]string, []string, error) {
	if len(proposed) == 0 {
		return nil, nil, nil
	}

	list := mappingValue(root, key)
	if list == nil {
		list = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		root.Content = append(root.Content, scalar(key), list)
	}
	if list.Kind != yaml.SequenceNode {
		return nil, nil, fmt.Errorf("the control plane's %s is not a list", key)
	}

	var added, kept []string

	for _, entry := range proposed {
		name := scalarValue(entry, id)

		var existing *yaml.Node
		for _, item := range list.Content {
			if scalarValue(item, id) == name {
				existing = item

				break
			}
		}

		if existing == nil {
			list.Content = append(list.Content, entry)
			added = append(added, name)

			continue
		}

		same, err := sameValue(existing, entry)
		if err != nil {
			return nil, nil, err
		}
		if !same {
			return nil, nil, fmt.Errorf("the control plane already has a %s entry with %s %q that "+
				"differs from the one this node proposes, so nothing was changed; decide which is right "+
				"and edit it, or give the node's a %s of its own and accept again:\n  control plane: %s\n  "+
				"proposed:      %s", key, id, name, id, flowYAML(existing), flowYAML(entry))
		}

		kept = append(kept, name)
	}

	return added, kept, nil
}

// sameValue compares two YAML nodes by the values they decode to, so key
// order, style and comments do not make two entries differ.
func sameValue(a, b *yaml.Node) (bool, error) {
	var av, bv any
	if err := a.Decode(&av); err != nil {
		return false, fmt.Errorf("decode an entry: %w", err)
	}
	if err := b.Decode(&bv); err != nil {
		return false, fmt.Errorf("decode an entry: %w", err)
	}

	return reflect.DeepEqual(av, bv), nil
}

// flowYAML renders a node on one line, for a refusal.
func flowYAML(n *yaml.Node) string {
	c := *n
	c.Style = yaml.FlowStyle

	raw, err := yaml.Marshal(&c)
	if err != nil {
		return "(unrenderable)"
	}

	return string(bytes.TrimSpace(raw))
}
