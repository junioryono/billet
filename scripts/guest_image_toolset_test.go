package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/runnerimages"
)

// TestTheBuildInstallsGitHubsDeclaredPackages drives the shell's own extraction
// and compares it against the Go reader's.
//
// TWO LANGUAGES READ ONE FILE, and the entire point of vendoring it was that they
// cannot disagree. A jq expression that sorted, dropped a group, or silently
// produced nothing would leave the guest image and the AMI carrying different
// package sets while both claimed to follow GitHub's declaration.
func TestTheBuildInstallsGitHubsDeclaredPackages(t *testing.T) {
	t.Parallel()

	got := runToolsetPackages(t)

	ts, err := runnerimages.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := ts.AptPackages()

	if len(got) != len(want) {
		t.Fatalf("the shell extracts %d packages and the Go reader %d; they read the same "+
			"file and must agree", len(got), len(want))
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("package %d is %q in the shell and %q in Go; ORDER matters, because it "+
				"decides which package resolves a shared dependency first", i, got[i], want[i])
		}
	}
}

// TestTheExtractionKeepsUpstreamOrder is the property `jq unique` would break.
//
// jq's `unique` SORTS, so the obvious way to deduplicate turns three ordered
// groups into one alphabetical list. That still installs every package, which is
// why it would survive a test that only compared sets.
func TestTheExtractionKeepsUpstreamOrder(t *testing.T) {
	t.Parallel()

	got := runToolsetPackages(t)

	ts, err := runnerimages.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	vitalAt := slices.Index(got, ts.Apt.VitalPackages[0])
	commonAt := slices.Index(got, ts.Apt.CommonPackages[0])
	cmdAt := slices.Index(got, ts.Apt.CmdPackages[0])

	if vitalAt < 0 || commonAt < 0 || cmdAt < 0 {
		t.Fatalf("a group's first package is missing entirely: vital=%d common=%d cmd=%d",
			vitalAt, commonAt, cmdAt)
	}

	if vitalAt >= commonAt || commonAt >= cmdAt {
		t.Errorf("groups are emitted out of order: vital at %d, common at %d, cmd at %d. "+
			"If this was `jq unique`, the list is alphabetical and every group boundary "+
			"is gone", vitalAt, commonAt, cmdAt)
	}

	if slices.IsSorted(got) {
		t.Error("the extracted list is fully sorted, which upstream's is not; something " +
			"reordered it")
	}
}

// TestTheDeclarationMustMatchItsPinBeforeAnythingIsBuilt covers the shell's own
// digest check.
//
// THE GO SIDE CHECKING IT IS NOT ENOUGH. This script reads the same file through
// jq and is the thing that actually builds the image, so a check living only in
// Go would leave the builder trusting whatever is on disk.
func TestTheDeclarationMustMatchItsPinBeforeAnythingIsBuilt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		toolset     string
		pin         string
		wantSuccess bool
	}{
		{
			name:        "the vendored file and its pin agree",
			toolset:     string(runnerimages.ToolsetBytes()),
			pin:         runnerimages.PinnedCommit() + " " + runnerimages.PinnedSHA256() + "\n",
			wantSuccess: true,
		},
		{
			name:    "an edited declaration stops the build",
			toolset: `{"apt":{"vital_packages":["curl"],"common_packages":[],"cmd_packages":[]}}`,
			pin:     runnerimages.PinnedCommit() + " " + runnerimages.PinnedSHA256() + "\n",
		},
		{
			name:    "an empty declaration stops the build",
			toolset: "",
			pin:     runnerimages.PinnedCommit() + " " + runnerimages.PinnedSHA256() + "\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()

			toolset := filepath.Join(dir, "toolset-2404.json")
			if err := os.WriteFile(toolset, []byte(tc.toolset), 0o600); err != nil {
				t.Fatalf("write the toolset: %v", err)
			}

			pin := filepath.Join(dir, "pinned.txt")
			if err := os.WriteFile(pin, []byte(tc.pin), 0o600); err != nil {
				t.Fatalf("write the pin: %v", err)
			}

			script := "#!/usr/bin/env bash\nset -euo pipefail\n" +
				"TOOLSET_FILE=" + toolset + "\nTOOLSET_PIN=" + pin + "\n" +
				guestImageFunction(t, "verify_toolset") + "\n" +
				"verify_toolset\necho VERIFIED\n"

			out, err := runHarness(t, script)

			if got := err == nil; got != tc.wantSuccess {
				t.Fatalf("verify_toolset success = %v, want %v\n%s", got, tc.wantSuccess, out)
			}

			// THE BUILD MUST NOT CONTINUE, not merely complain. A version that
			// printed a warning and carried on would satisfy an exit-status check
			// on the happy path and build an image from an unreviewed declaration.
			if got := strings.Contains(out, "VERIFIED"); got != tc.wantSuccess {
				t.Errorf("the build continued past verification = %v, want %v\n%s",
					got, tc.wantSuccess, out)
			}
		})
	}
}

// TestTheJavaPathsMatchOnBothSides. The build writes JAVA_HOME_<v>_X64 into the
// image and `runnerimages.JavaHomeVars` computes the same names and paths for the
// EC2 side; if they disagree, one backend exports a path the other does not have
// and a workflow reading the variable breaks on exactly one of them.
//
// THE PATH SHAPE IS ADOPTIUM'S PACKAGE LAYOUT, not a choice: temurin-<v>-jdk
// installs to /usr/lib/jvm/temurin-<v>-jdk-<arch>. A mismatch here is a variable
// pointing at a directory that does not exist, which fails worse than an unset
// one because setup-java trusts it instead of installing a JDK.
func TestTheJavaPathsMatchOnBothSides(t *testing.T) {
	t.Parallel()

	ts, err := runnerimages.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	vars := ts.JavaHomeVars("/usr/lib/jvm")
	build := readBuildScript(t)

	for _, v := range ts.Java.Versions {
		name := "JAVA_HOME_" + v + "_X64"

		want, ok := vars[name]
		if !ok {
			t.Errorf("the Go side computes no %s", name)

			continue
		}

		// THE PATH IS ADOPTIUM'S PACKAGE LAYOUT. The Go side is asked for the x64
		// spelling because JavaHomeVars names the variable setup-java reads, and
		// that name carries _X64 on both architectures.
		if got := "/usr/lib/jvm/temurin-" + v + "-jdk-amd64"; got != want {
			t.Errorf("%s: the layout is %q and the Go side computes %q", name, got, want)
		}
	}

	// THE TEMPLATE TAKES THE ARCHITECTURE, rather than baking amd64 into the path.
	// This used to assert the literal `-jdk-amd64`; that string is gone because an
	// arm64 image installs to `-jdk-arm64`, and an assertion on the old literal
	// would have read a parameterised writer as a missing one.
	if !strings.Contains(build, `JAVA_HOME_%s_X64=/usr/lib/jvm/temurin-%s-jdk-%s`) {
		t.Error("the build no longer writes JAVA_HOME_<version>_X64 in Adoptium's package " +
			"layout; setup-java would find a variable pointing nowhere")
	}

	if !strings.Contains(build, `printf 'JAVA_HOME=/usr/lib/jvm/temurin-%s-jdk-%s\n' "$default"`) {
		t.Error("the build no longer writes a default JAVA_HOME from the declared default " +
			"version")
	}

	// AND THE VALUE IT IS GIVEN IS RUN, NOT READ. The template above proves the
	// path is parameterised; this proves the parameter is dpkg's name for the
	// architecture, which is what Adoptium's package layout uses. Grepping alone
	// would accept a template fed the tool-cache spelling, producing
	// `temurin-21-jdk-x64` -- a directory that exists nowhere.
	for arch, want := range map[string]string{"x64": "amd64", "arm64": "arm64"} {
		if got := dpkgArchFor(t, arch); got != want {
			t.Errorf("billet_tc_set_arch maps %s to dpkg %q, want %q -- Adoptium installs "+
				"to /usr/lib/jvm/temurin-<v>-jdk-<dpkg arch>", arch, got, want)
		}
	}
}

// dpkgArchFor runs the installers' own architecture mapping and reports the dpkg
// spelling it derives.
func dpkgArchFor(t *testing.T, arch string) string {
	t.Helper()

	body := "#!/usr/bin/env bash\nset -euo pipefail\nBILLET_TC_ARCH=" + arch + "\n" +
		guestImageFunction(t, "billet_tc_set_arch") + "\n" +
		"billet_tc_set_arch\nprintf '%s' \"$BILLET_TC_DPKG\"\n"

	script := filepath.Join(t.TempDir(), "arch.sh")
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write the harness: %v", err)
	}

	out, err := exec.CommandContext(t.Context(), "bash", script).CombinedOutput()
	if err != nil {
		t.Fatalf("running the architecture mapping for %s: %v\n%s", arch, err, out)
	}

	return string(out)
}

func runToolsetPackages(t *testing.T) []string {
	t.Helper()

	toolset, err := filepath.Abs(filepath.Join("..", "internal", "runnerimages",
		"toolset-2404.json"))
	if err != nil {
		t.Fatalf("resolve the toolset: %v", err)
	}

	aliases, err := filepath.Abs(filepath.Join("..", "internal", "runnerimages",
		"apt-aliases.json"))
	if err != nil {
		t.Fatalf("resolve the aliases: %v", err)
	}

	script := "#!/usr/bin/env bash\nset -euo pipefail\nTOOLSET_FILE=" + toolset + "\n" +
		"APT_ALIASES=" + aliases + "\n" +
		guestImageFunction(t, "toolset_packages") + "\ntoolset_packages\n"

	out, err := runHarness(t, script)
	if err != nil {
		t.Fatalf("toolset_packages: %v\n%s", err, out)
	}

	var packages []string

	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line != "" {
			packages = append(packages, line)
		}
	}

	return packages
}

func runHarness(t *testing.T, script string) (string, error) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "run.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write the harness: %v", err)
	}

	// A SCRIPT THIS PROCESS JUST WROTE IS RUN THROUGH bash, NOT EXECUTED.
	//
	// ETXTBSY, and it is not hypothetical: CI failed twice with
	// "fork/exec …/run.sh: text file busy". os.WriteFile closes the file before the
	// exec, so single-threaded this is safe. In a PARALLEL test binary another
	// goroutine can fork for its own exec while this file's descriptor is still
	// open for writing, the child inherits it, and the kernel refuses to execve a
	// file any process holds open for writing until that child execs or exits. Go
	// narrows the window with O_CLOEXEC and ForkLock but does not close it
	// (golang/go#22315).
	//
	// Passing the path as an ARGUMENT means the file is only ever READ, so the race
	// cannot arise. Every harness here starts `#!/usr/bin/env bash`, and
	// `bash script args` propagates the script's exit status — which is what these
	// tests assert on.
	out, err := exec.CommandContext(t.Context(), "bash", path).CombinedOutput()

	return string(out), err
}
