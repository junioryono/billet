package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/regularfile"
)

// configChangeSchema versions the answer `billet node config-change` prints.
const configChangeSchema = 1

// configChangeLimit bounds what the command reads of either configuration.
const configChangeLimit = 8 << 20

// The verdicts, a closed vocabulary. A reader treats anything but keep,
// including no answer, as restart: the zero value is the restart.
const (
	configChangeRestart = "restart"
	configChangeKeep    = "keep"
)

// nodeIgnores are the top-level keys of billet.yaml no part of a running node
// reads. The catalogue reaches a node in each launch command, and the policy in
// nodes: is the control plane's; the images refresh reads tiers on its own
// schedule from the file, so a change to either needs no node restart (#183).
var nodeIgnores = []string{"tiers", "nodes"}

// configChangeAnswer is what the command prints.
type configChangeAnswer struct {
	Schema  int    `json:"schema"`
	Verdict string `json:"verdict"`
	Why     string `json:"why"`
}

// NodeConfigChange is `billet node config-change`: whether a node running on
// the installed configuration must restart to run on the candidate.
//
// THE ANSWER IS keep ONLY WHEN IT IS PROVED, and every other case is restart:
// the two files read, both load, neither describes a control plane, and they
// are equal once the keys a node does not read are set aside. A host that also
// runs the control plane always restarts its node, because a retirement holds
// that host's own node to running the file it started with
// (config_changed_since_start), and a node kept across a changed file could
// never satisfy it.
//
// It is how the host role and macos_node decide a converge's node restart,
// which is a drain behind every running job: the role infers nothing from the
// configurations itself, so this is billet's word on what billet reads.
func NodeConfigChange(_ context.Context, env cli.Env, args []string) error {
	flags := cli.NewFlagSet("billet node config-change", env.Stdout)
	installed := flags.String("installed", "", "the configuration the running node started on (required)")
	candidate := flags.String("candidate", "", "the configuration about to replace it (required)")
	asJSON := flags.Bool("json", false, "print the answer as JSON (the only form)")

	if err := cli.Parse(flags, args); err != nil {
		return err
	}

	if !*asJSON {
		return errors.New("billet node config-change: pass --json; the answer is for the roles that converge a node")
	}
	if *installed == "" || *candidate == "" {
		return errors.New("billet node config-change: --installed and --candidate are both required")
	}

	verdict, why := judgeNodeConfigChange(*installed, *candidate)

	out, err := json.Marshal(configChangeAnswer{Schema: configChangeSchema, Verdict: verdict, Why: why})
	if err != nil {
		return fmt.Errorf("billet node config-change: %w", err)
	}

	_, err = fmt.Fprintf(env.Stdout, "%s\n", out)

	return err
}

// judgeNodeConfigChange decides the verdict and says why.
func judgeNodeConfigChange(installedPath, candidatePath string) (string, string) {
	installed, err := readNodeView(installedPath)
	if err != nil {
		return configChangeRestart, fmt.Sprintf("the installed configuration: %v", err)
	}

	candidate, err := readNodeView(candidatePath)
	if err != nil {
		return configChangeRestart, fmt.Sprintf("the candidate configuration: %v", err)
	}

	if installed.server || candidate.server {
		return configChangeRestart, "this host runs the control plane too, and its node is held to " +
			"running the file it started with"
	}

	if changed := changedKeys(installed.doc, candidate.doc); len(changed) > 0 {
		return configChangeRestart, fmt.Sprintf("what the node reads changed: %v", changed)
	}

	return configChangeKeep, fmt.Sprintf("only %v changed, which no running node reads", nodeIgnores)
}

// nodeView is one configuration as the comparison sees it.
type nodeView struct {
	doc    map[string]any
	server bool
}

// readNodeView reads a configuration, proves it loads, and keeps its document
// without the keys a node does not read.
func readNodeView(path string) (nodeView, error) {
	raw, err := regularfile.ReadFile(path, configChangeLimit, regularfile.Options{})
	if err != nil {
		return nodeView{}, err
	}

	cfg, err := config.Parse(path, raw)
	if err != nil {
		return nodeView{}, fmt.Errorf("it does not load: %w", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nodeView{}, fmt.Errorf("it is not a YAML mapping: %w", err)
	}

	for _, key := range nodeIgnores {
		delete(doc, key)
	}

	return nodeView{doc: doc, server: cfg.Server != nil}, nil
}

// changedKeys names the top-level keys whose values differ, sorted.
func changedKeys(a, b map[string]any) []string {
	var out []string

	for key, av := range a {
		if bv, ok := b[key]; !ok || !reflect.DeepEqual(av, bv) {
			out = append(out, key)
		}
	}

	for key := range b {
		if _, ok := a[key]; !ok {
			out = append(out, key)
		}
	}

	slices.Sort(out)

	return out
}
