package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/retirement"
)

// The command and its production readers run the whole retained handoff. Only
// EnvironmentFiles comes from the disposable host's loaded shipped unit; the
// other observations and operations use the request fixture's manager.
func TestRealSystemdRetirementWithoutEnvironmentFiles(t *testing.T) {
	if os.Getenv("BILLET_TEST_SYSTEMD_OPERATION_EFFECTS") != "1" {
		t.Skip("requires a disposable systemd host and PostgreSQL")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Fatal("the disposable systemd host must run Linux as root")
	}
	if os.Getenv("BILLET_TEST_POSTGRES_DSN") == "" {
		t.Fatal("the command witness requires BILLET_TEST_POSTGRES_DSN")
	}
	version, err := exec.CommandContext(t.Context(), "/usr/bin/systemctl", "--version").Output()
	mustOK(t, err)
	if !strings.HasPrefix(string(version), "systemd 255 ") && !strings.HasPrefix(string(version), "systemd 255\n") {
		t.Fatalf("this witness requires systemd 255: %s", version)
	}
	unit := fmt.Sprintf("billet-retire-environment-%d-%d.service", os.Getpid(), time.Now().UnixNano())
	path := filepath.Join("/", "run", "systemd", "system", unit)
	body, err := os.ReadFile(filepath.Join("..", "..", "deploy", nodeUnit))
	mustOK(t, err)
	if strings.Contains(string(body), "EnvironmentFile=") {
		t.Fatal("the shipped control now contains environment-file directives")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	mustOK(t, err)
	t.Cleanup(func() {
		mustOK(t, os.Remove(path))
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 20*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "/usr/bin/systemctl", "daemon-reload").CombinedOutput()
		if err != nil {
			t.Errorf("cleanup reload: %v: %s", err, out)
		}
	})
	_, writeErr := file.Write(body)
	closeErr := file.Close()
	mustOK(t, writeErr)
	mustOK(t, closeErr)
	out, err := exec.CommandContext(t.Context(), "/usr/bin/systemctl", "daemon-reload").CombinedOutput()
	if err != nil {
		t.Fatalf("load shipped control: %v: %s", err, out)
	}
	for _, args := range [][]string{
		{"show", "--property=EnvironmentFiles", "--", unit},
		{"show", "--all", "--property=EnvironmentFiles", "--", unit},
	} {
		out, err := exec.CommandContext(t.Context(), "/usr/bin/systemctl", args...).Output()
		mustOK(t, err)
		if len(out) != 0 {
			t.Fatalf("empty structured-array printer changed: %q", out)
		}
	}
	// Forward the production bus arguments, changing only the fixture unit's
	// object path. A wrong interface or property must reach and fail on systemd.
	t.Setenv("BILLET_RETIRE_REAL_ENVIRONMENT_UNIT", unit)
	f := newRequestFixture(t)
	retainAndRestartANode(t, f)
	f.reserve(t)
	answer, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
	retiredAnswer(t, answer, code)
	if j := requireRetireJournal(t); j.Phase != retirement.PhaseDone {
		t.Fatalf("command did not finish retirement: %s", j.Phase)
	}
	if calls := mustRead(t, filepath.Join(f.unitsDir, ".real-environment-calls")); !strings.Contains(calls, "org.freedesktop.systemd1.Service EnvironmentFiles") {
		t.Fatal("command never reached the real EnvironmentFiles property")
	}
}
