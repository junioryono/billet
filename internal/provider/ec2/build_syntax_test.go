package ec2

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE SCRIPTS THAT RUN AS ROOT ON THE BUILDER ARE PARSED: the bootstrap in user
// data and the layer build-ami.sh runs after GitHub's template.
//
// THE FAILURE IF ONE IS WRONG IS THE EXPENSIVE SHAPE. A syntax error in the
// bootstrap is a builder that never starts its build and burns the whole timeout;
// one in the layer fails hours into a build.
//
// EVERY SHELL THAT MIGHT RUN IT. Ubuntu's /bin/sh is dash and this suite's is
// not, so a bashism would pass here and fail there; dash is checked when it is
// installed and the suite says so when it is not.
func TestTheGeneratedScriptParses(t *testing.T) {
	t.Parallel()

	for _, spec := range []struct {
		name string
		spec BuildSpec
	}{
		{
			name: "x64",
			spec: BuildSpec{RunnerSHA256: testRunnerDigest, RunnerVersion: "2.328.0", Arch: "x64"},
		},
		{
			name: "arm64",
			spec: BuildSpec{RunnerSHA256: testRunnerDigest, RunnerVersion: "2.328.0", Arch: "arm64"},
		},
		{
			name: "x64 with a CA",
			spec: BuildSpec{RunnerSHA256: testRunnerDigest, RunnerVersion: "2.328.0", Arch: "x64"},
		},
	} {
		t.Run(spec.name, func(t *testing.T) {
			t.Parallel()

			s := spec.spec
			if strings.Contains(spec.name, "CA") {
				s.CACertPEM = mintCert(t, true)
			}

			layer, err := layerScript(s)
			if err != nil {
				t.Fatalf("layerScript: %v", err)
			}

			s.payloadURL, s.payloadSHA256, s.nonce = testPayloadURL, testPayloadDigest, verifyProbeNonce

			bootstrap, err := bootstrapScript(s)
			if err != nil {
				t.Fatalf("bootstrapScript: %v", err)
			}

			for name, script := range map[string]string{"layer": layer, "bootstrap": bootstrap} {
				path := filepath.Join(t.TempDir(), name+".sh")
				if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
					t.Fatalf("write the script: %v", err)
				}

				shells := []string{"sh"}
				for _, extra := range []string{"dash", "bash"} {
					if _, err := exec.LookPath(extra); err == nil {
						shells = append(shells, extra)
					}
				}

				for _, shell := range shells {
					out, err := exec.CommandContext(t.Context(), shell, "-n", path).CombinedOutput()
					if err != nil {
						t.Errorf("the %s does not parse under %s: %v\n%s", name, shell, err, out)
					}
				}

				if len(shells) == 1 {
					t.Log("only /bin/sh was available; dash is what Ubuntu runs this under, so a " +
						"bashism could still reach a builder")
				}
			}
		})
	}
}

// THE DAEMON IS RESTARTED BEFORE IT IS ASKED, not started.
//
// `systemctl start` on an already-running unit is a NO-OP — it returns success,
// changes nothing, and the daemon keeps the configuration it read at start time.
// GitHub's install-docker.sh leaves dockerd running, so a `start` here means the
// gate below measures a daemon that may not be the one the image will run.
//
// MEASURED ON A REAL BUILDER, because both readings were defensible: driver
// `overlayfs` after `systemctl start`, `overlay2` after `systemctl restart`, same
// instance, same file. With `start` the gate fails on a correct image; the AMI
// itself was always fine, because a fresh boot from it starts Docker with
// daemon.json already on disk.
func TestTheDockerDaemonIsRestartedAfterItsConfigIsWritten(t *testing.T) {
	t.Parallel()

	script := mustScript(t)
	lines := strings.Split(script, "\n")

	// THE VERB, because `start` is the whole defect and it is one word away.
	for _, line := range lines {
		if strings.TrimSpace(line) == "systemctl start docker" {
			t.Error("the build starts the Docker daemon rather than restarting it; the " +
				"template leaves it running, so `start` is a no-op and the gate then measures " +
				"a daemon that may not be the one the image will run")
		}
	}

	restart := firstLineOf(t, lines, "systemctl restart docker")

	// AND THE ASSERTIONS COME AFTER THE RESTART, or they read the pre-restart
	// daemon and the restart is decoration.
	driver := firstLineOf(t, lines, "{{.Driver}}")
	if driver <= restart {
		t.Errorf("the driver is asserted at line %d and the daemon restarted at %d", driver,
			restart)
	}
}

// THE SCRIPTS HAVE A HOME BEFORE ANYTHING THAT NEEDS ONE RUNS. cloud-init runs
// user data with no HOME and a system unit gives its service none, and tools that
// want one refuse to start without it: an AMI build died on Composer's installer,
// before the runner was fetched, until its script exported root's (measured on a
// real builder, 2026-10-17).
func TestTheBuildScriptsHaveAHomeFirst(t *testing.T) {
	t.Parallel()

	bootstrap := mustBootstrap(t)

	for _, tc := range []struct {
		name, script, later string
	}{
		{"layer", mustScript(t), "curl -fsSL -o runner.tar.gz "},
		{"bootstrap", bootstrap, "curl -fsSL --retry 5"},
	} {
		lines := strings.Split(tc.script, "\n")
		home := firstLineOf(t, lines, "export HOME=/root")

		if at := firstLineOf(t, lines, tc.later); at <= home {
			t.Errorf("the %s runs %q at line %d and exports HOME at %d", tc.name, tc.later, at, home)
		}
	}
}
