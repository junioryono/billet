package host

import (
	"context"
	"fmt"
	"path"
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

	// DropInPaths is asked for the remedy alone, which says when it was not
	// answered; the judgement reads only these.
	names := []string{"LoadState", "FragmentPath", "RuntimeDirectory"}

	props, err := insp.UnitProperties(ctx, nodeUnit, append(slices.Clone(names), "DropInPaths")...)
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

		remedy := unitRemedy(fragment, firstProp(props, "RuntimeDirectory"), firstProp(props, "DropInPaths"))
		if _, ok := props["DropInPaths"]; !ok {
			remedy += "; its drop-ins could not be read, so check them too (systemctl cat " + nodeUnit + ")"
		}

		return "", fmt.Errorf("%s needs %s to provide %s, and the loaded unit (%s) does not; a rollout installs no "+
			"units, so %s, and the rollout takes this host when it next asks", candidate, nodeUnit,
			strings.Join(missing, " and "), fragment, remedy)
	}

	return fmt.Sprintf("the loaded node unit provides everything %s needs", candidate), nil
}

// unitRequirementsRemedyDoc is the operator page that holds the manual repair.
const unitRequirementsRemedyDoc = "docs/operating/upgrades.md"

// The directories systemd loads a system unit from, by where they rank against
// the two a billet unit is installed in: the role's /etc/systemd/system and the
// package's /usr/lib/systemd/system. The order is systemd's own (`systemd-analyze
// unit-paths`, systemd 255 on Ubuntu 24.04, measured 2026-10-16): system.control,
// transient and generator.early above /etc/systemd/system; system.attached,
// /run/systemd/system, generator and /usr/local/lib between the two; and
// generator.late below both. /lib/systemd/system is /usr/lib's on a host
// without a merged /usr.
var (
	unitDirsAboveRole = []string{"/etc/systemd/system.control", "/run/systemd/system.control",
		"/run/systemd/transient", "/run/systemd/generator.early"}
	unitDirsBetween = []string{"/etc/systemd/system.attached", "/run/systemd/system",
		"/run/systemd/system.attached", "/run/systemd/generator", "/usr/local/lib/systemd/system"}
	unitDirsPackage = []string{"/usr/lib/systemd/system", "/lib/systemd/system"}
)

const (
	// unitRemedyStopFirst is a converge's route to a unit its repair does not
	// admit: over a running node a converge stops at the repair.
	unitRemedyStopFirst = "stop " + nodeUnit + ", then converge the host role (over a running node a converge " +
		"stops at its repair, which edits only the role's own unit in its one admitted shape)"
	// roleUnitRemedy is the remedy for the role's own unit in a shape the
	// repair refuses.
	roleUnitRemedy = unitRemedyStopFirst + ", which renders that unit afresh; or follow the manual repair in " +
		unitRequirementsRemedyDoc + "; installing the package does not help, since a unit under " +
		"/etc/systemd/system outranks the packaged one"
)

// unitRemedy names what makes the unit in effect meet a requirement, by where
// its file ranks: the role's own unit is repaired by its converge (`billet node
// repair-unit`) or by hand, and a package install does not reach it; a unit
// between the two is outranked by the one a converge installs; a packaged unit,
// or one below it, is replaced by either; a unit above the role's, or one
// systemd names no file for, is neither's. A fragment named otherwise is the
// target of an alias, whose own rank its path does not show. A host refused
// for its unit runs a node of a release that publishes its record (the
// refusing binary is one), and a converge over that node stops at its repair,
// which edits the role's own unit alone and only in its one admitted shape
// (billet/locks alone, no drop-in): so a converge reaches any other unit, or
// the role's in another shape, only once the node is stopped. A drop-in comes
// first whatever the unit: a converge that upgrades billet refuses any drop-in
// on the node unit, and one that sets RuntimeDirectory applies over any unit.
// The upgrade's refusal and the repair's both name their remedy here.
func unitRemedy(fragment, runtime, dropIns string) string {
	var remedy string

	switch dir := path.Dir(fragment); {
	case fragment == "":
		remedy = "systemd names no file for the unit in effect, so find what defines it (systemctl cat " +
			nodeUnit + ") before converging or installing the package"
	case path.Base(fragment) != nodeUnit:
		remedy = nodeUnit + " is an alias of " + path.Base(fragment) + ", and where the alias itself ranks is not " +
			"shown by its target's path, so find what defines it (systemctl cat " + nodeUnit + ") before " +
			"converging or installing the package"
	case dir == "/etc/systemd/system":
		remedy = roleUnitRemedyFor(runtime)
	case slices.Contains(unitDirsBetween, dir):
		remedy = unitRemedyStopFirst + ", whose unit under /etc/systemd/system outranks that one; installing the " +
			"package does not help, since that one outranks the packaged unit"
	case slices.Contains(unitDirsPackage, dir), dir == "/run/systemd/generator.late":
		remedy = "install this release's package, which replaces that unit with one this release ships; or " +
			unitRemedyStopFirst
	case slices.Contains(unitDirsAboveRole, dir):
		remedy = "remove that unit or give it the directory by hand as " + unitRequirementsRemedyDoc +
			" describes; it outranks both the host role's unit and the packaged one, so neither a converge nor " +
			"installing the package changes it"
	default:
		remedy = "find what installed that unit (systemctl cat " + nodeUnit + "), which is neither the host " +
			"role's nor the package's, before converging or installing the package"
	}

	return dropInsFirst(dropIns, remedy)
}

// roleUnitRemedyFor is the remedy for the role's own unit declaring runtime,
// with any drop-in removed: a converge repairs the one shape its repair
// admits, and renders any other afresh once the node is stopped.
func roleUnitRemedyFor(runtime string) string {
	if runtime == "billet/locks" {
		return "converge the host role, which repairs that unit, or follow the manual repair in " +
			unitRequirementsRemedyDoc + "; installing the package does not help, since a unit under " +
			"/etc/systemd/system outranks the packaged one"
	}

	return roleUnitRemedy
}

// dropInsFirst puts the removal of the node unit's drop-ins before remedy.
func dropInsFirst(dropIns, remedy string) string {
	if dropIns == "" {
		return remedy
	}

	return "remove the drop-ins on " + nodeUnit + " (" + dropIns + ") or fold their settings into the host " +
		"role's variables, then systemctl daemon-reload, since a converge that upgrades billet refuses any drop-in " +
		"on " + nodeUnit + " and one that sets RuntimeDirectory applies over whatever unit is in effect; then " +
		remedy
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
