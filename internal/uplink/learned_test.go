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

// A RESUMED DIRECTION IS WHERE IT WAS LEFT: the rate as it was, never past the
// ceiling now in force, Near of the line it found or under the floor, and the
// line it found. A rate at a ceiling since removed is resumed as it was, not at
// the interface's speed, and anything that is not a positive, finite number
// leaves the start untouched.
func TestAResumedDirectionIsWhereItWasLeft(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		learned          learnedRate
		ceiling          float64
		rate, limit      float64
		wantRate, wantLm float64
	}{
		"a rate and its line":           {learnedRate{Max: 1000, Rate: 300, Limit: 340}, 1000, 25, 0, 300, 340},
		"a ceiling lowered since":       {learnedRate{Max: 400, Rate: 300, Limit: 340}, 200, 25, 0, 200, 340},
		"at its ceiling, no line found": {learnedRate{Max: 1000, Rate: 1000}, 1000, 25, 0, 1000, 0},
		"at a ceiling since removed":    {learnedRate{Max: 150, Rate: 150}, 1000, 25, 0, 150, 0},
		"at its ceiling, a line found":  {learnedRate{Max: 150, Rate: 150, Limit: 160}, 1000, 25, 0, 144, 160},
		"past the line it found":        {learnedRate{Max: 1000, Rate: 520, Limit: 500}, 1000, 25, 0, 450, 500},
		"a record from before limits":   {learnedRate{Max: 400, Rate: 120}, 1000, 25, 0, 120, 0},
		"a rate under the floor":        {learnedRate{Max: 400, Rate: 2}, 400, 25, 0, Floor, 0},
		"no rate at all":                {learnedRate{}, 400, 25, 0, 25, 0},
		"a rate that is not a number":   {learnedRate{Max: 400, Rate: math.NaN()}, 400, 25, 0, 25, 0},
		"a rate that is infinite":       {learnedRate{Max: 400, Rate: math.Inf(1)}, 400, 25, 0, 25, 0},
		"a limit that is not a number":  {learnedRate{Max: 400, Rate: 120, Limit: math.NaN()}, 400, 25, 0, 120, 0},
		"a limit that is below nothing": {learnedRate{Max: 400, Rate: 120, Limit: -3}, 400, 25, 0, 120, 0},
	} {
		d := Direction{Max: c.ceiling, Rate: c.rate, Limit: c.limit}
		c.learned.resume(&d, DefaultParams().Near)

		if d.Rate != c.wantRate || d.Limit != c.wantLm || d.Max != c.ceiling {
			t.Errorf("%s: resumed at %.2f with the line at %.2f under %.0f, want %.2f with it at %.2f under %.0f",
				name, d.Rate, d.Limit, d.Max, c.wantRate, c.wantLm, c.ceiling)
		}
	}
}

// RATES LEARNED ON ANOTHER INTERFACE DESCRIBE ANOTHER LINE. WITHIN ONE
// BOOT the index decides, so another device under the name is refused; ACROSS A
// REBOOT, which renumbers interfaces, the name and the hardware address do; and
// a boot ID unknown on either side wants the name as well as the index.
func TestLearnedRatesResumeOnlyOnTheirInterface(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)
	ago := now.Add(-time.Minute)
	l := func(iface, index, addr, boot string) learned {
		return learned{Iface: iface, Index: index, Addr: addr, Boot: boot, At: ago}
	}
	const mac, other = "7c:c2:55:e2:9d:6d", "7c:c2:55:e2:9d:6e"

	for name, c := range map[string]struct {
		l    learned
		at   nic
		want bool
	}{
		"the same interface":              {l("eno2np1", "2", mac, "a"), nic{"eno2np1", "2", mac, "a"}, true},
		"the same interface renamed":      {l("eth0", "2", mac, "a"), nic{"eno2np1", "2", mac, "a"}, true},
		"another device under the name":   {l("eno2np1", "2", mac, "a"), nic{"eno2np1", "7", mac, "a"}, false},
		"by name, no index":               {l("eno2np1", "", "", "a"), nic{"eno2np1", "", "", "a"}, true},
		"another name, no index":          {l("eth0", "", "", "a"), nic{"eno2np1", "", "", "a"}, false},
		"after a reboot renumbered it":    {l("eno2np1", "2", mac, "a"), nic{"eno2np1", "3", mac, "b"}, true},
		"after a reboot, another name":    {l("eth0", "2", mac, "a"), nic{"eno2np1", "2", mac, "b"}, false},
		"after a reboot, another device":  {l("eno2np1", "2", mac, "a"), nic{"eno2np1", "2", other, "b"}, false},
		"after a reboot, no address read": {l("eno2np1", "2", mac, "a"), nic{"eno2np1", "3", "", "b"}, true},
		"no boot recorded, renumbered":    {l("eno2np1", "2", mac, ""), nic{"eno2np1", "3", mac, "b"}, false},
		"no boot read now, renumbered":    {l("eno2np1", "2", mac, "a"), nic{"eno2np1", "3", mac, ""}, false},
		"no boot read now, renamed":       {l("eth0", "2", mac, "a"), nic{"eno2np1", "2", mac, ""}, false},
		"no boot either side, the same":   {l("eno2np1", "2", mac, ""), nic{"eno2np1", "2", mac, ""}, true},
		"no index, another device":        {l("eno2np1", "", mac, "a"), nic{"eno2np1", "", other, "a"}, false},
		"no index, the same device":       {l("eno2np1", "", mac, "a"), nic{"eno2np1", "", mac, "a"}, true},
		"no index now, another device":    {l("eno2np1", "2", mac, "a"), nic{"eno2np1", "", other, "a"}, false},
		"no index then, another device":   {l("eno2np1", "", mac, "a"), nic{"eno2np1", "2", other, "a"}, false},
		"no index, no address now":        {l("eno2np1", "", mac, "a"), nic{"eno2np1", "", "", "a"}, true},
	} {
		if ok, why := c.l.resumes(c.at); ok != c.want || (!ok && why == "") {
			t.Errorf("%s: resumes %v (%q), want %v with a reason when not", name, ok, why, c.want)
		}
	}
}

// fakeRuntimeDir points the record and the learned rates at directories of the
// test's own, apart as they are on a host, and sysfs at one where eno2np1 has
// the address testMAC. Not parallel: StateFile, LearnedFile and sysNet are the
// package's.
func fakeRuntimeDir(t *testing.T) {
	t.Helper()

	oldState, oldLearned, oldNet := StateFile, LearnedFile, sysNet
	StateFile = filepath.Join(t.TempDir(), "interface")
	LearnedFile = filepath.Join(t.TempDir(), "learned")
	sysNet = t.TempDir()
	t.Cleanup(func() { StateFile, LearnedFile, sysNet = oldState, oldLearned, oldNet })

	setAddress(t, testMAC)
}

// testMAC is eno2np1's hardware address in the fake sysfs.
const testMAC = "7c:c2:55:e2:9d:6d"

// setAddress gives eno2np1 the hardware address addr in the fake sysfs.
func setAddress(t *testing.T, addr string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(sysNet, "eno2np1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysNet, "eno2np1", "address"), []byte(addr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// WHAT A RUN RECORDS IS WHAT THE NEXT ONE RESUMES, under the state directory
// rather than beside the interface record a reboot empties.
func TestTheNextRunResumesWhatThisOneRecorded(t *testing.T) {
	fakeRuntimeDir(t)

	log := slog.New(&records{})
	ctl := newRunController(1000, Options{Params: DefaultParams(), MaxUp: 400, MaxDown: 300})
	ctl.Up.Limit, ctl.Down.Limit = 135, 90

	rememberer(log, ctl, nic{"eno2np1", "2", testMAC, "boot-a"})(120, 80)

	if _, err := os.Stat(LearnedFile); err != nil {
		t.Fatalf("the learned rates are not at %s: %v", LearnedFile, err)
	}
	if filepath.Dir(LearnedFile) == filepath.Dir(StateFile) {
		t.Fatalf("the learned rates are beside the record %s, which a reboot empties", StateFile)
	}

	logs := &records{}
	next := newRunController(1000, Options{Params: DefaultParams(), MaxUp: 400, MaxDown: 300})
	resumeLearned(slog.New(logs), next, nic{"eno2np1", "2", "7c:c2:55:e2:9d:6d", "boot-a"})

	if next.Up.Rate != 120 || next.Down.Rate != 80 {
		t.Errorf("the next run started at %.1f/%.1f, want the 120/80 recorded", next.Up.Rate, next.Down.Rate)
	}
	// AND WHERE THE LINE WAS FOUND, which decides how fast the next run climbs.
	if next.Up.Limit != 135 || next.Down.Limit != 90 {
		t.Errorf("the next run found the line at %.1f/%.1f, want the 135/90 recorded", next.Up.Limit,
			next.Down.Limit)
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
	resumeLearned(slog.New(&records{}), rebooted, nic{"eno2np1", "3", "7c:c2:55:e2:9d:6d", "boot-b"})
	if rebooted.Up.Rate != 120 || rebooted.Down.Rate != 80 || rebooted.Up.Limit != 135 {
		t.Errorf("after a reboot renumbered eno2np1 the run started at %.1f/%.1f (line %.1f), want the "+
			"120/80 recorded and its line", rebooted.Up.Rate, rebooted.Down.Rate, rebooted.Up.Limit)
	}

	// BUT A REBOOT ONTO ANOTHER DEVICE UNDER THE NAME, by its hardware address,
	// starts as nothing learned: the record says which device set it.
	start := DefaultParams().Start
	swapped := newRunController(1000, Options{Params: DefaultParams(), MaxUp: 400, MaxDown: 300})
	resumeLearned(slog.New(&records{}), swapped, nic{"eno2np1", "3", "7c:c2:55:e2:9d:6e", "boot-b"})
	if swapped.Up.Rate != start || swapped.Down.Rate != start || swapped.Up.Limit != 0 {
		t.Errorf("a NIC swapped under eno2np1 across a reboot started at %.1f/%.1f (line %.1f), want %.0f "+
			"with no line", swapped.Up.Rate, swapped.Down.Rate, swapped.Up.Limit, start)
	}

	// ON ANOTHER INTERFACE, AS NOTHING LEARNED.
	other := newRunController(1000, Options{Params: DefaultParams(), MaxUp: 400, MaxDown: 300})
	resumeLearned(slog.New(&records{}), other, nic{"wlan0", "7", "", "boot-a"})
	if other.Up.Rate != start || other.Down.Rate != start || other.Down.Limit != 0 {
		t.Errorf("rates learned on eno2np1 were resumed on wlan0: %.1f/%.1f", other.Up.Rate, other.Down.Rate)
	}
}

// A RECORD THAT CANNOT BE READ STARTS AS NOTHING LEARNED AND SAYS SO, and none
// at all starts there quietly: low, never at the interface's speed.
func TestUnreadableLearnedRatesStartAsNothingLearned(t *testing.T) {
	fakeRuntimeDir(t)

	start := DefaultParams().Start
	logs := &records{}
	ctl := newRunController(1000, Options{Params: DefaultParams()})
	resumeLearned(slog.New(logs), ctl, nic{"eno2np1", "2", "", "boot-a"})
	if ctl.Up.Rate != start || ctl.Down.Rate != start || len(logs.list) != 0 {
		t.Fatalf("with nothing recorded the run started at %.1f/%.1f and said %d things, want %.0f quietly",
			ctl.Up.Rate, ctl.Down.Rate, len(logs.list), start)
	}

	if err := os.WriteFile(learnedFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	logs = &records{}
	resumeLearned(slog.New(logs), ctl, nic{"eno2np1", "2", "", "boot-a"})
	if ctl.Up.Rate != start || ctl.Down.Rate != start || logs.count("could not be read") != 1 {
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

	// THE SAME INTERFACE GOES TO BOTH, read once by thisNIC, its boot with it.
	var resumed, installed, recorded, looped, remembering, read token.Pos
	ast.Inspect(funcs["Run"], func(n ast.Node) bool {
		if assign, ok := n.(*ast.AssignStmt); ok && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 &&
			isName(assign.Lhs[0], "at") && types.ExprString(assign.Rhs[0]) == "thisNIC(iface)" {
			read = assign.Pos()
		}

		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		switch fun := call.Fun.(type) {
		case *ast.Ident:
			switch {
			case fun.Name == "resumeLearned" && len(call.Args) == 3 && isName(call.Args[1], "ctl") &&
				isName(call.Args[2], "at"):
				resumed = call.Pos()
			case fun.Name == "rememberer" && len(call.Args) == 3 && isName(call.Args[2], "at"):
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
	if !read.IsValid() || !remembering.IsValid() || read > resumed || remembering < resumed {
		t.Error("Run does not read the interface with thisNIC and hand it to resumeLearned and rememberer")
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

// HOWEVER LONG THE SHAPER WAS DOWN, AND WHATEVER THE CLOCK SAYS, A RESUMED RATE
// IS THE ONE LEARNED, through the same path a start takes. One that returned
// toward the interface's speed with the time down started a host back from a day
// off at a gigabit, on a line it had found full at a third of that; a line that
// changed meanwhile is found again as any is. Not parallel: StateFile is the
// package's.
func TestAResumeKeepsTheRateHoweverLongTheShaperWasDown(t *testing.T) {
	fakeRuntimeDir(t)

	at := nic{"eno2np1", "2", testMAC, "boot-a"}
	for name, stopped := range map[string]time.Time{
		"a day ago":           time.Now().Add(-24 * time.Hour),
		"a month ago":         time.Now().Add(-30 * 24 * time.Hour),
		"ahead of this clock": time.Now().Add(time.Hour),
		"at no time":          {},
	} {
		if err := writeLearned(learned{Format: learnedFormat, Iface: "eno2np1", Index: "2", Boot: "boot-a", At: stopped,
			Up:   learnedRate{Max: 1000, Rate: 100, Limit: 120},
			Down: learnedRate{Max: 1000, Rate: 200, Limit: 230}}); err != nil {
			t.Fatal(err)
		}

		ctl := newRunController(1000, Options{Params: DefaultParams()})
		resumeLearned(slog.New(&records{}), ctl, at)

		if ctl.Up.Rate != 100 || ctl.Down.Rate != 200 || ctl.Up.Limit != 120 || ctl.Down.Limit != 230 {
			t.Errorf("%s: resumed at %.2f/%.2f with the line at %.2f/%.2f, want 100/200 and 120/230",
				name, ctl.Up.Rate, ctl.Down.Rate, ctl.Up.Limit, ctl.Down.Limit)
		}
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

// A RECORD THE SHAPER BEFORE THE LIMIT WROTE IS NOTHING LEARNED, said once:
// that shaper started each run at its ceiling and recorded it before measuring
// anything, and returned a rate toward it for every hour it was down, so its
// 894 of 1000 is a host three hours off after a cut to 150, and resumed it would
// install CAKE holding nothing back. And a record that cannot be read, or
// cannot be read at all, starts as nothing learned and says so.
func TestARecordFromBeforeTheLimitIsNothingLearned(t *testing.T) {
	fakeRuntimeDir(t)

	at := nic{"eno2np1", "2", testMAC, "boot-a"}
	start := DefaultParams().Start

	if err := os.WriteFile(LearnedFile, []byte(`{"interface":"eno2np1","index":"2","boot":"boot-a",`+
		`"up":{"max_mbit":1000,"rate_mbit":893.75},"down":{"max_mbit":1000,"rate_mbit":1000},"at":"`+
		time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	logs := &records{}
	old := newRunController(1000, Options{Params: DefaultParams()})
	resumeLearned(slog.New(logs), old, at)
	if old.Up.Rate != start || old.Down.Rate != start || logs.count("in a format this shaper does not resume") != 1 {
		t.Fatalf("a record from before the Limit started the run at %.1f/%.1f, said %d times; want %.0f, said",
			old.Up.Rate, old.Down.Rate, logs.count("in a format this shaper does not resume"), start)
	}

	// AND WHAT THIS SHAPER WRITES IS IN ITS FORMAT, so the next run resumes it.
	old.Up.Limit = 140
	rememberer(slog.New(&records{}), old, at)(120, 80)
	next := newRunController(1000, Options{Params: DefaultParams()})
	resumeLearned(slog.New(&records{}), next, at)
	if next.Up.Rate != 120 || next.Down.Rate != 80 || next.Up.Limit != 140 {
		t.Fatalf("the record this shaper wrote resumed at %.1f/%.1f with the line at %.1f, want 120/80 and 140",
			next.Up.Rate, next.Down.Rate, next.Up.Limit)
	}

	if err := os.WriteFile(LearnedFile, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs = &records{}
	garbled := newRunController(1000, Options{Params: DefaultParams()})
	resumeLearned(slog.New(logs), garbled, at)
	if garbled.Up.Rate != start || garbled.Down.Rate != start || logs.count("could not be read") != 1 {
		t.Errorf("an undecodable record started the run at %.1f/%.1f", garbled.Up.Rate, garbled.Down.Rate)
	}

	// NOR ONE THAT CANNOT BE READ AT ALL, here a directory where the file goes.
	if err := os.Remove(LearnedFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(LearnedFile, 0o755); err != nil {
		t.Fatal(err)
	}
	logs = &records{}
	unreadable := newRunController(1000, Options{Params: DefaultParams()})
	resumeLearned(slog.New(logs), unreadable, at)
	if unreadable.Up.Rate != start || unreadable.Down.Rate != start || logs.count("could not be read") != 1 {
		t.Errorf("a record that could not be read started the run at %.1f/%.1f",
			unreadable.Up.Rate, unreadable.Down.Rate)
	}
}

// THE RATES ARE INSTALLED THROUGH durablefile, whose one ordering syncs the
// file and the directory that names it: a power cut, the case they are kept
// across a reboot for, then leaves the last whole record. A sync cannot be
// observed from here, so this reads writeLearned.
func TestTheLearnedRatesAreInstalledDurably(t *testing.T) {
	t.Parallel()

	parsed, err := parser.ParseFile(token.NewFileSet(), "learned.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var write *ast.FuncDecl
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "writeLearned" {
			write = fn
		}
	}
	if write == nil {
		t.Fatal("no writeLearned in learned.go")
	}

	calls := map[string]bool{}
	ast.Inspect(write, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			calls[types.ExprString(call.Fun)] = true
		}

		return true
	})

	for _, want := range []string{"installer.MkdirAll", "installer.Install"} {
		if !calls[want] {
			t.Errorf("writeLearned does not call %s", want)
		}
	}
	for _, unsafe := range []string{"os.WriteFile", "os.Rename", "os.Create", "os.OpenFile"} {
		if calls[unsafe] {
			t.Errorf("writeLearned calls %s, which installs nothing durably", unsafe)
		}
	}
}

// THE ADDRESS RECORDED IS THE ONE THE INTERFACE HAS AT THE WRITE, so a MAC set
// while the shaper runs is the one the next boot compares, not the one it
// started with.
func TestTheAddressRecordedIsTheOneAtTheWrite(t *testing.T) {
	fakeRuntimeDir(t)

	ctl := newRunController(1000, Options{Params: DefaultParams(), MaxUp: 400, MaxDown: 300})
	remember := rememberer(slog.New(&records{}), ctl, nic{"eno2np1", "2", testMAC, "boot-a"})

	// EACH WRITE IS A WRITE, its rates new, and its address as the case says.
	rate := 100.0
	write := func(t *testing.T, want, why string) {
		t.Helper()

		rate++
		remember(rate, rate)

		l, found, err := readLearned()
		if err != nil || !found || l.Up.Rate != rate {
			t.Fatalf("%s: the write did not land (%+v, %v, %v)", why, l, found, err)
		}
		if l.Addr != want {
			t.Errorf("%s: recorded the address %q, want %q", why, l.Addr, want)
		}
	}

	// EACH WRITE READS IT AGAIN, while the name still has the run's index.
	setIndex(t, "2")
	for _, mac := range []string{"02:00:00:aa:bb:cc", "02:00:00:dd:ee:ff"} {
		setAddress(t, mac)
		write(t, mac, "the address at this write")
	}

	// ANOTHER DEVICE UNDER THE NAME, by its index: the last address this device
	// had stays beside its rates.
	setIndex(t, "7")
	setAddress(t, "02:00:00:11:22:33")
	write(t, "02:00:00:dd:ee:ff", "another device under the name")

	// THE INDEX UNREADABLE, the address readable: nothing says it is this device.
	if err := os.Remove(filepath.Join(sysNet, "eno2np1", "ifindex")); err != nil {
		t.Fatal(err)
	}
	write(t, "02:00:00:dd:ee:ff", "an index that cannot be read")

	// THE ADDRESS UNREADABLE UNDER THE RUN'S OWN INDEX: nothing is not an
	// address.
	if err := os.Remove(filepath.Join(sysNet, "eno2np1", "address")); err != nil {
		t.Fatal(err)
	}
	setIndex(t, "2")
	write(t, "02:00:00:dd:ee:ff", "an address that cannot be read")

	// A RUN THAT COULD NOT READ ITS INDEX AT THE START has nothing to tell its
	// own device by, and keeps the address it started with.
	blind := rememberer(slog.New(&records{}), ctl, nic{"eno2np1", "", testMAC, "boot-a"})
	setAddress(t, "02:00:00:44:55:66")
	blind(150, 150)
	if l, found, err := readLearned(); err != nil || !found || l.Up.Rate != 150 || l.Addr != testMAC {
		t.Errorf("a run with no index recorded %+v (%v, %v), want its starting address %q", l, found, err, testMAC)
	}

	// EVEN WHEN THE INDEX STILL CANNOT BE READ, so "none" equals "none": that is
	// no proof the device is the same one.
	if err := os.Remove(filepath.Join(sysNet, "eno2np1", "ifindex")); err != nil {
		t.Fatal(err)
	}
	setAddress(t, "02:00:00:77:88:99")
	blind(160, 160)
	if l, found, err := readLearned(); err != nil || !found || l.Up.Rate != 160 || l.Addr != testMAC {
		t.Errorf("a run with no index, its index still unreadable, recorded %+v (%v, %v), want %q", l, found,
			err, testMAC)
	}
}

// setIndex gives eno2np1 the kernel index index in the fake sysfs.
func setIndex(t *testing.T, index string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(sysNet, "eno2np1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysNet, "eno2np1", "ifindex"), []byte(index+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// THE HARDWARE ADDRESS IS SYSFS'S, trimmed, and nothing for a virtual device's
// all-zero one or one that cannot be read. Not parallel: sysNet is the package's.
func TestTheHardwareAddressIsSysfss(t *testing.T) {
	old := sysNet
	t.Cleanup(func() { sysNet = old })
	sysNet = t.TempDir()

	for iface, addr := range map[string]string{"eno2np1": "7c:c2:55:e2:9d:6d\n", "lo": "00:00:00:00:00:00\n"} {
		if err := os.MkdirAll(filepath.Join(sysNet, iface), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sysNet, iface, "address"), []byte(addr), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for iface, want := range map[string]string{"eno2np1": "7c:c2:55:e2:9d:6d", "lo": "", "absent": ""} {
		if got := ifaddr(iface); got != want {
			t.Errorf("%s's address read as %q, want %q", iface, got, want)
		}
	}
}

// THE UNIT MAKES THE DIRECTORY THE RATES ARE KEPT IN: the host role's unit
// declares StateDirectory for exactly the directory defaultLearnedFile names,
// so a reboot keeps them, and the RuntimeDirectory the interface record lives
// in is another.
func TestTheUnitMakesTheDirectoryTheRatesAreKeptIn(t *testing.T) {
	t.Parallel()

	unit, err := os.ReadFile("../../ansible_collections/junioryono/billet/roles/host/templates/billet-uplink.service.j2")
	if err != nil {
		t.Fatal(err)
	}

	var state, runtime []string
	for _, line := range strings.Split(string(unit), "\n") {
		if v, ok := strings.CutPrefix(line, "StateDirectory="); ok {
			state = append(state, v)
		}
		if v, ok := strings.CutPrefix(line, "RuntimeDirectory="); ok {
			runtime = append(runtime, v)
		}
	}

	if len(state) != 1 || "/var/lib/"+state[0] != filepath.Dir(defaultLearnedFile) {
		t.Errorf("the unit declares StateDirectory %q, want exactly the directory of %s", state, defaultLearnedFile)
	}
	if len(runtime) != 1 || "/run/"+runtime[0] == filepath.Dir(defaultLearnedFile) {
		t.Errorf("the unit's RuntimeDirectory %q holds the learned rates a reboot must keep", runtime)
	}
}
