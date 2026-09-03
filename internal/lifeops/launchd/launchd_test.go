package launchd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/lifeops"
)

// reply is one answer from a staged launchctl.
type reply struct {
	out  string
	code int
	err  error
}

// fake stands in for launchctl.
//
// IT ANSWERS IN SEQUENCE, because the questions this package asks are asked
// repeatedly and the interesting cases are the ones where the answer CHANGES —
// a service that is still in the domain on one poll and gone on the next, a
// process that outlives the record. A fake that returned one fixed answer per
// verb could not model a stop at all.
type fake struct {
	t *testing.T
	// replies are consumed in order per verb; the last one repeats once the
	// queue is empty, so a test only has to describe the transitions it cares
	// about.
	replies map[string][]reply
	calls   []string
	// alive is the world's opinion of each pid, which a test moves to model a
	// process exiting after launchd has let go of it.
	alive map[int]bool
	// before runs just before a launchctl call, so a test can observe the world
	// AS IT WAS at that moment. Some of what this package guarantees is an
	// ORDER between a file operation and a launchctl one, and an assertion made
	// afterwards cannot tell which came first.
	before func(args []string)

	// ticks bounds the stop poll so a test that never satisfies it ends rather
	// than hanging.
	ticks int
	// onTick runs inside the poll's wait, which is the only place a test can
	// change the world BETWEEN two observations. A goroutine racing the counter
	// was the first version, and which of the two arrived first decided the
	// result -- so the test could fail for a reason it did not name.
	onTick func(remaining int)
}

func (f *fake) run(_ context.Context, args []string) (string, int, error) {
	if f.before != nil {
		f.before(args)
	}

	f.calls = append(f.calls, strings.Join(args, " "))

	verb := args[0]

	queue := f.replies[verb]
	if len(queue) == 0 {
		f.t.Fatalf("nothing staged for `launchctl %s`", strings.Join(args, " "))
	}

	r := queue[0]
	if len(queue) > 1 {
		f.replies[verb] = queue[1:]
	}

	return r.out, r.code, r.err
}

// converger wires a Converger to this fake.
func (f *fake) converger(t *testing.T) *Converger {
	t.Helper()

	c := New(WithAgentsDir(t.TempDir()))
	c.uid = 501
	c.run = f.run
	c.alive = func(pid int) bool { return f.alive[pid] }
	c.sleep = func(ctx context.Context, _ time.Duration) bool {
		if err := ctx.Err(); err != nil {
			return false
		}

		f.ticks--

		if f.onTick != nil {
			f.onTick(f.ticks)
		}

		return f.ticks > 0
	}

	return c
}

// printOut renders a well-formed `launchctl print` reply for the node label.
func printOut(fields ...string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s = {\n", nodeTarget)

	for _, f := range fields {
		fmt.Fprintf(&b, "\t%s\n", f)
	}

	b.WriteString("}\n")

	return b.String()
}

const nodeTarget = "gui/501/sh.billet.node"

// A STOP IS PROVED BY THE PROCESS, NOT BY launchd LETTING GO.
//
// Measured on macOS 26: a plain `launchctl bootout` returns in ZERO seconds
// while the agent is still draining. The service stays in the domain reporting
// `state = SIGTERMed` with its pid, and the record disappears only when the
// process finally exits. So neither the command's return nor the service's
// absence is a stop on its own — and reporting one as a stop lets `down` go on
// to the SERVER, whose teardown releases every lease its listener holds,
// including the ones belonging to the job still running.
func TestStopAndProveWaitsForTheProcessAndNotJustTheRecord(t *testing.T) {
	t.Parallel()

	f := &fake{
		t:     t,
		ticks: 10,
		alive: map[int]bool{4242: true},
		replies: map[string][]reply{
			"bootout": {{}},
			"print": {
				// Before the bootout: running.
				{out: printOut("state = running", "pid = 4242")},
				// Still draining, still in the domain.
				{out: printOut("state = SIGTERMed", "pid = 4242")},
				// launchd has let go — but the process has not.
				{out: "", code: notLoaded},
			},
		},
	}

	// THE PROCESS DIES AFTER launchd HAS STOPPED ANSWERING ABOUT IT, which is
	// the window the independent proof exists for. Driven from inside the poll
	// so the ordering is a fact rather than a race.
	f.onTick = func(remaining int) {
		if remaining == 8 {
			f.alive[4242] = false
		}
	}

	got, err := f.converger(t).StopAndProve(t.Context(), "sh.billet.node")
	if err != nil {
		t.Fatalf("StopAndProve: %v", err)
	}

	if got.Gone != lifeops.Yes {
		t.Errorf("Gone = %v, want yes", got.Gone)
	}

	if !strings.Contains(got.How, "4242") {
		t.Errorf("How = %q, want the pid it followed", got.How)
	}
}

// AND IT NEVER REPORTS A STOP WHILE A WATCHED PROCESS IS ALIVE.
func TestStopAndProveRefusesWhileTheProcessLives(t *testing.T) {
	t.Parallel()

	f := &fake{
		t:     t,
		ticks: 4,
		alive: map[int]bool{4242: true},
		replies: map[string][]reply{
			"bootout": {{}},
			"print": {
				{out: printOut("state = running", "pid = 4242")},
				// Out of the domain immediately, process still draining.
				{out: "", code: notLoaded},
			},
		},
	}

	got, err := f.converger(t).StopAndProve(t.Context(), "sh.billet.node")
	if err == nil {
		t.Fatal("a host whose process was still alive was reported down")
	}

	// UNCERTAINTY, NOT "still running": billet stopped watching, which is a
	// different fact from having established anything.
	if got.Gone != lifeops.Unknown {
		t.Errorf("Gone = %v, want unknown when billet gave up watching", got.Gone)
	}

	if !strings.Contains(got.How, "alive") {
		t.Errorf("How = %q, want what was true when it stopped waiting", got.How)
	}
}

// A BOOTOUT THAT DID NOT SUCCEED IS NOT A STOP.
//
// `run` reports a non-zero exit separately from an error precisely so it can be
// acted on. Ignoring it meant a refused bootout — no permission, a domain that
// does not exist — fell through into a poll waiting for a process nobody had
// asked to stop.
func TestStopAndProveRefusesABootoutThatFailed(t *testing.T) {
	t.Parallel()

	f := &fake{
		t:     t,
		ticks: 4,
		alive: map[int]bool{4242: true},
		replies: map[string][]reply{
			"print":   {{out: printOut("state = running", "pid = 4242")}},
			"bootout": {{out: "Boot-out failed: 1: Operation not permitted", code: 1}},
		},
	}

	got, err := f.converger(t).StopAndProve(t.Context(), "sh.billet.node")
	if err == nil {
		t.Fatal("a bootout that was refused was treated as a stop")
	}

	if got.Gone != lifeops.Unknown {
		t.Errorf("Gone = %v, want unknown", got.Gone)
	}

	// AND IT DID NOT GO ON TO POLL, because there is nothing to wait for.
	if n := strings.Count(strings.Join(f.calls, "|"), "print"); n != 1 {
		t.Errorf("it polled after a failed bootout: %v", f.calls)
	}
}

// EVERY PID THE LABEL IS SEEN WITH IS FOLLOWED, not only the first.
//
// A bootout that leaves the job in place leaves KeepAlive with it, and launchd
// starts the service again. Following only the pid captured before the bootout
// would then prove a process gone that has already been replaced — and report
// the host down with a new one running.
func TestStopAndProveFollowsAReplacementProcess(t *testing.T) {
	t.Parallel()

	f := &fake{
		t:     t,
		ticks: 8,
		alive: map[int]bool{4242: false, 5353: true},
		replies: map[string][]reply{
			"bootout": {{}},
			"print": {
				{out: printOut("state = running", "pid = 4242")},
				// launchd restarted it under a NEW pid.
				{out: printOut("state = running", "pid = 5353")},
				{out: "", code: notLoaded},
			},
		},
	}

	got, err := f.converger(t).StopAndProve(t.Context(), "sh.billet.node")
	if err == nil {
		t.Fatal("a replacement process was not noticed, and the host was reported down")
	}

	if got.Gone != lifeops.Unknown {
		t.Errorf("Gone = %v, want unknown", got.Gone)
	}

	if !strings.Contains(got.How, "5353") {
		t.Errorf("How = %q, want the replacement pid it was still following", got.How)
	}
}

// A SERVICE THAT IS NOT IN THE DOMAIN HAS NOTHING TO STOP.
func TestStopAndProveAcceptsAServiceThatIsNotLoaded(t *testing.T) {
	t.Parallel()

	f := &fake{t: t, ticks: 4, replies: map[string][]reply{
		"print": {{out: "", code: notLoaded}},
	}}

	got, err := f.converger(t).StopAndProve(t.Context(), "sh.billet.node")
	if err != nil {
		t.Fatalf("StopAndProve: %v", err)
	}

	if got.Gone != lifeops.Yes {
		t.Errorf("Gone = %v, want yes", got.Gone)
	}

	// AND IT DID NOT BOOT ANYTHING OUT.
	if strings.Contains(strings.Join(f.calls, "|"), "bootout") {
		t.Errorf("it booted out a service that was not loaded: %v", f.calls)
	}
}

// ENABLEMENT IS THE OVERRIDE DATABASE AND THE PLIST, and neither alone.
func TestEnabledNowReadsBothHalves(t *testing.T) {
	t.Parallel()

	const disabledList = "\n\tdisabled services = {\n" +
		"\t\t\"sh.billet.node\" => %s\n" +
		"\t}\n"

	for name, tc := range map[string]struct {
		listed  string
		install bool
		want    lifeops.Tristate
		how     string
	}{
		// The landmine: a durable override survives everything else billet
		// installs, so a perfectly good plist is not enablement.
		"disabled, plist installed": {"disabled", true, lifeops.No, "disabled"},
		"disabled, no plist":        {"disabled", false, lifeops.No, "disabled"},
		"enabled, plist installed":  {"enabled", true, lifeops.Yes, "enabled"},
		// A different no, with a different remedy: this one needs a plist
		// rather than `launchctl enable`.
		"enabled, no plist": {"enabled", false, lifeops.No, "not installed"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := &fake{t: t, replies: map[string][]reply{
				"print-disabled": {{out: fmt.Sprintf(disabledList, tc.listed)}},
			}}

			c := f.converger(t)

			if tc.install {
				path := filepath.Join(c.agentsDir, "sh.billet.node.plist")
				if err := os.WriteFile(path, []byte("<plist/>"), 0o600); err != nil {
					t.Fatalf("install the agent: %v", err)
				}
			}

			got, err := c.EnabledNow(t.Context(), "sh.billet.node")
			if err != nil {
				t.Fatalf("EnabledNow: %v", err)
			}

			if got.Enabled != tc.want {
				t.Errorf("Enabled = %v, want %v", got.Enabled, tc.want)
			}

			if got.How != tc.how {
				t.Errorf("How = %q, want %q", got.How, tc.how)
			}
		})
	}
}

// A LABEL NOT IN THE DATABASE AT ALL IS NOT DISABLED.
//
// The database lists only labels somebody has acted on. Absence is the ordinary
// case and means the default, which is enabled.
func TestEnabledNowTreatsAnUnlistedLabelAsNotDisabled(t *testing.T) {
	t.Parallel()

	f := &fake{t: t, replies: map[string][]reply{
		"print-disabled": {{out: "\n\tdisabled services = {\n\t\t\"com.example.other\" => disabled\n\t}\n"}},
	}}

	c := f.converger(t)
	if err := os.WriteFile(filepath.Join(c.agentsDir, "sh.billet.node.plist"),
		[]byte("<plist/>"), 0o600); err != nil {
		t.Fatalf("install the agent: %v", err)
	}

	got, err := c.EnabledNow(t.Context(), "sh.billet.node")
	if err != nil {
		t.Fatalf("EnabledNow: %v", err)
	}

	if got.Enabled != lifeops.Yes {
		t.Errorf("Enabled = %v, want yes for a label nobody has disabled", got.Enabled)
	}
}

// A DATABASE BILLET CANNOT FULLY READ IS AN ERROR, because "not listed" is what
// authorises a start.
//
// A parser whose failure mode is a PERMISSION is the wrong way round: every one
// of these used to be skipped, and a skipped line is a label that comes back
// "not disabled".
func TestParseDisabledRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()

	for name, in := range map[string]string{
		"empty":                    "",
		"cut off":                  "\n\tdisabled services = {\n\t\t\"a\" => disabled\n",
		"a value it does not know": "\n\tdisabled services = {\n\t\t\"a\" => sometimes\n\t}\n",
		"a line it cannot read":    "\n\tdisabled services = {\n\t\tnonsense\n\t}\n",
		"an unquotable label":      "\n\tdisabled services = {\n\t\t\"a => disabled\n\t}\n",
		"content outside the list": "\n\tdisabled services = {\n\t}\n\t\"a\" => disabled\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := parseDisabled(in); err == nil {
				t.Error("billet read a disabled-services list it could not fully understand; " +
					"a label missing from it then reads as permission to start")
			}
		})
	}
}

// AND THE REAL SHAPE PARSES, in both directions.
func TestParseDisabledReadsBothStates(t *testing.T) {
	t.Parallel()

	got, err := parseDisabled("\n\tdisabled services = {\n" +
		"\t\t\"com.apple.Siri.agent\" => disabled\n" +
		"\t\t\"sh.billet.node\" => enabled\n" +
		"\t}\n")
	if err != nil {
		t.Fatalf("parseDisabled: %v", err)
	}

	if !got["com.apple.Siri.agent"] {
		t.Error("a disabled label was not read as disabled")
	}

	// LISTED IS NOT DISABLED. The database carries both states, and reading
	// every listed label as disabled would refuse to start every service
	// anybody had ever enabled by hand.
	if got["sh.billet.node"] {
		t.Error("a label listed as enabled was read as disabled")
	}
}

// A launchctl THAT COULD NOT BE RUN IS NOT AN EMPTY DATABASE.
func TestEnabledNowSurfacesAFailureToAsk(t *testing.T) {
	t.Parallel()

	f := &fake{t: t, replies: map[string][]reply{
		"print-disabled": {{err: errors.New("launchctl: no such file")}},
	}}

	if _, err := f.converger(t).EnabledNow(t.Context(), "sh.billet.node"); err == nil {
		t.Error("a launchctl that could not be run was read as a database with nothing in it")
	}
}
