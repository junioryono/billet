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

// Opt in only inside a disposable systemd host. Admission never submits a job;
// the counterfactuals deliberately bypass it on separate disposable unit sets.
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
		for _, host := range []string{"retained", "server-only masked", "server-only absent"} {
			t.Run(fmt.Sprintf("sequence/role=%v/%s", role, host), func(t *testing.T) {
				realRetirementSequence(t, role, host)
			})
		}
	}
	for _, bypass := range []bool{false, true} {
		t.Run(fmt.Sprintf("archive path/direct/counterfactual=%v", bypass), func(t *testing.T) {
			realRetirementArchivePath(t, bypass)
		})
	}
	t.Run("outside helper", realRetirementOutsideHelper)
	for _, bypass := range []bool{false, true} {
		t.Run(fmt.Sprintf("cross-role alias/counterfactual=%v", bypass), func(t *testing.T) {
			realRetirementRoleAlias(t, bypass)
		})
		for _, owner := range []string{"server", "backup"} {
			for _, reload := range []bool{false, true} {
				t.Run(fmt.Sprintf("private tmp/%s/reload=%v/counterfactual=%v", owner, reload, bypass), func(t *testing.T) {
					realRetirementPrivateTmp(t, owner, reload, bypass)
				})
			}
		}
		t.Run(fmt.Sprintf("runtime traversal/counterfactual=%v", bypass), func(t *testing.T) {
			realRetirementRuntimeTraversal(t, bypass)
		})
	}
	for _, hazard := range []string{"success timer", "standard completion anchor", "credential teardown", "stop propagation", "shared runtime", "dns stop", "socket runtime", "billet truncation"} {
		for _, bypass := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/counterfactual=%v", hazard, bypass), func(t *testing.T) {
				realRetirementHazard(t, hazard, bypass)
			})
		}
	}
}

type realOperationHost struct {
	t         *testing.T
	prefix    string
	installed []string
	dropins   []string
	masks     []string
	loops     []string
}

func newRealOperationHost(t *testing.T) *realOperationHost {
	t.Helper()
	h := &realOperationHost{t: t, prefix: fmt.Sprintf("billet-effects-%d-%d", os.Getpid(), time.Now().UnixNano())}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 45*time.Second)
		defer cancel()
		for _, name := range h.installed {
			if strings.Contains(name, "@.") {
				continue
			}
			if err := h.ctl(ctx, "disable", "--", name); err != nil {
				t.Error(err)
			}
		}
		// Stop activation sources first, then all services together: an
		// OnSuccess job cannot escape cleanup by rearming a stopped timer.
		for _, name := range h.installed {
			if strings.HasSuffix(name, ".timer") || strings.HasSuffix(name, ".path") || strings.HasSuffix(name, ".socket") {
				if err := h.ctl(ctx, "stop", "--", name); err != nil {
					t.Error(err)
				}
			}
		}
		args := []string{"stop", "--"}
		for _, name := range h.installed {
			if !strings.Contains(name, "@.") {
				args = append(args, name)
			}
		}
		if len(args) > 2 {
			if err := h.ctl(ctx, args...); err != nil {
				t.Error(err)
			}
		}

		for _, path := range h.dropins {
			if err := os.RemoveAll(path); err != nil {
				t.Error(err)
			}
		}
		for _, path := range h.masks {
			if err := os.Remove(path); err != nil {
				t.Error(err)
			}
		}
		for _, name := range h.installed {
			if err := os.Remove(filepath.Join("/run/systemd/system", name)); err != nil && !os.IsNotExist(err) {
				t.Error(err)
			}
		}
		if err := h.ctl(ctx, "daemon-reload"); err != nil {
			t.Error(err)
		}
		for _, device := range h.loops {
			out, err := exec.CommandContext(ctx, "losetup", "--detach", device).CombinedOutput()
			if err != nil {
				t.Errorf("detach %s: %v: %s", device, err, out)
			}
		}
		for _, root := range []string{"/run", "/var/lib", "/var/cache", "/var/log", "/etc"} {
			if err := os.RemoveAll(filepath.Join(root, h.prefix)); err != nil {
				t.Error(err)
			}
		}
	})
	return h
}

func (h *realOperationHost) ctl(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %v: %w: %s", args, err, out)
	}
	return nil
}

func (h *realOperationHost) run(args ...string) {
	h.t.Helper()
	if err := h.ctl(h.t.Context(), args...); err != nil {
		h.t.Fatal(err)
	}
}

func (h *realOperationHost) write(name, body string) {
	h.t.Helper()
	file, err := os.OpenFile(filepath.Join("/run/systemd/system", name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		h.t.Fatal(err)
	}
	h.installed = append(h.installed, name)
	_, writeErr := file.WriteString(body)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		h.t.Fatalf("write %s: %v %v", name, writeErr, closeErr)
	}
}

func (h *realOperationHost) property(unit, property string) string {
	h.t.Helper()
	props, err := NewInspector().UnitProperties(h.t.Context(), unit, property)
	if err != nil || len(props[property]) != 1 {
		h.t.Fatalf("read %s %s: %v %v", unit, property, props, err)
	}
	return first(props, property)
}

func (h *realOperationHost) admit(sequence []Operation, protection OperationProtection) error {
	h.t.Helper()
	i := NewInspector(WithObserver(func(_ context.Context, args []string) {
		if args[0] != "show" && args[0] != "get-property" && !(args[0] == "--json=short" && args[1] == "get-property") {
			h.t.Errorf("admission submitted a job: %v", args)
		}
	}))
	return i.AdmitOperations(h.t.Context(), sequence, protection)
}

// All role infrastructure is rendered from its real template. Only workload
// executables/readiness and fixture paths are adapted; relationship, directory,
// install, kill, sandbox and manager-action settings remain the source's own.
func realRetirementSequence(t *testing.T, role bool, host string) {
	t.Helper()
	h := newRealOperationHost(t)
	server, node := h.prefix+"-server.service", h.prefix+"-node.service"
	backupTimer, upgradeTimer := h.prefix+"-backup.timer", h.prefix+"-upgrade.timer"
	network, dns := h.prefix+"-network.service", h.prefix+"-dnsmasq@br0.service"
	ledgerPath := "/run/" + h.prefix + "/ledger"
	ledgerUnit := "run-" + strings.ReplaceAll(h.prefix, "-", `\x2d`) + "-ledger.mount"
	retained := host == "retained"
	for _, root := range []string{"/etc/", "/var/lib/", "/run/"} {
		for _, suffix := range []string{"", "/node", "/server", "/ledger"} {
			if err := os.MkdirAll(root+h.prefix+suffix, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	names := []string{"billet-server.service", "billet-node.service", "billet-backup.service", "billet-upgrade.service", "billet-backup.timer", "billet-upgrade.timer"}
	if role && retained {
		names = append(names, "billet-network.service", "billet-dnsmasq@.service")
	}
	for _, name := range names {
		if !retained && (name == "billet-node.service" || strings.HasSuffix(name, ".timer")) {
			if strings.HasSuffix(name, ".timer") && host == "server-only masked" {
				mapped := strings.Replace(name, "billet-", h.prefix+"-", 1)
				mask := filepath.Join("/etc/systemd/system", mapped)
				if err := os.Symlink("/dev/null", mask); err != nil {
					t.Fatal(err)
				}
				h.installed = append(h.installed, mapped)
				h.masks = append(h.masks, mask)
			}
			continue
		}
		path := filepath.Join("..", "..", "deploy", name)
		var body []byte
		var err error
		if role {
			path = filepath.Join("..", "..", "ansible_collections", "junioryono", "billet", "roles", "host", "templates", name+".j2")
			body, err = exec.CommandContext(t.Context(), "python3", "-c", `
import sys
from jinja2 import Environment, StrictUndefined
with open(sys.argv[1], encoding="utf-8") as source:
    template = Environment(undefined=StrictUndefined).from_string(source.read())
print(template.render(
    billet_firecracker_enabled=sys.argv[4] == "true",
    billet_config={},
    billet_ledger_volume_id="fixture-volume",
    billet_ledger_mount_unit_name=sys.argv[2],
    billet_candidate_server_state_dir=sys.argv[3],
    billet_service_user="root",
    billet_service_group="root",
    billet_server_environment_path="/etc/billet/server.env",
))
`, path, "FIXTURE_LEDGER_UNIT", "FIXTURE_LEDGER_PATH", fmt.Sprint(retained)).CombinedOutput()
		} else {
			body, err = os.ReadFile(path)
		}
		if err != nil {
			t.Fatalf("read/render %s (python3 Jinja2 required): %v: %s", path, err, body)
		}
		text := strings.NewReplacer("billet-dnsmasq/", h.prefix+"/dnsmasq/", "billet-", h.prefix+"-", "billet/", h.prefix+"/", "/etc/billet", "/etc/"+h.prefix).Replace(string(body))
		text = strings.NewReplacer("FIXTURE_LEDGER_UNIT", ledgerUnit, "FIXTURE_LEDGER_PATH", ledgerPath).Replace(text)
		var lines []string
		for _, line := range strings.Split(text, "\n") {
			switch {
			case strings.HasPrefix(line, "ExecStart="):
				line = "ExecStart=/usr/bin/true"
				if name == "billet-server.service" || name == "billet-node.service" || name == "billet-dnsmasq@.service" {
					line = "ExecStart=/bin/sleep infinity"
				}
			case strings.HasPrefix(line, "ExecReload="):
				line = "ExecReload=/usr/bin/true"
			case line == "Type=notify":
				line = "Type=exec"
			case strings.HasPrefix(line, "User="), strings.HasPrefix(line, "Group="):
				line = strings.SplitN(line, "=", 2)[0] + "=root"
			case strings.HasPrefix(line, "OnCalendar="), strings.HasPrefix(line, "OnBootSec="), strings.HasPrefix(line, "OnUnitActiveSec="):
				line = "OnActiveSec=1d"
			}
			lines = append(lines, line)
		}
		h.write(strings.Replace(name, "billet-", h.prefix+"-", 1), strings.Join(lines, "\n"))
	}
	if role {
		realRetirementLedger(t, h, ledgerUnit, ledgerPath)
	}
	h.run("daemon-reload")
	if role && retained {
		h.run("start", "--", network, dns)
		// The instance is loaded from the rendered template, not a second file.
		h.installed = append(h.installed, dns)
	}
	h.run("enable", "--", server)
	h.run("start", "--", server)
	ledgerInvocation := ""
	if role {
		ledgerInvocation = h.property(ledgerUnit, "InvocationID")
		if h.property(ledgerUnit, "Type") != "ext4" || !strings.HasPrefix(h.property(ledgerUnit, "What"), "/dev/loop") {
			t.Fatal("real ledger template is not mounted from the loop block device")
		}
		device := strings.TrimSuffix(operationPathUnit(h.property(ledgerUnit, "What")), ".mount") + ".device"
		if !slices.Equal(strings.Fields(h.property(ledgerUnit, "StopPropagatedFrom")), []string{device}) ||
			!slices.Contains(strings.Fields(h.property(device, "PropagatesStopTo")), ledgerUnit) {
			t.Fatal("real ledger template lacks its exact implicit device stop-propagation pair")
		}
	}
	if retained {
		h.run("enable", "--", backupTimer, upgradeTimer)
		h.run("start", "--", node, backupTimer, upgradeTimer)
	}
	protection := OperationProtection{
		Units: []string{server, node, h.prefix + "-backup.service", h.prefix + "-upgrade.service", backupTimer, upgradeTimer},
		Paths: []string{"/var/lib/" + h.prefix + "/retired", "/var/lib/" + h.prefix + "/authority.lock", "/var/lib/" + h.prefix + "/upgrades"},
		UnitPaths: map[string][]string{
			server: {"/var/lib/" + h.prefix + "/server", ledgerPath},
			node:   {"/var/lib/" + h.prefix + "/node", "/run/" + h.prefix + "/locks", "/run/" + h.prefix + "/registration"},
		},
	}
	var networkInvocations []string
	if retained {
		protection.RetainedPathUnits = []string{node}
		protection.ArchivedInputRoots = []string{"/var/lib/" + h.prefix + "/server"}
	}
	if role && retained {
		protection.Units = append(protection.Units, network, dns)
		protection.RequiredActive = []string{network, dns}
		for _, unit := range protection.RequiredActive {
			networkInvocations = append(networkInvocations, h.property(unit, "InvocationID"))
		}
	}
	var sequence []Operation
	for _, unit := range []string{upgradeTimer, backupTimer, server} {
		sequence = append(sequence, Operation{Verb: "stop", Unit: unit}, Operation{Verb: "disable", Unit: unit})
	}
	if retained {
		for _, verb := range []string{"enable", "stop", "start"} {
			sequence = append(sequence, Operation{Verb: verb, Unit: node})
		}
	}
	original := ""
	if retained {
		original = h.property(node, "InvocationID")
	}
	if retained {
		// Keep the real implicit chain: neither rendered nor packaged controls
		// disable DefaultDependencies to evade the distribution's handlers.
		if !slices.Contains(strings.Fields(h.property(node, "Requires")), "sysinit.target") ||
			!slices.Contains(strings.Fields(h.property(node, "After")), "sysinit.target") ||
			!slices.Contains(strings.Fields(h.property("sysinit.target", "Wants")), "local-fs.target") ||
			h.property("local-fs.target", "ActiveState") != "active" ||
			h.property("local-fs.target", "OnFailure") != "emergency.target" ||
			h.property("local-fs.target", "OnFailureJobMode") != "replace-irreversibly" {
			t.Fatal("retained control lacks the standard active dependency chain")
		}
	}
	protection.QuietUnits = []string{server, h.prefix + "-backup.service", h.prefix + "-upgrade.service"}
	protection.QuietExceptions = []string{backupTimer, upgradeTimer}
	var submitted []string
	c := NewConverger(NewInspector(WithObserver(func(_ context.Context, args []string) {
		if args[0] != "show" {
			submitted = append(submitted, strings.Join(args, " "))
		}
	})))
	var performed []Operation
	for n, op := range sequence {
		if err := h.admit(sequence[n:], protection); err != nil {
			t.Fatalf("remaining sequence after %v: %v", performed, err)
		}
		if err := h.admit([]Operation{op}, protection); err != nil {
			t.Fatalf("immediate admission %v: %v", op, err)
		}
		// Drive the same helpers retirement calls. Positive timer absence is
		// a successful no-op; no failed command is ever counted as performed.
		before := len(submitted)
		var err error
		switch op.Verb {
		case "stop":
			var result StopResult
			result, err = c.StopAndProve(t.Context(), op.Unit)
			if err == nil && result.Gone != Yes {
				t.Fatalf("stop did not prove disappearance: %+v", result)
			}
			if host == "server-only absent" && strings.HasSuffix(op.Unit, ".timer") && result.How != "not-found" {
				t.Fatalf("absent timer did not return positive absence: %+v", result)
			}
		case "disable":
			err = c.Disable(t.Context(), op.Unit)
		case "enable":
			err = c.Enable(t.Context(), op.Unit)
		case "start":
			_, err = c.StartAndProve(t.Context(), op.Unit)
		}
		if err != nil {
			t.Fatal(err)
		}
		if host == "server-only absent" && strings.HasSuffix(op.Unit, ".timer") && len(submitted) != before {
			t.Fatalf("positive absence submitted a command: %v", submitted[before:])
		}
		if op.Verb == "stop" {
			protection.QuietExceptions = slices.DeleteFunc(protection.QuietExceptions, func(unit string) bool { return unit == op.Unit })
		}
		performed = append(performed, op)
		if op.Verb == "stop" && op.Unit == server {
			if err := NewInspector().ProveUnitProcessesGone(t.Context(), server); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := NewInspector().AdmitQuietActivation(t.Context(), protection.QuietUnits, nil); err != nil {
		t.Fatalf("completed sequence left activation sources armed: %v", err)
	}
	if !slices.Equal(performed, sequence) || (retained && len(performed) != 9) {
		t.Fatalf("incomplete operation sequence: %v", performed)
	}
	for _, unit := range []string{server, backupTimer, upgradeTimer} {
		if h.property(unit, "ActiveState") != "inactive" {
			t.Fatalf("%s did not remain stopped", unit)
		}
		if state := h.property(unit, "UnitFileState"); state != "disabled" && state != "masked" && state != "" {
			t.Fatalf("%s enablement=%s", unit, state)
		}
	}
	if retained {
		if h.property(node, "ActiveState") != "active" || h.property(node, "UnitFileState") != "enabled" || h.property(node, "InvocationID") == original {
			t.Fatal("node did not complete an enabled new invocation")
		}
	} else if h.property(node, "LoadState") != "not-found" {
		t.Fatal("server-only retirement created a node")
	}
	if role && (h.property(ledgerUnit, "ActiveState") != "active" || h.property(ledgerUnit, "InvocationID") != ledgerInvocation) {
		t.Fatal("dedicated ledger mount changed across retirement")
	}
	for n, unit := range protection.RequiredActive {
		if h.property(unit, "ActiveState") != "active" || h.property(unit, "InvocationID") != networkInvocations[n] {
			t.Fatalf("guest network changed across the handoff: %s", unit)
		}
	}
}

func realRetirementHazard(t *testing.T, hazard string, bypass bool) {
	t.Helper()
	h := newRealOperationHost(t)
	server, node := h.prefix+"-server.service", h.prefix+"-node.service"
	timer, backup := h.prefix+"-backup.timer", h.prefix+"-backup.service"
	dns, socket := h.prefix+"-dnsmasq@br0.service", h.prefix+"-helper.socket"
	runtimeName := h.prefix + "/registration"
	record := filepath.Join("/run", runtimeName, "current")
	socketRuntime := h.prefix + "/socket-record"
	socketRecord := filepath.Join("/run", socketRuntime, "current")
	backupEffect := filepath.Join(t.TempDir(), "backup-started")
	unitExtra, serviceExtra := "", ""
	switch hazard {
	case "success timer":
		unitExtra = "OnSuccess=" + timer + "\n"
	case "standard completion anchor":
		unitExtra = "OnSuccess=multi-user.target\nOnSuccessJobMode=replace\n"
	case "stop propagation":
		unitExtra = "PropagatesStopTo=" + node + "\n"
	case "shared runtime":
		serviceExtra = "RuntimeDirectory=" + runtimeName + "\n"
	case "dns stop":
		unitExtra = "PropagatesStopTo=" + dns + "\n"
	case "socket runtime":
		unitExtra = "PropagatesStopTo=" + socket + "\n"
	}
	install := ""
	if hazard == "standard completion anchor" {
		install = "[Install]\nWantedBy=multi-user.target\n"
	}
	h.write(server, "[Unit]\nDefaultDependencies=no\n"+unitExtra+"[Service]\nType=exec\nExecStart=/bin/sleep infinity\n"+serviceExtra+install)
	h.write(node, "[Unit]\nDefaultDependencies=no\n[Service]\nType=exec\nExecStart=/bin/sleep infinity\nRuntimeDirectory="+runtimeName+"\n")
	h.write(timer, "[Unit]\nDefaultDependencies=no\n[Timer]\nOnActiveSec=1ms\nUnit="+backup+"\n")
	h.write(backup, "[Unit]\nDefaultDependencies=no\n[Service]\nType=oneshot\nExecStart=/usr/bin/touch "+backupEffect+"\n")
	h.write(dns, "[Unit]\nDefaultDependencies=no\n[Service]\nType=exec\nExecStart=/bin/sleep infinity\n")
	h.write(socket, "[Unit]\nDefaultDependencies=no\n[Socket]\nListenStream=/run/"+h.prefix+"/test.sock\nService="+h.prefix+"-socket-destination.service\nRuntimeDirectory="+socketRuntime+"\n")
	h.write(h.prefix+"-socket-destination.service", "[Service]\nType=oneshot\nExecStart=/usr/bin/true\n")
	h.run("daemon-reload")
	h.run("start", "--", node, server, dns, socket)
	if hazard == "standard completion anchor" {
		h.run("enable", "--", server)
		if h.property("multi-user.target", "ActiveState") != "active" || h.property(server, "UnitFileState") != "enabled" ||
			!slices.Contains(strings.Fields(h.property("multi-user.target", "Wants")), server) {
			t.Fatal("completion counterfactual lacks active anchor and enabled controller dependency")
		}
	}
	if hazard == "credential teardown" {
		dir := filepath.Join("/run/credentials", server)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(dir); err != nil {
				t.Error(err)
			}
		})
		record = filepath.Join(dir, "node.crt")
	}
	for _, path := range []string{record, socketRecord} {
		if err := os.WriteFile(path, []byte("original registration\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if hazard == "billet truncation" {
		dir := filepath.Join("/run/systemd/system", server+".d")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		h.dropins = append(h.dropins, dir)
		if err := os.WriteFile(filepath.Join(dir, "stdio.conf"), []byte("[Service]\nExecStop=/usr/bin/true\nStandardOutput=truncate:"+record+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		h.run("daemon-reload")
	}
	nodeInvocation, dnsInvocation := h.property(node, "InvocationID"), h.property(dns, "InvocationID")
	serverInvocation := h.property(server, "InvocationID")
	protection := OperationProtection{
		Units:     []string{server, node, timer, backup, dns},
		UnitPaths: map[string][]string{node: {filepath.Dir(record), filepath.Dir(socketRecord)}},
	}
	if !bypass {
		err := h.admit([]Operation{{Verb: "stop", Unit: server}}, protection)
		if err == nil || !strings.Contains(err.Error(), "operation-") {
			t.Fatalf("preventive %s refusal missing: %v", hazard, err)
		}
		if hazard == "standard completion anchor" && !strings.Contains(err.Error(), "operation-edge-outside-set: "+server+" OnSuccess=multi-user.target") {
			t.Fatalf("completion anchor refused for unrelated reason: %v", err)
		}
		if hazard == "credential teardown" && (!strings.Contains(err.Error(), "operation-directory-overlap") || !strings.Contains(err.Error(), "CredentialDirectory=/run/credentials/"+server)) {
			t.Fatalf("credential teardown refused for unrelated reason: %v", err)
		}
		if hazard == "billet truncation" && !strings.Contains(err.Error(), "operation-setup-unsupported: "+server+" StandardOutput") {
			t.Fatalf("truncation was refused for an unrelated reason: %v", err)
		}
		if h.property(server, "ActiveState") != "active" || h.property(node, "InvocationID") != nodeInvocation ||
			h.property(dns, "InvocationID") != dnsInvocation || h.property(socket, "ActiveState") != "active" {
			t.Fatal("preventive admission changed a protected service")
		}
		for _, path := range []string{record, socketRecord} {
			if body, err := os.ReadFile(path); err != nil || string(body) != "original registration\n" {
				t.Fatalf("preventive admission changed %s: %q %v", path, body, err)
			}
		}
		if _, err := os.Lstat(backupEffect); !os.IsNotExist(err) {
			t.Fatalf("preventive admission activated backup: %v", err)
		}
		return
	}
	// A separate fixture performs the exact hazardous stop without admission.
	h.run("stop", "--", server)
	deadline := time.Now().Add(5 * time.Second)
	for {
		occurred := false
		switch hazard {
		case "billet truncation":
			body, err := os.ReadFile(record)
			occurred = err == nil && len(body) == 0 && h.property(node, "ActiveState") == "active"
		case "standard completion anchor":
			occurred = h.property(server, "ActiveState") == "active" && h.property(server, "InvocationID") != serverInvocation
		case "credential teardown":
			_, err := os.Lstat(record)
			occurred = os.IsNotExist(err) && h.property(node, "ActiveState") == "active"
		case "success timer":
			_, err := os.Stat(backupEffect)
			occurred = err == nil
		case "stop propagation":
			occurred = h.property(node, "ActiveState") == "inactive"
		case "shared runtime":
			_, err := os.Lstat(record)
			occurred = os.IsNotExist(err) && h.property(node, "ActiveState") == "active"
		case "dns stop":
			occurred = h.property(dns, "ActiveState") == "inactive" && h.property(node, "ActiveState") == "active"
		case "socket runtime":
			_, err := os.Lstat(socketRecord)
			occurred = os.IsNotExist(err) && h.property(socket, "ActiveState") == "inactive" && h.property(node, "ActiveState") == "active"
		}
		if occurred {
			t.Logf("counterfactual collateral measured: %s", hazard)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("counterfactual %s did not produce its claimed collateral effect", hazard)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// PathChanged watches IN_MOVE_SELF in v255 src/core/path.c. The fixture maps
// /var/lib/billet/server and billet-backup.service into its disposable namespace.
func realRetirementArchivePath(t *testing.T, bypass bool) {
	t.Helper()
	h := newRealOperationHost(t)
	identity := filepath.Join("/var/lib", h.prefix, "server")
	archive := filepath.Join("/var/lib", h.prefix, "archived-server")
	mustCreate := func(path string) {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustCreate(identity)
	before, err := os.Stat(identity)
	if err != nil {
		t.Fatal(err)
	}
	backup, watcher := h.prefix+"-backup.service", h.prefix+"-backup.path"
	marker := filepath.Join("/var/lib", h.prefix, "backup-started")
	h.write(backup, "[Service]\nType=oneshot\nExecStart=/usr/bin/touch "+marker+"\nRemainAfterExit=yes\n")
	destination := backup
	h.write(watcher, "[Path]\nPathChanged="+identity+"\nUnit="+destination+"\n")
	h.run("daemon-reload")
	h.run("start", "--", watcher)
	if h.property(watcher, "ActiveState") != "active" || h.property(backup, "ActiveState") != "inactive" ||
		!slices.Contains(strings.Fields(h.property(destination, "TriggeredBy")), watcher) {
		t.Fatal("path watcher is not armed against the quiet backup")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("backup ran before the rename: %v", err)
	}
	if !bypass {
		err := NewInspector().AdmitQuietActivation(t.Context(), []string{backup}, nil)
		if err == nil || !strings.Contains(err.Error(), "operation-edge-outside-set") {
			t.Fatalf("active path source admitted: %v", err)
		}
		after, err := os.Stat(identity)
		if err != nil || !os.SameFile(before, after) {
			t.Fatalf("preventive refusal touched identity: %v", err)
		}
		if _, err := os.Stat(archive); !os.IsNotExist(err) {
			t.Fatalf("preventive refusal archived identity: %v", err)
		}
		return
	}
	if err := os.Rename(identity, archive); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			t.Log("counterfactual identity rename activated backup through PathChanged")
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("identity rename did not activate backup through the armed path")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Removing closed-set admission would submit the controller stop here.
func realRetirementOutsideHelper(t *testing.T) {
	t.Helper()
	h := newRealOperationHost(t)
	server, helper := h.prefix+"-server.service", h.prefix+"-external.service"
	h.write(server, "[Unit]\nPropagatesStopTo="+helper+"\n[Service]\nType=exec\nExecStart=/bin/sleep infinity\n")
	h.write(helper, "[Service]\nType=oneshot\nExecStart=/usr/bin/true\nExecStop=/usr/bin/true\nRemainAfterExit=yes\n")
	h.run("daemon-reload")
	h.run("start", "--", server, helper)
	if err := h.admit([]Operation{{Verb: "stop", Unit: server}}, OperationProtection{Units: []string{server}}); err == nil || !strings.Contains(err.Error(), "operation-edge-outside-set: "+server+" PropagatesStopTo="+helper) {
		t.Fatalf("outside helper admitted: %v", err)
	}
	if h.property(server, "ActiveState") != "active" || h.property(helper, "ActiveState") != "active" {
		t.Fatal("refusal performed a stop")
	}
}

// The template and its real block-device graph are the control. A tmpfs stand-in
// misses device binding and blockdev ordering. No workload runs on the device.
func realRetirementLedger(t *testing.T, h *realOperationHost, unit, where string) {
	t.Helper()
	imagePath := filepath.Join(t.TempDir(), "ledger.ext4")
	file, err := os.Create(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	resizeErr := file.Truncate(64 << 20)
	closeErr := file.Close()
	if resizeErr != nil || closeErr != nil {
		t.Fatalf("allocate ledger image: %v %v", resizeErr, closeErr)
	}
	out, err := exec.CommandContext(t.Context(), "losetup", "--find", "--show", imagePath).CombinedOutput()
	if err != nil {
		t.Fatalf("attach ledger loop: %v: %s", err, out)
	}
	device := strings.TrimSpace(string(out))
	if !strings.HasPrefix(device, "/dev/loop") || strings.ContainsAny(device, " \t\n") {
		t.Fatalf("unexpected loop device: %q", device)
	}
	h.loops = append(h.loops, device)
	out, err = exec.CommandContext(t.Context(), "mkfs.ext4", "-F", device).CombinedOutput()
	if err != nil {
		t.Fatalf("format ledger loop: %v: %s", err, out)
	}
	path := filepath.Join("..", "..", "ansible_collections", "junioryono", "billet", "roles", "host", "templates", "billet-ledger.mount.j2")
	body, err := exec.CommandContext(t.Context(), "python3", "-c", `
import sys
from jinja2 import Environment, StrictUndefined
env = Environment(undefined=StrictUndefined)
env.filters["comment"] = lambda text: "# " + text
with open(sys.argv[1], encoding="utf-8") as source:
    template = env.from_string(source.read())
print(template.render(
    ansible_managed="disposable systemd retirement witness",
    billet_ledger_volume_id="fixture-volume",
    billet_ledger_device_path=sys.argv[2],
    billet_candidate_server_state_dir=sys.argv[3],
))
`, path, device, where).CombinedOutput()
	if err != nil {
		t.Fatalf("render real ledger template: %v: %s", err, body)
	}
	h.write(unit, string(body))
}
