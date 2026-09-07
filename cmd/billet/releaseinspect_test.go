package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/provenance"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// The fixture stands a Linux host in: a procfs tree under procRoot, a fake
// systemctl answering from files, this test's own image at selfExePath and the
// managed path, and every other seam pointed under one temp directory.
type inspectFixture struct {
	dir, procDir, unitsDir, configPath, stateDir, binPath string
	// opened records every path the report opened as an image.
	opened []string
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
		hostOS, procRoot, selfExe, installed, systemctl, receipt, root, provenance string
		open                                                                       func(string) (*os.File, error)
		after                                                                      func(string)
		samples                                                                    int
	}{hostOS, procRoot, selfExePath, installedBinary, systemctlBinary, retiredJournalPath, upgradeRoot,
		provenance.Path, openImage, inspectAfterOpen, inspectSamples}
	t.Cleanup(func() {
		hostOS, procRoot, selfExePath, installedBinary = prev.hostOS, prev.procRoot, prev.selfExe, prev.installed
		systemctlBinary, retiredJournalPath, upgradeRoot = prev.systemctl, prev.receipt, prev.root
		provenance.Path, openImage, inspectAfterOpen, inspectSamples = prev.provenance, prev.open, prev.after, prev.samples
	})
	hostOS = "linux"
	procRoot = f.procDir
	selfExePath, installedBinary = f.binPath, f.binPath
	systemctlBinary = filepath.Join(dir, "bin", "systemctl")
	retiredJournalPath = filepath.Join(dir, "retired", "journal.json")
	upgradeRoot = filepath.Join(dir, "upgrades")
	provenance.Path = filepath.Join(dir, "installed.json")
	inspectAfterOpen = nil
	openImage = func(path string) (*os.File, error) {
		f.opened = append(f.opened, path)
		return os.Open(path)
	}

	writeFile(t, f.binPath, "IMAGE-A\n", 0o755)
	writeFile(t, filepath.Join(f.procDir, "stat"), "cpu  1 2 3 4\nbtime "+strconv.FormatInt(inspectBootTime, 10)+"\nprocesses 9\n", 0o644)
	writeFile(t, systemctlBinary, "#!/bin/sh\nverb=$1\nunit=\"\"\nfor a in \"$@\"; do unit=$a; done\nif [ \"$verb\" = cat ]; then cat \"$BILLET_FAKE_UNITS/$unit.cat\"; else cat \"$BILLET_FAKE_UNITS/$unit\"; fi\n", 0o755)
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
	writeFile(t, filepath.Join(f.unitsDir, unit), "LoadState=not-found\nUnitFileState=\nActiveState="+active+"\nSubState="+sub+"\nMainPID="+strconv.Itoa(pid)+"\nInvocationID=\nExecMainStartTimestamp=\nEnvironmentFiles=\nEnvironment=\n", 0o644)
}

// unitRunning renders the properties systemd reports for a unit with the
// shipped ExecStart shape; envFiles is the rendered EnvironmentFiles value.
func (f *inspectFixture) unitRunning(t *testing.T, unit, role, configPath string, envFiles []string) {
	t.Helper()
	f.unitWith(t, unit, fmt.Sprintf("%s %s --config %s", f.binPath, role, configPath), envFiles, "", inspectPID, "active", "running")
}

// unitWith renders a unit both ways systemd shows it: the properties of
// `systemctl show` (argv joined by spaces, one EnvironmentFiles line per file,
// as systemd 255 prints) and the text of `systemctl cat`.
func (f *inspectFixture) unitWith(t *testing.T, unit, argv string, envFiles []string, environment string, pid int, active, sub string) {
	t.Helper()
	path := strings.Fields(argv)[0]
	body := "LoadState=loaded\nUnitFileState=enabled\nActiveState=" + active + "\nSubState=" + sub + "\nMainPID=" + strconv.Itoa(pid) + "\n" +
		"InvocationID=0123456789abcdef0123456789abcdef\nExecMainStartTimestamp=Tue 2023-11-14 22:14:00 UTC\n" +
		"ExecStart={ path=" + path + " ; argv[]=" + argv + " ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }\n"
	if len(envFiles) == 0 {
		body += "EnvironmentFiles=\n"
	}
	for _, e := range envFiles {
		body += "EnvironmentFiles=" + e + " (ignore_errors=yes)\n"
	}
	body += "Environment=" + environment + "\n"
	writeFile(t, filepath.Join(f.unitsDir, unit), body, 0o644)
	text := "# /etc/systemd/system/" + unit + "\n[Service]\nExecStart=" + argv + "\n"
	for _, e := range envFiles {
		text += "EnvironmentFile=-" + e + "\n"
	}
	if environment != "" {
		text += "Environment=" + environment + "\n"
	}
	f.unitText(t, unit, text)
}

// unitText overrides what `systemctl cat` returns for a unit.
func (f *inspectFixture) unitText(t *testing.T, unit, text string) {
	t.Helper()
	writeFile(t, filepath.Join(f.unitsDir, unit+".cat"), text, 0o644)
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
}

func (f *inspectFixture) processStart(t *testing.T, pid int, startTicks int64) {
	t.Helper()
	fields := make([]string, 50)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
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
		"equal":          {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=\"" + dsn + "\"\n", "equal", ""},
		"equal unquoted": {[]string{"BILLET_PG_DSN=" + dsn}, true, "# rendered\nBILLET_PG_DSN=" + dsn + "\n", "equal", ""},
		"differs":        {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=postgres://other\n", "differs", ""},
		"not in file":    {[]string{"BILLET_PG_DSN=" + dsn}, true, "OTHER=1\n", "not_in_file", ""},
		"absent":         {[]string{"PATH=/usr/bin"}, true, "BILLET_PG_DSN=x\n", "absent", ""},
		"no file":        {[]string{"BILLET_PG_DSN=" + dsn}, false, "", "", "no single environment file"},
		"assigned twice": {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=postgres://first\nBILLET_PG_DSN=" + dsn + "\n", "", "assigned twice"},
		"quote inside":   {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=\"a\"b\"\n", "", "unsupported environment file syntax"},
		"backslash":      {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=a\\\\b\n", "", "unsupported environment file syntax"},
		"bad other line": {[]string{"BILLET_PG_DSN=" + dsn}, true, "BILLET_PG_DSN=" + dsn + "\nexport OTHER=1\n", "", "unsupported environment file syntax"},
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

// ARGUMENT BOUNDARIES COME FROM THE UNIT TEXT: `systemctl show` joins argv with
// spaces, so a two-argument `billet "server --config x"` displays like the
// supported four-argument form and only `systemctl cat` tells them apart.
func TestReleaseInspectReadsArgumentBoundariesFromTheUnitText(t *testing.T) {
	f := newInspectFixture(t)
	f.unitText(t, "billet-server.service", "[Service]\nExecStart="+f.binPath+" \"server --config "+f.configPath+"\"\n")
	r := f.report(t)
	svc := r.Services["server"]
	if mustKnown(t, "shape", svc.Shape) != "unsupported" {
		t.Errorf("shape = %v, want unsupported for a quoted two-argument ExecStart", svc.Shape.value)
	}
	mustUnknown(t, "config_binding", r.ConfigBinding, "unsupported")
	// The rendered properties must agree with the text: a unit text that reads
	// as the supported shape beside a rendered argv that does not is refused
	// on the rendered side.
	f.unitWith(t, "billet-server.service", "/usr/bin/env billet server --config "+f.configPath, nil, "", inspectPID, "active", "running")
	f.unitText(t, "billet-server.service", "[Service]\nExecStart="+f.binPath+" server --config "+f.configPath+"\n")
	if svc := f.report(t).Services["server"]; mustKnown(t, "shape", svc.Shape) != "unsupported" || !strings.Contains(svc.ShapeReason, "rendered ExecStart does not match") {
		t.Errorf("shape = %v (%s), want the rendered mismatch refused", svc.Shape.value, svc.ShapeReason)
	}
	// A drop-in adding a second ExecStart is visible in the text too.
	f.unitText(t, "billet-server.service", "[Service]\nExecStart="+f.binPath+" server --config "+f.configPath+"\n# /etc/systemd/system/billet-server.service.d/x.conf\n[Service]\nExecStart=/bin/true\n")
	if svc := f.report(t).Services["server"]; mustKnown(t, "shape", svc.Shape) != "unsupported" {
		t.Errorf("shape = %v, want unsupported for a drop-in ExecStart", svc.Shape.value)
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
	if r.Config.Readable {
		t.Fatal("a broken config read as readable")
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
	// THE KEY IS UNREADABLE, so a read of it, by any path, would surface as an
	// error in the report rather than go unnoticed.
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
		if err := os.MkdirAll(upgradeRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(upgradeRoot, "gone"), filepath.Join(upgradeRoot, "active")); err != nil {
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
config.readable
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
installed_config.has_node
installed_config.has_server
installed_config.path
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
services.node.running_sha256
services.node.same_as_executable
services.node.shape
services.node.started_at
services.node.sub_state
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
services.server.running_sha256
services.server.same_as_executable
services.server.shape
services.server.started_at
services.server.sub_state
services.server.unit_present
transaction.active
transaction.converge_guard
transaction.journal
transaction.lock_held
transaction.preparation.transaction_lock
transaction.root
`

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
config.readable
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
installed_config.has_node
installed_config.has_server
installed_config.path
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
services.node.running_sha256
services.node.same_as_executable
services.node.shape
services.node.started_at
services.node.sub_state
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
services.server.running_sha256
services.server.same_as_executable
services.server.shape
services.server.started_at
services.server.sub_state
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
// bundle, a retirement journal and a dangling Go claim.
const inspectFieldSetNode = `
config.path
config.readable
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
installed_config.has_node
installed_config.has_server
installed_config.path
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
services.node.running_sha256
services.node.same_as_executable
services.node.shape
services.node.started_at
services.node.sub_state
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
services.server.running_sha256
services.server.same_as_executable
services.server.shape
services.server.started_at
services.server.sub_state
services.server.unit_present
transaction.active
transaction.converge_guard
transaction.journal.unknown
transaction.lock_held
transaction.preparation.transaction_lock
transaction.root
`
