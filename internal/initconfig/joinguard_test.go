package initconfig

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/junioryono/billet/internal/config"
)

// entry parses one YAML mapping, for a proposal's tiers or nodes:.
func entry(t *testing.T, text string) *yaml.Node {
	t.Helper()

	var n yaml.Node
	if err := yaml.Unmarshal([]byte(text), &n); err != nil {
		t.Fatal(err)
	}

	return n.Content[0]
}

// A PROPOSAL REACHES ONLY ITS OWN ENTRIES (Codex, 2026-10-21): an anchor that
// re-encoding could let redefine one of the operator's, an alias, a merge key
// or a custom tag; policy for another host; a tier pinned to another host or
// on another backend; and a proposal naming no node or no deployment are each
// refused before anything is merged.
func TestAJoinProposalReachesOnlyItsOwnEntries(t *testing.T) {
	t.Parallel()

	cp := controlPlane(t)

	for name, c := range map[string]struct {
		mutate func(p *JoinProposal)
		want   string
	}{
		"an anchor": {func(p *JoinProposal) {
			p.Tiers = append(p.Tiers, entry(t, "label: billet-x\nprovider: tart\nimage: &org other\nvcpu: 2\nmemory: 8GiB\n"))
		}, "anchor"},
		"an alias": {func(p *JoinProposal) {
			p.Tiers = append(p.Tiers, entry(t, "label: billet-x\nprovider: tart\nimage: &i a\nmore: *i\n"))
		}, "anchor"},
		"a merge key": {func(p *JoinProposal) {
			p.Nodes[0] = entry(t, "name: "+p.Node+"\n<<: {macos_vm_limit: 9}\n")
		}, "merge key"},
		"a custom tag": {func(p *JoinProposal) {
			p.Tiers = append(p.Tiers, entry(t, "label: billet-x\nprovider: !mine tart\n"))
		}, "tag"},
		"another host's policy": {func(p *JoinProposal) {
			p.Nodes = append(p.Nodes, entry(t, "name: mac-old\nprovider: tart\nmacos_vm_limit: 9\n"))
		}, "policy for \"mac-old\""},
		"another backend's policy": {func(p *JoinProposal) {
			p.Nodes[0] = entry(t, "name: "+p.Node+"\nprovider: firecracker\n")
		}, "names provider firecracker"},
		"a tier pinned elsewhere": {func(p *JoinProposal) {
			p.Tiers = append(p.Tiers, entry(t, "label: billet-x\nprovider: tart\nnode: mac-old\nvcpu: 2\nmemory: 8GiB\n"))
		}, "pinned to \"mac-old\""},
		"a tier on another backend": {func(p *JoinProposal) {
			p.Tiers = append(p.Tiers, entry(t, "label: billet-x\nprovider: docker\nvcpu: 2\nmemory: 8GiB\n"))
		}, "runs on docker"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			p := macProposal(t)
			c.mutate(&p)

			if _, err := MergeJoin(cp, p, nil); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s answered %v, want a refusal saying %q", name, err, c.want)
			}
		})
	}

	p := macProposal(t)
	p.Deployment = "0123456789abcdef0123456789abcdef"
	// NO POLICY ENTRY, so a proposal without its node is refused for naming no
	// node and not for carrying another's policy.
	p.Nodes = nil
	doc, err := p.YAML()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseJoinProposal(string(doc)); err != nil {
		t.Fatalf("the proposal itself was refused: %v", err)
	}
	for name, text := range map[string]string{
		"no deployment": strings.Replace(string(doc), "deployment: "+p.Deployment+"\n", "", 1),
		"no node":       strings.Replace(string(doc), "node: "+p.Node+"\n", "", 1),
	} {
		if text == string(doc) {
			t.Fatalf("%s: the case changed nothing", name)
		}
		if _, err := ParseJoinProposal(text); err == nil || !strings.Contains(err.Error(), map[string]string{
			"no deployment": "bound to no deployment", "no node": "names no node",
		}[name]) {
			t.Errorf("a proposal with %s answered %v", name, err)
		}
	}
}

// AN INHERITED CATALOGUE IS NOT SHADOWED: a control plane whose top level
// merges keys in is refused rather than given an explicit tiers: list holding
// only the proposal.
func TestMergeJoinRefusesAnInheritedTopLevel(t *testing.T) {
	t.Parallel()

	cp := string(controlPlane(t))
	i := strings.Index(cp, "tiers:")
	if i < 0 {
		t.Fatal("the control plane has no tiers: to inherit")
	}
	inherited := cp[:i] + "<<:\n  " + strings.ReplaceAll(strings.TrimRight(cp[i:], "\n"), "\n", "\n  ") + "\n"

	// A CONFIG THAT LOADS, with the catalogue it inherits, so the refusal is
	// the merge's and not the loader's.
	cfg, err := config.Parse("inherited", []byte(inherited))
	if err != nil || len(cfg.Tiers) == 0 {
		t.Fatalf("the inherited control plane does not load with its tiers (%v):\n%s", err, inherited)
	}
	if _, err := MergeJoin([]byte(inherited), macProposal(t), nil); err == nil || !strings.Contains(err.Error(), "<<") {
		t.Errorf("an inherited top level was merged into: %v", err)
	}
}

// ENTRIES ARE COMPARED AS BILLET LOADS THEM: `node: TRUE` and `node: true` are
// one boolean to untyped YAML and two different pins to billet, so a tier
// differing only so is refused, never kept as identical.
func TestMergeJoinComparesEntriesAsBilletLoadsThem(t *testing.T) {
	t.Parallel()

	a := entry(t, "label: billet-x\nnode: TRUE\n")
	b := entry(t, "label: billet-x\nnode: true\n")

	if same, err := sameAs[config.Tier](a, b); err != nil || same {
		t.Errorf("two pins compared the same (%v)", err)
	}
	if same, err := sameAs[config.Tier](a, entry(t, "node: TRUE\nlabel: billet-x # reordered\n")); err != nil || !same {
		t.Errorf("a reordered, commented entry compared different (%v)", err)
	}
}

// A STALE PROPOSAL CANNOT HIDE A LARGER REGISTRATION: the node's share is the
// larger of what it proposes and what it registered, dimension by dimension.
func TestMergeJoinCoversTheLargerOfTheProposalAndTheRegistration(t *testing.T) {
	t.Parallel()

	p := macProposal(t)
	cp := controlPlane(t)
	curVCPU, curMemory := ceilingOf(t, cp)

	registered := []ContributingNode{
		{Name: "linux-1", VCPU: curVCPU, Memory: curMemory},
		{Name: p.Node, VCPU: p.MaxVCPU + 6, Memory: p.MaxMemory - config.GiB},
	}

	m, err := MergeJoin(cp, p, registered)
	if err != nil {
		t.Fatalf("MergeJoin: %v", err)
	}

	vcpu, memory := ceilingOf(t, m.Body)
	if vcpu != curVCPU+p.MaxVCPU+6 || memory != curMemory+p.MaxMemory {
		t.Errorf("the ceiling is %d and %s, want %d and %s (the registration's vCPU, the proposal's memory)",
			vcpu, memory, curVCPU+p.MaxVCPU+6, curMemory+p.MaxMemory)
	}
}
