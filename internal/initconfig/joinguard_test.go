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
		"a tier listing another backend": {func(p *JoinProposal) {
			p.Tiers = append(p.Tiers, entry(t, "label: billet-x\nproviders: [tart, firecracker]\nvcpu: 2\nmemory: 8GiB\n"))
		}, "runs on firecracker"},
		"a tier naming no backend": {func(p *JoinProposal) {
			p.Tiers = append(p.Tiers, entry(t, "label: billet-x\nvcpu: 2\nmemory: 8GiB\n"))
		}, "names no backend"},
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

// AN ALIAS INSIDE AN ENTRY IS REFUSED even when its anchor sits outside every
// entry, where the anchor check never sees it: a top-level field of the
// proposal may carry one.
func TestAJoinProposalRefusesAnAliasToAnAnchorOutsideItsEntries(t *testing.T) {
	t.Parallel()

	p := macProposal(t)
	p.Deployment = "0123456789abcdef0123456789abcdef"
	doc, err := p.YAML()
	if err != nil {
		t.Fatal(err)
	}

	text := strings.Replace(string(doc), "schema: 1\n", "schema: 1\nsite: &here office\n", 1)
	text = strings.Replace(text, "  - label: ", "  - region: *here\n    label: ", 1)
	if !strings.Contains(text, "&here") || !strings.Contains(text, "*here") {
		t.Fatalf("the case did not place the anchor and the alias:\n%s", text)
	}

	if _, err := ParseJoinProposal(text); err == nil || !strings.Contains(err.Error(), "holds an alias") {
		t.Errorf("an alias inside a tier answered %v", err)
	}
}

// NOTHING OUTSIDE THE NODE'S HALF CHANGES, AND THAT IS WHAT IS CHECKED: a
// merged config whose server block, existing tiers or existing nodes: entries
// differ from the original's is refused, each by its own clause; appended
// entries and a raised ceiling are not.
func TestOnlyTheJoinChangedRefusesAnyOtherDifference(t *testing.T) {
	t.Parallel()

	before, err := config.Parse("before", controlPlane(t))
	if err != nil {
		t.Fatal(err)
	}
	reparse := func() *config.Config {
		c, err := config.Parse("after", controlPlane(t))
		if err != nil {
			t.Fatal(err)
		}

		return c
	}

	ok := reparse()
	ok.Server.MaxVCPU += 4
	ok.Tiers = append(ok.Tiers, config.Tier{Label: "billet-added"})
	ok.Nodes = append(ok.Nodes, config.NodePolicy{Name: "mac-1"})
	if err := onlyTheJoinChanged(before, ok, 1); err != nil {
		t.Errorf("an appended tier and nodes: entry and a raised ceiling were refused: %v", err)
	}

	for name, c := range map[string]struct {
		change func(*config.Config)
		want   string
	}{
		"a server setting":  {func(c *config.Config) { c.Server.Listen = "0.0.0.0:7717" }, "outside the tiers"},
		"an existing tier":  {func(c *config.Config) { c.Tiers[0].VCPU++ }, "tiers already there"},
		"a dropped tier":    {func(c *config.Config) { c.Tiers = c.Tiers[1:] }, "tiers already there"},
		"an existing entry": {func(c *config.Config) { c.Nodes = []config.NodePolicy{{Name: "x"}} }, "nodes: entries"},
	} {
		after := reparse()
		c.change(after)
		if err := onlyTheJoinChanged(before, after, 0); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s answered %v, want a refusal saying %q", name, err, c.want)
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
// differing only so is refused, never kept as identical; driven through
// MergeJoin as well as the comparison it calls.
func TestMergeJoinComparesEntriesAsBilletLoadsThem(t *testing.T) {
	t.Parallel()

	p := macProposal(t)
	p.Node = "TRUE"
	p.Nodes = nil
	for _, tier := range p.Tiers {
		if scalarValue(tier, "node") != "" {
			setScalar(tier, "node", "TRUE")
		}
	}
	first, err := MergeJoin(controlPlane(t), p, nil)
	if err != nil {
		t.Fatalf("MergeJoin: %v", err)
	}

	lower := p
	lower.Node = "true"
	lower.Tiers = nil
	for _, tier := range p.Tiers {
		raw, err := yaml.Marshal(tier)
		if err != nil {
			t.Fatal(err)
		}
		copied := entry(t, string(raw))
		if scalarValue(copied, "node") != "" {
			setScalar(copied, "node", "true")
		}
		lower.Tiers = append(lower.Tiers, copied)
	}
	if _, err := MergeJoin(first.Body, lower, nil); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Errorf("a tier pinned to true over one pinned to TRUE answered %v, want the conflict", err)
	}

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
