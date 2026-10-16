package host

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/state"
)

// repairStandinEnv makes the test binary the witness's node: it publishes a
// registration record as the node does, and only once the directory the record
// needs exists, which is the skew #168 describes.
const repairStandinEnv = "BILLET_REPAIR_NODE_STANDIN"

// runRepairStandin is the witness's node, run by TestMain under systemd: it
// waits for the record's directory, writes a record naming its invocation, and
// stays up until stopped.
func runRepairStandin() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	dir := filepath.Dir(registrationRecordPath)
	ticker := time.NewTicker(100 * time.Millisecond)

	defer ticker.Stop()

	written := false

	for {
		if !written {
			if info, err := os.Stat(dir); err == nil && info.IsDir() {
				if err := writeStandinRecord(dir); err != nil {
					fmt.Fprintln(os.Stderr, err)

					return 1
				}

				written = true
			}
		}

		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
		}
	}
}

func writeStandinRecord(dir string) error {
	body, err := json.Marshal(map[string]any{
		"schema": 1, "node": os.Getenv("BILLET_REPAIR_NODE_NAME"), "deployment": os.Getenv("BILLET_REPAIR_DEPLOYMENT"),
		"incarnation": os.Getenv("BILLET_REPAIR_INCARNATION"), "invocation_id": os.Getenv("INVOCATION_ID"),
		"endpoint": os.Getenv("BILLET_REPAIR_ENDPOINT"), "registered_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}

	tmp := filepath.Join(dir, ".current")
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return err
	}

	return os.Rename(tmp, filepath.Join(dir, "current"))
}

// TestRealSystemdRepairGivesALegacyNodeUnitItsRecordDirectory reproduces #168
// against real systemd: a node unit in the shape an older collection rendered
// (RuntimeDirectory=billet/locks alone) runs a node that would publish its
// registration record and cannot, so the endpoint decision reports
// unit-cannot-hold; the repair's dry run changes nothing; the repair changes
// only that line, and the node it restarts publishes a record under the new
// invocation naming the installed endpoint, after which the decision reads it.
func TestRealSystemdRepairGivesALegacyNodeUnitItsRecordDirectory(t *testing.T) {
	if os.Getenv("BILLET_TEST_SYSTEMD_OPERATION_EFFECTS") != "1" {
		t.Skip("requires BILLET_TEST_SYSTEMD_OPERATION_EFFECTS=1 in a disposable systemd host")
	}

	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Fatal("the disposable systemd host must run Linux as root")
	}

	if _, err := os.Lstat(repairUnitPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists (%v); this witness installs its own and needs a host without billet", repairUnitPath, err)
	}

	version, err := exec.CommandContext(t.Context(), "systemctl", "--version").Output()
	mustOK(t, err)
	t.Logf("systemd measurement %s: %s", time.Now().UTC().Format(time.RFC3339),
		strings.SplitN(string(version), "\n", 2)[0])

	root := fmt.Sprintf("/var/lib/billet-repair-%d-%d", os.Getpid(), time.Now().UnixNano())
	mustOK(t, os.MkdirAll(root, 0o755))

	ctl := func(args ...string) error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
		defer cancel()

		out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl %v: %w: %s", args, err, out)
		}

		return nil
	}

	t.Cleanup(func() {
		if err := ctl("stop", "--", nodeUnit); err != nil {
			t.Error(err)
		}

		if err := os.Remove(repairUnitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error(err)
		}

		if err := ctl("daemon-reload"); err != nil {
			t.Error(err)
		}

		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})

	// THE NODE'S CONFIGURATION AND IDENTITY, as the role installs them.
	stateDir := filepath.Join(root, "node-state")
	deployment, err := state.DeploymentID(stateDir)
	mustOK(t, err)

	configPath := filepath.Join(root, "billet.yaml")
	writeFile(t, configPath, "node:\n  name: repair-witness\n  server_addr: 127.0.0.1:7717\n  provider: docker\n"+
		"  state_dir: "+stateDir+"\n  lock_dir: /run/billet/locks\n", 0o644)

	// THE STAND-IN, copied where the unit's sandbox can execute it.
	self, err := os.Executable()
	mustOK(t, err)

	body, err := os.ReadFile(self)
	mustOK(t, err)

	standin := filepath.Join(root, "standin")
	writeFile(t, standin, string(body), 0o755)

	incarnation := make([]byte, 16)
	_, err = rand.Read(incarnation)
	mustOK(t, err)

	legacy := strings.Join([]string{
		"[Unit]", "Description=billet repair witness (#168)", "",
		"[Service]", "Type=exec", "ExecStart=" + standin + " -test.run=^$",
		"Environment=" + repairStandinEnv + "=1", "Environment=BILLET_REPAIR_NODE_NAME=repair-witness",
		"Environment=BILLET_REPAIR_DEPLOYMENT=" + deployment,
		"Environment=BILLET_REPAIR_INCARNATION=" + hex.EncodeToString(incarnation),
		"Environment=BILLET_REPAIR_ENDPOINT=http://127.0.0.1:7717",
		"RuntimeDirectory=billet/locks", "RuntimeDirectoryMode=0750", "KillMode=mixed", "TimeoutStopSec=30",
		"ProtectSystem=strict", "", "[Install]", "WantedBy=multi-user.target", "",
	}, "\n")
	writeFile(t, repairUnitPath, legacy, 0o644)
	mustOK(t, ctl("daemon-reload"))
	mustOK(t, ctl("start", "--", nodeUnit))

	// THE SKEW: the node runs, and the decision cannot read a record.
	decision := runEndpoint(t, NodeMigrate, "", "--config", configPath, "--wait", "3s", "--dry-run")
	if decision.code != 0 || decision.str("record") != recordUnitCannotHold {
		t.Fatalf("the decision answered code %d record %q (%s %s), want %s", decision.code, decision.str("record"),
			decision.str("reason"), decision.str("why"), recordUnitCannotHold)
	}

	// THE DRY RUN CHANGES NOTHING.
	before := unitProperty(t, "InvocationID")

	dry := runEndpoint(t, NodeRepairUnit, "", "--config", configPath, "--wait", "30s", "--dry-run")
	if dry.code != 0 || !dry.boolean("required") {
		t.Fatalf("the repair's dry run answered code %d required %v (%s)", dry.code, dry.boolean("required"), dry.str("why"))
	}

	if got, err := os.ReadFile(repairUnitPath); err != nil || string(got) != legacy ||
		unitProperty(t, "InvocationID") != before {
		t.Fatalf("the repair's dry run changed the unit or restarted the node (%v)", err)
	}

	// THE REPAIR.
	o := runEndpoint(t, NodeRepairUnit, "", "--config", configPath, "--wait", "30s", "--stop-timeout", "1m")
	if o.code != 0 || o.str("outcome") != outcomeRepaired {
		t.Fatalf("the repair answered code %d outcome %q (%s: %s, state %s)", o.code, o.str("outcome"), o.str("reason"),
			o.str("why"), o.str("state"))
	}

	repaired, err := os.ReadFile(repairUnitPath)
	mustOK(t, err)

	if want := strings.Replace(legacy, "RuntimeDirectory=billet/locks\n",
		"RuntimeDirectory=billet/locks "+deploy.NodeRegistrationDirectory+"\n", 1); string(repaired) != want {
		t.Errorf("the repaired unit is not the legacy one with only RuntimeDirectory changed:\n%s", repaired)
	}

	after := unitProperty(t, "InvocationID")
	if after == before || o.str("invocation_id") != after || o.str("from_invocation") != before {
		t.Errorf("the repair answered invocation %s from %s, and systemd says %s from %s", o.str("invocation_id"),
			o.str("from_invocation"), after, before)
	}

	// THE DECISION NOW READS THE RECORD.
	decision = runEndpoint(t, NodeMigrate, "", "--config", configPath, "--wait", "30s", "--dry-run")
	if decision.code != 0 || decision.str("record") != recordCurrent {
		t.Errorf("after the repair the decision answered code %d record %q (%s)", decision.code, decision.str("record"),
			decision.str("why"))
	}
}

func unitProperty(t *testing.T, name string) string {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "systemctl", "show", "--property="+name, "--value", "--", nodeUnit).Output()
	mustOK(t, err)

	return string(bytes.TrimSpace(out))
}
