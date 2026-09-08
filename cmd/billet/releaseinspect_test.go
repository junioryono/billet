package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/hostupgrade"
	"github.com/junioryono/billet/internal/provenance"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// The fixture stands a Linux host in: a procfs tree under procRoot, a fake
// systemctl answering from files, this test's own image at selfExePath and the
// managed path, and every other seam pointed under one temp directory.
type inspectFixture struct {
	dir, procDir, unitsDir, configPath, stateDir, binPath string
	// opened records every path the report opened as an image; read records
	// every path it read as a public file.
	opened, read []string
}

const (
	inspectBootTime = int64(1_700_000_000)
	inspectPID      = 4242
	// inspectStartTicks puts the process start 30 s after boot.
	inspectStartTicks = int64(3000)
)

func newInspectFixture(t *testing.T) *inspectFixture {
	t.Helper()
	dir := t.TempDir()
	f := &inspectFixture{
		dir: dir, procDir: filepath.Join(dir, "proc"), unitsDir: filepath.Join(dir, "units"),
		configPath: filepath.Join(dir, "billet.yaml"), stateDir: filepath.Join(dir, "state"),
		binPath: filepath.Join(dir, "bin", "billet"),
	}
	for _, d := range []string{f.procDir, f.unitsDir, f.stateDir, filepath.Join(dir, "bin")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prev := struct {
		hostOS, procRoot, selfExe, installed, systemctl, busctl, receipt, root, provenance string
		open                                                                               func(string) (*os.File, error)
		read                                                                               func(string) ([]byte, error)
		after                                                                              func(string)
		samples                                                                            int
	}{hostOS, procRoot, selfExePath, installedBinary, systemctlBinary, busctlBinary, retiredJournalPath, upgradeRoot,
		provenance.Path, openImage, readPublicFile, inspectAfterOpen, inspectSamples}
	t.Cleanup(func() {
		hostOS, procRoot, selfExePath, installedBinary = prev.hostOS, prev.procRoot, prev.selfExe, prev.installed
		systemctlBinary, busctlBinary, retiredJournalPath, upgradeRoot = prev.systemctl, prev.busctl, prev.receipt, prev.root
		provenance.Path, openImage, readPublicFile = prev.provenance, prev.open, prev.read
		inspectAfterOpen, inspectSamples = prev.after, prev.samples
	})
	hostOS = "linux"
	procRoot = f.procDir
	selfExePath, installedBinary = f.binPath, f.binPath
	systemctlBinary = filepath.Join(dir, "bin", "systemctl")
	busctlBinary = filepath.Join(dir, "bin", "busctl")
	retiredJournalPath = filepath.Join(dir, "retired", "journal.json")
	upgradeRoot = filepath.Join(dir, "upgrades")
	provenance.Path = filepath.Join(dir, "installed.json")
	inspectAfterOpen = nil
	inspectAfterConfig, inspectBeforeClose, inspectBetweenClosingChecks = nil, nil, nil
	t.Cleanup(func() { inspectAfterConfig, inspectBeforeClose, inspectBetweenClosingChecks = nil, nil, nil })
	openImage = func(path string) (*os.File, error) {
		f.opened = append(f.opened, path)
		return os.Open(path)
	}
	readPublicFile = func(path string) ([]byte, error) {
		f.read = append(f.read, path)
		return os.ReadFile(path)
	}

	writeFile(t, f.binPath, "IMAGE-A\n", 0o755)
	writeFile(t, filepath.Join(f.procDir, "stat"), "cpu  1 2 3 4\nbtime "+strconv.FormatInt(inspectBootTime, 10)+"\nprocesses 9\n", 0o644)
	// Like the real `systemctl show --property=A --property=B -- unit`, the fake
	// prints only the properties requested: a property the inspector stops
	// asking for stops arriving, so a refusal that depends on it fails its test.
	writeFile(t, systemctlBinary, "#!/bin/sh\nunit=\"\"\nnames=\"\"\nfor a in \"$@\"; do case \"$a\" in --property=*) names=\"$names ${a#--property=}\";; --|show) ;; *) unit=$a;; esac; done\n"+
		"for n in $names; do grep \"^$n=\" \"$BILLET_FAKE_UNITS/$unit\"; done\nexit 0\n", 0o755)
	// busctl --json=short get-property org.freedesktop.systemd1 <object> <iface> ExecStart
	writeFile(t, busctlBinary, "#!/bin/sh\ncat \"$BILLET_FAKE_UNITS/$(basename \"$4\").exec\"\n", 0o755)
	t.Setenv("BILLET_FAKE_UNITS", f.unitsDir)

	f.writeConfig(t, f.serverConfig())
	f.unitAbsent(t, "billet-node.service")
	f.unitRunning(t, "billet-server.service", "server", f.configPath, nil)
	f.process(t, []string{f.binPath, "server", "--config", f.configPath}, nil)
	// The config predates the process start, so nothing has changed since.
	f.touchBeforeStart(t, f.configPath)
	return f
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func (f *inspectFixture) serverConfig() string {
	return `
server:
  listen: 127.0.0.1:7717
  state_dir: ` + f.stateDir + `
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: ` + filepath.Join(f.dir, "app.pem") + `
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`
}

func (f *inspectFixture) postgresConfig() string {
	return `
server:
  listen: 127.0.0.1:7717
  identity_dir: ` + f.stateDir + `
  state:
    backend: postgres
    postgres:
      dsn_env: BILLET_PG_DSN
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: ` + filepath.Join(f.dir, "app.pem") + `
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`
}

func (f *inspectFixture) nodeOnlyConfig() string {
	return `
node:
  name: node-a
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: ` + filepath.Join(f.dir, "node-state") + `
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`
}

func (f *inspectFixture) nodeTLSConfig(certPath, keyPath, caPath string) string {
	return `
node:
  name: node-a
  server_addr: 10.0.0.5:7717
  provider: docker
  state_dir: ` + filepath.Join(f.dir, "node-state") + `
  tls:
    cert: ` + certPath + `
    key: ` + keyPath + `
    ca: ` + caPath + `
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`
}

func (f *inspectFixture) writeConfig(t *testing.T, body string) {
	t.Helper()
	writeFile(t, f.configPath, body, 0o644)
}

// touchBeforeStart dates a file before the fixture's process start.
func (f *inspectFixture) touchBeforeStart(t *testing.T, path string) {
	t.Helper()
	before := time.Unix(inspectBootTime, 0).Add(-time.Hour)
	if err := os.Chtimes(path, before, before); err != nil {
		t.Fatal(err)
	}
}

func (f *inspectFixture) unitAbsent(t *testing.T, unit string) {
	t.Helper()
	f.unitAbsentWith(t, unit, "inactive", "dead", 0)
}

// unitAbsentWith is a unit whose fragment systemd no longer finds, with the
// runtime facts it still reports.
func (f *inspectFixture) unitAbsentWith(t *testing.T, unit, active, sub string, pid int) {
	t.Helper()
	writeFile(t, filepath.Join(f.unitsDir, unit), "LoadState=not-found\nUnitFileState=\nActiveState="+active+"\nSubState="+sub+"\nMainPID="+strconv.Itoa(pid)+"\nInvocationID=\nNeedDaemonReload=no\nExecMainStartTimestamp=\nEnvironmentFiles=\nEnvironment=\n", 0o644)
}

// unitRunning renders the properties systemd reports for a unit with the
// shipped ExecStart shape; envFiles is the rendered EnvironmentFiles value.
func (f *inspectFixture) unitRunning(t *testing.T, unit, role, configPath string, envFiles []string) {
	t.Helper()
	f.unitWith(t, unit, fmt.Sprintf("%s %s --config %s", f.binPath, role, configPath), envFiles, "", inspectPID, "active", "running")
}

// unitWith renders a unit both ways systemd shows it: the properties of
// `systemctl show` (argv joined by spaces, one EnvironmentFiles line per file,
// as systemd 255 prints) and the loaded ExecStart records the bus property
// holds (here derived by splitting argv on spaces; a test that needs other
// boundaries overrides them with unitExec).
func (f *inspectFixture) unitWith(t *testing.T, unit, argv string, envFiles []string, environment string, pid int, active, sub string) {
	t.Helper()
	words := strings.Fields(argv)
	body := "LoadState=loaded\nUnitFileState=enabled\nActiveState=" + active + "\nSubState=" + sub + "\nMainPID=" + strconv.Itoa(pid) + "\n" +
		"InvocationID=0123456789abcdef0123456789abcdef\nNeedDaemonReload=no\nExecMainStartTimestamp=Tue 2023-11-14 22:14:00 UTC\n" +
		"RootDirectory=\nRootImage=\nBindPaths=\nBindReadOnlyPaths=\nMountImages=\nExtensionImages=\nExtensionDirectories=\nTemporaryFileSystem=\n" +
		"ExecStart={ path=" + words[0] + " ; argv[]=" + argv + " ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }\n"
	if len(envFiles) == 0 {
		body += "EnvironmentFiles=\n"
	}
	for _, e := range envFiles {
		body += "EnvironmentFiles=" + e + " (ignore_errors=yes)\n"
	}
	body += "Environment=" + environment + "\n"
	writeFile(t, filepath.Join(f.unitsDir, unit), body, 0o644)
	f.unitExec(t, unit, [][]string{words})
}

// unitExec sets the loaded ExecStart records busctl answers for a unit: each
// record's path is its argv[0].
func (f *inspectFixture) unitExec(t *testing.T, unit string, records [][]string) {
	t.Helper()
	data := make([]any, 0, len(records))
	for _, argv := range records {
		data = append(data, []any{argv[0], argv, false, 0, 0, 0, 0, 0, 0, 0})
	}
	body, err := json.Marshal(map[string]any{"type": "a(sasbttttuii)", "data": data})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.unitsDir, busLabel(unit)+".exec"), string(body), 0o644)
}

// unitProperty rewrites one rendered property of the server unit.
func (f *inspectFixture) unitProperty(t *testing.T, name, value string) {
	t.Helper()
	path := filepath.Join(f.unitsDir, "billet-server.service")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if strings.HasPrefix(line, name+"=") {
			line = name + "=" + value
		}
		out = append(out, line)
	}
	writeFile(t, path, strings.Join(out, "\n")+"\n", 0o644)
}

// process lays down a pid's exe, stat, cmdline and environ.
func (f *inspectFixture) process(t *testing.T, cmdline, environ []string) {
	t.Helper()
	pid, startTicks := inspectPID, inspectStartTicks
	image := "IMAGE-A\n"
	dir := filepath.Join(f.procDir, strconv.Itoa(pid))
	// exe is a symlink to the image, as procfs's magic link is: an inspector
	// that resolved it and opened the target by name would be visible in the
	// paths it opened, and would hash a replacement rather than the running
	// bytes.
	target := filepath.Join(dir, "image")
	writeFile(t, target, image, 0o755)
	_ = os.Remove(filepath.Join(dir, "exe"))
	if err := os.Symlink(target, filepath.Join(dir, "exe")); err != nil {
		t.Fatal(err)
	}
	f.processStart(t, pid, startTicks)
	writeFile(t, filepath.Join(dir, "cmdline"), strings.Join(cmdline, "\x00")+"\x00", 0o644)
	writeFile(t, filepath.Join(dir, "environ"), strings.Join(environ, "\x00")+"\x00", 0o644)
	f.processRoot(t, "/")
}

// processRoot sets what /proc/<pid>/root resolves to.
func (f *inspectFixture) processRoot(t *testing.T, target string) {
	t.Helper()
	link := filepath.Join(f.procDir, strconv.Itoa(inspectPID), "root")
	_ = os.Remove(link)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func (f *inspectFixture) processStart(t *testing.T, pid int, startTicks int64) {
	t.Helper()
	f.processStat(t, pid, startTicks, "S")
}

// processState rewrites the fixture process's stat with the given state field.
func (f *inspectFixture) processState(t *testing.T, procState string) {
	t.Helper()
	f.processStat(t, inspectPID, inspectStartTicks, procState)
}

func (f *inspectFixture) processStat(t *testing.T, pid int, startTicks int64, procState string) {
	t.Helper()
	fields := make([]string, 50)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = procState
	fields[19] = strconv.FormatInt(startTicks, 10)
	body := strconv.Itoa(pid) + " (billet (server)) " + strings.Join(fields, " ") + "\n"
	writeFile(t, filepath.Join(f.procDir, strconv.Itoa(pid), "stat"), body, 0o644)
}

func (f *inspectFixture) report(t *testing.T) inspectReport {
	t.Helper()
	return inspectHostRelease(t.Context(), f.configPath)
}

func shaOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func mustKnown(t *testing.T, name string, m maybe) any {
	t.Helper()
	if !m.known {
		t.Fatalf("%s is unknown: %s", name, m.why)
	}
	return m.value
}

func mustUnknown(t *testing.T, name string, m maybe, fragment string) {
	t.Helper()
	if m.known {
		t.Fatalf("%s is known (%v), want unknown mentioning %q", name, m.value, fragment)
	}
	if !strings.Contains(m.why, fragment) {
		t.Errorf("%s is unknown for %q, want a reason mentioning %q", name, m.why, fragment)
	}
}

// THE IMAGE IS THE OPENED DESCRIPTOR'S BYTES AND THE PROCESS IS BOUND BY ITS
// OWN COMMAND LINE: the happy path of a Linux controller on SQLite.
func TestReleaseInspectHashesTheOpenedImageAndBindsTheRunningProcess(t *testing.T) {
	f := newInspectFixture(t)
	id, err := state.DeploymentID(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := wirecert.LoadOrCreateCA(f.stateDir, id)
	if err != nil {
		t.Fatal(err)
	}

	r := f.report(t)
	if !r.Config.Readable {
		t.Fatalf("the config was not readable: %s", r.Config.Error)
	}
	want := shaOf("IMAGE-A\n")
	if got := mustKnown(t, "executable.sha256", r.Executable.SHA256); got != want {
		t.Errorf("executable sha256 = %v, want %s", got, want)
	}
	if !r.Executable.ProcessBound || r.Executable.Image != f.binPath {
		t.Errorf("executable = %+v, want process-bound image %s", r.Executable, f.binPath)
	}
	if same := mustKnown(t, "installed_path_same", r.Executable.InstalledPathSame); same != true {
		t.Errorf("installed_path_same = %v, want true", same)
	}
	if got := mustKnown(t, "installed_config.sha256", r.Installed.SHA256); got != shaOf(f.serverConfig()) {
		t.Errorf("installed_config.sha256 = %v, want the digest of the configuration parsed", got)
	}
	if r.Provenance.Verdict != "none" {
		t.Errorf("provenance verdict = %q, want none with no record", r.Provenance.Verdict)
	}
	svc := r.Services["server"]
	if mustKnown(t, "unit_present", svc.UnitPresent) != true || mustKnown(t, "active_state", svc.ActiveState) != "active" {
		t.Fatalf("server = %+v, want a present active unit", svc)
	}
	if mustKnown(t, "config_binding", r.ConfigBinding) != true {
		t.Error("a unit and a process naming the inspector's config do not bind")
	}
	if mustKnown(t, "shape", svc.Shape) != "supported" {
		t.Errorf("shape = %v (%s), want supported for the packaged form", svc.Shape.value, svc.ShapeReason)
	}
	if got := mustKnown(t, "running_sha256", svc.RunningSHA256); got != want {
		t.Errorf("running_sha256 = %v, want %s", got, want)
	}
	if mustKnown(t, "same_as_executable", svc.SameAsExecutable) != true {
		t.Error("same_as_executable is not true for the same bytes")
	}
	if got := mustKnown(t, "cmdline_config_path", svc.CmdlineConfigPath); got != f.configPath {
		t.Errorf("cmdline_config_path = %v, want %s", got, f.configPath)
	}
	if mustKnown(t, "cmdline_matches_unit", svc.CmdlineMatchesUnit) != true {
		t.Error("cmdline_matches_unit is not true when the process and the unit name one file")
	}
	if mustKnown(t, "config_changed_since_start", svc.ConfigChangedSinceStart) != false {
		t.Error("a config dated before the start reads as changed")
	}
	wantStart := time.Unix(inspectBootTime+inspectStartTicks/100, 0).UTC().Format(time.RFC3339)
	if got := mustKnown(t, "started_at", svc.StartedAt); got != wantStart {
		t.Errorf("started_at = %v, want %s", got, wantStart)
	}
	mustUnknown(t, "loaded_config", svc.LoadedConfig, "nothing on the disk proves")
	if mustKnown(t, "dsn_env", svc.DSNEnv) != nil {
		t.Error("a SQLite controller reports a DSN")
	}
	if mustKnown(t, "environment_file_changed_since_start", svc.EnvironmentFileChangedSinceRun) != nil {
		t.Error("a unit with no environment file reports one changing")
	}
	if files, ok := mustKnown(t, "environment_files", svc.EnvironmentFiles).([]string); !ok || len(files) != 0 {
		t.Errorf("environment_files = %v, want an empty list", svc.EnvironmentFiles.value)
	}
	if mustKnown(t, "node unit_present", r.Services["node"].UnitPresent) != false {
		t.Error("an absent node unit reads as present")
	}
	if mustKnown(t, "has_server", r.Installed.HasServer) != true || mustKnown(t, "has_node", r.Installed.HasNode) != false {
		t.Errorf("installed_config = %+v, want a server-only config", r.Installed)
	}
	if got := mustKnown(t, "deployment_id", r.Host.DeploymentID); got != id {
		t.Errorf("deployment_id = %v, want %s", got, id)
	}
	auth, ok := mustKnown(t, "authority", r.Host.Authority).(inspectAuthority)
	if !ok {
		t.Fatalf("authority = %T, want inspectAuthority", r.Host.Authority.value)
	}
	sum := sha256.Sum256(caDER(t, ca))
	if auth.Current.DERSHA256 != hex.EncodeToString(sum[:]) || auth.RotationInProgress || !auth.Created || auth.Previous != nil {
		t.Errorf("authority = %+v, want the CA's DER fingerprint, created, not rotating", auth)
	}
	if mustKnown(t, "node_trust", r.Host.NodeTrust) != nil || mustKnown(t, "node_name", r.Host.NodeName) != nil {
		t.Error("a server-only host reports node trust or a node name")
	}
	if mustKnown(t, "retirement", r.Host.Retirement) != nil {
		t.Error("a host with no journal reports a retirement")
	}
	if r.Transaction.Root != "absent" || mustKnown(t, "active", r.Transaction.Active) != "none" || mustKnown(t, "lock_held", r.Transaction.LockHeld) != false {
		t.Errorf("transaction = %+v, want an absent root", r.Transaction)
	}
	// THE OPENS ARE BY LINK PATH: the process image through <pid>/exe, never a
	// resolved target.
	var sawProcessExe bool
	for _, p := range f.opened {
		if p == filepath.Join(f.procDir, strconv.Itoa(inspectPID), "exe") {
			sawProcessExe = true
		}
		if p == filepath.Join(f.procDir, strconv.Itoa(inspectPID), "image") {
			t.Errorf("the process image was opened by its resolved target, not the exe link")
		}
	}
	if !sawProcessExe {
		t.Errorf("the process image was not opened by its exe link path; opened %q", f.opened)
	}
}

// caDER is the DER of a CA's certificate, for the fingerprint assertion.
func caDER(t *testing.T, ca *wirecert.CA) []byte {
	t.Helper()
	certs, err := wirecert.ParseCertificates(ca.CertPEM())
	if err != nil || len(certs) != 1 {
		t.Fatalf("the CA's PEM does not hold one certificate: %v", err)
	}
	return certs[0].Raw
}

// A REPLACEMENT AFTER THE OPEN DOES NOT CHANGE THE HASH: the descriptor is the
// image, whatever the path holds by the time it is read.
func TestReleaseInspectHashesTheDescriptorNotThePathAfterReplacement(t *testing.T) {
	f := newInspectFixture(t)
	exe := filepath.Join(f.procDir, strconv.Itoa(inspectPID), "exe")
	inspectAfterOpen = func(path string) {
		if path != exe {
			return
		}
		replacement := exe + ".new"
		writeFile(t, replacement, "IMAGE-B\n", 0o755)
		if err := os.Rename(replacement, exe); err != nil {
			t.Fatal(err)
		}
	}
	r := f.report(t)
	if got := mustKnown(t, "running_sha256", r.Services["server"].RunningSHA256); got != shaOf("IMAGE-A\n") {
		t.Errorf("running_sha256 = %v, want the bytes that were open, not the replacement", got)
	}
}

func TestReleaseInspectProvenanceVerdicts(t *testing.T) {
	for name, tc := range map[string]struct {
		record  string
		verdict string
	}{
		"proved":        {`{"version":"v0.9.3","manifest_digest":"sha256:m","binary_sha256":"` + shaOf("IMAGE-A\n") + `"}`, "proved"},
		"contradiction": {`{"version":"v0.9.3","manifest_digest":"sha256:m","binary_sha256":"` + shaOf("IMAGE-B\n") + `"}`, "contradiction"},
		"unreadable":    {`{"version":"v0.9.3"}`, "unreadable"},
		"garbage":       {`not json`, "unreadable"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newInspectFixture(t)
			writeFile(t, provenance.Path, tc.record, 0o644)
			r := f.report(t)
			if r.Provenance.Verdict != tc.verdict {
				t.Errorf("verdict = %q (%s), want %s", r.Provenance.Verdict, r.Provenance.Reason, tc.verdict)
			}
		})
	}
}

// A PROCESS THAT RESTARTS UNDER THE OBSERVATION IS COULD-NOT-TELL, and one
// disturbed sample is retried.
func TestReleaseInspectBracketsTheProcessRead(t *testing.T) {
	t.Run("every sample disturbed", func(t *testing.T) {
		f := newInspectFixture(t)
		ticks := inspectStartTicks
		exe := filepath.Join(f.procDir, strconv.Itoa(inspectPID), "exe")
		inspectAfterOpen = func(path string) {
			if path != exe {
				return
			}
			ticks++
			f.processStart(t, inspectPID, ticks)
		}
		r := f.report(t)
		mustUnknown(t, "running_sha256", r.Services["server"].RunningSHA256, "restarted during the observation")
		mustUnknown(t, "same_as_executable", r.Services["server"].SameAsExecutable, "restarted")
	})
	t.Run("first sample disturbed only", func(t *testing.T) {
		f := newInspectFixture(t)
		opens := 0
		exe := filepath.Join(f.procDir, strconv.Itoa(inspectPID), "exe")
		inspectAfterOpen = func(path string) {
			if path != exe {
				return
			}
			opens++
			// Disturb the process start on the first sample only.
			if opens == 1 {
				f.processStart(t, inspectPID, inspectStartTicks+7)
			}
		}
		r := f.report(t)
		if got := mustKnown(t, "running_sha256", r.Services["server"].RunningSHA256); got != shaOf("IMAGE-A\n") {
			t.Errorf("running_sha256 = %v after one disturbed sample, want the value", got)
		}
	})
}

func TestReleaseInspectInstalledPathSameIsFalseForAnotherFile(t *testing.T) {
	f := newInspectFixture(t)
	other := filepath.Join(f.dir, "bin", "billet-copy")
	writeFile(t, other, "IMAGE-A\n", 0o755)
	installedBinary = other
	r := f.report(t)
	if mustKnown(t, "installed_path_same", r.Executable.InstalledPathSame) != false {
		t.Error("a different file with the same bytes reads as the same path")
	}
}

// THE SHAPE BILLET SHIPS AND NOTHING ELSE: zero or one environment file with
// the direct ExecStart is supported; a wrapper, an expansion, two files or an
// Environment= directive is could-not-tell.
func TestReleaseInspectUnitShapes(t *testing.T) {
	for name, tc := range map[string]struct {
		argv        string
		envFiles    []string
		environment string
		want        string
	}{
		"packaged":   {"", nil, "", "supported"},
		"role":       {"", []string{"/etc/billet/server.env"}, "", "supported"},
		"wrapper":    {"/usr/bin/env billet server --config CFG", nil, "", "unsupported"},
		"extra arg":  {"BIN server --config CFG --verbose", nil, "", "unsupported"},
		"other role": {"BIN node --config CFG", nil, "", "unsupported"},
		"expansion":  {"BIN server --config $BILLET_CONFIG", nil, "", "unsupported"},
		"two files":  {"", []string{"/etc/billet/a.env", "/etc/billet/b.env"}, "", "unsupported"},
		"directive":  {"", nil, "BILLET_X=1", "unsupported"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newInspectFixture(t)
			argv := tc.argv
			if argv == "" {
				argv = f.binPath + " server --config " + f.configPath
			}
			argv = strings.ReplaceAll(strings.ReplaceAll(argv, "CFG", f.configPath), "BIN", f.binPath)
			f.unitWith(t, "billet-server.service", argv, tc.envFiles, tc.environment, inspectPID, "active", "running")
			r := f.report(t)
			svc := r.Services["server"]
			if got := mustKnown(t, "shape", svc.Shape); got != tc.want {
				t.Errorf("shape = %v (%s), want %s", got, svc.ShapeReason, tc.want)
			}
			if tc.want == "unsupported" {
				mustUnknown(t, "cmdline_matches_unit", svc.CmdlineMatchesUnit, "unsupported")
			}
		})
	}
}

// THE BINDING IS THE PROCESS'S COMMAND LINE, NOT THE UNIT: a unit edited and
// reloaded without a restart names a path the process never read.
func TestReleaseInspectCmdlineBindsTheProcessNotTheUnit(t *testing.T) {
	f := newInspectFixture(t)
	f.unitRunning(t, "billet-server.service", "server", filepath.Join(f.dir, "other.yaml"), nil)
	r := f.report(t)
	svc := r.Services["server"]
	if mustKnown(t, "cmdline_matches_unit", svc.CmdlineMatchesUnit) != false {
		t.Error("a unit naming another config reads as matching the process")
	}
	if got := mustKnown(t, "cmdline_config_path", svc.CmdlineConfigPath); got != f.configPath {
		t.Errorf("cmdline_config_path = %v, want the process's own %s", got, f.configPath)
	}
}

// THE DSN IS COMPARED HERE AND NEVER PRINTED: each state of the comparison.
func TestReleaseInspectDSNStates(t *testing.T) {
	const dsn = "postgres://billet:s3cret@db/billet"
	for name, tc := range map[string]struct {
		environ  []string
		withFile bool
		fileBody string
		want     string
		unknown  string
	}{
		"equal":                   {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=\"" + dsn + "\"\n", "equal", ""},
		"equal unquoted":          {[]string{"BILLET_PG_DSN=" + dsn}, true, "# rendered\nBILLET_PG_DSN=" + dsn + "\n", "equal", ""},
		"differs":                 {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=postgres://other\n", "differs", ""},
		"not in file":             {[]string{"BILLET_PG_DSN=" + dsn}, true, "OTHER=1\n", "not_in_file", ""},
		"absent":                  {[]string{"PATH=/usr/bin"}, true, "BILLET_PG_DSN=x\n", "absent", ""},
		"no file":                 {[]string{"BILLET_PG_DSN=" + dsn}, false, "", "", "no single environment file"},
		"assigned twice":          {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=postgres://first\nBILLET_PG_DSN=" + dsn + "\n", "", "assigned twice"},
		"quote inside":            {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=\"a\"b\"\n", "", "unsupported environment file syntax"},
		"backslash":               {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=a\\\\b\n", "", "unsupported environment file syntax"},
		"bad other line":          {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=" + dsn + "\nexport OTHER=1\n", "", "unsupported environment file syntax"},
		"leading space":           {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN= " + dsn + "\n", "", "unsupported environment file syntax"},
		"trailing space":          {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=" + dsn + " \n", "", "unsupported environment file syntax"},
		"indented name":           {[]string{"BILLET_PG_DSN=" + dsn}, true, "  BILLET_PG_DSN=" + dsn + "\n", "", "unsupported environment file syntax"},
		"cr in comment":           {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=" + dsn + "\n# comment\rBILLET_PG_DSN=postgres://new\n", "", "control character"},
		"crlf":                    {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=" + dsn + "\r\n", "", "control character"},
		"nul":                     {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=" + dsn + "\x00\n", "", "control character"},
		"not utf8":                {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=" + dsn + "\xff\n", "", "not UTF-8"},
		"del":                     {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=" + dsn + "\x7f\n", "", "control character"},
		"noncharacter elsewhere":  {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=" + dsn + "\nOTHER=\uFDD0\n", "", "noncharacter"},
		"noncharacter in the dsn": {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=" + dsn + "\U0001FFFE\n", "", "noncharacter"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newInspectFixture(t)
			f.writeConfig(t, f.postgresConfig())
			envFile := filepath.Join(f.dir, "server.env")
			writeFile(t, envFile, tc.fileBody, 0o640)
			f.touchBeforeStart(t, envFile)
			f.touchBeforeStart(t, f.configPath)
			var files []string
			if tc.withFile {
				files = []string{envFile}
			}
			f.unitRunning(t, "billet-server.service", "server", f.configPath, files)
			f.process(t, []string{f.binPath, "server", "--config", f.configPath}, tc.environ)
			r := f.report(t)
			if !r.Config.Readable {
				t.Fatalf("postgres config unreadable: %s", r.Config.Error)
			}
			d, ok := mustKnown(t, "dsn_env", r.Services["server"].DSNEnv).(inspectDSNEnv)
			if !ok || d.Name != "BILLET_PG_DSN" {
				t.Fatalf("dsn_env = %+v, want the variable's name", r.Services["server"].DSNEnv.value)
			}
			if tc.unknown != "" {
				mustUnknown(t, "matches_file", d.MatchesFile, tc.unknown)
				return
			}
			if got := mustKnown(t, "matches_file", d.MatchesFile); got != tc.want {
				t.Errorf("matches_file = %v, want %s", got, tc.want)
			}
			body, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "s3cret") {
				t.Error("the report carries the DSN's value")
			}
		})
	}
}

// ONE CONFIGURATION BINDS THE REPORT: a unit or a process that names another
// configuration makes config_binding false, so identity read from the
// inspector's config cannot be attributed to a service running another one.
func TestReleaseInspectRefusesToBindAcrossConfigurations(t *testing.T) {
	t.Run("unit names another config", func(t *testing.T) {
		f := newInspectFixture(t)
		other := filepath.Join(f.dir, "other.yaml")
		f.unitRunning(t, "billet-server.service", "server", other, nil)
		f.process(t, []string{f.binPath, "server", "--config", other}, nil)
		r := f.report(t)
		if mustKnown(t, "config_binding", r.ConfigBinding) != false {
			t.Error("a unit and process naming another config bind to the inspector's")
		}
		if mustKnown(t, "cmdline_matches_unit", r.Services["server"].CmdlineMatchesUnit) != true {
			t.Error("the process and its unit agree with each other and should say so")
		}
	})
	t.Run("unit alone names another config", func(t *testing.T) {
		f := newInspectFixture(t)
		f.unitRunning(t, "billet-server.service", "server", filepath.Join(f.dir, "other.yaml"), nil)
		r := f.report(t)
		if mustKnown(t, "config_binding", r.ConfigBinding) != false {
			t.Error("a unit naming another config binds while its process names the inspector's")
		}
	})
	t.Run("process names another config", func(t *testing.T) {
		f := newInspectFixture(t)
		f.process(t, []string{f.binPath, "server", "--config", filepath.Join(f.dir, "other.yaml")}, nil)
		r := f.report(t)
		if mustKnown(t, "config_binding", r.ConfigBinding) != false {
			t.Error("a process naming another config binds to the inspector's")
		}
	})
	t.Run("unsupported shape is unknown", func(t *testing.T) {
		f := newInspectFixture(t)
		f.unitWith(t, "billet-server.service", "/usr/bin/env billet server --config "+f.configPath, nil, "", inspectPID, "active", "running")
		r := f.report(t)
		mustUnknown(t, "config_binding", r.ConfigBinding, "unsupported")
	})
	t.Run("absent unit binds trivially", func(t *testing.T) {
		f := newInspectFixture(t)
		f.unitAbsent(t, "billet-server.service")
		if mustKnown(t, "config_binding", f.report(t).ConfigBinding) != true {
			t.Error("a host with no units does not bind")
		}
	})
}

// THE WHOLE SAMPLE IS BRACKETED: a main pid that systemd no longer reports at
// the end of the sample discards the image, the command line and the
// environment together.
func TestReleaseInspectDiscardsASampleWhenTheMainPIDMoves(t *testing.T) {
	f := newInspectFixture(t)
	exe := filepath.Join(f.procDir, strconv.Itoa(inspectPID), "exe")
	inspectAfterOpen = func(path string) {
		if path != exe {
			return
		}
		// After the image is open, systemd reports a new main pid.
		f.unitWith(t, "billet-server.service", f.binPath+" server --config "+f.configPath, nil, "", inspectPID+1, "active", "running")
	}
	r := f.report(t)
	svc := r.Services["server"]
	mustUnknown(t, "running_sha256", svc.RunningSHA256, "restarted during the observation")
	mustUnknown(t, "cmdline_config_path", svc.CmdlineConfigPath, "restarted")
	mustUnknown(t, "config_binding", r.ConfigBinding, "restarted")
}

// A PROCESS STARTED SOME OTHER WAY IS COULD-NOT-TELL, not read for the first
// --config it carries.
func TestReleaseInspectRefusesAnUnsupportedCommandLine(t *testing.T) {
	f := newInspectFixture(t)
	f.process(t, []string{f.binPath, "server", "--config", f.configPath, "--extra"}, nil)
	r := f.report(t)
	svc := r.Services["server"]
	mustUnknown(t, "cmdline_config_path", svc.CmdlineConfigPath, "command line is not")
	mustUnknown(t, "config_binding", r.ConfigBinding, "command line is not")
	if got := mustKnown(t, "running_sha256", svc.RunningSHA256); got != shaOf("IMAGE-A\n") {
		t.Errorf("the image is still reported: got %v", got)
	}
}

// AN ENVIRONMENT FILE IN A SYNTAX BILLET DOES NOT WRITE IS COULD-NOT-TELL.
func TestReleaseInspectRefusesUnsupportedEnvironmentFileSyntax(t *testing.T) {
	f := newInspectFixture(t)
	f.writeConfig(t, f.postgresConfig())
	envFile := filepath.Join(f.dir, "server.env")
	writeFile(t, envFile, "export BILLET_PG_DSN=postgres://x\n", 0o640)
	f.touchBeforeStart(t, envFile)
	f.touchBeforeStart(t, f.configPath)
	f.unitRunning(t, "billet-server.service", "server", f.configPath, []string{envFile})
	f.process(t, []string{f.binPath, "server", "--config", f.configPath}, []string{"BILLET_PG_DSN=postgres://x"})
	d, ok := mustKnown(t, "dsn_env", f.report(t).Services["server"].DSNEnv).(inspectDSNEnv)
	if !ok {
		t.Fatal("dsn_env is not a DSN report")
	}
	mustUnknown(t, "matches_file", d.MatchesFile, "unsupported environment file syntax")
}

// EVERY EnvironmentFiles PROPERTY LINE IS READ: systemd prints one per file,
// so a reader of the first line alone would compare the DSN against a file
// another file overrides.
func TestReleaseInspectReadsEveryEnvironmentFileLine(t *testing.T) {
	f := newInspectFixture(t)
	f.unitRunning(t, "billet-server.service", "server", f.configPath, []string{"/etc/billet/a.env", "/etc/billet/b.env"})
	svc := f.report(t).Services["server"]
	if mustKnown(t, "shape", svc.Shape) != "unsupported" || !strings.Contains(svc.ShapeReason, "2 EnvironmentFile") {
		t.Errorf("shape = %v (%s), want unsupported for two files on two lines", svc.Shape.value, svc.ShapeReason)
	}
	if files, ok := mustKnown(t, "environment_files", svc.EnvironmentFiles).([]string); !ok || len(files) != 2 {
		t.Errorf("environment_files = %v, want both", svc.EnvironmentFiles.value)
	}
}

// ARGUMENT BOUNDARIES COME FROM THE LOADED RECORDS: `systemctl show` joins argv
// with spaces, so a two-argument `billet "server --config x"` displays like the
// supported four-argument form, and only the bus property tells them apart;
// the rendered view must agree with the loaded one, and a second record, a
// remapping directive and a relative path are each could-not-tell.
func TestReleaseInspectReadsArgumentBoundariesFromTheLoadedUnit(t *testing.T) {
	cases := map[string]struct {
		arrange func(t *testing.T, f *inspectFixture)
		reason  string
	}{
		"two arguments": {func(t *testing.T, f *inspectFixture) {
			t.Helper()
			f.unitExec(t, "billet-server.service", [][]string{{f.binPath, "server --config " + f.configPath}})
		}, "ExecStart is not"},
		"rendered disagrees": {func(t *testing.T, f *inspectFixture) {
			t.Helper()
			f.unitProperty(t, "ExecStart", "{ path=/usr/bin/env ; argv[]=/usr/bin/env billet server --config "+f.configPath+" ; ignore_errors=no }")
		}, "rendered ExecStart does not match"},
		"two records": {func(t *testing.T, f *inspectFixture) {
			t.Helper()
			f.unitExec(t, "billet-server.service", [][]string{{f.binPath, "server", "--config", f.configPath}, {"/bin/true"}})
		}, "2 ExecStart records"},
		"relative config": {func(t *testing.T, f *inspectFixture) {
			t.Helper()
			f.unitExec(t, "billet-server.service", [][]string{{f.binPath, "server", "--config", "billet.yaml"}})
			f.unitProperty(t, "ExecStart", "{ path="+f.binPath+" ; argv[]="+f.binPath+" server --config billet.yaml ; ignore_errors=no }")
		}, "not absolute"},
		"another root": {func(t *testing.T, f *inspectFixture) {
			t.Helper()
			f.unitProperty(t, "RootDirectory", "/srv/B")
		}, "remapping directive (RootDirectory)"},
		"bind paths": {func(t *testing.T, f *inspectFixture) {
			t.Helper()
			f.unitProperty(t, "BindReadOnlyPaths", "/srv/B/etc/billet:/etc/billet")
		}, "remapping directive (BindReadOnlyPaths)"},
		"extension directories": {func(t *testing.T, f *inspectFixture) {
			t.Helper()
			f.unitProperty(t, "ExtensionDirectories", "/var/lib/confexts/B")
		}, "remapping directive (ExtensionDirectories)"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newInspectFixture(t)
			tc.arrange(t, f)
			r := f.report(t)
			svc := r.Services["server"]
			if mustKnown(t, "shape", svc.Shape) != "unsupported" || !strings.Contains(svc.ShapeReason, tc.reason) {
				t.Errorf("shape = %v (%s), want unsupported for %q", svc.Shape.value, svc.ShapeReason, tc.reason)
			}
			mustUnknown(t, "config_binding", r.ConfigBinding, "")
		})
	}
}

// ONE OBSERVATION OF THE CONFIGURATION BINDS THE REPORT: the bytes parsed, the
// digest and the identity the process views are compared with are one read of
// one descriptor, so a file replaced after the parse is a binding that says
// false (the views name another inode than the one read) beside the digest of
// what was parsed, and a file rewritten in place is a binding and a digest that
// say nothing; never A's roles beside B's digest and a true.
func TestReleaseInspectBindsOneConfigurationObservation(t *testing.T) {
	t.Run("replaced by rename after the parse", func(t *testing.T) {
		f := newInspectFixture(t)
		other := f.serverConfig() + "# B\n"
		inspectAfterConfig = func() {
			writeFile(t, f.configPath+".new", other, 0o644)
			if err := os.Rename(f.configPath+".new", f.configPath); err != nil {
				t.Fatal(err)
			}
		}
		r := f.report(t)
		if got := mustKnown(t, "config_binding", r.ConfigBinding); got != false {
			t.Errorf("config_binding = %v, want false when the process's view is the replacement and the parse was the original", got)
		}
		if got := mustKnown(t, "installed_config.sha256", r.Installed.SHA256); got != shaOf(f.serverConfig()) {
			t.Errorf("installed_config.sha256 = %v, want the digest of the bytes parsed, not the replacement's", got)
		}
		if got := mustKnown(t, "has_server", r.Installed.HasServer); got != true {
			t.Errorf("has_server = %v", got)
		}
	})
	rewritten := func(t *testing.T, f *inspectFixture) {
		t.Helper()
		writeFile(t, f.configPath, f.serverConfig()+"# B\n", 0o644)
	}
	for name, arm := range map[string]func(t *testing.T, f *inspectFixture){
		"rewritten in place after the parse": func(t *testing.T, f *inspectFixture) {
			t.Helper()
			inspectAfterConfig = func() { rewritten(t, f) }
		},
		"rewritten in place after the sample": func(t *testing.T, f *inspectFixture) {
			t.Helper()
			inspectBeforeClose = func() { rewritten(t, f) }
		},
		"rewritten in place between the closing checks": func(t *testing.T, f *inspectFixture) {
			t.Helper()
			// The descriptor's own stat came too early to see this; the stat
			// of the name is the evidence, and it is not discarded.
			inspectBetweenClosingChecks = func() { rewritten(t, f) }
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newInspectFixture(t)
			arm(t, f)
			r := f.report(t)
			mustUnknown(t, "config_binding", r.ConfigBinding, "changed while the report")
			mustUnknown(t, "installed_config.sha256", r.Installed.SHA256, "changed while the report")
			// The service copied its digest from the observation, so it goes
			// with it.
			mustUnknown(t, "config_sha256", r.Services["server"].ConfigSHA256, "changed while the report")
			mustUnknown(t, "config_changed_since_start", r.Services["server"].ConfigChangedSinceStart, "changed while the report")
		})
	}
	t.Run("replaced by rename after the sample", func(t *testing.T) {
		// The process's view matched the observation, so the binding was true
		// and the service's digest is the observation's; then the name is
		// given another file. The name no longer holds what the report
		// describes, so the binding is false and both digests are A's.
		f := newInspectFixture(t)
		inspectBeforeClose = func() {
			writeFile(t, f.configPath+".new", f.serverConfig()+"# B\n", 0o644)
			if err := os.Rename(f.configPath+".new", f.configPath); err != nil {
				t.Fatal(err)
			}
		}
		r := f.report(t)
		if got := mustKnown(t, "config_binding", r.ConfigBinding); got != false {
			t.Errorf("config_binding = %v, want false once the name holds another file", got)
		}
		if got := mustKnown(t, "config.same_as_installed", r.Config.SameAsInstalled); got != false {
			t.Errorf("same_as_installed = %v, want false", got)
		}
		if got := mustKnown(t, "installed_config.sha256", r.Installed.SHA256); got != shaOf(f.serverConfig()) {
			t.Errorf("installed_config.sha256 = %v, want the parsed bytes' digest", got)
		}
		if got := mustKnown(t, "config_sha256", r.Services["server"].ConfigSHA256); got != shaOf(f.serverConfig()) {
			t.Errorf("services.server.config_sha256 = %v, want the observation's digest, not the replacement's", got)
		}
	})
	t.Run("withdrawn from a bound node, not only a server", func(t *testing.T) {
		// The fixture has one process, so the node is the bound service here;
		// a withdrawal that only knew the server's name would leave it known.
		f := newInspectFixture(t)
		f.unitAbsent(t, "billet-server.service")
		f.unitRunning(t, "billet-node.service", "node", f.configPath, nil)
		f.process(t, []string{f.binPath, "node", "--config", f.configPath}, nil)
		inspectBeforeClose = func() { rewritten(t, f) }
		r := f.report(t)
		mustUnknown(t, "node config_sha256", r.Services["node"].ConfigSHA256, "changed while the report")
		mustUnknown(t, "node config_changed_since_start", r.Services["node"].ConfigChangedSinceStart, "changed while the report")
	})
	t.Run("a service that hashed another configuration keeps its own evidence", func(t *testing.T) {
		f := newInspectFixture(t)
		other := filepath.Join(f.dir, "other.yaml")
		writeFile(t, other, f.serverConfig()+"# other\n", 0o644)
		f.unitRunning(t, "billet-server.service", "server", other, nil)
		f.process(t, []string{f.binPath, "server", "--config", other}, nil)
		inspectBeforeClose = func() { rewritten(t, f) }
		r := f.report(t)
		if got := mustKnown(t, "config_binding", r.ConfigBinding); got != false {
			t.Errorf("config_binding = %v, want false", got)
		}
		if got := mustKnown(t, "config_sha256", r.Services["server"].ConfigSHA256); got != shaOf(f.serverConfig()+"# other\n") {
			t.Errorf("a service that hashed another configuration lost its own digest: %v", got)
		}
		mustUnknown(t, "installed_config.sha256", r.Installed.SHA256, "changed while the report")
	})
	t.Run("rewritten to the same length with a later mtime", func(t *testing.T) {
		// Equal size, so only the modification time can tell; the closing
		// checks compare both.
		for name, arm := range map[string]func(touch func()){
			"between the closing checks": func(touch func()) { inspectBetweenClosingChecks = touch },
			"after the sample":           func(touch func()) { inspectBeforeClose = touch },
		} {
			t.Run(name, func(t *testing.T) {
				f := newInspectFixture(t)
				body := f.serverConfig()
				same := body[:len(body)-2] + "X\n"
				arm(func() {
					writeFile(t, f.configPath, same, 0o644)
					later := time.Now().Add(2 * time.Hour)
					if err := os.Chtimes(f.configPath, later, later); err != nil {
						t.Fatal(err)
					}
				})
				r := f.report(t)
				mustUnknown(t, "config_binding", r.ConfigBinding, "changed while the report")
				mustUnknown(t, "installed_config.sha256", r.Installed.SHA256, "changed while the report")
				mustUnknown(t, "config_sha256", r.Services["server"].ConfigSHA256, "changed while the report")
			})
		}
	})
	t.Run("rewritten in place and then replaced after the sample", func(t *testing.T) {
		// The name check sees another file and says false; only the
		// descriptor's own stat can see that the file the service was bound
		// to was rewritten before it was replaced, so the service's copied
		// digest goes too.
		f := newInspectFixture(t)
		inspectBeforeClose = func() {
			writeFile(t, f.configPath, f.serverConfig()+"# rewritten\n", 0o644)
			writeFile(t, f.configPath+".new", f.serverConfig()+"# B\n", 0o644)
			if err := os.Rename(f.configPath+".new", f.configPath); err != nil {
				t.Fatal(err)
			}
		}
		r := f.report(t)
		if got := mustKnown(t, "config_binding", r.ConfigBinding); got != false {
			t.Errorf("config_binding = %v, want false", got)
		}
		if got := mustKnown(t, "config.same_as_installed", r.Config.SameAsInstalled); got != false {
			t.Errorf("same_as_installed = %v, want false", got)
		}
		mustUnknown(t, "config_sha256", r.Services["server"].ConfigSHA256, "changed while the report")
		mustUnknown(t, "installed_config.sha256", r.Installed.SHA256, "changed while the report")
	})
	t.Run("replaced by rename on a stopped host", func(t *testing.T) {
		// No process, so no view is ever compared; the closing check of the
		// name is the only thing that can notice the replacement.
		f := newInspectFixture(t)
		f.unitWith(t, "billet-server.service", f.binPath+" server --config "+f.configPath, nil, "", 0, "inactive", "dead")
		inspectAfterConfig = func() {
			writeFile(t, f.configPath+".new", f.serverConfig()+"# B\n", 0o644)
			if err := os.Rename(f.configPath+".new", f.configPath); err != nil {
				t.Fatal(err)
			}
		}
		r := f.report(t)
		if got := mustKnown(t, "config_binding", r.ConfigBinding); got != false {
			t.Errorf("config_binding = %v, want false on a stopped host whose configuration was replaced after the parse", got)
		}
		if got := mustKnown(t, "installed_config.sha256", r.Installed.SHA256); got != shaOf(f.serverConfig()) {
			t.Errorf("installed_config.sha256 = %v, want the parsed bytes' digest", got)
		}
	})
	t.Run("untouched", func(t *testing.T) {
		f := newInspectFixture(t)
		r := f.report(t)
		if got := mustKnown(t, "config.same_as_installed", r.Config.SameAsInstalled); got != true {
			t.Errorf("same_as_installed = %v, want true", got)
		}
	})
}

// THE CONFIRMING READ COVERS EVERYTHING THE SHAPE RESTS ON: a reload under the
// sample that adds a bind mount, or moves an argument boundary while the
// rendered string stays the same, changes what the loaded unit is without
// changing the strings the first identity compared, so the structured records
// and the remapping directives are part of the identity.
func TestReleaseInspectDiscardsASampleWhenTheLoadedShapeMoves(t *testing.T) {
	for name, disturb := range map[string]func(t *testing.T, f *inspectFixture){
		"a bind mount appears": func(t *testing.T, f *inspectFixture) {
			t.Helper()
			f.unitProperty(t, "BindReadOnlyPaths", "/srv/B/etc/billet:/etc/billet")
		},
		"an argument boundary moves under the same rendering": func(t *testing.T, f *inspectFixture) {
			t.Helper()
			f.unitExec(t, "billet-server.service", [][]string{{f.binPath, "server --config " + f.configPath}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newInspectFixture(t)
			exe := filepath.Join(f.procDir, strconv.Itoa(inspectPID), "exe")
			inspectAfterOpen = func(path string) {
				if path == exe {
					disturb(t, f)
				}
			}
			r := f.report(t)
			mustUnknown(t, "running_sha256", r.Services["server"].RunningSHA256, "unit changed under it")
			mustUnknown(t, "config_binding", r.ConfigBinding, "unit changed under it")
		})
	}
}

// THE PENDING-RELOAD FLAG IS REPORTED, NOT JUDGED: on systemd 255 it answers
// yes when this unit's own file changed since load AND for every unit while
// the manager's `unit_file_state_outdated` is set (any enable or disable that
// changed something, cleared only by a reload; measured at 464 of 464 units on
// the reference controller after a snap refresh), so it cannot say which, and
// the loaded records are what systemd runs either way. A yes leaves the shape supported and the binding
// true; an answer that is neither yes nor no is could-not-tell.
func TestReleaseInspectReportsThePendingReloadAsAFact(t *testing.T) {
	for answer, want := range map[string]any{"yes": true, "no": false} {
		t.Run(answer, func(t *testing.T) {
			f := newInspectFixture(t)
			f.unitProperty(t, "NeedDaemonReload", answer)
			r := f.report(t)
			svc := r.Services["server"]
			if got := mustKnown(t, "need_daemon_reload", svc.NeedDaemonReload); got != want {
				t.Errorf("need_daemon_reload = %v, want %v", got, want)
			}
			if got := mustKnown(t, "shape", svc.Shape); got != "supported" {
				t.Errorf("shape = %v (%s), want supported whatever NeedDaemonReload says", got, svc.ShapeReason)
			}
			if got := mustKnown(t, "config_binding", r.ConfigBinding); got != true {
				t.Errorf("config_binding = %v, want true", got)
			}
		})
	}
	f := newInspectFixture(t)
	f.unitProperty(t, "NeedDaemonReload", "")
	mustUnknown(t, "need_daemon_reload", f.report(t).Services["server"].NeedDaemonReload, "NeedDaemonReload=")
}

// ENABLEMENT IS READ LITERALLY: a runtime enablement is one, an answer outside
// systemd's set is could-not-tell, never false, because a consumer that reads
// false as "positively disabled" must not be handed an empty answer.
func TestReleaseInspectReadsEnablementLiterally(t *testing.T) {
	for state, want := range map[string]any{"enabled": true, "enabled-runtime": true, "static": false, "disabled": false, "masked": false} {
		t.Run(state, func(t *testing.T) {
			f := newInspectFixture(t)
			f.unitProperty(t, "UnitFileState", state)
			svc := f.report(t).Services["server"]
			if got := mustKnown(t, "enabled", svc.Enabled); got != want {
				t.Errorf("enabled = %v for %q, want %v", got, state, want)
			}
			if got := mustKnown(t, "unit_file_state", svc.UnitFileState); got != state {
				t.Errorf("unit_file_state = %v, want %q", got, state)
			}
		})
	}
	f := newInspectFixture(t)
	f.unitProperty(t, "UnitFileState", "")
	svc := f.report(t).Services["server"]
	mustUnknown(t, "enabled", svc.Enabled, "UnitFileState=")
	mustUnknown(t, "unit_file_state", svc.UnitFileState, "UnitFileState=")
}

// PRESENCE IS TYPED: a consumer that must tell an absent configuration (a
// retired host) from one it could not read or parse gets the word, not a
// boolean that folds the three together.
func TestReleaseInspectTypesTheConfigurationsPresence(t *testing.T) {
	f := newInspectFixture(t)
	if got := f.report(t).Config.Presence; got != "present" {
		t.Errorf("presence = %q, want present", got)
	}
	if err := os.Remove(f.configPath); err != nil {
		t.Fatal(err)
	}
	r := f.report(t)
	if r.Config.Presence != "absent" || r.Config.Readable {
		t.Errorf("config = %+v, want absent", r.Config)
	}
	mustUnknown(t, "installed_config.sha256", r.Installed.SHA256, "no such file")
	// A directory at the path is a failed read, and a failed read is never
	// absence; a permission error would be the same case, but root reads
	// through one, so the directory is the deterministic form.
	if err := os.Mkdir(f.configPath, 0o755); err != nil {
		t.Fatal(err)
	}
	r = f.report(t)
	if r.Config.Presence != "unreadable" || r.Config.Readable {
		t.Errorf("config = %+v, want unreadable for a directory at the path", r.Config)
	}
	mustUnknown(t, "installed_config.sha256", r.Installed.SHA256, "is a directory")
}

// A PROCESS WHOSE --config IS RELATIVE CANNOT BE COMPARED: it is relative to a
// working directory this inspector does not share.
func TestReleaseInspectRefusesARelativeProcessConfigPath(t *testing.T) {
	f := newInspectFixture(t)
	f.process(t, []string{f.binPath, "server", "--config", "billet.yaml"}, nil)
	r := f.report(t)
	mustUnknown(t, "cmdline_config_path", r.Services["server"].CmdlineConfigPath, "relative")
	mustUnknown(t, "config_binding", r.ConfigBinding, "relative")
}

// A PROCESS UNDER ANOTHER ROOT CANNOT BE COMPARED: its absolute --config names
// a file under that root, which the inspector opens through /proc/<pid>/root
// and, when it is not there, cannot compare.
func TestReleaseInspectRefusesAProcessUnderAnotherRoot(t *testing.T) {
	f := newInspectFixture(t)
	f.processRoot(t, "/srv/B")
	r := f.report(t)
	mustUnknown(t, "config_sha256", r.Services["server"].ConfigSHA256, "could not be opened")
	mustUnknown(t, "config_binding", r.ConfigBinding, "could not be opened")
}

// A PROCESS WHOSE VIEW OF THE PATH IS ANOTHER FILE DOES NOT BIND, whatever the
// loaded unit says: a bind mount removed from the unit and reloaded without a
// restart leaves the running process in the namespace it started in, where the
// same absolute path is a different file, so the file is compared by identity
// through the process's own root and not by the path's spelling.
func TestReleaseInspectRefusesAProcessWhoseViewIsAnotherFile(t *testing.T) {
	f := newInspectFixture(t)
	view := filepath.Join(f.dir, "view")
	// Byte-identical, so only identity tells the two apart.
	writeFile(t, filepath.Join(view, f.configPath), f.serverConfig(), 0o644)
	f.processRoot(t, view)
	r := f.report(t)
	if got := mustKnown(t, "config_binding", r.ConfigBinding); got != false {
		t.Errorf("config_binding = %v, want false for a process whose view of the path is another file", got)
	}
	svc := r.Services["server"]
	if got := mustKnown(t, "cmdline_config_path", svc.CmdlineConfigPath); got != f.configPath {
		t.Errorf("cmdline_config_path = %v", got)
	}
	mustUnknown(t, "config_sha256", svc.ConfigSHA256, "is not the inspector's file")
}

// A SYMLINK INSIDE THE PROCESS'S VIEW RESOLVES UNDER THE PROCESS'S ROOT, never
// the inspector's: a plain open of "<root>/<path>" would follow an absolute
// symlink back into the inspector's namespace and open the inspector's own
// file, so a config that is a symlink into a directory the service mounts
// differently would compare equal to a file the service never reads.
func TestReleaseInspectResolvesTheProcessViewUnderItsRoot(t *testing.T) {
	t.Run("an absolute symlink does not escape the root", func(t *testing.T) {
		f := newInspectFixture(t)
		view := filepath.Join(f.dir, "view")
		// Under the view, the config path is a symlink to the inspector's real
		// file by its absolute name; under the process's root that name does
		// not exist, so the view cannot be opened and nothing binds.
		if err := os.MkdirAll(filepath.Dir(filepath.Join(view, f.configPath)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(f.configPath, filepath.Join(view, f.configPath)); err != nil {
			t.Fatal(err)
		}
		f.processRoot(t, view)
		r := f.report(t)
		mustUnknown(t, "config_binding", r.ConfigBinding, "could not be opened")
		mustUnknown(t, "config_sha256", r.Services["server"].ConfigSHA256, "could not be opened")
	})
	t.Run("an absolute symlink resolves inside the root", func(t *testing.T) {
		f := newInspectFixture(t)
		view := filepath.Join(f.dir, "view")
		// The symlink names /other/billet.yaml, which exists under the view as
		// a byte-identical copy: the process reads that copy, not the
		// inspector's file, so only identity tells them apart.
		writeFile(t, filepath.Join(view, "other", "billet.yaml"), f.serverConfig(), 0o644)
		if err := os.MkdirAll(filepath.Dir(filepath.Join(view, f.configPath)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/other/billet.yaml", filepath.Join(view, f.configPath)); err != nil {
			t.Fatal(err)
		}
		f.processRoot(t, view)
		r := f.report(t)
		if got := mustKnown(t, "config_binding", r.ConfigBinding); got != false {
			t.Errorf("config_binding = %v, want false for a symlink resolving to another file under the process's root", got)
		}
		mustUnknown(t, "config_sha256", r.Services["server"].ConfigSHA256, "is not the inspector's file")
	})
}

// A ZOMBIE KEEPS ITS START TIME, so equal ticks before and after the sample
// would not prove the process lived through it; a zombie or dead process is
// could-not-tell.
func TestReleaseInspectRefusesAZombie(t *testing.T) {
	f := newInspectFixture(t)
	f.processState(t, "Z")
	svc := f.report(t).Services["server"]
	mustUnknown(t, "running_sha256", svc.RunningSHA256, "zombie")
	mustUnknown(t, "config_binding", f.report(t).ConfigBinding, "zombie")
}

// AN IMAGE WRITTEN WHILE IT IS HASHED IS COULD-NOT-TELL: the bracket around
// every hash is the descriptor's own metadata before and after the read.
func TestReleaseInspectRefusesAnImageModifiedDuringTheHash(t *testing.T) {
	f := newInspectFixture(t)
	inspectAfterOpen = func(path string) {
		if path != f.binPath {
			return
		}
		// An in-place write: the same inode, new bytes, a new size.
		if err := os.WriteFile(f.binPath, []byte("IMAGE-A-touched\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r := f.report(t)
	mustUnknown(t, "executable.sha256", r.Executable.SHA256, "changed while it was being read")
}

// THE CONFIGURATION'S CONTENT IS READ ONCE. A service whose view is the
// observation's file publishes the observation's digest; a second content read
// of the pathname would be a window in which a replacement's bytes are paired
// with the parsed configuration and a true binding. The seam counts the
// seam-covered content reads (observeConfig and hashImage), not every open:
// the process-view open, which reads no bytes, is deliberately outside it.
func TestReleaseInspectOpensTheConfigurationOnce(t *testing.T) {
	f := newInspectFixture(t)
	opens := 0
	inspectAfterOpen = func(path string) {
		if path == f.configPath {
			opens++
		}
	}
	r := f.report(t)
	if opens != 1 {
		t.Errorf("the configuration's content was read %d times, want once", opens)
	}
	if got := mustKnown(t, "config_sha256", r.Services["server"].ConfigSHA256); got != shaOf(f.serverConfig()) {
		t.Errorf("services.server.config_sha256 = %v, want the observation's digest", got)
	}
}

// A FILE WRITTEN WHILE IT IS READ IS COULD-NOT-TELL: a digest paired with
// either stat would describe a file that never existed whole. The
// configuration is read once, as the observation, so that is where the bracket
// is exercised; every dependent section then says the configuration could not
// be read.
func TestReleaseInspectRefusesAFileModifiedDuringTheHash(t *testing.T) {
	f := newInspectFixture(t)
	inspectAfterOpen = func(path string) {
		if path != f.configPath {
			return
		}
		// An in-place write: the same inode, new bytes, a new mtime.
		if err := os.WriteFile(f.configPath, []byte(f.serverConfig()+"# touched\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := f.report(t)
	if r.Config.Readable || r.Config.Presence != "unreadable" || !strings.Contains(r.Config.Error, "changed while it was being read") {
		t.Errorf("config = %+v, want unreadable for a file written while it was read", r.Config)
	}
	mustUnknown(t, "installed_config.sha256", r.Installed.SHA256, "changed while it was being read")
	mustUnknown(t, "config_sha256", r.Services["server"].ConfigSHA256, "could not be read")
	mustUnknown(t, "config_binding", r.ConfigBinding, "could not be read")
}

// AN ENVIRONMENT FILE'S WHOLE NAME IS KEPT, " (" included: a parser cutting at
// the first " (" would name a sibling file and compare the DSN against it.
func TestReleaseInspectKeepsTheWholeEnvironmentFileName(t *testing.T) {
	f := newInspectFixture(t)
	f.writeConfig(t, f.postgresConfig())
	envFile := filepath.Join(f.dir, "server (prod.env")
	writeFile(t, envFile, "BILLET_PG_DSN=postgres://real\n", 0o640)
	writeFile(t, filepath.Join(f.dir, "server"), "BILLET_PG_DSN=postgres://sibling\n", 0o640)
	f.touchBeforeStart(t, envFile)
	f.touchBeforeStart(t, f.configPath)
	f.unitRunning(t, "billet-server.service", "server", f.configPath, []string{envFile})
	f.process(t, []string{f.binPath, "server", "--config", f.configPath}, []string{"BILLET_PG_DSN=postgres://real"})
	svc := f.report(t).Services["server"]
	if mustKnown(t, "shape", svc.Shape) != "supported" {
		t.Fatalf("shape = %v (%s)", svc.Shape.value, svc.ShapeReason)
	}
	d, ok := mustKnown(t, "dsn_env", svc.DSNEnv).(inspectDSNEnv)
	if !ok || mustKnown(t, "matches_file", d.MatchesFile) != "equal" {
		t.Errorf("dsn_env = %+v, want equal against the file whose name holds \" (\"", svc.DSNEnv.value)
	}
	f.unitProperty(t, "EnvironmentFiles", envFile+" (something else)")
	svc = f.report(t).Services["server"]
	if mustKnown(t, "shape", svc.Shape) != "unsupported" {
		t.Error("an EnvironmentFiles line in another form was read")
	}
}

// THE SAMPLE IS BOUND TO ONE INCARNATION: an InvocationID that changes across
// the sample discards it even when the pid reads the same.
func TestReleaseInspectDiscardsASampleWhenTheInvocationMoves(t *testing.T) {
	f := newInspectFixture(t)
	exe := filepath.Join(f.procDir, strconv.Itoa(inspectPID), "exe")
	inspectAfterOpen = func(path string) {
		if path != exe {
			return
		}
		body, err := os.ReadFile(filepath.Join(f.unitsDir, "billet-server.service"))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(f.unitsDir, "billet-server.service"), strings.Replace(string(body), "InvocationID=0123", "InvocationID=ffff", 1), 0o644)
	}
	svc := f.report(t).Services["server"]
	mustUnknown(t, "running_sha256", svc.RunningSHA256, "restarted during the observation")
}

// A UNIT WHOSE FRAGMENT IS GONE KEEPS ITS RUNTIME FACTS: absence of the file is
// not absence of the service, and only a positively stopped one binds
// trivially.
func TestReleaseInspectAMissingFragmentDoesNotEraseARunningService(t *testing.T) {
	f := newInspectFixture(t)
	f.unitAbsentWith(t, "billet-server.service", "active", "running", inspectPID)
	r := f.report(t)
	svc := r.Services["server"]
	if mustKnown(t, "unit_present", svc.UnitPresent) != false || mustKnown(t, "active_state", svc.ActiveState) != "active" || mustKnown(t, "main_pid", svc.MainPID) != inspectPID {
		t.Errorf("server = %+v, want present false with the runtime facts kept", svc)
	}
	mustUnknown(t, "config_binding", r.ConfigBinding, "fragment is gone")
}

// AN UNREADABLE CONFIG LEAVES EVERY DEPENDENT SECTION UNKNOWN, applicability
// included: whether a DSN applies is not knowable without the config.
func TestReleaseInspectAnUnreadableConfigLeavesDependentSectionsUnknown(t *testing.T) {
	f := newInspectFixture(t)
	f.writeConfig(t, "server: [not a mapping\n")
	r := f.report(t)
	if r.Config.Readable || r.Config.Presence != "malformed" {
		t.Fatalf("a broken config read as %+v, want malformed and unreadable", r.Config)
	}
	mustUnknown(t, "dsn_env", r.Services["server"].DSNEnv, "configuration could not be read")
	mustUnknown(t, "has_server", r.Installed.HasServer, "load")
	mustUnknown(t, "deployment_id", r.Host.DeploymentID, "configuration could not be read")
	if got := mustKnown(t, "executable.sha256", r.Executable.SHA256); got != shaOf("IMAGE-A\n") {
		t.Error("the executable section depends on the config and should not")
	}
}

// THE IDENTITY IS PEEKED AND NEVER MINTED.
func TestReleaseInspectPeeksAndNeverMintsTheIdentity(t *testing.T) {
	f := newInspectFixture(t)
	r := f.report(t)
	mustUnknown(t, "deployment_id", r.Host.DeploymentID, "not minted")
	if _, err := os.Stat(filepath.Join(f.stateDir, "deployment-id")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the inspector minted an identity: %v", err)
	}
	if mustKnown(t, "authority", r.Host.Authority) != nil {
		t.Error("a state directory with no CA reports an authority")
	}
	entries, err := os.ReadDir(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the inspector created %d entries in the state directory", len(entries))
	}
}

// THE JOURNAL IS READ FROM ITS FIXED PATH, config or no config, so a host whose
// config lost its server: block still reports its retirement; a journal that
// names no phase is could-not-tell.
func TestReleaseInspectReadsTheRetirementJournalFromTheFixedPath(t *testing.T) {
	f := newInspectFixture(t)
	f.writeConfig(t, f.nodeOnlyConfig())
	f.unitAbsent(t, "billet-server.service")
	writeFile(t, retiredJournalPath, `{"phase":"done","deployment_id":"dep-x","archive":"/var/lib/billet/retired/identity-2026-09-06"}`, 0o600)
	r := f.report(t)
	journal, ok := mustKnown(t, "retirement", r.Host.Retirement).(map[string]any)
	if !ok || journal["phase"] != "done" {
		t.Errorf("retirement = %v, want the journal's fields", r.Host.Retirement.value)
	}
	writeFile(t, retiredJournalPath, `{"deployment_id":"dep-x"}`, 0o600)
	mustUnknown(t, "retirement", f.report(t).Host.Retirement, "names no phase")
	if mustKnown(t, "deployment_id", r.Host.DeploymentID) != nil || mustKnown(t, "node_name", r.Host.NodeName) != "node-a" {
		t.Errorf("host = %+v, want a node-only host", r.Host)
	}
}

// A DORMANT PACKAGED SERVER UNIT ON A NODE-ONLY HOST IS NOT A CONTROLLER: the
// installed configuration says which roles the host has.
func TestReleaseInspectADormantUnitIsNotAController(t *testing.T) {
	f := newInspectFixture(t)
	f.writeConfig(t, f.nodeOnlyConfig())
	f.unitWith(t, "billet-server.service", f.binPath+" server --config "+f.configPath, nil, "", 0, "inactive", "dead")
	f.unitRunning(t, "billet-node.service", "node", f.configPath, nil)
	f.process(t, []string{f.binPath, "node", "--config", f.configPath}, nil)
	r := f.report(t)
	if mustKnown(t, "has_server", r.Installed.HasServer) != false || mustKnown(t, "has_node", r.Installed.HasNode) != true {
		t.Errorf("installed_config = %+v, want node only", r.Installed)
	}
	svc := r.Services["server"]
	if mustKnown(t, "unit_present", svc.UnitPresent) != true || mustKnown(t, "active_state", svc.ActiveState) != "inactive" || mustKnown(t, "main_pid", svc.MainPID) != nil {
		t.Errorf("server = %+v, want a present dormant unit", svc)
	}
	if mustKnown(t, "running_sha256", svc.RunningSHA256) != nil {
		t.Error("a unit with no process reports an image")
	}
}

func TestReleaseInspectReportsANodesTrustStoreWithoutItsKey(t *testing.T) {
	f := newInspectFixture(t)
	ca, err := wirecert.LoadOrCreateCA(f.stateDir, "dep-1234")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := ca.IssueNode("node-a")
	if err != nil {
		t.Fatal(err)
	}
	cert, key, caFile := filepath.Join(f.dir, "node.crt"), filepath.Join(f.dir, "node.key"), filepath.Join(f.dir, "ca.crt")
	writeFile(t, cert, string(bundle.CertPEM), 0o644)
	writeFile(t, key, string(bundle.KeyPEM), 0o600)
	// THE KEY IS UNREADABLE, so a read of it whose result the report uses
	// would surface as an error; a discarded read would not, which is what the
	// exact-reads assertion below and the source-level call list are for.
	if err := os.Chmod(key, 0); err != nil {
		t.Fatal(err)
	}
	writeFile(t, caFile, string(bundle.CAPEM)+string(bundle.CAPEM), 0o644)
	f.writeConfig(t, f.nodeTLSConfig(cert, key, caFile))
	f.unitAbsent(t, "billet-server.service")
	r := f.report(t)
	if !r.Config.Readable {
		t.Fatalf("node tls config unreadable: %s", r.Config.Error)
	}
	trust, ok := mustKnown(t, "node_trust", r.Host.NodeTrust).(inspectNodeTrust)
	if !ok {
		t.Fatalf("node_trust = %T", r.Host.NodeTrust.value)
	}
	if len(trust.CAs) != 2 || !strings.Contains(trust.Leaf.Subject, "node-a") {
		t.Errorf("node_trust = %+v, want the leaf and both CA copies", trust)
	}
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "PRIVATE KEY") {
		t.Error("the report carries key material")
	}
	if strings.Contains(string(body), "node.key") {
		t.Errorf("the report mentions the node's key, so something read it:\n%s", body)
	}
	// EXACTLY THE PUBLIC FILES, THROUGH THE SEAM: the journal (absent), the
	// leaf and the CA file, and never the key.
	want := []string{retiredJournalPath, cert, caFile}
	sort.Strings(want)
	got := append([]string(nil), f.read...)
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("public reads = %q, want exactly %q", got, want)
	}
}

// EVERY CLAIM SHAPE IS CLASSIFIED BY LSTAT, a dangling symlink included.
func TestReleaseInspectClassifiesEveryClaimShape(t *testing.T) {
	cases := map[string]struct {
		arrange func(t *testing.T, root string)
		active  string
		guard   bool
	}{
		"none": {func(t *testing.T, _ string) { t.Helper() }, "none", false},
		"host-upgrade dangling": {func(t *testing.T, root string) {
			t.Helper()
			if err := os.Symlink(filepath.Join(root, "upgrade-gone"), filepath.Join(root, "active")); err != nil {
				t.Fatal(err)
			}
		}, "host-upgrade", false},
		"legacy-role": {func(t *testing.T, root string) {
			t.Helper()
			writeFile(t, filepath.Join(root, "active"), root+"/20260906T000000000000000\n", 0o600)
		}, "legacy-role", false},
		"converge-guard": {func(t *testing.T, root string) {
			t.Helper()
			writeFile(t, filepath.Join(root, "recovery-1", "billet.candidate"), "IMAGE-C\n", 0o755)
			writeFile(t, filepath.Join(root, "active", "guard.json"), `{"holder":"repo/1/1/converge/abc","claimed_at":"2026-09-06T00:00:00Z","hostname":"cp-1","release_executable":"`+filepath.Join(root, "recovery-1", "billet.candidate")+`","release_executable_sha256":"`+shaOf("IMAGE-STALE\n")+`"}`, 0o600)
			writeFile(t, filepath.Join(root, "active", "recovery"), "", 0o600)
		}, "converge-guard", true},
		"unpublished-guard": {func(t *testing.T, root string) {
			t.Helper()
			writeFile(t, filepath.Join(root, "active", "guard.json.tmp"), "{", 0o600)
		}, "unpublished-guard", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newInspectFixture(t)
			if err := os.MkdirAll(upgradeRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			tc.arrange(t, upgradeRoot)
			r := f.report(t)
			if r.Transaction.Root != "present" {
				t.Fatalf("root = %s, want present", r.Transaction.Root)
			}
			if got := mustKnown(t, "active", r.Transaction.Active); got != tc.active {
				t.Errorf("active = %v, want %s", got, tc.active)
			}
			if tc.guard {
				g, ok := mustKnown(t, "converge_guard", r.Transaction.ConvergeGuard).(inspectGuard)
				if !ok || g.Holder != "repo/1/1/converge/abc" || mustKnown(t, "recovery_pointer", g.RecoveryPointer) != true {
					t.Errorf("converge_guard = %+v, want the holder and its pointer", r.Transaction.ConvergeGuard.value)
				}
				// The fixture's guard records an executable whose digest is stale:
				// verified false, and nothing ran it.
				if g.ReleaseExecutable == "" || mustKnown(t, "release_executable_verified", g.ReleaseExecutableVerified) != false {
					t.Errorf("converge_guard = %+v, want the recorded executable found not to verify", g)
				}
			}
			if name == "host-upgrade dangling" {
				mustUnknown(t, "journal", r.Transaction.Journal, "read the journal")
			}
		})
	}
}

// THE LOCK IS TRIED THROUGH A READ-ONLY DESCRIPTOR AND RELEASED AT ONCE.
func TestReleaseInspectSeesAHeldTransactionLock(t *testing.T) {
	f := newInspectFixture(t)
	if err := os.MkdirAll(upgradeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(upgradeRoot, txLockName)
	writeFile(t, lockPath, "", 0o600)
	if mustKnown(t, "lock_held", f.report(t).Transaction.LockHeld) != false {
		t.Error("an unheld lock reads as held")
	}
	holder, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	r := f.report(t)
	if mustKnown(t, "lock_held", r.Transaction.LockHeld) != true {
		t.Error("a held lock reads as free")
	}
	if p, ok := mustKnown(t, "preparation", r.Transaction.Preparation).(inspectPreparation); !ok || mustKnown(t, "transaction_lock", p.TransactionLock) != true {
		t.Errorf("preparation = %+v, want the lock file present", r.Transaction.Preparation.value)
	}
}

// ON DARWIN THE HASH IS PATH EVIDENCE AND EVERY PROCESS-BOUND FIELD IS UNKNOWN,
// asserted by value.
func TestReleaseInspectOnDarwinIsPathEvidence(t *testing.T) {
	f := newInspectFixture(t)
	hostOS = "darwin"
	r := f.report(t)
	if r.Executable.ProcessBound {
		t.Error("darwin reports a process-bound image")
	}
	if r.Executable.Image != installedBinary {
		t.Errorf("darwin image = %s, want the managed path %s", r.Executable.Image, installedBinary)
	}
	mustUnknown(t, "unit_present", r.Services["server"].UnitPresent, "launchd")
	mustUnknown(t, "need_daemon_reload", r.Services["server"].NeedDaemonReload, "launchd")
	mustUnknown(t, "running_sha256", r.Services["server"].RunningSHA256, "launchd")
	mustUnknown(t, "retirement", r.Host.Retirement, "darwin")
	mustUnknown(t, "config_binding", r.ConfigBinding, "launchd")
	if r.Host.OS != "darwin" {
		t.Errorf("host.os = %s", r.Host.OS)
	}
}

// THE FIELD SET IS THE CONTRACT: a consumer parses these names, so a rename
// or a removal under schema 1 fails here.
func TestReleaseInspectJSONFieldSet(t *testing.T) {
	t.Run("sqlite controller", func(t *testing.T) {
		f := newInspectFixture(t)
		if _, err := state.DeploymentID(f.stateDir); err != nil {
			t.Fatal(err)
		}
		if _, err := wirecert.LoadOrCreateCA(f.stateDir, "dep-1234"); err != nil {
			t.Fatal(err)
		}
		assertFieldSet(t, f.report(t), inspectFieldSet)
	})
	t.Run("postgres controller under a guard", func(t *testing.T) {
		f := newInspectFixture(t)
		f.writeConfig(t, f.postgresConfig())
		envFile := filepath.Join(f.dir, "server.env")
		writeFile(t, envFile, "BILLET_PG_DSN=postgres://x\n", 0o640)
		f.touchBeforeStart(t, envFile)
		f.touchBeforeStart(t, f.configPath)
		f.unitRunning(t, "billet-server.service", "server", f.configPath, []string{envFile})
		f.process(t, []string{f.binPath, "server", "--config", f.configPath}, []string{"BILLET_PG_DSN=postgres://x"})
		writeFile(t, provenance.Path, `{"version":"v0.9.3","manifest_digest":"sha256:m","binary_sha256":"`+shaOf("IMAGE-A\n")+`"}`, 0o644)
		if err := os.MkdirAll(upgradeRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(upgradeRoot, "active", "guard.json"), `{"holder":"h","claimed_at":"t","hostname":"cp-1","release_executable":"`+f.binPath+`","release_executable_sha256":"`+shaOf("IMAGE-A\n")+`"}`, 0o600)
		writeFile(t, filepath.Join(upgradeRoot, txLockName), "", 0o600)
		assertFieldSet(t, f.report(t), inspectFieldSetGuarded)
	})
	t.Run("node with a bundle and a journal", func(t *testing.T) {
		f := newInspectFixture(t)
		ca, err := wirecert.LoadOrCreateCA(f.stateDir, "dep-1234")
		if err != nil {
			t.Fatal(err)
		}
		bundle, err := ca.IssueNode("node-a")
		if err != nil {
			t.Fatal(err)
		}
		cert, key, caFile := filepath.Join(f.dir, "node.crt"), filepath.Join(f.dir, "node.key"), filepath.Join(f.dir, "ca.crt")
		writeFile(t, cert, string(bundle.CertPEM), 0o644)
		writeFile(t, key, string(bundle.KeyPEM), 0o600)
		writeFile(t, caFile, string(bundle.CAPEM), 0o644)
		f.writeConfig(t, f.nodeTLSConfig(cert, key, caFile))
		f.unitAbsent(t, "billet-server.service")
		f.unitRunning(t, "billet-node.service", "node", f.configPath, nil)
		f.process(t, []string{f.binPath, "node", "--config", f.configPath}, nil)
		writeFile(t, retiredJournalPath, `{"phase":"done"}`, 0o600)
		claim := filepath.Join(upgradeRoot, "upgrade-1")
		if err := os.MkdirAll(claim, 0o700); err != nil {
			t.Fatal(err)
		}
		journal := &hostupgrade.Journal{Dir: claim, FromVersion: "v0.9.3", ToVersion: "v0.9.4", TargetDigest: "sha256:m", Step: hostupgrade.StepStaged, StartedAt: "2026-09-06T00:00:00Z", Failure: "the probe never answered"}
		if err := journal.Write(); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(claim, filepath.Join(upgradeRoot, "active")); err != nil {
			t.Fatal(err)
		}
		assertFieldSet(t, f.report(t), inspectFieldSetNode)
	})
}

func assertFieldSet(t *testing.T, r inspectReport, want string) {
	t.Helper()
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	var paths []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		m, ok := v.(map[string]any)
		if !ok {
			paths = append(paths, prefix)
			return
		}
		for k, child := range m {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			walk(key, child)
		}
	}
	walk("", doc)
	sort.Strings(paths)
	if got := strings.Join(paths, "\n"); got != strings.Join(strings.Fields(want), "\n") {
		t.Errorf("the report's field set changed:\n%s", got)
	}
	if doc["schema"] != float64(inspectSchema) {
		t.Errorf("schema = %v, want %d", doc["schema"], inspectSchema)
	}
}

// inspectFieldSet is the sorted key set of a happy-path Linux controller
// report; an unknown field appears as <field>.unknown.
const inspectFieldSet = `
config.path
config.presence
config.readable
config.same_as_installed
config_binding
executable.image
executable.installed_path
executable.installed_path_same
executable.is_release
executable.process_bound
executable.sha256
executable.version
host.authority.created
host.authority.current.der_sha256
host.authority.current.not_after
host.authority.current.pem
host.authority.current.subject
host.authority.previous
host.authority.rotation_in_progress
host.deployment_id
host.node_name
host.node_trust
host.os
host.retirement
installed_config.controllers
installed_config.has_node
installed_config.has_server
installed_config.ledger_backend
installed_config.path
installed_config.sha256
provenance.reason
provenance.verdict
schema
services.node.active_state
services.node.cmdline_config_path
services.node.cmdline_matches_unit
services.node.config_changed_since_start
services.node.config_sha256
services.node.dsn_env
services.node.enabled
services.node.environment_file_changed_since_start
services.node.environment_files
services.node.exec_main_start
services.node.exec_start
services.node.loaded_config.unknown
services.node.main_pid
services.node.need_daemon_reload
services.node.running_sha256
services.node.same_as_executable
services.node.shape
services.node.started_at
services.node.sub_state
services.node.unit_file_state
services.node.unit_present
services.server.active_state
services.server.cmdline_config_path
services.server.cmdline_matches_unit
services.server.config_changed_since_start
services.server.config_sha256
services.server.dsn_env
services.server.enabled
services.server.environment_file_changed_since_start
services.server.environment_files
services.server.exec_main_start
services.server.exec_start
services.server.loaded_config.unknown
services.server.main_pid
services.server.need_daemon_reload
services.server.running_sha256
services.server.same_as_executable
services.server.shape
services.server.started_at
services.server.sub_state
services.server.unit_file_state
services.server.unit_present
transaction.active
transaction.converge_guard
transaction.journal
transaction.lock_held
transaction.preparation.transaction_lock
transaction.root
`

// EVERY DIRECT FILESYSTEM CALL IN THE INSPECTOR IS LISTED HERE. The public
// reads go through readPublicFile and openImage, which a test observes; a call
// beside them (a discarded read of the key, say) would leave no trace in any
// report, so the source is held to this list. THE BOUNDARY IS THE ENUMERATED
// FORMS: the os package's file-opening functions and factories (os.DirFS,
// os.OpenRoot) named directly, plus any alias of an os function other than the
// two seams; a call reached some other way (a helper in another file, a
// syscall) is outside what this test sees, and the two openInRoot files are the
// root-scoped resolution helper, read on their own.
func TestReleaseInspectReadsTheFilesystemOnlyWhereListed(t *testing.T) {
	src, err := os.ReadFile("releaseinspect.go")
	if err != nil {
		t.Fatal(err)
	}
	call := regexp.MustCompile(`\bos\s*\.\s*(ReadFile|ReadDir|Open|OpenFile|OpenRoot|DirFS|Create|CreateTemp|Stat|Lstat|Readlink|WriteFile|Mkdir|MkdirAll|MkdirTemp|Remove|RemoveAll|Rename|Chmod|Chown|Lchown|Link|Symlink|Truncate)\b`)
	alias := regexp.MustCompile(`=\s*os\.[A-Z]\w*\s*$`)
	var got, aliases []string
	for _, line := range strings.Split(string(src), "\n") {
		if alias.MatchString(line) {
			aliases = append(aliases, strings.TrimSpace(line))
			continue
		}
		if call.MatchString(line) {
			got = append(got, strings.TrimSpace(line))
		}
	}
	sort.Strings(aliases)
	if strings.Join(aliases, "\n") != "openImage = os.Open\nreadPublicFile     = os.ReadFile" {
		t.Errorf("os function aliases in releaseinspect.go:\n%s\nwant exactly the two seams", strings.Join(aliases, "\n"))
	}
	sort.Strings(got)
	want := []string{
		`_, err := os.Lstat(path)`,
		`body, err := os.ReadFile(filepath.Join(active, "guard.json"))`,
		`body, err := os.ReadFile(filepath.Join(dir, "stat"))`,
		`body, err := os.ReadFile(path)`,
		`cmdlineBody, err := os.ReadFile(filepath.Join(dir, "cmdline"))`,
		`dir, err := os.Readlink(active)`,
		`environ, err := os.ReadFile(filepath.Join(dir, "environ"))`,
		`f, err := os.Open(filepath.Join(procRoot, "stat"))`,
		`f, err := os.Open(path)`,
		`f, err := os.OpenFile(filepath.Join(upgradeRoot, txLockName), os.O_RDONLY|syscall.O_NOFOLLOW, 0)`,
		`info, err := os.Lstat(active)`,
		`installed, err := os.Stat(installedBinary)`,
		`pathInfo, err := os.Stat(configPath)`,
		`rootInfo, err := os.Lstat(upgradeRoot)`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("direct filesystem calls in releaseinspect.go:\n%s\nwant exactly:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// THE USER-SPACE RESOLVER FOLLOWS THE KERNEL'S PATH RULES, which is what makes
// the fixtures on a Mac prove what openat2 proves on Linux: ".." applies after
// the symlink before it, an absolute target restarts at the root, a regular
// file followed by anything is ENOTDIR, and a loop ends.
func TestResolveInRootFollowsTheKernelsPathRules(t *testing.T) {
	// The temp dir may itself sit under a symlink (macOS's /var), and the
	// resolver follows the root as the kernel follows the magic link.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "b", "config"), "b\n", 0o644)
	writeFile(t, filepath.Join(root, "b", "c", "config"), "bc\n", 0o644)
	writeFile(t, filepath.Join(root, "a", "file"), "f\n", 0o644)
	if err := os.Symlink("/b/c", filepath.Join(root, "a", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../b/c", filepath.Join(root, "a", "rel")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/a/file/", filepath.Join(root, "a", "slash")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/a/loop", filepath.Join(root, "a", "loop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "b", "config"), filepath.Join(root, "a", "escape")); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/a/link/../config": filepath.Join(root, "b", "config"),
		"/a/rel/../config":  filepath.Join(root, "b", "config"),
		"/a/link/config":    filepath.Join(root, "b", "c", "config"),
		"/../b/config":      filepath.Join(root, "b", "config"),
		"/a/./file":         filepath.Join(root, "a", "file"),
	} {
		got, err := resolveInRoot(root, path)
		if err != nil || got != want {
			t.Errorf("resolveInRoot(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
	for path, fragment := range map[string]string{
		"/a/file/..": "not a directory",
		"/a/file/":   "not a directory",
		"/a/slash":   "not a directory",
		"/a/loop":    "too many",
		"/a/escape":  "no such file",
		"/a/missing": "no such file",
	} {
		if got, err := resolveInRoot(root, path); err == nil || !strings.Contains(err.Error(), fragment) {
			t.Errorf("resolveInRoot(%q) = %q, %v; want an error containing %q", path, got, err, fragment)
		}
	}
}

// THE BUS LABEL IS SYSTEMD'S, by literal example: the fixture names its files
// through the same function, so only literals can catch a wrong escape.
func TestBusLabelEscapesLikeSystemd(t *testing.T) {
	for in, want := range map[string]string{
		"billet-server.service": "billet_2dserver_2eservice",
		"billet-node.service":   "billet_2dnode_2eservice",
		"9lives.service":        "_39lives_2eservice",
		"a_b.service":           "a_5fb_2eservice",
		"getty@tty1.service":    "getty_40tty1_2eservice",
	} {
		if got := busLabel(in); got != want {
			t.Errorf("busLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// THE COMMAND PARSES ITS FLAGS AND RENDERS: a smoke test of the entry point.
func TestReleaseInspectCommandRuns(t *testing.T) {
	f := newInspectFixture(t)
	if err := cmdReleaseInspect(t.Context(), []string{"--json", "--config", f.configPath}); err != nil {
		t.Fatalf("release inspect --json: %v", err)
	}
	if err := cmdReleaseInspect(t.Context(), []string{"--config", f.configPath}); err != nil {
		t.Fatalf("release inspect: %v", err)
	}
	if err := cmdRelease(t.Context(), []string{"inspect", "--json", "--config", f.configPath}); err != nil {
		t.Fatalf("release inspect through the dispatcher: %v", err)
	}
	if err := cmdRelease(t.Context(), []string{"nonsense"}); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Errorf("an unknown release command does not name inspect: %v", err)
	}
}

// inspectFieldSetGuarded is the sorted key set of a PostgreSQL controller under
// a converge guard with a provenance record.
const inspectFieldSetGuarded = `
config.path
config.presence
config.readable
config.same_as_installed
config_binding
executable.image
executable.installed_path
executable.installed_path_same
executable.is_release
executable.process_bound
executable.sha256
executable.version
host.authority
host.deployment_id.unknown
host.node_name
host.node_trust
host.os
host.retirement
installed_config.controllers
installed_config.has_node
installed_config.has_server
installed_config.ledger_backend
installed_config.path
installed_config.sha256
provenance.binary_sha256
provenance.manifest_digest
provenance.verdict
provenance.version
schema
services.node.active_state
services.node.cmdline_config_path
services.node.cmdline_matches_unit
services.node.config_changed_since_start
services.node.config_sha256
services.node.dsn_env
services.node.enabled
services.node.environment_file_changed_since_start
services.node.environment_files
services.node.exec_main_start
services.node.exec_start
services.node.loaded_config.unknown
services.node.main_pid
services.node.need_daemon_reload
services.node.running_sha256
services.node.same_as_executable
services.node.shape
services.node.started_at
services.node.sub_state
services.node.unit_file_state
services.node.unit_present
services.server.active_state
services.server.cmdline_config_path
services.server.cmdline_matches_unit
services.server.config_changed_since_start
services.server.config_sha256
services.server.dsn_env.matches_file
services.server.dsn_env.name
services.server.dsn_env.present
services.server.enabled
services.server.environment_file_changed_since_start
services.server.environment_files
services.server.exec_main_start
services.server.exec_start
services.server.loaded_config.unknown
services.server.main_pid
services.server.need_daemon_reload
services.server.running_sha256
services.server.same_as_executable
services.server.shape
services.server.started_at
services.server.sub_state
services.server.unit_file_state
services.server.unit_present
transaction.active
transaction.converge_guard.claimed_at
transaction.converge_guard.holder
transaction.converge_guard.hostname
transaction.converge_guard.recovery_pointer
transaction.converge_guard.release_executable
transaction.converge_guard.release_executable_sha256
transaction.converge_guard.release_executable_verified
transaction.journal
transaction.lock_held
transaction.preparation.transaction_lock
transaction.root
`

// inspectFieldSetNode is the sorted key set of a node-only host with a trust
// bundle, a retirement journal and a Go claim with a readable journal.
const inspectFieldSetNode = `
config.path
config.presence
config.readable
config.same_as_installed
config_binding
executable.image
executable.installed_path
executable.installed_path_same
executable.is_release
executable.process_bound
executable.sha256
executable.version
host.authority
host.deployment_id
host.node_name
host.node_trust.cas
host.node_trust.leaf.der_sha256
host.node_trust.leaf.not_after
host.node_trust.leaf.pem
host.node_trust.leaf.subject
host.os
host.retirement.phase
installed_config.controllers
installed_config.has_node
installed_config.has_server
installed_config.ledger_backend
installed_config.path
installed_config.sha256
provenance.reason
provenance.verdict
schema
services.node.active_state
services.node.cmdline_config_path
services.node.cmdline_matches_unit
services.node.config_changed_since_start
services.node.config_sha256
services.node.dsn_env
services.node.enabled
services.node.environment_file_changed_since_start
services.node.environment_files
services.node.exec_main_start
services.node.exec_start
services.node.loaded_config.unknown
services.node.main_pid
services.node.need_daemon_reload
services.node.running_sha256
services.node.same_as_executable
services.node.shape
services.node.started_at
services.node.sub_state
services.node.unit_file_state
services.node.unit_present
services.server.active_state
services.server.cmdline_config_path
services.server.cmdline_matches_unit
services.server.config_changed_since_start
services.server.config_sha256
services.server.dsn_env
services.server.enabled
services.server.environment_file_changed_since_start
services.server.environment_files
services.server.exec_main_start
services.server.exec_start
services.server.loaded_config.unknown
services.server.main_pid
services.server.need_daemon_reload
services.server.running_sha256
services.server.same_as_executable
services.server.shape
services.server.started_at
services.server.sub_state
services.server.unit_file_state
services.server.unit_present
transaction.active
transaction.converge_guard
transaction.journal.failure
transaction.journal.from_version
transaction.journal.started_at
transaction.journal.step
transaction.journal.to_version
transaction.lock_held
transaction.preparation.transaction_lock
transaction.root
`
