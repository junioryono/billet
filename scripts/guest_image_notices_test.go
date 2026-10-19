package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cutBlock returns the text of gate from the line after the one containing begin
// through the first line that is exactly end, inclusive.
func cutBlock(t *testing.T, gate, begin, end string) string {
	t.Helper()

	_, after, found := strings.Cut(gate, begin)
	if !found {
		t.Fatalf("check-guest-image.sh no longer has %q", begin)
	}
	_, after, _ = strings.Cut(after, "\n")
	block, _, closed := strings.Cut(after, "\n"+end+"\n")
	if !closed {
		t.Fatalf("check-guest-image.sh has no %q after %q", end, begin)
	}

	return block + "\n" + end + "\n"
}

// THE IMAGE REDISTRIBUTES BILLET AND THE CONTAINER HOOK, so the publication gate
// refuses an image that carries either without its notices. The block runs under the
// gate's own shell options, failure accumulator and final verdict, so what is
// asserted is the exit status that stops a publication, never pattern-matched.
func TestThePublicationGateRefusesAnImageWithoutItsNotices(t *testing.T) {
	t.Parallel()

	gate := readScriptFile(t, "check-guest-image.sh")
	block := cutBlock(t, gate, "# THE IMAGE REDISTRIBUTES BILLET AND THE CONTAINER HOOK", "fi")
	failDef := "fail() {\n" + cutBlock(t, gate, "\nfail() {", "}")
	verdict := "if [ \"$FAILED\" -ne 0 ]; then\n" + cutBlock(t, gate, "\nif [ \"$FAILED\" -ne 0 ]; then", "fi")
	const passDef = `pass() { echo "  ok    $*"; }`
	if !strings.Contains(gate, "\n"+passDef+"\n") || !strings.Contains(gate, "\nset -euo pipefail\n") ||
		!strings.Contains(gate, "\nFAILED=0\n") {
		t.Fatal("check-guest-image.sh no longer defines pass, its shell options or FAILED as this test runs them")
	}
	// THE VERDICT ABOVE IS THE ONE THE GATE REACHES only while nothing between the
	// block and the end clears the accumulator, so FAILED is written in exactly two
	// places: its initialisation and fail().
	if n := strings.Count(gate, "FAILED="); n != 2 || !strings.Contains(failDef, "\tFAILED=1\n") {
		t.Fatalf("check-guest-image.sh writes FAILED %d times, want only FAILED=0 at the start and FAILED=1 in fail()", n)
	}

	notices := "opt/billet/THIRD_PARTY_NOTICES"
	license := "usr/local/lib/billet/container-hook/LICENSE"
	bundled := "usr/local/lib/billet/container-hook/THIRD_PARTY_NOTICES"
	complete := map[string]string{notices: "notices\n", license: "MIT License\n", bundled: "undici\n"}
	without := func(path string, body ...string) map[string]string {
		files := map[string]string{}
		for p, b := range complete {
			if p != path {
				files[p] = b
			}
		}
		if len(body) > 0 {
			files[path] = body[0]
		}

		return files
	}

	for _, tc := range []struct {
		name   string
		files  map[string]string
		status int
	}{
		{"all present", complete, 0},
		{"no billet notices", without(notices), 1},
		{"no hook license", without(license), 1},
		{"no bundled notices", without(bundled), 1},
		{"an empty hook license", without(license, ""), 1},
		{"empty bundled notices", without(bundled, ""), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mnt := t.TempDir()
			for p, body := range tc.files {
				full := filepath.Join(mnt, p)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			script := "set -euo pipefail\n" + passDef + "\n" + failDef + "FAILED=0\nMNT=" + mnt + "\n" + block + verdict
			out, err := exec.CommandContext(t.Context(), "bash", "-c", script).CombinedOutput()
			status := 0
			if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
				status = exit.ExitCode()
			} else if err != nil {
				t.Fatalf("the block did not run: %v: %s", err, out)
			}
			if status != tc.status {
				t.Fatalf("the gate exited %d, want %d:\n%s", status, tc.status, out)
			}
			if tc.status != 0 && !strings.Contains(string(out), "without its notices") {
				t.Fatalf("the gate refused without naming the notices:\n%s", out)
			}
		})
	}
}

// The build installs every notice from files this tree carries, so the gate above
// is checking for something the build really writes.
func TestTheBuildInstallsTheNoticesTheGateChecks(t *testing.T) {
	t.Parallel()

	build := readScriptFile(t, "build-guest-image.sh")
	for source, dest := range map[string]string{
		"internal/notices/THIRD_PARTY_NOTICES":        `"$rootfs/opt/billet/THIRD_PARTY_NOTICES"`,
		"internal/guestassets/container-hook.LICENSE": `"$rootfs/usr/local/lib/billet/container-hook/LICENSE"`,
		"internal/guestassets/container-hook.NOTICE":  `"$rootfs/usr/local/lib/billet/container-hook/THIRD_PARTY_NOTICES"`,
	} {
		install := `install -m 0644 "$SCRIPT_DIR/../` + source + `" \` + "\n\t\t" + dest
		if !strings.Contains(build, install) {
			t.Errorf("build-guest-image.sh does not install %s as %s", source, dest)
		}
		if info, err := os.Stat(filepath.Join("..", source)); err != nil || info.Size() == 0 {
			t.Errorf("%s, which the build installs, is missing or empty: %v", source, err)
		}
	}
}

// THE BUNDLED NOTICES ARE THOSE OF THE PINNED HOOK. They name the version they were
// read for, so a pin bump that forgot to rewrite them, from the new tag's lockfile,
// fails here rather than shipping one release's notices with another's bundle.
func TestTheHookNoticesNameThePinnedVersion(t *testing.T) {
	t.Parallel()

	pin, err := os.ReadFile(filepath.Join("..", "internal", "guestassets", "container-hook.pin"))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(pin))
	if len(fields) != 2 {
		t.Fatalf("container-hook.pin is %q, want a version and a checksum", pin)
	}

	notice, err := os.ReadFile(filepath.Join("..", "internal", "guestassets", "container-hook.NOTICE"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "\nContainer hook version: " + fields[0] + "\n"; !strings.Contains(string(notice), want) {
		t.Fatalf("container-hook.NOTICE does not say %q; run scripts/container-hook-notices.py", strings.TrimSpace(want))
	}

	// Every component the script's EMBEDDED table names has its section, so a table
	// entry added without regenerating the file is caught here.
	script := readScriptFile(t, "container-hook-notices.py")
	entries := regexp.MustCompile(`\("([^"]+ [0-9.]+)", "([^"]+)",\n`).FindAllStringSubmatch(script, -1)
	if len(entries) == 0 {
		t.Fatal("found no EMBEDDED entry in container-hook-notices.py, so this test proves nothing")
	}
	for _, e := range entries {
		if want := "\n" + e[2] + ", embedded in " + e[1] + "\n"; !strings.Contains(string(notice), want) {
			t.Errorf("container-hook.NOTICE has no section %q; run scripts/container-hook-notices.py", strings.TrimSpace(want))
		}
	}
}
