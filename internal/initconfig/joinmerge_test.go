package initconfig

import (
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/junioryono/billet/internal/config"
)

// macProposal is what a tart Mac joining a control plane proposes.
func macProposal(t *testing.T) JoinProposal {
	t.Helper()

	body, _, err := Generate(tartParams(config.GuestMacOS, config.GuestLinux))
	if err != nil {
		t.Fatalf("Generate the Mac: %v", err)
	}

	joined, err := Join(body, "10.0.0.1:7717", "/usr/local/etc/billet/tls")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}

	if joined.Proposal.Node == "" || len(joined.Proposal.Tiers) == 0 {
		t.Fatalf("the Mac's proposal carries no node or tiers: %+v", joined.Proposal)
	}

	// THE POLICY ENTRY A MAC CARRIES when its generation writes one; the tart
	// generation leaves the defaults, so the entry is the one an operator would
	// add, and the merge must carry it like any other.
	var policy yaml.Node
	if err := yaml.Unmarshal([]byte("name: "+joined.Proposal.Node+
		"\nprovider: tart\nguest_os: [macos, linux]\nmacos_vm_limit: 2\n"), &policy); err != nil {
		t.Fatal(err)
	}
	joined.Proposal.Nodes = []*yaml.Node{policy.Content[0]}

	return joined.Proposal
}

// controlPlane is a control plane's config: a whole single-machine generation,
// with a comment of the operator's own.
func controlPlane(t *testing.T) []byte {
	t.Helper()

	body, _, err := Generate(dockerParams())
	if err != nil {
		t.Fatalf("Generate the control plane: %v", err)
	}

	// THE APP CREATED, as `billet github-app create` leaves it: a control plane
	// with zero ids does not load.
	body = strings.Replace(body, "app_id: 0\n", "app_id: 1\n", 1)
	body = strings.Replace(body, "installation_id: 0\n", "installation_id: 2\n", 1)

	return []byte("# the operator's own note, which a merge keeps\n" + body)
}

// ceilingOf reads a config's deployment ceiling.
func ceilingOf(t *testing.T, raw []byte) (int, config.ByteSize) {
	t.Helper()

	cfg, err := config.Parse("merged", raw)
	if err != nil {
		t.Fatalf("the config does not load: %v\n%s", err, raw)
	}

	return cfg.Server.MaxVCPU, cfg.Server.MaxMemory
}

// A JOIN ACCEPTED ADDS THE NODE'S TIERS AND ITS nodes: ENTRY AND RAISES THE
// CEILING TO COVER THE FLEET PLUS THE NODE (#183), keeping the operator's
// comments, and the result loads.
func TestMergeJoinAddsTheNodesHalfAndCoversTheFleet(t *testing.T) {
	t.Parallel()

	p := macProposal(t)
	cp := controlPlane(t)
	curVCPU, curMemory := ceilingOf(t, cp)

	// One host already registered, contributing exactly the ceiling.
	registered := []ContributingNode{{Name: "linux-1", VCPU: curVCPU, Memory: curMemory}}

	m, err := MergeJoin(cp, p, registered)
	if err != nil {
		t.Fatalf("MergeJoin: %v", err)
	}

	cfg, err := config.Parse("merged", m.Body)
	if err != nil {
		t.Fatalf("the merged config does not load: %v\n%s", err, m.Body)
	}

	for _, want := range p.Tiers {
		label := scalarValue(want, "label")
		found := false
		for i := range cfg.Tiers {
			if cfg.Tiers[i].Label == label {
				found = true
			}
		}
		if !found {
			t.Errorf("the merged config lacks the proposed tier %q", label)
		}
	}
	if len(m.AddedTiers) != len(p.Tiers) || len(m.AddedNodes) != len(p.Nodes) {
		t.Errorf("added tiers %v and nodes %v, want all %d and %d", m.AddedTiers, m.AddedNodes,
			len(p.Tiers), len(p.Nodes))
	}

	if cfg.Server.MaxVCPU != curVCPU+p.MaxVCPU || cfg.Server.MaxMemory != curMemory+p.MaxMemory {
		t.Errorf("the ceiling became %d and %s, want the fleet's %d and %s plus the node's %d and %s",
			cfg.Server.MaxVCPU, cfg.Server.MaxMemory, curVCPU, curMemory, p.MaxVCPU, p.MaxMemory)
	}

	if !strings.Contains(string(m.Body), "# the operator's own note, which a merge keeps") {
		t.Errorf("the merge dropped the operator's comment:\n%s", m.Body)
	}
}

// ACCEPTING TWICE, OR AFTER THE NODE REGISTERED, RAISES NOTHING AGAIN, and a
// ceiling already above the fleet is never lowered.
//
// Mutation: adding the share to the current ceiling instead of covering the
// fleet raises it on the second accept.
func TestMergeJoinIsIdempotentAndNeverLowersTheCeiling(t *testing.T) {
	t.Parallel()

	p := macProposal(t)
	cp := controlPlane(t)
	curVCPU, curMemory := ceilingOf(t, cp)
	fleet := []ContributingNode{{Name: "linux-1", VCPU: curVCPU, Memory: curMemory}}

	first, err := MergeJoin(cp, p, fleet)
	if err != nil {
		t.Fatalf("first MergeJoin: %v", err)
	}

	// The node has since registered: its own row is set aside as this node.
	registered := append(fleet, ContributingNode{Name: p.Node, VCPU: p.MaxVCPU, Memory: p.MaxMemory})

	for name, reg := range map[string][]ContributingNode{"again": fleet, "after registering": registered} {
		second, err := MergeJoin(first.Body, p, reg)
		if err != nil {
			t.Fatalf("%s: MergeJoin: %v", name, err)
		}
		if second.Changed() {
			t.Errorf("%s: a second accept changed the config: added %v %v, ceiling %v %v", name,
				second.AddedTiers, second.AddedNodes, second.VCPU, second.Memory)
		}
		if string(second.Body) != string(first.Body) {
			t.Errorf("%s: a second accept rewrote the config", name)
		}
	}

	// A ceiling already above what the fleet and the node need stays.
	high := strings.Replace(string(cp), "max_vcpu: "+strconv.Itoa(curVCPU), "max_vcpu: 512", 1)
	if high == string(cp) {
		t.Fatalf("the control plane's config no longer spells max_vcpu: %d", curVCPU)
	}
	m, err := MergeJoin([]byte(high), p, fleet)
	if err != nil {
		t.Fatalf("MergeJoin over a high ceiling: %v", err)
	}
	if got, _ := ceilingOf(t, m.Body); got != 512 {
		t.Errorf("a ceiling of 512 became %d", got)
	}
}

// AN ENTRY ALREADY THERE IS KEPT WHEN IT IS THE SAME, AND REFUSED WHEN IT IS
// NOT: a label is one scale set, and which definition is right is the
// operator's to decide. Nothing is changed by a refusal.
func TestMergeJoinRefusesADifferentEntryUnderTheSameLabel(t *testing.T) {
	t.Parallel()

	p := macProposal(t)
	cp := controlPlane(t)

	same, err := MergeJoin(cp, p, nil)
	if err != nil {
		t.Fatalf("MergeJoin: %v", err)
	}

	// The same proposal again finds every entry identical and keeps it.
	again, err := MergeJoin(same.Body, p, nil)
	if err != nil {
		t.Fatalf("MergeJoin again: %v", err)
	}
	if len(again.KeptTiers) != len(p.Tiers) || len(again.AddedTiers) != 0 {
		t.Errorf("kept %v and added %v, want every tier kept", again.KeptTiers, again.AddedTiers)
	}

	// A proposal whose first tier differs under the same label is refused.
	var changed yaml.Node
	raw, err := yaml.Marshal(p.Tiers[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &changed); err != nil {
		t.Fatal(err)
	}
	tier := changed.Content[0]
	setScalar(tier, "vcpu", "3")
	other := p
	other.Tiers = append([]*yaml.Node{tier}, p.Tiers[1:]...)

	_, err = MergeJoin(same.Body, other, nil)
	if err == nil {
		t.Fatal("a different tier under a label the control plane already has was merged")
	}
	if label := scalarValue(tier, "label"); !strings.Contains(err.Error(), label) || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("the refusal does not name the label %q and say nothing changed: %v", label, err)
	}
}

// A MERGE THE CONTROL PLANE WOULD REFUSE TO LOAD IS NEVER RETURNED, and a
// config that is not a control plane's is refused.
func TestMergeJoinRefusesWhatWouldNotLoad(t *testing.T) {
	t.Parallel()

	p := macProposal(t)

	bad := p
	var broken yaml.Node
	if err := yaml.Unmarshal([]byte("label: billet-broken\nvcpu: -1\n"), &broken); err != nil {
		t.Fatal(err)
	}
	bad.Tiers = []*yaml.Node{broken.Content[0]}
	if _, err := MergeJoin(controlPlane(t), bad, nil); err == nil || !strings.Contains(err.Error(), "does not load") {
		t.Errorf("a merge that does not load was returned: %v", err)
	}

	body, _, err := Generate(dockerParams())
	if err != nil {
		t.Fatal(err)
	}
	joined, err := Join(body, "10.0.0.1:7717", "/etc/billet/tls")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MergeJoin([]byte(joined.Node), p, nil); err == nil || !strings.Contains(err.Error(), "server") {
		t.Errorf("a node-only config was merged into as a control plane's: %v", err)
	}
}

// THE TOKEN CARRIES THE PROPOSAL WHOLE, and anything that is not one billet
// printed is refused rather than merged.
func TestJoinProposalTokenRoundTripsAndRefusesAForgery(t *testing.T) {
	t.Parallel()

	p := macProposal(t)
	p.Deployment = "0123456789abcdef0123456789abcdef"

	token, err := p.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if strings.ContainsAny(token, " \n\t") || !strings.HasPrefix(token, joinTokenPrefix) {
		t.Fatalf("the token is not one shell word with its prefix: %q", token)
	}

	got, err := ParseJoinProposal(token)
	if err != nil {
		t.Fatalf("ParseJoinProposal: %v", err)
	}
	if got.Node != p.Node || got.Provider != p.Provider || got.Deployment != p.Deployment ||
		got.MaxVCPU != p.MaxVCPU || got.MaxMemory != p.MaxMemory || len(got.Tiers) != len(p.Tiers) ||
		len(got.Nodes) != len(p.Nodes) {
		t.Fatalf("the proposal did not survive its token: %+v, want %+v", got, p)
	}
	for i := range p.Tiers {
		if same, err := sameValue(p.Tiers[i], got.Tiers[i]); err != nil || !same {
			t.Errorf("tier %d changed through the token (%v)", i, err)
		}
	}

	doc, err := p.YAML()
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"a truncated token":    token[:len(token)-8],
		"another encoding":     "billet-join-2." + strings.TrimPrefix(token, joinTokenPrefix),
		"an unknown field":     strings.Replace(string(doc), "schema: 1", "schema: 1\nextra: x", 1),
		"another schema":       strings.Replace(string(doc), "schema: 1", "schema: 2", 1),
		"two documents":        string(doc) + "---\nschema: 1\n",
		"no vCPU":              strings.Replace(string(doc), "max_vcpu: "+strconv.Itoa(p.MaxVCPU), "max_vcpu: 0", 1),
		"an unknown provider":  strings.Replace(string(doc), "provider: tart", "provider: mainframe", 1),
		"a tier with no label": strings.Replace(string(doc), "label:", "notlabel:", 1),
	} {
		if text == token || text == string(doc) {
			t.Fatalf("%s: the case changed nothing", name)
		}
		if _, err := ParseJoinProposal(text); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
