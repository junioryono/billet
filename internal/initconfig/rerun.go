package initconfig

import (
	"bytes"
	"fmt"

	"github.com/junioryono/billet/internal/config"

	"gopkg.in/yaml.v3"
)

// A RE-RUN OF `billet init` MUST NEVER SILENTLY DROP OPERATOR CONTENT. The
// generated file is meant to be edited — provider ordering, sites, bridges,
// a capped ceiling, a pinned image, even a comment explaining why — and a
// wholesale rewrite is how a working deployment loses the only record of
// those decisions. So a re-run decides between exactly two safe moves:
// REGENERATE, allowed only when the existing file is provably identical to
// what THIS run would generate (nothing can be lost, because nothing
// differs); and WRITE BESIDE, which leaves the original untouched and puts
// the fresh generation at <path>.new for the operator to merge.
//
// The proof compares the existing file against the NEW generation — never
// against a reconstruction of the old parameters. A reconstruction reads
// values back from the file, which makes the comparison blind to in-place
// edits of exactly those values: a lowered ceiling reconstructs to itself,
// compares equal, and converge would then rewrite it from fresh detection —
// silently doubling the budget an operator deliberately capped. Comparing
// against the new generation makes "the machine changed", "the flags
// changed" and "the operator edited it" all land on the same conservative
// side, which is correct because they are indistinguishable from here.
//
// The comparison is over CANONICALIZED BYTES, not parsed structs: both sides
// round-trip through the same yaml.Node encoder (the one `github-app create`
// already rewrites configs with), with only the App identity neutralized. A
// struct comparison is blind to comments and to explicitly-written defaults,
// and an operator's comment is operator content like any other.

// ReRun is what an init re-run may do to the existing file.
type ReRun int

const (
	// WriteBeside: the existing file differs from what this run generates;
	// the fresh generation goes to <path>.new. The default whenever equality
	// cannot be PROVED.
	WriteBeside ReRun = iota
	// Regenerate: the existing file is byte-equivalent (canonicalized, App
	// identity aside) to what this run would write, so replacing it loses
	// nothing — not even a comment.
	Regenerate
)

// PlanReRun decides which move an init re-run may make, comparing the
// existing file's contents against the fresh generation for this run's
// parameters.
func PlanReRun(existing []byte, fresh string) ReRun {
	a, err := canonicalize(existing)
	if err != nil {
		return WriteBeside
	}

	b, err := canonicalize([]byte(fresh))
	if err != nil {
		return WriteBeside
	}

	if !bytes.Equal(a, b) {
		return WriteBeside
	}

	return Regenerate
}

// canonicalize renders a config through the node encoder with the App
// identity neutralized: app_id and installation_id set to 0 and client_id
// removed, because those are what `github-app create` fills in and the one
// difference a converge must tolerate. Everything else — values, field
// presence, ordering, comments — survives into the compared bytes.
func canonicalize(raw []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("empty document")
	}

	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("not a mapping")
	}

	if gh := mappingValue(root, "github"); gh != nil && gh.Kind == yaml.MappingNode {
		setMappingScalar(gh, "app_id", "0")
		setMappingScalar(gh, "installation_id", "0")
		removeMappingKey(gh, "client_id")
	}

	var out bytes.Buffer

	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)

	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}

	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("render: %w", err)
	}

	return out.Bytes(), nil
}

// mappingValue returns the value node for a top-level key, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}

	return nil
}

// setMappingScalar overwrites a key's scalar value when the key exists;
// canonicalization must never ADD structure, only neutralize what is there.
func setMappingScalar(m *yaml.Node, key, value string) {
	if v := mappingValue(m, key); v != nil && v.Kind == yaml.ScalarNode {
		v.Value = value
		v.Tag = ""
		v.Style = 0
	}
}

// removeMappingKey drops a key and its value — UNLESS either carries a
// comment. `github-app create` adds client_id without one, so a comment there
// is operator content; removing it would let canonicalization declare
// equality and converge would then re-add the line without the note. Keeping
// the commented node makes the sides differ, which lands beside — the
// conservative answer.
func removeMappingKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			continue
		}

		k, v := m.Content[i], m.Content[i+1]
		if k.HeadComment != "" || k.LineComment != "" || k.FootComment != "" ||
			v.HeadComment != "" || v.LineComment != "" || v.FootComment != "" {
			return
		}

		m.Content = append(m.Content[:i], m.Content[i+2:]...)

		return
	}
}

// ServerStateDir is where a generation with these parameters points its
// control-plane state — what the deployment-identity refusal compares.
func (p Params) ServerStateDir() string {
	q := p
	if q.Profile == "" {
		q.Profile = ProfileLocal
	}

	return q.paths().serverState
}

// ExistingServerStateDir reads the directory holding an existing config's
// deployment identity, leniently. ok is false only when the file is not YAML at
// all — the caller must fail CLOSED there, because "cannot read it" is not
// "there is no deployment to protect". An ABSENT key reports the same default
// the config layer fills in: a running deployment that omitted it keeps its
// state exactly there.
//
// IT READS BOTH SPELLINGS, and must. `state_dir` is the shorthand and is what
// almost every file says; `identity_dir` is what a config naming its ledger
// backend explicitly writes instead, and a deployment whose ledger is in
// PostgreSQL has ONLY that one. Reading `state_dir` alone would report the
// default directory for such a config — and this answer is what protects an
// existing deployment's CA and identity from being written over.
func ExistingServerStateDir(body []byte) (string, bool) {
	var doc struct {
		Server *struct {
			IdentityDir string `yaml:"identity_dir"`
			StateDir    string `yaml:"state_dir"`
		} `yaml:"server"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return "", false
	}

	if doc.Server == nil {
		// No server section: the file describes a standalone node, and there
		// is no control-plane state to protect.
		return "", true
	}

	// identity_dir wins when both are present. The config layer refuses that
	// combination outright, so reaching it here means a file billet would not
	// load — and the safe answer for a directory that must not be written over
	// is the one that names an identity.
	if dir := doc.Server.IdentityDir; dir != "" {
		return dir, true
	}

	if doc.Server.StateDir == "" {
		return config.DefaultServerStateDir(), true
	}

	return doc.Server.StateDir, true
}
