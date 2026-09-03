package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// THE JIT CONFIG MUST NEVER REACH ARGV.
//
// It is a live runner registration until consumed, and `docker run -e VAR=value`
// puts the value in this process's command line where every user on the host can
// read it out of ps. That is a credential disclosure whose implementation looks
// entirely correct — the same class as the App-key argv residual the
// billet-security skill records.
//
// So this records the exact argv handed to the container CLI and asserts the
// secret is absent from it. A stub binary is used rather than real docker
// because the assertion is about what billet SAYS, and a real run would let a
// pull failure hide the check.
func TestTheJITConfigNeverAppearsInArgv(t *testing.T) {
	const secret = "eyJzZXJ2ZXJVcmwiOiJodHRwczovL2V4YW1wbGUiLCJ0b2tlbiI6IlNVUEVSU0VDUkVUIn0="

	stub, argvFile := stubDocker(t)

	p := New("billet-test", WithBinary(stub))

	inst, err := p.Launch(t.Context(), provider.Spec{
		Name:      "billet-runner-1",
		Image:     "ghcr.io/actions/actions-runner:latest",
		VCPU:      2,
		Memory:    2 * config.GiB,
		Trust:     provider.TrustTrusted,
		JITConfig: secret,
		Command:   config.Tier{}.RunnerCommand(),
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if inst.ID == "" {
		t.Error("Launch returned no container id")
	}

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}

	if strings.Contains(string(argv), secret) {
		t.Errorf("the JIT config was passed on the command line, where ps exposes it to every "+
			"user on the host:\n%s", argv)
	}

	// And it was actually delivered — a test that only checks absence would pass
	// against a provider that forgot the credential entirely.
	if !strings.Contains(string(argv), "--env-file") {
		t.Error("no --env-file was passed, so the runner would start with no registration")
	}
}

// The env file holds the credential, so it is 0600 and it does not outlive the
// launch.
func TestTheEnvFileIsPrivateAndRemoved(t *testing.T) {
	p := New("billet-test")

	path, cleanup, err := p.writeEnvFile(provider.Spec{
		Name:      "billet-runner-1",
		JITConfig: "a-registration",
	})
	if err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat env file: %v", err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the env file is %o; it holds a runner registration and must be 0600", perm)
	}

	if dir, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("stat env directory: %v", err)
	} else if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("the env directory is %o; 0700 is what stops a listing", perm)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}

	if !strings.Contains(string(body), jitEnvVar+"=a-registration") {
		t.Errorf("the runner would not find its registration; file holds %q", body)
	}

	cleanup()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the JIT config file outlived the launch; it holds a live registration")
	}
}

// Destroying something already gone is success.
//
// Teardown runs on paths that have already failed once, so an error here turns a
// recoverable state into a stuck one.
func TestDestroyIsIdempotent(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}

	p := New("billet-test")

	if _, err := p.Destroy(t.Context(), "billet-nonexistent-container-name"); err != nil {
		t.Errorf("destroying a container that does not exist reported an error: %v", err)
	}
}

// A LAUNCHED CONTAINER MUST BE TOLD TO RUN THE RUNNER.
//
// Found on the first job billet was ever given, against a real organization.
// `docker run` succeeded, the JIT config was in the container's environment, and
// billet logged "started a runner" — then the container exited 0 within twenty
// seconds and the job stayed queued, because the stock image's default CMD is
// /bin/bash and a detached bash with no TTY has nothing to do.
//
// Every signal billet had said the launch worked, which is what makes this worth
// a test rather than only a fix: the failure is a tier that looks healthy and
// silently runs nothing, and no amount of dry-running reaches it, because a dry
// run never launches anything.
func TestLaunchTellsTheContainerToRunTheRunner(t *testing.T) {
	stub, argvFile := stubDocker(t)

	p := New("billet-test", WithBinary(stub))

	// The command a tier with no override supplies, taken from config rather than
	// restated, so this test follows the default if it ever moves.
	want := config.Tier{}.RunnerCommand()

	if _, err := p.Launch(t.Context(), provider.Spec{
		Name:      "billet-runner-1",
		Image:     "ghcr.io/actions/actions-runner:latest",
		VCPU:      2,
		Memory:    2 * config.GiB,
		Trust:     provider.TrustTrusted,
		JITConfig: "eyJzZXJ2ZXJVcmwiOiJodHRwczovL2V4YW1wbGUifQ==",
		Command:   want,
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}

	fields := strings.Fields(string(argv))
	if len(fields) < 2 {
		t.Fatalf("nothing meaningful was handed to the container CLI: %s", argv)
	}

	// LAST, AND AFTER THE IMAGE. Docker reads everything following the image
	// reference as the command, so asserting mere presence would pass against an
	// argv that put it earlier — where it is swallowed as a flag value and the
	// container runs its default shell anyway.
	if last := fields[len(fields)-1]; last != want[0] {
		t.Errorf("the container's command is %q, want %q; without it the image runs its "+
			"default shell, which exits at once and leaves the job queued while billet "+
			"reports a runner started\nargv: %s", last, want[0], argv)
	}

	if image := fields[len(fields)-2]; image != "ghcr.io/actions/actions-runner:latest" {
		t.Errorf("the command does not directly follow the image; got %q before it\nargv: %s",
			image, argv)
	}
}

// A spec with no command is refused rather than launched.
//
// The refusal IS the fix, not the default. Defaulting to ./run.sh here would
// suit the stock image and silently break any other, and the failure it produces
// is the worst kind: a container that starts, exits at once, and reports success
// the whole way while the job stays queued.
func TestLaunchRefusesASpecWithNoCommand(t *testing.T) {
	stub, _ := stubDocker(t)

	p := New("billet-test", WithBinary(stub))

	_, err := p.Launch(t.Context(), provider.Spec{
		Name:      "billet-runner-1",
		Image:     "ghcr.io/actions/actions-runner:latest",
		Trust:     provider.TrustTrusted,
		JITConfig: "eyJzZXJ2ZXJVcmwiOiJodHRwczovL2V4YW1wbGUifQ==",
	})
	if err == nil {
		t.Fatal("launched a container with no command; the image's default is a shell, so " +
			"it would exit immediately while billet reported a started runner")
	}

	if !strings.Contains(err.Error(), "command") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

// A spec with no registration is refused rather than launched.
//
// A container that starts without one registers nothing, takes no job, and sits
// there looking healthy — the failure is invisible where it happens and obvious
// only much later, as a tier that never picks anything up.
func TestLaunchRefusesASpecWithNoRegistration(t *testing.T) {
	stub, _ := stubDocker(t)

	p := New("billet-test", WithBinary(stub))

	_, err := p.Launch(t.Context(), provider.Spec{
		Name:  "billet-runner-1",
		Image: "ghcr.io/actions/actions-runner:latest",
		Trust: provider.TrustTrusted,
	})
	if err == nil {
		t.Fatal("launched a runner with no JIT config; it would register nothing and take no job")
	}

	if !strings.Contains(err.Error(), "JIT config") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

// stubDocker writes a script that records its argv and prints a container id,
// standing in for the container CLI. Returns the script path and the file its
// arguments land in.
func stubDocker(t *testing.T) (script, argvFile string) {
	t.Helper()

	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv")
	script = filepath.Join(dir, "docker")

	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + argvFile +
		"\necho 0123456789abcdef0123456789abcdef\n"

	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	return script, argvFile
}

// stubPrinting is a docker that prints one fixed thing on stdout.
//
// Separate from stubDocker, which records argv and returns a container id: these
// tests are about how billet READS output, so what matters is controlling stdout
// exactly, including lines a wrapper might add.
//
// A test that fork+execs a stub written this way must NOT be t.Parallel(). Go
// runs parallel tests concurrently with each other, and one test's fork can
// transiently inherit a sibling's still-open writable fd to its just-written
// stub, so the exec fails with ETXTBSY — "text file busy" (golang.org/issue/22315).
// It reproduced on Linux CI and was invisible on the darwin dev machine. Running
// these exec-based tests sequentially is the fix; the process-global hazard they
// share (a concurrent fork against a WriteFile) is exactly what the billet-testing
// guidance says must stay off the parallel path.
func stubPrinting(t *testing.T, out string) string {
	t.Helper()

	script := filepath.Join(t.TempDir(), "docker")

	// A quoted heredoc, so nothing in the payload is expanded by the shell.
	body := "#!/bin/sh\ncat <<'BILLET_EOF'\n" + out + "\nBILLET_EOF\n"

	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	return script
}

// Untrusted and UNCLASSIFIED work are both refused.
//
// The package comment always said this backend must refuse untrusted work rather
// than warn about it, and for a while nothing enforced that — the claim was
// documentation rather than behaviour. A container shares the host kernel, so
// fork-pull-request code here has a materially weaker boundary than the microVM
// the rest of the design assumes.
//
// Unknown is refused alongside untrusted, and that is the part that matters. A
// caller who has not classified a job has not established it is safe to run
// somewhere weak, and treating the zero value as "probably fine" is how the
// refusal gets bypassed by omission rather than by decision.
func TestUntrustedAndUnclassifiedWorkIsRefused(t *testing.T) {
	stub, argvFile := stubDocker(t)

	for name, trust := range map[string]provider.TrustClass{
		"unclassified": provider.TrustUnknown,
		"untrusted":    provider.TrustUntrusted,
	} {
		t.Run(name, func(t *testing.T) {
			p := New("billet-test", WithBinary(stub))

			_, err := p.Launch(t.Context(), provider.Spec{
				Name:      "billet-runner-1",
				Image:     "ghcr.io/actions/actions-runner:latest",
				Trust:     trust,
				JITConfig: "a-registration",
			})
			if err == nil {
				t.Fatalf("ran %s work in a container, which shares the host kernel", name)
			}

			if !strings.Contains(err.Error(), "shares the host kernel") {
				t.Errorf("the refusal does not say why: %v", err)
			}
		})
	}

	// And nothing was started. A refusal that still launches is not a refusal.
	if body, err := os.ReadFile(argvFile); err == nil && strings.Contains(string(body), "run") {
		t.Errorf("a container was launched despite the refusal:\n%s", body)
	}
}

// A line List cannot read is an error, not something to skip.
//
// The danger is specific: List feeds a loop that destroys whatever is NOT
// accounted for, and enumeration failure is deliberately fatal. A wrapper script
// or a podman version that prints one extra line would otherwise make List
// report success while omitting a billet container — the one outcome the caller
// cannot detect.
func TestListRefusesOutputItCannotRead(t *testing.T) {
	// Not parallel: fork+execs a stub (see stubPrinting's ETXTBSY note).
	p := New("test-deployment", WithBinary(stubPrinting(t,
		"abc123\tbillet-one\trunning\nWARNING: some wrapper wrote this")))

	if _, err := p.List(t.Context()); err == nil {
		t.Fatal("a line with no tab was skipped silently; a missing container would look like an empty host")
	}
}

func TestListReadsWellFormedOutput(t *testing.T) {
	// Not parallel: fork+execs a stub (see stubPrinting's ETXTBSY note).
	p := New("test-deployment", WithBinary(stubPrinting(t,
		"abc123\tbillet-one\trunning\ndef456\tbillet-two\texited\n"+
			"ghi789\tbillet-three\tcreated\njkl012\tbillet-four\tsomethingnew")))

	got, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(got) != 4 {
		t.Fatalf("read %d instances from four lines: %v", len(got), got)
	}

	if got[0].ID != "abc123" || got[0].Name != "billet-one" || !got[0].Running {
		t.Errorf("first instance parsed as %+v", got[0])
	}

	// The second is EXITED, and that distinction decides whether an adopted
	// container is left to finish or cleaned up.
	if got[1].Running {
		t.Errorf("an exited container was reported as running: %+v", got[1])
	}

	// CREATED IS NOT RUNNING. A container that exists but was never started never
	// will be — whatever would have started it is gone — so adopting it means
	// holding its lease open forever for a job that cannot begin. It is the one
	// state that looks alive and is not.
	if got[2].Running {
		t.Errorf("a created-but-never-started container was reported as running: %+v", got[2])
	}

	// An UNRECOGNISED state counts as running, and that asymmetry is deliberate:
	// the caller destroys what is not running, and a state billet has never heard
	// of is not evidence that a job is over.
	if !got[3].Running {
		t.Errorf("an unrecognised state was treated as finished, which would destroy live "+
			"work: %+v", got[3])
	}
}

// An extra column is refused, not absorbed into the name.
//
// strings.Cut proved a tab EXISTED; it did not prove there was only one, so a
// wrapper printing a fourth field used to land inside the container name. A name
// with a suffix still parses as billet's, so the lease lookup misses and the
// periodic sweep destroys what it decides is an orphan — a live job.
func TestListRefusesAnExtraColumn(t *testing.T) {
	// Not parallel: fork+execs a stub (see stubPrinting's ETXTBSY note).
	p := New("test-deployment", WithBinary(stubPrinting(t,
		"abc123\tbillet-lease123\trunning\textra")))

	if _, err := p.List(t.Context()); err == nil {
		t.Fatal("an extra column was absorbed into the name, which mis-identifies the lease")
	}
}

// THE LABEL OPERATORS ARE TOLD TO LOOK FOR IS THIS ONE.
//
// internal/state tells an operator recovering a lost deployment identity to read
// it off a container's label, and it cannot import this package to name it — the
// control plane must not depend on a compute backend. So the string is written
// out there and pinned here.
//
// Not hypothetical tidiness: that message shipped naming "billet.deployment",
// which billet has never written to a container. An operator following it during
// an incident would have found nothing and concluded their compute was
// unrecoverable — while the containers sat there labelled correctly.
func TestTheOwnerLabelMatchesWhatOperatorsAreTold(t *testing.T) {
	t.Parallel()

	const told = "sh.billet.owner"

	if ownerLabel != told {
		t.Fatalf("containers are labelled %q, but internal/state tells operators to look for "+
			"%q; one of the two has to change, and the operator-facing message is the one "+
			"somebody reads during an incident", ownerLabel, told)
	}
}
