package scripts_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
		{name: "forward new series", requested: "v0.6.0", wantOutput: "tag=v0.6.0\nbranch=release/v0.6\n", wantSuccess: true},
		{name: "maintained older series hotfix", requested: "v0.4.3", wantOutput: "tag=v0.4.3\nbranch=release/v0.4\n", wantSuccess: true},
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
		binary:     "not executable on the control host\n",
	})
	if !strings.Contains(run.output, "Installed linux_amd64 billet to "+run.installed) {
		t.Fatalf("output = %q; want cross-target installation report", run.output)
	}
	if strings.Contains(run.requests, "darwin_arm64.tar.gz") || !strings.Contains(run.requests, "linux_amd64.tar.gz") {
		t.Fatalf("requests = %q; want only the linux_amd64 archive", run.requests)
	}
	if _, err := os.Stat(run.marker); !os.IsNotExist(err) {
		t.Fatalf("foreign binary execution marker error = %v; want not-exist", err)
	}
	assertFileEquals(t, run.installed, "not executable on the control host\n")
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
		binary:     "foreign\n",
	})
	if !strings.Contains(run.output, "Installed linux_amd64 billet to "+run.installed) {
		t.Fatalf("output = %q; want cross-target installation report", run.output)
	}
	if strings.Contains(run.output, "does not build for macOS on Intel") {
		t.Fatalf("output contains a fatal host diagnostic: %q", run.output)
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

type installerFixture struct {
	hostOS      string
	hostArch    string
	targetOS    *string
	targetArch  *string
	binary      string
	badChecksum bool
}

type installerRun struct {
	output    string
	requests  string
	installed string
	marker    string
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
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatalf("create tools: %v", err)
	}
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatalf("create install directory: %v", err)
	}

	archive := filepath.Join(root, "archive")
	archiveBody := []byte("fixture archive\n")
	if err := os.WriteFile(archive, archiveBody, 0o600); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	sum := sha256.Sum256(archiveBody)
	if fixture.badChecksum {
		sum = sha256.Sum256([]byte("different archive\n"))
	}
	checksums := filepath.Join(root, "checksums.txt")
	checksumBody := fmt.Sprintf("%x  billet_0.0.0_linux_amd64.tar.gz\n%x  billet_0.0.0_darwin_arm64.tar.gz\n", sum, sum)
	if err := os.WriteFile(checksums, []byte(checksumBody), 0o600); err != nil {
		t.Fatalf("write checksums: %v", err)
	}
	binary := filepath.Join(root, "fixture-billet")
	if err := os.WriteFile(binary, []byte(fixture.binary), 0o700); err != nil {
		t.Fatalf("write fixture binary: %v", err)
	}

	writeExecutable(t, filepath.Join(tools, "uname"), fmt.Sprintf(`#!/bin/sh
case "$1" in
    -s) printf '%%s\n' %q ;;
    -m) printf '%%s\n' %q ;;
    *) exit 2 ;;
esac
`, fixture.hostOS, fixture.hostArch))
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
    *.tar.gz) cp "$BILLET_TEST_ARCHIVE" "$output" ;;
    *) exit 2 ;;
esac
`)
	writeExecutable(t, filepath.Join(tools, "tar"), `#!/bin/sh
[ "$1" = -xzf ] && [ "$3" = -C ] || exit 2
cp "$BILLET_TEST_BINARY_SOURCE" "$4/billet"
`)

	requests := filepath.Join(root, "requests")
	marker := filepath.Join(root, "executed")
	cmd := exec.CommandContext(t.Context(), "sh", installer)
	cmd.Env = append(environmentWithout("BILLET_OS", "BILLET_ARCH"),
		"PATH="+tools+":"+os.Getenv("PATH"),
		"BILLET_INSTALL_DIR="+installDir,
		"BILLET_TEST_ARCHIVE="+archive,
		"BILLET_TEST_BINARY_SOURCE="+binary,
		"BILLET_TEST_CHECKSUMS="+checksums,
		"BILLET_TEST_CURL_LOG="+requests,
		"BILLET_TEST_MARKER="+marker,
	)
	if fixture.targetOS != nil {
		cmd.Env = append(cmd.Env, "BILLET_OS="+*fixture.targetOS)
	}
	if fixture.targetArch != nil {
		cmd.Env = append(cmd.Env, "BILLET_ARCH="+*fixture.targetArch)
	}
	output, runErr := cmd.CombinedOutput()
	requestsBody, err := os.ReadFile(requests)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read requests: %v", err)
	}
	return installerRun{
		output:    string(output),
		requests:  string(requestsBody),
		installed: filepath.Join(installDir, "billet"),
		marker:    marker,
	}, runErr
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
