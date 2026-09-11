package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/rollout"
	"github.com/junioryono/billet/internal/state"
)

// `rollout registration` (fixtures-c4b.md, section B): the controller's half
// of a migration's proof, over a real ledger through the read-only open,
// polling one snapshot at a time with the binding revalidated at every poll,
// the epoch reported and never compared.

const (
	regIncarnationOld = "0000000000000000000000000000aaaa"
	regIncarnationNew = "0000000000000000000000000000bbbb"
	regDeployment2    = "fedcba9876543210fedcba9876543210"
)

// regLedger is a claimed SQLite ledger and the config that names it.
type regLedger struct {
	stateDir, cfgPath, deployment string
}

func newRegLedger(t *testing.T, claim bool) *regLedger {
	t.Helper()

	stateDir := t.TempDir()
	l := &regLedger{stateDir: stateDir, cfgPath: writeCAConfig(t, stateDir)}

	deployment, err := state.DeploymentID(stateDir)
	mustOK(t, err)

	l.deployment = deployment

	statusPlane(t, stateDir, func(db *state.DB) {
		if claim {
			if _, err := db.ClaimController(t.Context(), "billet-control-01", deployment); err != nil {
				t.Fatal(err)
			}
		}
	})

	prev := endpointPoll
	endpointPoll = 10 * time.Millisecond

	t.Cleanup(func() { endpointPoll = prev })

	return l
}

// register registers node under incarnation through a control plane, the
// way a node does, and returns the epoch.
func (l *regLedger) register(t *testing.T, node, incarnation string) int64 {
	t.Helper()

	var epoch int64

	statusPlane(t, l.stateDir, func(db *state.DB) {
		epoch = registerStatusNode(t, db, node, "v0.9.4", statusDigestB, incarnation)
	})

	return epoch
}

func (l *regLedger) gone(t *testing.T, node string, epoch int64) {
	t.Helper()
	statusPlane(t, l.stateDir, func(db *state.DB) { markStatusNodeGone(t, db, node, epoch) })
}

// polls replaces the snapshot seam with one that counts polls and runs
// between(n) after the n-th poll returned, before the next.
func (l *regLedger) polls(t *testing.T, between func(n int)) *int {
	t.Helper()

	prev := registrationPoll
	t.Cleanup(func() { registrationPoll = prev })

	n := 0
	registrationPoll = func(ctx context.Context, store *rollout.Store) (rollout.StatusSnapshot, error) {
		snap, err := store.StatusSnapshot(ctx)
		n++

		if between != nil {
			between(n)
		}

		return snap, err
	}

	return &n
}

func (l *regLedger) run(t *testing.T, extra ...string) endpointOut {
	t.Helper()

	args := append([]string{"--config", l.cfgPath, "--node", "node-a", "--incarnation", regIncarnationNew, "--wait", "300ms"},
		extra...)

	return runEndpoint(t, cmdRolloutRegistration, "", args...)
}

func mustTimeout(t *testing.T, o endpointOut) {
	t.Helper()

	if o.str("outcome") != outcomeTimeout || o.code != exitUnknown {
		t.Fatalf("outcome %q code %d (%v), want timeout/%d", o.str("outcome"), o.code, o.doc, exitUnknown)
	}
}

// B1: the row with the incarnation and live → confirmed with its epoch and
// the binding; the epoch rules.
func TestRegistrationConfirmsTheLiveRowWhateverTheEpochDid(t *testing.T) {
	t.Run("the row present at the first poll", func(t *testing.T) {
		l := newRegLedger(t, true)
		epoch := l.register(t, "node-a", regIncarnationNew)
		n := l.polls(t, nil)

		o := l.run(t)
		mustEndpointOutcome(t, o, outcomeConfirmed)

		if epochOf(t, o.doc["epoch"]) != epoch || !o.boolean("live") || o.str("node") != "node-a" ||
			o.str("incarnation") != regIncarnationNew {
			t.Errorf("answer %v", o.doc)
		}

		dep := asMap(o.doc["deployment"])
		if dep["bound"] != true || dep["id"] != l.deployment {
			t.Errorf("deployment %v", dep)
		}

		if *n != 1 {
			t.Errorf("%d polls for a row present at the first", *n)
		}
	})

	t.Run("old at the first poll, new at the second", func(t *testing.T) {
		l := newRegLedger(t, true)
		l.register(t, "node-a", regIncarnationOld)

		var newEpoch int64

		n := l.polls(t, func(n int) {
			if n == 1 {
				newEpoch = l.register(t, "node-a", regIncarnationNew)
			}
		})

		o := l.run(t)
		mustEndpointOutcome(t, o, outcomeConfirmed)

		if epochOf(t, o.doc["epoch"]) != newEpoch || *n != 2 {
			t.Errorf("epoch %v polls %d, want %d and 2", o.doc["epoch"], *n, newEpoch)
		}
	})

	t.Run("a re-registration under the same incarnation", func(t *testing.T) {
		l := newRegLedger(t, true)
		l.register(t, "node-a", regIncarnationNew)
		epoch := l.register(t, "node-a", regIncarnationNew)

		o := l.run(t)
		mustEndpointOutcome(t, o, outcomeConfirmed)

		if epochOf(t, o.doc["epoch"]) != epoch {
			t.Errorf("epoch %v, want %d", o.doc["epoch"], epoch)
		}
	})

	t.Run("the old incarnation with its epoch advanced throughout", func(t *testing.T) {
		l := newRegLedger(t, true)

		for range 9 {
			l.register(t, "node-a", regIncarnationOld)
		}

		o := l.run(t)
		mustTimeout(t, o)

		last := asMap(o.doc["last"])
		if last["incarnation"] != regIncarnationOld || epochOf(t, last["epoch"]) != 9 {
			t.Errorf("last %v", last)
		}
	})
}

// B2: the timeout names the last row; an absent row is null; a row that is
// not live never confirms.
func TestRegistrationTimesOutNamingTheLastRow(t *testing.T) {
	t.Run("the old incarnation throughout", func(t *testing.T) {
		l := newRegLedger(t, true)
		epoch := l.register(t, "node-a", regIncarnationOld)
		n := l.polls(t, nil)

		started := time.Now()

		o := l.run(t)
		mustTimeout(t, o)

		if took := time.Since(started); took < 300*time.Millisecond || took > 5*time.Second {
			t.Errorf("the wait took %s", took)
		}

		last := asMap(o.doc["last"])
		if last["incarnation"] != regIncarnationOld || epochOf(t, last["epoch"]) != epoch || last["live"] != true {
			t.Errorf("last %v", last)
		}

		for _, member := range []string{"live", "epoch"} {
			if _, present := o.doc[member]; present {
				t.Errorf("a timeout carried the confirmation's member %s: %v", member, o.doc)
			}
		}

		if len(o.doc) != 6 {
			t.Errorf("a timeout's members: %v", o.doc)
		}

		if *n < 2 {
			t.Errorf("%d polls over a 300ms wait at a 20ms interval", *n)
		}
	})

	t.Run("the row absent", func(t *testing.T) {
		l := newRegLedger(t, true)
		l.register(t, "node-b", regIncarnationNew)

		o := l.run(t)
		mustTimeout(t, o)

		if last, present := o.doc["last"]; !present || last != nil {
			t.Errorf("last %v (present %v), want a null member", last, present)
		}
	})

	t.Run("the new incarnation not live", func(t *testing.T) {
		l := newRegLedger(t, true)
		epoch := l.register(t, "node-a", regIncarnationNew)
		l.gone(t, "node-a", epoch)

		o := l.run(t)
		mustTimeout(t, o)

		last := asMap(o.doc["last"])
		if last["incarnation"] != regIncarnationNew || last["live"] != false {
			t.Errorf("last %v", last)
		}
	})
}

// B3: the binding. Unbound refuses at once; bound to another deployment
// refuses as a foreign ledger; and the binding is REVALIDATED AT EVERY
// POLL against the identity re-read then, so an identity that moved under
// the polls is refused at the poll that sees it and never confirmed.
func TestRegistrationJudgesTheBindingAtEveryPoll(t *testing.T) {
	t.Run("unbound", func(t *testing.T) {
		l := newRegLedger(t, false)
		l.register(t, "node-a", regIncarnationNew)
		n := l.polls(t, nil)

		o := l.run(t)
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonUnbound)

		if *n != 1 {
			t.Errorf("%d polls after an unbound refusal", *n)
		}
	})

	t.Run("bound to another deployment", func(t *testing.T) {
		l := newRegLedger(t, false)
		statusPlane(t, l.stateDir, func(db *state.DB) {
			if _, err := db.ClaimController(t.Context(), "elsewhere", regDeployment2); err != nil {
				t.Fatal(err)
			}
		})
		l.register(t, "node-a", regIncarnationNew)

		// The read-only open refuses a foreign binding before any poll, as
		// the status report does: the error, nothing printed.
		o := l.run(t)
		if !errors.Is(o.err, state.ErrForeignLedger) || o.raw != "" {
			t.Errorf("err %v out %q, want ErrForeignLedger and nothing printed", o.err, o.raw)
		}
	})

	t.Run("the identity moved before the second poll", func(t *testing.T) {
		l := newRegLedger(t, true)

		n := l.polls(t, func(n int) {
			if n == 1 {
				// The row that would confirm appears, and the host's identity
				// says another deployment: a comparison at the open alone
				// confirms here.
				l.register(t, "node-a", regIncarnationNew)
				mustOK(t, os.WriteFile(filepath.Join(l.stateDir, "deployment-id"), []byte(regDeployment2+"\n"), 0o600))
			}
		})

		o := l.run(t)
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonTrust)

		if !strings.Contains(o.str("why"), state.ErrForeignLedger.Error()) || *n != 2 {
			t.Errorf("why %q polls %d", o.str("why"), *n)
		}
	})

	t.Run("the identity unreadable at a poll", func(t *testing.T) {
		l := newRegLedger(t, true)
		l.polls(t, func(n int) {
			if n == 1 {
				mustOK(t, os.WriteFile(filepath.Join(l.stateDir, "deployment-id"), []byte("not an id\n"), 0o600))
			}
		})

		o := l.run(t)
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonUnexamined)
	})

	// B3e: the command's poll is StatusSnapshot and nothing else on the
	// store, so the binding and the rows come from one transaction.
	t.Run("structure", func(t *testing.T) {
		src, err := os.ReadFile("rolloutregistration.go")
		mustOK(t, err)

		calls := regexp.MustCompile(`\bstore\.(\w+)\(`).FindAllStringSubmatch(string(src), -1)
		if len(calls) == 0 {
			t.Fatal("no call on the store")
		}

		for _, c := range calls {
			if c[1] != "StatusSnapshot" {
				t.Errorf("the command calls store.%s; the poll is StatusSnapshot alone", c[1])
			}
		}
	})
}

// B4: the open's refusals create nothing; as root over another account's
// ledger the command re-executes and the child's exit is preserved, whether
// it answered timeout, refused or unknown.
func TestRegistrationOpensNothingAndPreservesTheChildsExit(t *testing.T) {
	t.Run("an absent ledger", func(t *testing.T) {
		stateDir := t.TempDir()
		cfgPath := writeCAConfig(t, stateDir)
		mustOK(t, os.WriteFile(filepath.Join(stateDir, "deployment-id"), []byte(regDeployment2+"\n"), 0o600))

		var runErr error

		out := capture(t, func() {
			runErr = cmdRolloutRegistration(t.Context(), []string{"--json", "--config", cfgPath, "--node", "node-a",
				"--incarnation", regIncarnationNew})
		})

		if runErr == nil || out != "" {
			t.Fatalf("an absent ledger: err %v out %q", runErr, out)
		}

		if _, err := os.Stat(state.LedgerPath(stateDir)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the registration created a ledger: %v", err)
		}
	})

	l := newRegLedger(t, true)

	for _, c := range []struct {
		name string
		code int
	}{{"timeout", exitUnknown}, {"refused", exitRefused}, {"unknown", exitUnknown}, {"confirmed", 0}} {
		t.Run("the child answered "+c.name, func(t *testing.T) {
			savedEUID, savedOwner, savedReexec := statusEUID, statusOwnerOf, statusReexec
			t.Cleanup(func() { statusEUID, statusOwnerOf, statusReexec = savedEUID, savedOwner, savedReexec })

			statusEUID = func() int { return 0 }
			statusOwnerOf = func(string) (uint32, uint32, error) { return 1001, 1001, nil }

			var calls [][]string

			statusReexec = func(_ context.Context, _, _ uint32, args []string) (int, error) {
				calls = append(calls, args)

				return c.code, nil
			}

			var runErr error

			out := capture(t, func() {
				runErr = cmdRolloutRegistration(t.Context(), []string{"--json", "--config", l.cfgPath, "--node", "node-a",
					"--incarnation", regIncarnationNew})
			})

			if out != "" {
				t.Errorf("the parent printed beside the child's answer: %q", out)
			}

			if len(calls) != 1 || calls[0][0] != "rollout" || calls[0][1] != "registration" {
				t.Errorf("re-executed as %v", calls)
			}

			switch {
			case c.code == 0 && runErr != nil:
				t.Errorf("err %v for a child that exited 0", runErr)
			case c.code != 0 && exitStatus(runErr) != c.code:
				t.Errorf("exit %d (err %v), want the child's %d", exitStatus(runErr), runErr, c.code)
			}
		})
	}
}

// B5: the inputs are refused before the open.
func TestRegistrationRefusesItsInputsBeforeTheOpen(t *testing.T) {
	stateDir := t.TempDir()
	cfgPath := writeCAConfig(t, stateDir)

	for _, c := range []struct {
		name string
		args []string
	}{
		{"a non-hex incarnation", []string{"--node", "node-a", "--incarnation", "zz"}},
		{"an empty node", []string{"--incarnation", regIncarnationNew}},
		{"an invalid node name", []string{"--node", "not a name", "--incarnation", regIncarnationNew}},
		{"a zero wait", []string{"--node", "node-a", "--incarnation", regIncarnationNew, "--wait", "0"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := runEndpoint(t, cmdRolloutRegistration, "", append([]string{"--config", cfgPath}, c.args...)...)
			mustEndpointRefusal(t, o, outcomeRefused, endpointReasonCombination)
		})
	}

	if _, err := os.Stat(state.LedgerPath(stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused input opened the ledger: %v", err)
	}

	t.Run("without --json", func(t *testing.T) {
		err := cmdRolloutRegistration(t.Context(), []string{"--config", cfgPath, "--node", "node-a", "--incarnation",
			regIncarnationNew})
		if err == nil || !strings.Contains(err.Error(), "--json") {
			t.Errorf("err %v", err)
		}
	})
}

// B6, B7: the fixtures the role's parser consumes are the command's own
// answers over planted ledgers, and the unknown one is produced by a poll
// that fails through the seam.
func TestTheRegistrationFixturesAreTheCommandsOwn(t *testing.T) {
	shapes := map[string]func(t *testing.T, l *regLedger) endpointOut{
		"confirmed": func(t *testing.T, l *regLedger) endpointOut {
			t.Helper()
			l.register(t, "node-a", regIncarnationNew)

			return l.run(t)
		},
		"timeout": func(t *testing.T, l *regLedger) endpointOut {
			t.Helper()
			l.register(t, "node-a", regIncarnationOld)

			return l.run(t, "--wait", "50ms")
		},
		"timeout-absent": func(t *testing.T, l *regLedger) endpointOut {
			t.Helper()

			return l.run(t, "--wait", "50ms")
		},
		"refused-unbound": func(t *testing.T, l *regLedger) endpointOut {
			t.Helper()

			return l.run(t)
		},
		"unknown-read": func(t *testing.T, l *regLedger) endpointOut {
			t.Helper()

			prev := registrationPoll
			t.Cleanup(func() { registrationPoll = prev })

			n := 0
			registrationPoll = func(ctx context.Context, store *rollout.Store) (rollout.StatusSnapshot, error) {
				n++
				if n == 2 {
					return rollout.StatusSnapshot{}, errors.New("the ledger's disk answered EIO")
				}

				return store.StatusSnapshot(ctx)
			}

			return l.run(t)
		},
	}

	for name, produce := range shapes {
		t.Run(name, func(t *testing.T) {
			l := newRegLedger(t, name != "refused-unbound")

			o := produce(t, l)

			if name == "unknown-read" && o.str("reason") != "unexamined" {
				t.Errorf("reason %q, want the protocol's spelling unexamined", o.str("reason"))
			}

			out := strings.ReplaceAll(o.raw, l.deployment, strings.Repeat("d", 32))
			compareFixture(t, "rollout-registration", name, out)
		})
	}

	fixtureSetIs(t, "rollout-registration", fixtureNames(shapes))
}
