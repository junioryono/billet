package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/rollout"
	"github.com/junioryono/billet/internal/state"
)

// `billet rollout status --json` IS THE LEDGER'S OWN ACCOUNT, READ THROUGH AN
// INSPECTION. These fixtures pin what it says, prove it binds nothing, claims
// nothing and locks nothing, and prove a read that fails is the command's
// failure rather than an empty fleet.

const (
	statusDigestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	statusDigestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	statusDigestC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// statusPlane is a control plane open on stateDir for the length of fn, the way
// the fixtures write what the command then reads.
func statusPlane(t *testing.T, stateDir string, fn func(db *state.DB)) {
	t.Helper()

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	fn(db)

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// registerStatusNode registers one host through the allocator, the way a node
// does, returning the epoch the ledger assigned.
func registerStatusNode(t *testing.T, db *state.DB, name, release, digest, incarnation string) int64 {
	t.Helper()

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, nil)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	epoch, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: name, Provider: config.ProviderDocker, VCPU: 8, Memory: 32 * config.GiB,
		Release: release, Digest: digest, Incarnation: incarnation,
		WireMin: 12, WireMax: alloc.BarrierWireVersion, WireVersion: alloc.BarrierWireVersion,
	})
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}

	return epoch
}

// markStatusNodeGone records that the control plane gave up on a host, the
// way the plane does when a registration expires.
func markStatusNodeGone(t *testing.T, db *state.DB, name string, epoch int64) {
	t.Helper()

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, nil)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	if err := a.NodeGone(t.Context(), name, epoch); err != nil {
		t.Fatalf("NodeGone(%s): %v", name, err)
	}
}

func startStatusRollout(t *testing.T, store *rollout.Store, target, digest string, nodes ...string) *rollout.Rollout {
	t.Helper()

	r, err := store.Start(t.Context(), rollout.StartRequest{
		Channel: "stable", TargetVersion: target, TargetDigest: digest, PriorVersion: "v0.9.3",
		Policy:    rollout.Policy{Cohort: 2, FailureBudget: 1, AllowDowngrade: false},
		CreatedBy: "ops", Nodes: nodes,
	})
	if err != nil {
		t.Fatalf("Start %s: %v", target, err)
	}

	return r
}

func advanceStatus(t *testing.T, store *rollout.Store, req rollout.AdvanceRequest) {
	t.Helper()

	if err := store.Advance(t.Context(), req); err != nil {
		t.Fatalf("Advance %s to %s: %v", req.Node, req.To, err)
	}
}

// statusJSON runs the command with --json and decodes what it printed.
func statusJSON(t *testing.T, cfgPath string, extra ...string) (rolloutStatusReport, map[string]any, string) {
	t.Helper()

	var runErr error

	out := capture(t, func() {
		runErr = cmdRolloutStatus(t.Context(), append([]string{"--json", "--config", cfgPath}, extra...))
	})

	if runErr != nil {
		t.Fatalf("rollout status --json: %v", runErr)
	}

	var report rolloutStatusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, out)
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}

	return report, raw, out
}

// objectAt is the object at key inside an object, or a failed test.
func objectAt(t *testing.T, v any, key string) any {
	t.Helper()

	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%v is not an object", v)
	}

	return m[key]
}

// firstOf is the first element of an array, or a failed test.
func firstOf(t *testing.T, v any) any {
	t.Helper()

	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("%v is not a non-empty array", v)
	}

	return list[0]
}

func keysOf(t *testing.T, v any) []string {
	t.Helper()

	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%v is not an object", v)
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

func requireRFC3339(t *testing.T, what, value string) {
	t.Helper()

	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		t.Errorf("%s is %q, not a timestamp", what, value)
	}
}

// THE REPORT IS PINNED, VALUES AND FIELD NAMES: the open rollout is the one
// reported when one is open, with every node of that rollout and the refusal
// each carries, every registered host's current registration whether or not a
// rollout names it, and the ledger's binding. The field set is asserted by name,
// because a consumer reads these names and a renamed field under the same
// schema number is a broken consumer.
func TestRolloutStatusJSONPinsTheReport(t *testing.T) {
	stateDir := t.TempDir()
	cfgPath := writeCAConfig(t, stateDir)

	deployment, err := state.DeploymentID(stateDir)
	if err != nil {
		t.Fatal(err)
	}

	var (
		epochs  = map[string]int64{}
		open    *rollout.Rollout
		aborted *rollout.Rollout
	)

	statusPlane(t, stateDir, func(db *state.DB) {
		if _, err := db.ClaimController(t.Context(), "billet-control-01", deployment); err != nil {
			t.Fatalf("ClaimController: %v", err)
		}

		// Three registrations of one host from three processes, one of another,
		// one of a host no rollout names; a rollout member that never registered.
		registerStatusNode(t, db, "epyc-1", "v0.9.3", statusDigestA, "inc-1a")
		registerStatusNode(t, db, "epyc-1", "v0.9.3", statusDigestA, "inc-1b")
		epochs["epyc-1"] = registerStatusNode(t, db, "epyc-1", "v0.9.4", statusDigestB, "inc-1c")
		// epyc-2 came back on an OLDER release than it once ran, so its highest
		// release is not its current one; outside-1 is OFFLINE, and stays reported.
		registerStatusNode(t, db, "epyc-2", "v0.9.4", statusDigestB, "inc-2z")
		epochs["epyc-2"] = registerStatusNode(t, db, "epyc-2", "v0.9.3", statusDigestA, "inc-2a")
		epochs["outside-1"] = registerStatusNode(t, db, "outside-1", "v0.9.3", statusDigestA, "inc-o")
		markStatusNodeGone(t, db, "outside-1", epochs["outside-1"])

		store := rollout.New(db)

		// Generation 1: completed. Generation 2: aborted, with a refusal on
		// epyc-1 that must not leak into generation 3's report. Generation 3:
		// open, with distinct refusals and details per node.
		completed := startStatusRollout(t, store, "v0.9.3", statusDigestA, "epyc-1")
		for _, phase := range []rollout.Phase{rollout.PhaseDraining, rollout.PhaseReadyToInstall,
			rollout.PhaseInstalling, rollout.PhaseVerifying, rollout.PhaseCommitted} {
			advanceStatus(t, store, rollout.AdvanceRequest{RolloutID: completed.ID, To: phase})
			advanceStatus(t, store, rollout.AdvanceRequest{RolloutID: completed.ID, Node: "epyc-1", To: phase})
		}

		if err := store.Finish(t.Context(), completed.ID, rollout.StateCompleted, "every host converged"); err != nil {
			t.Fatalf("Finish completed: %v", err)
		}

		aborted = startStatusRollout(t, store, "v0.9.4", statusDigestB, "epyc-1", "epyc-2")
		advanceStatus(t, store, rollout.AdvanceRequest{RolloutID: aborted.ID, Node: "epyc-1", To: rollout.PhasePending,
			Backoff: time.Hour, Refusal: "the older refusal, in the aborted rollout"})

		if err := store.Finish(t.Context(), aborted.ID, rollout.StateAborted, "the operator changed their mind"); err != nil {
			t.Fatalf("Finish aborted: %v", err)
		}

		open = startStatusRollout(t, store, "v0.9.4", statusDigestC, "epyc-1", "epyc-2", "never-registered")
		advanceStatus(t, store, rollout.AdvanceRequest{RolloutID: open.ID, To: rollout.PhaseDraining})
		advanceStatus(t, store, rollout.AdvanceRequest{RolloutID: open.ID, Node: "epyc-1", To: rollout.PhasePending,
			Backoff: time.Hour, Refusal: "the host holds a converge guard for holder ci-42"})
		advanceStatus(t, store, rollout.AdvanceRequest{RolloutID: open.ID, Node: "epyc-2", To: rollout.PhasePending,
			Backoff: time.Hour, Refusal: "the node did not answer"})
		advanceStatus(t, store, rollout.AdvanceRequest{RolloutID: open.ID, Node: "epyc-2", To: rollout.PhaseDraining,
			DispatchEpoch: epochs["epyc-2"], ClearRefusal: true, PriorRelease: "v0.9.3"})
		advanceStatus(t, store, rollout.AdvanceRequest{RolloutID: open.ID, Node: "never-registered",
			To: rollout.PhaseExempt, ExemptReason: "retired before the rollout"})
	})

	report, raw, out := statusJSON(t, cfgPath)

	for _, n := range report.Nodes {
		requireRFC3339(t, "nodes."+n.Node+".updated_at", n.UpdatedAt)
	}

	if report.Rollout == nil {
		t.Fatalf("no rollout in the report:\n%s", out)
	}

	requireRFC3339(t, "rollout.created_at", report.Rollout.CreatedAt)

	if report.Nodes[0].NextAttemptAt == "" {
		t.Error("epyc-1's next attempt is empty after a backoff")
	}

	requireRFC3339(t, "nodes[0].next_attempt_at", report.Nodes[0].NextAttemptAt)

	want := rolloutStatusReport{
		Schema:     1,
		Deployment: rolloutStatusDeployment{Bound: true, ID: deployment},
		Rollout: &rolloutStatusRollout{
			ID: open.ID, Generation: 3, State: rollout.StateOpen, Channel: "stable",
			TargetVersion: "v0.9.4", TargetDigest: statusDigestC, PriorVersion: "v0.9.3",
			ControllerPhase: string(rollout.PhaseDraining),
			Policy:          rolloutStatusPolicy{Cohort: 2, FailureBudget: 1, AllowDowngrade: false},
			CreatedBy:       "ops", CreatedAt: report.Rollout.CreatedAt, FinishedAt: "", TerminalReason: "",
		},
		Nodes: []rolloutStatusNode{
			{Node: "epyc-1", Phase: string(rollout.PhasePending), Attempts: 1,
				NextAttemptAt: report.Nodes[0].NextAttemptAt, UpdatedAt: report.Nodes[0].UpdatedAt,
				LastRefusal: "the host holds a converge guard for holder ci-42"},
			{Node: "epyc-2", Phase: string(rollout.PhaseDraining), Attempts: 1,
				NextAttemptAt: report.Nodes[1].NextAttemptAt, UpdatedAt: report.Nodes[1].UpdatedAt,
				PriorRelease: "v0.9.3", DispatchEpoch: epochs["epyc-2"], LastRefusal: ""},
			{Node: "never-registered", Phase: string(rollout.PhaseExempt),
				UpdatedAt: report.Nodes[2].UpdatedAt, ExemptReason: "retired before the rollout"},
		},
		Registrations: []rolloutStatusRegistration{
			{Name: "epyc-1", Live: true, Epoch: epochs["epyc-1"], Incarnation: "inc-1c",
				Release: "v0.9.4", Digest: statusDigestB, HighestRelease: "v0.9.4"},
			{Name: "epyc-2", Live: true, Epoch: epochs["epyc-2"], Incarnation: "inc-2a",
				Release: "v0.9.3", Digest: statusDigestA, HighestRelease: "v0.9.4"},
			{Name: "outside-1", Live: false, Epoch: epochs["outside-1"], Incarnation: "inc-o",
				Release: "v0.9.3", Digest: statusDigestA, HighestRelease: "v0.9.3"},
		},
	}

	if epochs["epyc-1"] != 3 || epochs["epyc-2"] != 2 {
		t.Errorf("the registrations gave epochs %v, want epyc-1 3 and epyc-2 2", epochs)
	}

	// THE JSON SAYS SO TOO, by membership: an offline host is in the array with
	// live false, and a host's highest release is its own field with its own value.
	if !strings.Contains(out, `"name": "outside-1"`) || !strings.Contains(out, `"live": false`) {
		t.Errorf("the offline host is not reported offline:\n%s", out)
	}

	if !strings.Contains(out, `"highest_release": "v0.9.4"`) || strings.Count(out, `"highest_release": "v0.9.3"`) != 1 {
		t.Errorf("the highest releases are not reported as their own values:\n%s", out)
	}

	if !reflect.DeepEqual(report, want) {
		t.Errorf("the report is not the pinned one.\n got: %+v\nwant: %+v\n\n%s", report, want, out)
	}

	// THE FIELD NAMES, by name.
	wantKeys := map[string][]string{
		"":           {"deployment", "nodes", "registrations", "rollout", "schema"},
		"deployment": {"bound", "id"},
		"rollout": {"channel", "controller_phase", "created_at", "created_by", "finished_at", "generation",
			"id", "policy", "prior_version", "state", "target_digest", "target_version", "terminal_reason"},
		"rollout.policy": {"allow_downgrade", "cohort", "failure_budget"},
		"nodes[0]": {"attempts", "blocker", "converged_digest", "dispatch_epoch", "exempt_reason", "last_refusal",
			"next_attempt_at", "node", "phase", "prior_release", "rollback_result", "updated_at"},
		"registrations[0]": {"digest", "epoch", "highest_release", "incarnation", "live", "name", "release"},
	}

	got := map[string][]string{
		"":                 keysOf(t, raw),
		"deployment":       keysOf(t, raw["deployment"]),
		"rollout":          keysOf(t, raw["rollout"]),
		"rollout.policy":   keysOf(t, objectAt(t, raw["rollout"], "policy")),
		"nodes[0]":         keysOf(t, firstOf(t, raw["nodes"])),
		"registrations[0]": keysOf(t, firstOf(t, raw["registrations"])),
	}

	for object, keys := range wantKeys {
		if !reflect.DeepEqual(got[object], keys) {
			t.Errorf("%s has fields %v, want %v", object, got[object], keys)
		}
	}

	// WITHOUT AN OPEN ROLLOUT THE NEWEST FINISHED ONE IS REPORTED, aborted or
	// not, with ITS nodes and ITS refusals: the same host name in another
	// rollout carries another record.
	statusPlane(t, stateDir, func(db *state.DB) {
		if err := rollout.New(db).Finish(t.Context(), open.ID, rollout.StateAborted, "rehearsal over"); err != nil {
			t.Fatalf("Finish open: %v", err)
		}
	})

	report, _, out = statusJSON(t, cfgPath)

	if report.Rollout == nil || report.Rollout.ID != open.ID || report.Rollout.State != rollout.StateAborted ||
		report.Rollout.TerminalReason != "rehearsal over" || report.Rollout.Generation != 3 {
		t.Fatalf("after the open rollout was aborted the report names %+v, want generation 3 aborted\n%s",
			report.Rollout, out)
	}

	requireRFC3339(t, "rollout.finished_at", report.Rollout.FinishedAt)

	// And an older finished rollout is reported only when it is the newest.
	statusPlane(t, stateDir, func(db *state.DB) {
		if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
			// The newest generation is deleted by the test alone, to make
			// generation 2 the newest without another rollout's writer involved.
			_, err := tx.ExecContext(t.Context(), `DELETE FROM rollout_nodes WHERE rollout_id = ?`, open.ID)
			if err != nil {
				return err
			}

			_, err = tx.ExecContext(t.Context(), `DELETE FROM rollouts WHERE id = ?`, open.ID)

			return err
		}); err != nil {
			t.Fatalf("remove generation 3: %v", err)
		}
	})

	report, _, out = statusJSON(t, cfgPath)

	if report.Rollout == nil || report.Rollout.ID != aborted.ID || report.Rollout.Generation != 2 {
		t.Fatalf("with generation 3 gone the report names %+v, want generation 2\n%s", report.Rollout, out)
	}

	if len(report.Nodes) != 2 || report.Nodes[0].Node != "epyc-1" ||
		report.Nodes[0].LastRefusal != "the older refusal, in the aborted rollout" || report.Nodes[1].LastRefusal != "" {
		t.Errorf("generation 2's nodes are %+v", report.Nodes)
	}

	if len(report.Registrations) != 3 {
		t.Errorf("the registrations changed with the rollout: %+v", report.Registrations)
	}
}

// A LEDGER WITH NOTHING IN IT REPORTS NOTHING, POSITIVELY: null for the rollout,
// empty arrays for the hosts, and an unbound deployment.
func TestRolloutStatusJSONReportsAnEmptyLedgerPositively(t *testing.T) {
	stateDir := t.TempDir()
	cfgPath := writeCAConfig(t, stateDir)

	statusPlane(t, stateDir, func(*state.DB) {})

	report, raw, out := statusJSON(t, cfgPath)

	if report.Rollout != nil || raw["rollout"] != nil {
		t.Errorf("an empty ledger reports a rollout: %s", out)
	}

	if nodes, ok := raw["nodes"].([]any); !ok || len(nodes) != 0 {
		t.Errorf("nodes is %v, want []", raw["nodes"])
	}

	if regs, ok := raw["registrations"].([]any); !ok || len(regs) != 0 {
		t.Errorf("registrations is %v, want []", raw["registrations"])
	}

	if report.Deployment.Bound || report.Deployment.ID != "" {
		t.Errorf("an unbound ledger reports %+v", report.Deployment)
	}
}

// THE BINDING IS THE LEDGER'S, NEVER THE HOST'S IDENTITY FILE: an identity
// beside an unbound ledger is not reported as a binding and is not written into
// one; a bound ledger with no identity file reports the ledger's id and mints
// nothing; and a ledger bound to another deployment is refused, as every
// operator command refuses it.
func TestRolloutStatusReportsTheLedgersBindingAndNeverMintsOne(t *testing.T) {
	t.Run("identity present, ledger unbound", func(t *testing.T) {
		stateDir := t.TempDir()
		cfgPath := writeCAConfig(t, stateDir)

		if _, err := state.DeploymentID(stateDir); err != nil {
			t.Fatal(err)
		}

		statusPlane(t, stateDir, func(*state.DB) {})

		report, _, _ := statusJSON(t, cfgPath)

		if report.Deployment.Bound || report.Deployment.ID != "" {
			t.Errorf("the host's identity was reported as the ledger's binding: %+v", report.Deployment)
		}

		statusPlane(t, stateDir, func(db *state.DB) {
			if got, err := db.DeploymentBinding(t.Context()); err != nil || got != "" {
				t.Errorf("after the status the ledger is bound to %q (err %v)", got, err)
			}
		})
	})

	t.Run("ledger bound, identity absent", func(t *testing.T) {
		stateDir := t.TempDir()
		cfgPath := writeCAConfig(t, stateDir)

		deployment, err := state.DeploymentID(stateDir)
		if err != nil {
			t.Fatal(err)
		}

		statusPlane(t, stateDir, func(db *state.DB) {
			if _, err := db.ClaimController(t.Context(), "billet-control-01", deployment); err != nil {
				t.Fatal(err)
			}
		})

		identity := filepath.Join(stateDir, "deployment-id")
		if err := os.Remove(identity); err != nil {
			t.Fatal(err)
		}

		report, _, _ := statusJSON(t, cfgPath)

		if !report.Deployment.Bound || report.Deployment.ID != deployment {
			t.Errorf("the ledger's binding was not reported: %+v", report.Deployment)
		}

		if _, err := os.Lstat(identity); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the status minted an identity file (lstat err = %v)", err)
		}
	})

	t.Run("both present and different", func(t *testing.T) {
		stateDir := t.TempDir()
		cfgPath := writeCAConfig(t, stateDir)

		statusPlane(t, stateDir, func(db *state.DB) {
			if _, err := db.ClaimController(t.Context(), "billet-control-01",
				"0123456789abcdef0123456789abcdef"); err != nil {
				t.Fatal(err)
			}
		})

		if err := os.WriteFile(filepath.Join(stateDir, "deployment-id"),
			[]byte("fedcba9876543210fedcba9876543210\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		var runErr error

		out := capture(t, func() {
			runErr = cmdRolloutStatus(t.Context(), []string{"--json", "--config", cfgPath})
		})

		if !errors.Is(runErr, state.ErrForeignLedger) {
			t.Errorf("a foreign ledger: err = %v, want ErrForeignLedger", runErr)
		}

		if out != "" {
			t.Errorf("a refused status printed a report:\n%s", out)
		}
	})
}

// THE COMMAND OPENS THROUGH THE INSPECTION AND NOTHING ELSE, read structurally
// (no operator open, no store helper, no direct state open anywhere in its
// body), and behaviourally: beside a control plane it reads, and the directory
// lock it never took is still the control plane's afterwards; with nobody
// holding the ledger, a control plane can still start afterwards.
func TestRolloutStatusIsAnInspection(t *testing.T) {
	t.Run("structure", func(t *testing.T) {
		fset := token.NewFileSet()

		file, err := parser.ParseFile(fset, "rollout.go", nil, 0)
		if err != nil {
			t.Fatal(err)
		}

		var status *ast.FuncDecl

		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "cmdRolloutStatus" {
				status = fn
			}
		}

		if status == nil {
			t.Fatal("cmdRolloutStatus was not found in rollout.go")
		}

		inspects := 0

		ast.Inspect(status.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			switch fn := call.Fun.(type) {
			case *ast.Ident:
				switch fn.Name {
				case "openStateInspect":
					inspects++
				case "openStateAdmin", "rolloutStore", "openStateForDecision", "openStateMaintenance",
					"openStateStandby", "openState":
					t.Errorf("%s: cmdRolloutStatus opens through %s", fset.Position(call.Pos()), fn.Name)
				}
			case *ast.SelectorExpr:
				if pkg, name, ok := selector(fn); ok && pkg == "state" && strings.HasPrefix(name, "Open") {
					t.Errorf("%s: cmdRolloutStatus opens through state.%s directly", fset.Position(call.Pos()), name)
				}
			}

			return true
		})

		if inspects != 1 {
			t.Errorf("cmdRolloutStatus calls openStateInspect %d times, want exactly once", inspects)
		}
	})

	t.Run("beside a control plane", func(t *testing.T) {
		stateDir := t.TempDir()
		cfgPath := writeCAConfig(t, stateDir)

		plane, err := state.Open(t.Context(), stateDir)
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = plane.Close() })

		registerStatusNode(t, plane, "epyc-1", "v0.9.3", statusDigestA, "inc-1")

		report, _, _ := statusJSON(t, cfgPath)

		if len(report.Registrations) != 1 || report.Registrations[0].Name != "epyc-1" {
			t.Errorf("beside a control plane the report holds %+v", report.Registrations)
		}

		// A later commit is visible to a later status, and the lock is still the
		// control plane's.
		registerStatusNode(t, plane, "epyc-2", "v0.9.3", statusDigestA, "inc-2")

		report, _, _ = statusJSON(t, cfgPath)

		if len(report.Registrations) != 2 {
			t.Errorf("a commit made after the first status is missing: %+v", report.Registrations)
		}

		if _, err := state.Open(t.Context(), stateDir); !errors.Is(err, state.ErrLocked) {
			t.Errorf("a second control plane after two status reads: err = %v, want ErrLocked; "+
				"the status released a lock it never held", err)
		}
	})

	t.Run("with nobody holding the ledger", func(t *testing.T) {
		stateDir := t.TempDir()
		cfgPath := writeCAConfig(t, stateDir)

		statusPlane(t, stateDir, func(*state.DB) {})

		if _, _, out := statusJSON(t, cfgPath); !strings.Contains(out, `"schema": 1`) {
			t.Errorf("the report lacks its schema:\n%s", out)
		}

		plane, err := state.Open(t.Context(), stateDir)
		if err != nil {
			t.Fatalf("a control plane could not start after a status read: %v", err)
		}

		_ = plane.Close()
	})
}

// sqliteExec runs one statement against a closed ledger through the driver the
// state package registered, the way another binary would.
func sqliteExec(t *testing.T, stateDir, stmt string, args ...any) {
	t.Helper()

	raw, err := sql.Open("sqlite", "file:"+filepath.Join(stateDir, "billet.db"))
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = raw.Close() }()

	if _, err := raw.ExecContext(t.Context(), stmt, args...); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}

// A LEDGER BEHIND THIS BINARY IS REFUSED, NAMING THE CONTROL PLANE'S RESTART,
// WITH NOTHING PRINTED AND NOTHING MIGRATED: a status is not permission to move
// the schema, and a report over a half-known schema is not a report.
func TestRolloutStatusRefusesALedgerBehindItsBinaryWithoutMigrating(t *testing.T) {
	stateDir := t.TempDir()
	cfgPath := writeCAConfig(t, stateDir)

	statusPlane(t, stateDir, func(*state.DB) {})

	latest := state.LatestSchemaVersion()

	sqliteExec(t, stateDir, `DELETE FROM schema_migrations WHERE version = ?`, latest)

	before, err := state.PeekMigrations(t.Context(), filepath.Join(stateDir, "billet.db"))
	if err != nil {
		t.Fatal(err)
	}

	var runErr error

	out := capture(t, func() {
		runErr = cmdRolloutStatus(t.Context(), []string{"--json", "--config", cfgPath})
	})

	if !errors.Is(runErr, state.ErrSchemaBehind) {
		t.Fatalf("a behind ledger: err = %v, want ErrSchemaBehind", runErr)
	}

	if !strings.Contains(runErr.Error(), "restart the control plane") {
		t.Errorf("the refusal does not name the restart: %v", runErr)
	}

	if out != "" {
		t.Errorf("a refused status printed:\n%s", out)
	}

	after, err := state.PeekMigrations(t.Context(), filepath.Join(stateDir, "billet.db"))
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(before, after) || after[len(after)-1].Version != latest-1 {
		t.Errorf("the refused status migrated the ledger: %d rows, newest %d", len(after), after[len(after)-1].Version)
	}
}

// afterOpen installs the seam that runs between the command's open and its one
// read, and restores it afterwards. Not parallel: the seam is package state.
func afterOpen(t *testing.T, fn func(db *state.DB)) {
	t.Helper()

	statusAfterOpen = fn

	t.Cleanup(func() { statusAfterOpen = nil })
}

// A READ THAT FAILS IS THE COMMAND'S FAILURE, never an empty fleet or an
// unbound deployment: a fence raised AFTER the open, so the report's own read
// is what fails, refuses the report with nothing printed; and so does a fence
// raised before the open.
func TestRolloutStatusFailsRatherThanReportingAnEmptyFleet(t *testing.T) {
	stateDir := t.TempDir()
	cfgPath := writeCAConfig(t, stateDir)

	statusPlane(t, stateDir, func(db *state.DB) {
		registerStatusNode(t, db, "epyc-1", "v0.9.3", statusDigestA, "inc-1")
	})

	for name, when := range map[string]string{"after the open": "after", "before the open": "before"} {
		t.Run(name, func(t *testing.T) {
			raised := false

			if when == "after" {
				afterOpen(t, func(*state.DB) {
					if _, err := state.WriteMaintenanceFence(stateDir, "host upgrade"); err != nil {
						t.Fatal(err)
					}

					raised = true
				})
			} else if _, err := state.WriteMaintenanceFence(stateDir, "host upgrade"); err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() {
				if err := state.ClearMaintenanceFence(stateDir, "host upgrade"); err != nil {
					t.Errorf("clear the fence: %v", err)
				}
			})

			var runErr error

			out := capture(t, func() {
				runErr = cmdRolloutStatus(t.Context(), []string{"--json", "--config", cfgPath})
			})

			if when == "after" && !raised {
				t.Fatal("the seam never ran, so the fence was never raised after the open")
			}

			if !errors.Is(runErr, state.ErrMaintenance) {
				t.Errorf("a fenced ledger: err = %v, want ErrMaintenance", runErr)
			}

			if out != "" {
				t.Errorf("a refused status printed:\n%s", out)
			}
		})
	}
}

// THE REPORT IS ONE SNAPSHOT, AND THE BINDING IS COMPARED INSIDE IT: a ledger
// bound to another deployment between the open and the read is refused, not
// printed under this host's identity; and structurally the report reads through
// the store's one snapshot and no other read of the store or the handle.
func TestRolloutStatusReadsOneSnapshotAndRefusesABindingThatMovedUnderIt(t *testing.T) {
	t.Run("a binding written after the open", func(t *testing.T) {
		stateDir := t.TempDir()
		cfgPath := writeCAConfig(t, stateDir)

		if _, err := state.DeploymentID(stateDir); err != nil {
			t.Fatal(err)
		}

		statusPlane(t, stateDir, func(*state.DB) {})

		bound := false

		afterOpen(t, func(*state.DB) {
			// The inspection holds no lock, so a control plane can claim beside
			// it and bind the ledger to a deployment that is not this host's.
			statusPlane(t, stateDir, func(db *state.DB) {
				if _, err := db.ClaimController(t.Context(), "elsewhere", "fedcba9876543210fedcba9876543210"); err != nil {
					t.Fatal(err)
				}
			})

			bound = true
		})

		var runErr error

		out := capture(t, func() {
			runErr = cmdRolloutStatus(t.Context(), []string{"--json", "--config", cfgPath})
		})

		if !bound {
			t.Fatal("the seam never ran")
		}

		if !errors.Is(runErr, state.ErrForeignLedger) {
			t.Errorf("a binding that moved under the report: err = %v, want ErrForeignLedger", runErr)
		}

		if out != "" {
			t.Errorf("a refused status printed:\n%s", out)
		}
	})

	t.Run("structure", func(t *testing.T) {
		fset := token.NewFileSet()

		file, err := parser.ParseFile(fset, "rolloutstatus.go", nil, 0)
		if err != nil {
			t.Fatal(err)
		}

		var build *ast.FuncDecl

		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "buildRolloutStatusReport" {
				build = fn
			}
		}

		if build == nil {
			t.Fatal("buildRolloutStatusReport was not found")
		}

		snapshots := 0

		ast.Inspect(build.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			switch sel.Sel.Name {
			case "StatusSnapshot":
				snapshots++
			case "Open", "History", "Nodes", "Registrations", "DeploymentBinding", "View", "Reader":
				t.Errorf("%s: the report reads through %s outside the snapshot", fset.Position(call.Pos()), sel.Sel.Name)
			}

			return true
		})

		if snapshots != 1 {
			t.Errorf("the report takes %d snapshots, want exactly one", snapshots)
		}
	})
}

// writePostgresConfig is a control-plane config whose ledger is in PostgreSQL,
// named through an environment variable.
func writePostgresConfig(t *testing.T, identityDir string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "billet.yaml")

	body := `
server:
  listen: 127.0.0.1:7717
  identity_dir: ` + identityDir + `
  state:
    backend: postgres
    postgres:
      dsn_env: BILLET_STATE_DSN
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: /tmp/key.pem
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func writeEnvFile(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "server.env")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// THE ENVIRONMENT FILE IS READ WHENEVER IT IS NAMED, UNDER THE RESTRICTED
// GRAMMAR, AND ITS VALUE REACHES NO OUTPUT: on SQLite a file outside the grammar
// refuses and a file inside it changes nothing; on PostgreSQL the file's value
// is the one used, over the process environment, a missing variable is named,
// and neither a parse failure nor a connection failure prints the password.
func TestRolloutStatusReadsTheDSNFromTheEnvironmentFile(t *testing.T) {
	t.Run("sqlite ignores the value and still refuses the grammar", func(t *testing.T) {
		stateDir := t.TempDir()
		cfgPath := writeCAConfig(t, stateDir)
		statusPlane(t, stateDir, func(*state.DB) {})

		good := writeEnvFile(t, "# the unit's file\nBILLET_STATE_DSN=postgres://u:FILESENTINEL@127.0.0.1:1/db\n")

		if _, _, out := statusJSON(t, cfgPath, "--environment-file", good); strings.Contains(out, "FILESENTINEL") {
			t.Errorf("the file's value reached the report:\n%s", out)
		}

		bad := writeEnvFile(t, "BILLET_STATE_DSN=x\nBILLET_STATE_DSN=y\n")

		var runErr error

		out := capture(t, func() {
			runErr = cmdRolloutStatus(t.Context(), []string{"--json", "--config", cfgPath, "--environment-file", bad})
		})

		if runErr == nil || !strings.Contains(runErr.Error(), "read "+bad) {
			t.Errorf("a file outside the grammar on SQLite: err = %v", runErr)
		}

		if out != "" {
			t.Errorf("a refused status printed:\n%s", out)
		}
	})

	t.Run("postgres uses the file over the environment", func(t *testing.T) {
		cfgPath := writePostgresConfig(t, t.TempDir())

		// The environment holds a DSN that cannot be parsed; the file holds one
		// that parses and cannot connect. A connection failure proves the file
		// was the source.
		t.Setenv("BILLET_STATE_DSN", "postgres://u:AMBIENTSENTINEL@[bad/db")

		file := writeEnvFile(t, "BILLET_STATE_DSN=postgres://u:FILESENTINEL@127.0.0.1:1/db?sslmode=disable&connect_timeout=2\n")

		var runErr error

		out := capture(t, func() {
			runErr = cmdRolloutStatus(t.Context(), []string{"--json", "--config", cfgPath, "--environment-file", file})
		})

		if runErr == nil {
			t.Fatal("a DSN pointing at port 1 connected")
		}

		if strings.Contains(runErr.Error(), "parse") || strings.Contains(runErr.Error(), "missing ']'") {
			t.Errorf("the ambient DSN was used instead of the file's: %v", runErr)
		}

		for _, sentinel := range []string{"FILESENTINEL", "AMBIENTSENTINEL"} {
			if strings.Contains(runErr.Error(), sentinel) || strings.Contains(out, sentinel) {
				t.Errorf("the password %s reached the output: %v\n%s", sentinel, runErr, out)
			}
		}

		// The other way round: the file's DSN cannot be parsed, so the parse
		// failure proves the file was read and the ambient value never tried.
		t.Setenv("BILLET_STATE_DSN", "postgres://u:AMBIENTSENTINEL@127.0.0.1:1/db")

		file = writeEnvFile(t, "BILLET_STATE_DSN=postgres://u:FILESENTINEL@[bad/db\n")

		out = capture(t, func() {
			runErr = cmdRolloutStatus(t.Context(), []string{"--json", "--config", cfgPath, "--environment-file", file})
		})

		if runErr == nil || !strings.Contains(runErr.Error(), "parse") {
			t.Errorf("an unparsable file DSN: err = %v, want a parse failure", runErr)
		}

		for _, sentinel := range []string{"FILESENTINEL", "AMBIENTSENTINEL"} {
			if runErr != nil && strings.Contains(runErr.Error(), sentinel) || strings.Contains(out, sentinel) {
				t.Errorf("the password %s reached the output: %v\n%s", sentinel, runErr, out)
			}
		}
	})

	t.Run("postgres names a variable the file does not set", func(t *testing.T) {
		cfgPath := writePostgresConfig(t, t.TempDir())

		t.Setenv("BILLET_STATE_DSN", "postgres://u:AMBIENTSENTINEL@127.0.0.1:1/db")

		for _, body := range []string{"OTHER=1\n", "BILLET_STATE_DSN=\n"} {
			file := writeEnvFile(t, body)

			var runErr error

			out := capture(t, func() {
				runErr = cmdRolloutStatus(t.Context(), []string{"--json", "--config", cfgPath, "--environment-file", file})
			})

			if runErr == nil || !strings.Contains(runErr.Error(), "BILLET_STATE_DSN") ||
				!strings.Contains(runErr.Error(), file) {
				t.Errorf("a file that does not set the variable (%q): err = %v", body, runErr)
			}

			if strings.Contains(out, "AMBIENTSENTINEL") || (runErr != nil && strings.Contains(runErr.Error(), "AMBIENTSENTINEL")) {
				t.Errorf("the ambient DSN was used or printed: %v\n%s", runErr, out)
			}
		}
	})
}

// THE REPORT RUNS AS THE LEDGER'S OWNER AND AS NOBODY ELSE: root over another
// account's directory re-executes with this command's own arguments and runs
// nothing here; the owner runs in place; and another account is refused
// before the ledger is opened, because the sidecars a read-only open of a
// stopped SQLite ledger creates belong to whoever opened it.
func TestRolloutStatusRunsAsTheLedgersOwner(t *testing.T) {
	stateDir := t.TempDir()
	cfgPath := writeCAConfig(t, stateDir)

	statusPlane(t, stateDir, func(*state.DB) {})

	seams := func(t *testing.T, euid int, uid, gid uint32, ownerErr error, code int, reexecErr error) *[][]string {
		t.Helper()

		savedEUID, savedOwner, savedReexec := statusEUID, statusOwnerOf, statusReexec
		t.Cleanup(func() { statusEUID, statusOwnerOf, statusReexec = savedEUID, savedOwner, savedReexec })

		statusEUID = func() int { return euid }
		statusOwnerOf = func(path string) (uint32, uint32, error) {
			if path != stateDir {
				t.Errorf("the owner of %s was asked, want %s", path, stateDir)
			}

			return uid, gid, ownerErr
		}

		var calls [][]string

		statusReexec = func(_ context.Context, u, g uint32, args []string) (int, error) {
			if u != uid || g != gid {
				t.Errorf("re-executed as %d:%d, want %d:%d", u, g, uid, gid)
			}

			calls = append(calls, args)

			return code, reexecErr
		}

		return &calls
	}

	t.Run("root over another account's directory re-executes", func(t *testing.T) {
		calls := seams(t, 0, 1001, 1001, nil, 0, nil)

		var runErr error

		out := capture(t, func() {
			runErr = cmdRolloutStatus(t.Context(), []string{"--json", "--config", cfgPath, "--environment-file", "/nonexistent"})
		})

		if runErr != nil {
			t.Fatalf("err = %v", runErr)
		}

		want := [][]string{{"rollout", "status", "--json", "--config", cfgPath, "--environment-file", "/nonexistent"}}
		if !reflect.DeepEqual(*calls, want) {
			t.Errorf("re-executed with %v, want %v", *calls, want)
		}

		// NOTHING RAN HERE: the environment file that does not exist would have
		// refused an in-place run, and the report was not printed by this process.
		if out != "" {
			t.Errorf("the parent printed a report beside the child's:\n%s", out)
		}
	})

	t.Run("a child that failed is the command's failure", func(t *testing.T) {
		seams(t, 0, 1001, 1001, nil, 3, nil)

		err := cmdRolloutStatus(t.Context(), []string{"--json", "--config", cfgPath})
		if !errors.Is(err, errStatusChildFailed) || !strings.Contains(err.Error(), "exit status 3") {
			t.Errorf("err = %v", err)
		}

		seams(t, 0, 1001, 1001, nil, 0, errors.New("fork refused"))

		if err := cmdRolloutStatus(t.Context(), []string{"--json", "--config", cfgPath}); err == nil ||
			!strings.Contains(err.Error(), "fork refused") {
			t.Errorf("err = %v", err)
		}
	})

	for name, tc := range map[string]struct {
		euid     int
		uid      uint32
		ownerErr error
	}{
		"root over a root-owned directory": {euid: 0, uid: 0},
		"the owner":                        {euid: 1001, uid: 1001},
		"root, directory absent":           {euid: 0, ownerErr: fs.ErrNotExist},
	} {
		t.Run(name+" runs in place", func(t *testing.T) {
			calls := seams(t, tc.euid, tc.uid, tc.uid, tc.ownerErr, 0, nil)

			var runErr error

			out := capture(t, func() {
				runErr = cmdRolloutStatus(t.Context(), []string{"--json", "--config", cfgPath})
			})

			if len(*calls) != 0 {
				t.Errorf("re-executed: %v", *calls)
			}

			if tc.ownerErr != nil {
				// The directory exists in this fixture; the seam said it did not,
				// and the open is what answers, in place.
				if runErr != nil {
					t.Errorf("err = %v", runErr)
				}
			} else if runErr != nil || !strings.Contains(out, `"schema": 1`) {
				t.Errorf("err = %v, out:\n%s", runErr, out)
			}
		})
	}

	t.Run("another account with access to the directory is refused before the open", func(t *testing.T) {
		calls := seams(t, 501, 1001, 1001, nil, 0, nil)

		opened := false
		savedAfterOpen := statusAfterOpen
		statusAfterOpen = func(*state.DB) { opened = true }

		t.Cleanup(func() { statusAfterOpen = savedAfterOpen })

		err := cmdRolloutStatus(t.Context(), []string{"--json", "--config", cfgPath})
		if err == nil || !strings.Contains(err.Error(), "owned by uid 1001") || !strings.Contains(err.Error(), "uid 501") {
			t.Errorf("err = %v", err)
		}

		if opened {
			t.Error("the ledger was opened by an account that is not its owner")
		}

		if len(*calls) != 0 {
			t.Errorf("re-executed: %v", *calls)
		}
	})

	t.Run("another account, owner unreadable, refuses", func(t *testing.T) {
		calls := seams(t, 501, 0, 0, syscall.EACCES, 0, nil)

		err := cmdRolloutStatus(t.Context(), []string{"--json", "--config", cfgPath})
		if err == nil || !strings.Contains(err.Error(), "read who owns") || !errors.Is(err, syscall.EACCES) {
			t.Errorf("err = %v", err)
		}

		if len(*calls) != 0 {
			t.Errorf("re-executed: %v", *calls)
		}
	})

	t.Run("root, owner unreadable, refuses", func(t *testing.T) {
		calls := seams(t, 0, 0, 0, syscall.EACCES, 0, nil)

		err := cmdRolloutStatus(t.Context(), []string{"--json", "--config", cfgPath})
		if err == nil || !strings.Contains(err.Error(), "read who owns") || !errors.Is(err, syscall.EACCES) {
			t.Errorf("err = %v", err)
		}

		if len(*calls) != 0 {
			t.Errorf("re-executed: %v", *calls)
		}
	})
}

// A JSON REPORT THAT COULD NOT BE WRITTEN IS THE COMMAND'S FAILURE: a machine
// reading a redirected file must not find a cut report behind a zero exit.
func TestRolloutStatusJSONReportsAFailedWrite(t *testing.T) {
	stateDir := t.TempDir()
	cfgPath := writeCAConfig(t, stateDir)

	statusPlane(t, stateDir, func(*state.DB) {})

	savedOut := statusOut
	statusOut = failingWriter{}

	t.Cleanup(func() { statusOut = savedOut })

	err := cmdRolloutStatus(t.Context(), []string{"--json", "--config", cfgPath})
	if err == nil || !strings.Contains(err.Error(), "write the report") || !errors.Is(err, errDiskFull) {
		t.Errorf("err = %v, want the write's failure", err)
	}
}

var errDiskFull = errors.New("no space left on device")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errDiskFull }

// THE TEXT REPORT ESCAPES A REFUSAL'S CONTROL CHARACTERS: a multi-line dispatch
// error stays one row, a tab does not move a column, and a terminal escape is
// shown rather than obeyed. The stored value is untouched.
func TestRolloutStatusTextEscapesTheRefusal(t *testing.T) {
	nodes := []rollout.Node{
		{Node: "refused", Phase: rollout.PhasePending, LastRefusal: "line one\nline two\ttabbed\x1b[31mred\r\u2028\u202eb\u0085"},
		{Node: "quiet", Phase: rollout.PhasePending},
	}

	out := capture(t, func() { printRolloutNodes(nodes) })

	line := lineFor(t, out, "refused")
	if want := `last dispatch refused: line one\nline two\ttabbed\x1b[31mred\r\u2028\u202eb\u0085`; !strings.Contains(line, want) {
		t.Errorf("the refused row reads %q, want it to carry %q", line, want)
	}

	for _, raw := range []string{"\x1b", "line two\t", "\r", "\u2028", "\u202e", "\u0085"} {
		if strings.Contains(out, raw) {
			t.Errorf("the report carries the raw control sequence %q", raw)
		}
	}

	// ONE ROW PER HOST: a raw newline would start a line with the refusal's
	// second half.
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "line two") {
			t.Errorf("the refusal broke the row structure:\n%s", out)
		}
	}

	if nodes[0].LastRefusal != "line one\nline two\ttabbed\x1b[31mred\r\u2028\u202eb\u0085" {
		t.Error("rendering changed the stored refusal")
	}
}

// THE TEXT REPORT'S DETAIL HAS ONE PRECEDENCE: a blocker, then an exemption,
// then a rollback result, and the last refusal only when none of them says
// where the host is.
func TestRolloutStatusTextShowsTheRefusalLast(t *testing.T) {
	nodes := []rollout.Node{
		{Node: "blocked", Phase: rollout.PhaseBlocked, Blocker: "an older wire", LastRefusal: "refused"},
		{Node: "exempt", Phase: rollout.PhaseExempt, ExemptReason: "retired", LastRefusal: "refused"},
		{Node: "rolled", Phase: rollout.PhaseRolledBack, RollbackResult: "restored v0.9.3", LastRefusal: "refused"},
		{Node: "refused", Phase: rollout.PhasePending, LastRefusal: "the host holds a converge guard"},
		{Node: "quiet", Phase: rollout.PhasePending},
	}

	out := capture(t, func() { printRolloutNodes(nodes) })

	for _, want := range []struct{ node, detail string }{
		{"blocked", "an older wire"},
		{"exempt", "retired"},
		{"rolled", "restored v0.9.3"},
		{"refused", "last dispatch refused: the host holds a converge guard"},
	} {
		line := lineFor(t, out, want.node)
		if !strings.Contains(line, want.detail) {
			t.Errorf("%s's line does not carry %q: %s", want.node, want.detail, line)
		}

		if want.node != "refused" && strings.Contains(line, "refused") {
			t.Errorf("%s's line shows the refusal over its own detail: %s", want.node, line)
		}
	}

	if line := lineFor(t, out, "quiet"); strings.Contains(line, "refused") {
		t.Errorf("a host with no refusal shows one: %s", line)
	}
}

func lineFor(t *testing.T, out, node string) string {
	t.Helper()

	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, node+" ") {
			return line
		}
	}

	t.Fatalf("no line for %s in:\n%s", node, out)

	return ""
}
