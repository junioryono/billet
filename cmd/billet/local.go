package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
	"github.com/junioryono/billet/internal/lifeops"
)

// cmdLocal is the local deployment's lifecycle: the services on THIS machine,
// as opposed to `billet status`, which reports what the capacity ledger holds.
// The two are deliberately different questions — a control plane can be serving
// a perfectly healthy ledger from a unit that runs a binary the operator
// replaced an hour ago, and only one of these commands can say so.
func cmdLocal(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(
			"usage: billet local <up|down|status|backup|restore|recover|uninstall>")
	}

	// THE PLATFORM CHECK STAYS ABOVE THE DISPATCH, so a subcommand added later
	// inherits it rather than having to remember. It is a switch now rather than
	// a refusal: billet manages systemd units on Linux and launch agents on
	// macOS, and everything else has neither.
	switch hostOS {
	case "linux", "darwin":
	default:
		return fmt.Errorf("billet local manages the services billet ships — systemd units on "+
			"Linux, launch agents on macOS — and this host is %s. Run the two roles directly "+
			"(`billet server` and `billet node`)", hostOS)
	}

	switch args[0] {
	case "status":
		return cmdLocalStatus(ctx, args[1:])
	case "up":
		return cmdLocalUp(ctx, args[1:])
	case "down":
		return cmdLocalDown(ctx, args[1:])
	case "backup":
		return cmdLocalBackup(ctx, args[1:])
	case "restore":
		return cmdLocalRestore(ctx, args[1:])
	case "recover":
		return cmdLocalRecover(ctx, args[1:])
	case "uninstall":
		return cmdLocalUninstall(ctx, args[1:])
	}

	return fmt.Errorf("unknown local command %q; try up, down, status, backup, restore, "+
		"recover or uninstall", args[0])
}

// addServiceConfigFlag is the same flag defaulted to the path the PACKAGED
// UNITS read.
//
// billet's ordinary default is per-user, which is right for a command an
// operator runs against their own configuration and wrong for the `local`
// family: those commands are about the systemd services on this machine, and
// those services read /etc/billet/billet.yaml and nothing else. With the
// per-user default, the guided install path failed on the very host the package
// had just prepared — `billet local up` reported that a file in root's home
// directory did not exist, which is true, unhelpful, and about a file nothing
// ever creates.
func addServiceConfigFlag(fs *flag.FlagSet) *string {
	return fs.String("config", initconfig.ServiceConfigPathFor(hostOS),
		"path to billet.yaml (defaults to the config the services billet ships read)")
}

// inspect is a seam. The inspector's own fakes are package-private to
// internal/lifeops — rightly, since nothing outside it can construct a
// systemctl reply — so this is what lets the command's rendering be asserted
// against a host that does not exist.
var inspect = func(ctx context.Context, cfgPath string, keyPaths []string) (lifeops.Report, error) {
	return lifeops.NewInspector().Inspect(ctx, cfgPath, keyPaths)
}

// cmdLocalStatus reports what this machine is actually doing.
//
// IT ANSWERS ON A HALF-BUILT HOST, which is when somebody runs it: a config
// that will not load, a unit that is not installed and a binary it cannot
// resolve are all things to REPORT rather than reasons to fail. The one thing
// it will not do is invent an answer — every uncertain fact says so by name.
func cmdLocalStatus(ctx context.Context, args []string) error {
	fs := newFlagSet("billet local status")
	cfgPath := addServiceConfigFlag(fs)
	if err := parse(fs, args); err != nil {
		return err
	}

	// EACH MANAGER REPORTS ITS OWN FACTS. See macStatus for why this is a second
	// renderer rather than the same one with different words.
	if hostOS == "darwin" {
		return macStatus(ctx, *cfgPath)
	}

	fmt.Printf("config   %s\n", *cfgPath)

	// The config is READ FOR ITS PATHS, and a config that will not load is
	// reported rather than fatal: the units and the binary are still worth
	// naming, and they are often what explains the config problem.
	var keyPaths []string
	cfg, cfgErr := config.Load(*cfgPath)
	if cfgErr != nil {
		fmt.Printf("         UNREADABLE: %v\n", cfgErr)
	} else {
		keyPaths = appKeyFilePaths(cfg)
	}

	report, err := inspect(ctx, *cfgPath, keyPaths)
	if err != nil {
		return err
	}

	printFileFacts("config", report.Config)
	for _, key := range report.AppKeys {
		printFileFacts("key", key)
	}

	switch {
	case report.BinaryErr != nil:
		fmt.Printf("binary   UNKNOWN: %v\n", report.BinaryErr)
	default:
		fmt.Printf("binary   %s\n", report.Binary)
	}

	printService("server", report.Server)
	printService("node", report.Node)
	printTimers(ctx)

	return nil
}

// printTimers reports whether the scheduled updaters are enabled.
//
// REPORTED, NEVER MANAGED, for the backup timer's reason: `up` and `down` own
// the two services whose order is their safety content, and a timer is a
// oneshot with nothing to drain. What an operator needs to see is that a host
// which stopped acting on rollouts is one whose timer somebody disabled — the
// package enables both, and a disabled one looks exactly like a working one
// from every other angle.
func printTimers(ctx context.Context) {
	for _, timer := range []string{deploy.UpgradeTimerName, deploy.ImagesRefreshTimerName} {
		out, err := exec.CommandContext(ctx, "systemctl", "is-enabled", "--", timer).Output()

		state := strings.TrimSpace(string(out))

		switch state {
		case "":
			if err != nil {
				state = "enablement unknown (" + err.Error() + ")"
			} else {
				state = "enablement unknown"
			}
		case "not-found":
			state = "not installed; the package ships it"
		}

		fmt.Printf("%-8s %s %s\n", "updates", timer, state)
	}
}

// printFileFacts reports one path's ownership and mode, which is the pair that
// decides whether the service can read it at all.
func printFileFacts(label string, f lifeops.FileFacts) {
	switch f.Exists {
	case lifeops.Yes:
		owner := f.Owner
		if f.Group != "" {
			owner += ":" + f.Group
		}
		fmt.Printf("         %-6s %s (%s %04o)\n", label, f.Path, owner, f.Mode.Perm())
	case lifeops.No:
		fmt.Printf("         %-6s %s (MISSING)\n", label, f.Path)
	case lifeops.Unknown:
		fmt.Printf("         %-6s %s (UNREADABLE: %v)\n", label, f.Path, f.Err)
	}
}

// sortedProps keeps two runs of `status` in the same order.
func sortedProps(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)

	return out
}

// printService reports one unit: what systemd will run, and the two ways that
// can silently differ from what the operator thinks is installed.
func printService(label string, s lifeops.ServiceFacts) {
	if !s.Installed() {
		fmt.Printf("%-8s not installed (%s)\n", label, s.Name)
		fmt.Printf("         install the billet package, which ships the units\n")

		return
	}

	enabled := s.UnitFileState
	if enabled == "" {
		enabled = "enablement unknown"
	}

	fmt.Printf("%-8s %s (%s)", label, s.ActiveState, enabled)
	if s.MainPID > 0 {
		fmt.Printf(", pid %d", s.MainPID)
	}
	if s.NRestarts > 0 {
		fmt.Printf(", %d restart(s)", s.NRestarts)
	}
	fmt.Println()

	// A MASKED OR LINKED UNIT IS NOT THE UNIT ITS FILE SAYS IT IS, and neither
	// state is visible from the file on disk.
	switch s.LoadState {
	case "masked":
		fmt.Printf("         MASKED: systemd will refuse to start this unit\n")
	case "loaded":
	default:
		fmt.Printf("         LOAD STATE %s\n", s.LoadState)
	}
	if s.UnitFileState == "linked" {
		fmt.Printf("         LINKED: the unit is a symlink to a file outside the unit directories\n")
	}

	// NAMED FOR WHAT IT COMPARES, not for a verdict about who edited what: the
	// Ansible role renders its own units and they are deliberately not
	// byte-equal to the packaged ones, so "differs" is the ordinary state of
	// every role-managed host rather than an accusation.
	provenance := ""
	switch s.MatchesPackagedUnit {
	case lifeops.Yes:
		provenance = " (the packaged unit, unmodified)"
	case lifeops.No:
		provenance = " (differs from the packaged unit — expected on an Ansible-managed host)"
	case lifeops.Unknown:
		provenance = " (could not be compared with the packaged unit)"
	}
	fmt.Printf("         unit %s%s\n", s.FragmentPath, provenance)

	if s.ReloadPending == lifeops.Yes {
		fmt.Printf("         PENDING RELOAD: the unit file changed since systemd read it, so " +
			"what is on disk is not what would run. Run `systemctl daemon-reload`\n")
	}

	if len(s.DropInPaths) > 0 {
		fmt.Printf("         DROP-INS: %s\n", strings.Join(s.DropInPaths, " "))
		fmt.Printf("         (these override the unit above; billet did not write them)\n")
	}

	// WHAT `up` WOULD REFUSE, REPORTED HERE. These are the things that make a
	// unit do more than billet accounted for — another command, a filesystem
	// where the checked paths mean something else, an identity wider than the
	// one User= names. An operator whose `up` refused should be able to see
	// why from the command whose job is to report.
	for _, group := range []struct {
		label string
		facts map[string]string
	}{
		{"ALSO RUNS", s.ExecExtra},
		{"REPLACES ITS FILESYSTEM", s.Namespace},
		{"WIDENS ITS IDENTITY", s.Elevation},
	} {
		for _, name := range sortedProps(group.facts) {
			fmt.Printf("         %s: %s=%s\n", group.label, name, group.facts[name])
		}
	}

	if s.ExecStartCount > 1 {
		fmt.Printf("         AMBIGUOUS: the unit carries %d ExecStart directives\n", s.ExecStartCount)
	}

	// TWO DIFFERENT QUESTIONS, and the second is the one nothing else answers.
	//
	// What the unit WOULD run matters before a start: the packaged units name
	// /usr/bin/billet while scripts/install.sh writes /usr/local/bin, and a host
	// with both is a service that never picks up an upgrade.
	switch s.ExecStartIsThisBuild {
	case lifeops.Yes:
	case lifeops.No:
		fmt.Printf("         BINARY MISMATCH: this unit would run %s, %s\n", s.ExecStart, s.ExecStartWhy)
	case lifeops.Unknown:
		fmt.Printf("         BINARY UNCONFIRMED: %s\n", s.ExecStartWhy)
	}

	// What it IS running matters once it is up: replacing the binary without a
	// restart leaves the unit pointing at the new file while the service keeps
	// executing the old one, and every other signal on the host looks healthy.
	if s.Active() {
		switch s.RunningIsThisBuild {
		case lifeops.Yes:
		case lifeops.No:
			fmt.Printf("         RUNNING AN OLDER BINARY: %s\n", s.RunningWhy)
			fmt.Printf("         (restart the service to pick up the installed one)\n")
		case lifeops.Unknown:
			fmt.Printf("         RUNNING BINARY UNCONFIRMED: %s\n", s.RunningWhy)
		}
	}
}
