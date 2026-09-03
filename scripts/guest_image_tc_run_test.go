package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// billet_tc_run IS THE WHOLE SEAM, and getting it wrong is not a subtle failure.
//
// The guest build assembles a filesystem it is NOT running, so anything that must
// see the target's own apt, libraries or interpreter goes through chroot. Drop
// the chroot and those commands run on the BUILD HOST: the build installs JDKs
// and Python into the machine doing the building, reports success, and ships an
// image with none of it. The EC2 build is the opposite case — it IS the target —
// and the same function must then run the command directly.
//
// A FAKE chroot ON PATH, so this observes what would be executed rather than
// asserting on the text of the function. Actually chrooting needs root and a real
// target root, neither of which a unit test should want.
func TestTheChrootIndirectionIsTheSeam(t *testing.T) {
	t.Parallel()

	bin := t.TempDir()

	// The fake prints its whole argument list, so a missing or extra argument is
	// visible rather than merely a different exit status.
	if err := os.WriteFile(filepath.Join(bin, "chroot"),
		[]byte("#!/bin/sh\nprintf 'CHROOT'\nfor a in \"$@\"; do printf ' %s' \"$a\"; done\n"+
			"printf '\\n'\n"), 0o755); err != nil {
		t.Fatalf("write the fake chroot: %v", err)
	}

	for _, tc := range []struct {
		name string
		root string
		want string
		deny string
	}{
		{
			// THE GUEST BUILD. Without this the installers run on the build host.
			name: "a target root means chroot",
			root: "/target",
			want: "CHROOT /target echo hello",
		},
		{
			// THE EC2 BUILD. The builder IS the target, so chrooting into "" is
			// both wrong and impossible.
			name: "no target root means run it here",
			root: "",
			want: "hello",
			deny: "CHROOT",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			script := ". " + toolcacheAssetPath + "\n" +
				"BILLET_TC_ROOT='" + tc.root + "' billet_tc_run echo hello\n"

			cmd := exec.CommandContext(t.Context(), "bash", "-c", script)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))

			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("billet_tc_run failed: %v\n%s", err, out)
			}

			got := strings.TrimSpace(string(out))
			if got != tc.want {
				t.Errorf("billet_tc_run with root %q ran %q, want %q", tc.root, got, tc.want)
			}

			if tc.deny != "" && strings.Contains(got, tc.deny) {
				t.Errorf("billet_tc_run with no target root still went through %s", tc.deny)
			}
		})
	}
}

// THE ENTRY POINT REFUSES AN INCOMPLETE CONTRACT rather than installing into the
// wrong place.
//
// Every one of these paths decides where several GiB of runtimes land. An unset
// BILLET_TC_DIR would make `$tc` empty and the installers write to /node,
// /Python and /go on whichever machine is running — so the check has to fire
// before anything is fetched, and it has to name which variable is missing.
func TestTheToolcacheEntryPointRefusesAnIncompleteContract(t *testing.T) {
	t.Parallel()

	// BILLET_TC_ROOT IS DELIBERATELY ABSENT FROM THIS LIST. Empty is what "the
	// target is this machine" looks like, so refusing it would refuse the EC2
	// build for being correct — it is checked for being SET, not non-empty.
	required := []string{
		"BILLET_TC_DIR",
		"BILLET_TC_IN_TARGET",
		"BILLET_TC_WORK",
		"BILLET_TC_TOOLSET",
		"BILLET_TC_ENV_FILE",
		// THE ARCHITECTURE IS REQUIRED RATHER THAN DEFAULTED, and belongs here for
		// the same reason as the rest: an unset one defaulting to x64 would build
		// an x64 toolcache onto an arm64 image, complete in every structural way
		// and wrong in every binary, with nothing failing until a job execs one.
		"BILLET_TC_ARCH",
	}

	for _, missing := range required {
		t.Run("without "+missing, func(t *testing.T) {
			t.Parallel()

			var b strings.Builder

			b.WriteString(". " + toolcacheAssetPath + "\n")
			b.WriteString("BILLET_TC_ROOT=''\n")

			for _, name := range required {
				if name == missing {
					continue
				}

				b.WriteString(name + "=/tmp/billet-unused\n")
			}

			b.WriteString("billet_install_toolcache\n")

			out, err := exec.CommandContext(t.Context(), "bash", "-c", b.String()).CombinedOutput()
			if err == nil {
				t.Fatalf("the entry point ran with %s unset, so several GiB of runtimes would "+
					"land somewhere nobody chose\n%s", missing, out)
			}

			if !strings.Contains(string(out), missing) {
				t.Errorf("the refusal does not name %s, so an operator cannot tell which of "+
					"six paths is wrong: %s", missing, out)
			}
		})
	}

	// AND AN UNSET ROOT IS STILL REFUSED, because "not set" and "set to empty"
	// are different states and only the second one means "this machine".
	t.Run("without BILLET_TC_ROOT at all", func(t *testing.T) {
		t.Parallel()

		var b strings.Builder

		b.WriteString(". " + toolcacheAssetPath + "\n")

		for _, name := range required {
			b.WriteString(name + "=/tmp/billet-unused\n")
		}

		b.WriteString("billet_install_toolcache\n")

		out, err := exec.CommandContext(t.Context(), "bash", "-c", b.String()).CombinedOutput()
		if err == nil {
			t.Fatalf("the entry point ran with BILLET_TC_ROOT never set\n%s", out)
		}

		if !strings.Contains(string(out), "BILLET_TC_ROOT") {
			t.Errorf("the refusal does not name BILLET_TC_ROOT: %s", out)
		}
	})
}
