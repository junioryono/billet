package runnerrelease

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE SHELL READS LINE 1 AND ONLY LINE 1, and this is what keeps that true.
//
// scripts/build-guest-image.sh takes the checksum with `awk "NR==1{print $2}"`, so
// the platform table was APPENDED rather than replacing the original shape. Nothing
// in Go can notice if line 1 stops being `<version> <sha256>` — the guest image
// build is what breaks, on a machine running as root, hours later.
func TestTheFirstLineStillCarriesTheVersionAndOneChecksum(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("pinned.txt")
	if err != nil {
		t.Fatalf("read pinned.txt: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 0 {
		t.Fatal("pinned.txt is empty")
	}

	fields := strings.Fields(lines[0])
	if len(fields) != 2 {
		t.Fatalf("line 1 has %d fields (%q), want exactly 2: the guest image build reads "+
			"field 2 with awk and a third field would silently change what it installs",
			len(fields), lines[0])
	}

	if fields[0] != Pinned() {
		t.Errorf("line 1's version %q is not what Pinned() reports (%q)", fields[0], Pinned())
	}

	if fields[1] != PinnedSHA256() {
		t.Errorf("line 1's checksum %q is not what PinnedSHA256() reports (%q)",
			fields[1], PinnedSHA256())
	}
}

// A CHECKSUM MUST NOT CARRY A NEWLINE, which is the exact defect extending this file
// introduced and the reason the accessors now read line 1 rather than cutting the
// whole embedded string at its first space.
//
// The old PinnedSHA256 was `Cut(TrimSpace(pinned), " ")`, correct for a one-line
// file and silently returning the entire platform table for a four-line one. What
// makes it worth a test rather than a comment is that the result still LOOKS like a
// checksum at the start, and the download it verifies fails with a mismatch that
// names no reason.
func TestNoPinnedChecksumCarriesWhitespace(t *testing.T) {
	t.Parallel()

	for _, platform := range []string{"linux-x64", "linux-arm64", "osx-arm64"} {
		sum, ok := PinnedSHA256For(platform)
		if !ok {
			t.Errorf("no checksum is pinned for %s", platform)

			continue
		}

		if strings.ContainsAny(sum, " \t\n\r") {
			t.Errorf("the %s checksum contains whitespace: %q", platform, sum)
		}

		if len(sum) != 64 {
			t.Errorf("the %s checksum is %d characters, want 64: %q", platform, len(sum), sum)
		}

		for _, r := range sum {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				t.Errorf("the %s checksum is not lowercase hex: %q", platform, sum)

				break
			}
		}
	}

	if got := PinnedSHA256(); strings.ContainsAny(got, " \t\n\r") || len(got) != 64 {
		t.Errorf("PinnedSHA256() = %q, which is not one bare checksum", got)
	}
}

// LINE 1 IS linux-x64's ENTRY, RESOLVED RATHER THAN REPEATED. A duplicate would be
// the two-pins problem this file exists to remove: a bump that updated one copy
// would verify the arm64 download correctly and the x64 one against a stale number.
func TestTheX64ChecksumIsNotStoredTwice(t *testing.T) {
	t.Parallel()

	sum, ok := PinnedSHA256For("linux-x64")
	if !ok {
		t.Fatal("no checksum is pinned for linux-x64")
	}

	if sum != PinnedSHA256() {
		t.Errorf("PinnedSHA256For(linux-x64) = %q but PinnedSHA256() = %q; they must be one "+
			"value, not two that agree", sum, PinnedSHA256())
	}

	raw, err := os.ReadFile("pinned.txt")
	if err != nil {
		t.Fatalf("read pinned.txt: %v", err)
	}

	if strings.Contains(string(raw), "linux-x64") {
		t.Error("pinned.txt names linux-x64 in the table as well as on line 1; the checksum " +
			"would then exist twice and a bump could update one of them")
	}
}

// AN UNKNOWN PLATFORM ANSWERS false, NOT AN EMPTY STRING.
//
// This is the whole point of the second return value. An empty checksum handed to a
// verifying download is a verification that passes against anything, so a caller
// that cannot name a platform has to refuse the launch — and it can only do that if
// "billet pins nothing for this" is distinguishable from "the checksum is the empty
// string".
func TestAnUnpinnedPlatformIsRefusedRatherThanEmpty(t *testing.T) {
	t.Parallel()

	for _, platform := range []string{
		// Real actions/runner assets billet deliberately does not install: no
		// backend runs Windows, and no backend runs an x64 Mac.
		"win-x64", "osx-x64", "linux-arm",
		// And something that is not an asset at all.
		"", "moon-x64",
	} {
		if sum, ok := PinnedSHA256For(platform); ok {
			t.Errorf("PinnedSHA256For(%q) claimed a checksum %q", platform, sum)
		} else if sum != "" {
			t.Errorf("PinnedSHA256For(%q) returned %q alongside false", platform, sum)
		}
	}
}

// THE DAILY BUMP MUST WRITE THE WHOLE FILE, and this is the only place that can say
// so.
//
// The workflow that opens the pin-bump pull request wrote line 1 with `>`, which
// truncates the platform table away. Nothing in Go could notice: the file in the
// repository is correct, and the damage would appear in the pull request the next
// time actions/runner published — after which `PinnedSHA256For` has no checksum for
// linux-arm64 or osx-arm64, and a codebuild macOS or arm64 launch is refused
// outright, correctly and for a reason nobody would look for in a file that agreed
// with itself yesterday.
//
// A CHECKSUM IS ONLY TRUE OF ITS VERSION, so refreshing one row and not the others
// is the same bug wearing different clothes: the arm64 download of the new release
// would be verified against the old release's digest.
//
// THE RENDERING IS RUN, NOT READ. Two weaker versions of this test were written
// first and both were mutation-checked and found wanting: searching the whole file
// for platform names passes for a workflow that extracts every checksum and then
// writes line 1 alone, and searching the 500 bytes before the redirect passes for
// one whose `printf` commands have been replaced by no-ops. What the file must do
// is PRODUCE something, so the statement is extracted and executed against fixture
// values and the result is compared with the file it must have written.
func TestTheReleaseBumpWorkflowWritesEveryPinnedPlatform(t *testing.T) {
	t.Parallel()

	const (
		version   = "9.999.0"
		linuxX64  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		linuxArm  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		darwinArm = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	)

	rendered := runPinRendering(t, map[string]string{
		"VERSION":         version,
		"SHA":             linuxX64,
		"SHA_LINUX_ARM64": linuxArm,
		"SHA_OSX_ARM64":   darwinArm,
	})

	want := version + " " + linuxX64 + "\n" +
		"linux-arm64 " + linuxArm + "\n" +
		"osx-arm64 " + darwinArm + "\n"

	if rendered != want {
		t.Fatalf("the bump would write\n%q\nwant\n%q", rendered, want)
	}

	// AND WHAT IT WRITES COVERS WHAT THE FILE CARRIES. The rendering above is fixed
	// text; this is what fails when a platform is added to pinned.txt and the bump is
	// not taught to emit it — which would leave the new row frozen at whatever
	// release it was added on.
	for _, platform := range pinnedPlatforms(t) {
		if platform == primaryPinPlatform {
			// Line 1 carries it by position rather than by name.
			continue
		}

		if !strings.Contains(rendered, platform+" ") {
			t.Errorf("the bump emits no %s row, so that platform's checksum would freeze "+
				"at the release it was added on", platform)
		}
	}
}

// AND EACH PLATFORM'S CHECKSUM IS ITS OWN, which the rendering test cannot see.
//
// That test proves the marked block writes every variable it is GIVEN. It says
// nothing about which release-notes marker each variable was read from — so a
// copy-paste reading linux-arm64 into SHA_OSX_ARM64 survives it, and the resulting
// pull request pins Linux's digest as the macOS one. billet then verifies a macOS
// download against a Linux digest, the checksum fails, and every CodeBuild macOS
// launch stops with a mismatch nobody would look for in a file that agreed with
// itself yesterday.
//
// A DISTINCT DIGEST PER PLATFORM IN THE FIXTURE, because identical ones would make
// every wiring indistinguishable from every other.
func TestTheReleaseBumpWorkflowExtractsEachPlatformsOwnChecksum(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("the extraction is jq; nothing here can fake it")
	}

	digests := map[string]string{
		"linux-x64":   strings.Repeat("a", 64),
		"linux-arm64": strings.Repeat("b", 64),
		"osx-arm64":   strings.Repeat("c", 64),
		// A PLATFORM BILLET DOES NOT PIN, present in the real notes and a plausible
		// thing for a wrong marker to pick up.
		"win-x64": strings.Repeat("d", 64),
	}

	got := runShaExtraction(t, digests)

	for variable, platform := range map[string]string{
		"sha":             "linux-x64",
		"sha_linux_arm64": "linux-arm64",
		"sha_osx_arm64":   "osx-arm64",
	} {
		if got[variable] != digests[platform] {
			t.Errorf("%s holds %q; %s's digest is %q, and pinning another platform's "+
				"would fail every download of this one",
				variable, got[variable], platform, digests[platform])
		}
	}
}

// runShaExtraction executes the workflow's own checksum extraction against a fixture
// release body and returns the variables it set.
func runShaExtraction(t *testing.T, digests map[string]string) map[string]string {
	t.Helper()

	var notes strings.Builder

	for platform, digest := range digests {
		fmt.Fprintf(&notes, "<!-- BEGIN SHA %s -->%s<!-- END SHA %s -->\n",
			platform, digest, platform)
	}

	release, err := json.Marshal(map[string]string{"body": notes.String()})
	if err != nil {
		t.Fatalf("render the fixture release: %v", err)
	}

	block := workflowBlock(t, "BILLET_SHA_EXTRACT")

	out := filepath.Join(t.TempDir(), "vars")

	script := "set -euo pipefail\nbody=$(cat)\n" + dedent(block) + "\n" +
		"{ echo \"sha=$sha\"; echo \"sha_linux_arm64=$sha_linux_arm64\"; " +
		"echo \"sha_osx_arm64=$sha_osx_arm64\"; } > " + out + "\n"

	cmd := exec.CommandContext(t.Context(), "bash", "-c", script)
	cmd.Stdin = strings.NewReader(string(release))

	if combined, runErr := cmd.CombinedOutput(); runErr != nil {
		t.Fatalf("the bump's extraction does not run (%v):\n%s\n--- script ---\n%s",
			runErr, combined, script)
	}

	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the extraction set nothing: %v", err)
	}

	vars := map[string]string{}

	for line := range strings.SplitSeq(strings.TrimSpace(string(body)), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			vars[name] = value
		}
	}

	return vars
}

// workflowBlock is the region of the bump workflow between a marker pair.
//
// BY MARKERS rather than by line offsets, the shape
// scripts/cache_conformance_cached_test.go already uses: what is being tested is the
// workflow's own text, so the test names a region the workflow itself delimits.
func workflowBlock(t *testing.T, marker string) string {
	t.Helper()

	raw, err := os.ReadFile("../../.github/workflows/runner-release.yml")
	if err != nil {
		t.Fatalf("read the bump workflow: %v", err)
	}

	_, rest, found := strings.Cut(string(raw), marker+"_BEGIN")
	if !found {
		t.Fatalf("the bump no longer marks %s", marker)
	}

	_, rest, found = strings.Cut(rest, "\n")
	if !found {
		t.Fatalf("%s_BEGIN ends the file", marker)
	}

	block, _, found := strings.Cut(rest, marker+"_END")
	if !found {
		t.Fatalf("%s has no end marker", marker)
	}

	// The end marker sits inside a comment on its own line; drop that line.
	if at := strings.LastIndex(block, "\n#"); at >= 0 {
		block = block[:at]
	}

	return block
}

// runPinRendering executes the workflow's own pinned.txt statement with fixture
// values and returns what it wrote.
//
// EXTRACTED BY MARKERS rather than by guessing at line offsets, the shape
// scripts/cache_conformance_cached_test.go already uses for the cache assertion:
// what is being tested is the workflow's text, so the test has to name a region of
// it that the workflow itself delimits.
func runPinRendering(t *testing.T, env map[string]string) string {
	t.Helper()

	raw, err := os.ReadFile("../../.github/workflows/runner-release.yml")
	if err != nil {
		t.Fatalf("read the bump workflow: %v", err)
	}

	_, rest, found := strings.Cut(string(raw), "BILLET_PIN_RENDER_BEGIN")
	if !found {
		t.Fatal("the bump no longer marks the statement that writes pinned.txt")
	}

	_, rest, found = strings.Cut(rest, "\n")
	if !found {
		t.Fatal("the begin marker ends the file")
	}

	block, _, found := strings.Cut(rest, "BILLET_PIN_RENDER_END")
	if !found {
		t.Fatal("the statement that writes pinned.txt has no end marker")
	}

	// The end marker sits inside a comment on its own line; drop that line.
	if at := strings.LastIndex(block, "\n#"); at >= 0 {
		block = block[:at]
	}

	dir := t.TempDir()

	script := "set -euo pipefail\ncd " + dir + "\nmkdir -p internal/runnerrelease\n" +
		dedent(block) + "\n"

	cmd := exec.CommandContext(t.Context(), "bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+os.Getenv("PATH"))

	for name, value := range env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the bump's pinned.txt statement does not run (%v):\n%s\n--- script ---\n%s",
			err, out, script)
	}

	body, err := os.ReadFile(filepath.Join(dir, "internal", "runnerrelease", "pinned.txt"))
	if err != nil {
		t.Fatalf("the bump's statement wrote no pinned.txt: %v", err)
	}

	return string(body)
}

// dedent removes the common leading whitespace a YAML block scalar carries, so the
// extracted statement is a script rather than an indented fragment.
func dedent(block string) string {
	lines := strings.Split(strings.Trim(block, "\n"), "\n")

	indent := -1

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		if n := len(line) - len(strings.TrimLeft(line, " ")); indent < 0 || n < indent {
			indent = n
		}
	}

	for i, line := range lines {
		if len(line) >= indent {
			lines[i] = line[indent:]
		}
	}

	return strings.Join(lines, "\n")
}

// pinnedPlatforms is every platform the committed file carries, line 1 included.
func pinnedPlatforms(t *testing.T) []string {
	t.Helper()

	body, err := os.ReadFile("pinned.txt")
	if err != nil {
		t.Fatalf("read the pin: %v", err)
	}

	platforms := []string{primaryPinPlatform}

	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n")[1:] {
		name, _, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok {
			platforms = append(platforms, name)
		}
	}

	return platforms
}
