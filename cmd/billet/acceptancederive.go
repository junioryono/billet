package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/junioryono/billet/internal/config"

	"gopkg.in/yaml.v3"
)

// The YAML surgery that turns a real config into an isolated acceptance one.
//
// EVERY EDIT HERE IS ONE OF TWO THINGS: a path that must not be shared with the
// deployment this was derived from, or a NAME that must not collide with one
// GitHub or AWS already has. Nothing else is touched — not the provider, not the
// shapes, not the ceilings, not the trust policy — because the point of an
// acceptance run is to exercise the operator's real configuration, and a
// derivation that quietly simplified it would prove something about a deployment
// nobody has.
//
// THROUGH THE NODE TREE, so an operator can read the derived file and recognise
// their own: comments survive, key order survives, and only the keys named below
// differ. A round trip through the parsed struct would rewrite the whole document
// in billet's own shape and fill in every default, which turns "what did this run
// actually use" into a question nobody can answer by looking.

// errSharedLedger is what a base config whose ledger is EXTERNAL gets.
//
// AN ISOLATED IDENTITY IS NOT AN ISOLATED LEDGER, and this is the one place that
// distinction is fatal. With `server.state.backend: postgres` the capacity ledger
// is in a database billet does not operate, named by `dsn_env` — an environment
// VARIABLE. Rewriting `identity_dir` gives the run its own CA and its own
// deployment identity and changes nothing about which database it connects to,
// because the derived config names the same variable and the acceptance process
// inherits the same environment.
//
// WHAT THAT WOULD MEAN is not a confused test run: `billet acceptance down`
// opens that ledger as an operator command and SEALS it. The acceptance run's
// teardown would seal the production deployment.
//
// So it is refused. Deriving a second DSN is not billet's to do — it would mean
// inventing a database, a schema and a credential — and the operator who wants
// an acceptance run against PostgreSQL points the base config at one they made.
var errSharedLedger = errors.New(
	"this config's ledger is in a database billet does not operate, and an acceptance run " +
		"cannot isolate it: server.state.postgres.dsn_env names an environment VARIABLE, so " +
		"the derived deployment would connect to the same database — and `billet acceptance " +
		"down` seals whatever ledger it opens. Give the acceptance run a base config naming " +
		"its own database (a separate schema is enough; point its dsn_env at a variable " +
		"holding that connection string), or run the acceptance against a SQLite base")

// rewriteForAcceptance edits the tree in place and returns the derived labels.
func rewriteForAcceptance(root *yaml.Node, dir, prefix, listen string) ([]string, error) {
	if err := refuseSharedLedger(root); err != nil {
		return nil, err
	}

	// THE SERVER'S STATE, AND BOTH SPELLINGS OF IT. `state_dir` is the shorthand
	// and `identity_dir` is what a config naming its ledger backend writes; a
	// derivation that rewrote only the first would leave a PostgreSQL acceptance
	// deployment pointing its identity at the real one's directory.
	if server := mappingValue(root, "server"); server != nil {
		if err := setStateDir(server, filepath.Join(dir, "server")); err != nil {
			return nil, err
		}

		setScalar(server, "listen", listen)

		// AN ACCEPTANCE RUN NEVER SERVES THE FLEET. A base config binding a real
		// address would have this deployment listening where the real one does, or
		// racing it for the port; loopback is what an isolated run needs and is
		// what `up` picked a port on.
		removeScalar(server, "bootstrap_listen")
		removeScalar(server, "node_tls_hosts")
	}

	if node := mappingValue(root, "node"); node != nil {
		setScalar(node, "state_dir", filepath.Join(dir, "node"))
		setScalar(node, "lock_dir", filepath.Join(dir, "locks"))
		setScalar(node, "server_addr", listen)

		// THE NODE'S NAME IS A FLEET IDENTIFIER, so it is prefixed for the reason
		// tier labels are: the control plane keys placement, custody and the
		// compute barrier on it, and two deployments using one name on one machine
		// is the shape ErrSuperseded exists to complain about.
		prefixScalar(node, "name", prefix)

		// TLS AGAINST A LOOPBACK LISTENER IS A CONFIG ERROR, and the base may well
		// carry it for a real fleet. Removing it here is not a simplification of
		// the thing under test: a loopback wire serves plain HTTP by design, and
		// leaving this would make the derived config refuse to load.
		removeScalar(node, "tls")
	}

	// THE BACKUP DESTINATION IS DROPPED. An acceptance run's archives have no
	// business in the real deployment's bucket — they would be indistinguishable
	// from it's own, under a prefix its retention rule governs — and billet's
	// no-clobber writes mean the collision would surface as a failure rather than
	// as corruption, which is still a failure nobody asked for.
	removeScalar(root, "backup")

	// NEVER ON AUTOMATIC UPDATES. The binary under acceptance is whatever the
	// workflow built, which reports no release, and a fleet whose hosts report no
	// release is not on the channel's target, so the starter would open a rollout
	// to the channel on its first tick and drain the one node the acceptance job
	// needs (measured 2026-09-04 on the recover rehearsal, whose snapshot
	// deployment began moving itself to v0.6.0 a minute after boot). An
	// acceptance run proves the tree it was built from, never the channel; the
	// rest of a `release:` block the base carries is left alone.
	forceScalar(ensureMapping(root, "release"), "automatic", "false")

	labels, err := prefixTierLabels(root, prefix)
	if err != nil {
		return nil, err
	}

	prefixNodeNames(root, prefix)

	return labels, nil
}

// refuseSharedLedger stops a derivation whose ledger this command cannot make
// its own.
//
// ANYTHING BUT AN ABSENT OR SQLITE BACKEND, and the default is deliberate rather
// than lenient: `state_dir` and an absent `state:` both mean SQLite, which the
// derivation DOES isolate by pointing the directory into the workspace. A
// backend billet does not recognise is refused too — an acceptance run must not
// decide that a value it has never heard of is safe to share.
func refuseSharedLedger(root *yaml.Node) error {
	server := mappingValue(root, "server")
	if server == nil {
		return nil
	}

	state := mappingValue(server, "state")
	if state == nil {
		return nil
	}

	backend := mappingValue(state, "backend")
	if backend == nil || backend.Value == string(config.StateSQLite) {
		return nil
	}

	return fmt.Errorf("server.state.backend is %q: %w", backend.Value, errSharedLedger)
}

// setStateDir writes the server's state directory under whichever spelling the
// config already uses.
//
// THE SPELLING IS THE CONFIG'S, NOT THIS FUNCTION'S CHOICE, because the two are
// MUTUALLY EXCLUSIVE at load: writing `state_dir` into a config that carries
// `identity_dir` produces a file billet refuses, and the refusal would name a key
// the operator never typed. A config carrying NEITHER is given the shorthand,
// which is what its absence already meant.
func setStateDir(server *yaml.Node, dir string) error {
	identity := mappingValue(server, "identity_dir")
	shorthand := mappingValue(server, "state_dir")

	if identity != nil && shorthand != nil {
		return errors.New(
			"the base config names both server.state_dir and server.identity_dir, which " +
				"billet refuses at load; fix the base config rather than deriving from it")
	}

	if identity != nil {
		setScalar(server, "identity_dir", dir)

		return nil
	}

	setScalar(server, "state_dir", dir)

	return nil
}

// prefixTierLabels prefixes every tier's label and returns the results.
//
// THE LABEL IS THE SCALE SET, which is why this is the single most load-bearing
// edit in the file. `billet acceptance down` runs `teardown --all`, which deletes
// the scale set of every tier in the config it is given — so if a derived label
// equalled a real one, the teardown would delete the production deployment's
// scale set and every runner registration in it.
func prefixTierLabels(root *yaml.Node, prefix string) ([]string, error) {
	tiers := mappingValue(root, "tiers")
	if tiers == nil || tiers.Kind != yaml.SequenceNode {
		return nil, errors.New(
			"the base config declares no tiers, so an acceptance run derived from it has " +
				"nothing to run a job on")
	}

	// EVERY LABEL THE BASE CONFIG NAMES, captured BEFORE anything is rewritten,
	// because the check below is against the set as the operator wrote it.
	base := make(map[string]bool, len(tiers.Content))

	for _, tier := range tiers.Content {
		if tier.Kind != yaml.MappingNode {
			continue
		}

		if label := mappingValue(tier, "label"); label != nil {
			base[label.Value] = true
		}
	}

	var labels []string

	for _, tier := range tiers.Content {
		if tier.Kind != yaml.MappingNode {
			continue
		}

		label := mappingValue(tier, "label")
		if label == nil || label.Value == "" {
			return nil, errors.New("the base config has a tier with no label")
		}

		derived := prefix + "-" + label.Value

		// PREFIXING IS NOT A PROOF OF DISJOINTNESS, AND THAT IS THE WHOLE POINT OF
		// CHECKING. With base tiers `linux` and `accept-linux`, the default prefix
		// derives `accept-linux` from the first — which IS the second, an existing
		// production scale set. `billet acceptance down` runs `teardown --all`, so
		// the run would delete it along with every runner registration in it.
		//
		// The check is against every base label rather than against this tier's
		// own, because the collision is across tiers and a per-tier comparison
		// cannot see it.
		if base[derived] {
			return nil, fmt.Errorf(
				"the derived tier label %q is already a tier in the base config, so this run "+
					"and that deployment would share a GitHub scale set — and `billet "+
					"acceptance down` deletes every scale set in the config it is given. "+
					"Choose a --label-prefix no existing label starts with", derived)
		}

		label.Value = derived
		label.Tag = ""
		// THE STYLE IS RESET so a label that was quoted for its own reasons does
		// not carry a quoting decision made about different bytes.
		label.Style = 0

		labels = append(labels, derived)

		// A TIER PINNED TO A HOST NAMES THE DERIVED HOST. The node's own name was
		// prefixed above, so a tier still naming the base one pins to a machine
		// that never registers — and a pinned tier whose host never appears
		// advertises nothing, forever, with nothing saying why.
		prefixScalar(tier, "node", prefix)
	}

	if len(labels) == 0 {
		return nil, errors.New("the base config's tiers list is empty")
	}

	return labels, nil
}

// prefixNodeNames prefixes every nodes[] policy entry, which is the other half of
// prefixing node.name: a policy keyed on the old name describes a host that no
// longer exists, and a macOS tier pinned to it would be refused at load.
func prefixNodeNames(root *yaml.Node, prefix string) {
	nodes := mappingValue(root, "nodes")
	if nodes == nil || nodes.Kind != yaml.SequenceNode {
		return
	}

	for _, entry := range nodes.Content {
		if entry.Kind == yaml.MappingNode {
			prefixScalar(entry, "name", prefix)
		}
	}
}

// mappingValue returns the value node for a key, or nil.
//
// THROUGH isKey rather than a string comparison on Value, because that is the
// definition this package already has and it is the correct one: an ANCHOR whose
// value decodes to the key is that key, and a raw comparison misses it. One
// definition for every caller — the same reason githubapp.go's own comment gives.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}

	for i := 0; i+1 < len(m.Content); i += 2 {
		if isKey(m.Content[i], key) {
			return m.Content[i+1]
		}
	}

	return nil
}

// ensureMapping returns the mapping under key, creating an empty one when the
// key is absent. A key holding something other than a mapping (a `null`, or an
// alias of one) is replaced, because the caller is about to write a key into it
// and a scalar there would make the derived config unparseable rather than
// merely different; the comments the operator hung on the old value move to the
// replacement, because this file's contract is that the derivation eats none.
func ensureMapping(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if isKey(m.Content[i], key) {
			if old := m.Content[i+1]; old.Kind != yaml.MappingNode {
				// The comments move to the KEY, because a block mapping has no line
				// of its own for a line comment to render on; the key does.
				key := m.Content[i]
				key.HeadComment = joinComments(key.HeadComment, old.HeadComment)
				key.LineComment = joinComments(key.LineComment, old.LineComment)
				key.FootComment = joinComments(key.FootComment, old.FootComment)
				m.Content[i+1] = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			}

			return m.Content[i+1]
		}
	}

	value := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, value)

	return value
}

// joinComments keeps both comments when both exist.
func joinComments(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "\n" + b
	}
}

// forceScalar sets key to a plain scalar whatever the value node was. setScalar
// rewrites Value, Tag and Style in place, which is right for a scalar and wrong
// for an alias: an AliasNode keeps its Kind and its Alias pointer, renders as a
// reference to whatever the anchor still says, and a boolean written through
// `automatic: *shared` would have come out unchanged or unparseable. Replacing
// the node breaks only this reference; the anchor and every other use of it
// stay as the operator wrote them, and so do the key's comments.
func forceScalar(m *yaml.Node, key, value string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if isKey(m.Content[i], key) {
			old := m.Content[i+1]
			m.Content[i+1] = &yaml.Node{
				Kind:        yaml.ScalarNode,
				Value:       value,
				HeadComment: old.HeadComment,
				LineComment: old.LineComment,
				FootComment: old.FootComment,
			}

			return
		}
	}

	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: value})
}

// prefixScalar prefixes an existing scalar, and does nothing when the key is
// absent — an absent value is one billet defaults, and inventing one here would
// pin something the operator left free.
func prefixScalar(m *yaml.Node, key, prefix string) {
	node := mappingValue(m, key)
	if node == nil || node.Kind != yaml.ScalarNode || strings.TrimSpace(node.Value) == "" {
		return
	}

	node.Value = prefix + "-" + node.Value
	node.Tag = ""
	node.Style = 0
}

// acceptanceScaleSets is what this run owns on GitHub, for a report that has to
// name them without re-deriving the config.
func acceptanceScaleSets(labels []string) string {
	if len(labels) == 0 {
		return "(none)"
	}

	return fmt.Sprintf("%d scale set(s): %s", len(labels), strings.Join(labels, ", "))
}
