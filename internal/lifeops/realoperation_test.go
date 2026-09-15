package lifeops

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Opt in only inside a disposable systemd host. The command tests use the
// production retirement caller; this test establishes the manager's actual
// effects, logging its version and measurement time without a report artifact.
func TestRealSystemdRetirementOperationEffects(t *testing.T) {
	if os.Getenv("BILLET_TEST_SYSTEMD_OPERATION_EFFECTS") != "1" {
		t.Skip("requires BILLET_TEST_SYSTEMD_OPERATION_EFFECTS=1 in a disposable systemd host")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Fatal("the disposable systemd host must run Linux as root")
	}
	version, err := exec.CommandContext(t.Context(), "systemctl", "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(version), "systemd 255 ") && !strings.HasPrefix(string(version), "systemd 255\n") {
		t.Fatalf("this measurement is pinned to systemd 255: %s", version)
	}
	t.Logf("systemd measurement %s: %s", time.Now().UTC().Format(time.RFC3339), strings.TrimSpace(string(version)))
	for _, role := range []bool{false, true} {
		name := "shipped sequence"
		if role {
			name = "role sequence"
		}
		t.Run(name, func(t *testing.T) { realRetirementSequence(t, role) })
	}
	for _, hazard := range []string{"clean", "success timer", "stop propagation", "shared runtime"} {
		t.Run(hazard, func(t *testing.T) {
			prefix := fmt.Sprintf("billet-effects-%d-%d", os.Getpid(), time.Now().UnixNano())
			server, node := prefix+"-server.service", prefix+"-node.service"
			timer, backup := prefix+"-backup.timer", prefix+"-backup.service"
			runtimeName := prefix + "/registration"
			record := filepath.Join("/run", runtimeName, "current")
			backupEffect := filepath.Join(t.TempDir(), "backup-started")
			unitDirectory := "/run/systemd/system"
			names := []string{server, node, timer, backup}
			ctl := func(ctx context.Context, args ...string) error {
				out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
				if err != nil {
					return fmt.Errorf("systemctl %v: %w: %s", args, err, out)
				}
				return nil
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
				defer cancel()
				for _, unit := range names {
					if err := ctl(ctx, "stop", "--", unit); err != nil {
						t.Error(err)
					}
				}
				for _, unit := range names {
					if err := os.Remove(filepath.Join(unitDirectory, unit)); err != nil && !os.IsNotExist(err) {
						t.Error(err)
					}
				}
				if err := ctl(ctx, "daemon-reload"); err != nil {
					t.Error(err)
				}
			})
			unitExtra, serviceExtra := "", ""
			switch hazard {
			case "success timer":
				unitExtra = "OnSuccess=" + timer + "\n"
			case "stop propagation":
				unitExtra = "PropagatesStopTo=" + node + "\n"
			case "shared runtime":
				serviceExtra = "RuntimeDirectory=" + runtimeName + "\n"
			}
			bodies := map[string]string{
				server: "[Unit]\nDefaultDependencies=no\n" + unitExtra + "[Service]\nType=simple\nExecStart=/bin/sleep infinity\n" + serviceExtra,
				node:   "[Unit]\nDefaultDependencies=no\n[Service]\nType=simple\nExecStart=/bin/sleep infinity\nRuntimeDirectory=" + runtimeName + "\n",
				timer:  "[Unit]\nDefaultDependencies=no\n[Timer]\nOnActiveSec=1ms\nUnit=" + backup + "\n",
				backup: "[Unit]\nDefaultDependencies=no\n[Service]\nType=oneshot\nExecStart=/usr/bin/touch " + backupEffect + "\n",
			}
			for _, unit := range names {
				if err := os.WriteFile(filepath.Join(unitDirectory, unit), []byte(bodies[unit]), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := ctl(t.Context(), "daemon-reload"); err != nil {
				t.Fatal(err)
			}
			if err := ctl(t.Context(), "start", "--", node, server); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(record, []byte("original registration\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var mutations []string
			i := NewInspector(WithObserver(func(_ context.Context, args []string) {
				if args[0] != "show" && args[0] != "get-property" {
					mutations = append(mutations, strings.Join(args, " "))
				}
			}))
			before, err := i.UnitProperties(t.Context(), node, "InvocationID")
			if err != nil {
				t.Fatal(err)
			}
			err = i.AdmitOperations(t.Context(), []Operation{{Verb: "stop", Unit: server}}, OperationProtection{
				Units: names, UnitPaths: map[string][]string{node: {filepath.Dir(record)}},
			})
			if err == nil {
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				_, err = NewConverger(i).StopAndProve(ctx, server)
			}
			if hazard == "clean" {
				if err != nil || !slices.ContainsFunc(mutations, func(op string) bool { return strings.HasPrefix(op, "stop ") }) {
					t.Fatalf("clean controller stop: %v, operations=%v", err, mutations)
				}
			} else if err == nil || !strings.Contains(err.Error(), "operation-") || len(mutations) != 0 {
				t.Fatalf("preventive %s refusal: %v, operations=%v", hazard, err, mutations)
			}
			after, err := i.UnitProperties(t.Context(), node, "InvocationID", "ActiveState")
			if err != nil || first(after, "ActiveState") != "active" || first(after, "InvocationID") != first(before, "InvocationID") {
				t.Fatalf("node invocation moved: before=%v after=%v err=%v", before, after, err)
			}
			if body, err := os.ReadFile(record); err != nil || string(body) != "original registration\n" {
				t.Fatalf("node runtime record changed: %q %v", body, err)
			}
			if _, err := os.Lstat(backupEffect); !os.IsNotExist(err) {
				t.Fatalf("controller stop activated backup: %v", err)
			}
		})
	}
}

// Load the shipped definitions without running their workload. Default
// dependencies remain enabled, so this covers the host's real infrastructure
// closure. The role variant renders the actual templates, including firecracker
// networking and the ledger mount fence; Jinja2 is required by that opt-in host.
func realRetirementSequence(t *testing.T, role bool) {
	t.Helper()
	prefix := fmt.Sprintf("billet-sequence-%d-%d", os.Getpid(), time.Now().UnixNano())
	unitDirectory := "/run/systemd/system"
	server, node := prefix+"-server.service", prefix+"-node.service"
	backupTimer, upgradeTimer := prefix+"-backup.timer", prefix+"-upgrade.timer"
	ledgerPath := "/run/" + prefix + "/ledger"
	ledgerUnit := "run-" + strings.ReplaceAll(prefix, "-", `\x2d`) + "-ledger.mount"
	var installed []string
	ctl := func(ctx context.Context, args ...string) error {
		out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemctl %v: %w: %s", args, err, out)
		}
		return nil
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()
		for _, name := range installed {
			if err := os.Remove(filepath.Join(unitDirectory, name)); err != nil {
				t.Error(err)
			}
		}
		if err := ctl(ctx, "daemon-reload"); err != nil {
			t.Error(err)
		}
	})
	writeUnit := func(name, body string) {
		path := filepath.Join(unitDirectory, name)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		installed = append(installed, name)
		_, writeErr := file.WriteString(body)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("write %s: %v %v", path, writeErr, closeErr)
		}
	}
	for _, name := range []string{"billet-server.service", "billet-node.service", "billet-backup.service", "billet-upgrade.service", "billet-backup.timer", "billet-upgrade.timer"} {
		path := filepath.Join("..", "..", "deploy", name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if role && (name == "billet-server.service" || name == "billet-node.service") {
			path = filepath.Join("..", "..", "ansible_collections", "junioryono", "billet", "roles", "host", "templates", name+".j2")
			body, err = exec.CommandContext(t.Context(), "python3", "-c", `
import sys
from jinja2 import Environment, StrictUndefined
with open(sys.argv[1], encoding="utf-8") as source:
    template = Environment(undefined=StrictUndefined).from_string(source.read())
print(template.render(
    billet_firecracker_enabled=True,
    billet_config={},
    billet_ledger_volume_id="fixture-volume",
    billet_ledger_mount_unit_name=sys.argv[2],
    billet_candidate_server_state_dir=sys.argv[3],
    billet_service_user="root",
    billet_service_group="root",
))
`, path, "FIXTURE_LEDGER_UNIT", "FIXTURE_LEDGER_PATH").CombinedOutput()
			if err != nil {
				t.Fatalf("render role template (requires python3 Jinja2): %v: %s", err, body)
			}
		}
		body = []byte(strings.NewReplacer("billet-", prefix+"-", "billet/", prefix+"/").Replace(string(body)))
		body = []byte(strings.NewReplacer("FIXTURE_LEDGER_UNIT", ledgerUnit, "FIXTURE_LEDGER_PATH", ledgerPath).Replace(string(body)))
		writeUnit(strings.Replace(name, "billet-", prefix+"-", 1), string(body))
	}
	if role {
		writeUnit(prefix+"-network.service", "[Unit]\nWants=network-online.target\n[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStartPre=/usr/bin/true\nExecStart=/usr/bin/true\nExecStop=/usr/bin/true\n")
		writeUnit(ledgerUnit, "[Mount]\nWhat=tmpfs\nType=tmpfs\nWhere="+ledgerPath+"\n")
	}
	if err := ctl(t.Context(), "daemon-reload"); err != nil {
		t.Fatal(err)
	}
	protection := OperationProtection{
		Units: []string{server, node, prefix + "-backup.service", prefix + "-upgrade.service", backupTimer, upgradeTimer},
		Paths: []string{"/var/lib/" + prefix + "/retired", "/var/lib/" + prefix + "/authority.lock", "/var/lib/" + prefix + "/upgrades"},
		UnitPaths: map[string][]string{
			server: {"/var/lib/" + prefix + "/server", ledgerPath},
			node:   {"/var/lib/" + prefix + "/node", "/run/" + prefix + "/locks", "/run/" + prefix + "/registration"},
		},
	}
	var sequence []Operation
	for _, unit := range []string{upgradeTimer, backupTimer, server} {
		sequence = append(sequence, Operation{Verb: "stop", Unit: unit}, Operation{Verb: "disable", Unit: unit})
	}
	for _, verb := range []string{"enable", "stop", "start"} {
		sequence = append(sequence, Operation{Verb: verb, Unit: node})
	}
	var calls []string
	i := NewInspector(WithObserver(func(_ context.Context, args []string) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] != "show" && args[0] != "get-property" {
			t.Errorf("admission submitted a job: %v", args)
		}
	}))
	if err := i.AdmitOperations(t.Context(), sequence, protection); err != nil {
		t.Fatalf("clean full retirement sequence (role=%v): %v", role, err)
	}
	if !slices.ContainsFunc(calls, func(call string) bool { return strings.HasPrefix(call, "get-property ") }) {
		t.Fatal("clean sequence did not obtain typed directory and command evidence")
	}
}
