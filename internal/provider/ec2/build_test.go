package ec2

import (
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
		{"Runner.Listener --version", "an arch mismatch is otherwise invisible until a job"},
		{"setpriv --reuid=runner --regid=runner --init-groups \\\n  env HOME=/home/runner", "the check has to take the path a job takes"},
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

	// AND THE RUNNER IS PROVEN TO EXECUTE BEFORE IT. An arm64 tarball extracts
	// happily on x64, so without this the script reaches poweroff and billet
	// registers an image whose runner cannot exec — surfacing on somebody's first
	// job as a machine that booted and registered nothing.
	// THROUGH THE ENTRY POINT'S OWN INVOCATION, not around it. Using `sudo -u
	// runner` proved the binary runs and proved nothing about the path a job takes:
	// a base image with sudo but no setpriv would pass, be imaged, and fail every
	// job before the runner started.
	// BOTH USES COME FROM ONE STRING. Two hand-matched copies of a privilege drop
	// is how deleting --init-groups from the entry point left the validation
	// passing and every job failing — the check proved a COPY of the thing it was
	// meant to prove.
	// WHAT THE STRING CONTAINS, not only that both uses share it. Counting
	// occurrences of the constant proves the copies match and says nothing about
	// whether the invocation is right — deleting --init-groups from the constant
	// changes both consistently and the count stays 2. My own harness caught that.
	for _, required := range []struct {
		fragment string
		why      string
	}{
		{"--reuid=runner", "the runner refuses to run as root and untrusted jobs must not"},
		{"--init-groups", "setpriv requires a supplementary-group option when it sets the " +
			"primary GID, and without it the runner never gets the docker group"},
		{"HOME=/home/runner", "setpriv does not reset the environment, so without this the " +
			"runner inherits an unwritable HOME=/root and fails jobs, not registration"},
	} {
		if !strings.Contains(privilegeDrop, required.fragment) {
			t.Errorf("the privilege drop is missing %q — %s", required.fragment, required.why)
		}
	}

	if n := strings.Count(got, privilegeDrop); n != 2 {
		t.Errorf("the privilege drop appears %d times as the shared constant, want 2 (the "+
			"entry point and the validation that proves it); a hand-written second copy "+
			"means a change to one silently stops being true of the other", n)
	}

	if strings.Contains(got, "sudo -u runner") {
		t.Error("the validation uses sudo rather than the entry point's own setpriv, so a base " +
			"image lacking setpriv would build successfully and fail every job")
	}

	version := strings.Index(got, "Runner.Listener --version")
	power := strings.LastIndex(got, "poweroff")

	if version < 0 || version > power {
		t.Error("the runner is not executed before the success signal, so an architecture " +
			"mismatch would produce a registered image that cannot run a job")
	}

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

	for fragment, why := range map[string]string{
		`ACTIONS_RUNNER_HOOK_JOB_STARTED=/usr/local/bin/billet-job-started.sh`: "the runner would not invoke the cold-start measurement hook",
		`BILLET_LAUNCH_EPOCH_NS="${BILLET_LAUNCH_EPOCH_NS:-}"`:                 "the launch timestamp would be lost across the user change",
		`BILLET_RUNNER_START_EPOCH_NS="$runner_started"`:                       "the hook could not split boot from registration and pickup",
	} {
		if !strings.Contains(entry, fragment) {
			t.Errorf("the entry point is missing %q — %s", fragment, why)
		}
	}
}

// GITHUB SELECTS THE HOOK INTERPRETER FROM ITS EXTENSION. A real EC2 job reached
// the runner and was rejected before its first step because the original path
// had no .sh suffix, even though the file was executable and had a shebang.
func TestTheJobStartHookHasASupportedScriptExtension(t *testing.T) {
	t.Parallel()

	got := mustScript(t)
	path := "/usr/local/bin/billet-job-started.sh"

	for _, fragment := range []string{
		"cat > " + path,
		"chmod 0755 " + path,
		"ACTIONS_RUNNER_HOOK_JOB_STARTED=" + path,
	} {
		if !strings.Contains(got, fragment) {
			t.Errorf("the image script is missing %q; GitHub refuses an administrator hook "+
				"whose path has no supported script extension", fragment)
		}
	}
}

// THE HOOK IS EXECUTED, not pattern-matched. It is part of the runner's job-start
// path, so malformed arithmetic or a non-zero exit would make the measurement
// alter the job it is supposed to observe.
func TestTheJobStartHookReportsColdStartPhasesWithoutFailingTheJob(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	date := filepath.Join(dir, "date")
	if err := os.WriteFile(date, []byte("#!/bin/sh\nprintf '%s\\n' 3000000000\n"), 0o755); err != nil {
		t.Fatalf("write fake date: %v", err)
	}

	run := func(t *testing.T, launch, runner string) string {
		t.Helper()

		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", jobTimingHook())
		cmd.Env = append(os.Environ(),
			"PATH="+dir,
			"BILLET_LAUNCH_EPOCH_NS="+launch,
			"BILLET_RUNNER_START_EPOCH_NS="+runner,
		)

		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the hook failed the job: %v\n%s", err, out)
		}

		return string(out)
	}

	want := "billet timing: launch_to_job_start_ms=2000 launch_to_runner_ms=1000 " +
		"runner_to_job_start_ms=1000\n"
	if got := run(t, "1000000000", "2000000000"); got != want {
		t.Errorf("hook output = %q, want %q", got, want)
	}

	if got := run(t, "not-a-time", "2000000000"); got != "" {
		t.Errorf("an invalid timestamp produced %q; instrumentation should fail open", got)
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

// buildFake answers the four calls a build makes, with the stop reason and image
// state a test wants to see.
type buildFake struct {
	stopReason string
	imageState string
	describes  int
	// notFoundFirst makes the first DescribeInstances answer as EC2 really did on
	// the first live build: the instance does not exist yet.
	notFoundFirst bool
}

func (b *buildFake) reply(action string, params url.Values) (int, string) {
	switch action {
	case "DescribeImages":
		// The base-image lookup comes first and must look like a real EBS root; the
		// later calls are asking whether the NEW image is ready.
		if params.Get("ImageId.1") == "ami-base" {
			return http.StatusOK, `<DescribeImagesResponse><imagesSet><item>` +
				`<imageId>ami-base</imageId><rootDeviceName>/dev/xvda</rootDeviceName>` +
				`<rootDeviceType>ebs</rootDeviceType><blockDeviceMapping>` +
				`<item><deviceName>/dev/xvda</deviceName><ebs>` +
				`<deleteOnTermination>true</deleteOnTermination></ebs></item>` +
				`<item><deviceName>/dev/sdb</deviceName><ebs>` +
				`<deleteOnTermination>false</deleteOnTermination></ebs></item>` +
				`<item><deviceName>/dev/sdc</deviceName><ebs>` +
				`<deleteOnTermination>false</deleteOnTermination></ebs></item>` +
				`</blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`
		}

		return http.StatusOK, `<DescribeImagesResponse><imagesSet><item>` +
			`<imageId>ami-new</imageId><rootDeviceName>/dev/xvda</rootDeviceName>` +
			`<rootDeviceType>ebs</rootDeviceType>` +
			`<imageState>` + b.imageState + `</imageState>` +
			`</item></imagesSet></DescribeImagesResponse>`

	case "RunInstances":
		return http.StatusOK, `<RunInstancesResponse><instancesSet><item>` +
			`<instanceId>i-builder</instanceId>` +
			`<instanceState><name>pending</name></instanceState>` +
			`</item></instancesSet></RunInstancesResponse>`

	case "DescribeInstances":
		b.describes++

		// NOT VISIBLE YET, ON THE FIRST ASK. This is the only bug in this PR that a
		// real machine demonstrated: the first live build called DescribeInstances
		// the instant RunInstances returned an id, got InvalidInstanceID.NotFound,
		// and shot a builder that was starting perfectly normally. Without this the
		// tolerance can be deleted and every test here stays green.
		if b.notFoundFirst && b.describes == 1 {
			return http.StatusBadRequest, `<Response><Errors><Error>` +
				`<Code>InvalidInstanceID.NotFound</Code><Message>nope</Message>` +
				`</Error></Errors></Response>`
		}

		return http.StatusOK, `<DescribeInstancesResponse><reservationSet><item>` +
			`<instancesSet><item><instanceId>i-builder</instanceId>` +
			`<instanceState><name>stopped</name></instanceState>` +
			`<stateReason><code>` + b.stopReason + `</code></stateReason>` +
			`</item></instancesSet></item></reservationSet></DescribeInstancesResponse>`

	case "CreateImage":
		return http.StatusOK, `<CreateImageResponse><imageId>ami-new</imageId></CreateImageResponse>`

	default:
		return http.StatusOK, defaultReply(action)
	}
}

// ONLY THE GUEST STOPPING ITSELF COUNTS AS SUCCESS.
//
// The whole build protocol is "provisioning finished" == "the instance stopped
// itself". Any other stop — an operator, a cost scheduler on a tag, a host
// failure — means the disk is whatever provisioning had reached, and imaging it
// produces a runner image that is not one.
//
// An ABSENT reason is refused too, and that is the half the first fix got wrong:
// AWS marks the code optional, so "" is not "the guest did it", it is "nobody
// said".
func TestOnlyAGuestInitiatedStopIsTreatedAsASuccessfulBuild(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		wantOK bool
	}{
		{name: "the guest powered itself off", reason: "Client.InstanceInitiatedShutdown", wantOK: true},
		{name: "somebody called StopInstances", reason: "Client.UserInitiatedShutdown"},
		{name: "the host stopped it", reason: "Server.ScheduledStop"},
		{name: "nobody said", reason: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &buildFake{stopReason: tc.reason, imageState: "available"}

			f := newFakeEC2(t)
			f.respond = b.reply

			p := newTestProvider(t, f, nil)

			image, err := p.BuildImage(t.Context(), BuildSpec{
				BaseImage: "ami-base", InstanceType: "c7i.xlarge",
				Arch: "x64", RunnerVersion: "2.328.0", Name: "test-image",
			})

			if tc.wantOK {
				if err != nil {
					t.Fatalf("a guest-initiated stop was not accepted: %v", err)
				}

				if image != "ami-new" {
					t.Errorf("built %q, want ami-new", image)
				}

				return
			}

			if err == nil {
				t.Fatalf("a stop with reason %q produced an image; whatever was on that disk "+
					"is not a finished runner image", tc.reason)
			}

			// AND NOTHING WAS IMAGED, which the error alone does not say. A
			// warn-and-proceed edit is entirely plausible in a codebase whose own
			// two-channel pattern is warn-and-proceed, and it would leave this test
			// green while registering an image from a half-provisioned disk.
			if n := f.countOf("CreateImage"); n != 0 {
				t.Errorf("%d images were registered from a builder stopped by %q", n, tc.reason)
			}
		})
	}
}

// EVERY DEVICE THE BASE IMAGE DECLARES IS RESTATED, not just the root.
//
// Restating only the root leaves a worse hole than it closes: a base AMI with a
// non-root device marked to survive leaks a volume on every build, AND
// CreateImage copies that mapping into the produced image — so every JOB launched
// from it leaks one too. One careless base image becomes a per-job leak.
func TestTheBuilderRestatesEveryDeviceTheBaseImageDeclares(t *testing.T) {
	b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available"}

	f := newFakeEC2(t)
	f.respond = b.reply

	p := newTestProvider(t, f, nil)

	if _, err := p.BuildImage(t.Context(), BuildSpec{
		BaseImage: "ami-base", InstanceType: "c7i.xlarge",
		Arch: "x64", RunnerVersion: "2.328.0", Name: "test-image",
	}); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}

	got := f.paramsFor(t, "RunInstances")

	// The base image declares /dev/sdb with DeleteOnTermination=false. The builder
	// must override it, or that disk outlives every build.
	seen := blockDevices(t, got)

	// TWO non-root devices, so a mutant emitting only layout.devices[0] dies. One
	// would have passed it.
	for _, device := range []string{"/dev/xvda", "/dev/sdb", "/dev/sdc"} {
		if seen[device] != "true" {
			t.Errorf("%s went out as %q, want true: the base image asks to keep it, and this "+
				"client cannot delete volumes", device, seen[device])
		}
	}

	if len(seen) != 3 {
		t.Errorf("sent %d devices, want 3: %v", len(seen), seen)
	}

	// AND THE BUILDER IS DESTROYED. Deleting the defer entirely passed every test
	// in this file until this line existed — the one property that burns money by
	// the hour.
	if n := f.countOf("TerminateInstances"); n != 1 {
		t.Errorf("the builder was terminated %d times, want 1", n)
	}

	// AND THE LAUNCH IS IDEMPOTENT, so a lost response is a recovery rather than a
	// second billable builder.
	if got.Get("ClientToken") == "" {
		t.Error("the builder launch carries no ClientToken; a lost response would buy a " +
			"second machine on retry")
	}
}

// AN INSTANCE THAT IS NOT VISIBLE YET IS NOT AN INSTANCE THAT DIED.
//
// DescribeInstances is eventually consistent, so it can answer
// InvalidInstanceID.NotFound for an instance RunInstances has already returned an
// id for. The first live build of this feature did exactly that and terminated a
// healthy builder — the only bug here a real machine demonstrated, and until this
// test existed the tolerance could be deleted with every other test still green.
func TestABuilderThatIsNotVisibleYetIsNotTreatedAsGone(t *testing.T) {
	b := &buildFake{
		stopReason:    "Client.InstanceInitiatedShutdown",
		imageState:    "available",
		notFoundFirst: true,
	}

	f := newFakeEC2(t)
	f.respond = b.reply

	p := newTestProvider(t, f, nil)

	image, err := p.BuildImage(t.Context(), BuildSpec{
		BaseImage: "ami-base", InstanceType: "c7i.xlarge",
		Arch: "x64", RunnerVersion: "2.328.0", Name: "test-image",
	})
	if err != nil {
		t.Fatalf("a builder that was not visible on the first ask was treated as gone: %v", err)
	}

	if image != "ami-new" {
		t.Errorf("built %q, want ami-new", image)
	}
}

// A REGISTERED IMAGE THAT NEVER BECOMES USABLE IS NAMED, because nothing here
// deletes it and the retry collides on the duplicate name.
func TestAnImageThatFailsToBecomeAvailableIsNamedInTheError(t *testing.T) {
	b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "failed"}

	f := newFakeEC2(t)
	f.respond = b.reply

	p := newTestProvider(t, f, nil)

	_, err := p.BuildImage(t.Context(), BuildSpec{
		BaseImage: "ami-base", InstanceType: "c7i.xlarge",
		Arch: "x64", RunnerVersion: "2.328.0", Name: "test-image",
	})
	if err == nil {
		t.Fatal("an image that ended in state \"failed\" was returned as a success")
	}

	if !strings.Contains(err.Error(), "ami-new") {
		t.Errorf("the error does not name the image that was left behind: %v", err)
	}
}
