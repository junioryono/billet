package host

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/nodeending"
)

// endingFixture is a node-only host: its configuration, its state directory
// holding the ending record, and a systemctl that answers only the properties
// it is asked for, from a file the test rewrites.
type endingFixture struct {
	configPath string
	stateDir   string
	unitFile   string
	claimed    int
}

func newEndingFixture(t *testing.T) *endingFixture {
	t.Helper()

	dir := t.TempDir()
	f := &endingFixture{stateDir: filepath.Join(dir, "node"), unitFile: filepath.Join(dir, "unit.properties")}

	if err := os.MkdirAll(f.stateDir, 0o750); err != nil {
		t.Fatal(err)
	}

	f.configPath = filepath.Join(dir, "billet.yaml")
	writeFile(t, f.configPath, fmt.Sprintf(`node:
  name: n1
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: %s
  lock_dir: %s
`, f.stateDir, filepath.Join(dir, "lock")), 0o600)

	systemctl := filepath.Join(dir, "systemctl")
	writeFile(t, systemctl, `#!/bin/sh
case " $* " in *" billet-node.service "*) ;; *) exit 0 ;; esac
for a in "$@"; do case "$a" in --property=*) grep "^${a#--property=}=" "`+f.unitFile+`";; esac; done
exit 0
`, 0o755)

	f.unit(t, "inactive", "dead", "success", "0", "")

	prevInsp, prevOS, prevEUID, prevClaim := endingInspector, cli.HostOS, endingEUID, endingClaim
	endingInspector = func() *lifeops.Inspector { return lifeops.NewInspector(lifeops.WithSystemctl(systemctl)) }
	cli.HostOS = "linux"
	endingEUID = func() int { return 0 }
	endingClaim = func(*config.Config) (func() error, error) {
		f.claimed++

		return func() error { return nil }, nil
	}

	t.Cleanup(func() { endingInspector, cli.HostOS, endingEUID, endingClaim = prevInsp, prevOS, prevEUID, prevClaim })

	return f
}

// unit sets what systemd answers about the node unit; Job is answered empty.
func (f *endingFixture) unit(t *testing.T, active, sub, result, mainPID, invocation string) {
	t.Helper()

	writeFile(t, f.unitFile, fmt.Sprintf("ActiveState=%s\nSubState=%s\nResult=%s\nMainPID=%s\nJob=\nInvocationID=%s\n",
		active, sub, result, mainPID, invocation), 0o644)
}

// begin records a node process in the fixture's record and ends it, unless
// ending is Running.
func (f *endingFixture) begin(t *testing.T, invocation string, ending nodeending.Ending) {
	t.Helper()

	h, err := nodeending.Begin(f.stateDir, nodeending.Entry{InvocationID: invocation, PID: 4,
		BootID: nodeending.BootID(), Role: nodeending.RoleNode, RunsOnHost: true}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if ending != nodeending.Running {
		if err := h.End(ending, false); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *endingFixture) run(t *testing.T, args ...string) endpointOut {
	t.Helper()

	return runEndpoint(t, NodeEnding, "", append(append([]string{args[0]}, "--config", f.configPath),
		args[1:]...)...)
}

// CHECK ANSWERS BY THE STOPPED INVOCATION'S OWN ENDING, through its exit
// status: a drain exits 0; a killed node (its entry still running) exits 2
// even after reset-failed made systemd's triple clean; and a node that kept
// no record falls back to systemd's clean triple, which check then records so
// the start that follows sees it.
func TestEndingCheckAnswersByTheStoppedInvocation(t *testing.T) {
	t.Run("a drain", func(t *testing.T) {
		f := newEndingFixture(t)
		f.begin(t, "inv-1", nodeending.Drained)

		o := f.run(t, "check", "--invocation", "inv-1", "--drain")
		if o.code != 0 || o.str("outcome") != "proved" || o.str("proof") != nodeending.ProofRecord {
			t.Fatalf("code %d outcome %q proof %q (%s), want proved by the record", o.code, o.str("outcome"),
				o.str("proof"), o.str("why"))
		}
	})

	t.Run("an idle stop over a handoff, with and without --drain", func(t *testing.T) {
		f := newEndingFixture(t)
		f.begin(t, "inv-0", nodeending.HandedOff)
		f.begin(t, "inv-1", nodeending.Idle)

		if o := f.run(t, "check", "--invocation", "inv-1"); o.code != 0 {
			t.Fatalf("an idle stop without --drain: code %d (%s), want proved", o.code, o.str("why"))
		}

		if o := f.run(t, "check", "--invocation", "inv-1", "--drain"); o.code != exitRefused ||
			!strings.Contains(o.str("why"), "never adopted it") {
			t.Fatalf("an idle stop with --drain: code %d (%s), want unproved", o.code, o.str("why"))
		}
	})

	t.Run("no record, systemd timeout", func(t *testing.T) {
		f := newEndingFixture(t)
		f.begin(t, "inv-0", nodeending.Drained)
		f.unit(t, "failed", "failed", "timeout", "0", "inv-old")

		o := f.run(t, "check", "--invocation", "inv-old")
		if o.code != exitRefused || !o.boolean("recorded") || o.str("acknowledge") != "--invocation inv-old" {
			t.Fatalf("a node with no record killed at the timeout: code %d recorded %v acknowledge %q (%s), want "+
				"unproved, recorded, and the failure named", o.code, o.boolean("recorded"), o.str("acknowledge"),
				o.str("why"))
		}

		f.unit(t, "inactive", "dead", "success", "0", "")

		if c := f.run(t, "check", "--invocation", "inv-old"); c.code != exitRefused {
			t.Fatalf("check after reset-failed: code %d (%s), want the first account to stand", c.code, c.str("why"))
		}

		if a := f.run(t, "admit-start"); a.code != exitRefused || a.str("acknowledge") != "--invocation inv-old" {
			t.Fatalf("admit-start after reset-failed: code %d acknowledge %q (%s), want the recorded failure "+
				"refused", a.code, a.str("acknowledge"), a.str("why"))
		}

		// The acknowledgement the refusal named is one the command accepts.
		args := append([]string{"acknowledge"}, strings.Fields(o.str("acknowledge"))...)
		if k := f.run(t, args...); k.code != 0 {
			t.Fatalf("%v: code %d (%s), want accepted", args, k.code, k.str("why"))
		}

		if a := f.run(t, "admit-start"); a.code != 0 {
			t.Fatalf("admit-start after the acknowledgement: code %d (%s)", a.code, a.str("why"))
		}

		// The acknowledgement admits a start; it does not make the killed stop
		// a proved one, however clean systemd reports the unit now.
		if c := f.run(t, "check", "--invocation", "inv-old"); c.code != exitRefused {
			t.Fatalf("check after the acknowledgement: code %d (%s), want still unproved", c.code, c.str("why"))
		}
	})

	t.Run("a repeated check names the newest process", func(t *testing.T) {
		f := newEndingFixture(t)
		f.unit(t, "failed", "failed", "timeout", "0", "inv-old")

		if o := f.run(t, "check", "--invocation", "inv-old"); o.code != exitRefused || !o.boolean("recorded") {
			t.Fatalf("the first check: code %d recorded %v (%s)", o.code, o.boolean("recorded"), o.str("why"))
		}

		f.begin(t, "inv-new", nodeending.LeftRunning)

		o := f.run(t, "check", "--invocation", "inv-old")
		if o.code != exitRefused || o.str("acknowledge") != "--invocation inv-new" {
			t.Fatalf("the repeated check: code %d acknowledge %q (%s), want the newest, inv-new, named", o.code,
				o.str("acknowledge"), o.str("why"))
		}
	})

	t.Run("an acknowledged history does not pass the floor a drain needs", func(t *testing.T) {
		f := newEndingFixture(t)
		f.begin(t, "inv-1", nodeending.Running)

		if k := f.run(t, "acknowledge", "--invocation", "inv-1"); k.code != 0 {
			t.Fatalf("acknowledge: code %d (%s)", k.code, k.str("why"))
		}

		if o := f.run(t, "check", "--invocation", "inv-absent", "--drain"); o.code != exitRefused ||
			o.boolean("recorded") {
			t.Fatalf("the floor over an acknowledged killed node with --drain: code %d recorded %v (%s), want "+
				"unproved and nothing recorded", o.code, o.boolean("recorded"), o.str("why"))
		}

		if o := f.run(t, "check", "--invocation", "inv-absent"); o.code != 0 {
			t.Fatalf("the floor over an acknowledged node without --drain: code %d (%s), want proved", o.code,
				o.str("why"))
		}
	})

	t.Run("admit-start over a running node agrees with the host's gate", func(t *testing.T) {
		f := newEndingFixture(t)
		f.begin(t, "inv-1", nodeending.Running)
		f.unit(t, "active", "running", "success", "41", "inv-1")

		if a := f.run(t, "admit-start"); a.code != 0 || a.str("proof") != nodeending.ProofRunning ||
			!strings.Contains(a.raw, `"active_state": "active"`) {
			t.Fatalf("admit-start over a running node: code %d proof %q (%s), want admitted as running with "+
				"the unit it rests on:\n%s", a.code, a.str("proof"), a.str("why"), a.raw)
		}
	})

	t.Run("a stale invocation over a killed node", func(t *testing.T) {
		f := newEndingFixture(t)
		f.begin(t, "inv-1", nodeending.Running)

		if o := f.run(t, "check", "--invocation", "inv-stale"); o.code != exitRefused || o.boolean("recorded") {
			t.Fatalf("a stale invocation over a killed node: code %d recorded %v (%s), want unproved and "+
				"nothing recorded", o.code, o.boolean("recorded"), o.str("why"))
		}

		if a := f.run(t, "admit-start"); a.code != exitRefused || a.str("acknowledge") != "--invocation inv-1" {
			t.Fatalf("admit-start after a stale check: code %d acknowledge %q (%s), want refused", a.code,
				a.str("acknowledge"), a.str("why"))
		}
	})

	t.Run("killed, then reset-failed", func(t *testing.T) {
		f := newEndingFixture(t)
		f.begin(t, "inv-1", nodeending.Running)

		o := f.run(t, "check", "--invocation", "inv-1")
		if o.code != exitRefused || o.str("outcome") != "unproved" || !strings.Contains(o.str("why"),
			"killed or crashed") {
			t.Fatalf("code %d outcome %q (%s), want unproved with exit %d", o.code, o.str("outcome"), o.str("why"),
				exitRefused)
		}
	})

	t.Run("no record, systemd clean", func(t *testing.T) {
		f := newEndingFixture(t)

		o := f.run(t, "check", "--invocation", "inv-old")
		if o.code != 0 || o.str("proof") != nodeending.ProofSystemd || !o.boolean("recorded") {
			t.Fatalf("code %d proof %q recorded %v (%s), want the systemd floor, recorded", o.code,
				o.str("proof"), o.boolean("recorded"), o.str("why"))
		}

		a := f.run(t, "admit-start")
		if a.code != 0 || a.str("proof") != nodeending.ProofSystemd || !strings.Contains(a.str("why"),
			"kept no record") {
			t.Fatalf("admit-start after the floor: code %d proof %q (%s), want the recorded systemd stop",
				a.code, a.str("proof"), a.str("why"))
		}
	})

	t.Run("systemd unanswered", func(t *testing.T) {
		f := newEndingFixture(t)
		f.begin(t, "inv-1", nodeending.Drained)
		writeFile(t, f.unitFile, "", 0o644)

		o := f.run(t, "check", "--invocation", "inv-1")
		if o.code != exitUnknown || o.str("outcome") != "unknown" {
			t.Fatalf("code %d outcome %q (%s), want could-not-tell with exit %d", o.code, o.str("outcome"),
				o.str("why"), exitUnknown)
		}
	})

	t.Run("the invocation required", func(t *testing.T) {
		f := newEndingFixture(t)

		o := f.run(t, "check")
		if o.err == nil || !strings.Contains(o.err.Error(), "--invocation is required") {
			t.Fatalf("check without an invocation returned %v, want the flag's refusal", o.err)
		}
	})
}

// A START REFUSED FOR A KILLED NODE IS ADMITTED BY ITS ACKNOWLEDGEMENT, which
// is root's, taken under the node's own lock with systemd reporting no
// process, and only of the newest stop by its exact invocation.
func TestAcknowledgementAdmitsTheStartItWasRefused(t *testing.T) {
	f := newEndingFixture(t)
	f.begin(t, "inv-0", nodeending.Drained)
	f.begin(t, "inv-1", nodeending.Running)
	f.unit(t, "failed", "failed", "timeout", "0", "inv-1")

	if o := f.run(t, "admit-start"); o.code != exitRefused || o.str("acknowledge") != "--invocation inv-1" {
		t.Fatalf("admit-start over a killed node: code %d acknowledge %q (%s), want refused naming the "+
			"acknowledgement", o.code, o.str("acknowledge"), o.str("why"))
	}

	endingEUID = func() int { return 1000 }

	if o := f.run(t, "acknowledge", "--invocation", "inv-1"); o.code != exitRefused ||
		!strings.Contains(o.str("why"), "root's") || f.claimed != 0 {
		t.Fatalf("a non-root acknowledgement: code %d (%s), claims %d; want refused before the lock", o.code,
			o.str("why"), f.claimed)
	}

	endingEUID = func() int { return 0 }
	f.unit(t, "active", "running", "success", "77", "inv-2")

	if o := f.run(t, "acknowledge", "--invocation", "inv-1"); o.code != exitRefused ||
		!strings.Contains(o.str("why"), "is active") {
		t.Fatalf("an acknowledgement with a node running: code %d (%s), want refused", o.code, o.str("why"))
	}

	f.unit(t, "failed", "failed", "timeout", "0", "inv-1")

	if o := f.run(t, "acknowledge", "--invocation", "inv-0"); o.code != exitRefused ||
		!strings.Contains(o.str("why"), `not "inv-0"`) {
		t.Fatalf("acknowledging another invocation: code %d (%s), want refused", o.code, o.str("why"))
	}

	if o := f.run(t, "acknowledge", "--invocation", "inv-1"); o.code != 0 ||
		o.str("proof") != nodeending.ProofAcknowledged {
		t.Fatalf("acknowledging the newest stop: code %d (%s)", o.code, o.str("why"))
	}

	if f.claimed == 0 {
		t.Error("the acknowledgement never took the node's deployment lock")
	}

	if o := f.run(t, "admit-start"); o.code != 0 || o.str("proof") != nodeending.ProofAcknowledged {
		t.Fatalf("admit-start after the acknowledgement: code %d (%s), want admitted", o.code, o.str("why"))
	}
}

// AN UNREADABLE RECORD IS RECOVERED BY THE RESET IT NAMES: admit-start answers
// could-not-tell naming --reset-unreadable, which sets the record aside under
// the node's lock, after which a start is admitted.
func TestAnUnreadableRecordIsRecoveredByItsReset(t *testing.T) {
	f := newEndingFixture(t)
	writeFile(t, nodeending.Path(f.stateDir), "{", 0o600)

	if o := f.run(t, "admit-start"); o.code != exitUnknown || o.str("acknowledge") != "--reset-unreadable" {
		t.Fatalf("admit-start over an unreadable record: code %d acknowledge %q (%s), want could-not-tell "+
			"naming the reset", o.code, o.str("acknowledge"), o.str("why"))
	}

	if o := f.run(t, "acknowledge", "--reset-unreadable"); o.code != 0 || f.claimed != 1 ||
		!strings.Contains(o.str("why"), "set aside") {
		t.Fatalf("the reset: code %d claims %d (%s), want proved under the lock", o.code, f.claimed, o.str("why"))
	}

	if o := f.run(t, "admit-start"); o.code != 0 {
		t.Fatalf("admit-start after the reset: code %d (%s), want admitted", o.code, o.str("why"))
	}

	if o := f.run(t, "acknowledge", "--reset-unreadable", "--unrecorded"); o.err == nil {
		t.Fatal("two acknowledgements at once were accepted")
	}
}

// AN ACKNOWLEDGEMENT WHOSE LOCK IS HELD REFUSES: a node process may hold the
// identity, and an acknowledgement is of a stop.
func TestAnAcknowledgementWhoseLockIsHeldRefuses(t *testing.T) {
	f := newEndingFixture(t)
	f.begin(t, "inv-1", nodeending.Running)

	endingClaim = func(*config.Config) (func() error, error) { return nil, errors.New("held by pid 9") }

	o := f.run(t, "acknowledge", "--invocation", "inv-1")
	if o.code != exitRefused || !strings.Contains(o.str("why"), "held by pid 9") {
		t.Fatalf("code %d (%s), want refused naming the lock", o.code, o.str("why"))
	}

	rec, _, err := nodeending.Read(f.stateDir)
	if err != nil || rec.Entries[0].Acknowledged {
		t.Errorf("the record is %+v (%v), want nothing acknowledged", rec.Entries, err)
	}
}

// OFF systemd THE JUDGEMENTS REFUSE, and show still prints the record.
func TestEndingJudgementsRefuseOffSystemd(t *testing.T) {
	f := newEndingFixture(t)
	f.begin(t, "inv-1", nodeending.Drained)
	cli.HostOS = "darwin"

	for _, args := range [][]string{{"check", "--invocation", "inv-1"}, {"admit-start"},
		{"acknowledge", "--unrecorded"}} {
		if o := f.run(t, args...); o.code != exitRefused || !strings.Contains(o.str("why"), "systemd") {
			t.Errorf("%v on a Mac: code %d (%s), want refused", args, o.code, o.str("why"))
		}
	}

	if o := f.run(t, "show"); o.code != 0 || !strings.Contains(o.raw, `"inv-1"`) {
		t.Errorf("show on a Mac: code %d, want the record printed:\n%s", o.code, o.raw)
	}
}
