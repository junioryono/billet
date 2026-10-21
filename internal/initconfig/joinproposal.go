package initconfig

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/junioryono/billet/internal/config"
)

// THE CONTROL PLANE'S HALF OF A JOIN, CARRIED AS ONE LINE (#183). A joining
// node knows what the control plane needs for it to be useful: its tiers, its
// nodes: policy entry and what it contributes, by which the deployment ceiling
// has to rise. That used to be printed as YAML for an operator to merge into
// another machine's file by hand, and a hand merge is where a node's share was
// once left out of the ceiling. The proposal is the same facts as data: printed
// as a token the operator carries across with the join token, and applied on
// the control plane by `billet join accept`.

// joinTokenPrefix names the proposal's encoding and version.
const joinTokenPrefix = "billet-join-1."

// joinProposalSchema is the proposal document's version.
const joinProposalSchema = 1

// maxJoinProposal bounds what a proposal may decompress to.
const maxJoinProposal = 1 << 20

// JoinProposal is what a joining node proposes to the control plane.
type JoinProposal struct {
	// Node is the joining node's name: empty when the generation does not know
	// it (it comes from a certificate not yet issued), and then the ceiling is
	// raised as for a node that has never registered.
	Node string
	// Provider and Site describe it, for the operator reading the proposal.
	Provider config.ProviderKind
	Site     string
	// Deployment is the control plane's deployment id as the node learned it
	// enrolling, or empty when the proposal was made without enrolling.
	Deployment string
	// MaxVCPU and MaxMemory are what the node contributes.
	MaxVCPU   int
	MaxMemory config.ByteSize
	// Tiers and Nodes are the entries for the control plane's tiers: and
	// nodes:, each a mapping exactly as the generation wrote it.
	Tiers []*yaml.Node
	Nodes []*yaml.Node
}

// joinProposalDoc is the proposal's encoding.
type joinProposalDoc struct {
	Schema     int         `yaml:"schema"`
	Node       string      `yaml:"node,omitempty"`
	Provider   string      `yaml:"provider"`
	Site       string      `yaml:"site,omitempty"`
	Deployment string      `yaml:"deployment,omitempty"`
	MaxVCPU    int         `yaml:"max_vcpu"`
	MaxMemory  string      `yaml:"max_memory"`
	Tiers      []yaml.Node `yaml:"tiers,omitempty"`
	Nodes      []yaml.Node `yaml:"nodes,omitempty"`
}

// Token encodes the proposal as one line: the prefix, then base64url of the
// gzipped YAML, so it survives a terminal, a chat message and a shell argument.
func (p JoinProposal) Token() (string, error) {
	raw, err := p.YAML()
	if err != nil {
		return "", err
	}

	var z bytes.Buffer

	w, err := gzip.NewWriterLevel(&z, gzip.BestCompression)
	if err != nil {
		return "", fmt.Errorf("encode the join proposal: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return "", fmt.Errorf("encode the join proposal: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("encode the join proposal: %w", err)
	}

	return joinTokenPrefix + base64.RawURLEncoding.EncodeToString(z.Bytes()), nil
}

// YAML is the proposal as a document a person can read.
func (p JoinProposal) YAML() ([]byte, error) {
	doc := joinProposalDoc{
		Schema: joinProposalSchema, Node: p.Node, Provider: string(p.Provider), Site: p.Site,
		Deployment: p.Deployment, MaxVCPU: p.MaxVCPU, MaxMemory: p.MaxMemory.String(),
	}
	for _, t := range p.Tiers {
		doc.Tiers = append(doc.Tiers, *t)
	}
	for _, n := range p.Nodes {
		doc.Nodes = append(doc.Nodes, *n)
	}

	var out bytes.Buffer

	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("encode the join proposal: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode the join proposal: %w", err)
	}

	return out.Bytes(), nil
}

// ParseJoinProposal reads a proposal from its token or its YAML document.
//
// EVERY FIELD IS CHECKED, because the proposal crossed from another machine
// and is about to be merged into the control plane's own configuration: an
// unknown key, a second document, a schema this build does not know, a tier
// without a label or a node entry without a name are each refused here.
func ParseJoinProposal(text string) (JoinProposal, error) {
	text = strings.TrimSpace(text)

	raw := []byte(text)
	if strings.HasPrefix(text, joinTokenPrefix) {
		z, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(text, joinTokenPrefix))
		if err != nil {
			return JoinProposal{}, fmt.Errorf("the join proposal is not one billet printed (base64): %w", err)
		}

		r, err := gzip.NewReader(bytes.NewReader(z))
		if err != nil {
			return JoinProposal{}, fmt.Errorf("the join proposal is not one billet printed (gzip): %w", err)
		}

		raw, err = io.ReadAll(io.LimitReader(r, maxJoinProposal+1))
		if err != nil {
			return JoinProposal{}, fmt.Errorf("the join proposal is not one billet printed (gzip): %w", err)
		}
		if len(raw) > maxJoinProposal {
			return JoinProposal{}, fmt.Errorf("the join proposal is larger than %d bytes", maxJoinProposal)
		}
	} else if strings.HasPrefix(text, "billet-join-") {
		return JoinProposal{}, errors.New("the join proposal is from another billet's encoding; " +
			"print it again with this billet's `billet join`")
	}

	var doc joinProposalDoc

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return JoinProposal{}, fmt.Errorf("the join proposal does not parse: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return JoinProposal{}, errors.New("the join proposal holds more than one document")
	}

	if doc.Schema != joinProposalSchema {
		return JoinProposal{}, fmt.Errorf("the join proposal's schema is %d and this billet reads %d; "+
			"print it again with this billet", doc.Schema, joinProposalSchema)
	}

	p := JoinProposal{
		Node: doc.Node, Provider: config.ProviderKind(doc.Provider), Site: doc.Site,
		Deployment: doc.Deployment, MaxVCPU: doc.MaxVCPU,
	}

	if !p.Provider.Valid() {
		return JoinProposal{}, fmt.Errorf("the join proposal names provider %q", doc.Provider)
	}
	if p.Node != "" {
		if err := config.ValidateNodeName("the join proposal's node", p.Node); err != nil {
			return JoinProposal{}, err
		}
	}
	if p.MaxVCPU <= 0 {
		return JoinProposal{}, fmt.Errorf("the join proposal contributes %d vCPU", p.MaxVCPU)
	}

	memory, err := config.ParseByteSize(doc.MaxMemory)
	if err != nil || memory <= 0 {
		return JoinProposal{}, fmt.Errorf("the join proposal contributes memory %q", doc.MaxMemory)
	}
	p.MaxMemory = memory

	for i := range doc.Tiers {
		t := &doc.Tiers[i]
		if t.Kind != yaml.MappingNode || scalarValue(t, "label") == "" {
			return JoinProposal{}, fmt.Errorf("tier %d of the join proposal has no label", i)
		}
		p.Tiers = append(p.Tiers, t)
	}

	for i := range doc.Nodes {
		n := &doc.Nodes[i]
		if n.Kind != yaml.MappingNode || scalarValue(n, "name") == "" {
			return JoinProposal{}, fmt.Errorf("nodes entry %d of the join proposal has no name", i)
		}
		p.Nodes = append(p.Nodes, n)
	}

	return p, nil
}

// scalarValue is a mapping's scalar value at key, or "".
func scalarValue(m *yaml.Node, key string) string {
	if v := mappingValue(m, key); v != nil && v.Kind == yaml.ScalarNode {
		return v.Value
	}

	return ""
}

// proposalFrom builds a join's proposal from the generation's root, before the
// join removes anything from it.
func proposalFrom(root *yaml.Node, maxVCPU, maxMemory string) (JoinProposal, error) {
	node := mappingValue(root, "node")
	if node == nil {
		return JoinProposal{}, errors.New("the generated config has no node to propose")
	}

	vcpu := 0
	if _, err := fmt.Sscanf(maxVCPU, "%d", &vcpu); err != nil || vcpu <= 0 {
		return JoinProposal{}, fmt.Errorf("the generated ceiling %q is not a vCPU count", maxVCPU)
	}

	memory, err := config.ParseByteSize(maxMemory)
	if err != nil {
		return JoinProposal{}, fmt.Errorf("the generated ceiling %q: %w", maxMemory, err)
	}

	p := JoinProposal{
		Node:      scalarValue(node, "name"),
		Provider:  config.ProviderKind(scalarValue(node, "provider")),
		Site:      scalarValue(node, "site"),
		MaxVCPU:   vcpu,
		MaxMemory: memory,
	}

	if tiers := mappingValue(root, "tiers"); tiers != nil {
		p.Tiers = append(p.Tiers, tiers.Content...)
	}
	if nodes := mappingValue(root, "nodes"); nodes != nil {
		p.Nodes = append(p.Nodes, nodes.Content...)
	}

	return p, nil
}
