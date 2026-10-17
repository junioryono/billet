package host

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/version"
)

// What a candidate release needs of the node unit that will run it (#168), held
// against the unit systemd has loaded before an upgrade claims, accepts or
// stops anything. A rollout replaces the binary and never the units, so a host
// moved onto a release whose node writes into a runtime directory its unit does
// not declare runs a node that cannot publish its registration record, and the
// next converge waits for a record that never appears.
//
// FROM THIS BUILD'S OWN TABLE (deploy.NodeRuntimeRequirements), not from the
// candidate: the refusal must reach the node before it is told the job was
// taken, which is before the candidate is downloaded, and a refusal after that
// leaves the rollout waiting on a host that will never move. The table names
// each requirement with the first release that has it, so every candidate this
// build can be asked to install is judged; a requirement a LATER release adds is
// that release's to declare, and a host reaches it through a converge or a
// package that installs its units.

// nodeUnitRequirementsOf is what a release needs its node unit to declare: every
// entry of the table at or after the release that first needed it. A release
// whose order cannot be told is held to all of them, the direction that can
// only refuse a unit that lacks one.
func nodeUnitRequirementsOf(release string) []string {
	var required []string

	for _, req := range deploy.NodeRuntimeRequirements {
		order, ok := version.Compare(release, req.Since)
		if !ok || order >= 0 {
			required = append(required, req.Entry)
		}
	}

	return required
}

// missingRuntimeDirectories is what the loaded node unit lacks of the entries
// required, read from systemd's rendering of RuntimeDirectory (its entries
// separated by spaces); none is a unit that meets every requirement.
func missingRuntimeDirectories(required []string, runtime string) []string {
	declared := strings.Fields(runtime)

	var missing []string

	for _, entry := range required {
		if !slices.Contains(declared, entry) {
			missing = append(missing, "RuntimeDirectory="+entry)
		}
	}

	return missing
}

// checkUnitRequirements holds the loaded node unit to what the candidate
// release needs of it, and refuses naming the unit in effect and its remedy. A
// host with no node section has no node to hold; one with a node section and a
// node unit absent or masked refuses, since the transaction starts that unit
// and installs none; any property systemd did not answer is could-not-tell,
// which refuses. The string is what was found, for the upgrade's report.
func checkUnitRequirements(ctx context.Context, cfg *config.Config, candidate string, insp *lifeops.Inspector) (string, error) {
	if cfg == nil || cfg.Node == nil {
		return "this host runs no node, so the candidate needs nothing of a node unit", nil
	}

	required := nodeUnitRequirementsOf(candidate)
	if len(required) == 0 {
		return "the candidate needs nothing of a node unit", nil
	}

	names := []string{"LoadState", "FragmentPath", "RuntimeDirectory"}

	props, err := insp.UnitProperties(ctx, nodeUnit, names...)
	if err != nil {
		return "", fmt.Errorf("ask systemd about %s: %w", nodeUnit, err)
	}

	// AN UNANSWERED PROPERTY IS NOT AN EMPTY ONE: only an answer says what the
	// unit declares.
	for _, name := range names {
		if values, ok := props[name]; !ok || len(values) != 1 {
			return "", fmt.Errorf("systemd answered no %s for %s, so whether it meets what %s needs of it cannot be "+
				"told", name, nodeUnit, candidate)
		}
	}

	switch load := firstProp(props, "LoadState"); load {
	case "loaded":
	case "not-found", "masked":
		// THE CONFIGURATION NAMES A NODE, so the transaction stops and starts this
		// unit and installs none: a unit systemd cannot start would be found only
		// after the binary moved, and masking proves nothing about a process.
		return "", fmt.Errorf("the configuration names a node and %s is %s, so the upgrade would start a unit "+
			"systemd cannot start; install the node unit (converge the host role, or install the package) or "+
			"remove the node section before upgrading to %s", nodeUnit, load, candidate)
	default:
		return "", fmt.Errorf("%s is LoadState=%s, so whether it meets what %s needs of it cannot be told",
			nodeUnit, load, candidate)
	}

	if missing := missingRuntimeDirectories(required, firstProp(props, "RuntimeDirectory")); len(missing) > 0 {
		fragment := firstProp(props, "FragmentPath")

		return "", fmt.Errorf("%s needs %s to provide %s, and the loaded unit (%s) does not; a rollout installs no "+
			"units, so %s, and the rollout takes this host when it next asks", candidate, nodeUnit,
			strings.Join(missing, " and "), fragment, unitRemedy(fragment))
	}

	return fmt.Sprintf("the loaded node unit provides everything %s needs", candidate), nil
}

// unitRequirementsRemedyDoc is the operator page that holds the manual repair.
const unitRequirementsRemedyDoc = "docs/operating/upgrades.md"

// unitRemedy names what makes the unit in effect meet a requirement. A unit
// under /etc overrides the packaged one, so installing a package changes
// nothing there: the host role's converge repairs it (`billet node
// repair-unit`), or the manual procedure does. A packaged unit is replaced by
// installing this release's package, and a converge installs the role's.
func unitRemedy(fragment string) string {
	if strings.HasPrefix(fragment, "/etc/") {
		return "converge the host role, which repairs that unit, or follow the manual repair in " +
			unitRequirementsRemedyDoc + "; installing the package does not help, since a unit under /etc " +
			"overrides the packaged one"
	}

	return "install this release's package, or converge the host role, so that the unit in effect is one this " +
		"release ships"
}

// unitRequirementsInspector is the inspector the upgrade holds the node unit
// with; a variable so a test can stand a systemctl in.
var unitRequirementsInspector = func() *lifeops.Inspector { return lifeops.NewInspector() }

// refuseUnmetUnitRequirements is the upgrade's preflight of the node unit, on a
// host that runs systemd: a refusal before the claim, answered to the node as
// any other preflight refusal is, so the rollout backs off and asks again.
func refuseUnmetUnitRequirements(ctx context.Context, env cli.Env, cfg *config.Config, candidate string) error {
	if cli.HostOS != "linux" {
		return nil
	}

	found, err := checkUnitRequirements(ctx, cfg, candidate, unitRequirementsInspector())
	if err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "unit requirements: %s\n", found)

	return nil
}
