package node

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/nodeapi"
)

// fakeSystemdRun records the real dispatch argv and executes its command in a
// separate process. Its child closes output so CombinedOutput observes the launcher.
func fakeSystemdRun(t *testing.T, systemd bool, refusal string) string {
	t.Helper()

	originalUnder, originalRun := underSystemd, systemdRun
	underSystemd = func() bool { return systemd }
	systemdRun = "systemd-run"

	t.Cleanup(func() {
		underSystemd, systemdRun = originalUnder, originalRun
	})

	dir := t.TempDir()
	record := filepath.Join(dir, "args")
	body := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$@\" > " + shellQuoteUpgrade(record) + "\n"
	if refusal != "" {
		body += "printf '%s\\n' " + shellQuoteUpgrade(refusal) + " >&2\nexit 1\n"
	} else {
		body += `dsn_name=''
dsn_value=''
while [ "$1" != -- ]; do
    case "$1" in
        --setenv=*)
            dsn_name=${1#--setenv=}
            dsn_value=$(printenv "$dsn_name")
            ;;
    esac
    shift
done
shift
if [ -n "$dsn_name" ]; then
    env -i "$dsn_name=$dsn_value" "$@" >/dev/null 2>&1 &
else
    env -i "$@" >/dev/null 2>&1 &
fi
`
	}

	if err := os.WriteFile(filepath.Join(dir, "systemd-run"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return record
}

func assertNoAckSockets(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "upgrade-ack-") {
			t.Errorf("dispatch left its answer socket behind: %s", entry.Name())
		}
	}
}

func TestSystemdDispatchRunsTheUpdaterInItsOwnUnit(t *testing.T) {
	record := fakeSystemdRun(t, true, "")
	e := ExecUpgrader{Binary: fakeUpdater(t, AckAccepted), AckDir: shortAckDir(t),
		ConfigPath: "/etc/billet/billet.yaml"}
	spec := nodeapi.UpgradeSpec{Version: "v0.9.3", RolloutID: "abc123", Generation: 7,
		ManifestSHA256: strings.Repeat("a", 64)}

	if err := e.StartUpgrade(t.Context(), spec); err != nil {
		t.Fatalf("the transient updater refused: %v", err)
	}

	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("systemd-run was not invoked: %v", err)
	}

	args := strings.Split(strings.TrimSpace(string(body)), "\n")
	for _, want := range []string{"--unit=billet-host-upgrade-abc123-g7",
		"--description=billet host upgrade to v0.9.3", "--service-type=oneshot",
		"--collect", "--quiet", "--no-block", "--property=TimeoutStartSec=88200"} {
		if !slices.Contains(args, want) {
			t.Errorf("systemd-run lacks %q: %q", want, args)
		}
	}

	separator := slices.Index(args, "--")
	if separator < 0 {
		t.Fatalf("no command separator: %q", args)
	}

	command := args[separator+1:]
	ackFlag := slices.Index(command, "--ack-path")
	if ackFlag < 0 || ackFlag+1 >= len(command) {
		t.Fatalf("no answer socket in the updater command: %q", command)
	}

	ackPath := command[ackFlag+1]
	if !strings.HasPrefix(ackPath, filepath.Join(e.AckDir, "upgrade-ack-")) {
		t.Errorf("answer socket is outside the node's writable state directory: %s", ackPath)
	}

	want := []string{e.Binary, "host-upgrade", "--version", spec.Version,
		"--manifest-sha256", spec.ManifestSHA256, "--rollout", spec.RolloutID,
		"--generation", "7", "--config", e.ConfigPath, "--ack-path", ackPath}
	if !slices.Equal(command, want) {
		t.Errorf("transient command = %q, want %q", command, want)
	}

	assertNoAckSockets(t, e.AckDir)
}

func TestSystemdDispatchCarriesTheUpdatersRefusal(t *testing.T) {
	fakeSystemdRun(t, true, "")
	e := ExecUpgrader{Binary: fakeUpdater(t, AckRefused+"another upgrade holds the claim"),
		AckDir: shortAckDir(t)}

	err := e.StartUpgrade(t.Context(), nodeapi.UpgradeSpec{Version: "v0.9.3"})
	if !errors.Is(err, ErrUpgradeRefused) || !strings.Contains(err.Error(), "holds the claim") {
		t.Errorf("transient updater's refusal was lost: %v", err)
	}

	assertNoAckSockets(t, e.AckDir)
}

func TestSystemdRunFailureIsReportedWithoutWaitingForAnAnswer(t *testing.T) {
	fakeSystemdRun(t, true, "Unit billet-host-upgrade-abc123-g7.service already exists")
	e := ExecUpgrader{Binary: fakeUpdater(t, AckAccepted), AckDir: shortAckDir(t)}
	start := time.Now()
	err := e.StartUpgrade(t.Context(), nodeapi.UpgradeSpec{Version: "v0.9.3"})

	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("systemd-run's failure was lost: %v", err)
	}

	if time.Since(start) > 5*time.Second {
		t.Error("the dispatch waited for an answer after systemd-run failed")
	}

	assertNoAckSockets(t, e.AckDir)
}

func TestDispatchOutsideSystemdLaunchesDirectly(t *testing.T) {
	record := fakeSystemdRun(t, false, "systemd-run must not be called")
	e := ExecUpgrader{Binary: fakeUpdater(t, AckAccepted), AckDir: shortAckDir(t)}

	if err := e.StartUpgrade(t.Context(), nodeapi.UpgradeSpec{Version: "v0.9.3"}); err != nil {
		t.Fatalf("direct updater refused: %v", err)
	}

	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Errorf("systemd-run was called outside systemd, or its record is unreadable: %v", err)
	}

	assertNoAckSockets(t, e.AckDir)
}

// The manager starts with its own environment; the launcher must explicitly carry
// the configured ledger variable, without exposing its value in the command line.
func TestSystemdDispatchPreservesTheLedgerEnvironmentWithoutExposingIt(t *testing.T) {
	const name = "BILLET_UPGRADE_TEST_DSN"
	const value = "postgres://fixture:private-fixture@localhost/ledger"

	t.Setenv(name, value)
	t.Setenv("BILLET_UPGRADE_UNRELATED", "must not be forwarded")
	record := fakeSystemdRun(t, true, "")
	e := ExecUpgrader{Binary: fakeUpdaterMode(t, "ledger-env", ""),
		AckDir: shortAckDir(t), DSNEnv: name}

	if err := e.StartUpgrade(t.Context(), nodeapi.UpgradeSpec{Version: "v0.9.3"}); err != nil {
		t.Fatalf("the updater did not receive its ledger environment: %v", err)
	}

	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}

	args := strings.Split(strings.TrimSpace(string(body)), "\n")
	if !slices.Contains(args, "--setenv="+name) {
		t.Errorf("the configured variable name was not forwarded: %q", args)
	}

	if strings.Contains(string(body), value) {
		t.Error("the ledger credential appeared in systemd-run's argv")
	}
}
