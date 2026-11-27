package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/cli"
)

// errNoLVM says this host has no lvmconfig, so no LVM, and nothing scans the images
// it maps.
var errNoLVM = errors.New("no lvmconfig on this host")

// lvmConfig runs lvmconfig with args and answers its standard output, its standard
// error and its exit status. An lvmconfig that ran and exited non-zero is not an
// error; one that could not be run is, and errNoLVM is one that is not installed.
var lvmConfig = func(ctx context.Context, args ...string) (string, string, int, error) {
	path, err := exec.LookPath("lvmconfig")
	if err != nil {
		return "", "", 0, errNoLVM
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err = cmd.Run()

	if exit, ok := errors.AsType[*exec.ExitError](err); ok && ctx.Err() == nil {
		return stdout.String(), stderr.String(), exit.ExitCode(), nil
	}

	if err != nil {
		return "", "", 0, err
	}

	return stdout.String(), stderr.String(), 0, nil
}

// The device names a mapped image has, which LVM's filter must reject: a kernel RBD
// mapping and an rbd-nbd one.
var lvmMappedDevices = []string{"/dev/rbd0", "/dev/nbd0"}

// reportLVMFilter says whether LVM's scans are kept off the RBD and NBD devices this
// host maps (#922). ADVISORY, like the pool's fill: it fails nothing.
//
// AN OSD COULD NOT RESTART FOR HALF AN HOUR ON 2026-11-25 because its activation's
// `lvs` read every mapped image's label, and those reads waited on placement groups
// whose only copy was on that OSD. The host role writes a global_filter rejecting
// them; this says so where an operator looks, and says when it is not there.
//
// THREE ANSWERS. lvmconfig answers a setting nobody wrote with exit 5 and "not
// found" (2.03.16 and 2.03.31), which is no filter; any other failure, a
// configuration file it could not read included (exit 4, measured as a user who
// cannot read a 0600 lvmlocal.conf, 2026-11-27), is could-not-tell.
func reportLVMFilter(ctx context.Context, env cli.Env) {
	out, stderr, exit, err := lvmConfig(ctx, "devices/global_filter")

	switch {
	case errors.Is(err, errNoLVM):
		fmt.Fprintf(env.Stdout, "lvm      no LVM on this host, so nothing scans the images it maps\n")

		return
	case err != nil:
		fmt.Fprintf(env.Stdout, "lvm      UNCHECKED: could not run lvmconfig: %v\n", err)

		return
	case exit == 5 && strings.Contains(stderr, "not found"):
		out = ""
	case exit != 0:
		fmt.Fprintf(env.Stdout, "lvm      UNCHECKED: lvmconfig devices/global_filter exited %d: %s\n",
			exit, strings.TrimSpace(lastLine(stderr)))

		return
	}

	verdict, err := lvmFilterRejects(out, lvmMappedDevices)
	if err != nil {
		fmt.Fprintf(env.Stdout, "lvm      UNCHECKED: could not read LVM's global_filter: %v\n", err)

		return
	}

	if verdict != "" {
		fmt.Fprintf(env.Stdout, "lvm      WARNING: LVM's scans reach the RBD images this host maps (%s): an OSD that "+
			"restarts while it holds their only copy can wait on its own scan for as long as it is down "+
			"(#922); the host role writes a global_filter rejecting /dev/rbd and /dev/nbd "+
			"(billet_lvm_rbd_filter)\n", verdict)

		return
	}

	fmt.Fprintf(env.Stdout, "lvm      LVM's global_filter keeps its scans off the RBD and NBD devices this host maps\n")

	// A DEVICES FILE'S ENTRIES BYPASS THE FILTER, so one in use is said; the host role
	// refuses one that names a mapped image, and reading it needs root.
	if out, _, exit, err := lvmConfig(ctx, "--typeconfig", "full", "devices/use_devicesfile"); err == nil &&
		exit == 0 && strings.TrimSpace(out) == "use_devicesfile=1" {
		fmt.Fprintf(env.Stdout, "         a devices file is in use, and the filter does not govern the devices it "+
			"names\n")
	}
}

// lvmFilterRejects reads `lvmconfig devices/global_filter`'s answer and says which of
// devices the filter would let LVM scan, or "" when it rejects every one. An empty
// answer is no filter, which rejects nothing. LVM takes a device's first matching
// pattern, and a device no pattern matches is accepted.
func lvmFilterRejects(answer string, devices []string) (string, error) {
	answer = strings.TrimSpace(answer)

	var patterns []string

	if answer != "" {
		value, ok := strings.CutPrefix(answer, "global_filter=")
		if !ok {
			return "", fmt.Errorf("unexpected answer %q", answer)
		}

		var err error
		if patterns, err = lvmStringList(value); err != nil {
			return "", err
		}
	}

	type rule struct {
		accept bool
		re     *regexp.Regexp
	}

	rules := make([]rule, 0, len(patterns))

	for _, p := range patterns {
		if len(p) < 3 || (p[0] != 'a' && p[0] != 'r') {
			return "", fmt.Errorf("a pattern that is not a|...| or r|...|: %q", p)
		}

		// THE DELIMITER IS WHATEVER FOLLOWS a OR r, and ends the expression.
		delim := p[1]

		end := strings.LastIndexByte(p, delim)
		if end <= 1 {
			return "", fmt.Errorf("a pattern with no closing delimiter: %q", p)
		}

		re, err := regexp.Compile(p[2:end])
		if err != nil {
			return "", fmt.Errorf("a pattern Go cannot read as LVM does: %q: %w", p, err)
		}

		rules = append(rules, rule{accept: p[0] == 'a', re: re})
	}

	var scanned []string

	for _, dev := range devices {
		accepted := true

		for _, r := range rules {
			if r.re.MatchString(dev) {
				accepted = r.accept

				break
			}
		}

		if accepted {
			scanned = append(scanned, dev)
		}
	}

	return strings.Join(scanned, ", "), nil
}

// lvmStringList reads lvmconfig's rendering of a string array: ["a","b"].
func lvmStringList(value string) ([]string, error) {
	value = strings.TrimSpace(value)

	inner, ok := strings.CutPrefix(value, "[")
	if !ok {
		return nil, fmt.Errorf("not a list: %q", value)
	}

	inner, ok = strings.CutSuffix(inner, "]")
	if !ok {
		return nil, fmt.Errorf("not a list: %q", value)
	}

	var out []string

	for inner = strings.TrimSpace(inner); inner != ""; {
		rest, ok := strings.CutPrefix(inner, `"`)
		if !ok {
			return nil, fmt.Errorf("an element that is not a string: %q", inner)
		}

		end := strings.IndexByte(rest, '"')
		if end < 0 {
			return nil, fmt.Errorf("an unterminated string: %q", inner)
		}

		out = append(out, rest[:end])

		inner = strings.TrimSpace(rest[end+1:])
		if inner == "" {
			break
		}

		if inner, ok = strings.CutPrefix(inner, ","); !ok {
			return nil, fmt.Errorf("elements not separated by a comma: %q", inner)
		}

		inner = strings.TrimSpace(inner)
	}

	return out, nil
}

// lastLine is the last non-empty line of s.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")

	return lines[len(lines)-1]
}
