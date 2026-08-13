package ec2

import (
	"strings"
	"testing"
)

// THE SCRIPT IS THE IMAGE. Everything a runner image contains is decided by this
// one string, and a mistake in it produces the failure this whole backend keeps
// trying to avoid: an instance that boots, reports success, and never starts a
// runner, leaving the job queued until GitHub gives up.
func TestTheProvisionScriptContainsWhatAnImageNeeds(t *testing.T) {
	t.Parallel()

	got := mustScript(t)

	// NOT installdependencies.sh, which the runner ships and which does not work on
	// Amazon Linux 2023: it reads /etc/os-release, finds ID_LIKE="fedora", and exits
	// non-zero, ending the build under set -e. A real build found that; this keeps
	// it from being reintroduced by someone reading the runner's own instructions.
	if strings.Contains(mustScript(t), "installdependencies.sh") {
		t.Error("the script calls installdependencies.sh, which fails on dnf distributions " +
			"it does not recognise and takes the whole build down with it")
	}

	for _, want := range []struct {
		fragment string
		why      string
	}{
		{"set -eux", "without -e a failed step is followed by poweroff, and billet images it"},
		{"dnf install -y docker", "workflows use service containers and docker build"},
		{"git", "actions/checkout wants it"},
		{"tar", "actions/setup-* unpack what they download"},
		{"useradd", "the runner refuses to run as root, and untrusted jobs must not"},
		{"usermod -aG docker runner", "the runner user has to reach the daemon"},
		{"actions-runner-linux-x64-2.328.0.tar.gz", "the release being installed"},
		{"libicu", "the runner is a .NET app and dies on globalization without ICU"},
		{"/usr/local/bin/billet-runner", "the entry point a tier names in command:"},
		{jitEnvVar, "the one variable billet's boot script exports"},
		{"poweroff", "the only signal that provisioning succeeded"},
	} {
		if !strings.Contains(got, want.fragment) {
			t.Errorf("the script is missing %q — %s", want.fragment, want.why)
		}
	}
}

// POWEROFF IS LAST, and that is the entire success protocol.
//
// billet learns that provisioning worked by seeing the instance stop ITSELF. If
// anything ran after the poweroff, or if the script could reach it without having
// installed the runner, billet would image a machine that is not a runner image
// and the failure would surface as queued jobs on somebody's repository.
func TestNothingRunsAfterTheSuccessSignal(t *testing.T) {
	t.Parallel()

	got, err := provisionScript(BuildSpec{RunnerVersion: "2.328.0", Arch: "arm64"})
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(got), "\n")

	if last := lines[len(lines)-1]; last != "poweroff" {
		t.Errorf("the script ends with %q, not poweroff: billet would wait for a stop that "+
			"never comes, or image a machine that stopped for another reason", last)
	}

	if n := strings.Count(got, "poweroff"); n != 1 {
		t.Errorf("poweroff appears %d times; a second one earlier would end the build before "+
			"the runner was installed", n)
	}
}

// THE ARCHITECTURE IS PART OF THE DOWNLOAD URL, so getting it wrong produces an
// image whose runner cannot execute — on a machine that otherwise looks healthy.
func TestTheRunnerArchitectureIsRefusedUnlessItIsOneAWSHas(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		arch    string
		wantErr bool
	}{
		{arch: "x64"},
		{arch: "arm64"},
		{arch: "amd64", wantErr: true},
		{arch: "aarch64", wantErr: true},
		{arch: "", wantErr: true},
	} {
		t.Run(tc.arch, func(t *testing.T) {
			t.Parallel()

			got, err := provisionScript(BuildSpec{RunnerVersion: "2.328.0", Arch: tc.arch})

			if tc.wantErr {
				if err == nil {
					t.Fatalf("architecture %q was accepted; the download would 404 and the "+
						"build would fail halfway through", tc.arch)
				}

				return
			}

			if err != nil {
				t.Fatalf("architecture %q was refused: %v", tc.arch, err)
			}

			if !strings.Contains(got, "actions-runner-linux-"+tc.arch+"-") {
				t.Errorf("the download URL does not name %q", tc.arch)
			}
		})
	}
}

// A BUILD WITHOUT A VERSION WOULD INSTALL NOTHING, and the URL it built would be
// a 404 that surfaces minutes into a build somebody is paying for.
func TestAProvisionScriptNeedsARunnerVersion(t *testing.T) {
	t.Parallel()

	if _, err := provisionScript(BuildSpec{Arch: "x64"}); err == nil {
		t.Fatal("a build with no runner version was accepted")
	}
}

// THE ENTRY POINT DROPS PRIVILEGES WITHOUT LOSING THE REGISTRATION.
//
// billet's boot script exports the JIT config and execs the tier's command AS
// ROOT. The runner refuses to run as root, and running untrusted work as root
// would give away the isolation this backend exists to provide — so something has
// to change user and carry one variable across. If that variable is dropped the
// runner starts, finds no registration, and exits, which reads as a machine that
// booted fine.
func TestTheEntryPointCarriesTheRegistrationAcrossTheUserChange(t *testing.T) {
	t.Parallel()

	got, err := provisionScript(BuildSpec{RunnerVersion: "2.328.0", Arch: "x64"})
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}

	start := strings.Index(got, "cat > /usr/local/bin/billet-runner")
	if start < 0 {
		t.Fatal("no entry point is written")
	}

	end := strings.Index(got[start:], "BILLETEOF\n")
	if end < 0 {
		t.Fatal("the entry point heredoc is not closed")
	}

	entry := got[start : start+end]

	if !strings.Contains(entry, "setpriv --reuid=runner") {
		t.Error("the entry point does not drop privileges, so the runner would run as root")
	}

	// ASSERTED THROUGH THE CONSTANT, so a rename cannot leave this green while the
	// image stops working. The boot script that exports this name uses jitEnvVar;
	// spelling it out on both sides made the two independently editable.
	if !strings.Contains(entry, jitEnvVar+`="$`+jitEnvVar+`"`) {
		t.Error("the entry point does not forward the JIT config across the user change; the " +
			"runner would start with no registration and exit, looking like a healthy boot")
	}

	// AND A WRITABLE HOME. setpriv does not reset the environment, so without this
	// the runner inherits cloud-init's HOME=/root, registers fine, and then fails
	// job steps that touch $HOME — the docker CLI's config, ~/.gitconfig, much of
	// the actions ecosystem.
	if !strings.Contains(entry, "HOME=/home/runner") {
		t.Error("the entry point does not give the runner a writable HOME; it would register " +
			"successfully and then fail jobs on anything that writes to $HOME")
	}

	if !strings.Contains(entry, "/opt/actions-runner/run.sh") {
		t.Error("the entry point does not exec the runner")
	}

	// AND IT WAITS FOR DOCKER FIRST. A verification run of a real image found the
	// daemon still starting when the runner would have been exec'd, and a container
	// running fine seven seconds later — so without this the first job on a fresh
	// instance can fail a `docker build` on a machine that is about to be healthy.
	if !strings.Contains(entry, "docker info") {
		t.Error("the entry point starts the runner without waiting for the Docker daemon; " +
			"the first job on a fresh instance can lose a race it will never lose again")
	}
}

// mustScript builds the ordinary script or fails the test.
func mustScript(t *testing.T) string {
	t.Helper()

	got, err := provisionScript(BuildSpec{RunnerVersion: "2.328.0", Arch: "x64"})
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}

	return got
}
