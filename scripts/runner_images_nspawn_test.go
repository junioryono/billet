package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// nspawnFakes are systemd-run and machinectl as the driver meets them. Each call
// appends its argv to $FAKE/calls. systemd-run exits with $FAKE/status for an
// exec, answers `test -d` from $FAKE/dirs, and machinectl answers the machine's
// Leader from $FAKE/leader, moving it on to $FAKE/next-leader after a reboot.
var nspawnFakes = map[string]string{
	"systemd-run": `#!/bin/sh
echo "systemd-run $*" >>"$FAKE/calls"
for last; do :; done
case " $* " in
*" -- test -d "*) grep -qxF -- "$last" "$FAKE/dirs" ; exit $? ;;
*" -- /bin/true "*) exit 0 ;;
esac
[ -f "$FAKE/status" ] && exit "$(cat "$FAKE/status")"
exit 0
`,
	"machinectl": `#!/bin/sh
echo "machinectl $*" >>"$FAKE/calls"
case "$1" in
show) cat "$FAKE/leader" 2>/dev/null ;;
reboot) [ -f "$FAKE/next-leader" ] && cp "$FAKE/next-leader" "$FAKE/leader" ;;
poweroff) rm -f "$FAKE/leader" ;;
esac
exit 0
`,
}

func runDriver(t *testing.T, fake string, args ...string) (string, error) {
	t.Helper()

	bin := filepath.Join(fake, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range nspawnFakes {
		if err := forkSafeWriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), "bash", append([]string{"runner-images-nspawn.sh"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE="+fake,
		"BILLET_RI_ROOTFS=/mnt/rootfs", "BILLET_RI_MACHINE=probe")
	output, err := cmd.CombinedOutput()

	return string(output), err
}

func driverCalls(t *testing.T, fake string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(fake, "calls"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	return string(raw)
}

func writeFake(t *testing.T, fake, name, body string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(fake, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// AN EXEC CARRIES THE COMMAND'S STATUS, which is the step's verdict, and runs in
// the machine with root's HOME.
func TestTheNspawnDriverCarriesTheCommandsStatus(t *testing.T) {
	t.Parallel()

	fake := t.TempDir()
	writeFake(t, fake, "status", "7")
	_, err := runDriver(t, fake, "exec", "env", "A=b", "/var/tmp/billet-ri/3")
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitErr.ExitCode() != 7 {
		t.Fatalf("exec answered %v, want the command's status 7", err)
	}
	calls := driverCalls(t, fake)
	for _, want := range []string{"--machine=probe", "--wait --pipe", "--setenv=HOME=/root",
		"-- env A=b /var/tmp/billet-ri/3"} {
		if !strings.Contains(calls, want) {
			t.Errorf("exec did not pass %q:\n%s", want, calls)
		}
	}
}

// A COPY GOES THROUGH machinectl, with `cp -R` semantics decided inside the
// machine: into an existing directory it lands inside it.
func TestTheNspawnDriverCopiesInsideTheMachine(t *testing.T) {
	t.Parallel()

	fake := t.TempDir()
	writeFake(t, fake, "dirs", "/imagegeneration\n")
	if output, err := runDriver(t, fake, "copy-in", "/host/scripts/tests", "/imagegeneration"); err != nil {
		t.Fatalf("copy-in: %v\n%s", err, output)
	}
	if output, err := runDriver(t, fake, "copy-in", "/host/scripts/helpers", "/imagegeneration/helpers"); err != nil {
		t.Fatalf("copy-in: %v\n%s", err, output)
	}
	calls := driverCalls(t, fake)
	for _, want := range []string{
		"machinectl copy-to probe /host/scripts/tests /imagegeneration/tests",
		"machinectl copy-to probe /host/scripts/helpers /imagegeneration/helpers",
	} {
		if !strings.Contains(calls, want+"\n") {
			t.Errorf("copy-in did not run %q:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "cp ") {
		t.Errorf("a copy went around machinectl:\n%s", calls)
	}
}

// A REBOOT WAITS FOR A NEW INIT, not for the old one to answer again, and fails
// when none appears.
func TestTheNspawnDriverWaitsForTheRebootedMachine(t *testing.T) {
	t.Parallel()

	fake := t.TempDir()
	writeFake(t, fake, "leader", "100\n")
	writeFake(t, fake, "next-leader", "200\n")
	if output, err := runDriver(t, fake, "reboot"); err != nil {
		t.Fatalf("reboot: %v\n%s", err, output)
	}
	if calls := driverCalls(t, fake); !strings.Contains(calls, "machinectl reboot probe") ||
		!strings.Contains(calls, "-- /bin/true") {
		t.Fatalf("reboot did not reboot and then wait for an answer:\n%s", calls)
	}

	stopped := t.TempDir()
	if output, err := runDriver(t, stopped, "reboot"); err == nil || !strings.Contains(output, "is not running") {
		t.Fatalf("a reboot of a machine that is not running answered %v:\n%s", err, output)
	}
}
