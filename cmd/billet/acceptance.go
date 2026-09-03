package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awssts"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"

	"gopkg.in/yaml.v3"
)

// `billet acceptance` runs the procedure docs/aws-acceptance.md used to describe
// by hand: stand a deployment up beside whatever else is on the machine, let a
// real GitHub Actions job run on it, record what happened, and tear it down
// leaving nothing billable.
//
// WHAT MAKES IT SAFE IS DERIVED, NOT IMPROVISED. A fresh state directory mints
// its own state.DeploymentID, and that identity already scopes every List, every
// Destroy and every cloud tag billet writes — so an acceptance run cannot
// discover or delete another deployment's compute by construction rather than by
// this command being careful. What this command adds on top is three refusals
// against the ways an operator could point it somewhere real: a workspace that
// already holds a DIFFERENT identity, a workspace inside the base config's own
// state directory, and an AWS credential in an account they did not name.
//
// THE TIER LABELS ARE PREFIXED, which is the other half. A tier's label is its
// GitHub scale set, so an unprefixed acceptance tier would share a scale set with
// production's — and `billet teardown --all` would then delete the one that
// matters. Prefixing makes the scale sets disjoint, which makes the teardown
// disjoint.
//
// WHAT IT CANNOT ISOLATE, AND SAYS SO: the GitHub App and the organization. There
// is one App per deployment identity in billet's model but one App per ORG in
// practice, and minting a second for every acceptance run is not something a test
// harness should do — an App's private key is issued exactly once. So the run
// shares the App, and everything else about it is its own.

const (
	// acceptanceRecord is the workspace's own file: what this run was told to be,
	// and the identity it turned out to have.
	acceptanceRecord = "acceptance.json"
	// acceptanceConfig is the derived config, inside the workspace, so the
	// services started against it cannot be pointed at anything else by accident.
	acceptanceConfig = "billet.yaml"
	// acceptanceEvidence is where `evidence` writes by default.
	acceptanceEvidence = "evidence.json"

	// defaultLabelPrefix is what an acceptance tier's label is prefixed with. It
	// is short and unmistakable: an operator reading `runs-on` in a workflow, or a
	// scale set in GitHub's UI, should not have to wonder.
	defaultLabelPrefix = "accept"

	// acceptanceJobLimit bounds the evidence read. An acceptance ledger is minutes
	// old and holds the jobs this run caused; the bound is here because nothing
	// about a table's size is a report's to assume.
	acceptanceJobLimit = 500
)

// errWrongAccount is what an account assertion that FAILED produces, as opposed
// to one that could not be made. The two are different answers and a run must
// not treat them alike: one says the operator is pointed at the wrong place, the
// other says billet could not find out.
var errWrongAccount = errors.New("the AWS credential is in a different account")

// acceptanceWorkspace is the workspace's durable record.
//
// IT EXISTS SO `down` CAN BE RUN LATER, from a different shell, after a failure,
// by a person who no longer remembers what `up` was told — which is the case that
// matters, because a failed acceptance run is exactly when compute is left
// running. Everything `down` needs is here.
type acceptanceWorkspace struct {
	// Version is the record's schema. Read before anything else, and a value this
	// binary does not know is refused rather than partly understood.
	Version int `json:"version"`

	CreatedAt string `json:"created_at"`
	// BaseConfig is the config this run was derived FROM, recorded for the report
	// rather than read again — a base that changed underneath a run does not
	// retroactively change what the run did.
	BaseConfig string `json:"base_config"`
	// ConfigPath is the derived config every later subcommand acts on.
	ConfigPath string `json:"config_path"`
	// LabelPrefix is what every tier label carries, so a report can say which
	// scale sets this run owns without re-deriving them.
	LabelPrefix string `json:"label_prefix"`
	// Listen is the loopback address the derived deployment uses.
	Listen string `json:"listen"`
	// Account is the AWS account this run asserted, empty when none was named.
	Account string `json:"account,omitempty"`
	// CallerARN is who the credential turned out to be. Reported, never matched:
	// the question a person asks after "was it the right account" is "which
	// principal did the work".
	CallerARN string `json:"caller_arn,omitempty"`
	// DeploymentID is the identity every cloud resource this run creates is tagged
	// with. Written by `up` once the state directory has minted one, and it is
	// what makes a later `down` provably about THIS deployment.
	DeploymentID string `json:"deployment_id,omitempty"`
	// Tiers are the derived labels, so the report names them without the config.
	Tiers []string `json:"tiers,omitempty"`

	// ConfigSHA256 is the digest of the derived config as `up` wrote it.
	//
	// BECAUSE PROVING THE IDENTITY PROVES THE WRONG THING. Every destructive step
	// reads ws.ConfigPath — `down` deletes the scale set of every tier in it —
	// while the identity check only proves that <workspace>/server holds the
	// recorded deployment. Replace the derived config with the PRODUCTION one and
	// the identity check still passes, after which `teardown --all` deletes the
	// production scale sets and `decommission` scopes cloud deletion from that
	// config's identity rather than the one just proved. The digest is what makes
	// the file part of what was proved.
	ConfigSHA256 string `json:"config_sha256,omitempty"`

	// Created says billet made the workspace DIRECTORY, rather than adopting one
	// that already existed. Only a directory billet created is removed — see
	// removeAcceptanceWorkspace — because `down` ends in os.RemoveAll and an
	// operator who pointed --workspace at a directory holding something else
	// would otherwise lose it.
	Created bool `json:"created"`
}

// acceptanceRecordVersion is the record schema this binary writes and the only
// one it reads.
//
// A CLOSED NUMBER RATHER THAN A TOLERANT PARSE, for the reason the host-upgrade
// journal is: every later subcommand acts on this file, and a reader that half
// understood a newer one would tear down what it could see and leave the rest.
const acceptanceRecordVersion = 1

func cmdAcceptance(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("billet acceptance needs a subcommand: up, run, evidence, down or sweep")
	}

	switch args[0] {
	case "up":
		return cmdAcceptanceUp(ctx, args[1:])
	case "run":
		return cmdAcceptanceRun(ctx, args[1:])
	case "evidence":
		return cmdAcceptanceEvidence(ctx, args[1:])
	case "down":
		return cmdAcceptanceDown(ctx, args[1:])
	case "sweep":
		return cmdAcceptanceSweep(ctx, args[1:])
	default:
		return fmt.Errorf("billet acceptance %s: unknown subcommand; it is up, run, evidence, "+
			"down or sweep", args[0])
	}
}

// cmdAcceptanceUp derives the isolated deployment and writes the workspace.
func cmdAcceptanceUp(ctx context.Context, args []string) error {
	fs := newFlagSet("billet acceptance up")
	base := fs.String("config", defaultConfigPath(),
		"the config to derive an isolated acceptance deployment FROM; it is read, never written")
	workspace := fs.String("workspace", "",
		"a directory this run owns entirely: the derived config, both state directories, "+
			"the locks and the evidence")
	account := fs.String("account", "",
		"the AWS account this run must be in; refused if the credential says otherwise "+
			"(omit to skip the assertion, which also skips finding out)")
	prefix := fs.String("label-prefix", defaultLabelPrefix,
		"what every derived tier label carries, so this run's scale sets are disjoint "+
			"from the ones the base config names")
	region := fs.String("region", "",
		"the region to ask sts:GetCallerIdentity in (default: the config's own)")

	if err := parse(fs, args); err != nil {
		return err
	}

	ws, err := deriveAcceptance(ctx, acceptanceInputs{
		base:      *base,
		workspace: *workspace,
		account:   *account,
		prefix:    *prefix,
		region:    *region,
		creds:     awscreds.Default(),
	})
	if err != nil {
		return err
	}

	printAcceptanceUp(ws)

	return nil
}

// acceptanceInputs is what `up` was told.
type acceptanceInputs struct {
	base      string
	workspace string
	account   string
	prefix    string
	region    string
	creds     awscreds.Source
	// stsEndpoint points the account assertion at a test server. Empty is the
	// real regional endpoint.
	stsEndpoint string
}

// deriveAcceptance is `up` without the printing, so a test can reach it.
func deriveAcceptance(ctx context.Context, in acceptanceInputs) (acceptanceWorkspace, error) {
	if strings.TrimSpace(in.workspace) == "" {
		return acceptanceWorkspace{}, errors.New(
			"--workspace is required: an acceptance run owns a directory entirely, and " +
				"defaulting it would mean picking one on the operator's behalf that their " +
				"real deployment might already be using")
	}

	if err := checkLabelPrefix(in.prefix); err != nil {
		return acceptanceWorkspace{}, err
	}

	dir, err := filepath.Abs(in.workspace)
	if err != nil {
		return acceptanceWorkspace{}, fmt.Errorf("resolve --workspace: %w", err)
	}

	raw, err := os.ReadFile(in.base)
	if err != nil {
		return acceptanceWorkspace{}, fmt.Errorf("read the base config: %w", err)
	}

	// LOADED AS WELL AS READ. The BYTES are what is rewritten — a round trip
	// through the struct would drop every comment an operator wrote — but the
	// LOADED config is what says where the base deployment keeps its state and
	// which region to ask about, and it is also the proof that the file billet is
	// about to derive from is one billet can read at all.
	cfg, err := config.Load(in.base)
	if err != nil {
		return acceptanceWorkspace{}, fmt.Errorf("read the base config: %w", err)
	}

	// THE BASE'S OWN STATE DIRECTORY IS THE ONE PLACE THIS MUST NOT BE. A
	// workspace inside it — or equal to it — would put the derived deployment's
	// ledger, identity and CA under a directory a live control plane owns, and
	// the acceptance teardown would then be acting on the real deployment's
	// identity rather than on one it minted. Checked before anything is created.
	if err := refuseWorkspaceInsideTheDeployment(dir, cfg); err != nil {
		return acceptanceWorkspace{}, err
	}

	// AN EXISTING WORKSPACE IS RESUMED, NOT REPLACED, and one holding a DIFFERENT
	// deployment is refused. `up` is run again by an operator re-driving a failed
	// run, and silently minting a second identity in the same directory would
	// leave the first run's compute owned by an identity nothing points at any
	// more — which is precisely the state `down` exists to prevent.
	existing, found, err := readAcceptanceWorkspace(dir)
	if err != nil {
		return acceptanceWorkspace{}, err
	}

	created := true
	if found {
		created = existing.Created
	}

	if !found {
		// A DIRECTORY BILLET MADE, OR AN EMPTY ONE. `down` ends in os.RemoveAll,
		// so adopting a directory that already held something means a successful
		// run destroys it — and "it has no acceptance.json in it" is not consent
		// to delete somebody's files. An empty directory is fine and is what a
		// `mktemp -d` gives.
		if err := requireEmptyWorkspace(dir); err != nil {
			return acceptanceWorkspace{}, err
		}
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return acceptanceWorkspace{}, fmt.Errorf("create the workspace: %w", err)
	}

	// AND A RESUME MUST BE THE SAME RUN. Re-running `up` with a different base
	// config or a different prefix keeps the identity and overwrites both the
	// config and the record — after which `down` knows only the NEW labels, and
	// the scale sets the first attempt created are unreachable by anything on
	// this machine. Refused, naming both, because the way out is a fresh
	// workspace rather than a guess about which run to clean up.
	if found {
		if err := refuseChangedRun(dir, existing, in); err != nil {
			return acceptanceWorkspace{}, err
		}
	}

	listen := ""
	if found {
		listen = existing.Listen
	}

	if listen == "" {
		if listen, err = pickLoopbackAddress(ctx); err != nil {
			return acceptanceWorkspace{}, err
		}
	}

	derived, labels, err := deriveAcceptanceConfig(in.base, raw, dir, in.prefix, listen)
	if err != nil {
		return acceptanceWorkspace{}, err
	}

	ws := acceptanceWorkspace{
		Version:     acceptanceRecordVersion,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		BaseConfig:  in.base,
		ConfigPath:  filepath.Join(dir, acceptanceConfig),
		LabelPrefix: in.prefix,
		Listen:      listen,
		Account:     in.account,
		Tiers:       labels,
	}

	ws.Created = created

	if found {
		ws.CreatedAt = existing.CreatedAt
		ws.DeploymentID = existing.DeploymentID
	}

	// THE ACCOUNT ASSERTION RUNS BEFORE THE IDENTITY IS MINTED, so a run pointed
	// at the wrong credential leaves nothing behind at all — not even a state
	// directory that a later `down` would have to reason about.
	if in.account != "" {
		id, err := assertAWSAccount(ctx, in, cfg)
		if err != nil {
			return acceptanceWorkspace{}, err
		}

		ws.CallerARN = id.ARN
	}

	if err := os.WriteFile(ws.ConfigPath, derived, 0o600); err != nil {
		return acceptanceWorkspace{}, fmt.Errorf("write the derived config: %w", err)
	}

	ws.ConfigSHA256 = fmt.Sprintf("%x", sha256.Sum256(derived))

	// MINTED HERE, so the record carries the identity every later subcommand
	// compares against. state.DeploymentID creates one if the directory has none
	// and returns the existing one otherwise, which is what makes `up` resumable.
	serverDir := filepath.Join(dir, "server")
	if err := os.MkdirAll(serverDir, 0o700); err != nil {
		return acceptanceWorkspace{}, fmt.Errorf("create the state directory: %w", err)
	}

	deployment, err := state.DeploymentID(serverDir)
	if err != nil {
		return acceptanceWorkspace{}, fmt.Errorf("mint this run's deployment identity: %w", err)
	}

	// AND REFUSED IF IT MOVED. A workspace whose recorded identity is not the one
	// its state directory holds is a directory two runs have used, and a teardown
	// scoped to either is wrong about the other's compute.
	if ws.DeploymentID != "" && ws.DeploymentID != deployment {
		return acceptanceWorkspace{}, fmt.Errorf(
			"%s records deployment %s but its state directory holds %s. Two acceptance runs "+
				"have used this workspace, so a teardown scoped to either is wrong about the "+
				"other's compute. Use a fresh --workspace",
			filepath.Join(dir, acceptanceRecord), ws.DeploymentID, deployment)
	}

	ws.DeploymentID = deployment

	if err := writeAcceptanceWorkspace(dir, ws); err != nil {
		return acceptanceWorkspace{}, err
	}

	return ws, nil
}

// requireEmptyWorkspace refuses a directory that already holds something.
//
// BECAUSE A SUCCESSFUL TEARDOWN ENDS IN os.RemoveAll. An operator who points
// --workspace at a directory with anything else in it loses all of it, and
// "there is no acceptance.json here" is not consent to delete their files. An
// absent directory and an empty one are both fine, and an empty one is what
// `mktemp -d` gives.
func requireEmptyWorkspace(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("inspect the workspace: %w", err)
	}

	if len(entries) == 0 {
		return nil
	}

	return fmt.Errorf(
		"--workspace %s already holds %d entr(ies) and no acceptance record. A successful "+
			"teardown REMOVES this directory, so billet will not adopt one whose contents it "+
			"did not create. Use an empty directory or a new one",
		dir, len(entries))
}

// refuseChangedRun stops a resume that is not the same run.
//
// RE-RUNNING `up` IS ORDINARY — an operator re-driving a failed attempt — and it
// keeps the deployment identity, which is what makes it safe. What is NOT safe is
// re-running it with a different base config or a different prefix: the record
// and the derived config are overwritten, so `down` afterwards knows only the new
// tier labels and the new backend, and whatever the first attempt created is
// unreachable by anything on this machine.
//
// REFUSED RATHER THAN MERGED, because there is no answer to "which run should
// this clean up". The way out is `down` on this workspace, then a fresh one.
func refuseChangedRun(dir string, existing acceptanceWorkspace, in acceptanceInputs) error {
	base, err := filepath.Abs(in.base)
	if err != nil {
		base = in.base
	}

	existingBase, err := filepath.Abs(existing.BaseConfig)
	if err != nil {
		existingBase = existing.BaseConfig
	}

	switch {
	case existingBase != base:
		return fmt.Errorf(
			"%s was derived from %s and this run says %s. Re-deriving would replace the "+
				"record that describes what the first attempt created, leaving its scale sets "+
				"and compute unreachable. Tear this workspace down first, or use a new one",
			dir, existing.BaseConfig, in.base)

	case existing.LabelPrefix != in.prefix:
		return fmt.Errorf(
			"%s owns the tier labels prefixed %q and this run says %q. Re-deriving would "+
				"leave the %q scale sets with nothing pointing at them. Tear this workspace "+
				"down first, or use a new one",
			dir, existing.LabelPrefix, in.prefix, existing.LabelPrefix)
	}

	return nil
}

// checkLabelPrefix refuses a prefix that would not separate anything.
//
// AN EMPTY PREFIX IS THE DANGEROUS ONE: the derived tiers would carry the base
// config's own labels, so they would be the SAME GitHub scale sets — and
// `billet acceptance down` runs `teardown --all`, which would then delete the
// production deployment's scale sets. The character rule is the config layer's
// own, because a label that config refuses is a generation that cannot load.
func checkLabelPrefix(prefix string) error {
	if strings.TrimSpace(prefix) == "" {
		return errors.New(
			"--label-prefix cannot be empty: it is what makes this run's GitHub scale sets " +
				"disjoint from the ones the base config names, and without it the acceptance " +
				"teardown would delete the real deployment's scale sets")
	}

	if prefix != strings.TrimSpace(prefix) {
		return fmt.Errorf("--label-prefix %q begins or ends with whitespace, and it is "+
			"joined to a label GitHub matches exactly", prefix)
	}

	if strings.ContainsAny(prefix, " \t\n,") {
		return fmt.Errorf("--label-prefix %q contains whitespace or a comma; a runner label "+
			"carries neither", prefix)
	}

	return nil
}

// refuseWorkspaceInsideTheDeployment keeps an acceptance run out of the state
// directory of the deployment it was derived from.
func refuseWorkspaceInsideTheDeployment(dir string, cfg *config.Config) error {
	for _, occupied := range deploymentStateDirs(cfg) {
		if occupied == "" {
			continue
		}

		abs, err := filepath.Abs(occupied)
		if err != nil {
			continue
		}

		// EQUAL OR BENEATH. `filepath.Rel` answering something that does not start
		// with ".." is the containment test, and it is done on cleaned absolute
		// paths so `/var/lib/billet/../billet/server` is not a way around it.
		rel, err := filepath.Rel(abs, dir)
		if err != nil {
			continue
		}

		if rel == "." || !strings.HasPrefix(rel, "..") {
			return fmt.Errorf(
				"--workspace %s is inside %s, which is the state directory the base config "+
					"names. An acceptance run mints its own deployment identity and then "+
					"destroys everything carrying it; run from there and that identity, and "+
					"that teardown, would be the real deployment's. Use a directory of its own",
				dir, abs)
		}
	}

	return nil
}

// pickLoopbackAddress asks the kernel for a free port on the loopback.
//
// BOUND AND RELEASED, which is a race and is the right one to take: the
// alternative is a fixed port, and a fixed port is guaranteed to collide with the
// real deployment on the one machine where both are running — which is the whole
// case this command is for. The window between closing this listener and the
// server binding it is small, and a collision fails loudly at bind rather than
// silently sharing anything.
func pickLoopbackAddress(ctx context.Context) (string, error) {
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("find a free loopback port: %w", err)
	}

	addr := l.Addr().String()

	if err := l.Close(); err != nil {
		return "", fmt.Errorf("release the probed port: %w", err)
	}

	return addr, nil
}

// deriveAcceptanceConfig rewrites the base config into the isolated one and
// returns it with the derived tier labels.
//
// THROUGH THE YAML NODE TREE rather than by re-marshalling the parsed Config,
// because a round trip through the struct would silently drop every comment an
// operator wrote and normalise every value config.Load defaulted — turning a
// derived file into something an operator reading it could not recognise as
// theirs. Editing the tree changes exactly the keys named here.
func deriveAcceptanceConfig(
	basePath string, raw []byte, dir, prefix, listen string,
) ([]byte, []string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse the base config: %w", err)
	}

	root := documentRoot(&doc)
	if root == nil {
		return nil, nil, errors.New("the base config is not a YAML mapping")
	}

	labels, err := rewriteForAcceptance(root, dir, prefix, listen)
	if err != nil {
		return nil, nil, err
	}

	var out strings.Builder

	out.WriteString(acceptanceHeader(basePath, prefix))

	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)

	if err := enc.Encode(root); err != nil {
		return nil, nil, fmt.Errorf("render the derived config: %w", err)
	}

	if err := enc.Close(); err != nil {
		return nil, nil, fmt.Errorf("render the derived config: %w", err)
	}

	// PROVED BEFORE IT IS WRITTEN, the way `billet init` proves its own output:
	// a derived file that does not load is a failure an operator meets as a
	// service that will not start, several steps after the command that caused it.
	if _, err := config.Parse("the acceptance config billet derived", []byte(out.String())); err != nil {
		return nil, nil, fmt.Errorf("the derived acceptance config is not valid: %w", err)
	}

	return []byte(out.String()), labels, nil
}

func acceptanceHeader(basePath, prefix string) string {
	var b strings.Builder

	b.WriteString("# billet — an ISOLATED ACCEPTANCE DEPLOYMENT, derived by `billet acceptance up`\n")
	b.WriteString("# from " + basePath + ". Do not edit it: the next `up` regenerates it.\n")
	b.WriteString("#\n")
	b.WriteString("# Its state directories, its loopback ports and its deployment identity are its\n")
	b.WriteString("# own, and every tier label carries the `" + prefix + "-` prefix — so its GitHub\n")
	b.WriteString("# scale sets are disjoint from the ones the base config names, and so is what\n")
	b.WriteString("# `billet acceptance down` deletes.\n")
	b.WriteString("#\n")
	b.WriteString("# WHAT IT SHARES is the GitHub App and the organization. There is no way to\n")
	b.WriteString("# isolate those without minting a second App, whose private key GitHub issues\n")
	b.WriteString("# exactly once — not something a test harness should do per run.\n")
	b.WriteString("\n")

	return b.String()
}

// documentRoot unwraps a parsed document to its mapping node.
func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		doc = doc.Content[0]
	}

	if doc.Kind != yaml.MappingNode {
		return nil
	}

	return doc
}

func printAcceptanceUp(ws acceptanceWorkspace) {
	fmt.Printf("Derived an isolated acceptance deployment.\n\n")
	fmt.Printf("  config       %s\n", ws.ConfigPath)
	fmt.Printf("  deployment   %s\n", ws.DeploymentID)
	fmt.Printf("  listen       %s\n", ws.Listen)
	fmt.Printf("  tiers        %s\n", strings.Join(ws.Tiers, ", "))

	if ws.Account != "" {
		fmt.Printf("  account      %s (%s)\n", ws.Account, ws.CallerARN)
	}

	fmt.Printf("\nEverything this run creates carries deployment %s, which nothing else does —\n",
		ws.DeploymentID)
	fmt.Printf("so `billet acceptance down --workspace %s` destroys exactly what it made.\n",
		filepath.Dir(ws.ConfigPath))
}

// readAcceptanceWorkspace reads a workspace record, answering three ways:
// present, absent, or unreadable. The third is an error rather than "absent",
// because acting on "there is no run here" when there might be is how a teardown
// gets skipped.
func readAcceptanceWorkspace(dir string) (acceptanceWorkspace, bool, error) {
	path := filepath.Join(dir, acceptanceRecord)

	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return acceptanceWorkspace{}, false, nil
	}

	if err != nil {
		return acceptanceWorkspace{}, false, fmt.Errorf("read %s: %w", path, err)
	}

	var ws acceptanceWorkspace
	if err := json.Unmarshal(body, &ws); err != nil {
		return acceptanceWorkspace{}, false, fmt.Errorf("parse %s: %w", path, err)
	}

	if ws.Version != acceptanceRecordVersion {
		return acceptanceWorkspace{}, false, fmt.Errorf(
			"%s records schema %d and this billet writes %d; a teardown that half understood "+
				"it would destroy what it recognised and leave the rest",
			path, ws.Version, acceptanceRecordVersion)
	}

	return ws, true, nil
}

func writeAcceptanceWorkspace(dir string, ws acceptanceWorkspace) error {
	body, err := json.MarshalIndent(ws, "", "  ")
	if err != nil {
		return fmt.Errorf("render the workspace record: %w", err)
	}

	path := filepath.Join(dir, acceptanceRecord)
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// requireAcceptanceWorkspace is what every subcommand after `up` opens with.
func requireAcceptanceWorkspace(workspace string) (acceptanceWorkspace, error) {
	if strings.TrimSpace(workspace) == "" {
		return acceptanceWorkspace{}, errors.New("--workspace is required")
	}

	dir, err := filepath.Abs(workspace)
	if err != nil {
		return acceptanceWorkspace{}, fmt.Errorf("resolve --workspace: %w", err)
	}

	ws, found, err := readAcceptanceWorkspace(dir)
	if err != nil {
		return acceptanceWorkspace{}, err
	}

	if !found {
		return acceptanceWorkspace{}, fmt.Errorf(
			"%s holds no acceptance run (%s is missing). Run `billet acceptance up "+
				"--workspace %s` first", dir, acceptanceRecord, dir)
	}

	// AND THE CONFIG IT NAMES IS INSIDE THE WORKSPACE IT WAS FOUND IN.
	//
	// Everything after this reads `ws.ConfigPath` — the teardown deletes the scale
	// set of every tier in it — while the operator named a DIRECTORY. A record
	// whose config_path pointed elsewhere would make `down` act on another
	// deployment's config while looking like it acted on this workspace, and the
	// record is a plain JSON file. The workspace is the thing the operator chose;
	// the record is data found inside it.
	if want := filepath.Join(dir, acceptanceConfig); ws.ConfigPath != want {
		return acceptanceWorkspace{}, fmt.Errorf(
			"%s names the config %s, which is not this workspace's own (%s). Every "+
				"destructive step reads that file — `down` deletes the scale set of every "+
				"tier in it — so it will not be taken from a record that points elsewhere",
			filepath.Join(dir, acceptanceRecord), ws.ConfigPath, want)
	}

	// AND ITS CONTENT IS THE ONE `up` WROTE.
	//
	// PROVING THE PATH IS NOT PROVING THE FILE, and the difference is the whole
	// attack: replace <workspace>/billet.yaml with the PRODUCTION config and every
	// check above still passes — the record is untouched, the state directory
	// still holds the recorded identity — after which `teardown --all` deletes the
	// production deployment's scale sets and `decommission` scopes cloud deletion
	// from that config's identity rather than the one just proved. A digest is
	// what makes the file part of what was proved.
	//
	// A RECORD WITHOUT ONE IS REFUSED rather than trusted: it was written by a
	// billet that did not record digests, and this one cannot vouch for the file.
	if err := verifyAcceptanceConfig(ws); err != nil {
		return acceptanceWorkspace{}, err
	}

	// THE IDENTITY IS RE-PROVED, not trusted from the record. What the record says
	// and what the state directory holds are two facts, and every destructive
	// thing that follows is scoped by the second — so a disagreement must stop the
	// command rather than let it act on the first.
	held, ok, err := state.PeekDeploymentID(filepath.Join(dir, "server"))
	if err != nil {
		return acceptanceWorkspace{}, fmt.Errorf("read this workspace's deployment identity: %w", err)
	}

	if !ok {
		return acceptanceWorkspace{}, fmt.Errorf(
			"%s records deployment %s but its state directory holds none. Nothing here can "+
				"be scoped to this run, so nothing here will be destroyed by it",
			filepath.Join(dir, acceptanceRecord), ws.DeploymentID)
	}

	if held != ws.DeploymentID {
		return acceptanceWorkspace{}, fmt.Errorf(
			"%s records deployment %s and its state directory holds %s. A teardown scoped to "+
				"either is wrong about the other's compute",
			filepath.Join(dir, acceptanceRecord), ws.DeploymentID, held)
	}

	return ws, nil
}

// verifyAcceptanceConfig proves the derived config is the one `up` wrote.
func verifyAcceptanceConfig(ws acceptanceWorkspace) error {
	if ws.ConfigSHA256 == "" {
		return fmt.Errorf(
			"%s records no digest for %s, so this billet cannot vouch that the config every "+
				"destructive step is about is the one that was derived. Re-run "+
				"`billet acceptance up` against this workspace",
			filepath.Join(filepath.Dir(ws.ConfigPath), acceptanceRecord), ws.ConfigPath)
	}

	body, err := os.ReadFile(ws.ConfigPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", ws.ConfigPath, err)
	}

	if got := fmt.Sprintf("%x", sha256.Sum256(body)); got != ws.ConfigSHA256 {
		return fmt.Errorf(
			"%s is not the config this workspace recorded (%s, want %s). `down` deletes the "+
				"scale set of every tier in it and scopes cloud deletion from the identity it "+
				"names, so it will not act on a file that changed under the record",
			ws.ConfigPath, got, ws.ConfigSHA256)
	}

	return nil
}

// assertAWSAccount refuses a credential in an account the operator did not name.
//
// THREE ANSWERS, AND ONLY ONE OF THEM PROCEEDS. The account matches; the account
// differs (refused, naming both); or billet could not ask (refused, saying so
// separately) — because "I could not find out" must never be reported as "it is
// the right account", and this is the check standing in front of a command that
// launches compute and then destroys things.
func assertAWSAccount(
	ctx context.Context, in acceptanceInputs, cfg *config.Config,
) (awssts.Identity, error) {
	region := in.region
	if region == "" {
		region = acceptanceRegion(cfg)
	}

	if region == "" {
		return awssts.Identity{}, errors.New(
			"--account needs a region to ask sts:GetCallerIdentity in, and this config names " +
				"no AWS backend to take one from; pass --region")
	}

	id, err := awssts.New(region, in.stsEndpoint, in.creds, nil).Whoami(ctx)
	if err != nil {
		return awssts.Identity{}, fmt.Errorf(
			"could not establish which AWS account this credential is in, so --account %s is "+
				"unverified and this run will not start: %w", in.account, err)
	}

	if id.Account != in.account {
		return awssts.Identity{}, fmt.Errorf(
			"%w: --account said %s and the credential is in %s (%s). An acceptance run "+
				"launches compute and then destroys what carries its own identity; it will "+
				"not do that in an account nobody named",
			errWrongAccount, in.account, id.Account, id.ARN)
	}

	return id, nil
}

// acceptanceRegion is the AWS region a config's own backend names, if any.
func acceptanceRegion(cfg *config.Config) string {
	if cfg.Node == nil {
		return ""
	}

	if cfg.Node.EC2 != nil && cfg.Node.EC2.Region != "" {
		return cfg.Node.EC2.Region
	}

	if cfg.Node.CodeBuild != nil && cfg.Node.CodeBuild.Region != "" {
		return cfg.Node.CodeBuild.Region
	}

	return ""
}
