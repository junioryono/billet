package ec2

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/runnerimages"
)

// WHAT ARRIVES ON THE BUILDER IS A PROGRAM, and this checks it as bash rather
// than as text.
//
// The installers are stripped of whole-line comments to fit EC2's user-data
// budget, so what runs there is not byte-identical to the file on disk. The strip
// that looks obvious -- `s/#.*$//` -- eats `${v#v}` and `${version#go}`, which
// are parameter expansions, and leaves something that fails to parse. Go cannot
// check shell syntax, so this runs bash over the delivered bytes.
// THE INSTALLERS THE BUILDER RECEIVES ARE A WORKING BASH PROGRAM.
//
// They travel as a gzipped tar now rather than as a heredoc, which changes where
// this has to look but not what it has to prove: the bytes a builder unpacks are
// the ones that must parse, and a build that discovers otherwise has already
// bought a machine.
func TestTheDeliveredInstallersAreAWorkingProgram(t *testing.T) {
	t.Parallel()

	body, _, err := payloadArchive()
	if err != nil {
		t.Fatalf("payloadArchive: %v", err)
	}

	installer := archiveMember(t, body, toolcacheAssetPath)

	if len(installer) == 0 {
		t.Fatalf("the payload carries no %s", toolcacheAssetPath)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "installers.sh")

	if err := os.WriteFile(path, installer, 0o600); err != nil {
		t.Fatalf("write the delivered installers: %v", err)
	}

	// RUN, NOT PATTERN-MATCHED. This file is bash and the strip that used to
	// shrink it for user data was measured wrong twice by reading it; `bash -n` is
	// the only reader whose opinion decides whether a builder can source it.
	if out, err := exec.CommandContext(t.Context(), "bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("the installers the builder receives do not parse: %v\n%s", err, out)
	}
}

// AND THE DECLARATION IT RECEIVES IS THE PROJECTION BILLET SENT.
//
// The projection is what the installers read; shipping the whole file costs a
// tenth of a budget on twenty keys nobody on this path reads, and shipping a
// STALE projection is worse than either, because every reader agrees with itself
// about the wrong thing.
func TestTheDeliveredToolsetMatchesWhatBilletSent(t *testing.T) {
	t.Parallel()

	body, _, err := payloadArchive()
	if err != nil {
		t.Fatalf("payloadArchive: %v", err)
	}

	got := archiveMember(t, body, toolsetPath)

	want, err := runnerimages.InstallerToolset()
	if err != nil {
		t.Fatalf("InstallerToolset: %v", err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("the declaration in the payload is %d bytes and the projection is %d; the "+
			"builder would install from a file billet did not send", len(got), len(want))
	}
}

// AND THE BUILDER CHECKS THE PAYLOAD BEFORE IT UNPACKS IT.
//
// A presigned URL says who may READ an object, not that the object is the one
// billet uploaded — so verifying is what makes an interfered-with bucket a failed
// build rather than a root shell running someone else's installers. ADR-005 makes
// this rule for the guest build and it holds here for the same reason: a check on
// only the Go side leaves the thing that actually installs the toolcache trusting
// whatever arrived.
func TestTheBuilderRefusesADeclarationThatIsNotThePinnedOne(t *testing.T) {
	t.Parallel()

	script, err := provisionScript(BuildSpec{
		RunnerVersion: "2.328.0", Arch: "x64",
		payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest,
	})
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}

	lines := strings.Split(script, "\n")

	verify := firstLineOf(t, lines, "sha256sum -c -")
	extract := firstLineOf(t, lines, "tar -xzf ")

	if verify > extract {
		t.Errorf("the payload is extracted at line %d and verified at line %d; a digest "+
			"checked after the archive is unpacked has already run whatever arrived",
			extract, verify)
	}

	if !strings.Contains(script, testPayloadDigest) {
		t.Error("the script does not carry the digest billet computed, so the check on the " +
			"builder compares against nothing billet chose")
	}
}

// archiveMember returns one file's bytes out of the gzipped tar the builder
// fetches.
func archiveMember(t *testing.T, body []byte, name string) []byte {
	t.Helper()

	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("read the payload: %v", err)
	}

	tr := tar.NewReader(zr)

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			t.Fatalf("walk the payload: %v", err)
		}

		if h.Name == name || h.Name == strings.TrimPrefix(name, "/") {
			out, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read %s from the payload: %v", name, err)
			}

			return out
		}
	}
}

// THE BUILDER RUNS THE SAME CODE THE GUEST BUILD RUNS, and the driver states the
// one thing that differs.
func TestTheBuilderRunsTheSharedInstallers(t *testing.T) {
	t.Parallel()

	script := mustScript(t)

	for _, want := range []struct {
		fragment string
		why      string
	}{
		{"bash -c '", "the installers are bash and this script is /bin/sh"},
		{". " + toolcacheAssetPath, "sourced, because a function does not cross a process"},
		{`BILLET_TC_ROOT=""`, "the builder IS the target, so nothing chroots"},
		{"BILLET_TC_DIR=" + toolcacheDir, "where the toolcache lands"},
		{"BILLET_TC_IN_TARGET=" + toolcacheDir, "and how the runner will see it"},
		{"BILLET_TC_TOOLSET=" + toolsetPath, "the declaration that was just verified"},
		{"BILLET_TC_ENV_FILE=" + imageEnvFile, "so the JDKs can append JAVA_HOME"},
		{"billet_install_toolcache", "the one entry point"},
	} {
		if !strings.Contains(script, want.fragment) {
			t.Errorf("the driver is missing %q — %s", want.fragment, want.why)
		}
	}

	// NOTHING BILLET WROTE TO BUILD THE IMAGE STAYS IN IT.
	if !strings.Contains(script, "rm -rf "+toolcacheAssetPath) {
		t.Error("the build inputs are left in the image")
	}

	// AND THE CLEANUP IS AFTER THE INSTALL, not before it.
	lines := strings.Split(script, "\n")
	run := lineOf(t, lines, "  billet_install_toolcache")
	clean := lineOf(t, lines, "rm -rf "+toolcacheAssetPath)

	if clean <= run {
		t.Errorf("the build inputs are removed at line %d and used at %d", clean, run)
	}
}
