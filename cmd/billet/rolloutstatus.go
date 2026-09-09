package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unicode"

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

// buildRolloutStatusReport assembles the report from ONE SNAPSHOT of the
// ledger (rollout.Store.StatusSnapshot, one read transaction), so the binding,
// the rollout, its hosts and the registrations describe the same instant. A
// read that fails is the command's error, never an empty fleet, no rollout or
// an unbound deployment.
//
// THE BINDING IS COMPARED AGAIN INSIDE THE SNAPSHOT. The open refused a foreign
// ledger already, but a ledger unbound at the open can be bound to another
// deployment by the time the report is read, and a report that then printed
// that binding under this host's identity would be the foreign-ledger case the
// open exists to refuse. identity is the host's own deployment id when the
// identity file holds one, and empty when it does not.
func buildRolloutStatusReport(ctx context.Context, store *rollout.Store, identity string,
) (*rolloutStatusReport, error) {
	report := &rolloutStatusReport{Schema: rolloutStatusSchema, Nodes: []rolloutStatusNode{},
		Registrations: []rolloutStatusRegistration{}}

	snapshot, err := store.StatusSnapshot(ctx)
	if err != nil {
		return nil, err
	}

	if identity != "" && snapshot.Binding != "" && snapshot.Binding != identity {
		return nil, fmt.Errorf("%w: this ledger is bound to deployment %s and this host's identity "+
			"directory says %s", state.ErrForeignLedger, snapshot.Binding, identity)
	}

	report.Deployment = rolloutStatusDeployment{Bound: snapshot.Binding != "", ID: snapshot.Binding}

	current := snapshot.Rollout

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

		nodes := snapshot.Nodes

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

	registrations := snapshot.Registrations

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

	// A REPORT THAT DID NOT ARRIVE IS A FAILURE: a machine reading a redirected
	// file must not find a cut JSON behind a zero exit.
	var out io.Writer = os.Stdout
	if statusOut != nil {
		out = statusOut
	}

	if _, err := fmt.Fprintln(out, string(body)); err != nil {
		return fmt.Errorf("write the report: %w", err)
	}

	return nil
}

// escapeControl renders a string a foreign process produced for one line of a
// text report: every control character becomes its escape, and so does every
// format character and line or paragraph separator (a bidi override or a
// U+2028 would reorder or break the row as a raw newline would), so a
// multi-line dispatch error cannot break the row structure and a terminal
// escape cannot alter the report. The stored value and the JSON are untouched.
func escapeControl(s string) string {
	var b strings.Builder

	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}

	return b.String()
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
// owns a path, and how the child is run; and statusAfterOpen, which a test uses
// to change the ledger between the open and the report's one read.
var (
	statusEUID      = os.Geteuid
	statusOwnerOf   = pathOwner
	statusReexec    = reexecAs
	statusAfterOpen func(*state.DB)
	// statusOut replaces standard output for the JSON report; a test stands a
	// failing writer in. Resolved at the write, so a captured stdout is seen.
	statusOut io.Writer
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

// runAsLedgerOwner runs the report as the identity directory's owner and as
// nobody else: root over another account's directory re-executes this command
// under that account and reports that it did (the caller then returns the
// child's outcome and does nothing itself); the owner runs the report in
// place; and on a SQLite ledger ANY OTHER ACCOUNT IS REFUSED before the ledger
// is opened, because a read-only open of a stopped SQLite ledger creates
// sidecars owned by whoever opened it, and an operator with group access to
// the directory would leave files the service account cannot write exactly as
// root would. A PostgreSQL ledger has no sidecar, so there the rule is root's
// alone. A directory that does not exist is left for the open to refuse with
// its own diagnostic.
func runAsLedgerOwner(ctx context.Context, cfg *config.Config, args []string) (bool, error) {
	if cfg.Server == nil {
		return false, nil
	}

	euid := statusEUID()
	sqlite := cfg.Server.LedgerBackend() != config.StatePostgres

	if euid != 0 && !sqlite {
		return false, nil
	}

	uid, gid, err := statusOwnerOf(cfg.Server.IdentityDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read who owns %s: %w", cfg.Server.IdentityDir, err)
	case int(uid) == euid:
		return false, nil
	case euid != 0:
		return false, fmt.Errorf("%s is owned by uid %d and this process runs as uid %d; a status read of a "+
			"SQLite ledger leaves files owned by whoever read it, so run this as the ledger's owner, or as root, "+
			"which runs it as the owner", cfg.Server.IdentityDir, uid, euid)
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
