package lifeops

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Measurement specified 2026-09-17, systemd 255. CI records the measured
// version and exact typed rendering; this test makes no claim about BindsTo
// dependents surviving a skipped unit. MainPID belongs to services only.
func TestRealSystemdRetiredConditions(t *testing.T) {
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
		t.Fatalf("measurement requires systemd 255: %s", version)
	}
	t.Logf("retirement condition measurement %s: %s", time.Now().UTC().Format(time.RFC3339), version)
	for _, mechanism := range []string{"Wants", "Requires", "timer", "Also", "direct"} {
		t.Run(mechanism, func(t *testing.T) { realRetiredConditionMechanism(t, mechanism) })
	}
}

func realRetiredConditionMechanism(t *testing.T, mechanism string) {
	t.Helper()
	h := newRealOperationHost(t)
	marker := filepath.Join(t.TempDir(), "retired")
	witness := filepath.Join(filepath.Dir(marker), "executed")
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(marker, "retired\n")
	service, timer := h.prefix+"-protected.service", h.prefix+"-protected.timer"
	target := h.prefix + "-boot.target"
	h.write(target, "[Unit]\nDescription=Disposable boot activation target\n")
	h.write(service, "[Service]\nType=oneshot\nExecStart=/usr/bin/touch "+witness+"\n[Install]\nWantedBy="+target+"\n")
	h.write(timer, "[Timer]\nOnActiveSec=1min\nUnit="+service+"\n[Install]\nWantedBy="+target+"\n")
	protected := []string{service, timer}
	if mechanism == "timer" {
		protected = []string{service}
	}
	for _, unit := range protected {
		dir := filepath.Join(runtimeRoot, "systemd", "system", unit+".d")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		h.dropins = append(h.dropins, dir)
		write(filepath.Join(dir, "10-billet-retired.conf"), "[Unit]\nConditionPathExists=!"+marker+"\n")
	}
	driver := h.prefix + "-driver.service"
	switch mechanism {
	case "Wants", "Requires":
		h.write(driver, "[Unit]\n"+mechanism+"="+service+" "+timer+"\n[Service]\nExecStart=/usr/bin/sleep infinity\n")
	case "timer":
		driver = h.prefix + "-trigger.timer"
		h.write(driver, "[Timer]\nOnActiveSec=50ms\nAccuracySec=1ms\nUnit="+service+"\n")
	case "Also":
		// Activation is through the enablement links at a fresh boot target.
		// No Wants directive on the enabler can hide a missing Also directive.
		h.write(driver, "[Service]\nExecStart=/usr/bin/sleep infinity\n[Install]\nWantedBy="+target+"\nAlso="+service+" "+timer+"\n")
		t.Cleanup(func() {
			if err := h.ctl(context.WithoutCancel(t.Context()), "disable", "--runtime", "--", driver); err != nil {
				t.Error(err)
			}
		})
	case "direct":
		driver = ""
	}
	h.run("daemon-reload")
	insp := NewInspector()
	stamp := func(unit string) uint64 {
		t.Helper()
		value, err := strconv.ParseUint(h.property(unit, "ConditionTimestampMonotonic"), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	measure := func(unit string, result int) {
		t.Helper()
		reply, err := insp.operationTypedProperty(t.Context(), unit, "org.freedesktop.systemd1.Unit", "Conditions")
		if err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal([]any{[]any{"ConditionPathExists", false, true, marker, result}})
		if err != nil {
			t.Fatal(err)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, reply.Data); err != nil {
			t.Fatal(err)
		}
		if reply.Type != "a(sbbsi)" || compact.String() != string(want) {
			t.Fatalf("Conditions rendering changed: type=%s data=%s want=%s", reply.Type, reply.Data, want)
		}
		t.Logf("%s Conditions type=%s data=%s", unit, reply.Type, reply.Data)
		shown, err := exec.CommandContext(t.Context(), "systemctl", "show", "--all", "--property=Conditions", "--", unit).Output()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s systemctl Conditions rendering: %q", unit, shown)
	}
	activate := func() {
		t.Helper()
		switch mechanism {
		case "direct":
			for _, unit := range protected {
				h.run("start", "--", unit)
			}
		case "Also":
			h.run("enable", "--runtime", "--", driver)
			h.run("daemon-reload")
			h.run("start", "--", driver)
			h.run("start", "--", target)
			for _, unit := range protected {
				if h.property(unit, "UnitFileState") != "enabled-runtime" {
					t.Fatalf("Also did not enable %s", unit)
				}
			}
		default:
			h.run("start", "--", driver)
		}
		if driver != "" && mechanism != "timer" && h.property(driver, "ActiveState") != "active" {
			t.Fatal("dependent did not start")
		}
	}
	wait := func(ready func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ready() {
			if time.Now().After(deadline) {
				t.Fatal("activation mechanism did not reach its protected service/timer")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	before := make(map[string]uint64)
	for _, unit := range protected {
		measure(unit, 0)
		before[unit] = stamp(unit)
	}
	activate()
	for _, unit := range protected {
		wait(func() bool { return stamp(unit) > before[unit] })
		if h.property(unit, "ActiveState") != "inactive" || h.property(unit, "ConditionResult") != "no" {
			t.Fatalf("protected unit did not skip fresh activation: %s", unit)
		}
		if err := insp.ProveRetiredCondition(t.Context(), unit, marker); err != nil {
			t.Fatal(err)
		}
		measure(unit, -1)
	}
	if h.property(service, "MainPID") != "0" {
		t.Fatal("protected service acquired a main process")
	}
	if _, err := os.Lstat(witness); !os.IsNotExist(err) {
		t.Fatalf("protected ExecStart ran or witness absence unknown: %v", err)
	}
	// Every mechanism has its own executable counterfactual. Removing its
	// directive or start call must prevent this witness, independently of any
	// preceding case's condition history.
	if driver != "" {
		h.run("stop", "--", driver)
	}
	h.run("stop", "--", target, timer, service)
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	activate()
	wait(func() bool {
		_, err := os.Stat(witness)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return err == nil
	})
	if mechanism != "timer" && h.property(timer, "ActiveState") != "active" {
		t.Fatal("marker-absent counterfactual did not arm the timer")
	}
	// A later reset must invalidate the effective proof and actually run.
	if driver != "" {
		h.run("stop", "--", driver)
	}
	h.run("stop", "--", target, timer, service)
	write(marker, "retired\n")
	if err := os.Remove(witness); err != nil {
		t.Fatal(err)
	}
	for _, unit := range protected {
		write(filepath.Join(runtimeRoot, "systemd", "system", unit+".d", "90-reset.conf"), "[Unit]\nConditionPathExists=\n")
	}
	h.run("daemon-reload")
	for _, unit := range protected {
		if err := insp.ProveRetiredCondition(t.Context(), unit, marker); err == nil || !strings.Contains(err.Error(), "require exactly one") {
			t.Fatalf("reset did not refuse: %v", err)
		}
		reply, err := insp.operationTypedProperty(t.Context(), unit, "org.freedesktop.systemd1.Unit", "Conditions")
		if err != nil || string(reply.Data) != "[]" {
			t.Fatalf("reset rendering: %s %v", reply.Data, err)
		}
	}
	h.run("start", "--", service, timer)
	if _, err := os.Stat(witness); err != nil {
		t.Fatalf("reset counterfactual did not execute: %v", err)
	}
	if h.property(timer, "ActiveState") != "active" {
		t.Fatal("reset counterfactual did not arm timer")
	}
}
