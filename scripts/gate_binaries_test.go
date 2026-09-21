package scripts_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gateBinariesScript is the one definition of how the converge guard gate's
// backing binaries are built and checked.
var gateBinariesScript = filepath.Join("..", "ansible_collections", "junioryono", "billet", "tests", "gate-binaries.sh")

// stand-in is a Go program that answers `version` the way billet does, with the
// version its build stamps, so verify reads a real build's tags.
const standIn = `package main

import (
	"fmt"
	"os"
)

var version = "(devel)"

// rev is empty for a build without VCS metadata, which billet prints as the
// version alone.
var rev = "0123456789ab"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		if rev == "" {
			fmt.Printf("billet %s\n  go test\n", version)
		} else {
			fmt.Printf("billet %s %s\n  go test\n", version, rev)
		}
	}
}
`

func gateVersions(t *testing.T) []string {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "bash", gateBinariesScript, "versions").Output()
	if err != nil {
		t.Fatalf("gate-binaries.sh versions: %v", err)
	}

	return strings.Fields(string(out))
}

// buildStandIns builds one stand-in per gate version into a directory, stamping
// stamp(version) and building with tags.
func buildStandIns(t *testing.T, tags string, stamp func(string) string) string {
	t.Helper()

	return buildStandInsWith(t, tags, stamp, "")
}

// buildStandInsWith adds extra linker flags, such as clearing the revision.
func buildStandInsWith(t *testing.T, tags string, stamp func(string) string, ldflags string) string {
	t.Helper()

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module standin\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(standIn), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()

	for _, v := range gateVersions(t) {
		args := []string{"build", "-ldflags", strings.TrimSpace("-X main.version=" + stamp(v) + " " + ldflags), "-o", filepath.Join(dir, "billet-"+v)}
		if tags != "" {
			args = append(args, "-tags", tags)
		}

		cmd := exec.CommandContext(t.Context(), "go", append(args, ".")...)
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build the stand-in for %s: %v\n%s", v, err, out)
		}
	}

	return dir
}

func verifyGateBinaries(t *testing.T, dir string) (string, error) {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "bash", gateBinariesScript, "verify", dir).CombinedOutput()

	return string(out), err
}

func sameVersion(v string) string { return v }

// A PREBUILT SET IS USED ONLY WHEN IT IS WHAT THE GATE WOULD HAVE BUILT: each
// binary answers the version its name says and carries the crash seam's tag.
func TestTheGateAcceptsBinariesBuiltTheWayItBuildsThem(t *testing.T) {
	t.Parallel()

	dir := buildStandIns(t, "billetgatecrash", sameVersion)

	if out, err := verifyGateBinaries(t, dir); err != nil {
		t.Fatalf("a correct set was refused: %v\n%s", err, out)
	}
}

func TestTheGateRefusesABinaryStampedWithAnotherVersion(t *testing.T) {
	t.Parallel()

	dir := buildStandIns(t, "billetgatecrash", func(string) string { return "v9.9.9" })

	out, err := verifyGateBinaries(t, dir)
	if err == nil || !strings.Contains(out, "not v") {
		t.Fatalf("a binary reporting another version was accepted (err %v):\n%s", err, out)
	}
}

// WITHOUT THE CRASH SEAM the gate's crash cases would run against a binary that
// cannot crash where they ask it to.
func TestTheGateRefusesABinaryBuiltWithoutTheCrashSeam(t *testing.T) {
	t.Parallel()

	dir := buildStandIns(t, "", sameVersion)

	out, err := verifyGateBinaries(t, dir)
	if err == nil || !strings.Contains(out, "billetgatecrash") {
		t.Fatalf("a binary without the crash seam was accepted (err %v):\n%s", err, out)
	}
}

func TestTheGateRefusesAMissingBinary(t *testing.T) {
	t.Parallel()

	dir := buildStandIns(t, "billetgatecrash", sameVersion)

	versions := gateVersions(t)
	if err := os.Remove(filepath.Join(dir, "billet-"+versions[len(versions)-1])); err != nil {
		t.Fatal(err)
	}

	out, err := verifyGateBinaries(t, dir)
	if err == nil || !strings.Contains(out, "missing") {
		t.Fatalf("a set with a binary missing was accepted (err %v):\n%s", err, out)
	}
}

// A BUILD WITHOUT VCS METADATA prints the version alone, and is still the right
// binary.
func TestTheGateAcceptsABinaryThatPrintsNoRevision(t *testing.T) {
	t.Parallel()

	dir := buildStandInsWith(t, "billetgatecrash", sameVersion, "-X main.rev=")

	if out, err := verifyGateBinaries(t, dir); err != nil {
		t.Fatalf("a correct set without revisions was refused: %v\n%s", err, out)
	}
}

// provided is what one run of gate-binaries.sh provide left: its output and
// error, the directory it provided into, and every recorded go invocation.
type provided struct {
	out, dir, goCalls string
	err               error
}

// provide runs gate-binaries.sh provide into a fresh directory with
// BILLET_GATE_PREBUILT set to prebuilt, and a `go` on PATH that records every
// invocation before running the real one.
func provide(t *testing.T, prebuilt string) provided {
	t.Helper()

	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}

	fakes := t.TempDir()
	calls := filepath.Join(t.TempDir(), "go-calls")

	shim := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>" + calls + "\nexec " + realGo + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(fakes, "go"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "bins")

	cmd := exec.CommandContext(t.Context(), "bash", gateBinariesScript, "provide", dir)
	cmd.Env = append(os.Environ(), "PATH="+fakes+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BILLET_GATE_PREBUILT="+prebuilt)

	out, runErr := cmd.CombinedOutput()

	recorded, err := os.ReadFile(calls)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}

	return provided{out: string(out), dir: dir, goCalls: string(recorded), err: runErr}
}

// A VERIFIED PREBUILT SET IS WHAT THE GATE USES, and nothing is built.
func TestProvideUsesAVerifiedPrebuiltSetAndBuildsNothing(t *testing.T) {
	t.Parallel()

	prebuilt := buildStandIns(t, "billetgatecrash", sameVersion)

	p := provide(t, prebuilt)
	if p.err != nil {
		t.Fatalf("provide refused a correct prebuilt set: %v\n%s", p.err, p.out)
	}

	if strings.Contains(p.goCalls, "build") {
		t.Errorf("provide built although a prebuilt set was given:\n%s", p.goCalls)
	}

	for _, v := range gateVersions(t) {
		want, err := os.ReadFile(filepath.Join(prebuilt, "billet-"+v))
		if err != nil {
			t.Fatal(err)
		}

		got, err := os.ReadFile(filepath.Join(p.dir, "billet-"+v))
		if err != nil {
			t.Fatalf("provide left no billet-%s: %v", v, err)
		}

		if !bytes.Equal(got, want) {
			t.Errorf("billet-%s is not the prebuilt file", v)
		}
	}
}

// AND AN UNVERIFIED ONE STOPS THE GATE with nothing handed on.
func TestProvideRefusesAPrebuiltSetThatFailsVerification(t *testing.T) {
	t.Parallel()

	prebuilt := buildStandIns(t, "", sameVersion)

	p := provide(t, prebuilt)
	if p.err == nil {
		t.Fatalf("provide accepted a set without the crash seam:\n%s", p.out)
	}

	if strings.Contains(p.goCalls, "build") {
		t.Errorf("provide fell back to building after refusing the prebuilt set:\n%s", p.goCalls)
	}

	entries, err := os.ReadDir(p.dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}

	if len(entries) != 0 {
		t.Errorf("provide handed on %d files after refusing the set", len(entries))
	}
}

// THE HARNESS GETS ITS BINARIES ONLY FROM provide, and a refusal fails it.
func TestTheGuardHarnessTakesItsBinariesFromProvide(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", "ansible_collections", "junioryono", "billet", "tests", "converge-guard-check.sh"))
	if err != nil {
		t.Fatal(err)
	}

	harness := string(raw)

	if !strings.Contains(harness, `"$here/gate-binaries.sh" provide "$bins" || fail `) {
		t.Error("converge-guard-check.sh does not take its backing binaries from gate-binaries.sh provide, failing on a refusal")
	}

	if strings.Contains(harness, "billetgatecrash -ldflags") || strings.Contains(harness, "BILLET_GATE_PREBUILT/") {
		t.Error("converge-guard-check.sh still builds or copies the backing binaries itself")
	}
}
