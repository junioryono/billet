package host

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
)

// LVM TAKES A DEVICE'S FIRST MATCHING PATTERN, and a device none matches is scanned:
// what the host role writes rejects both mapped-image names, and nothing else here
// does.
func TestTheLVMFilterIsJudgedAsLVMReadsIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, answer, scanned string
		err                   bool
	}{
		{name: "the host role's filter", answer: `global_filter=["r|^/dev/rbd|","r|^/dev/nbd|"]`},
		{name: "no filter", answer: "", scanned: "/dev/rbd0, /dev/nbd0"},
		{name: "an accept-all ahead of the rejects",
			answer: `global_filter=["a|.*|","r|^/dev/rbd|","r|^/dev/nbd|"]`, scanned: "/dev/rbd0, /dev/nbd0"},
		{name: "only RBD rejected", answer: `global_filter=["r|^/dev/rbd|"]`, scanned: "/dev/nbd0"},
		{name: "an operator's filter on another disk", answer: `global_filter=["r|^/dev/sdz|","a|.*|"]`,
			scanned: "/dev/rbd0, /dev/nbd0"},
		{name: "another delimiter", answer: `global_filter=["r/rbd/","r/nbd/"]`},
		{name: "a trailing newline and spaces", answer: "global_filter=[ \"r|^/dev/rbd|\" , \"r|^/dev/nbd|\" ]\n"},
		{name: "not the setting asked for", answer: `filter=["r|.*|"]`, err: true},
		{name: "not a list", answer: `global_filter="r|.*|"`, err: true},
		{name: "a pattern that is neither a nor r", answer: `global_filter=["x|rbd|"]`, err: true},
		{name: "an unterminated pattern", answer: `global_filter=["r|rbd"]`, err: true},
		{name: "an expression Go cannot read", answer: `global_filter=["r|(rbd|"]`, err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			scanned, err := lvmFilterRejects(tc.answer, lvmMappedDevices)
			if tc.err {
				if err == nil {
					t.Fatalf("%q read without an error, as scanning %q", tc.answer, scanned)
				}

				return
			}
			if err != nil {
				t.Fatalf("%q: %v", tc.answer, err)
			}
			if scanned != tc.scanned {
				t.Errorf("%q scans %q, want %q", tc.answer, scanned, tc.scanned)
			}
		})
	}
}

// THREE ANSWERS, AND NO LVM IS A FOURTH: the filter there, the filter missing, and
// could-not-tell, which an exit status other than "not found"'s 5 is.
//
// Not parallel: it replaces the package's lvmConfig.
func TestBilletCheckSaysWhetherLVMScansTheMappedImages(t *testing.T) {
	saved := lvmConfig
	t.Cleanup(func() { lvmConfig = saved })

	type answer struct {
		out, stderr string
		exit        int
		err         error
	}

	filter := answer{out: `global_filter=["r|^/dev/rbd|","r|^/dev/nbd|"]` + "\n"}
	noDevicesFile := answer{out: "use_devicesfile=0\n"}

	for _, tc := range []struct {
		name          string
		filter, files answer
		want          string
		absent        string
	}{
		{name: "the filter", filter: filter, files: noDevicesFile,
			want: "LVM's global_filter keeps its scans off the RBD and NBD devices", absent: "devices file"},
		{name: "the filter beside a devices file", filter: filter, files: answer{out: "use_devicesfile=1\n"},
			want: "a devices file is in use"},
		{name: "no filter", filter: answer{stderr: "  Configuration node devices/global_filter not found\n", exit: 5},
			want: "WARNING: LVM's scans reach the RBD images this host maps (/dev/rbd0, /dev/nbd0)"},
		{name: "a filter that lets RBD through", filter: answer{out: `global_filter=["r|^/dev/nbd|"]`},
			want: "WARNING: LVM's scans reach the RBD images this host maps (/dev/rbd0)"},
		{name: "a configuration it could not read",
			filter: answer{stderr: "  Failed to load config file /etc/lvm/lvmlocal.conf\n", exit: 4},
			want:   "UNCHECKED: lvmconfig devices/global_filter exited 4: Failed to load config file", absent: "WARNING"},
		{name: "exit 5 for anything but not found", filter: answer{stderr: "  something else\n", exit: 5},
			want: "UNCHECKED", absent: "WARNING"},
		{name: "an lvmconfig that could not run", filter: answer{err: errors.New("signal: killed")},
			want: "UNCHECKED: could not run lvmconfig: signal: killed", absent: "WARNING"},
		{name: "an answer it cannot read", filter: answer{out: "global_filter=nonsense"},
			want: "UNCHECKED: could not read LVM's global_filter", absent: "WARNING"},
		{name: "no LVM", filter: answer{err: errNoLVM}, want: "no LVM on this host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lvmConfig = func(_ context.Context, args ...string) (string, string, int, error) {
				a := tc.filter
				if strings.Join(args, " ") == "--typeconfig full devices/use_devicesfile" {
					a = tc.files
				} else if strings.Join(args, " ") != "devices/global_filter" {
					t.Fatalf("lvmconfig asked %v", args)
				}

				return a.out, a.stderr, a.exit, a.err
			}

			var out bytes.Buffer

			reportLVMFilter(t.Context(), cli.Env{Stdout: &out, Stderr: &out, Getenv: func(string) string { return "" }})

			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("said %q, want it to say %q", out.String(), tc.want)
			}
			if tc.absent != "" && strings.Contains(out.String(), tc.absent) {
				t.Errorf("said %q, which it must not for this answer", out.String())
			}
		})
	}
}
