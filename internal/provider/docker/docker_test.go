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
// entirely correct — the same class as the App-key argv residual already
// documented in CLAUDE.md.
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

	if err := p.Destroy(t.Context(), "billet-nonexistent-container-name"); err != nil {
		t.Errorf("destroying a container that does not exist reported an error: %v", err)
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
