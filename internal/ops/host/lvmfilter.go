package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/cli"
)

// errNoLVMConfig says lvmconfig was not found where this process looked. It is not
// proof the host has no LVM: a user whose PATH omits /usr/sbin finds none either.
var errNoLVMConfig = errors.New("no lvmconfig on PATH or in /usr/sbin or /sbin")

// lvmConfig runs lvmconfig with args and answers its standard output, its standard
// error and its exit status. An lvmconfig that ran and exited non-zero is not an
// error; one that could not be found or run is.
var lvmConfig = func(ctx context.Context, args ...string) (string, string, int, error) {
	path, err := exec.LookPath("lvmconfig")
	if err != nil {
		for _, candidate := range []string{"/usr/sbin/lvmconfig", "/sbin/lvmconfig"} {
			if p, lookErr := exec.LookPath(candidate); lookErr == nil {
				path, err = p, nil

				break
			}
		}
	}
	if err != nil {
		return "", "", 0, errNoLVMConfig
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

// lvmRoleFilter is what lvmconfig answers for the global_filter the host role writes,
// and what the role itself reads back and requires (lvm-rbd-filter.yml).
const lvmRoleFilter = `global_filter=["r|^/dev/rbd|","r|^/dev/nbd|"]`

// reportLVMFilter says whether LVM's scans are kept off the RBD and NBD devices this
// host maps (#922). ADVISORY, like the pool's fill: it fails nothing.
//
// AN OSD COULD NOT RESTART FOR HALF AN HOUR ON 2026-11-25 because its activation's
// `lvs` read every mapped image's label, and those reads waited on placement groups
// whose only copy was on that OSD. The host role writes a global_filter rejecting
// them; this says so where an operator looks, and says when it is not there.
//
// THE ROLE'S FILTER, EXACTLY, OR NOTHING VOUCHED FOR. Whether some other filter keeps
// LVM off every mapped image depends on LVM's own regex dialect, its delimiters and
// every name a device has, and a second reading of those in Go is a way to certify a
// filter LVM reads differently. So only the filter the role writes is said to hold;
// any other is could-not-tell, and none at all is the warning.
//
// lvmconfig answers a setting nobody wrote with exit 5 and "not found" (2.03.16 and
// 2.03.31); any other failure, a configuration file it cannot read included (exit 4,
// measured as a user who cannot read a 0600 lvmlocal.conf, 2026-11-27), is
// could-not-tell. It runs as any user, with a warning on stderr.
func reportLVMFilter(ctx context.Context, env cli.Env) {
	out, stderr, exit, err := lvmConfig(ctx, "devices/global_filter")

	switch {
	case err != nil:
		fmt.Fprintf(env.Stdout, "lvm      UNCHECKED: could not run lvmconfig: %v\n", err)

		return
	case exit == 5 && strings.Contains(stderr, "not found"):
		fmt.Fprintf(env.Stdout, "lvm      WARNING: LVM sets no global_filter, so its scans read the RBD images this "+
			"host maps: an OSD that restarts while it holds their only copy can wait on its own scan for as "+
			"long as it is down (#922); the host role writes one rejecting /dev/rbd and /dev/nbd "+
			"(billet_lvm_rbd_filter)\n")

		return
	case exit != 0:
		fmt.Fprintf(env.Stdout, "lvm      UNCHECKED: lvmconfig devices/global_filter exited %d: %s\n",
			exit, strings.TrimSpace(lastLine(stderr)))

		return
	case strings.TrimSpace(out) != lvmRoleFilter:
		fmt.Fprintf(env.Stdout, "lvm      UNCHECKED: LVM applies %s, not the filter the host role writes, and "+
			"billet cannot tell whether it keeps LVM's scans off every RBD and NBD device this host maps\n",
			strings.TrimSpace(out))

		return
	}

	// A DEVICES FILE'S ENTRIES BYPASS THE FILTER, so the filter is vouched for only
	// once LVM says it uses none.
	files, stderr, exit, err := lvmConfig(ctx, "--typeconfig", "full", "devices/use_devicesfile")

	switch {
	case err != nil:
		fmt.Fprintf(env.Stdout, "lvm      UNCHECKED: the host role's global_filter is set, but whether LVM uses a "+
			"devices file, whose entries it does not govern, could not be read: %v\n", err)
	case exit != 0:
		fmt.Fprintf(env.Stdout, "lvm      UNCHECKED: the host role's global_filter is set, but whether LVM uses a "+
			"devices file, whose entries it does not govern, could not be read: exit %d: %s\n",
			exit, strings.TrimSpace(lastLine(stderr)))
	case strings.TrimSpace(files) == "use_devicesfile=0":
		fmt.Fprintf(env.Stdout, "lvm      the host role's global_filter keeps LVM's scans off the RBD and NBD "+
			"devices this host maps\n")
	case strings.TrimSpace(files) == "use_devicesfile=1":
		fmt.Fprintf(env.Stdout, "lvm      UNCHECKED: the host role's global_filter is set, but LVM uses a devices "+
			"file, and the filter does not govern the devices it names; the host role refuses one naming a "+
			"mapped image, and `lvmdevices` lists it\n")
	default:
		fmt.Fprintf(env.Stdout, "lvm      UNCHECKED: the host role's global_filter is set, but lvmconfig answered "+
			"%q for devices/use_devicesfile\n", strings.TrimSpace(files))
	}
}

// lastLine is the last non-empty line of s.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")

	return lines[len(lines)-1]
}
