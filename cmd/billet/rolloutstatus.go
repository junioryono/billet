package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"syscall"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/rollout"
	"github.com/junioryono/billet/internal/state"
)

// `billet rollout status --json` is the ledger's own account of the fleet's
// decision, for a converge that must decide whether the fleet is one
// deployment on one release and quiet before it selects that release's
// collection. It is read through state.OpenInspect, a construction path that
// validates everything an open validates and mutates nothing: no directory
// created or tightened, no lock, no claim, no migration, no watermark write.
//
// THE REPORT RUNS AS THE LEDGER'S OWNER. Measured with the bundled driver
// (2026-09-09): a read-only open of a cleanly stopped WAL ledger CREATES the
// -shm and -wal sidecars beside it, owned by the process that opened it, and
// leaves them there. Run as root against a stopped controller that would leave
// root-owned sidecars the service account cannot open, and the control plane's
// next start would fail on its own ledger. So when this command runs as root
// and the identity directory belongs to another account, it re-executes itself
// under that account's uid and gid: whatever the driver creates is then owned
// exactly as the control plane's own open would have owned it, and the report
// can write nothing root-owned at all.

// rolloutStatusSchema is the report's schema version; a field renamed or
// removed under the same number is a broken consumer, so the tests pin the
// field set.
const rolloutStatusSchema = 1

type rolloutStatusReport struct {
	Schema        int                         `json:"schema"`
	Deployment    rolloutStatusDeployment     `json:"deployment"`
	Rollout       *rolloutStatusRollout       `json:"rollout"`
	Nodes         []rolloutStatusNode         `json:"nodes"`
	Registrations []rolloutStatusRegistration `json:"registrations"`
}

// rolloutStatusDeployment is the ledger's own binding, positively: Bound is
// false for a ledger that carries no binding row, and ID is then empty. It is
// never the host's identity file, which the command peeks only to refuse a
// foreign ledger and never mints.
type rolloutStatusDeployment struct {
	Bound bool   `json:"bound"`
	ID    string `json:"id"`
}

type rolloutStatusRollout struct {
	ID              string              `json:"id"`
	Generation      int64               `json:"generation"`
	State           string              `json:"state"`
	Channel         string              `json:"channel"`
	TargetVersion   string              `json:"target_version"`
	TargetDigest    string              `json:"target_digest"`
	PriorVersion    string              `json:"prior_version"`
	ControllerPhase string              `json:"controller_phase"`
	Policy          rolloutStatusPolicy `json:"policy"`
	CreatedBy       string              `json:"created_by"`
	CreatedAt       string              `json:"created_at"`
	FinishedAt      string              `json:"finished_at"`
	TerminalReason  string              `json:"terminal_reason"`
}

type rolloutStatusPolicy struct {
	Cohort         int  `json:"cohort"`
	FailureBudget  int  `json:"failure_budget"`
	AllowDowngrade bool `json:"allow_downgrade"`
}

type rolloutStatusNode struct {
	Node            string `json:"node"`
	Phase           string `json:"phase"`
	Attempts        int    `json:"attempts"`
	NextAttemptAt   string `json:"next_attempt_at"`
	Blocker         string `json:"blocker"`
	PriorRelease    string `json:"prior_release"`
	RollbackResult  string `json:"rollback_result"`
	ExemptReason    string `json:"exempt_reason"`
	UpdatedAt       string `json:"updated_at"`
	DispatchEpoch   int64  `json:"dispatch_epoch"`
	ConvergedDigest string `json:"converged_digest"`
	LastRefusal     string `json:"last_refusal"`
}

// rolloutStatusRegistration is one host's CURRENT registration, from the
// ledger's registrations and never from a rollout's rows: the incarnation the
// host presents now and the epoch it holds, which a reader compares with a
// receipt that names them.
type rolloutStatusRegistration struct {
	Name           string `json:"name"`
	Live           bool   `json:"live"`
	Epoch          int64  `json:"epoch"`
	Incarnation    string `json:"incarnation"`
	Release        string `json:"release"`
	Digest         string `json:"digest"`
	HighestRelease string `json:"highest_release"`
}

// buildRolloutStatusReport assembles the report from one inspect handle. Every
// read goes through the store's and the handle's View methods; a read that
// fails is the command's error, never an empty fleet, no rollout or an unbound
// deployment.
func buildRolloutStatusReport(ctx context.Context, db *state.DB, store *rollout.Store) (*rolloutStatusReport, error) {
	report := &rolloutStatusReport{Schema: rolloutStatusSchema, Nodes: []rolloutStatusNode{},
		Registrations: []rolloutStatusRegistration{}}

	binding, err := db.DeploymentBinding(ctx)
	if err != nil {
		return nil, err
	}

	report.Deployment = rolloutStatusDeployment{Bound: binding != "", ID: binding}

	current, err := store.Open(ctx)

	switch {
	case err == nil:
	case errors.Is(err, rollout.ErrNoRollout):
		history, err := store.History(ctx, 1)
		if err != nil {
			return nil, err
		}

		if len(history) > 0 {
			current = &history[0]
		}
	default:
		return nil, err
	}

	if current != nil {
		report.Rollout = &rolloutStatusRollout{
			ID: current.ID, Generation: current.Generation, State: current.State,
			Channel: current.Channel, TargetVersion: current.TargetVersion,
			TargetDigest: current.TargetDigest, PriorVersion: current.PriorVersion,
			ControllerPhase: string(current.ControllerPhase),
			Policy: rolloutStatusPolicy{
				Cohort: current.Policy.Cohort, FailureBudget: current.Policy.FailureBudget,
				AllowDowngrade: current.Policy.AllowDowngrade,
			},
			CreatedBy: current.CreatedBy, CreatedAt: current.CreatedAt,
			FinishedAt: current.FinishedAt, TerminalReason: current.TerminalReason,
		}

		nodes, err := store.Nodes(ctx, current.ID)
		if err != nil {
			return nil, err
		}

		for i := range nodes {
			n := &nodes[i]
			report.Nodes = append(report.Nodes, rolloutStatusNode{
				Node: n.Node, Phase: string(n.Phase), Attempts: n.Attempts,
				NextAttemptAt: n.NextAttemptAt, Blocker: n.Blocker, PriorRelease: n.PriorRelease,
				RollbackResult: n.RollbackResult, ExemptReason: n.ExemptReason,
				UpdatedAt: n.UpdatedAt, DispatchEpoch: n.DispatchEpoch,
				ConvergedDigest: n.ConvergedDigest, LastRefusal: n.LastRefusal,
			})
		}
	}

	registrations, err := store.Registrations(ctx)
	if err != nil {
		return nil, err
	}

	for i := range registrations {
		r := &registrations[i]
		report.Registrations = append(report.Registrations, rolloutStatusRegistration{
			Name: r.Name, Live: r.Live, Epoch: r.Epoch, Incarnation: r.Incarnation,
			Release: r.Release, Digest: r.Digest, HighestRelease: r.HighestRelease,
		})
	}

	return report, nil
}

func printRolloutStatusJSON(report *rolloutStatusReport) error {
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("render the report: %w", err)
	}

	fmt.Println(string(body))

	return nil
}

// ledgerDSNFrom is the connection string for a PostgreSQL ledger, from an
// environment file when one is named and from the process environment
// otherwise, and empty for a SQLite one.
//
// THE FILE IS READ WHENEVER IT IS NAMED, under the restricted grammar the
// release inspector reads systemd environment files with, so a file outside
// that grammar refuses on every backend rather than being read one way here
// and another by systemd. The value is used only on PostgreSQL and reaches no
// argument, no report and no error: pgx's own parse error redacts the
// password (measured 2026-09-09), and nothing here prints the string.
func ledgerDSNFrom(cfg *config.Config, environmentFile string) (string, error) {
	if environmentFile == "" {
		return ledgerDSN(cfg)
	}

	postgres := cfg.Server != nil && cfg.Server.LedgerBackend() == config.StatePostgres
	name := ""

	if postgres {
		name = cfg.Server.LedgerDSNEnv()
	}

	value, found, err := environmentFileValue(environmentFile, name)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", environmentFile, err)
	}

	if !postgres {
		return "", nil
	}

	if !found || value == "" {
		return "", fmt.Errorf("server.state.postgres.dsn_env names %s and %s does not set it, so "+
			"billet has no connection string", name, environmentFile)
	}

	return value, nil
}

// The seams the report's re-execution goes through: who this process is, who
// owns a path, and how the child is run.
var (
	statusEUID    = os.Geteuid
	statusOwnerOf = pathOwner
	statusReexec  = reexecAs
)

// pathOwner reads the owner of a path without following a symlink, or reports
// that the path carries no Unix owner.
func pathOwner(path string) (uint32, uint32, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, 0, err
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("%s carries no owner this platform reports", path)
	}

	return st.Uid, st.Gid, nil
}

// runAsLedgerOwner re-executes this command under the identity directory's
// owner when it runs as root and that owner is another account, and reports
// whether it did so (the caller then returns the child's outcome and does
// nothing itself). A directory owned by root, or a process that is not root,
// runs the report in place; a directory that does not exist is left for the
// open to refuse with its own diagnostic.
func runAsLedgerOwner(ctx context.Context, cfg *config.Config, args []string) (bool, error) {
	if cfg.Server == nil || statusEUID() != 0 {
		return false, nil
	}

	uid, gid, err := statusOwnerOf(cfg.Server.IdentityDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read who owns %s: %w", cfg.Server.IdentityDir, err)
	case uid == 0:
		return false, nil
	}

	code, err := statusReexec(ctx, uid, gid, args)
	if err != nil {
		return true, err
	}

	if code != 0 {
		return true, fmt.Errorf("%w: exit status %d", errStatusChildFailed, code)
	}

	return true, nil
}

// errStatusChildFailed is the report's own failure, run as the ledger's owner;
// the child has already printed why.
var errStatusChildFailed = errors.New("billet rollout status failed as the ledger's owner")

// reexecAs runs this executable again with the same arguments as uid:gid, its
// output and errors passed through, and returns the child's exit status. The
// child is not root, so it runs the report in place rather than here again.
func reexecAs(ctx context.Context, uid, gid uint32, args []string) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("find this billet to run the report as the ledger's owner: %w", err)
	}

	//nolint:gosec // this executable, with this command's own arguments, under the ledger owner's identity
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: []uint32{gid}},
	}

	err = cmd.Run()

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}

	if err != nil {
		return 0, fmt.Errorf("run the report as the ledger's owner: %w", err)
	}

	return 0, nil
}
