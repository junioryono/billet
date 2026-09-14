package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/state"
)

// THE ENDPOINT MIGRATION (fixtures-c4b.md, section A): the command decides
// from the running node's record against the installed configuration and
// the rendering, stops under its own deadline with only its subprocess
// signalled, proves the stop by the command's own triple, starts, waits for
// the record under the new invocation, and closes every successful answer
// against the configuration and the process it rests on.

// endpointFixture is the registration fixture with a certless node, its
// deployment minted in the node's state directory, and a fake systemctl that
// answers show from per-unit files and stop and start from mode files,
// recording every argv.
type endpointFixture struct {
	*registrationFixture
	deployment string
	logPath    string
	nodeState  string
}

const (
	endpointA = "127.0.0.1:7717"
	endpointB = "127.0.0.1:7719"
	// The canonical texts the representation prints for a certless node.
	canonicalA     = "http://127.0.0.1:7717"
	canonicalB     = "http://127.0.0.1:7719"
	newInvocation  = "abcdefabcdefabcdefabcdefabcdefab"
	newIncarnation = "ffeeddccbbaa99887766554433221100"
)

func newEndpointFixture(t *testing.T) *endpointFixture {
	t.Helper()

	// The registration fixture's own directory holds the node's state, so
	// the deployment is minted there once the fixture exists and the node
	// configuration is installed over the placeholder.
	base := newRegistrationFixture(t, "node:\n  name: node-a\n")
	nodeState := filepath.Join(base.dir, "node-state")

	deployment, err := state.DeploymentID(nodeState)
	if err != nil {
		t.Fatal(err)
	}

	f := &endpointFixture{registrationFixture: base, deployment: deployment, nodeState: nodeState}
	f.writeConfig(t, f.nodeOnlyConfig())
	f.touchBeforeStart(t, f.configPath)
	f.logPath = filepath.Join(f.dir, "systemctl.log")
	t.Setenv("BILLET_FAKE_LOG", f.logPath)
	t.Setenv("BILLET_FAKE_RECORD", f.recordPath)
	t.Setenv("BILLET_FAKE_CONFIG", f.configPath)

	// THE FAKE SYSTEMCTL: show from the unit files as the inspector's fake
	// does; stop and start per mode file, each applying the unit's after-state
	// and, after a start, publishing (or removing) the record the fixture
	// staged for it.
	writeFile(t, systemctlBinary, `#!/bin/sh
echo "$*" >> "$BILLET_FAKE_LOG"
cmd=""
unit=""
names=""
for a in "$@"; do case "$a" in --property=*) names="$names ${a#--property=}";; --) ;; show|stop|start) cmd=$a;; *) unit=$a;; esac; done
case "$cmd" in
  show)
    file="$BILLET_FAKE_UNITS/$unit"
    if [ "$unit" = billet-node.service ]; then
      count=$(grep -c '^show' "$BILLET_FAKE_LOG")
      if [ -f "$BILLET_FAKE_UNITS/after.n" ] && [ "$count" -gt "$(cat "$BILLET_FAKE_UNITS/after.n")" ]; then file="$BILLET_FAKE_UNITS/after.body"; fi
      if [ -f "$BILLET_FAKE_UNITS/until.n" ] && [ "$count" -gt "$(cat "$BILLET_FAKE_UNITS/until.n")" ]; then file="$BILLET_FAKE_UNITS/$unit"; fi
      if [ -f "$BILLET_FAKE_UNITS/every.n" ] && [ $((count % $(cat "$BILLET_FAKE_UNITS/every.n"))) -eq 0 ]; then file="$BILLET_FAKE_UNITS/after.body"; fi
      if [ -f "$BILLET_FAKE_UNITS/alternate.body" ] && [ $((count % 2)) -eq 0 ]; then file="$BILLET_FAKE_UNITS/alternate.body"; fi
    fi
    for n in $names; do grep "^$n=" "$file"; done
    exit 0 ;;
  stop)
    mode=$(cat "$BILLET_FAKE_UNITS/stop.mode" 2>/dev/null || echo ok)
    case "$mode" in
      hang) exec sleep 30 ;;
      hang-ignoring-term) trap '' TERM; exec sleep 30 ;;
      fail) exit 1 ;;
      replace-config) cp "$BILLET_FAKE_CONFIG" "$BILLET_FAKE_CONFIG.new" && mv "$BILLET_FAKE_CONFIG.new" "$BILLET_FAKE_CONFIG" ;;
    esac
    [ -f "$BILLET_FAKE_UNITS/$unit.after-stop" ] && cp "$BILLET_FAKE_UNITS/$unit.after-stop" "$BILLET_FAKE_UNITS/$unit"
    exit 0 ;;
  start)
    mode=$(cat "$BILLET_FAKE_UNITS/start.mode" 2>/dev/null || echo ok)
    [ "$mode" = fail ] && exit 1
    [ -f "$BILLET_FAKE_UNITS/$unit.after-start" ] && cp "$BILLET_FAKE_UNITS/$unit.after-start" "$BILLET_FAKE_UNITS/$unit"
    [ -f "$BILLET_FAKE_UNITS/record.remove" ] && rm -f "$BILLET_FAKE_RECORD"
    [ -f "$BILLET_FAKE_UNITS/record.after-start" ] && cp "$BILLET_FAKE_UNITS/record.after-start" "$BILLET_FAKE_RECORD"
    exit 0 ;;
esac
exit 1
`, 0o755)

	prevPoll := endpointPoll
	endpointPoll = 10 * time.Millisecond

	t.Cleanup(func() { endpointPoll = prevPoll })

	f.setNode(t, "active", "running", inspectPID, nodeInvocation, "mixed")
	f.writeRecord(t, f.record(map[string]any{"deployment": deployment, "endpoint": canonicalA}))

	return f
}

// nodeUnitBody renders the node unit's properties the endpoint commands read.
func nodeUnitBody(active, sub string, pid int, invocation, killMode, result string) string {
	return "LoadState=loaded\nUnitFileState=enabled\nActiveState=" + active + "\nSubState=" + sub + "\nMainPID=" +
		strconv.Itoa(pid) + "\nInvocationID=" + invocation + "\nKillMode=" + killMode + "\nResult=" + result +
		"\nStateChangeTimestamp=Tue 2026-09-09 12:00:00 UTC\nNeedDaemonReload=no\nExecMainStartTimestamp=\n" +
		"NRestarts=0\nExecMainStartTimestampMonotonic=" + strconv.Itoa(pid*1000) + "\nEnvironmentFiles=\nEnvironment=\n"
}

func (f *endpointFixture) unitFile(name string) string { return filepath.Join(f.unitsDir, name) }

func (f *endpointFixture) setNode(t *testing.T, active, sub string, pid int, invocation, killMode string) {
	t.Helper()
	writeFile(t, f.unitFile(nodeUnit), nodeUnitBody(active, sub, pid, invocation, killMode, "success"), 0o644)
}

func (f *endpointFixture) afterStop(t *testing.T, active, sub, result string) {
	t.Helper()
	writeFile(t, f.unitFile(nodeUnit+".after-stop"), nodeUnitBody(active, sub, 0, nodeInvocation, "mixed", result), 0o644)
}

func (f *endpointFixture) afterStart(t *testing.T) {
	t.Helper()
	writeFile(t, f.unitFile(nodeUnit+".after-start"), nodeUnitBody("active", "running", inspectPID+1, newInvocation, "mixed", "success"), 0o644)
}

func (f *endpointFixture) recordAfterStart(t *testing.T, endpoint string) {
	t.Helper()

	body, err := json.Marshal(f.record(map[string]any{"deployment": f.deployment, "endpoint": endpoint,
		"invocation_id": newInvocation, "incarnation": newIncarnation}))
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, f.unitFile("record.after-start"), string(body)+"\n", 0o600)
}

// recordRemovedAtStart makes the fake's start remove the record, so the
// started node has published nothing.
func (f *endpointFixture) recordRemovedAtStart(t *testing.T) {
	t.Helper()
	writeFile(t, f.unitFile("record.remove"), "", 0o644)
}

// rBinary makes the managed binary and the running process's image one
// script that answers the preparation's dry run, so the process is an R
// release and never pre-R.
func (f *endpointFixture) rBinary(t *testing.T) {
	t.Helper()

	script := "#!/bin/sh\nprintf '{\"outcome\":\"reported\"}\\n'\n"
	writeFile(t, installedBinary, script, 0o755)
	writeFile(t, filepath.Join(f.procDir, strconv.Itoa(inspectPID), "image"), script, 0o755)
}

// afterShows makes every show of the node unit after the n-th answer body
// instead of the unit file: the closing observation of a command whose
// earlier observations agreed.
func (f *endpointFixture) afterShows(t *testing.T, n int, body string) {
	t.Helper()
	writeFile(t, f.unitFile("after.n"), strconv.Itoa(n)+"\n", 0o644)
	writeFile(t, f.unitFile("after.body"), body, 0o644)
}

// everyShow makes every n-th show of the node unit answer body: with the
// judgement's four shows per attempt, n = 4 makes each attempt's closing
// observation see another process, so the judgement keeps moving.
func (f *endpointFixture) everyShow(t *testing.T, n int, body string) {
	t.Helper()
	writeFile(t, f.unitFile("every.n"), strconv.Itoa(n)+"\n", 0o644)
	writeFile(t, f.unitFile("after.body"), body, 0o644)
}

// showsBetween makes the shows after the n-th and up to the m-th answer
// body, the unit file again afterwards: one movement, then stability.
func (f *endpointFixture) showsBetween(t *testing.T, n, m int, body string) {
	t.Helper()
	f.afterShows(t, n, body)
	writeFile(t, f.unitFile("until.n"), strconv.Itoa(m)+"\n", 0o644)
}

// afterCall runs fn once, after the fake recorded its first call of verb
// (a start, say), polling the log until the test ends; joined at cleanup.
func (f *endpointFixture) afterCall(t *testing.T, verb string, d time.Duration, fn func()) {
	t.Helper()

	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)

		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}

			body, err := os.ReadFile(f.logPath)
			if err == nil && strings.Contains(string(body), verb+" ") {
				break
			}
		}

		select {
		case <-time.After(d):
			fn()
		case <-stop:
		}
	}()

	t.Cleanup(func() {
		close(stop)
		<-done
	})
}

// alternating makes every even show of the node unit answer body, so a
// bracket's two observations never agree.
func (f *endpointFixture) alternating(t *testing.T, body string) {
	t.Helper()
	writeFile(t, f.unitFile("alternate.body"), body, 0o644)
}

func (f *endpointFixture) stopMode(t *testing.T, mode string) {
	t.Helper()
	writeFile(t, f.unitFile("stop.mode"), mode+"\n", 0o644)
}

func (f *endpointFixture) startMode(t *testing.T, mode string) {
	t.Helper()
	writeFile(t, f.unitFile("start.mode"), mode+"\n", 0o644)
}

// rendering is the node-only configuration naming addr.
func (f *endpointFixture) rendering(addr string) string {
	return strings.Replace(f.nodeOnlyConfig(), "server_addr: "+endpointA, "server_addr: \""+addr+"\"", 1)
}

// installB installs the rendering naming B, as the role's render does.
func (f *endpointFixture) installB(t *testing.T) {
	t.Helper()
	f.writeConfig(t, f.rendering(endpointB))
}

// systemctlCalls is the fake's argv log, one call per line.
func (f *endpointFixture) systemctlCalls(t *testing.T) []string {
	t.Helper()

	body, err := os.ReadFile(f.logPath)
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		t.Fatal(err)
	}

	return strings.Split(strings.TrimSpace(string(body)), "\n")
}

func (f *endpointFixture) calls(t *testing.T, verb string) int {
	t.Helper()

	n := 0

	for _, c := range f.systemctlCalls(t) {
		if strings.HasPrefix(c, verb+" ") {
			n++
		}
	}

	return n
}

// endpointOut is a command's printed answer and its exit.
type endpointOut struct {
	doc  map[string]any
	raw  string
	code int
	err  error
}

func (o endpointOut) str(key string) string { return asString(o.doc[key]) }

func (o endpointOut) boolean(key string) bool { return asBool(o.doc[key]) }

// runEndpoint runs one of the endpoint commands with the rendering on stdin
// and decodes its answer.
func runEndpoint(t *testing.T, fn func(context.Context, []string) error, stdin string, args ...string) endpointOut {
	t.Helper()

	prev := renderingStdin
	renderingStdin = strings.NewReader(stdin)

	t.Cleanup(func() { renderingStdin = prev })

	var err error

	out := capture(t, func() { err = fn(t.Context(), append(args, "--json")) })

	o := endpointOut{err: err, raw: out}
	if err != nil {
		o.code = exitStatus(err)
	}

	if strings.TrimSpace(out) == "" {
		if err == nil {
			t.Fatalf("%v printed nothing and returned nil", args)
		}

		return o
	}

	if uerr := json.Unmarshal([]byte(out), &o.doc); uerr != nil {
		t.Fatalf("%v printed something that is not JSON: %v\n%s", args, uerr, out)
	}

	return o
}

func (f *endpointFixture) migrate(t *testing.T, stdin string, extra ...string) endpointOut {
	t.Helper()

	args := append([]string{"--config", f.configPath, "--wait", "200ms"}, extra...)
	if stdin != "" {
		args = append(args, "--desired", "-")
	}

	return runEndpoint(t, cmdNodeMigrate, stdin, args...)
}

func mustEndpointOutcome(t *testing.T, o endpointOut, outcome string) {
	t.Helper()

	if o.str("outcome") != outcome || o.code != 0 {
		t.Fatalf("outcome %q code %d (reason %q why %q), want %s", o.str("outcome"), o.code, o.str("reason"),
			o.str("why"), outcome)
	}
}

func mustEndpointRefusal(t *testing.T, o endpointOut, outcome, reason string) {
	t.Helper()

	want := exitRefused
	if outcome == outcomeUnknown {
		want = exitUnknown
	}

	if o.str("outcome") != outcome || o.str("reason") != reason || o.code != want {
		t.Fatalf("outcome %q reason %q code %d (why %q), want %s/%s/%d", o.str("outcome"), o.str("reason"), o.code,
			o.str("why"), outcome, reason, want)
	}
}

// A1: unchanged, in another spelling, no stop; a rooted name is not the
// unrooted one; a node not running is unchanged with effective null; an
// inactive node after a change is unchanged too (the ordinary start follows).
func TestMigrateAnswersUnchangedWithoutTouchingTheNode(t *testing.T) {
	t.Run("another spelling", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.writeConfig(t, f.rendering("[::1]:7717"))
		f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": "http://[::1]:7717"}))

		o := f.migrate(t, f.rendering("[0:0:0:0:0:0:0:1]:7717"))
		mustEndpointOutcome(t, o, outcomeUnchanged)

		if o.str("effective") != "http://[::1]:7717" || o.str("endpoint") != "http://[::1]:7717" || o.str("record") != recordCurrent {
			t.Errorf("answer %v", o.doc)
		}

		if o.str("node") != "node-a" || o.str("deployment") != f.deployment {
			t.Errorf("identity %v", o.doc)
		}

		if f.calls(t, "stop") != 0 || f.calls(t, "start") != 0 {
			t.Errorf("an unchanged endpoint stopped or started: %v", f.systemctlCalls(t))
		}
	})

	t.Run("not running", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.setNode(t, "inactive", "dead", 0, "", "mixed")

		o := f.migrate(t, f.rendering(endpointA))
		mustEndpointOutcome(t, o, outcomeUnchanged)

		if o.doc["effective"] != nil || o.str("record") != recordNone {
			t.Errorf("answer %v", o.doc)
		}
	})

	t.Run("inactive after a change", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.setNode(t, "inactive", "dead", 0, "", "mixed")
		f.installB(t)

		o := f.migrate(t, f.rendering(endpointB))
		mustEndpointOutcome(t, o, outcomeUnchanged)

		if o.doc["effective"] != nil || o.str("endpoint") != canonicalB || f.calls(t, "stop") != 0 {
			t.Errorf("answer %v calls %v", o.doc, f.systemctlCalls(t))
		}
	})
}

// A2: the first observation. A unit still stopping refuses whatever the
// endpoints say, whatever LoadState says, in a dry run too; a failed show or
// one without ActiveState is could-not-tell; nothing is signalled.
func TestMigrateRefusesAUnitStillStopping(t *testing.T) {
	for _, c := range []struct {
		name     string
		load     string
		rendered string
		dryRun   bool
	}{
		{"a changed endpoint", "loaded", endpointB, false},
		{"an unchanged endpoint", "loaded", endpointA, false},
		{"a fragment gone", "not-found", endpointB, false},
		{"a dry run", "loaded", endpointB, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Helper()
			f := newEndpointFixture(t)
			body := strings.Replace(nodeUnitBody("deactivating", "stop-sigterm", inspectPID, nodeInvocation, "mixed", "success"),
				"LoadState=loaded", "LoadState="+c.load, 1)
			writeFile(t, f.unitFile(nodeUnit), body, 0o644)

			var extra []string
			if c.dryRun {
				extra = []string{"--dry-run"}
			}

			o := f.migrate(t, f.rendering(c.rendered), extra...)
			mustEndpointRefusal(t, o, outcomeRefused, endpointReasonStopping)

			if !strings.Contains(o.str("why"), "2026-09-09 12:00:00") || o.str("state") != stateNothing {
				t.Errorf("why %q state %q", o.str("why"), o.str("state"))
			}

			if calls := f.systemctlCalls(t); len(calls) != 1 || !strings.HasPrefix(calls[0], "show") {
				t.Errorf("calls %v, want the one observation", calls)
			}
		})
	}

	t.Run("a show without ActiveState", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		writeFile(t, f.unitFile(nodeUnit), "LoadState=loaded\nMainPID=4242\n", 0o644)

		o := f.migrate(t, f.rendering(endpointA))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonUnit)
	})

	t.Run("a failed show", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		mustOK(t, os.Remove(f.unitFile(nodeUnit)))

		o := f.migrate(t, f.rendering(endpointA))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonUnit)
	})

	// A2f-h: a MainPID missing, malformed or zero beside an active unit is
	// uncertainty, never "not running".
	for _, c := range []struct{ name, body string }{
		{"MainPID absent", "LoadState=loaded\nActiveState=active\nSubState=running\nInvocationID=" + nodeInvocation + "\nKillMode=mixed\n"},
		{"MainPID malformed", "LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=x\nInvocationID=" + nodeInvocation + "\nKillMode=mixed\n"},
		{"MainPID zero while active", "LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=0\nInvocationID=" + nodeInvocation + "\nKillMode=mixed\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Helper()
			f := newEndpointFixture(t)
			writeFile(t, f.unitFile(nodeUnit), c.body, 0o644)

			o := f.migrate(t, f.rendering(endpointB), "--dry-run")
			mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonUnit)

			if f.calls(t, "stop") != 0 {
				t.Error("an uncertain MainPID stopped the node")
			}
		})
	}
}

// A3: the dry run reports the plan from the running node and the installed
// configuration against the rendering, and stops nothing.
func TestMigrateDryRunReportsThePlan(t *testing.T) {
	type want struct {
		planned             bool
		from, to, effective string
		record              string
		firstStart, removed bool
	}

	cases := []struct {
		name    string
		prepare func(t *testing.T, f *endpointFixture)
		stdin   func(f *endpointFixture) string
		want    want
	}{
		{"a change against the running node", nil, func(f *endpointFixture) string { return f.rendering(endpointB) },
			want{planned: true, from: canonicalA, to: canonicalB, effective: canonicalA, record: recordCurrent}},
		{"an inactive node with a change", func(t *testing.T, f *endpointFixture) {
			t.Helper()
			f.setNode(t, "inactive", "dead", 0, "", "mixed")
		}, func(f *endpointFixture) string { return f.rendering(endpointB) },
			want{planned: true, from: canonicalA, to: canonicalB, record: recordNone}},
		{"an unchanged endpoint", nil, func(f *endpointFixture) string { return f.rendering(endpointA) },
			want{from: canonicalA, to: canonicalA, effective: canonicalA, record: recordCurrent}},
		{"the render already installed, the node still on A", func(t *testing.T, f *endpointFixture) {
			t.Helper()
			f.installB(t)
		}, func(f *endpointFixture) string { return f.rendering(endpointB) },
			want{planned: true, from: canonicalA, to: canonicalB, effective: canonicalA, record: recordCurrent}},
		{"the node already on B, the installed still A", func(t *testing.T, f *endpointFixture) {
			t.Helper()
			f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalB}))
		}, func(f *endpointFixture) string { return f.rendering(endpointB) },
			want{planned: true, from: canonicalB, to: canonicalB, effective: canonicalB, record: recordCurrent}},
		{"everything on B", func(t *testing.T, f *endpointFixture) {
			t.Helper()
			f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalB}))
			f.installB(t)
		}, func(f *endpointFixture) string { return f.rendering(endpointB) },
			want{from: canonicalB, to: canonicalB, effective: canonicalB, record: recordCurrent}},
		{"without a rendering, the node on A and B installed", func(t *testing.T, f *endpointFixture) {
			t.Helper()
			f.installB(t)
		}, func(*endpointFixture) string { return "" },
			want{planned: true, from: canonicalA, effective: canonicalA, record: recordCurrent}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Helper()
			f := newEndpointFixture(t)

			if c.prepare != nil {
				c.prepare(t, f)
			}

			o := f.migrate(t, c.stdin(f), "--dry-run")
			mustEndpointOutcome(t, o, outcomeReported)

			got := want{planned: o.boolean("planned"), from: o.str("from"), to: o.str("to"), effective: o.str("effective"),
				record: o.str("record"), firstStart: o.boolean("first_start"), removed: o.boolean("node_removed")}
			if got != c.want {
				t.Errorf("reported %+v, want %+v", got, c.want)
			}

			if o.str("config") != "present" || asMap(o.doc["unit"])["kill_mode"] != "mixed" {
				t.Errorf("config %q unit %v", o.str("config"), o.doc["unit"])
			}

			if f.calls(t, "stop") != 0 || f.calls(t, "start") != 0 {
				t.Errorf("a dry run stopped or started: %v", f.systemctlCalls(t))
			}
		})
	}
}

// A4: the policy and the unit, before any stop.
func TestMigrateRefusesAKillModeUnderWhichAStopProvesNothing(t *testing.T) {
	f := newEndpointFixture(t)
	f.setNode(t, "active", "running", inspectPID, nodeInvocation, "process")
	f.installB(t)

	o := f.migrate(t, f.rendering(endpointB))
	mustEndpointRefusal(t, o, outcomeRefused, endpointReasonPolicy)

	if f.calls(t, "stop") != 0 {
		t.Error("a refused policy stopped the node")
	}

	f = newEndpointFixture(t)
	body := strings.Replace(nodeUnitBody("active", "running", inspectPID, nodeInvocation, "mixed", "success"),
		"LoadState=loaded", "LoadState=not-found", 1)
	writeFile(t, f.unitFile(nodeUnit), body, 0o644)
	f.installB(t)

	o = f.migrate(t, f.rendering(endpointB))
	mustEndpointRefusal(t, o, outcomeRefused, endpointReasonUnit)
}

// A5: the deadline. A stop that hangs ends when the migration's deadline
// expires, by TERM and, when it is ignored, by KILL after the wait delay;
// nothing is installed or started, exactly one stop was recorded, and a clean
// observation after the expiry is still no proof.
func TestMigrateStopDeadlineSignalsOnlyItsOwnSubprocess(t *testing.T) {
	prev := guardWaitDelay
	guardWaitDelay = 500 * time.Millisecond

	t.Cleanup(func() { guardWaitDelay = prev })

	for _, c := range []struct{ name, mode, after string }{
		{"a hang", "hang", "deactivating"},
		{"a hang ignoring TERM", "hang-ignoring-term", "deactivating"},
		{"a clean observation after the expiry", "hang", "inactive"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Helper()
			f := newEndpointFixture(t)
			f.installB(t)
			f.stopMode(t, c.mode)
			f.afterStart(t)

			// The unit's state after the expiry: the fake's stop never applied
			// an after-state (it hung), so the file is rewritten here to what
			// the post-expiry observation should see.
			f.afterCall(t, "stop", 300*time.Millisecond, func() {
				writeFile(t, f.unitFile(nodeUnit), nodeUnitBody(c.after, "dead", 0, nodeInvocation, "mixed", "success"), 0o644)
			})

			started := time.Now()

			o := f.migrate(t, f.rendering(endpointB), "--stop-timeout", "1s")

			mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonUnproved)

			if o.str("state") != stateNothing || !strings.Contains(o.str("why"), "deadline") {
				t.Errorf("why %q state %q", o.str("why"), o.str("state"))
			}

			if took := time.Since(started); took > 6*time.Second {
				t.Errorf("the deadline took %s to end the stop", took)
			}

			if f.calls(t, "stop") != 1 || f.calls(t, "start") != 0 {
				t.Errorf("calls %v, want one stop and no start", f.systemctlCalls(t))
			}
		})
	}
}

// A5c, A6: the post-stop triple is the command's own and every conjunct is
// required; a stop that exited zero is not the proof.
func TestMigrateRequiresThePostStopTripleExactly(t *testing.T) {
	for _, c := range []struct{ name, active, sub, result string }{
		{"result timeout", "failed", "failed", "timeout"},
		{"inactive dead timeout", "inactive", "dead", "timeout"},
		{"inactive failed success", "inactive", "failed", "success"},
		{"activating dead success", "activating", "dead", "success"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Helper()
			f := newEndpointFixture(t)
			f.installB(t)
			f.afterStop(t, c.active, c.sub, c.result)
			f.afterStart(t)

			o := f.migrate(t, f.rendering(endpointB))
			mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonUnproved)

			if o.str("state") != stateStopped || !strings.Contains(o.str("why"), c.result) {
				t.Errorf("why %q state %q", o.str("why"), o.str("state"))
			}

			if f.calls(t, "stop") != 1 || f.calls(t, "start") != 0 {
				t.Errorf("calls %v", f.systemctlCalls(t))
			}
		})
	}

	t.Run("the row lacks Result", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.installB(t)
		writeFile(t, f.unitFile(nodeUnit+".after-stop"), "LoadState=loaded\nActiveState=inactive\nSubState=dead\nMainPID=0\nInvocationID=\nKillMode=mixed\n", 0o644)
		f.afterStart(t)

		o := f.migrate(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonUnproved)

		if o.str("state") != stateStopped {
			t.Errorf("state %q", o.str("state"))
		}
	})
}

// A7: the happy migration, its trace, and the installed file untouched.
func TestMigrateStopsStartsAndWaitsForTheRecordUnderTheNewInvocation(t *testing.T) {
	f := newEndpointFixture(t)
	f.installB(t)
	f.afterStop(t, "inactive", "dead", "success")
	f.afterStart(t)
	f.recordAfterStart(t, canonicalB)

	before, err := os.Stat(f.configPath)
	mustOK(t, err)

	o := f.migrate(t, f.rendering(endpointB))
	mustEndpointOutcome(t, o, outcomeMigrated)

	if o.str("from") != canonicalA || o.str("to") != canonicalB || o.str("invocation_id") != newInvocation ||
		o.str("incarnation") != newIncarnation || o.str("node") != "node-a" || o.str("deployment") != f.deployment ||
		o.str("config_path") != f.configPath || !sha256Hex.MatchString(o.str("installed_sha256")) {
		t.Errorf("evidence %v", o.doc)
	}

	stopped := asMap(o.doc["stopped"])
	if stopped["active_state"] != "inactive" || stopped["sub_state"] != "dead" || stopped["result"] != "success" {
		t.Errorf("stopped %v", stopped)
	}

	// THE TRACE: the first observation, the bracket, the stop, StopAndProve's
	// own observation, the command's post-stop observation, the start,
	// StartAndProve's samples, the new invocation, the bracket around the
	// record, the closing observation. Verbs in order.
	var verbs []string

	for _, c := range f.systemctlCalls(t) {
		verbs = append(verbs, strings.Fields(c)[0])
	}

	joined := strings.Join(verbs, " ")
	if !strings.Contains(joined, "show stop show show start show") || strings.Count(joined, "stop") != 1 ||
		strings.Count(joined, "start") != 1 {
		t.Errorf("the trace: %s", joined)
	}

	after, err := os.Stat(f.configPath)
	mustOK(t, err)

	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Error("the migration touched the installed configuration; it installs nothing")
	}
}

// A8: a failed start leaves the node stopped; a missing invocation after the
// start is could-not-tell.
func TestMigrateReportsAFailedStart(t *testing.T) {
	f := newEndpointFixture(t)
	f.installB(t)
	f.afterStop(t, "inactive", "dead", "success")
	f.startMode(t, "fail")

	o := f.migrate(t, f.rendering(endpointB))
	mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonUnproved)

	if o.str("state") != stateStopped || !strings.Contains(o.str("next"), "systemctl start") {
		t.Errorf("why %q next %q state %q", o.str("why"), o.str("next"), o.str("state"))
	}
}

// A9: the record wait after the start.
func TestMigrateWaitsForTheRecordUnderTheNewInvocation(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, f *endpointFixture)
		words string
	}{
		{"no record", func(t *testing.T, f *endpointFixture) {
			t.Helper()
			f.recordRemovedAtStart(t)
		}, "published no usable record"},
		{"the old invocation throughout", func(*testing.T, *endpointFixture) {}, "published no usable record"},
		{"another endpoint", func(t *testing.T, f *endpointFixture) {
			t.Helper()
			f.recordAfterStart(t, canonicalA)
		}, "dials"},
		{"another node", func(t *testing.T, f *endpointFixture) {
			t.Helper()
			body, err := json.Marshal(f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalB,
				"invocation_id": newInvocation, "node": "node-b"}))
			mustOK(t, err)
			writeFile(t, f.unitFile("record.after-start"), string(body)+"\n", 0o600)
		}, "cannot be judged"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Helper()
			f := newEndpointFixture(t)
			f.installB(t)
			f.afterStop(t, "inactive", "dead", "success")
			f.afterStart(t)
			c.setup(t, f)

			o := f.migrate(t, f.rendering(endpointB))
			mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonUnproved)

			if o.str("state") != stateStartedNoRecord || !strings.Contains(o.str("why"), c.words) {
				t.Errorf("why %q state %q, want %q", o.str("why"), o.str("state"), c.words)
			}
		})
	}
}

// A10: the effective endpoint's wait before the decision: a running R node
// without a record is could-not-tell (never pre-R by absence), in the dry
// run and the action alike; a record that appears is decided from.
func TestMigrateDoesNotReadAMissingRecordAsPreR(t *testing.T) {
	f := newEndpointFixture(t)
	mustOK(t, os.Remove(f.recordPath))

	f.rBinary(t)

	for _, extra := range [][]string{{"--dry-run"}, nil} {
		o := f.migrate(t, f.rendering(endpointB), extra...)
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonRecord)

		if !strings.Contains(o.str("why"), "published no record") || f.calls(t, "stop") != 0 {
			t.Errorf("why %q calls %v", o.str("why"), f.systemctlCalls(t))
		}
	}
}

// A11: the inputs.
func TestMigrateRefusesInputsItCannotJudge(t *testing.T) {
	f := newEndpointFixture(t)

	for _, c := range []struct {
		name    string
		stdin   string
		extra   []string
		outcome string
		reason  string
	}{
		{"a rendering that does not parse", "node: [", nil, outcomeRefused, endpointReasonDesired},
		{"a rendering naming another node", strings.Replace(f.rendering(endpointB), "name: node-a", "name: node-b", 1), nil,
			outcomeRefused, endpointReasonDesired},
		{"a zone", f.rendering("[fe80::1%25eth0]:7717"), nil, outcomeRefused, endpointReasonDesired},
		{"no rendering outside a dry run", "", nil, outcomeRefused, endpointReasonCombination},
		{"a zero stop timeout", f.rendering(endpointB), []string{"--stop-timeout", "0"}, outcomeRefused, endpointReasonCombination},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Helper()
			o := f.migrate(t, c.stdin, c.extra...)
			mustEndpointRefusal(t, o, c.outcome, c.reason)
		})
	}

	t.Run("a config that is absent in the action", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		mustOK(t, os.Remove(f.configPath))

		o := f.migrate(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonConfig)
	})

	t.Run("a config that does not parse", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.writeConfig(t, "node: [")

		o := f.migrate(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonConfig)
	})

	t.Run("darwin", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		hostOS = "darwin"

		o := f.migrate(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonPlatform)
	})
}

// A14: the closing check on every successful answer.
func TestMigrateClosesEveryAnswerAgainstTheConfigurationAndTheProcess(t *testing.T) {
	t.Run("the dry run over a configuration replaced under the wait", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		mustOK(t, os.Remove(f.recordPath))

		// After the first read finds no record, the configuration is replaced
		// and the record published; the next bracket decides from the record
		// and the closing check finds the configuration moved.
		f.onRecordRead(t, 1, func() {
			f.installB(t)
			f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalB}))
		})

		o := f.migrate(t, f.rendering(endpointB), "--dry-run", "--wait", "2s")
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)
	})

	t.Run("a process replaced before an unchanged answer", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		// The first observation, the bracket's two, then the closing one
		// sees another process; each retry's closing observation does too.
		f.everyShow(t, 4, nodeUnitBody("active", "running", 9999, newInvocation, "mixed", "success"))

		o := f.migrate(t, f.rendering(endpointA))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonProcess)
	})

	t.Run("a unit found stopping at the close", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.afterShows(t, 3, nodeUnitBody("deactivating", "stop-sigterm", inspectPID, nodeInvocation, "mixed", "success"))

		o := f.migrate(t, f.rendering(endpointA))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonStopping)
	})

	t.Run("the closing stat failing", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)

		prev := closingStat
		closingStat = func(string) (os.FileInfo, error) { return nil, syscall.EACCES }

		t.Cleanup(func() { closingStat = prev })

		o := f.migrate(t, f.rendering(endpointA))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)
	})

	t.Run("a bracket that never agrees", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.alternating(t, nodeUnitBody("active", "running", inspectPID, newInvocation, "mixed", "success"))

		o := f.migrate(t, f.rendering(endpointB), "--dry-run")
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonUnit)

		if f.calls(t, "stop") != 0 {
			t.Error("a bracket that never agreed stopped the node")
		}
	})
}

// A15: node presence is the unit's, never a configuration's: a leftover
// node beside a server-only pre-render configuration is migrated like any
// other once the render installed a node; a removal is unchanged with
// node_removed; two server-only configurations beside a stopping node refuse.
func TestMigrateJudgesTheUnitWhateverTheConfigurationsSay(t *testing.T) {
	serverOnly := func(f *endpointFixture) string {
		return "server:\n  listen: 127.0.0.1:7717\n  state_dir: " + f.stateDir + "\n  max_vcpu: 8\n  max_memory: 32GiB\n" +
			"github:\n  org: acme\n  app_id: 1\n  installation_id: 2\n  private_key_path: " + filepath.Join(f.dir, "app.pem") +
			"\ntiers:\n  - label: billet-2vcpu\n    provider: docker\n    vcpu: 2\n    memory: 8GiB\n    image: ubuntu:24.04\n"
	}

	t.Run("a leftover node migrated after the render", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.installB(t)
		f.afterStop(t, "inactive", "dead", "success")
		f.afterStart(t)
		f.recordAfterStart(t, canonicalB)

		o := f.migrate(t, f.rendering(endpointB))
		mustEndpointOutcome(t, o, outcomeMigrated)
	})

	t.Run("a removal is unchanged with node_removed", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.writeConfig(t, serverOnly(f))

		o := f.migrate(t, serverOnly(f))
		mustEndpointOutcome(t, o, outcomeUnchanged)

		if !o.boolean("node_removed") || o.str("record") != recordUnread || o.doc["endpoint"] != nil || o.doc["node"] != nil {
			t.Errorf("answer %v", o.doc)
		}

		if f.ops["read"] != 0 {
			t.Errorf("an observation-only answer read the record %d times", f.ops["read"])
		}
	})

	t.Run("two server-only configurations beside a stopping node", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.writeConfig(t, serverOnly(f))
		f.setNode(t, "deactivating", "stop-sigterm", inspectPID, nodeInvocation, "mixed")

		o := f.migrate(t, serverOnly(f), "--dry-run")
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonStopping)
	})

	t.Run("a first start", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.writeConfig(t, serverOnly(f))
		f.setNode(t, "inactive", "dead", 0, "", "mixed")

		o := f.migrate(t, f.rendering(endpointB), "--dry-run")
		mustEndpointOutcome(t, o, outcomeReported)

		if !o.boolean("first_start") || o.boolean("planned") || o.str("record") != recordNone {
			t.Errorf("answer %v", o.doc)
		}
	})
}

// A16: an absent configuration is not proof that no node runs.
func TestMigrateObservesTheUnitWithNoConfigurationInstalled(t *testing.T) {
	t.Run("a fresh host", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		rendering := f.rendering(endpointB)
		mustOK(t, os.Remove(f.configPath))
		f.setNode(t, "inactive", "dead", 0, "", "mixed")

		o := f.migrate(t, rendering, "--dry-run")
		mustEndpointOutcome(t, o, outcomeReported)

		if o.str("config") != "absent" || o.boolean("planned") || o.str("record") != recordNone {
			t.Errorf("answer %v", o.doc)
		}
	})

	t.Run("a leftover running node", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		rendering := f.rendering(endpointB)
		mustOK(t, os.Remove(f.configPath))

		o := f.migrate(t, rendering, "--dry-run")
		mustEndpointOutcome(t, o, outcomeReported)

		if o.str("config") != "absent" || !o.boolean("planned") || o.str("effective") != canonicalA {
			t.Errorf("answer %v", o.doc)
		}
	})

	t.Run("a configuration that appeared under the wait", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		rendering := f.rendering(endpointB)
		mustOK(t, os.Remove(f.configPath))
		mustOK(t, os.Remove(f.recordPath))

		f.onRecordRead(t, 1, func() {
			f.writeConfig(t, rendering)
			f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalB}))
		})

		o := f.migrate(t, rendering, "--dry-run", "--wait", "2s")
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)
	})
}
