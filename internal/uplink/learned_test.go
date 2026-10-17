package uplink

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A RESUMED RATE RETURNS TOWARD ITS CEILING WITH THE TIME THE SHAPER WAS DOWN:
// a restart of seconds keeps the cut, an hour keeps half of it, a day nothing.
// It never passes the ceiling now in force, never falls under the floor, and a
// rate at the ceiling it was learned under is nothing learned.
func TestAResumedRateReturnsTowardItsCeilingWithTheTimeDown(t *testing.T) {
	t.Parallel()

	near := func(got, want float64) bool { return math.Abs(got-want) < 0.01 }

	for name, c := range map[string]struct {
		learned learnedRate
		ceiling float64
		down    time.Duration
		want    float64
	}{
		"a restart of seconds":           {learnedRate{Max: 400, Rate: 100}, 400, 5 * time.Second, 400 - 300*math.Exp2(-5.0/3600)},
		"down for one half-life":         {learnedRate{Max: 400, Rate: 100}, 400, time.Hour, 250},
		"down for a day":                 {learnedRate{Max: 400, Rate: 100}, 400, 24 * time.Hour, 400 - 300*math.Exp2(-24)},
		"a ceiling lowered since":        {learnedRate{Max: 400, Rate: 300}, 200, 0, 200},
		"a ceiling raised since":         {learnedRate{Max: 400, Rate: 100}, 1000, 0, 100},
		"nothing learned under the last": {learnedRate{Max: 400, Rate: 400}, 1000, 0, 1000},
		"a rate under the floor":         {learnedRate{Max: 400, Rate: 2}, 400, 0, Floor},
		"no rate at all":                 {learnedRate{}, 400, 0, 400},
		"a rate that is not a number":    {learnedRate{Max: 400, Rate: math.NaN()}, 400, 0, 400},
	} {
		if got := c.learned.resume(c.ceiling, c.down); !near(got, c.want) {
			t.Errorf("%s: resumed at %.2f, want %.2f", name, got, c.want)
		}
	}
}

// RATES LEARNED ON ANOTHER INTERFACE DESCRIBE ANOTHER LINE, and a record from a
// clock ahead of this one cannot say how long the shaper was down. WITHIN ONE
// BOOT the index decides, so another device under the name is refused; ACROSS A
// REBOOT, which renumbers interfaces, the name does.
func TestLearnedRatesResumeOnlyOnTheirInterface(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)
	ago := now.Add(-time.Minute)

	for name, c := range map[string]struct {
		l                  learned
		iface, index, boot string
		want               bool
	}{
		"the same interface":            {learned{Iface: "eno2np1", Index: "2", Boot: "a", At: ago}, "eno2np1", "2", "a", true},
		"the same interface renamed":    {learned{Iface: "eth0", Index: "2", Boot: "a", At: ago}, "eno2np1", "2", "a", true},
		"another device under the name": {learned{Iface: "eno2np1", Index: "2", Boot: "a", At: ago}, "eno2np1", "7", "a", false},
		"by name, no index":             {learned{Iface: "eno2np1", At: ago}, "eno2np1", "", "a", true},
		"another name, no index":        {learned{Iface: "eth0", At: ago}, "eno2np1", "", "a", false},
		"a time ahead of this clock":    {learned{Iface: "eno2np1", Index: "2", At: now.Add(time.Minute)}, "eno2np1", "2", "a", false},
		"no time":                       {learned{Iface: "eno2np1", Index: "2"}, "eno2np1", "2", "a", false},
		"after a reboot renumbered it":  {learned{Iface: "eno2np1", Index: "2", Boot: "a", At: ago}, "eno2np1", "3", "b", true},
		"after a reboot, another name":  {learned{Iface: "eth0", Index: "2", Boot: "a", At: ago}, "eno2np1", "2", "b", false},
		"no boot recorded, renumbered":  {learned{Iface: "eno2np1", Index: "2", At: ago}, "eno2np1", "3", "b", false},
		"no boot read now, renumbered":  {learned{Iface: "eno2np1", Index: "2", Boot: "a", At: ago}, "eno2np1", "3", "", false},
	} {
		ok, why := c.l.resumes(c.iface, c.index, c.boot, now)
		if ok != c.want || (!ok && why == "") {
			t.Errorf("%s: resumes %v (%q), want %v with a reason when not", name, ok, why, c.want)
		}
	}
}

// fakeRuntimeDir points the record and the learned rates at directories of the
// test's own, apart as they are on a host. Not parallel: StateFile and
// LearnedFile are the package's.
func fakeRuntimeDir(t *testing.T) {
	t.Helper()

	oldState, oldLearned := StateFile, LearnedFile
	StateFile = filepath.Join(t.TempDir(), "interface")
	LearnedFile = filepath.Join(t.TempDir(), "learned")
	t.Cleanup(func() { StateFile, LearnedFile = oldState, oldLearned })
}

// WHAT A RUN RECORDS IS WHAT THE NEXT ONE RESUMES, under the state directory
// rather than beside the interface record a reboot empties, and a clear of the
// shaping leaves it.
func TestTheNextRunResumesWhatThisOneRecorded(t *testing.T) {
	fakeRuntimeDir(t)

	log := slog.New(&records{})
	ctl := newRunController(1000, Options{Params: DefaultParams(), MaxUp: 400, MaxDown: 300})

	rememberer(log, ctl, "eno2np1", "2", "boot-a")(120, 80)

	if _, err := os.Stat(LearnedFile); err != nil {
		t.Fatalf("the learned rates are not at %s: %v", LearnedFile, err)
	}
	if filepath.Dir(LearnedFile) == filepath.Dir(StateFile) {
		t.Fatalf("the learned rates are beside the record %s, which a reboot empties", StateFile)
	}

	logs := &records{}
	next := newRunController(1000, Options{Params: DefaultParams(), MaxUp: 400, MaxDown: 300})
	resumeLearned(slog.New(logs), next, "eno2np1", "2", "boot-a", time.Now())

	if math.Abs(next.Up.Rate-120) > 0.5 || math.Abs(next.Down.Rate-80) > 0.5 {
		t.Errorf("the next run started at %.1f/%.1f, want about the 120/80 recorded", next.Up.Rate, next.Down.Rate)
	}
	if next.Up.Max != 400 || next.Down.Max != 300 {
		t.Errorf("resuming moved the ceilings to %.1f/%.1f", next.Up.Max, next.Down.Max)
	}
	if logs.count("resuming the rates an earlier run learned") != 1 {
		t.Errorf("the resumption was not said")
	}

	// AFTER A REBOOT THAT RENUMBERED THE INTERFACE, the same rates, by its name:
	// the record says which boot set it.
	rebooted := newRunController(1000, Options{Params: DefaultParams(), MaxUp: 400, MaxDown: 300})
	resumeLearned(slog.New(&records{}), rebooted, "eno2np1", "3", "boot-b", time.Now())
	if math.Abs(rebooted.Up.Rate-120) > 0.5 || math.Abs(rebooted.Down.Rate-80) > 0.5 {
		t.Errorf("after a reboot renumbered eno2np1 the run started at %.1f/%.1f, want about the 120/80 recorded",
			rebooted.Up.Rate, rebooted.Down.Rate)
	}

	// ON ANOTHER INTERFACE, THE CEILING.
	other := newRunController(1000, Options{Params: DefaultParams(), MaxUp: 400, MaxDown: 300})
	resumeLearned(slog.New(&records{}), other, "wlan0", "7", "boot-a", time.Now())
	if other.Up.Rate != 400 || other.Down.Rate != 300 {
		t.Errorf("rates learned on eno2np1 were resumed on wlan0: %.1f/%.1f", other.Up.Rate, other.Down.Rate)
	}
}

// A RECORD THAT CANNOT BE READ STARTS AT THE CEILING AND SAYS SO, and none at
// all starts there quietly.
func TestUnreadableLearnedRatesStartAtTheCeiling(t *testing.T) {
	fakeRuntimeDir(t)

	ctl := newRunController(1000, Options{Params: DefaultParams()})
	resumeLearned(slog.New(&records{}), ctl, "eno2np1", "2", "boot-a", time.Now())
	if ctl.Up.Rate != 1000 || ctl.Down.Rate != 1000 {
		t.Fatalf("with nothing recorded the run started at %.1f/%.1f", ctl.Up.Rate, ctl.Down.Rate)
	}

	if err := os.WriteFile(learnedFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	logs := &records{}
	resumeLearned(slog.New(logs), ctl, "eno2np1", "2", "boot-a", time.Now())
	if ctl.Up.Rate != 1000 || ctl.Down.Rate != 1000 || logs.count("could not be read") != 1 {
		t.Errorf("an unreadable record started the run at %.1f/%.1f, said %d times",
			ctl.Up.Rate, ctl.Down.Rate, logs.count("could not be read"))
	}
}

// THE STEPPER REMEMBERS WHAT CAKE WAS SET TO, AND ONLY THAT: a rate a failed tc
// command never applied is not what the line was held at.
func TestTheStepperRemembersOnlyAppliedRates(t *testing.T) {
	t.Parallel()

	var remembered [][2]float64

	failing := true
	counters := Counters{}
	clock := time.Unix(1_000_000, 0)

	// THE GATEWAY STALLS IN EVERY ROUND while the host downloads 400 Mbit/s, so
	// the step cuts; the reflectors answer at their baseline.
	p := &fakeProber{reflectors: three, gateway: "192.168.1.254", rtt: func(round int) map[string]time.Duration {
		answers := idle()
		answers["192.168.1.254"] = time.Millisecond
		if round > 0 {
			answers["192.168.1.254"] = 60 * time.Millisecond
		}

		return answers
	}}
	ctl := newController()
	logs := &records{}

	st, err := newStepper(slog.New(logs), p, ctl,
		func() (Counters, error) {
			counters.Sent += uint64(5 * 1e6 * Tick.Seconds() / 8)
			counters.Received += uint64(400 * 1e6 * Tick.Seconds() / 8)

			return counters, nil
		},
		func(context.Context, float64, float64) error {
			if failing {
				return errors.New("tc: RTNETLINK answers: Device or resource busy")
			}

			return nil
		},
		func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	st.remember = func(up, down float64) { remembered = append(remembered, [2]float64{up, down}) }

	var setErr error
	for range 8 {
		clock = clock.Add(Tick)
		if setErr = st.tick(t.Context()); setErr != nil {
			break
		}
	}
	if setErr == nil {
		t.Fatal("a gateway risen 60 ms under a 400 Mbit/s download never asked tc for a cut")
	}
	if len(remembered) != 0 {
		t.Fatalf("rates tc refused were remembered: %v", remembered)
	}

	// AND THE MINUTE'S AND THE STOP'S RECORD SAY NOTHING EITHER, since a set
	// that failed may have changed one direction.
	st.record()
	if len(remembered) != 0 {
		t.Fatalf("after a failed set, record told %v", remembered)
	}

	failing = false
	for range 8 {
		clock = clock.Add(Tick)
		if err := st.tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	if len(remembered) == 0 {
		t.Fatal("no applied rate was remembered")
	}
	if last := remembered[len(remembered)-1]; last != [2]float64{ctl.Up.Rate, ctl.Down.Rate} {
		t.Errorf("remembered %v, want the rates applied last, %.1f/%.1f", last, ctl.Up.Rate, ctl.Down.Rate)
	}

	// ONCE A SET HAS SUCCEEDED, record tells the rates CAKE holds, read when it
	// runs.
	told := len(remembered)
	st.record()
	if len(remembered) != told+1 || remembered[told] != st.applied {
		t.Errorf("record told %v, want the applied %v", remembered[told:], st.applied)
	}

	// AND THE CUT SAYS WHAT THE GATEWAY MEASURED, and the confirmed stall it was
	// cut on: past Bloat, and no more than this round's reading, being the lesser
	// of two.
	said := false
	for _, rec := range logs.list {
		if !strings.HasSuffix(rec.Message, "; cut") {
			continue
		}

		a := attrs(rec)
		reading, read := a["gateway_delay"]
		stall, stalled := a["gateway_stall"]
		if read && stalled && reading.Kind() == slog.KindDuration && stall.Kind() == slog.KindDuration &&
			stall.Duration() > ctl.Params.Bloat && stall.Duration() <= reading.Duration() {
			said = true
		}
	}
	if !said {
		t.Error("no cut logged the gateway's measured rise beside the confirmed stall it was cut on")
	}
}

// RUN RESUMES BEFORE IT INSTALLS, RECORDS ONCE INSTALLED, AND HANDS THE LOOP THE
// SAME RECORDER, which hands it to the stepper. Run needs root and tc, so this
// reads it.
func TestARunResumesBeforeInstallingAndRemembersThroughItsStepper(t *testing.T) {
	t.Parallel()

	parsed, err := parser.ParseFile(token.NewFileSet(), "run.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	funcs := map[string]*ast.FuncDecl{}
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			funcs[fn.Name.Name] = fn
		}
	}

	// THE SAME BOOT GOES TO BOTH, read from bootID: a record that does not say
	// which boot set it would have its index compared across a reboot, which
	// renumbers interfaces.
	var resumed, installed, recorded, looped, remembering, booted token.Pos
	ast.Inspect(funcs["Run"], func(n ast.Node) bool {
		if assign, ok := n.(*ast.AssignStmt); ok && len(assign.Lhs) == 2 && len(assign.Rhs) == 2 &&
			isName(assign.Lhs[1], "boot") && types.ExprString(assign.Rhs[1]) == "bootID()" {
			booted = assign.Pos()
		}

		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		switch fun := call.Fun.(type) {
		case *ast.Ident:
			switch {
			case fun.Name == "resumeLearned" && len(call.Args) == 6 && isName(call.Args[1], "ctl") &&
				isName(call.Args[4], "boot"):
				resumed = call.Pos()
			case fun.Name == "rememberer" && len(call.Args) == 5 && isName(call.Args[4], "boot"):
				remembering = call.Pos()
			case fun.Name == "remember" && !recorded.IsValid():
				recorded = call.Pos()
			case fun.Name == "loop" && len(call.Args) == 9 && isName(call.Args[7], "remember") &&
				isName(call.Args[8], "follow"):
				looped = call.Pos()
			}
		case *ast.SelectorExpr:
			// WITH THE CONTROLLER'S RATES, which resumeLearned set: installing at
			// the ceilings would resume nothing.
			if fun.Sel.Name == "Install" && isName(fun.X, "shaper") && len(call.Args) == 3 &&
				types.ExprString(call.Args[1]) == "ctl.Up.Rate" && types.ExprString(call.Args[2]) == "ctl.Down.Rate" {
				installed = call.Pos()
			}
		}

		return true
	})

	if !resumed.IsValid() || !installed.IsValid() || resumed > installed {
		t.Error("Run does not resume what an earlier run learned before it installs CAKE")
	}
	if !booted.IsValid() || !remembering.IsValid() || booted > resumed {
		t.Error("Run does not read the boot from bootID and hand it to both resumeLearned and rememberer")
	}
	if !recorded.IsValid() || recorded < installed || !looped.IsValid() {
		t.Error("Run does not record the installed rates and hand loop its recorder")
	}

	handed := false
	ast.Inspect(funcs["loop"], func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if ok && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 {
			if sel, ok := assign.Lhs[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "remember" && isName(sel.X, "st") &&
				isName(assign.Rhs[0], "remember") {
				handed = true
			}
		}

		return true
	})
	if !handed {
		t.Error("loop does not hand its stepper the recorder")
	}

	// AND RECORDS THE APPLIED RATES EVERY MINUTE AND AS IT ENDS, so the next run
	// counts only the time the shaper was down: a call of the recorder with the
	// stepper's applied pair inside the minute's block, and one deferred.
	// st.record READS THE RATES WHEN IT RUNS (a method value on the stepper), so
	// even deferred it records what CAKE holds as the loop ends.
	applied := func(call *ast.CallExpr) bool {
		return types.ExprString(call.Fun) == "st.record" && len(call.Args) == 0
	}

	var inMinute, deferred, follows bool
	ast.Inspect(funcs["loop"], func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.IfStmt:
			if types.ExprString(node.Cond) == "time.Since(exeCheckedAt) >= time.Minute" {
				for _, stmt := range node.Body.List {
					if expr, ok := stmt.(*ast.ExprStmt); ok {
						if call, ok := expr.X.(*ast.CallExpr); ok && applied(call) {
							inMinute = true
						}
					}

					// AND FOLLOWS THE DEFAULT ROUTE'S GATEWAY, when it was given that.
					if inner, ok := stmt.(*ast.IfStmt); ok && types.ExprString(inner.Cond) == "follow != nil" {
						ast.Inspect(inner, func(m ast.Node) bool {
							if call, ok := m.(*ast.CallExpr); ok && isName(call.Fun, "followGateway") &&
								len(call.Args) == 5 && isName(call.Args[1], "follow") && isName(call.Args[2], "pinger") {
								follows = true
							}

							return true
						})
					}
				}
			}
		case *ast.DeferStmt:
			ast.Inspect(node, func(inner ast.Node) bool {
				if call, ok := inner.(*ast.CallExpr); ok && applied(call) {
					deferred = true
				}

				return true
			})
		}

		return true
	})
	if !inMinute || !deferred {
		t.Errorf("loop records the applied rates every minute %v and as it ends %v, want both", inMinute, deferred)
	}
	if !follows {
		t.Error("loop does not follow the default route's gateway every minute")
	}

	// RUN FOLLOWS ONLY THE DEFAULT ROUTE'S: a gateway the operator named, or
	// none, is not replaced.
	onlyDefault := false
	ast.Inspect(funcs["Run"], func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || types.ExprString(ifs.Cond) != `!opts.NoGateway && opts.Gateway == ""` {
			return true
		}
		for _, stmt := range ifs.Body.List {
			if assign, ok := stmt.(*ast.AssignStmt); ok && len(assign.Lhs) == 1 && isName(assign.Lhs[0], "follow") {
				onlyDefault = true
			}
		}

		return true
	})
	if !onlyDefault {
		t.Error(`Run does not set follow only when !opts.NoGateway && opts.Gateway == ""`)
	}
}

// A RUN THAT WAS DOWN AN HOUR RESUMES HALFWAY BACK TO ITS CEILING, through the
// same path a start takes. Not parallel: StateFile is the package's.
func TestAResumeCountsTheTimeTheShaperWasDown(t *testing.T) {
	fakeRuntimeDir(t)

	stopped := time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)
	if err := writeLearned(learned{Iface: "eno2np1", Index: "2", At: stopped,
		Up: learnedRate{Max: 400, Rate: 100}, Down: learnedRate{Max: 400, Rate: 200}}); err != nil {
		t.Fatal(err)
	}

	ctl := newRunController(1000, Options{Params: DefaultParams(), MaxUp: 400, MaxDown: 400})
	resumeLearned(slog.New(&records{}), ctl, "eno2np1", "2", "boot-a", stopped.Add(time.Hour))

	if math.Abs(ctl.Up.Rate-250) > 0.01 || math.Abs(ctl.Down.Rate-300) > 0.01 {
		t.Errorf("an hour down resumed at %.2f/%.2f, want halfway back, 250/300", ctl.Up.Rate, ctl.Down.Rate)
	}
}

// count is how many records' messages contain text.
func (r *records) count(text string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	n := 0
	for i := range r.list {
		if strings.Contains(r.list[i].Message, text) {
			n++
		}
	}

	return n
}

func isName(e ast.Expr, name string) bool {
	ident, ok := e.(*ast.Ident)

	return ok && ident.Name == name
}

// THE BOOT IS THE KERNEL'S BOOT ID, trimmed, and nothing when it cannot be
// read, which leaves the index compared as within one boot. Not parallel:
// bootIDFile is the package's.
func TestTheBootIsTheKernelsBootID(t *testing.T) {
	old := bootIDFile
	t.Cleanup(func() { bootIDFile = old })

	bootIDFile = filepath.Join(t.TempDir(), "boot_id")
	if err := os.WriteFile(bootIDFile, []byte("0f3c9a1e-77b2-4d1c-9e52-6c1b2a8f4d10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := bootID(); got != "0f3c9a1e-77b2-4d1c-9e52-6c1b2a8f4d10" {
		t.Errorf("read the boot as %q", got)
	}

	bootIDFile = filepath.Join(t.TempDir(), "absent")
	if got := bootID(); got != "" {
		t.Errorf("an unreadable boot ID read as %q, want nothing", got)
	}
}
