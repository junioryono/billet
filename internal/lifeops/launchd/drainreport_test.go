package launchd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/junioryono/billet/internal/lifeops"
)

// reports publishes pid's drain report the way the node does, through the
// converger's own start reader.
func (f *fake) reports(t *testing.T, c *Converger, pid int) {
	t.Helper()

	if err := publishDrainReport(c.logDir, "sh.billet.node", "v0.12.21", pid, c.processStart); err != nil {
		t.Fatalf("publish the drain report of pid %d: %v", pid, err)
	}
}

// stopsAfterTheDrain stages the bootout: it asserts no process of the label is
// alive when launchd is asked, and makes the service leave the domain.
func (f *fake) stopsAfterTheDrain(t *testing.T, pids ...int) {
	t.Helper()

	f.before = func(args []string) {
		if args[0] != "bootout" {
			return
		}

		for _, pid := range pids {
			if f.alive[pid] {
				t.Errorf("bootout ran with pid %d alive: launchd's grace, not the drain, would end it", pid)
			}
		}

		f.replies["print"] = []reply{{out: "", code: notLoaded}}
	}
}

// noStopRecord asserts the stop wrote no once-per-process record.
func noStopRecord(t *testing.T, c *Converger) {
	t.Helper()

	if _, err := os.Lstat(filepath.Join(c.logDir, ".stop-sh.billet.node")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a stop that sent only the idempotent request left a stop record (%v)", err)
	}
}

// A PROCESS THAT SAYS IT HANDLES THE DRAIN REQUEST IS SENT IT ON EVERY POLL, and
// never a SIGTERM: a repeat of the request changes nothing, and a SIGTERM is the
// signal whose second arrival escalates.
func TestStopAndProveRequestsTheDrainOnEveryPoll(t *testing.T) {
	t.Parallel()

	f := &fake{
		t:     t,
		ticks: 100,
		alive: map[int]bool{4242: true},
		replies: map[string][]reply{
			"bootout": {{}},
			"print":   {{out: printOut("state = running", "pid = 4242")}},
		},
	}

	c := f.converger(t)
	f.reports(t, c, 4242)
	f.stopsAfterTheDrain(t, 4242)

	f.onRequest = func(pid int) {
		if f.requests[pid] == 5 {
			f.alive[pid] = false
		}
	}

	got, err := c.StopAndProve(t.Context(), "sh.billet.node")
	if err != nil {
		t.Fatalf("StopAndProve: %v", err)
	}

	if got.Gone != lifeops.Yes || !got.Asked {
		t.Errorf("result = %+v, want gone and asked", got)
	}

	if f.requests[4242] != 5 || f.terms != 0 {
		t.Errorf("requests = %v, SIGTERMs = %d; want the request on every poll until the exit, "+
			"and no SIGTERM", f.requests, f.terms)
	}

	noStopRecord(t, c)
}

// A RESTART AROUND THE REQUEST IS SIMPLY ASKED AGAIN. With the SIGTERM, which of
// the two processes received it could not be told and the stop ended unknown;
// the request is harmless to repeat, so the replacement is asked in its turn
// and the stop finishes.
func TestStopAndProveAsksAProcessRestartedAroundTheRequestAgain(t *testing.T) {
	t.Parallel()

	f := &fake{
		t:     t,
		ticks: 100,
		alive: map[int]bool{4242: true},
		replies: map[string][]reply{
			"bootout": {{}},
			"print":   {{out: printOut("state = running", "pid = 4242")}},
		},
	}

	c := f.converger(t)
	f.reports(t, c, 4242)
	f.stopsAfterTheDrain(t, 4242, 5353)

	f.onRequest = func(pid int) {
		switch {
		case pid == 4242:
			// Crashed as the request arrived; launchd starts the current binary,
			// which reports in its turn.
			f.alive[4242] = false
			f.alive[5353] = true
			f.replies["print"] = []reply{{out: printOut("state = running", "pid = 5353")}}
			f.reports(t, c, 5353)
		case f.requests[pid] == 2:
			f.alive[pid] = false
		}
	}

	got, err := c.StopAndProve(t.Context(), "sh.billet.node")
	if err != nil {
		t.Fatalf("StopAndProve: %v", err)
	}

	if got.Gone != lifeops.Yes || !got.Asked {
		t.Errorf("result = %+v, want gone and asked", got)
	}

	if f.requests[5353] != 2 || f.terms != 0 {
		t.Errorf("requests = %v, SIGTERMs = %d; want the restarted process asked until it left",
			f.requests, f.terms)
	}

	noStopRecord(t, c)
}

// A STOP RETRIED AFTER ITS CALLER GAVE UP ASKS AGAIN, because nothing about the
// request needs remembering.
func TestStopAndProveRetriedRequestsTheDrainAgain(t *testing.T) {
	t.Parallel()

	f := &fake{
		t:     t,
		ticks: 3,
		alive: map[int]bool{4242: true},
		replies: map[string][]reply{
			"print": {{out: printOut("state = running", "pid = 4242")}},
		},
	}

	c := f.converger(t)
	f.reports(t, c, 4242)

	if _, err := c.StopAndProve(t.Context(), "sh.billet.node"); err == nil {
		t.Fatal("a stop whose drain never ended was reported done")
	}

	first := f.requests[4242]
	if first == 0 {
		t.Fatal("the first attempt sent no drain request")
	}

	f.ticks = 3

	got, err := c.StopAndProve(t.Context(), "sh.billet.node")
	if err == nil {
		t.Fatal("a stop whose drain never ended was reported done")
	}

	if f.requests[4242] <= first || f.terms != 0 {
		t.Errorf("requests = %d then %d, SIGTERMs = %d; want the retry to ask again and never SIGTERM",
			first, f.requests[4242], f.terms)
	}

	if got.Gone != lifeops.Unknown || !got.Asked {
		t.Errorf("result = %+v, want unknown and asked", got)
	}

	noStopRecord(t, c)
}

// A PROCESS THAT STOPS PROVING IT HANDLES THE REQUEST, once asked, is waited for
// and not asked again, by either request.
func TestStopAndProveStopsRequestingWhenTheReportNoLongerProvesIt(t *testing.T) {
	t.Parallel()

	f := &fake{
		t:     t,
		ticks: 100,
		alive: map[int]bool{4242: true},
		replies: map[string][]reply{
			"bootout": {{}},
			"print":   {{out: printOut("state = running", "pid = 4242")}},
		},
	}

	c := f.converger(t)
	f.reports(t, c, 4242)
	f.stopsAfterTheDrain(t, 4242)

	f.onRequest = func(int) {
		if err := os.Remove(drainReportPath(c.logDir, "sh.billet.node")); err != nil {
			t.Errorf("remove the report: %v", err)
		}
	}

	f.onTick = func(remaining int) {
		if remaining == 90 {
			f.alive[4242] = false
		}
	}

	if _, err := c.StopAndProve(t.Context(), "sh.billet.node"); err != nil {
		t.Fatalf("StopAndProve: %v", err)
	}

	if f.requests[4242] != 1 || f.terms != 0 {
		t.Errorf("requests = %v, SIGTERMs = %d; want one request and nothing after it", f.requests, f.terms)
	}
}

// writeReport writes a drain report's bytes by hand, for the shapes the writer
// never produces.
func writeReport(t *testing.T, c *Converger, body string) {
	t.Helper()

	if err := os.WriteFile(drainReportPath(c.logDir, "sh.billet.node"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// reportBody renders a drain report for pid 4242 with one member changed.
func reportBody(schema int, label string, pid int, started, request string) string {
	return fmt.Sprintf(`{"schema":%d,"label":%q,"pid":%d,"started":%q,"request":%q,"release":"v0.12.21"}`,
		schema, label, pid, started, request)
}

// AN OLDER RELEASE IS NEVER SENT THE REQUEST. A process is proved to handle it
// only by its own report, naming its pid, its start and this request; anything
// else, and no report at all, is what a release before the request looks like,
// and gets the one recorded SIGTERM it always did.
func TestStopAndProveNeverSendsTheRequestToAProcessNotProvedToHandleIt(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"no report":                         "",
		"another pid's report":              reportBody(1, "sh.billet.node", 9999, "started-9999", "SIGUSR1"),
		"an earlier process under this pid": reportBody(1, "sh.billet.node", 4242, "an earlier start", "SIGUSR1"),
		"another request":                   reportBody(1, "sh.billet.node", 4242, "started-4242", "SIGUSR2"),
		"another schema":                    reportBody(2, "sh.billet.node", 4242, "started-4242", "SIGUSR1"),
		"another label":                     reportBody(1, "sh.billet.server", 4242, "started-4242", "SIGUSR1"),
		"a member this build does not know": `{"schema":1,"label":"sh.billet.node","pid":4242,"started":"started-4242","request":"SIGUSR1","release":"v","hurry":true}`,
		"two reports":                       reportBody(1, "sh.billet.node", 4242, "started-4242", "SIGUSR1") + reportBody(1, "sh.billet.node", 4242, "started-4242", "SIGUSR1"),
		"a report cut short":                `{"schema":1,"label":"sh.billet.node","pid":4242`,
		"a member in another case":          `{"schema":1,"label":"sh.billet.node","pid":4242,"started":"started-4242","REQUEST":"SIGUSR1","release":"v"}`,
		"a member twice":                    `{"schema":1,"label":"sh.billet.node","pid":4242,"started":"started-4242","request":"SIGUSR2","request":"SIGUSR1","release":"v"}`,
		"a member missing":                  `{"schema":1,"label":"sh.billet.node","pid":4242,"started":"started-4242","request":"SIGUSR1"}`,
		"a stray brace after it":            reportBody(1, "sh.billet.node", 4242, "started-4242", "SIGUSR1") + "}",
		"more than a report can hold":       reportBody(1, "sh.billet.node", 4242, "started-4242", "SIGUSR1") + strings.Repeat(" ", 5000) + "x",
		"an empty start that matches none":  reportBody(1, "sh.billet.node", 4242, "", "SIGUSR1"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := oldPathFake(t)
			c := f.converger(t)

			if body != "" {
				writeReport(t, c, body)
			}

			assertOldPath(t, f, c)
		})
	}
}

// THE CONTROL FOR THE TABLE ABOVE: the report every case there breaks one thing
// of is proved as written.
func TestTheUnbrokenReportIsProved(t *testing.T) {
	t.Parallel()

	f := &fake{t: t, alive: map[int]bool{4242: true}}
	c := f.converger(t)
	writeReport(t, c, reportBody(1, "sh.billet.node", 4242, "started-4242", "SIGUSR1")+"\n")

	if err := c.provesDrainRequest("sh.billet.node", 4242); err != nil {
		t.Errorf("a well-formed report for this process was not proved: %v", err)
	}
}

// A REPORT THAT CANNOT BE READ IS COULD-NOT-TELL, and could-not-tell takes the
// recorded path rather than ending the stop or sending the request.
func TestStopAndProveTakesTheRecordedPathWhenTheReportCannotBeRead(t *testing.T) {
	t.Parallel()

	for name, plant := range map[string]func(t *testing.T, path string){
		"a link to a valid report": func(t *testing.T, path string) {
			t.Helper()

			target := filepath.Join(t.TempDir(), "report")
			if err := os.WriteFile(target, []byte(reportBody(1, "sh.billet.node", 4242, "started-4242",
				"SIGUSR1")), 0o600); err != nil {
				t.Fatal(err)
			}

			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
		"a directory": func(t *testing.T, path string) {
			t.Helper()

			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"a FIFO": func(t *testing.T, path string) {
			t.Helper()

			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := oldPathFake(t)
			c := f.converger(t)
			plant(t, drainReportPath(c.logDir, "sh.billet.node"))

			assertOldPath(t, f, c)
		})
	}
}

// A RESTART INTO AN OLDER RELEASE IS ASKED THE OLD WAY. A rollback, or a binary
// replaced under a crashing node, restarts a process that never reports; it
// gets its one SIGTERM, and never the request its predecessor was sent.
func TestStopAndProveAsksARestartedOlderReleaseWithASIGTERM(t *testing.T) {
	t.Parallel()

	f := &fake{
		t:     t,
		ticks: 100,
		alive: map[int]bool{4242: true},
		replies: map[string][]reply{
			"bootout": {{}},
			"print":   {{out: printOut("state = running", "pid = 4242")}},
		},
	}

	c := f.converger(t)
	f.reports(t, c, 4242)
	f.stopsAfterTheDrain(t, 4242, 5353)

	f.onRequest = func(pid int) {
		if pid == 4242 {
			f.alive[4242] = false
			f.alive[5353] = true
			f.replies["print"] = []reply{{out: printOut("state = running", "pid = 5353")}}
			f.exitsOnTerm = []int{5353}
		}
	}

	got, err := c.StopAndProve(t.Context(), "sh.billet.node")
	if err != nil {
		t.Fatalf("StopAndProve: %v", err)
	}

	if f.requests[5353] != 0 || f.terms != 1 {
		t.Errorf("requests = %v, SIGTERMs = %d; want the older process asked once with a SIGTERM",
			f.requests, f.terms)
	}

	if got.Gone != lifeops.Yes || !got.Asked {
		t.Errorf("result = %+v, want gone and asked", got)
	}
}

// oldPathFake is a node with nothing to drain that leaves on its SIGTERM.
func oldPathFake(t *testing.T) *fake {
	t.Helper()

	f := &fake{
		t:           t,
		ticks:       20,
		alive:       map[int]bool{4242: true},
		exitsOnTerm: []int{4242},
		replies: map[string][]reply{
			"bootout": {{}},
			"print":   {{out: printOut("state = running", "pid = 4242")}},
		},
	}

	f.stopsAfterTheDrain(t, 4242)

	return f
}

// assertOldPath runs the stop and asserts it asked with one SIGTERM and never
// the request.
func assertOldPath(t *testing.T, f *fake, c *Converger) {
	t.Helper()

	got, err := c.StopAndProve(t.Context(), "sh.billet.node")
	if err != nil {
		t.Fatalf("StopAndProve: %v", err)
	}

	if f.sent() != 0 {
		t.Errorf("the drain request went to a process not proved to handle it: %v", f.requests)
	}

	if f.terms != 1 {
		t.Errorf("SIGTERMs = %d, want the one recorded SIGTERM", f.terms)
	}

	if got.Gone != lifeops.Yes || !got.Asked {
		t.Errorf("result = %+v, want gone and asked", got)
	}
}

// THE WRITER AND THE READER AGREE ABOUT A REAL PROCESS, through the kernel's
// own start time: this process's report proves this process, and not its
// parent.
func TestADrainReportProvesTheProcessThatPublishedIt(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "darwin" {
		t.Skip("a process's start is read only on macOS, where the launch agents run")
	}

	c := New(WithLogDir(t.TempDir()))

	if err := publishDrainReport(c.logDir, "sh.billet.node", "v0.12.21", os.Getpid(), processStart); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if err := c.provesDrainRequest("sh.billet.node", os.Getpid()); err != nil {
		t.Errorf("this process's own report did not prove it: %v", err)
	}

	if err := c.provesDrainRequest("sh.billet.node", os.Getppid()); err == nil {
		t.Error("this process's report proved its parent")
	}
}
