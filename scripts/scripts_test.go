package scripts_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRepositoryKeyVerifierRequiresTheExactPrimaryKeySet(t *testing.T) {
	t.Parallel()

	pinned := strings.Repeat("A", 40)
	attacker := strings.Repeat("B", 40)
	verifier, err := filepath.Abs(filepath.Join("..", "ansible_collections", "junioryono", "billet", "roles", "development_host", "files", "verify-repository-key.sh"))
	if err != nil {
		t.Fatalf("absolute verifier path: %v", err)
	}
	for _, tc := range []struct {
		name         string
		fingerprints []string
		wantSuccess  bool
	}{
		{name: "one pinned primary", fingerprints: []string{pinned}, wantSuccess: true},
		{name: "pinned plus an extra primary", fingerprints: []string{pinned, attacker}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tools := t.TempDir()
			var output strings.Builder
			output.WriteString("#!/bin/sh\n")
			for _, fingerprint := range tc.fingerprints {
				output.WriteString("printf '%s\\n' 'pub:::::::::' 'fpr:::::::::")
				output.WriteString(fingerprint)
				output.WriteString(":'\n")
			}
			if err := os.WriteFile(filepath.Join(tools, "gpg"), []byte(output.String()), 0o755); err != nil {
				t.Fatalf("write fake gpg: %v", err)
			}
			cmd := exec.CommandContext(t.Context(), verifier, filepath.Join(tools, "bundle.asc"), pinned)
			cmd.Env = append(os.Environ(), "PATH="+tools+":"+os.Getenv("PATH"))
			err := cmd.Run()
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("verification error = %v; want success %t", err, tc.wantSuccess)
			}
		})
	}
}

func TestProductionSurfacesUseTheTestedSecurityHelpers(t *testing.T) {
	t.Parallel()

	assertContains(t,
		filepath.Join("..", "ansible_collections", "junioryono", "billet", "roles", "development_host", "tasks", "packages-linux.yml"),
		`- "{{ billet_development_apt_stage }}/verify-repository-key.sh"`)
	assertContains(t, filepath.Join("..", ".github", "workflows", "cut-release.yml"),
		"run: scripts/plan-release.sh")
	assertContains(t, filepath.Join("..", ".github", "workflows", "cut-release.yml"),
		"ansible_collections/junioryono/billet/galaxy.yml")
}

func TestReleasePlannerOrdersNewSeriesWithoutBlockingMaintainedHotfixes(t *testing.T) {
	t.Parallel()

	planner, err := filepath.Abs("plan-release.sh")
	if err != nil {
		t.Fatalf("absolute planner path: %v", err)
	}
	repository := t.TempDir()
	runGit(t, repository, "init", "--quiet")
	runGit(t, repository, "config", "user.name", "Billet Test")
	runGit(t, repository, "config", "user.email", "billet@example.invalid")
	runGit(t, repository, "commit", "--allow-empty", "--quiet", "-m", "fixture")
	runGit(t, repository, "tag", "v0.4.2")
	runGit(t, repository, "tag", "v0.5.0")

	for _, tc := range []struct {
		name        string
		requested   string
		wantOutput  string
		wantSuccess bool
	}{
		{name: "backward new series", requested: "v0.3.0"},
		{name: "forward new series", requested: "v0.6.0", wantOutput: "tag=v0.6.0\nbranch=release/v0.6\ncollection_version=0.6.0\n", wantSuccess: true},
		{name: "maintained older series hotfix", requested: "v0.4.3", wantOutput: "tag=v0.4.3\nbranch=release/v0.4\ncollection_version=0.4.3\n", wantSuccess: true},
		{name: "backward older series patch", requested: "v0.4.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			output := filepath.Join(t.TempDir(), "github-output")
			cmd := exec.CommandContext(t.Context(), planner)
			cmd.Dir = repository
			cmd.Env = append(os.Environ(), "REQUESTED="+tc.requested, "GITHUB_OUTPUT="+output)
			err := cmd.Run()
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("plan error = %v; want success %t", err, tc.wantSuccess)
			}
			if !tc.wantSuccess {
				return
			}
			body, err := os.ReadFile(output)
			if err != nil {
				t.Fatalf("read workflow output: %v", err)
			}
			if string(body) != tc.wantOutput {
				t.Fatalf("workflow output = %q; want %q", body, tc.wantOutput)
			}
		})
	}
}

func TestInstallerSelectsAndDoesNotExecuteACrossTargetBinary(t *testing.T) {
	t.Parallel()

	run := runInstaller(t, installerFixture{
		hostOS:     "Darwin",
		hostArch:   "arm64",
		targetOS:   stringPtr("linux"),
		targetArch: stringPtr("amd64"),
		binary:     "#!/bin/sh\nprintf 'executed\\n' > \"$BILLET_TEST_MARKER\"\nprintf 'foreign fixture\\n'\n",
	})
	if !strings.Contains(run.output, "Installed linux_amd64 billet to "+run.installed) {
		t.Fatalf("output = %q; want cross-target installation report", run.output)
	}
	if !strings.Contains(run.output, "Supply "+run.installed+" to your provisioning tool") {
		t.Fatalf("output = %q; want cross-target next step", run.output)
	}
	if strings.Contains(run.output, "billet github-app create") {
		t.Fatalf("output = %q; foreign binary must not be presented as runnable here", run.output)
	}
	if strings.Contains(run.requests, "darwin_arm64.tar.gz") || !strings.Contains(run.requests, "linux_amd64.tar.gz") {
		t.Fatalf("requests = %q; want only the linux_amd64 archive", run.requests)
	}
	if _, err := os.Stat(run.marker); !os.IsNotExist(err) {
		t.Fatalf("foreign binary execution marker error = %v; want not-exist", err)
	}
	assertFileEquals(t, run.installed, "#!/bin/sh\nprintf 'executed\\n' > \"$BILLET_TEST_MARKER\"\nprintf 'foreign fixture\\n'\n")
}

func TestInstallerExecutesANativeBinaryForItsVersion(t *testing.T) {
	t.Parallel()

	run := runInstaller(t, installerFixture{
		hostOS:   "Darwin",
		hostArch: "arm64",
		binary:   "#!/bin/sh\nprintf 'billet fixture 0.0.0\\n'\nprintf 'executed\\n' > \"$BILLET_TEST_MARKER\"\n",
	})
	if !strings.Contains(run.output, "Installed: billet fixture 0.0.0") {
		t.Fatalf("output = %q; want native version report", run.output)
	}
	if !strings.Contains(run.requests, "darwin_arm64.tar.gz") {
		t.Fatalf("requests = %q; want darwin_arm64 archive", run.requests)
	}
	assertFileEquals(t, run.marker, "executed\n")
	if run.unameRequests != "-s\n-m\n" {
		t.Fatalf("uname calls = %q; want one OS and one architecture lookup", run.unameRequests)
	}
}

func TestInstallerValidatesAnExplicitNativeTarget(t *testing.T) {
	t.Parallel()

	run := runInstaller(t, installerFixture{
		hostOS:     "Darwin",
		hostArch:   "arm64",
		targetOS:   stringPtr("darwin"),
		targetArch: stringPtr("arm64"),
		binary:     "#!/bin/sh\nprintf 'billet explicit native\\n'\nprintf 'executed\\n' > \"$BILLET_TEST_MARKER\"\n",
	})
	if !strings.Contains(run.output, "Installed: billet explicit native") {
		t.Fatalf("output = %q; want explicit native version report", run.output)
	}
	assertFileEquals(t, run.marker, "executed\n")
}

func TestInstallerSelectsLinuxARM64(t *testing.T) {
	t.Parallel()

	run := runInstaller(t, installerFixture{
		hostOS:     "Darwin",
		hostArch:   "arm64",
		targetOS:   stringPtr("linux"),
		targetArch: stringPtr("arm64"),
		binary:     "#!/bin/sh\nprintf 'executed\\n' > \"$BILLET_TEST_MARKER\"\n",
	})
	if !strings.Contains(run.requests, "linux_arm64.tar.gz") {
		t.Fatalf("requests = %q; want linux_arm64 archive", run.requests)
	}
	if _, err := os.Stat(run.marker); !os.IsNotExist(err) {
		t.Fatalf("foreign binary execution marker error = %v; want not-exist", err)
	}
	assertFileEquals(t, run.installed, "#!/bin/sh\nprintf 'executed\\n' > \"$BILLET_TEST_MARKER\"\n")
}

func TestInstallerRefusesEmptyOrPartialTargetPlatforms(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		targetOS   *string
		targetArch *string
		want       string
	}{
		{name: "only os", targetOS: stringPtr("linux"), want: "must be set together"},
		{name: "only arch", targetArch: stringPtr("amd64"), want: "must be set together"},
		{name: "empty os", targetOS: stringPtr(""), targetArch: stringPtr("amd64"), want: "must not be empty"},
		{name: "empty arch", targetOS: stringPtr("linux"), targetArch: stringPtr(""), want: "must not be empty"},
		{name: "both empty", targetOS: stringPtr(""), targetArch: stringPtr(""), want: "must not be empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			run := runInstallerExpectingFailure(t, installerFixture{
				hostOS:     "Darwin",
				hostArch:   "arm64",
				targetOS:   tc.targetOS,
				targetArch: tc.targetArch,
				binary:     "unused\n",
			})
			if !strings.Contains(run.output, tc.want) {
				t.Fatalf("output = %q; want %q", run.output, tc.want)
			}
		})
	}
}

func TestInstallerStagesForAnUnsupportedControlHost(t *testing.T) {
	t.Parallel()

	run := runInstaller(t, installerFixture{
		hostOS:     "Darwin",
		hostArch:   "x86_64",
		targetOS:   stringPtr("linux"),
		targetArch: stringPtr("amd64"),
		binary:     "#!/bin/sh\nprintf 'executed\\n' > \"$BILLET_TEST_MARKER\"\nprintf 'foreign fixture\\n'\n",
	})
	if !strings.Contains(run.output, "Installed linux_amd64 billet to "+run.installed) {
		t.Fatalf("output = %q; want cross-target installation report", run.output)
	}
	if strings.Contains(run.output, "does not build for macOS on Intel") {
		t.Fatalf("output contains a fatal host diagnostic: %q", run.output)
	}
	assertFileEquals(t, run.installed, "#!/bin/sh\nprintf 'executed\\n' > \"$BILLET_TEST_MARKER\"\nprintf 'foreign fixture\\n'\n")
	if _, err := os.Stat(run.marker); !os.IsNotExist(err) {
		t.Fatalf("foreign binary execution marker error = %v; want not-exist", err)
	}
}

func TestInstallerRefusesHostDetectionFailureForAnExplicitTarget(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		failFlag  string
		wantError string
		wantCalls string
	}{
		{name: "operating system", failFlag: "-s", wantError: "could not detect the host operating system", wantCalls: "-s\n"},
		{name: "architecture", failFlag: "-m", wantError: "could not detect the host architecture", wantCalls: "-s\n-m\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			run := runInstallerExpectingFailure(t, installerFixture{
				hostOS:        "Darwin",
				hostArch:      "arm64",
				targetOS:      stringPtr("linux"),
				targetArch:    stringPtr("amd64"),
				binary:        "unused\n",
				unameFailFlag: tc.failFlag,
			})
			if !strings.Contains(run.output, tc.wantError) {
				t.Fatalf("output = %q; want %q", run.output, tc.wantError)
			}
			if run.unameRequests != tc.wantCalls {
				t.Fatalf("uname calls = %q; want %q", run.unameRequests, tc.wantCalls)
			}
			if _, err := os.Stat(run.installed); !os.IsNotExist(err) {
				t.Fatalf("installed binary error = %v; want not-exist", err)
			}
		})
	}
}

func TestInstallerCannotBeDisabledByTheFormerTestHook(t *testing.T) {
	t.Parallel()

	run := runInstaller(t, installerFixture{
		hostOS:       "Darwin",
		hostArch:     "arm64",
		binary:       "#!/bin/sh\nprintf 'billet fixture 0.0.0\\n'\n",
		legacyBypass: true,
	})
	if !strings.Contains(run.output, "Installed: billet fixture 0.0.0") {
		t.Fatalf("output = %q; want production entrypoint to run", run.output)
	}
	assertFileEquals(t, run.installed, "#!/bin/sh\nprintf 'billet fixture 0.0.0\\n'\n")
}

func TestInstallerPreservesAnExistingBinaryWhenNativeValidationFails(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		binary string
		want   string
	}{
		{name: "command fails", binary: "#!/bin/sh\nexit 7\n", want: "could not run"},
		{name: "version is empty", binary: "#!/bin/sh\nexit 0\n", want: "reported no version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			run := runInstallerExpectingFailure(t, installerFixture{
				hostOS:    "Darwin",
				hostArch:  "arm64",
				binary:    tc.binary,
				oldBinary: "working billet\n",
			})
			if !strings.Contains(run.output, tc.want) {
				t.Fatalf("output = %q; want %q", run.output, tc.want)
			}
			assertFileEquals(t, run.installed, "working billet\n")
			matches, err := filepath.Glob(filepath.Join(filepath.Dir(run.installed), ".billet.incoming.*"))
			if err != nil {
				t.Fatalf("glob staging files: %v", err)
			}
			if len(matches) != 0 {
				t.Fatalf("staging residue = %v; want none", matches)
			}
		})
	}
}

func TestInstallerRefusesAChecksumMismatch(t *testing.T) {
	t.Parallel()

	run := runInstallerExpectingFailure(t, installerFixture{
		hostOS:      "Darwin",
		hostArch:    "arm64",
		binary:      "unused\n",
		badChecksum: true,
	})
	if !strings.Contains(run.output, "checksum mismatch") {
		t.Fatalf("output = %q; want checksum mismatch", run.output)
	}
	if _, err := os.Stat(run.installed); !os.IsNotExist(err) {
		t.Fatalf("installed binary error = %v; want not-exist", err)
	}
}

func TestInstallerRefusesUnsafeArchiveNames(t *testing.T) {
	t.Parallel()

	for _, archive := range []string{
		"../escape_darwin_arm64.tar.gz",
		"billét_darwin_arm64.tar.gz",
	} {
		t.Run(archive, func(t *testing.T) {
			t.Parallel()

			run := runInstallerExpectingFailure(t, installerFixture{
				hostOS:            "Darwin",
				hostArch:          "arm64",
				binary:            "unused\n",
				darwinArchiveName: archive,
			})
			if !strings.Contains(run.output, "unsafe archive name") {
				t.Fatalf("output = %q; want unsafe archive refusal", run.output)
			}
			if strings.Contains(run.requests, archive) {
				t.Fatalf("requests = %q; unsafe archive must not be downloaded", run.requests)
			}
		})
	}
}

func TestInstallerStreamsOnlyTheExpectedArchiveMember(t *testing.T) {
	t.Parallel()

	t.Run("traversal entry", func(t *testing.T) {
		t.Parallel()

		run := runInstaller(t, installerFixture{
			hostOS:        "Darwin",
			hostArch:      "arm64",
			binary:        "#!/bin/sh\nprintf 'billet safe archive fixture\n'\n",
			archiveThreat: "traversal",
		})
		if _, err := os.Stat(run.escaped); !os.IsNotExist(err) {
			t.Fatalf("archive escape path error = %v; want not-exist", err)
		}
		assertFileEquals(t, run.installed, "#!/bin/sh\nprintf 'billet safe archive fixture\n'\n")
	})

	t.Run("symlink payload", func(t *testing.T) {
		t.Parallel()

		run := runInstallerExpectingFailure(t, installerFixture{
			hostOS:        "Darwin",
			hostArch:      "arm64",
			binary:        "unused\n",
			archiveThreat: "symlink",
		})
		assertFileEquals(t, run.escaped, "outside stays unchanged\n")
		if _, err := os.Stat(run.installed); !os.IsNotExist(err) {
			t.Fatalf("installed binary error = %v; want not-exist", err)
		}
	})
}

func TestInstallerCleansUpAfterSignals(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		signal   syscall.Signal
		exitCode int
	}{
		{name: "interrupt", signal: syscall.SIGINT, exitCode: 130},
		{name: "terminate", signal: syscall.SIGTERM, exitCode: 143},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			run := runInstallerExpectingFailure(t, installerFixture{
				hostOS:    "Darwin",
				hostArch:  "arm64",
				binary:    "#!/bin/sh\nprintf 'ready\n' > \"$BILLET_TEST_MARKER\"\nwhile :; do sleep 1; done\n",
				oldBinary: "working billet\n",
				signal:    tc.signal,
			})
			if run.exitCode != tc.exitCode {
				t.Fatalf("exit code = %d; want %d; output = %q", run.exitCode, tc.exitCode, run.output)
			}
			assertFileEquals(t, run.installed, "working billet\n")
			assertNoInstallerResidue(t, run)
		})
	}
}

func TestInstallerUsesPrivilegedStagingAndCleanup(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		run := runInstaller(t, installerFixture{
			hostOS:     "Darwin",
			hostArch:   "arm64",
			binary:     "#!/bin/sh\nprintf 'billet privileged fixture\n'\n",
			oldBinary:  "working billet\n",
			privileged: true,
		})
		assertFileEquals(t, run.installed, "#!/bin/sh\nprintf 'billet privileged fixture\n'\n")
		for _, command := range []string{"mktemp", "cp", "chmod", "mv"} {
			if !strings.Contains(run.sudoRequests, command+"\n") {
				t.Fatalf("sudo requests = %q; want %s", run.sudoRequests, command)
			}
		}
	})

	t.Run("validation failure", func(t *testing.T) {
		t.Parallel()

		run := runInstallerExpectingFailure(t, installerFixture{
			hostOS:     "Darwin",
			hostArch:   "arm64",
			binary:     "#!/bin/sh\nexit 7\n",
			oldBinary:  "working billet\n",
			privileged: true,
		})
		assertFileEquals(t, run.installed, "working billet\n")
		if !strings.Contains(run.sudoRequests, "rm\n") {
			t.Fatalf("sudo requests = %q; want cleanup through sudo", run.sudoRequests)
		}
		assertNoInstallerResidue(t, run)
	})
}

func TestInstallerRefusesADirectoryDestination(t *testing.T) {
	t.Parallel()

	run := runInstallerExpectingFailure(t, installerFixture{
		hostOS:               "Darwin",
		hostArch:             "arm64",
		binary:               "#!/bin/sh\nprintf 'billet fixture\\n'\n",
		destinationDirectory: true,
	})
	if !strings.Contains(run.output, "is a directory") {
		t.Fatalf("output = %q; want directory refusal", run.output)
	}
	entries, err := os.ReadDir(run.installed)
	if err != nil {
		t.Fatalf("read destination directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("destination directory contains %v; want empty", entries)
	}
}

func TestInstallerDoesNotReuseTheFormerFixedStagingName(t *testing.T) {
	t.Parallel()

	run := runInstaller(t, installerFixture{
		hostOS:             "Darwin",
		hostArch:           "arm64",
		binary:             "#!/bin/sh\nprintf 'billet fixture\\n'\n",
		fixedStageSentinel: "do not overwrite\n",
	})
	assertFileEquals(t, filepath.Join(filepath.Dir(run.installed), ".billet.incoming"), "do not overwrite\n")
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(run.installed), ".billet.incoming.*"))
	if err != nil {
		t.Fatalf("glob staging files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("staging residue = %v; want none", matches)
	}
}

type installerFixture struct {
	hostOS               string
	hostArch             string
	targetOS             *string
	targetArch           *string
	binary               string
	badChecksum          bool
	oldBinary            string
	legacyBypass         bool
	unameFailFlag        string
	darwinArchiveName    string
	destinationDirectory bool
	fixedStageSentinel   string
	archiveThreat        string
	privileged           bool
	signal               syscall.Signal
}

type installerRun struct {
	output        string
	requests      string
	installed     string
	marker        string
	unameRequests string
	escaped       string
	tempParent    string
	sudoRequests  string
	exitCode      int
}

func runInstaller(t *testing.T, fixture installerFixture) installerRun {
	t.Helper()
	run, err := executeInstaller(t, fixture)
	if err != nil {
		t.Fatalf("run installer: %v\n%s", err, run.output)
	}
	return run
}

func runInstallerExpectingFailure(t *testing.T, fixture installerFixture) installerRun {
	t.Helper()
	run, err := executeInstaller(t, fixture)
	if err == nil {
		t.Fatalf("installer succeeded unexpectedly:\n%s", run.output)
	}
	return run
}

func executeInstaller(t *testing.T, fixture installerFixture) (installerRun, error) {
	t.Helper()

	installer, err := filepath.Abs("install.sh")
	if err != nil {
		t.Fatalf("absolute installer path: %v", err)
	}
	root := t.TempDir()
	tools := filepath.Join(root, "tools")
	installDir := filepath.Join(root, "install")
	tempParent := filepath.Join(root, "tmp")
	escaped := filepath.Join(root, "escaped")
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatalf("create tools: %v", err)
	}
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatalf("create install directory: %v", err)
	}
	if err := os.MkdirAll(tempParent, 0o755); err != nil {
		t.Fatalf("create temporary parent: %v", err)
	}

	selectedPlatform := fixturePlatform(fixture)
	linuxBinary := "#!/bin/sh\nprintf 'wrong linux fixture\\n'\n"
	linuxARMBinary := "#!/bin/sh\nprintf 'wrong linux arm fixture\\n'\n"
	darwinBinary := "#!/bin/sh\nprintf 'wrong darwin fixture\\n'\n"
	switch selectedPlatform {
	case "linux_amd64":
		linuxBinary = fixture.binary
	case "linux_arm64":
		linuxARMBinary = fixture.binary
	default:
		darwinBinary = fixture.binary
	}
	linuxArchive := writeInstallerArchive(t, root, "linux-amd64", linuxBinary)
	linuxARMArchive := writeInstallerArchive(t, root, "linux-arm64", linuxARMBinary)
	darwinArchive := writeInstallerArchive(t, root, "darwin", darwinBinary)
	if fixture.archiveThreat != "" {
		switch selectedPlatform {
		case "linux_amd64":
			linuxArchive = writeHostileInstallerArchive(t, root, "linux-amd64-hostile", fixture.archiveThreat, linuxBinary, escaped)
		case "linux_arm64":
			linuxARMArchive = writeHostileInstallerArchive(t, root, "linux-arm64-hostile", fixture.archiveThreat, linuxARMBinary, escaped)
		default:
			darwinArchive = writeHostileInstallerArchive(t, root, "darwin-hostile", fixture.archiveThreat, darwinBinary, escaped)
		}
	}
	linuxSum := fileSHA256(t, linuxArchive)
	linuxARMSum := fileSHA256(t, linuxARMArchive)
	darwinSum := fileSHA256(t, darwinArchive)
	if fixture.badChecksum {
		linuxSum = sha256.Sum256([]byte("different linux archive\n"))
		linuxARMSum = sha256.Sum256([]byte("different linux arm archive\n"))
		darwinSum = sha256.Sum256([]byte("different darwin archive\n"))
	}
	darwinArchiveName := fixture.darwinArchiveName
	if darwinArchiveName == "" {
		darwinArchiveName = "billet_0.0.0_darwin_arm64.tar.gz"
	}
	checksums := filepath.Join(root, "checksums.txt")
	checksumBody := fmt.Sprintf("%x  billet_0.0.0_linux_amd64.tar.gz\n%x  billet_0.0.0_linux_arm64.tar.gz\n%x  %s\n", linuxSum, linuxARMSum, darwinSum, darwinArchiveName)
	if err := os.WriteFile(checksums, []byte(checksumBody), 0o600); err != nil {
		t.Fatalf("write checksums: %v", err)
	}
	if fixture.oldBinary != "" {
		if err := os.WriteFile(filepath.Join(installDir, "billet"), []byte(fixture.oldBinary), 0o755); err != nil {
			t.Fatalf("write existing binary: %v", err)
		}
	}
	if fixture.destinationDirectory {
		if err := os.Mkdir(filepath.Join(installDir, "billet"), 0o755); err != nil {
			t.Fatalf("create destination directory: %v", err)
		}
	}
	if fixture.fixedStageSentinel != "" {
		if err := os.WriteFile(filepath.Join(installDir, ".billet.incoming"), []byte(fixture.fixedStageSentinel), 0o600); err != nil {
			t.Fatalf("write fixed staging sentinel: %v", err)
		}
	}

	uname := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$1" >> "$BILLET_TEST_UNAME_LOG"
[ "$1" != "$BILLET_TEST_UNAME_FAIL" ] || exit 1
case "$1" in
    -s) printf '%%s\n' %q ;;
    -m) printf '%%s\n' %q ;;
    *) exit 2 ;;
esac
`, fixture.hostOS, fixture.hostArch)
	writeExecutable(t, filepath.Join(tools, "uname"), uname)
	writeExecutable(t, filepath.Join(tools, "curl"), `#!/bin/sh
url=
output=
while [ "$#" -gt 0 ]; do
    case "$1" in
        -o) shift; output=$1 ;;
        https://*) url=$1 ;;
    esac
    shift
done
printf '%s\n' "$url" >> "$BILLET_TEST_CURL_LOG"
case "$url" in
    */checksums.txt) cp "$BILLET_TEST_CHECKSUMS" "$output" ;;
    *_linux_amd64.tar.gz) cp "$BILLET_TEST_LINUX_ARCHIVE" "$output" ;;
    *_linux_arm64.tar.gz) cp "$BILLET_TEST_LINUX_ARM_ARCHIVE" "$output" ;;
    *_darwin_arm64.tar.gz) cp "$BILLET_TEST_DARWIN_ARCHIVE" "$output" ;;
    *) exit 2 ;;
esac
`)

	requests := filepath.Join(root, "requests")
	unameRequests := filepath.Join(root, "uname-requests")
	sudoRequests := filepath.Join(root, "sudo-requests")
	marker := filepath.Join(root, "executed")
	if fixture.privileged {
		writeExecutable(t, filepath.Join(tools, "sudo"), `#!/bin/sh
printf '%s\n' "$1" >> "$BILLET_TEST_SUDO_LOG"
chmod u+w "$BILLET_TEST_INSTALL_DIR"
"$@"
status=$?
chmod u-w "$BILLET_TEST_INSTALL_DIR"
exit "$status"
`)
		if err := os.Chmod(installDir, 0o555); err != nil {
			t.Fatalf("make install directory non-writable: %v", err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(installDir, 0o755); err != nil && !os.IsNotExist(err) {
				t.Errorf("restore install directory permissions: %v", err)
			}
		})
		writable := exec.CommandContext(t.Context(), "sh", "-c", `[ -w "$1" ]`, "sh", installDir)
		if err := writable.Run(); err == nil {
			t.Skip("this environment still grants write access to a mode-0555 directory")
		}
	}
	cmd := exec.CommandContext(t.Context(), "sh", installer)
	cmd.Env = append(environmentWithout(
		"BILLET_ARCH",
		"BILLET_INSTALL_DIR",
		"BILLET_INSTALL_SH_TEST",
		"BILLET_OS",
		"BILLET_VERSION",
		"TMPDIR",
	),
		"PATH="+tools+":"+os.Getenv("PATH"),
		"BILLET_INSTALL_DIR="+installDir,
		"BILLET_TEST_CHECKSUMS="+checksums,
		"BILLET_TEST_CURL_LOG="+requests,
		"BILLET_TEST_DARWIN_ARCHIVE="+darwinArchive,
		"BILLET_TEST_LINUX_ARM_ARCHIVE="+linuxARMArchive,
		"BILLET_TEST_LINUX_ARCHIVE="+linuxArchive,
		"BILLET_TEST_INSTALL_DIR="+installDir,
		"BILLET_TEST_MARKER="+marker,
		"BILLET_TEST_SUDO_LOG="+sudoRequests,
		"BILLET_TEST_UNAME_FAIL="+fixture.unameFailFlag,
		"BILLET_TEST_UNAME_LOG="+unameRequests,
		"TMPDIR="+tempParent,
	)
	if fixture.legacyBypass {
		cmd.Env = append(cmd.Env, "BILLET_INSTALL_SH_TEST=1")
	}
	if fixture.targetOS != nil {
		cmd.Env = append(cmd.Env, "BILLET_OS="+*fixture.targetOS)
	}
	if fixture.targetArch != nil {
		cmd.Env = append(cmd.Env, "BILLET_ARCH="+*fixture.targetArch)
	}
	var output []byte
	var runErr error
	if fixture.signal != 0 {
		var combined bytes.Buffer
		cmd.Stdout = &combined
		cmd.Stderr = &combined
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start installer for signal: %v", err)
		}
		waitForFile(t, marker)
		if err := syscall.Kill(-cmd.Process.Pid, fixture.signal); err != nil {
			t.Fatalf("signal installer process group: %v", err)
		}
		runErr = cmd.Wait()
		output = combined.Bytes()
	} else {
		output, runErr = cmd.CombinedOutput()
	}
	requestsBody, err := os.ReadFile(requests)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read requests: %v", err)
	}
	unameRequestsBody, err := os.ReadFile(unameRequests)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read uname requests: %v", err)
	}
	sudoRequestsBody, err := os.ReadFile(sudoRequests)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read sudo requests: %v", err)
	}
	exitCode := 0
	var exitError *exec.ExitError
	if errors.As(runErr, &exitError) {
		exitCode = exitError.ExitCode()
	}
	return installerRun{
		output:        string(output),
		requests:      string(requestsBody),
		installed:     filepath.Join(installDir, "billet"),
		marker:        marker,
		unameRequests: string(unameRequestsBody),
		escaped:       escaped,
		tempParent:    tempParent,
		sudoRequests:  string(sudoRequestsBody),
		exitCode:      exitCode,
	}, runErr
}

func fixturePlatform(fixture installerFixture) string {
	if fixture.targetOS != nil && fixture.targetArch != nil && *fixture.targetOS != "" && *fixture.targetArch != "" {
		return *fixture.targetOS + "_" + *fixture.targetArch
	}
	if fixture.hostOS == "Linux" && (fixture.hostArch == "x86_64" || fixture.hostArch == "amd64") {
		return "linux_amd64"
	}
	if fixture.hostOS == "Linux" && (fixture.hostArch == "aarch64" || fixture.hostArch == "arm64") {
		return "linux_arm64"
	}
	return "darwin_arm64"
}

func writeInstallerArchive(t *testing.T, root, name, binary string) string {
	t.Helper()
	directory := filepath.Join(root, name+"-archive")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatalf("create %s archive directory: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(directory, "billet"), []byte(binary), 0o755); err != nil {
		t.Fatalf("write %s archive binary: %v", name, err)
	}
	archive := filepath.Join(root, name+".tar.gz")
	cmd := exec.CommandContext(t.Context(), "tar", "-czf", archive, "-C", directory, "billet")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create %s archive: %v\n%s", name, err, output)
	}
	return archive
}

func writeHostileInstallerArchive(t *testing.T, root, name, threat, binary, escaped string) string {
	t.Helper()

	archive := filepath.Join(root, name+".tar.gz")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatalf("create hostile archive: %v", err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	writeRegular := func(name, body string, mode int64) {
		t.Helper()
		header := &tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("write hostile archive header: %v", err)
		}
		if _, err := tarWriter.Write([]byte(body)); err != nil {
			t.Fatalf("write hostile archive body: %v", err)
		}
	}

	switch threat {
	case "traversal":
		writeRegular("../escaped", "archive escaped\n", 0o644)
		writeRegular("billet", binary, 0o755)
	case "symlink":
		if err := os.WriteFile(escaped, []byte("outside stays unchanged\n"), 0o600); err != nil {
			t.Fatalf("write symlink target: %v", err)
		}
		header := &tar.Header{Name: "billet", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: escaped}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("write hostile symlink header: %v", err)
		}
	default:
		t.Fatalf("unknown archive threat %q", threat)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close hostile tar: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close hostile gzip: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close hostile archive: %v", err)
	}
	return archive
}

func waitForFile(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := os.Stat(path)
		if err == nil {
			return
		}
		if !os.IsNotExist(err) {
			t.Fatalf("wait for %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertNoInstallerResidue(t *testing.T, run installerRun) {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(filepath.Dir(run.installed), ".billet.incoming.*"))
	if err != nil {
		t.Fatalf("glob staging files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("staging residue = %v; want none", matches)
	}
	entries, err := os.ReadDir(run.tempParent)
	if err != nil {
		t.Fatalf("read temporary parent: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary residue = %v; want none", entries)
	}
}

func fileSHA256(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return sha256.Sum256(body)
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertFileEquals(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if got := string(body); got != want {
		t.Fatalf("%s = %q; want %q", path, got, want)
	}
}

func stringPtr(value string) *string {
	return &value
}

func environmentWithout(names ...string) []string {
	blocked := make(map[string]struct{}, len(names))
	for _, name := range names {
		blocked[name] = struct{}{}
	}
	var environment []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, found := blocked[name]; !found {
			environment = append(environment, entry)
		}
	}
	return environment
}

func assertContains(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(body), want) {
		t.Fatalf("%s is not wired to tested helper %q", path, want)
	}
}

func runGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = directory
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
