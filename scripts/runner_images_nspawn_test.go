package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// nspawnFakes are systemd-run, machinectl, systemctl, iptables and timeout as the
// driver meets them. Each call appends its argv to $FAKE/calls.
//   - systemd-run exits with $FAKE/status for an exec and answers `test -d` from
//     $FAKE/dirs; starting nspawn makes the unit active and the machine
//     registered.
//   - machinectl: the machine is registered while $FAKE/leader exists, and show
//     answers its Leader or Name, or "No machine" when it is not; a reboot moves
//     the leader to $FAKE/next-leader; a poweroff or terminate ends it unless
//     $FAKE/stuck names that verb.
//   - systemctl answers is-active from $FAKE/state (inactive when absent); a
//     stop ends the unit and the machine unless $FAKE/stuck names stop.
//   - iptables keeps its FORWARD rules in $FAKE/rules; -S fails while
//     $FAKE/iptables-broken exists.
//   - timeout drops its bounds and runs the command.
var nspawnFakes = map[string]string{
	"systemd-run": `#!/bin/sh
echo "systemd-run $*" >>"$FAKE/calls"
for last; do :; done
case " $* " in
*" -- test -d "*) grep -qxF -- "$last" "$FAKE/dirs" ; exit $? ;;
*" -- /bin/true "* | *" -- getent hosts "*) exit 0 ;;
*" -- systemd-nspawn "*) echo active >"$FAKE/state"; echo 100 >"$FAKE/leader"; exit 0 ;;
esac
[ -f "$FAKE/status" ] && exit "$(cat "$FAKE/status")"
exit 0
`,
	"machinectl": `#!/bin/sh
echo "machinectl $*" >>"$FAKE/calls"
stuck() { grep -qx "$1" "$FAKE/stuck" 2>/dev/null; }
case "$1" in
show)
	if [ ! -f "$FAKE/leader" ]; then echo "No machine 'probe' known" >&2; exit 1; fi
	case "$*" in *Leader*) cat "$FAKE/leader" ;; *) echo probe ;; esac ;;
reboot) [ -f "$FAKE/next-leader" ] && cp "$FAKE/next-leader" "$FAKE/leader" ;;
poweroff | terminate) stuck "$1" || { echo inactive >"$FAKE/state"; rm -f "$FAKE/leader"; } ;;
esac
exit 0
`,
	"systemctl": `#!/bin/sh
echo "systemctl $*" >>"$FAKE/calls"
case "$1" in
is-active) state=$(cat "$FAKE/state" 2>/dev/null || echo inactive); echo "$state"
	[ "$state" = active ] ;;
stop) grep -qx stop "$FAKE/stuck" 2>/dev/null || { echo inactive >"$FAKE/state"; rm -f "$FAKE/leader"; } ;;
show) echo "" ;;
esac
`,
	"iptables": `#!/bin/sh
echo "iptables $*" >>"$FAKE/calls"
shift
case "$1" in
-S) [ -f "$FAKE/iptables-broken" ] && exit 1
	echo "-P FORWARD DROP"; cat "$FAKE/rules" 2>/dev/null ;;
-I) shift; echo "-A $*" >>"$FAKE/rules" ;;
-D) shift; want="-A $*"; grep -vxF -- "$want" "$FAKE/rules" >"$FAKE/rules.new" || true
	mv "$FAKE/rules.new" "$FAKE/rules" ;;
esac
exit 0
`,
	"timeout": `#!/bin/sh
shift 3
exec "$@"
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
	cmd := exec.CommandContext(t.Context(), "./runner-images-nspawn.sh", args...)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE="+fake,
		"BILLET_RI_ROOTFS=/mnt/rootfs", "BILLET_RI_MACHINE=probe", "BILLET_RI_STOP_WAIT=1")
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

// START BRINGS THE MACHINE UP WITH ITS OWN NETWORK AND OWNED FORWARDING RULES, and
// refuses one that is already running rather than claiming it.
func TestTheNspawnDriverStartsItsOwnMachine(t *testing.T) {
	t.Parallel()

	fake := t.TempDir()
	if output, err := runDriver(t, fake, "start"); err != nil {
		t.Fatalf("start: %v\n%s", err, output)
	}
	calls := driverCalls(t, fake)
	for _, want := range []string{"--unit=probe-nspawn.service", "--network-veth", "--resolv-conf=replace-uplink",
		"--machine=probe", "--directory=/mnt/rootfs", "-- getent hosts archive.ubuntu.com",
		"--keep-unit",
		"iptables -w -I FORWARD -i ve-probe -m comment --comment billet-runner-images:probe -j ACCEPT"} {
		if !strings.Contains(calls, want) {
			t.Errorf("start did not pass %q:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "/dev/fuse") {
		t.Error("start binds /dev/fuse, which the guest kernel does not provide")
	}
	if output, err := runDriver(t, fake, "start"); err == nil || !strings.Contains(output, "already running") {
		t.Fatalf("a second start of a running machine answered %v:\n%s", err, output)
	}
}

// STOP PROVES THE MACHINE GONE: a poweroff that does not take is followed by
// stopping the unit, a launcher that failed while its machine is still registered
// is not taken for stopped, a unit whose state cannot be read never is, and the
// forwarding rules come out with it, or the stop fails.
func TestTheNspawnDriverProvesTheMachineStopped(t *testing.T) {
	t.Parallel()

	fake := t.TempDir()
	if output, err := runDriver(t, fake, "start"); err != nil {
		t.Fatalf("start: %v\n%s", err, output)
	}
	writeFake(t, fake, "stuck", "poweroff\n")
	if output, err := runDriver(t, fake, "stop"); err != nil {
		t.Fatalf("stop: %v\n%s", err, output)
	}
	calls := driverCalls(t, fake)
	if !strings.Contains(calls, "systemctl stop probe-nspawn.service") {
		t.Fatalf("a machine that would not power off was not stopped:\n%s", calls)
	}
	if rules, err := os.ReadFile(filepath.Join(fake, "rules")); err != nil || strings.TrimSpace(string(rules)) != "" {
		t.Fatalf("the forwarding rules survived the stop (%v): %q", err, rules)
	}

	failed := t.TempDir()
	writeFake(t, failed, "state", "failed\n")
	writeFake(t, failed, "leader", "100\n")
	writeFake(t, failed, "stuck", "poweroff\nstop\nterminate\n")
	if output, err := runDriver(t, failed, "stop"); err == nil || !strings.Contains(output, "still running") {
		t.Fatalf("a failed launcher whose machine is still registered answered %v:\n%s", err, output)
	}

	unknown := t.TempDir()
	writeFake(t, unknown, "state", "unknown\n")
	if output, err := runDriver(t, unknown, "stop"); err == nil || !strings.Contains(output, "cannot tell") {
		t.Fatalf("a unit whose state cannot be read answered %v:\n%s", err, output)
	}

	broken := t.TempDir()
	if output, err := runDriver(t, broken, "start"); err != nil {
		t.Fatalf("start: %v\n%s", err, output)
	}
	writeFake(t, broken, "iptables-broken", "")
	if output, err := runDriver(t, broken, "stop"); err == nil || !strings.Contains(output, "cannot read the FORWARD chain") {
		t.Fatalf("a stop that could not read the ruleset answered %v:\n%s", err, output)
	}
}

// A MACHINE NAME TOO LONG FOR ITS LINK NAME IS REFUSED, since the forwarding rules
// match the link by that exact name.
func TestTheNspawnDriverRefusesALongMachineName(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(t.Context(), "./runner-images-nspawn.sh", "stop")
	cmd.Env = append(os.Environ(), "BILLET_RI_ROOTFS=/mnt/rootfs", "BILLET_RI_MACHINE=billet-runner-images")
	if output, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(output), "longer than twelve") {
		t.Fatalf("a long machine name answered %v:\n%s", err, output)
	}
}
