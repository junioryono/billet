package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// THE INSPECTOR'S THREE SCHEDULED UNITS: observed for their runtime facts,
// each its own, exempt from the binding, the shape rule and the executable
// comparison.

// I1: exactly five services, the three new ones carrying exactly their fields,
// each read from its own unit.
func TestReleaseInspectReportsTheScheduledUnitsAsTheirOwn(t *testing.T) {
	f := newInspectFixture(t)
	r := f.report(t)

	keys := make([]string, 0, len(r.Services))
	for k := range r.Services {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	if want := []string{"backup_service", "backup_timer", "node", "server", "upgrade_timer"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("the services map holds %v, want %v", keys, want)
	}

	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Services map[string]map[string]any `json:"services"`
	}

	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}

	fields := func(unit string) []string {
		out := make([]string, 0, len(doc.Services[unit]))
		for k := range doc.Services[unit] {
			out = append(out, k)
		}

		sort.Strings(out)

		return out
	}

	timerFields := []string{"active_state", "enabled", "sub_state", "unit_file_state", "unit_present"}
	serviceFields := []string{"active_state", "enabled", "main_pid", "sub_state", "unit_file_state", "unit_present"}

	for unit, want := range map[string][]string{"upgrade_timer": timerFields, "backup_timer": timerFields, "backup_service": serviceFields} {
		if got := fields(unit); !reflect.DeepEqual(got, want) {
			t.Errorf("%s carries %v, want %v", unit, got, want)
		}
	}

	// DISTINCT STATES, each the unit's own: the fixture gives the upgrade timer
	// enabled and waiting, the backup timer disabled and dead, the backup
	// service static and dead.
	want := map[string]map[string]any{
		"upgrade_timer":  {"unit_present": true, "unit_file_state": "enabled", "enabled": true, "active_state": "active", "sub_state": "waiting"},
		"backup_timer":   {"unit_present": true, "unit_file_state": "disabled", "enabled": false, "active_state": "inactive", "sub_state": "dead"},
		"backup_service": {"unit_present": true, "unit_file_state": "static", "enabled": false, "active_state": "inactive", "sub_state": "dead", "main_pid": nil},
	}

	for unit, w := range want {
		if !reflect.DeepEqual(doc.Services[unit], w) {
			t.Errorf("%s reads %v, want %v", unit, doc.Services[unit], w)
		}
	}
}

// I2: a not-found unit keeps its runtime facts, a running one its pid; an
// observed zero is null; a value that is not a number, and a unit systemd did
// not answer for, are unknown.
func TestReleaseInspectScheduledUnitPIDs(t *testing.T) {
	f := newInspectFixture(t)
	f.scheduledUnit(t, "billet-backup.service", "not-found", "", "active", "running", 4242)
	f.scheduledUnit(t, "billet-backup.timer", "not-found", "", "inactive", "dead", 0)

	r := f.report(t)

	svc := r.Services["backup_service"]
	if mustKnown(t, "backup_service.unit_present", svc.UnitPresent) != false {
		t.Error("a not-found backup service reads present")
	}

	if got := mustKnown(t, "backup_service.active_state", svc.ActiveState); got != "active" {
		t.Errorf("a running not-found backup service reads %v", got)
	}

	if got := mustKnown(t, "backup_service.main_pid", svc.MainPID); got != 4242 {
		t.Errorf("a running not-found backup service's pid reads %v, want 4242 retained", got)
	}

	if got := mustKnown(t, "backup_service.unit_file_state", svc.UnitFileState); got != nil {
		t.Errorf("a not-found unit's file state reads %v, want null", got)
	}

	timer := r.Services["backup_timer"]
	if mustKnown(t, "backup_timer.unit_present", timer.UnitPresent) != false ||
		mustKnown(t, "backup_timer.active_state", timer.ActiveState) != "inactive" {
		t.Errorf("a not-found timer reads %+v", timer)
	}

	f.scheduledUnit(t, "billet-backup.service", "loaded", "static", "inactive", "dead", 0)
	if got := mustKnown(t, "an observed zero pid", f.report(t).Services["backup_service"].MainPID); got != nil {
		t.Errorf("an observed zero pid reads %v, want null", got)
	}

	writeFile(t, f.unitsDir+"/billet-backup.service", "LoadState=loaded\nUnitFileState=static\nActiveState=active\nSubState=running\nMainPID=x\n", 0o644)
	mustUnknown(t, "a pid that is not a number", f.report(t).Services["backup_service"].MainPID, "not a number")

	writeFile(t, f.unitsDir+"/billet-upgrade.timer", "", 0o644)
	mustUnknown(t, "a unit systemd did not answer for", f.report(t).Services["upgrade_timer"].UnitPresent, "no LoadState")
}

// I3: THE EXEMPTION'S SCOPE. A healthy host with all five units binds true;
// the backup service active on another executable, with a running pid whose
// image differs, still binds true; and the controls on server and node keep
// their own answers.
func TestReleaseInspectScheduledUnitsAreExemptFromTheBinding(t *testing.T) {
	f := newInspectFixture(t)

	if got := mustKnown(t, "binding on a healthy host", f.report(t).ConfigBinding); got != true {
		t.Fatalf("a healthy host with all five units binds %v", got)
	}

	// The backup service, active, on another executable, with a main pid.
	writeFile(t, f.unitsDir+"/billet-backup.service", "LoadState=loaded\nUnitFileState=static\nActiveState=active\nSubState=running\n"+
		"MainPID=4242\nExecStart={ path=/usr/bin/billet-backup-other ; argv[]=/usr/bin/billet-backup-other run ; ignore_errors=no ; "+
		"start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }\n", 0o644)

	r := f.report(t)
	if got := mustKnown(t, "binding with the backup service running another executable", r.ConfigBinding); got != true {
		t.Errorf("the backup service's executable reached the binding: %v", got)
	}

	if got := mustKnown(t, "backup_service.main_pid", r.Services["backup_service"].MainPID); got != 4242 {
		t.Errorf("the running backup service's pid reads %v", got)
	}

	// The controls, each on server or node: another config on the server's
	// ExecStart binds FALSE; an unsupported node shape is UNKNOWN; a node whose
	// running image differs keeps the binding TRUE with same_as_executable
	// false. The backup service stays as it is through each.
	t.Run("another config on the server", func(t *testing.T) {
		f := newInspectFixture(t)
		f.unitRunning(t, "billet-server.service", "server", f.dir+"/other.yaml", nil)

		r := f.report(t)
		if got := mustKnown(t, "binding", r.ConfigBinding); got != false {
			t.Errorf("a server bound to another config binds %v, want false", got)
		}

		if _, ok := r.Services["backup_service"]; !ok {
			t.Error("the backup service is missing")
		}
	})

	t.Run("an unsupported node shape", func(t *testing.T) {
		f := newInspectFixture(t)
		f.unitWith(t, "billet-node.service", "/usr/bin/env billet node --config "+f.configPath, nil, "", inspectPID, "active", "running")

		r := f.report(t)
		mustUnknown(t, "binding", r.ConfigBinding, "shape")

		if got := mustKnown(t, "node shape", r.Services["node"].Shape); got != "unsupported" {
			t.Errorf("the node's shape reads %v", got)
		}
	})

	t.Run("a node whose image differs", func(t *testing.T) {
		f := newInspectFixture(t)
		f.unitAbsent(t, "billet-server.service")
		f.unitRunning(t, "billet-node.service", "node", f.configPath, nil)
		f.process(t, []string{f.binPath, "node", "--config", f.configPath}, nil)
		// The process runs another build than the executable inspecting it.
		writeFile(t, filepath.Join(f.procDir, strconv.Itoa(inspectPID), "image"), "IMAGE-B\n", 0o755)

		r := f.report(t)
		if got := mustKnown(t, "node same_as_executable", r.Services["node"].SameAsExecutable); got != false {
			t.Errorf("a node whose running image differs reads same_as_executable %v", got)
		}

		if got := mustKnown(t, "binding", r.ConfigBinding); got != true {
			t.Errorf("a differing image made the binding %v; the image is the executable comparison, not the binding", got)
		}
	})
}

// I4: the scheduled units' enablement is the one derivation, structurally.
func TestReleaseInspectScheduledUnitsUseTheOneEnablementDerivation(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "releaseinspect.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	calls := map[string]int{}

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || (fn.Name.Name != "inspectScheduledUnit" && fn.Name.Name != "inspectServiceSection") {
			continue
		}

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "unitEnablement" {
				calls[fn.Name.Name]++
			}

			return true
		})
	}

	if calls["inspectScheduledUnit"] != 1 || calls["inspectServiceSection"] != 1 {
		t.Errorf("unitEnablement is called %v times; want once from each reader", calls)
	}

	// And nothing else in the file spells the enablement states.
	body, err := parser.ParseFile(fset, "releaseinspect.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}

	count := 0

	ast.Inspect(body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "enabled-runtime") {
			count++
		}

		return true
	})

	if count != 1 {
		t.Errorf("%d string literals spell enabled-runtime; want the one derivation", count)
	}
}
