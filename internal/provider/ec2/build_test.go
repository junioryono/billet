package ec2

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/version"
)

// THE SCRIPT IS THE IMAGE. Everything a runner image contains is decided by this
// one string, and a mistake in it produces the failure this whole backend keeps
// trying to avoid: an instance that boots, reports success, and never starts a
// runner, leaving the job queued until GitHub gives up.
// The payload fields staging would have filled in, for tests that render a
// provisioning script without standing up an S3 fake.
//
// BuildImage only stages when PayloadBucket is set and never overwrites these, so
// a spec carrying them takes exactly the path a real build takes — which is the
// only path there is now that the installers no longer fit user data.
const (
	testPayloadURL = "https://example-bucket.s3.us-west-2.amazonaws.com/billet-payload-" +
		"0000000000000000000000000000000000000000000000000000000000000000-0011223344556677" +
		".tar.gz?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Date=20260101T000000Z" +
		"&X-Amz-Expires=3600&X-Amz-SignedHeaders=host&X-Amz-Signature=" +
		"0000000000000000000000000000000000000000000000000000000000000000"
	testPayloadDigest = "1111111111111111111111111111111111111111111111111111111111111111"
)

func TestTheProvisionScriptContainsWhatAnImageNeeds(t *testing.T) {
	t.Parallel()

	got := mustScript(t)

	// DELIVERABLE, NOT PLAIN. This asserted `len(got) <= maxUserData`, which was
	// right while the script was always sent as plain text and is wrong now: past
	// the plain budget it is gzipped, which cloud-init documents and expects, so
	// the plain assertion would fail on a CORRECT build the moment parity pushes
	// the script over 16 KiB. What has to hold is that it can be carried at all.
	if _, err := packUserData(got); err != nil {
		t.Fatalf("the provisioning script cannot be delivered as user data: %v", err)
	}

	// STILL NOT installdependencies.sh, and the reason changed with the base image.
	//
	// On Amazon Linux it could not be used at all: it read /etc/os-release, found
	// ID_LIKE="fedora", and exited non-zero, ending the build under set -e -- found
	// by a real build. On Ubuntu it works, and is still refused: it runs its own
	// apt-get update and install for a set it chooses, which would sit beside the
	// pinned declaration as a second unversioned source of packages for one image.
	if strings.Contains(mustScript(t), "installdependencies.sh") {
		t.Error("the script calls installdependencies.sh, which installs a package set of " +
			"its own choosing beside the pinned declaration -- two sources of truth for " +
			"what one image contains")
	}

	for _, want := range []struct {
		fragment string
		why      string
	}{
		{"set -eux", "without -e a failed step is followed by poweroff, and billet images it"},
		{"apt-get -o DPkg::Lock::Timeout=600 update", "cloud-init can still hold the dpkg lock when this runs"},
		{"docker.io", "workflows use service containers and docker build"},
		{"docker-compose-v2", "Compose comes from the archive rather than a hand-pinned binary"},
		{"docker-buildx", "so does Buildx"},
		{"docker buildx version", "the image build must execute the packaged Buildx plugin"},
		{"docker compose version", "the image build must execute the Compose plugin"},
		{`"containerd-snapshotter": false`, "pulled images must stay inside the cache-backed Docker data root"},
		{`"storage-driver": "overlay2"`, "the Docker cache snapshots the classic image store atomically"},
		{"e2fsprogs", "the transparent Docker cache formats and verifies ext4"},
		{"util-linux", "and mounts it"},
		{"openssh-client", "one of github's declared packages, absent from any hand-written list"},
		{"shellcheck", "another, and neither is anything billet itself needs"},
		{"git", "actions/checkout wants it"},
		{"tar", "actions/setup-* unpack what they download"},
		{"useradd", "the runner refuses to run as root, and untrusted jobs must not"},
		{"usermod -aG docker runner", "the runner user has to reach the daemon"},
		{"actions-runner-linux-x64-2.328.0.tar.gz", "the release being installed"},
		{"libicu74", "the runner is a .NET app and dies on globalization without ICU"},
		{"/usr/local/bin/billet-runner", "the entry point a tier names in command:"},
		{jitEnvVar, "the one variable billet's boot script exports"},
		{"Runner.Listener --version", "an arch mismatch is otherwise invisible until a job"},
		{"setpriv --reuid=runner --regid=runner --init-groups \\\n  env -i PATH=", "the check has to take the path a job takes with a clean environment"},
		{"poweroff", "the only signal that provisioning succeeded"},
	} {
		if !strings.Contains(got, want.fragment) {
			t.Errorf("the script is missing %q — %s", want.fragment, want.why)
		}
	}
}

func TestTheArm64ProvisionScriptCarriesTheMatchingRunner(t *testing.T) {
	t.Parallel()

	script, err := provisionScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, RunnerVersion: "2.328.0", Arch: "arm64"})
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}

	// THIS USED TO ASSERT A PINNED COMPOSE BINARY PER ARCHITECTURE, which the
	// archive now supplies for both -- so the per-arch hazard that remains is the
	// runner tarball itself. An arm64 tarball extracts perfectly well on x64 and
	// every command succeeds; the failure surfaces on somebody's first job as a
	// machine that registered nothing.
	if !strings.Contains(script, "actions-runner-linux-arm64-2.328.0.tar.gz") {
		t.Error("the arm64 script does not install the arm64 runner build")
	}

	if strings.Contains(script, "actions-runner-linux-x64-") {
		t.Error("the arm64 script installs an x64 runner, which extracts cleanly and then " +
			"cannot exec")
	}
}

func TestTheCloudRunnerNeverInheritsTheReadinessCapability(t *testing.T) {
	t.Parallel()

	script := mustScript(t)
	const begin = "cat > /usr/local/bin/billet-runner <<'BILLETEOF'\n"
	_, after, ok := strings.Cut(script, begin)
	if !ok {
		t.Fatal("the EC2 runner entry point is missing")
	}
	entrypoint, _, ok := strings.Cut(after, "BILLETEOF\n")
	if !ok {
		t.Fatal("the EC2 runner entry point is not delimited")
	}
	if !strings.Contains(entrypoint, "env -i ") ||
		!strings.Contains(entrypoint, "BILLET_CACHE_TOKEN=\"${BILLET_CACHE_TOKEN:-}\"") {
		t.Fatal("the EC2 runner does not receive its cache session through a clean environment")
	}
	if strings.Contains(script, "BILLET_CACHE_READY_TOKEN") {
		t.Fatal("the EC2 image still carries a readiness pseudo-secret inside a root-controlled guest")
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

	got, err := provisionScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, RunnerVersion: "2.328.0", Arch: "arm64"})
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

	versionCheck := strings.Index(got, "Runner.Listener --version")
	power := strings.LastIndex(got, "poweroff")

	if versionCheck < 0 || versionCheck > power {
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

			got, err := provisionScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, RunnerVersion: "2.328.0", Arch: tc.arch})

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

	if _, err := provisionScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, Arch: "x64"}); err == nil {
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

	got, err := provisionScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, RunnerVersion: "2.328.0", Arch: "x64"})
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

	if !strings.Contains(entry, "/opt/actions-runner/billet-runner-service") {
		t.Error("the entry point does not run the runner")
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
		`ACTIONS_RUNNER_HOOK_JOB_STARTED=/usr/local/bin/billet-job-started.sh`:                                         "the runner would not invoke the cold-start measurement hook",
		`ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED=true`:                                                             "the image store could not distinguish a clean success from a failed job",
		`ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE="${ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE:-}"`: "the runner's requested deprecated-version failure would be masked",
		`BILLET_LAUNCH_EPOCH_NS="${BILLET_LAUNCH_EPOCH_NS:-}"`:                                                         "the launch timestamp would be lost across the user change",
		`BILLET_RUNNER_START_EPOCH_NS="$runner_started"`:                                                               "the hook could not split boot from registration and pickup",
		`/usr/local/bin/billet-docker-cache prepare`:                                                                   "service-container images would be pulled before their cache is mounted",
		`/usr/local/bin/billet-docker-cache complete "$job_status"`:                                                    "the image-store clone would never be published or discarded",
		`/usr/local/bin/billet-docker-cache service-status "$job_status"`:                                              "the runner service exit contract would not be preserved",
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

	if ext := filepath.Ext(jobTimingHookPath); ext != ".sh" {
		t.Fatalf("the job-start hook extension is %q, want .sh; GitHub will refuse it", ext)
	}

	for _, fragment := range []string{
		"cat > " + jobTimingHookPath + " <<'BILLETJOBEOF'\n",
		"chmod 0755 " + jobTimingHookPath + "\n",
		"ACTIONS_RUNNER_HOOK_JOB_STARTED=" + jobTimingHookPath + " \\\n",
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

	got, err := provisionScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, RunnerVersion: "2.328.0", Arch: "x64"})
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}

	return got
}

// buildFake answers the calls a build makes, with the stop reason and image state
// a test wants to see.
type buildFake struct {
	stopReason string
	imageState string
	describes  int
	// notFoundFirst makes the first DescribeInstances answer as EC2 really did on
	// the first live build: the instance does not exist yet.
	notFoundFirst bool

	// The verification half. verdict is what the booted image claims about itself
	// and arch is what DescribeImages says the produced AMI is for; both have
	// working defaults so every test that predates verification is unaffected.
	verdict string
	arch    string
	// nonce is read back OUT of the verifier's own user data rather than invented
	// here, which is what makes a console fixture a round trip: billet's parser has
	// to agree with billet's emitter, and a hand-written marker would agree with
	// neither if either moved.
	nonce string
	// silentConsole makes the machine boot and print nothing, which is the failure
	// an empty console cannot be distinguished from.
	silentConsole bool
	// consoleNoise goes in front of the report, standing in for a boot log.
	consoleNoise string
	// promoted records the contract value CreateTags wrote, so a later
	// DescribeImages answers as a real one would.
	promoted string
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

		// THE PRODUCED IMAGE DESCRIBES ITSELF THE WAY A REAL ONE DOES: an
		// architecture, a block device mapping, and whatever tags it has been given.
		// The tag set is what the promotion reads back, so a fake that always
		// answered "no tags" would fail a correct promote, and one that always
		// answered "tagged" would pass a CreateTags that never happened.
		arch := b.arch
		if arch == "" {
			arch = "x86_64"
		}

		tags := ""
		if b.promoted != "" {
			tags = `<tagSet><item><key>` + amiContractTag + `</key><value>` +
				b.promoted + `</value></item></tagSet>`
		}

		return http.StatusOK, `<DescribeImagesResponse><imagesSet><item>` +
			`<imageId>ami-new</imageId><rootDeviceName>/dev/xvda</rootDeviceName>` +
			`<rootDeviceType>ebs</rootDeviceType>` +
			`<architecture>` + arch + `</architecture>` +
			`<imageState>` + b.imageState + `</imageState>` +
			`<blockDeviceMapping><item><deviceName>/dev/xvda</deviceName><ebs>` +
			`<deleteOnTermination>true</deleteOnTermination></ebs></item>` +
			`</blockDeviceMapping>` + tags +
			`</item></imagesSet></DescribeImagesResponse>`

	case "RunInstances":
		// THE VERIFIER IS A DIFFERENT MACHINE AND ANSWERS TO A DIFFERENT ID, so a
		// test can tell the two terminates apart. A fake that returned one id for
		// both would let a build that never launched a verifier — or one that
		// terminated the builder twice — pass.
		if params.Get("ImageId") != "ami-base" {
			b.nonce = nonceFromUserData(params.Get("UserData"))

			return http.StatusOK, `<RunInstancesResponse><instancesSet><item>` +
				`<instanceId>i-verify</instanceId>` +
				`<instanceState><name>pending</name></instanceState>` +
				`</item></instancesSet></RunInstancesResponse>`
		}

		return http.StatusOK, `<RunInstancesResponse><instancesSet><item>` +
			`<instanceId>i-builder</instanceId>` +
			`<instanceState><name>pending</name></instanceState>` +
			`</item></instancesSet></RunInstancesResponse>`

	case "GetConsoleOutput":
		return http.StatusOK, `<GetConsoleOutputResponse><instanceId>i-verify</instanceId>` +
			`<output>` + b.console() + `</output></GetConsoleOutputResponse>`

	case "CreateTags":
		if params.Get("Tag.1.Key") == amiContractTag {
			b.promoted = params.Get("Tag.1.Value")
		}

		return http.StatusOK, `<CreateTagsResponse><return>true</return></CreateTagsResponse>`

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

		// THE INSTANCE THAT WAS ASKED ABOUT, not always the builder. The verifier's
		// cleanup waits for its own id to become visible before terminating it —
		// because a terminate issued seconds after a launch can be answered
		// NotFound and reported as done — so a fake that only ever describes the
		// builder makes that wait time out on every run.
		if params.Get("InstanceId.1") == "i-verify" {
			return http.StatusOK, `<DescribeInstancesResponse><reservationSet><item>` +
				`<instancesSet><item><instanceId>i-verify</instanceId>` +
				`<instanceState><name>running</name></instanceState>` +
				`</item></instancesSet></item></reservationSet></DescribeInstancesResponse>`
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

			image, err := p.BuildImage(t.Context(), BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest,
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

	if _, err := p.BuildImage(t.Context(), BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest,
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

	image, err := p.BuildImage(t.Context(), BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest,
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

	_, err := p.BuildImage(t.Context(), BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest,
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

// TestTheImageStoreIsProvenAgainstTheDaemonNotTheScript is the regression for
// ami-08c37af8484ff7d29, which was built before daemon.json existed and lost
// every cached image for nine days while reporting nothing.
//
// It asserts POSITION, not presence. A substring check would pass with the
// assertion sitting inside one of the heredocs this script writes — which is
// exactly the mistake that produced the defect's first attempted fix, where
// `docker info` was read from the job-time billet-runner script and mistaken for
// something the provisioner runs.
func TestTheImageStoreIsProvenAgainstTheDaemonNotTheScript(t *testing.T) {
	t.Parallel()

	got, err := provisionScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, RunnerVersion: "2.328.0", Arch: "arm64"})
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}

	// The last heredoc terminator. Everything after it is executed by the
	// provisioning shell rather than written into a file for later.
	lastHeredoc := strings.LastIndex(got, "BILLETEOF\n")
	if lastHeredoc < 0 {
		t.Fatal("no heredoc terminator found; this test can no longer tell " +
			"provisioning-time commands from ones written into a file")
	}

	power := strings.LastIndex(got, "poweroff")
	if power < lastHeredoc {
		t.Fatal("poweroff precedes the last heredoc terminator, so the script has no " +
			"provisioning-time tail for these assertions to live in")
	}

	// THE TAIL, NOT THE WHOLE SCRIPT, and the difference is the whole point. Some
	// of these strings also occur INSIDE the files this script writes: the embedded
	// billet-docker-cache asset contains `systemctl start docker`, and the job-time
	// billet-runner contains a `docker info` readiness wait. A whole-script search
	// finds those and reports success for commands that never run at build time —
	// which is exactly the confusion that let a broken image ship, and which this
	// test caught in its own first draft.
	tail := got[lastHeredoc:power]

	for _, probe := range []struct {
		needle string
		why    string
	}{
		{dockerGateAnchor,
			"START is not enough: apt's postinst can leave the daemon RUNNING, and " +
				"`systemctl start` on an active unit succeeds without re-reading " +
				"daemon.json -- so the builder keeps Docker 29's containerd " +
				"snapshotter and the gate fails every build on a correct image"},
		{`.features["containerd-snapshotter"] == false`,
			"nothing proves the AMI itself carries the classic-store daemon.json"},
		{`{{.Driver}}`,
			"nothing proves the daemon actually read daemon.json"},
		{`{{.DockerRootDir}}`,
			"nothing proves the data root is where the cache attaches its filesystem"},
	} {
		if !strings.Contains(tail, probe.needle) {
			t.Errorf("%q does not run between the last heredoc and poweroff, so it is "+
				"either absent or written into a file rather than executed while the "+
				"image is built: %s", probe.needle, probe.why)
		}
	}
}

// TestTheDockerRootCheckResolvesBeforeComparing pins the two halves of the
// boundary, because each was wrong on its own at some point.
//
// A bare prefix accepts /var/lib/docker-elsewhere. Two case arms fix that and are
// still only LEXICAL: /var/lib/docker/../containerd matches /var/lib/docker/*, and
// resolves to exactly the directory this check exists to keep Docker out of. So
// both sides are canonicalised first.
func TestTheDockerRootCheckResolvesBeforeComparing(t *testing.T) {
	t.Parallel()

	got, err := provisionScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, RunnerVersion: "2.328.0", Arch: "arm64"})
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}

	if !strings.Contains(got, "realpath -m") {
		t.Error("the data-root check compares unresolved paths, so " +
			"/var/lib/docker/../containerd satisfies it while Docker stores images " +
			"exactly where the cache cannot see them")
	}

	if !strings.Contains(got, `"$billet_cache_root"|"$billet_cache_root"/*)`) {
		t.Error("the data-root check does not match the resolved directory and its " +
			"subtree as separate arms, so a prefix such as /var/lib/docker-elsewhere " +
			"would pass")
	}
}

// TestAnUntaggedImageIsBelowTheContract is the case every AMI in service is in
// today: built before billet stamped its output, so it answers nothing about what
// made it. That has to read as "needs a rebuild", not as a pass.
func TestAnUntaggedImageIsBelowTheContract(t *testing.T) {
	t.Parallel()

	if AMIContract < 1 {
		t.Fatalf("AMIContract is %d; an untagged image reports 0, so a contract of 0 or "+
			"less makes every unstamped image indistinguishable from a current one",
			AMIContract)
	}

	var untagged ImageInfo
	if untagged.Contract >= AMIContract {
		t.Errorf("an image with no contract tag reports %d against a required %d, so the "+
			"stale image this check exists for would be reported as current",
			untagged.Contract, AMIContract)
	}
}

// TestCreateImageStampsTheProvenanceOnTheImage drives createImage through a fake
// API and asserts the request AWS would actually receive.
//
// NOT stampImage DIRECTLY, which is what the first version of this test did and
// why it was worthless: deleting the call site in createImage left it green while
// CreateImage emitted no tags at all. A helper proven in isolation says nothing
// about whether anything calls it — the same defect this session already shipped
// once, in the cache adapter's signed-URL test.
func TestCreateImageStampsTheProvenanceOnTheImage(t *testing.T) {
	t.Parallel()

	var got url.Values

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		parsed, err := url.ParseQuery(body)
		if err != nil {
			t.Errorf("the CreateImage request body did not parse: %v", err)
		}
		got = parsed

		fmt.Fprint(w, `<CreateImageResponse><imageId>ami-stamped</imageId>`+
			`</CreateImageResponse>`)
	}))
	t.Cleanup(srv.Close)

	p, err := New("deployment-under-test", config.EC2Config{
		Region:           "us-west-2",
		Endpoint:         srv.URL,
		SubnetID:         "subnet-1",
		SecurityGroupIDs: []string{"sg-trusted"},
		InstanceTypes: []config.EC2InstanceType{
			{Type: "c7i.large", VCPU: 2, Memory: 4 * config.GiB},
		},
	}, WithHTTPClient(srv.Client()), WithCredentials(awscreds.Static{
		AccessKeyID: "AKID", SecretAccessKey: "s",
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := p.createImage(t.Context(), "i-builder", "billet-runner-test"); err != nil {
		t.Fatalf("createImage: %v", err)
	}

	// EXACT PARAMETER NAMES. Index 1 with ResourceType=image is what CreateImage
	// documents; asserting the shape by suffix would accept TagSpecification.0,
	// which AWS does not.
	if v := got.Get("TagSpecification.1.ResourceType"); v != "image" {
		t.Errorf("CreateImage tags the %q resource, want \"image\": the builder instance "+
			"is terminated minutes later and only the image outlives the build", v)
	}

	want := map[string]string{
		ownerTag:      "deployment-under-test",
		amiBuiltByTag: version.String(),
	}

	// FOUR, NOT TWO, so a stamp that grew a tag is visible here rather than
	// silently ignored by a loop sized to what it expects.
	stamped := map[string]string{}
	for i := 1; i <= 4; i++ {
		key := got.Get("TagSpecification.1.Tag." + strconv.Itoa(i) + ".Key")
		if key == "" {
			continue
		}

		stamped[key] = got.Get("TagSpecification.1.Tag." + strconv.Itoa(i) + ".Value")
	}

	for key, expected := range want {
		if stamped[key] != expected {
			t.Errorf("the image records %s=%q, want %q; without it nothing about a built "+
				"image says who owns it or what made it", key, stamped[key], expected)
		}
	}

	// AND THE CONTRACT IS NOT AMONG THEM, which is the half that is easy to
	// regress: whether the image MEETS a contract is a fact about the artifact,
	// established by booting it, and a create-time stamp is a build's claim about
	// itself. Putting it back here would make a failed verification leave an AMI
	// already carrying the contract it just failed.
	if v, ok := stamped[amiContractTag]; ok {
		t.Errorf("CreateImage stamped %s=%q; the contract is written by the promotion after "+
			"the image has been booted and asserted on, not by the build claiming it",
			amiContractTag, v)
	}

	if n := len(stamped); n != len(want) {
		t.Errorf("CreateImage sent %d tags, want %d: %v", n, len(want), stamped)
	}
}

// dockerGateAnchor is the first command of the image-store gate, shared by the
// test that locates the block and asserted to exist in the generated script — so
// renaming it in build.go fails loudly here instead of silently emptying the
// table below.
const dockerGateAnchor = "systemctl restart docker"

// TestTheImageStoreAssertionsActuallyRefuse runs the provisioning tail under
// /bin/sh against fake systemctl, docker and jq, and asserts the EXIT STATUS.
//
// WHY THIS EXISTS ALONGSIDE THE TOKEN TESTS, which are not enough on their own.
// Every substring the position test looks for survives changes that make the
// check toothless: dropping -e from jq, rendering {{.Driver}} without comparing
// it, deleting the default arm's `exit 1`. Each of those keeps the tokens and
// accepts an image whose cache publishes empty. Only running it can tell.
//
// The tail is what runs at build time — everything after the last heredoc — so
// this executes exactly the region the position test guards, with no daemon and
// no AWS anywhere near it.
func TestTheImageStoreAssertionsActuallyRefuse(t *testing.T) {
	t.Parallel()

	// CAPABILITY, NOT PRESENCE. macOS ships a BSD realpath that exists and rejects
	// -m ("illegal option"), so LookPath alone reports a tool this cannot use --
	// measured, after that exact failure. Amazon Linux 2023, which the AMI is built
	// from, has GNU coreutils where -m resolves a path whose tail does not exist.
	//
	// SKIPPED RATHER THAN FAKED: a stub realpath would make this test agree with
	// itself about the one behaviour the boundary now depends on.
	if err := exec.CommandContext(t.Context(), "realpath", "-m", "/x").Run(); err != nil {
		t.Skip("no realpath -m here; this runs on the Linux images the AMI targets")
	}

	script, err := provisionScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, RunnerVersion: "2.328.0", Arch: "arm64"})
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}

	lastHeredoc := strings.LastIndex(script, "BILLETEOF\n")
	power := strings.LastIndex(script, "poweroff")
	if lastHeredoc < 0 || power < lastHeredoc {
		t.Fatal("the script has no provisioning-time tail to execute")
	}
	// FROM THE DOCKER BLOCK, not from the heredoc terminator. The tail also
	// carries a chmod and the setpriv runner check, and faking those would mean
	// stubbing the whole provisioning environment to test four assertions. The
	// position test above proves this block lives in the tail; this one proves
	// what it does once it gets there.
	// ANCHORED ON THE RESTART, and this line is why the anchor is derived from a
	// constant rather than retyped: it said "start" after the production code
	// moved to "restart", so the slice found nothing and every case in this table
	// stopped running. CI caught it because this test SKIPS on macOS (no
	// `realpath -m`), so a green local run proved nothing about it.
	start := strings.Index(script[lastHeredoc:power], dockerGateAnchor)
	if start < 0 {
		t.Fatalf("the image-store block does not start with %q in the provisioning "+
			"tail; if that command was renamed, update dockerGateAnchor — otherwise "+
			"every case in this table silently stops running", dockerGateAnchor)
	}
	tail := script[lastHeredoc+start : power]

	for _, tc := range []struct {
		name     string
		driver   string
		root     string
		jqStatus string
		ready    string
		refuse   bool
		// wantMessage, when set, is a fragment the refusal must contain. A
		// non-zero exit alone cannot tell WHICH assertion fired, so the ordering
		// claim needs the diagnostic rather than the status.
		wantMessage string
	}{
		{name: "a correct image", driver: "overlay2", root: "/var/lib/docker",
			jqStatus: "0", ready: "0", refuse: false},
		{name: "the containerd snapshotter", driver: "overlayfs", root: "/var/lib/docker",
			jqStatus: "0", ready: "0", refuse: true},
		{name: "a data root off the cache filesystem", driver: "overlay2",
			root: "/var/lib/containerd", jqStatus: "0", ready: "0", refuse: true},
		// THE ORDER MATTERS, NOT ONLY THE OUTCOME. A containerd-snapshotter image
		// reports BOTH a wrong root and a different driver name; the root is the
		// property and must be what fails, so the message names where the bytes
		// went rather than how the driver is spelled.
		{name: "the containerd snapshotter, reported by both signals",
			driver: "overlayfs", root: "/var/lib/containerd",
			jqStatus: "0", ready: "0", refuse: true,
			// THE MESSAGE IS THE POINT. Both signals are wrong here, so the block
			// exits non-zero whichever assertion runs first -- checking only the
			// status proves nothing about order, which is what the first version
			// of this case did. Requiring the data-root diagnostic is what makes
			// it fail if the driver check is moved back in front.
			wantMessage: "resolves outside"},
		{name: "a data root escaping through traversal", driver: "overlay2",
			root: "/var/lib/docker/../containerd", jqStatus: "0", ready: "0", refuse: true},
		{name: "a daemon.json that does not select the classic store", driver: "overlay2",
			root: "/var/lib/docker", jqStatus: "1", ready: "0", refuse: true},
		{name: "a daemon that never comes up", driver: "overlay2", root: "/var/lib/docker",
			jqStatus: "0", ready: "1", refuse: true},
		// ACCEPTED, because the loop waits. Without the loop this is refused, which
		// is the only case that gives the wait any coverage at all.
		{name: "a daemon that comes up late", driver: "overlay2", root: "/var/lib/docker",
			jqStatus: "0", ready: "late", refuse: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bin := t.TempDir()

			// A LATE DAEMON IS WHAT THE WAIT LOOP IS FOR, and nothing exercised it:
			// the only readiness case was one that never comes up, which the bare
			// `docker info` after the loop refuses on its own. So deleting the loop
			// entirely used to pass the whole suite, silently reintroducing the
			// seven-second race it was added for.
			readiness := "if [ \"$2\" != -f ]; then exit " + tc.ready + "; fi\n"
			if tc.ready == "late" {
				readiness = "n=$(cat " + filepath.Join(bin, "tries") + " 2>/dev/null || echo 0)\n" +
					"if [ \"$2\" != -f ]; then\n" +
					"  n=$((n+1)); printf '%s' \"$n\" > " + filepath.Join(bin, "tries") + "\n" +
					"  [ \"$n\" -ge 3 ] || exit 1\n" +
					"  exit 0\n" +
					"fi\n"
			}
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
					t.Fatalf("write %s: %v", name, err)
				}
			}

			// THE FAKES MODEL THE ACTUAL FAILURE, which is the only way this test
			// has any power over `start` versus `restart`.
			//
			// systemctl used to be `exit 0`, so the executing test could not see
			// the commit's central claim at all: mutating restart->start died only
			// because the extraction anchor IS that string, which is a tautology
			// dressed as a mutation kill.
			//
			// So systemctl RECORDS its verb, and docker answers from it: a daemon
			// that was merely `start`ed is one apt already left running, which
			// never re-read daemon.json and still reports the containerd
			// snapshotter. Exactly the production failure, reproduced.
			write("systemctl", "#!/bin/sh\nprintf '%s' \"$1\" > "+
				filepath.Join(bin, "verb")+"\nexit 0\n")
			write("jq", "#!/bin/sh\nexit "+tc.jqStatus+"\n")
			// `docker info` with no -f is the readiness probe; with -f it is one of
			// the two assertions. Both go through this one fake, as they do through
			// one real binary.
			write("docker", "#!/bin/sh\n"+
				"if [ \"$1\" != info ]; then exit 0; fi\n"+
				"verb=$(cat "+filepath.Join(bin, "verb")+" 2>/dev/null || true)\n"+
				readiness+
				"if [ \"$2\" != -f ]; then exit 0; fi\n"+
				"# A daemon that was only started is the one apt left running: it\n"+
				"# never re-read daemon.json, so it still reports the snapshotter.\n"+
				"if [ \"$verb\" != restart ]; then\n"+
				"  case \"$3\" in\n"+
				"    *Driver*) printf 'overlayfs\\n' ;;\n"+
				"    *DockerRootDir*) printf '/var/lib/containerd\\n' ;;\n"+
				"  esac\n"+
				"  exit 0\n"+
				"fi\n"+
				"case \"$3\" in\n"+
				"  *Driver*) printf '%s\\n' "+tc.driver+" ;;\n"+
				"  *DockerRootDir*) printf '%s\\n' "+tc.root+" ;;\n"+
				"esac\n")

			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "set -eu\n"+tail)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()

			if tc.wantMessage != "" && !strings.Contains(string(out), tc.wantMessage) {
				t.Errorf("the refusal does not mention %q, so it came from a different "+
					"assertion than the one this case is about — the data root is the "+
					"property and must be what fails:\n%s", tc.wantMessage, out)
			}

			switch {
			case tc.refuse && err == nil:
				t.Errorf("the tail ACCEPTED %s, so an image in that state would be "+
					"registered and its cache would publish with no images in it:\n%s",
					tc.name, out)
			case !tc.refuse && err != nil:
				t.Errorf("the tail refused %s, which would fail every AMI build: %v\n%s",
					tc.name, err, out)
			}
		})
	}
}
