package lifeops

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// runner executes one systemctl invocation. A seam, so a test can assert the
// ARGUMENTS billet builds — which is where the mistakes are — without a service
// manager, and can answer as a host with a masked unit or no unit at all.
type runner func(ctx context.Context, bin string, args []string) ([]byte, error)

// execRunner runs systemctl for real.
//
// STDOUT AND STDERR ARE SEPARATE, never combined: systemctl writes values to
// stdout and narration to stderr, so a combined buffer silently corrupts the
// value being read back. Stderr is folded into the error instead, where it is
// the explanation rather than the data.
func execRunner(ctx context.Context, bin string, args []string) ([]byte, error) {
	var stdout, stderr bytes.Buffer

	// #nosec G204 -- bin is the configured systemctl path and every argument is
	// built here; nothing from a config file reaches argv.
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err,
			strings.TrimSpace(stderr.String()))
	}

	return stdout.Bytes(), nil
}

// properties asks systemd about one unit.
//
// ABSENCE IS A VALUE HERE, NOT AN ERROR. Measured on systemd 255: `systemctl
// show` exits 0 for a unit that does not exist and answers LoadState=not-found
// with an empty UnitFileState. So an error from this call means systemd could
// not be asked at all — no manager, no permission — which is a different fact
// from "there is no such unit" and must not be flattened into it.
//
// Values come back as a slice per key because a directive may legitimately
// appear more than once (a unit may carry several ExecStart= lines), and a
// caller that silently kept the last one would act on half the truth.
func (i *Inspector) properties(ctx context.Context, unit string, names ...string) (map[string][]string, error) {
	args := make([]string, 0, len(names)+3)
	args = append(args, "show")
	for _, n := range names {
		args = append(args, "--property="+n)
	}

	// `--` BEFORE THE UNIT NAME, because a unit name can begin with a dash and
	// one of them is on every Linux host: `-.mount` is the root filesystem, and
	// billet's own units pull it in through their implicit Requires=. Without
	// this, systemctl answers `invalid option -- '.'`, billet reads that as a
	// unit it cannot ask about, and `local up` refuses a correct host —
	// measured, on a machine the package had just prepared.
	args = append(args, "--", unit)

	out, err := i.run(ctx, i.systemctl, args)
	if err != nil {
		return nil, err
	}

	props := make(map[string][]string, len(names))
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		props[key] = append(props[key], value)
	}

	return props, nil
}

// first returns the single value of a property, or "" when it is absent.
func first(props map[string][]string, name string) string {
	if v := props[name]; len(v) > 0 {
		return strings.TrimSpace(v[0])
	}

	return ""
}

// execStartPath pulls the executable out of SYSTEMD'S OWN rendering of
// ExecStart, which is the point: this is systemd's parse of the unit rather
// than billet's parse of the unit's text.
//
//	{ path=/usr/bin/billet ; argv[]=/usr/bin/billet server --config /etc/... ; ... }
//
// Reading `path=` out of that answer keeps billet out of the business of
// reimplementing systemd's quoting and escaping rules — where a unit written
// with quotes or an escaped space would otherwise be parsed one way here and
// another way by the manager that actually runs it.
//
// THE RESIDUAL, STATED RATHER THAN CLAIMED AWAY: this rendering is for humans
// and systemd does not escape the field it delimits, so an executable whose
// PATH itself contains " ;" is cut short here, and one containing a newline
// breaks the line-oriented reader above. Both require a unit file naming such a
// path, which is already a host where somebody chooses what billet runs. It is
// why this answer is a diagnostic and must not be the thing an authorization
// decision rests on. The value is not trimmed, because a path may legitimately
// end in a space and trimming would silently name a different file.
func execStartPath(rendered string) string {
	const marker = "path="

	idx := strings.Index(rendered, marker)
	if idx < 0 {
		return ""
	}

	rest := rendered[idx+len(marker):]
	if end := strings.Index(rest, " ;"); end >= 0 {
		rest = rest[:end]
	}

	return rest
}

// execStartArgv pulls the COMMAND LINE out of systemd's rendering of
// ExecStart, alongside the path execStartPath reads.
//
// The path answers which file runs; this answers what it is told to do, and
// they are different questions with different consequences. A unit named
// billet-node.service whose argv says `billet server` runs a control plane, and
// every executable-identity check in this package would pass it.
//
// It carries the same residual as execStartPath — a rendering meant for humans,
// delimited by " ;" — and it fails the same way: a mangled or truncated answer
// does not match what the caller expects, and a mismatch refuses.
func execStartArgv(rendered string) string {
	const marker = "argv[]="

	idx := strings.Index(rendered, marker)
	if idx < 0 {
		return ""
	}

	rest := rendered[idx+len(marker):]
	if end := strings.Index(rest, " ;"); end >= 0 {
		rest = rest[:end]
	}

	return rest
}

// dropInPaths splits systemd's space-separated DropInPaths answer. Empty when
// a unit has no overrides, which is the ordinary case.
func dropInPaths(rendered string) []string {
	return strings.Fields(rendered)
}

// present returns the named properties that carry a value, so a caller can
// refuse "this unit does something billet did not account for" without naming
// every property systemd has.
func present(props map[string][]string, names ...string) map[string]string {
	found := map[string]string{}
	for _, n := range names {
		if v := first(props, n); v != "" {
			found[n] = v
		}
	}

	if len(found) == 0 {
		return nil
	}

	return found
}

// commands is present() for properties whose value is a rendered command, kept
// to the argv so a refusal names what would run rather than reprinting
// systemd's pids and timestamps at an operator.
func commands(props map[string][]string, names ...string) map[string]string {
	found := present(props, names...)
	for name, rendered := range found {
		if argv := execStartArgv(rendered); argv != "" {
			found[name] = argv
		}
	}

	return found
}

// actions collects the job settings whose ordinary value is a word rather than
// an empty string, keeping only the ones that differ from it.
func actions(props map[string][]string) map[string]string {
	ordinary := map[string]string{
		"OnFailureJobMode": "replace",
		"FailureAction":    "none",
		"SuccessAction":    "none",
		"StartLimitAction": "none",
		"JobTimeoutAction": "none",
	}

	// AN ABSENT ANSWER IS NOT THE ORDINARY ONE. Each of these has a known
	// harmless value, so systemd not reporting one at all leaves billet unable
	// to say whether this unit reboots the machine when it fails.
	found := map[string]string{}
	for name, want := range ordinary {
		v := first(props, name)
		switch {
		case v == "":
			found[name] = "(no answer)"
		case v != want:
			found[name] = v
		}
	}

	if len(found) == 0 {
		return nil
	}

	return found
}

// elevation collects the properties that change who a service runs as, past the
// account its User= names.
//
// DynamicUser is handled separately because its harmless value is "no" rather
// than empty: with it set, systemd allocates the account itself, so the name in
// User= stops being the account billet resolved and chowned for.
func elevation(props map[string][]string) map[string]string {
	found := present(props,
		"SupplementaryGroups", "AmbientCapabilities", "PAMName", "ExecSearchPath")

	if v := first(props, "DynamicUser"); v != "" && v != "no" {
		if found == nil {
			found = map[string]string{}
		}
		found["DynamicUser"] = v
	}

	return found
}

// execStartFlags pulls the command's PREFIXES out of systemd's extended
// rendering of ExecStart.
//
// THIS IS THE ONLY PLACE THEY EXIST. Measured on systemd 255: a command written
// `ExecStart=+/usr/bin/billet server …` runs with full privileges no matter what
// User= says, and the ordinary `-p ExecStart` answer renders it byte-identically
// to the unprefixed form. `-p ExecStartEx` renders the same command with
// `flags=privileged`, and an ordinary one with `flags=`.
//
// Reading systemd's own extended answer replaced a reader that parsed the unit
// FILE for the same purpose, and that is a correction rather than a
// simplification: the file reader was defeated by `ExecStart = ` with spaces
// around the key, which systemd accepts, treats as a list reset, and does not
// warn about — so a benign first line and a privileged third line passed every
// check billet had. systemd's own parse cannot disagree with systemd.
func execStartFlags(rendered string) (string, bool) {
	// FIELD BY FIELD, not by substring. The rendering is " ; "-delimited, and a
	// bare search for "flags=" finds one inside argv[] first if a config path or
	// an argument happens to contain it — which would read the argument and
	// never reach the field that decides privilege.
	for _, field := range strings.Split(rendered, " ; ") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(field), "flags="); ok {
			return strings.TrimSpace(strings.TrimSuffix(value, " }")), true
		}
	}

	return "", false
}
