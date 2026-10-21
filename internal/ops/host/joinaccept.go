package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
	"github.com/junioryono/billet/internal/ops/fleetops"
	"github.com/junioryono/billet/internal/ops/setup"
	"github.com/junioryono/billet/internal/state"
)

// maxProposalText bounds what `join accept` reads from a file or stdin.
const maxProposalText = 1 << 20

// joinAcceptMerged runs between the merge and the write; a test changes the
// file there.
var joinAcceptMerged = func() {}

// joinAccept is `billet join accept`: the control plane's half of a join,
// applied by machine (#183).
//
// THE NODE'S HALF, MERGED INTO THIS CONTROL PLANE'S CONFIG IN PLACE: its tiers
// by label, its nodes: entry by name, and the deployment ceiling raised to
// cover every registered host plus the node (initconfig.MergeJoin), through the
// same writer `github-app create --config` edits a config with, keeping its
// comments, mode and owner. It used to be YAML printed on the joining machine
// for an operator to merge by hand, and a hand merge is where a node's share was
// once left out of the ceiling, so the node registered, advertised and got none
// of its own room.
//
// The control plane reads tiers when it starts, so the merge ends with the
// restart that takes it, which is a handoff (#370) rather than a drain.
// Accepting tiers into a running control plane without a restart is declined
// (#183): every tier is a scale set the listener opens at start.
//
// A CONFIG THE INVENTORY OWNS IS NOT EDITED. The roles render billet.yaml from
// the inventory and mark its first line (initconfig.RoleRenderedMarker), so an
// edit here would be undone by the next converge; the same half is printed in
// the inventory's terms instead.
//
// ONE EDIT AT A TIME, ON THE FILE ITSELF: the read, the merge and the write run
// under the lifecycle lock, the file is read again before it is replaced and a
// change since refuses, and a symlinked config is edited at its target, because
// the write is a rename and would otherwise replace the link. Two accepts run
// side by side would each merge into the file the other had not yet written,
// and the second would put back the first's ceiling.
func joinAccept(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet join accept", env.Stdout)
	cfgPath := addServiceConfigFlag(fs)
	dryRun := fs.Bool("dry-run", false, "say what the merge would change, and write nothing")

	arg, err := cli.ParseWithName(fs, args)
	if err != nil {
		return err
	}
	if arg == "" {
		return errors.New("usage: billet join accept [--config PATH] [--dry-run] <the token billet join printed | " +
			"@file | ->")
	}

	text, err := proposalText(env, arg)
	if err != nil {
		return err
	}

	p, err := initconfig.ParseJoinProposal(text)
	if err != nil {
		return err
	}

	path, err := editTarget(*cfgPath)
	if err != nil {
		return err
	}

	if !*dryRun {
		lock, err := lifecycleLock()
		if err != nil {
			return err
		}

		defer func() {
			if err := lock.Release(); err != nil {
				fmt.Fprintf(env.Stdout, "warn     could not release the lifecycle lock: %v\n", err)
			}
		}()
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	cfg, err := config.Parse(path, raw)
	if err != nil {
		return err
	}
	if cfg.Server == nil {
		return fmt.Errorf("%s has no server block: billet join accept runs on the control plane the node "+
			"is joining", path)
	}

	if err := checkProposalDeployment(cfg, p); err != nil {
		return err
	}

	registered, err := registeredContribution(ctx, path)
	if err != nil {
		return err
	}

	merge, err := initconfig.MergeJoin(raw, p, registered)
	if err != nil {
		return err
	}

	describeMerge(env, path, p, merge)

	if !merge.Changed() {
		fmt.Fprintf(env.Stdout, "\n%s already has everything this node proposes; nothing to change.\n", path)

		return pendingEnrollmentNote(env, p)
	}

	if initconfig.RoleRendered(raw) {
		return inventoryForm(env, path, p, merge)
	}

	if *dryRun {
		fmt.Fprintf(env.Stdout, "\nNothing was written (--dry-run).\n")

		return nil
	}

	joinAcceptMerged()

	// THE FILE MERGED INTO IS THE FILE REPLACED: ReplaceConfig refuses, as its
	// last step before the rename, a file an editor or `github-app create`
	// (which do not take the lifecycle lock) changed since it was read.
	if err := setup.ReplaceConfig(env, path, raw, merge.Body); err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "\nWrote %s. The control plane reads tiers when it starts; restart it to take "+
		"them, which hands over to the next process rather than draining:\n  %s\n", path, serverRestartCommand())

	return pendingEnrollmentNote(env, p)
}

// proposalText is the proposal from the argument: the token itself, @file, or
// - for stdin.
func proposalText(env cli.Env, arg string) (string, error) {
	var r io.Reader

	switch {
	case arg == "-":
		r = env.Stdin
	case strings.HasPrefix(arg, "@"):
		f, err := os.Open(strings.TrimPrefix(arg, "@"))
		if err != nil {
			return "", fmt.Errorf("read the join proposal: %w", err)
		}
		defer func() { _ = f.Close() }()
		r = f
	default:
		return arg, nil
	}

	raw, err := io.ReadAll(io.LimitReader(r, maxProposalText+1))
	if err != nil {
		return "", fmt.Errorf("read the join proposal: %w", err)
	}
	if len(raw) > maxProposalText {
		return "", fmt.Errorf("the join proposal is larger than %d bytes", maxProposalText)
	}

	return string(raw), nil
}

// editTarget is the file an edit of path lands on: its target when path is a
// symlink. A path that cannot be read is left for the read to report.
func editTarget(path string) (string, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", fmt.Errorf("%s is a symbolic link and billet cannot resolve what it points at: %w",
				path, err)
		}

		return resolved, nil
	}

	return path, nil
}

// checkProposalDeployment refuses a proposal made against another deployment.
// Every proposal names one (initconfig.ParseJoinProposal refuses one that does
// not).
func checkProposalDeployment(cfg *config.Config, p initconfig.JoinProposal) error {
	id, ok, err := state.PeekDeploymentID(cfg.Server.IdentityDir)
	switch {
	case err != nil:
		return fmt.Errorf("read this control plane's deployment to compare the proposal with: %w", err)
	case !ok:
		return fmt.Errorf("this control plane has no deployment identity yet, and the proposal was made "+
			"enrolling with deployment %s, so it was made against another control plane", p.Deployment)
	case id != p.Deployment:
		return fmt.Errorf("the proposal was made enrolling with deployment %s, and this control plane is "+
			"deployment %s: it is another deployment's node", p.Deployment, id)
	}

	return nil
}

// registeredContribution is what every registered host contributes, read
// from the control plane's ledger; a running control plane holds it, so this
// opens it as one-shot operator commands do.
func registeredContribution(ctx context.Context, cfgPath string) ([]initconfig.ContributingNode, error) {
	a, closeDB, err := fleetops.ControlPlaneAllocator(ctx, cfgPath)
	if err != nil {
		return nil, fmt.Errorf("read the registered hosts the ceiling must cover: %w", err)
	}
	defer closeDB()

	rows, err := a.RegisteredContribution(ctx)
	if err != nil {
		return nil, err
	}

	return contributingNodes(rows), nil
}

// contributingNodes translates the ledger's rows.
func contributingNodes(rows []alloc.Contribution) []initconfig.ContributingNode {
	out := make([]initconfig.ContributingNode, 0, len(rows))
	for _, r := range rows {
		out = append(out, initconfig.ContributingNode{Name: r.Node, VCPU: r.VCPU, Memory: r.Memory})
	}

	return out
}

// describeMerge says what the merge adds, keeps and raises.
func describeMerge(env cli.Env, cfgPath string, p initconfig.JoinProposal, m initconfig.JoinMerge) {
	name := p.Node
	if name == "" {
		name = "(named by its certificate)"
	}

	fmt.Fprintf(env.Stdout, "Joining node %s (%s, contributing %d vCPU and %s) into %s:\n", name, p.Provider,
		p.MaxVCPU, p.MaxMemory, cfgPath)
	for _, label := range m.AddedTiers {
		fmt.Fprintf(env.Stdout, "  add tier      %s\n", label)
	}
	for _, label := range m.KeptTiers {
		fmt.Fprintf(env.Stdout, "  keep tier     %s (already here, identical)\n", label)
	}
	for _, n := range m.AddedNodes {
		fmt.Fprintf(env.Stdout, "  add nodes:    %s\n", n)
	}
	for _, n := range m.KeptNodes {
		fmt.Fprintf(env.Stdout, "  keep nodes:   %s (already here, identical)\n", n)
	}
	if m.VCPU[0] != m.VCPU[1] || m.Memory[0] != m.Memory[1] {
		fmt.Fprintf(env.Stdout, "  raise ceiling %d vCPU, %s -> %d vCPU, %s (every registered host plus this node)\n",
			m.VCPU[0], m.Memory[0], m.VCPU[1], m.Memory[1])
	} else {
		fmt.Fprintf(env.Stdout, "  keep ceiling  %d vCPU, %s (it already covers every registered host and this node)\n",
			m.VCPU[0], m.Memory[0])
	}
}

// inventoryForm prints the half in the inventory's terms, for a config the
// roles render, and refuses to edit the file.
func inventoryForm(env cli.Env, cfgPath string, p initconfig.JoinProposal, m initconfig.JoinMerge) error {
	fmt.Fprintf(env.Stdout, "\n%s is rendered by the junioryono.billet collection from the inventory, and the "+
		"next converge would undo an edit here. Make the change in the inventory instead, then converge "+
		"the control plane:\n", cfgPath)

	added := func(entries []*yaml.Node, key string, names []string) []*yaml.Node {
		var out []*yaml.Node
		for _, e := range entries {
			for _, n := range names {
				if v := mappingScalar(e, key); v == n {
					out = append(out, e)
				}
			}
		}

		return out
	}

	if tiers := added(p.Tiers, "label", m.AddedTiers); len(tiers) > 0 {
		fmt.Fprintf(env.Stdout, "\nAdd to the tier catalogue (billet_config.tiers, or the shared anchor it "+
			"points at):\n%s", indentedYAML(tiers))
	}
	if nodes := added(p.Nodes, "name", m.AddedNodes); len(nodes) > 0 {
		fmt.Fprintf(env.Stdout, "\nAdd to billet_config.nodes:\n%s", indentedYAML(nodes))
	}
	if m.VCPU[0] != m.VCPU[1] || m.Memory[0] != m.Memory[1] {
		fmt.Fprintf(env.Stdout, "\nSet billet_config.server.max_vcpu to %d and max_memory to %s.\n", m.VCPU[1], m.Memory[1])
	}

	return errors.New("billet join accept: the config is the inventory's; nothing was written")
}

// mappingScalar is a mapping node's scalar at key, or "".
func mappingScalar(m *yaml.Node, key string) string {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.ScalarNode {
			return m.Content[i+1].Value
		}
	}

	return ""
}

// indentedYAML renders entries as a YAML list, indented for reading.
func indentedYAML(entries []*yaml.Node) string {
	list := &yaml.Node{Kind: yaml.SequenceNode, Content: entries}

	raw, err := yaml.Marshal(list)
	if err != nil {
		return "  (unrenderable)\n"
	}

	var b strings.Builder
	for line := range strings.SplitSeq(strings.TrimRight(string(raw), "\n"), "\n") {
		b.WriteString("  " + line + "\n")
	}

	return b.String()
}

// serverRestartCommand is the command that restarts this platform's control
// plane service.
func serverRestartCommand() string {
	if cli.HostOS == "darwin" {
		return "launchctl kickstart -k gui/$(id -u)/" + deploy.ServerAgentLabel
	}

	return "sudo systemctl restart " + deploy.ServerUnitName
}

// pendingEnrollmentNote reminds the operator of the other half of a join: the
// node's enrollment request, which is approved by the fingerprint the node
// printed, never by one read here.
func pendingEnrollmentNote(env cli.Env, p initconfig.JoinProposal) error {
	fmt.Fprintf(env.Stdout, "\nIf %s is waiting to enroll, `billet nodes pending` shows its request: compare "+
		"the fingerprint there with the one the node printed, then\n  billet nodes approve %s --fingerprint "+
		"<the value you compared>\n", p.Node, p.Node)

	return nil
}
