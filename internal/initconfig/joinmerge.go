package initconfig

import (
	"bytes"
	"errors"
	"fmt"
	"math"
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
// share, the larger of what it proposes and what it registered, dimension by
// dimension. That is idempotent: a second accept, or one run after the node has
// registered, raises nothing again, where adding the share to the ceiling would
// raise it each time; and a stale proposal cannot hide a larger registration.
//
// NOTHING ELSE CHANGES, AND THAT IS PROVED: the result loads, and once its
// tiers, its nodes: and its ceiling are set aside it is the configuration it
// was, with the existing tiers and policy entries unchanged and in order. A
// merge key or an anchor in the operator's file is kept by the re-encoding,
// and this is what catches one that would have changed what it means.
func MergeJoin(cp []byte, p JoinProposal, registered []ContributingNode) (JoinMerge, error) {
	if err := p.ownEntriesOnly(); err != nil {
		return JoinMerge{}, err
	}

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

	// INHERITED KEYS ARE NOT EDITED: a `tiers:` that arrives through a merge key
	// is not a key of this mapping, and adding one would shadow the whole list.
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "<<" {
			return JoinMerge{}, errors.New("the control plane's config merges keys into its top level " +
				"(<<), so its tiers and nodes: may be inherited; merge this node's half by hand")
		}
	}

	before, err := config.Parse("the control plane's config", cp)
	if err != nil {
		return JoinMerge{}, fmt.Errorf("the control plane's config does not load, so nothing was merged: %w", err)
	}

	var m JoinMerge

	if m.AddedTiers, m.KeptTiers, err = mergeEntries(root, "tiers", "label", p.Tiers, sameAs[config.Tier]); err != nil {
		return JoinMerge{}, err
	}
	if m.AddedNodes, m.KeptNodes, err = mergeEntries(root, "nodes", "name", p.Nodes, sameAs[config.NodePolicy]); err != nil {
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
		if r.Name == p.Node {
			wantVCPU, wantMemory = max(wantVCPU, r.VCPU), max(wantMemory, r.Memory)
		}
	}
	for _, r := range registered {
		if r.Name == p.Node {
			continue
		}
		if r.VCPU < 0 || r.Memory < 0 || wantVCPU > math.MaxInt-r.VCPU ||
			wantMemory > config.ByteSize(math.MaxInt64)-r.Memory {
			return JoinMerge{}, fmt.Errorf("the registered contributions do not sum (at %s)", r.Name)
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
	after, err := config.Parse("the merged config", out.Bytes())
	if err != nil {
		return JoinMerge{}, fmt.Errorf("the config this merge renders does not load, so nothing was "+
			"changed: %w", err)
	}

	if err := onlyTheJoinChanged(before, after, len(m.AddedNodes)); err != nil {
		return JoinMerge{}, fmt.Errorf("the merge would change more than this node's half, so nothing was "+
			"changed (%w); merge it by hand", err)
	}

	m.Body = out.Bytes()

	return m, nil
}

// prefixEqual reports whether long begins with every element of short, each
// deeply equal (a nil and an empty list are the same list here).
func prefixEqual[T any](long, short []T) bool {
	if len(long) < len(short) {
		return false
	}

	for i := range short {
		if !reflect.DeepEqual(long[i], short[i]) {
			return false
		}
	}

	return true
}

// onlyTheJoinChanged proves after is before with tiers and nodes: entries
// appended and the ceiling raised, and nothing else.
func onlyTheJoinChanged(before, after *config.Config, addedNodes int) error {
	if len(after.Tiers) < len(before.Tiers) || !prefixEqual(after.Tiers, before.Tiers) {
		return errors.New("the tiers already there would change")
	}
	if len(after.Nodes) != len(before.Nodes)+addedNodes || !prefixEqual(after.Nodes, before.Nodes) {
		return errors.New("the nodes: entries already there would change")
	}

	b, a := *before, *after
	b.Tiers, b.Nodes, a.Tiers, a.Nodes = nil, nil, nil, nil
	if b.Server == nil || a.Server == nil {
		return errors.New("the server block would go")
	}
	bs, as := *b.Server, *a.Server
	bs.MaxVCPU, bs.MaxMemory, as.MaxVCPU, as.MaxMemory = 0, 0, 0, 0
	b.Server, a.Server = &bs, &as

	if !reflect.DeepEqual(a, b) {
		return errors.New("a setting outside the tiers, the nodes: entries and the ceiling would change")
	}

	return nil
}

// mergeEntries adds each proposed entry to root's list at key, matched by the
// scalar at id: an identical entry is kept, a differing one refused.
func mergeEntries(root *yaml.Node, key, id string, proposed []*yaml.Node,
	same func(a, b *yaml.Node) (bool, error),
) ([]string, []string, error) {
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

		equal, err := same(existing, entry)
		if err != nil {
			return nil, nil, err
		}
		if !equal {
			return nil, nil, fmt.Errorf("the control plane already has a %s entry with %s %q that "+
				"differs from the one this node proposes, so nothing was changed; decide which is right "+
				"and edit it, or give the node's a %s of its own and accept again:\n  control plane: %s\n  "+
				"proposed:      %s", key, id, name, id, flowYAML(existing), flowYAML(entry))
		}

		kept = append(kept, name)
	}

	return added, kept, nil
}

// sameAs compares two entries as billet loads them, as a T: key order, style
// and comments do not make two differ, and `node: TRUE` and `node: true`,
// one boolean to untyped YAML, are two different pins.
func sameAs[T any](a, b *yaml.Node) (bool, error) {
	var av, bv T
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
