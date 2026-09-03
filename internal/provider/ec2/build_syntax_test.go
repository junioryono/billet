package ec2

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE SCRIPT THAT RUNS AS ROOT ON THE BUILDER IS PARSED, and nothing did that.
//
// This branch grew it by three nested heredocs, a single-quoted `bash -c` block,
// a `case` guard, a digest comparison and a per-glob gate. Only the DELIVERED
// installers were ever checked as a program; the /bin/sh script carrying them was
// checked by nobody.
//
// THE FAILURE IF IT IS WRONG IS THE EXPENSIVE SHAPE. A syntax error produces a
// builder that boots, has cloud-init fail, never reaches `poweroff`, and burns
// the whole 60-minute timeout on a paid instance before billet reports that the
// guest never stopped — which names neither the script nor the line.
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
			spec: BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, RunnerVersion: "2.328.0", Arch: "x64"},
		},
		{
			// arm64 TAKES A DIFFERENT PATH — no toolcache, no gate — so parsing
			// one says nothing about the other.
			name: "arm64",
			spec: BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, RunnerVersion: "2.328.0", Arch: "arm64"},
		},
		{
			name: "x64 with a CA",
			spec: BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, RunnerVersion: "2.328.0", Arch: "x64"},
		},
	} {
		t.Run(spec.name, func(t *testing.T) {
			t.Parallel()

			s := spec.spec
			if strings.Contains(spec.name, "CA") {
				s.CACertPEM = mintCert(t, true)
			}

			script, err := provisionScript(s)
			if err != nil {
				t.Fatalf("provisionScript: %v", err)
			}

			path := filepath.Join(t.TempDir(), "provision.sh")
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
					t.Errorf("the provisioning script does not parse under %s: %v\n%s",
						shell, err, out)
				}
			}

			if len(shells) == 1 {
				t.Log("only /bin/sh was available; dash is what Ubuntu runs this under, so a " +
					"bashism could still reach a builder")
			}
		})
	}
}

// AND THE GATE COMES BEFORE THE SUCCESS SIGNAL.
//
// The branch's whole thesis is that a check after `poweroff` is not a check.
// Today the ordering holds by construction — but by construction is how the
// Docker gate was once wrong, so it is asserted rather than assumed.
func TestEveryGateRunsBeforeTheSuccessSignal(t *testing.T) {
	t.Parallel()

	script := mustScript(t)
	lines := strings.Split(script, "\n")

	poweroff := -1

	for i, l := range lines {
		if strings.TrimSpace(l) == "poweroff" {
			poweroff = i
		}
	}

	if poweroff < 0 {
		t.Fatal("the script never powers off, so billet would never see it finish")
	}

	for _, gate := range []struct {
		marker string
		why    string
	}{
		{"billet_free_kib=$(df -Pk /", "the disk is measured before anything installs"},
		{"billet_found=0", "the declared toolcache lines are present"},
		{"billet_env_java() {", "JAVA_HOME names a JDK that runs"},
		// THE ARCHIVE'S DIGEST, which is what replaced the on-builder check of a
		// heredoc'd declaration when the installers stopped fitting in user data.
		// The property is unchanged: the BUILDER verifies what it received, so a
		// check on only the Go side does not leave it trusting whatever arrived.
		{"sha256sum -c -", "the staged payload is the one billet sent"},
	} {
		at := firstLineOf(t, lines, gate.marker)
		if at >= poweroff {
			t.Errorf("the check that %s is at line %d and poweroff is at %d; a check after "+
				"the success signal is not a check", gate.why, at, poweroff)
		}
	}
}

// THE DAEMON IS RESTARTED AFTER daemon.json IS WRITTEN, not started.
//
// `systemctl start` on an already-running unit is a NO-OP — it returns success,
// changes nothing, and the daemon keeps the configuration it read at start time.
// apt's postinst leaves docker.io running, so a `start` here means the daemon
// never reads the daemon.json written a few lines above, and the gate below
// measures a daemon that is not the one the image will run.
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
			t.Error("the build starts the Docker daemon rather than restarting it; apt " +
				"leaves it running, so `start` is a no-op and the daemon never reads the " +
				"daemon.json written above — the gate then measures a daemon that is not " +
				"the one the image will run, and fails on a correct image")
		}
	}

	restart := firstLineOf(t, lines, "systemctl restart docker")
	config := firstLineOf(t, lines, "BILLETDOCKERDAEMON")

	if restart <= config {
		t.Errorf("the daemon is restarted at line %d and its configuration written at %d; "+
			"restarting before the file exists reads the old configuration and is the same "+
			"defect with the lines swapped", restart, config)
	}

	// AND THE ASSERTIONS COME AFTER THE RESTART, or they read the pre-restart
	// daemon and the restart is decoration.
	driver := firstLineOf(t, lines, "{{.Driver}}")
	if driver <= restart {
		t.Errorf("the driver is asserted at line %d and the daemon restarted at %d", driver,
			restart)
	}
}
