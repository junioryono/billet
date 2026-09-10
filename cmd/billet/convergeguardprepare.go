package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/version"
)

// `billet converge-guard prepare` is the host role's preparation, as ONE
// COMMAND UNDER ONE LOCK: the claim classified, the pointer validated, the
// recorded executable verified, the guard acquired or validated, a staged
// candidate judged and bound, every answer one JSON object the role parses
// once. Before it, the role classified through `status`, held through `hold`
// and decided between the two, and every gap between two separately locked
// commands was a window an updater or another driver could move through.
//
// TWO CALLS PER CONVERGE. The FIRST, `--validate`, acquires a guard when
// there is none (recording the managed binary, or a staged candidate on the
// candidate-first paths: a bootstrap, a pre-R managed binary) or validates
// this holder's existing one, and never judges intent. The SECOND declares
// the intent, `--candidate C`, `--no-change` or `--recovery`, and is judged:
// the candidate's capability and the downgrade before anything is rewritten,
// then the record re-bound to the candidate when it still names the managed
// binary. `settle` closes the acquirer's preparation window. `release
// --cleanup --token` releases a guard only inside that window, by the
// invocation that published it. `--dry-run` reports every shape, takes no
// lock and writes nothing.
//
// THE RECORD carries a public `id` (continuity: a run that held this host
// passes `--expect-id` on every later call, and the command refuses under the
// lock to acquire or to admit another guard for it), a `token` the acquiring
// invocation alone receives (the cleanup release's one authorisation), and
// `preparing`, true from acquisition to settlement. A record from before this
// change, five members and no id, is ADOPTED by the first `--validate` that
// finds it: an id minted, `preparing` false, no token, no acquisition.
//
// ONE DRIVER PER HOLDER IS THE HOLDER'S CONTRACT, not this command's
// enforcement: the guard tells drivers under different holders apart; a
// driver's own reruns and inclusions under its holder are what the protocol
// serves.

const (
	// The answers' outcomes.
	prepareAcquired  = "acquired"
	prepareValidated = "validated"
	prepareRebound   = "rebound"
	prepareReported  = "reported"
	prepareRefused   = "refused"
	prepareUnknown   = "unknown"

	// The refusals' reasons, machine-readable beside the prose.
	reasonCombination   = "combination"
	reasonShape         = "shape"
	reasonHeld          = "held"
	reasonRecord        = "record"
	reasonVerification  = "verification"
	reasonPointer       = "pointer"
	reasonGone          = "gone"
	reasonReplaced      = "replaced"
	reasonManaged       = "managed"
	reasonBootstrap     = "bootstrap"
	reasonCandidate     = "candidate"
	reasonFloor         = "floor"
	reasonDowngrade     = "downgrade"
	reasonIntent        = "intent"
	reasonToken         = "token"
	reasonWindowClosed  = "window-closed"
	reasonSettled       = "already-settled"
	reasonTrust         = "trust"
	reasonStray         = "stray"
	reasonUnexaminable  = "unexaminable"
	reasonRandomness    = "randomness"
	reasonRecoveryOwing = "recovery"

	// exitRefused and exitUnknown are the command's two non-zero statuses:
	// a refusal it can name, and something it could not examine.
	exitRefused = 2
	exitUnknown = 3

	// guardIDBytes is the size of an id and of a token.
	guardIDBytes = 16
	// maxCommandOutput bounds what a candidate's answer may say.
	maxCommandOutput = 1 << 20
)

// guardRandom is the entropy the id and the token come from: crypto/rand in
// production; a fixture's sequence in the corpus writer, so a committed
// answer is reproducible.
var guardRandom = func(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return nil, err
	}

	return buf, nil
}

// guardCommandTimeout bounds every execution the command makes of a binary
// (`version`, `converge-guard status`): a candidate that hangs is killed at
// the bound and refused.
var guardCommandTimeout = 60 * time.Second

// hex32 is the shape of an id and of a token.
var guardHex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// The two grammars of a recovery directory's name: the role's exclusive
// allocation and the legacy timestamp, exactly as upgrade-inspect.yml admits
// them.
var recoveryNameGrammar = regexp.MustCompile(`^(recovery-[0-9]{8}T[0-9]{6}-[0-9a-f]{8}|[0-9]{8}T[0-9]{15})$`)

// unknownCommandPattern is what a release before the guard answers.
var unknownCommandPattern = regexp.MustCompile(`unknown (converge-guard )?command`)

// prepareExecutable is one executable as the answer describes it.
type prepareExecutable struct {
	Path string `json:"path"`
	// Present is true, false, or "unknown" with Why.
	Present any    `json:"present"`
	Why     string `json:"why,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Version string `json:"version,omitempty"`
	// VersionProblem is why `version` could not be read; a development build
	// that reports no release has an empty Version and no problem.
	VersionProblem string `json:"version_problem,omitempty"`
	// Capable is set on a candidate: whether it answers the guard's commands.
	Capable *bool `json:"capable,omitempty"`
}

// prepareRecord is the record as the answer carries it.
type prepareRecord struct {
	ReleaseExecutable       string `json:"release_executable"`
	ReleaseExecutableSHA256 string `json:"release_executable_sha256"`
	Verified                any    `json:"verified"`
}

// prepareAnswer is the successful answer of every mutating mode.
type prepareAnswer struct {
	Outcome       string             `json:"outcome"`
	ID            string             `json:"id"`
	Adopted       bool               `json:"adopted"`
	Holder        string             `json:"holder"`
	ClaimedAt     string             `json:"claimed_at"`
	Hostname      string             `json:"hostname"`
	Preparing     bool               `json:"preparing"`
	Token         string             `json:"token,omitempty"`
	Record        prepareRecord      `json:"record"`
	Pointer       bool               `json:"pointer"`
	PointerTarget string             `json:"pointer_target,omitempty"`
	RecoveryDir   string             `json:"recovery_dir,omitempty"`
	StrayRemoved  bool               `json:"stray_removed"`
	Managed       prepareExecutable  `json:"managed"`
	Candidate     *prepareExecutable `json:"candidate,omitempty"`
	Downgrade     bool               `json:"downgrade"`
}

// prepareRefusal is the answer of a refusal (exit 2) or of something that
// could not be examined (exit 3).
type prepareRefusal struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	Shape   string `json:"shape,omitempty"`
	Why     string `json:"why"`
	Next    string `json:"next,omitempty"`
}

// prepareReport is the dry run's answer, one schema over every shape.
type prepareReport struct {
	Outcome string            `json:"outcome"`
	Shape   string            `json:"shape"`
	Why     string            `json:"why,omitempty"`
	Guard   *prepareDryGuard  `json:"guard"`
	Managed prepareExecutable `json:"managed"`
}

type prepareDryGuard struct {
	ID             *string       `json:"id"`
	Holder         string        `json:"holder"`
	ClaimedAt      string        `json:"claimed_at"`
	Hostname       string        `json:"hostname"`
	Preparing      bool          `json:"preparing"`
	Record         prepareRecord `json:"record"`
	RecordError    string        `json:"record_error,omitempty"`
	Pointer        bool          `json:"pointer"`
	PointerTarget  string        `json:"pointer_target,omitempty"`
	PointerProblem string        `json:"pointer_problem,omitempty"`
	StrayTemporary bool          `json:"stray_temporary"`
}

// prepareMode is what the flag table admitted.
type prepareMode struct {
	validate, noChange, recovery, dryRun bool
	candidate                            string
	expectBootstrap                      bool
	expectID                             string
	allowDowngrade                       bool
	token                                string
}

// refuse builds a refusal with its exit status; the JSON is printed by the
// command before it returns.
func refuse(reason, shape, why, next string) *prepareRefusal {
	return &prepareRefusal{Outcome: prepareRefused, Reason: reason, Shape: shape, Why: why, Next: next}
}

func couldNotTell(why string) *prepareRefusal {
	return &prepareRefusal{Outcome: prepareUnknown, Reason: reasonUnexaminable, Why: why}
}

// answer prints a JSON object and returns the exit the outcome carries.
func answerJSON(v any, code int, msg string) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}

	fmt.Println(string(body))

	if code == 0 {
		return nil
	}

	return &exitError{code: code, msg: msg}
}

func answerRefusal(r *prepareRefusal) error {
	code := exitRefused
	if r.Outcome == prepareUnknown {
		code = exitUnknown
	}

	return answerJSON(r, code, r.Why)
}

func cmdGuardPrepare(ctx context.Context, args []string) error {
	flags := newFlagSet("billet converge-guard prepare")
	holder := flags.String("holder", "", "who holds the guard: the converge's run id, or an operator's handle")
	validate := flags.Bool("validate", false, "the first call: acquire a guard when there is none, or validate this holder's")
	candidate := flags.String("candidate", "", "the staged candidate this converge intends to install; with --validate, "+
		"the executable a candidate-first acquisition records")
	noChange := flags.Bool("no-change", false, "the second call: this converge changes no binary")
	recovery := flags.Bool("recovery", false, "the second call: this converge recovers the transaction the pointer names")
	dryRun := flags.Bool("dry-run", false, "report every shape; take no lock, write nothing")
	expectBootstrap := flags.Bool("expect-bootstrap", false, "with --validate --candidate: require the managed path "+
		"absent and the root to hold only recovery directories")
	expectID := flags.String("expect-id", "", "the id of the guard this run held before; a guard that is gone or "+
		"replaced refuses, and nothing is acquired")
	allowDowngrade := flags.Bool("allow-downgrade", false, "with --candidate: admit a candidate older than the managed binary")
	token := flags.String("token", "", "the acquiring invocation's token; accepted by the second call and ignored")
	asJSON := flags.Bool("json", false, "print the answer as JSON (the only form)")

	if err := parse(flags, args); err != nil {
		return err
	}

	if !*asJSON {
		return errors.New("prepare answers as JSON; pass --json")
	}

	mode := prepareMode{validate: *validate, noChange: *noChange, recovery: *recovery, dryRun: *dryRun,
		candidate: *candidate, expectBootstrap: *expectBootstrap, expectID: *expectID,
		allowDowngrade: *allowDowngrade, token: *token}

	if r := checkPrepareCombination(mode); r != nil {
		return answerRefusal(r)
	}

	if !mode.dryRun {
		if err := checkHolder(*holder); err != nil {
			return answerRefusal(refuse(reasonCombination, "", err.Error(), ""))
		}
	} else if *holder != "" {
		if err := checkHolder(*holder); err != nil {
			return answerRefusal(refuse(reasonCombination, "", err.Error(), ""))
		}
	}

	if mode.expectID != "" && !guardHex32.MatchString(mode.expectID) {
		return answerRefusal(refuse(reasonCombination, "", "--expect-id is not a 32-hex id", ""))
	}

	if mode.token != "" && !guardHex32.MatchString(mode.token) {
		return answerRefusal(refuse(reasonCombination, "", "--token is not a 32-hex token", ""))
	}

	if mode.dryRun {
		return prepareDryRun(ctx)
	}

	root, err := prepareUpgradeRoot()
	if err != nil {
		if errors.Is(err, errTrustBoundary) {
			return answerRefusal(refuse(reasonTrust, "", err.Error(), ""))
		}

		return answerRefusal(couldNotTell(err.Error()))
	}

	defer root.close()

	answer, refusal := prepareUnderLock(ctx, root, *holder, mode)
	if refusal != nil {
		return answerRefusal(refusal)
	}

	return answerJSON(answer, 0, "")
}

// checkPrepareCombination is the flag table, judged before anything is read.
func checkPrepareCombination(m prepareMode) *prepareRefusal {
	modes := 0

	for _, on := range []bool{m.validate, m.noChange, m.recovery, m.dryRun} {
		if on {
			modes++
		}
	}

	bad := func(why string) *prepareRefusal {
		return refuse(reasonCombination, "", why, "one of --validate, --validate --candidate C, --candidate C, "+
			"--no-change, --recovery, --dry-run")
	}

	switch {
	case modes == 0 && m.candidate == "":
		return bad("prepare needs a mode")
	case modes > 1:
		return bad("prepare takes one mode")
	case m.noChange && m.candidate != "":
		return bad("--no-change and --candidate contradict each other")
	case m.recovery && m.candidate != "":
		return bad("--recovery and --candidate contradict each other")
	case m.expectBootstrap && (!m.validate || m.candidate == ""):
		return bad("--expect-bootstrap belongs to --validate --candidate C")
	case m.dryRun && (m.candidate != "" || m.expectBootstrap || m.expectID != "" || m.allowDowngrade || m.token != ""):
		return bad("--dry-run takes no other flag")
	case m.allowDowngrade && (m.validate || m.noChange || m.recovery):
		return bad("--allow-downgrade belongs to --candidate C")
	}

	return nil
}

// prepareUnderLock is the whole preparation under the transaction lock.
func prepareUnderLock(ctx context.Context, root *txLock, holder string, m prepareMode) (*prepareAnswer, *prepareRefusal) {
	dir, shape, err := openGuardForMutation(root)
	if err != nil {
		if errors.Is(err, errTrustBoundary) {
			return nil, refuse(reasonTrust, string(shape.Kind), err.Error(), "")
		}

		return nil, couldNotTell(err.Error())
	}

	if dir != nil {
		defer func() { _ = dir.Close() }()
	}

	// CONTINUITY FIRST: a run that held this host must find its guard.
	if m.expectID != "" {
		switch {
		case shape.Kind == claimNone:
			return nil, refuse(reasonGone, string(shape.Kind), "the guard this run held is gone; nothing was acquired",
				"inspect the host; `billet converge-guard status`")
		case shape.Kind != claimGuard:
			return nil, refuse(reasonReplaced, string(shape.Kind),
				fmt.Sprintf("the guard this run held was replaced by %s; nothing was acquired", shape), "")
		case shape.RecordErr != "":
			return nil, refuse(reasonReplaced, string(shape.Kind),
				"the guard this run held was replaced by one whose record cannot be read: "+shape.RecordErr, "")
		case shape.Guard.ID != m.expectID:
			return nil, refuse(reasonReplaced, string(shape.Kind),
				fmt.Sprintf("the guard this run held (%s) was replaced by another (%s); nothing was acquired",
					m.expectID, shape.Guard.ID), "")
		}
	}

	switch shape.Kind {
	case claimNone:
		if !m.validate {
			return nil, refuse(reasonShape, string(claimNone), "no guard is held on this host, and only "+
				"--validate acquires one", "run `prepare --validate` first")
		}

		return acquireGuard(ctx, root, holder, m)
	case claimGuard:
	default:
		return nil, refuseShapeJSON(shape)
	}

	// THIS HOLDER'S GUARD, OR ANOTHER'S.
	if shape.RecordErr != "" {
		return nil, refuse(reasonRecord, string(claimGuard), "a guard is held on this host and its record cannot be read: "+
			shape.RecordErr, "`billet converge-guard status`")
	}

	if shape.Guard.Holder != holder {
		return nil, refuse(reasonHeld, string(claimGuard), refuseHeld(shape).Error(), "")
	}

	// A STRAY TEMPORARY of an interrupted rewrite is removed, when it is the
	// regular file a rewrite leaves; any other shape refuses naming it.
	strayRemoved, r := removeStrayTemporary(dir)
	if r != nil {
		return nil, r
	}

	// THE POINTER, validated; THE RECORD, verified; THE FLUSHES an
	// interruption may owe, completed. Each on every branch that found a guard.
	pointer, target, r := validatePointer(root.dir, dir)
	if r != nil {
		return nil, r
	}

	if r := verifyRecorded(shape); r != nil {
		return nil, r
	}

	if err := syncDirFD(dir); err != nil {
		return nil, couldNotTell(err.Error())
	}

	if err := syncDirFD(root.dir); err != nil {
		return nil, couldNotTell(err.Error())
	}

	// ADOPTION of a record from before this change.
	adopted := false

	if shape.Guard.ID == "" {
		id, err := newGuardID()
		if err != nil {
			return nil, refuse(reasonRandomness, string(claimGuard), err.Error(), "")
		}

		record := shape.Guard
		record.ID, record.Token, record.Preparing = id, "", false

		if err := rewriteGuardRecord(root, dir, record); err != nil {
			return nil, couldNotTell(err.Error())
		}

		shape.Guard = record
		adopted = true
	}

	managed := describeManaged(ctx)

	base := &prepareAnswer{
		ID: shape.Guard.ID, Adopted: adopted, Holder: shape.Guard.Holder, ClaimedAt: shape.Guard.ClaimedAt,
		Hostname: shape.Guard.Hostname, Preparing: shape.Guard.Preparing,
		Record: prepareRecord{ReleaseExecutable: shape.Guard.ReleaseExecutable,
			ReleaseExecutableSHA256: shape.Guard.ReleaseExecutableSHA256, Verified: true},
		Pointer: pointer, PointerTarget: target, StrayRemoved: strayRemoved, Managed: managed,
	}

	switch {
	case m.validate:
		base.Outcome = prepareValidated

		return base, nil
	case m.recovery:
		if !pointer {
			return nil, refuse(reasonPointer, string(claimGuard), "the guard carries no transaction pointer, so "+
				"there is no transaction to recover", "`prepare --candidate C` or `prepare --no-change`")
		}

		base.Outcome, base.RecoveryDir = prepareValidated, target

		return base, nil
	case pointer:
		return nil, refuse(reasonPointer, string(claimGuard), fmt.Sprintf("the guard carries the transaction "+
			"pointer %s, which only `--recovery` admits", target), "`prepare --recovery`")
	case m.noChange:
		return judgeNoChange(base, shape, managed)
	}

	return judgeCandidate(ctx, root, dir, base, shape, managed, m)
}

// acquireGuard publishes a guard for a claim of none: the managed binary
// recorded, or the candidate on a candidate-first path.
func acquireGuard(ctx context.Context, root *txLock, holder string, m prepareMode) (*prepareAnswer, *prepareRefusal) {
	if m.expectBootstrap {
		if r := requireBootstrapRoot(root); r != nil {
			return nil, r
		}
	}

	if m.candidate == "" {
		// THE MANAGED BINARY BY LSTAT AND IDENTITY, never by an exec: absent,
		// dangling, non-regular or unexaminable each refuse naming what was found.
		if r := requireManagedPresent(); r != nil {
			return nil, r
		}
	}

	id, err := newGuardID()
	if err != nil {
		return nil, refuse(reasonRandomness, string(claimNone), err.Error(), "")
	}

	token, err := newGuardID()
	if err != nil {
		return nil, refuse(reasonRandomness, string(claimNone), err.Error(), "")
	}

	record := guardRecord{Holder: holder, ID: id, Token: token, Preparing: true}
	record.ClaimedAt = guardNow().UTC().Format(time.RFC3339)

	hostname, err := guardHostname()
	if err != nil || hostname == "" {
		return nil, couldNotTell(fmt.Sprintf("read this host's name for the guard: %v", err))
	}

	record.Hostname = hostname

	path, sum, err := recordExecutable(root, m.candidate, true)
	if err != nil {
		if errors.Is(err, errTrustBoundary) {
			return nil, refuse(reasonTrust, string(claimNone), err.Error(), "")
		}

		return nil, refuse(reasonCandidate, string(claimNone), err.Error(), "")
	}

	record.ReleaseExecutable, record.ReleaseExecutableSHA256 = path, sum

	if err := publishGuard(root, record); err != nil {
		return nil, couldNotTell(err.Error())
	}

	answer := &prepareAnswer{
		Outcome: prepareAcquired, ID: id, Holder: holder, ClaimedAt: record.ClaimedAt, Hostname: hostname,
		Preparing: true, Token: token,
		Record:  prepareRecord{ReleaseExecutable: path, ReleaseExecutableSHA256: sum, Verified: true},
		Managed: describeManaged(ctx),
	}

	if m.candidate != "" {
		answer.Candidate = &prepareExecutable{Path: path, Present: true, SHA256: sum}
	}

	return answer, nil
}

// judgeNoChange is the second call declaring no binary change.
func judgeNoChange(base *prepareAnswer, shape claimShape, managed prepareExecutable) (*prepareAnswer, *prepareRefusal) {
	base.Outcome = prepareValidated

	if shape.Guard.ReleaseExecutable == installedBinary {
		return base, nil
	}

	// A CANDIDATE RECORDED: no change is admitted only once the managed
	// binary IS that candidate (a committed upgrade).
	if managed.SHA256 != "" && managed.SHA256 == shape.Guard.ReleaseExecutableSHA256 {
		return base, nil
	}

	return nil, refuse(reasonIntent, string(claimGuard), fmt.Sprintf("this converge intends no binary change over "+
		"a candidate the guard records (%s, %s) that is not the managed binary (%s)", shape.Guard.ReleaseExecutable,
		shape.Guard.ReleaseExecutableSHA256, managed.SHA256), "stage that candidate, or release the guard")
}

// judgeCandidate is the second call declaring a candidate: the judgement
// (place, capability, downgrade, intent) before any rewrite, then the
// re-binding of a record that still names the managed binary.
func judgeCandidate(ctx context.Context, root *txLock, dir *os.File, base *prepareAnswer, shape claimShape,
	managed prepareExecutable, m prepareMode) (*prepareAnswer, *prepareRefusal) {
	cleaned, sum, err := hashCandidate(root, m.candidate, false)
	if err != nil {
		if errors.Is(err, errTrustBoundary) {
			return nil, refuse(reasonTrust, string(claimGuard), err.Error(), "")
		}

		return nil, refuse(reasonCandidate, string(claimGuard), err.Error(), "")
	}

	cand := &prepareExecutable{Path: cleaned, Present: true, SHA256: sum}
	base.Candidate = cand

	// CAPABILITY: the candidate executed under a bound and its answer judged
	// as the status protocol's schema.
	capable, problem := candidateCapable(ctx, cleaned)
	cand.Capable = &capable

	if !capable {
		return nil, refuse(reasonFloor, string(claimGuard), "a release before the converge guard cannot take part in "+
			"the exclusion: "+problem, "pin a release that carries the guard")
	}

	candVersion, err := executableVersion(ctx, cleaned)
	if err != nil {
		return nil, couldNotTell("the candidate's release could not be read, so no downgrade can be judged: " + err.Error())
	}

	cand.Version = candVersion

	if managed.Present == true && managed.VersionProblem != "" {
		return nil, couldNotTell("the managed binary's release could not be read, so no downgrade can be judged: " +
			managed.VersionProblem)
	}

	// THE DOWNGRADE, proved through version.Compare and nothing weaker.
	if cand.Version != "" && managed.Version != "" {
		if order, ok := version.Compare(cand.Version, managed.Version); ok && order < 0 {
			base.Downgrade = true

			if !m.allowDowngrade {
				return nil, refuse(reasonDowngrade, string(claimGuard), fmt.Sprintf("the candidate %s is older than "+
					"the managed binary's %s", cand.Version, managed.Version), "--allow-downgrade, deliberately")
			}
		}
	}

	// THE CANDIDATE JUDGED IS THE CANDIDATE RECORDED: hashed and flushed again
	// after the probes executed it, so a file replaced under the judgement is
	// refused rather than recorded under the judged digest.
	_, again, err := hashCandidate(root, m.candidate, true)
	if err != nil {
		return nil, couldNotTell(err.Error())
	}

	if again != sum {
		return nil, refuse(reasonCandidate, string(claimGuard), fmt.Sprintf("%s changed while it was being judged "+
			"(digest %s, then %s)", cleaned, sum, again), "stage it again")
	}

	// THE INTENT against the record.
	switch {
	case shape.Guard.ReleaseExecutable == installedBinary:
		record := shape.Guard
		record.ReleaseExecutable, record.ReleaseExecutableSHA256 = cleaned, sum

		if err := rewriteGuardRecord(root, dir, record); err != nil {
			return nil, couldNotTell(err.Error())
		}

		base.Outcome = prepareRebound
		base.Record = prepareRecord{ReleaseExecutable: cleaned, ReleaseExecutableSHA256: sum, Verified: true}

		return base, nil
	case shape.Guard.ReleaseExecutableSHA256 == sum:
		base.Outcome = prepareValidated

		return base, nil
	}

	return nil, refuse(reasonIntent, string(claimGuard), fmt.Sprintf("this guard records %s (%s) and this converge "+
		"stages %s (%s); a converge under one holder keeps one intent", shape.Guard.ReleaseExecutable,
		shape.Guard.ReleaseExecutableSHA256, cleaned, sum), "release the guard and converge afresh")
}

// rewriteGuardRecord rewrites the record by temporary, fsync and rename, then
// flushes the directory and the root.
func rewriteGuardRecord(root *txLock, dir *os.File, record guardRecord) error {
	if err := writeGuardRecordAt(dir, record, true); err != nil {
		return err
	}

	if err := syncDirFD(dir); err != nil {
		return err
	}

	return syncDirFD(root.dir)
}

// removeStrayTemporary removes `guard.json.tmp` when it is the regular file an
// interrupted rewrite leaves; anything else at that name refuses.
func removeStrayTemporary(dir *os.File) (bool, *prepareRefusal) {
	st, err := statAt(dir, guardTmpName)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, couldNotTell(fmt.Sprintf("examine %s: %v", guardTmpName, err))
	case modeOf(st)&unix.S_IFMT != unix.S_IFREG:
		return false, refuse(reasonStray, string(claimGuard), fmt.Sprintf("%s beside the record is %s, which no "+
			"rewrite leaves", guardTmpName, fileTypeOf(modeOf(st))), "inspect it by hand")
	}

	if err := guardObserve("unlink", filepath.Join(dir.Name(), guardTmpName), nil); err != nil {
		return false, couldNotTell(err.Error())
	}

	if err := unix.Unlinkat(int(dir.Fd()), guardTmpName, 0); err != nil {
		return false, couldNotTell(fmt.Sprintf("remove %s: %v", guardTmpName, err))
	}

	return true, nil
}

// validatePointer validates `active/recovery` as the role's transaction
// pointer: absent, or a symlink whose target is the canonical absolute name
// of a recovery directory directly under the root, opened by that name
// relative to the root's descriptor (following nothing, so a symlink at the
// name is not a directory) and judged owned by the root's owner and writable
// by nobody else. A target spelled through `..` or a symlink is refused
// rather than resolved, because the name the record answers must be the
// directory the transaction runs from.
func validatePointer(rootDir, dir *os.File) (bool, string, *prepareRefusal) {
	st, err := statAt(dir, guardPointerName)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, "", nil
	case err != nil:
		return false, "", couldNotTell(fmt.Sprintf("examine the pointer: %v", err))
	case modeOf(st)&unix.S_IFMT != unix.S_IFLNK:
		return false, "", refuse(reasonPointer, string(claimGuard), fmt.Sprintf("%s/%s is %s, and a transaction "+
			"pointer is a symlink", activePath(), guardPointerName, fileTypeOf(modeOf(st))), "inspect it by hand")
	}

	target, err := readlinkAt(dir, guardPointerName)
	if err != nil {
		return false, "", couldNotTell(fmt.Sprintf("read the pointer: %v", err))
	}

	if !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return false, "", refuse(reasonPointer, string(claimGuard), fmt.Sprintf("the transaction pointer names %q, "+
			"which is not a canonical absolute path", target), "inspect it by hand")
	}

	if filepath.Dir(target) != upgradeRoot || !recoveryNameGrammar.MatchString(filepath.Base(target)) {
		return false, "", refuse(reasonPointer, string(claimGuard), fmt.Sprintf("the transaction pointer names %s, "+
			"which is not a recovery directory directly under %s", target, upgradeRoot), "inspect it by hand")
	}

	fd, err := unix.Openat(int(rootDir.Fd()), filepath.Base(target),
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, "", refuse(reasonPointer, string(claimGuard), fmt.Sprintf("the transaction pointer names %s, "+
			"which does not exist", target), "inspect it by hand")
	case errors.Is(err, unix.ELOOP):
		return false, "", refuse(reasonPointer, string(claimGuard), fmt.Sprintf("the transaction pointer names %s, "+
			"which is a symlink", target), "inspect it by hand")
	case errors.Is(err, unix.ENOTDIR):
		what := "not a directory"
		if st, err := statAt(rootDir, filepath.Base(target)); err == nil {
			what = fileTypeOf(modeOf(st))
		}

		return false, "", refuse(reasonPointer, string(claimGuard), fmt.Sprintf("the transaction pointer names %s, "+
			"which is %s", target, what), "inspect it by hand")
	case err != nil:
		return false, "", couldNotTell(fmt.Sprintf("examine the pointer's target: %v", err))
	}

	recovery := os.NewFile(uintptr(fd), target)
	defer func() { _ = recovery.Close() }()

	info, err := recovery.Stat()
	if err != nil {
		return false, "", couldNotTell(fmt.Sprintf("examine the pointer's target: %v", err))
	}

	if err := requireTrustedDir(target, info, 0o700); err != nil {
		return false, "", refuse(reasonTrust, string(claimGuard), err.Error(), "inspect it by hand")
	}

	return true, target, nil
}

// verifyRecorded verifies the recorded executable's digest now.
func verifyRecorded(shape claimShape) *prepareRefusal {
	if shape.Guard.ReleaseExecutable == "" || shape.Guard.ReleaseExecutableSHA256 == "" {
		return refuse(reasonRecord, string(claimGuard), "the guard records no release executable",
			"`billet converge-guard status`")
	}

	sum, _, err := hashRegular(shape.Guard.ReleaseExecutable, maxExecutableBytes)
	if err != nil {
		return refuse(reasonVerification, string(claimGuard), fmt.Sprintf("the recorded executable %s cannot be "+
			"verified: %v", shape.Guard.ReleaseExecutable, err), "`billet converge-guard status`")
	}

	if sum != shape.Guard.ReleaseExecutableSHA256 {
		return refuse(reasonVerification, string(claimGuard), fmt.Sprintf("the recorded executable %s has digest %s "+
			"and the record names %s", shape.Guard.ReleaseExecutable, sum, shape.Guard.ReleaseExecutableSHA256),
			"`billet converge-guard status`")
	}

	return nil
}

// requireManagedPresent is the managed binary's presence by lstat and
// identity: a regular file, or a refusal naming what was found.
func requireManagedPresent() *prepareRefusal {
	info, err := os.Lstat(installedBinary)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return refuse(reasonManaged, string(claimNone), fmt.Sprintf("%s is absent: nothing to hold through; a "+
			"bootstrap holds through its staged candidate", installedBinary), "`prepare --validate --candidate C --expect-bootstrap`")
	case err != nil:
		return couldNotTell(fmt.Sprintf("examine %s: %v", installedBinary, err))
	case info.Mode()&fs.ModeSymlink != 0:
		return refuse(reasonManaged, string(claimNone), fmt.Sprintf("%s is a symlink, and the managed binary is a "+
			"regular file", installedBinary), "inspect it by hand")
	case !info.Mode().IsRegular():
		return refuse(reasonManaged, string(claimNone), fmt.Sprintf("%s is %s, and the managed binary is a regular "+
			"file", installedBinary, info.Mode().Type()), "inspect it by hand")
	}

	return nil
}

// requireBootstrapRoot is `--expect-bootstrap`'s premise: the managed path
// positively absent, and the root holding only recovery directories of either
// grammar and the lock.
func requireBootstrapRoot(root *txLock) *prepareRefusal {
	switch _, err := os.Lstat(installedBinary); {
	case err == nil:
		return refuse(reasonBootstrap, string(claimNone), fmt.Sprintf("%s exists, so this host is not being "+
			"bootstrapped; an updater may have installed it", installedBinary), "rerun the converge")
	case !errors.Is(err, fs.ErrNotExist):
		return couldNotTell(fmt.Sprintf("examine %s: %v", installedBinary, err))
	}

	entries, err := root.dir.ReadDir(-1)
	if err != nil {
		return couldNotTell(fmt.Sprintf("list %s: %v", upgradeRoot, err))
	}

	for _, e := range entries {
		st, err := statAt(root.dir, e.Name())
		if err != nil {
			return couldNotTell(fmt.Sprintf("examine %s: %v", filepath.Join(upgradeRoot, e.Name()), err))
		}

		kind := modeOf(st) & unix.S_IFMT

		switch {
		case e.Name() == txLockName && kind == unix.S_IFREG:
		case recoveryNameGrammar.MatchString(e.Name()) && kind == unix.S_IFDIR:
		default:
			return refuse(reasonBootstrap, string(claimNone), fmt.Sprintf("%s holds %s (%s), which a bootstrap does "+
				"not expect", upgradeRoot, e.Name(), fileTypeOf(modeOf(st))), "inspect it by hand")
		}
	}

	return nil
}

// describeManaged is the managed binary as the answer carries it: present by
// lstat, its digest and its version when it is a regular file.
func describeManaged(ctx context.Context) prepareExecutable {
	out := prepareExecutable{Path: installedBinary}

	info, err := os.Lstat(installedBinary)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		out.Present = false

		return out
	case err != nil:
		out.Present, out.Why = "unknown", err.Error()

		return out
	case !info.Mode().IsRegular():
		out.Present, out.Why = "unknown", installedBinary+" is "+info.Mode().Type().String()

		return out
	}

	out.Present = true

	if sum, _, err := hashRegular(installedBinary, maxExecutableBytes); err == nil {
		out.SHA256 = sum
	} else {
		out.Why = err.Error()
	}

	out.Version, err = executableVersion(ctx, installedBinary)
	if err != nil {
		out.VersionProblem = err.Error()
	}

	return out
}

// executableVersion runs `<binary> version` under the bound and answers the
// release it names, canonical; a development build that names no release
// answers nothing with no error, and an invocation that did not run, exited
// non-zero or answered another shape is an error, because a downgrade
// judgement over an unread version is no judgement.
func executableVersion(ctx context.Context, binary string) (string, error) {
	stdout, stderr, rc, err := runBounded(ctx, binary, "version")

	switch {
	case err != nil:
		return "", fmt.Errorf("%s version: %w", binary, err)
	case rc != 0:
		return "", fmt.Errorf("%s version exited %d: %s", binary, rc, strings.TrimSpace(string(stderr)))
	}

	fields := strings.Fields(string(stdout))
	if len(fields) < 2 || fields[0] != "billet" {
		return "", fmt.Errorf("%s version answered %q, not `billet <version> <platform>`", binary,
			strings.TrimSpace(string(stdout)))
	}

	if v, ok := version.Canonical(fields[1]); ok {
		return v, nil
	}

	return "", nil
}

// candidateCapable executes the candidate's `converge-guard status --json`
// under the bound and judges the answer as the status protocol's schema.
func candidateCapable(ctx context.Context, binary string) (bool, string) {
	stdout, stderr, rc, err := runBounded(ctx, binary, "converge-guard", "status", "--json")

	var timedOut bool

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			timedOut = true
		} else if rc < 0 {
			return false, "the candidate could not be run: " + err.Error()
		}
	}

	if !timedOut && rc != 0 && unknownCommandPattern.Match(append(append([]byte{}, stdout...), stderr...)) {
		return false, "the candidate answered \"unknown command\" to converge-guard"
	}

	if problem := judgeStatusAnswer(rc, stdout, stderr, timedOut); problem != "" {
		return false, problem
	}

	return true, ""
}

// runBounded runs one executable with the arguments given, under the command
// timeout, its output bounded; the exit status is -1 when it did not run.
func runBounded(ctx context.Context, binary string, args ...string) ([]byte, []byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, guardCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.WaitDelay = 10 * time.Second
	cmd.Stdin = nil
	cmd.Env = withoutNotifySocket(os.Environ())

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &limitedWriter{w: &stdout, n: maxCommandOutput}
	cmd.Stderr = &limitedWriter{w: &stderr, n: maxCommandOutput}

	err := cmd.Run()

	rc := -1
	if cmd.ProcessState != nil {
		rc = cmd.ProcessState.ExitCode()
	}

	if ctx.Err() != nil {
		return stdout.Bytes(), stderr.Bytes(), rc, context.DeadlineExceeded
	}

	return stdout.Bytes(), stderr.Bytes(), rc, err
}

// limitedWriter keeps the first n bytes and drops the rest.
type limitedWriter struct {
	w *bytes.Buffer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if room := l.n - l.w.Len(); room > 0 {
		if len(p) > room {
			l.w.Write(p[:room])
		} else {
			l.w.Write(p)
		}
	}

	return len(p), nil
}

// newGuardID mints an id or a token.
func newGuardID() (string, error) {
	buf, err := guardRandom(guardIDBytes)
	if err != nil {
		return "", fmt.Errorf("read randomness for the guard: %w", err)
	}

	if len(buf) != guardIDBytes {
		return "", errors.New("read randomness for the guard: short read")
	}

	return hex.EncodeToString(buf), nil
}

// refuseShapeJSON is refuseShape as an answer, with what resolves the shape.
func refuseShapeJSON(shape claimShape) *prepareRefusal {
	next := ""

	switch shape.Kind {
	case claimHostUpgrade:
		next = "`billet host-upgrade --status`, `--resume`"
	case claimLegacyRole:
		next = "the role's own recovery (upgrade-recover.yml), then a fresh converge"
	case claimUnpublished:
		next = "`billet converge-guard recover --unpublished`"
	}

	return refuse(reasonShape, string(shape.Kind), refuseShape(shape).Error(), next)
}

// cmdGuardSettle closes the acquirer's preparation window.
func cmdGuardSettle(args []string) error {
	flags := newFlagSet("billet converge-guard settle")
	holder := flags.String("holder", "", "the holder whose guard is settled")
	token := flags.String("token", "", "the acquiring invocation's token")
	asJSON := flags.Bool("json", false, "print the answer as JSON")

	if err := parse(flags, args); err != nil {
		return err
	}

	if err := checkHolder(*holder); err != nil {
		return err
	}

	if !guardHex32.MatchString(*token) {
		return errors.New("--token is not a 32-hex token")
	}

	root, err := prepareUpgradeRoot()
	if err != nil {
		return err
	}

	defer root.close()

	dir, shape, err := openGuardForMutation(root)
	if err != nil {
		return err
	}

	if dir != nil {
		defer func() { _ = dir.Close() }()
	}

	if shape.Kind != claimGuard {
		return refuseShape(shape)
	}

	if shape.RecordErr != "" {
		return fmt.Errorf("%w, and its record cannot be read (%s); nothing was settled", errGuardHeld, shape.RecordErr)
	}

	if shape.Guard.Holder != *holder {
		return fmt.Errorf("%w: it names %s, not %s; nothing was settled", errGuardHeld, shape.Guard.Holder, *holder)
	}

	if _, r := removeStrayTemporary(dir); r != nil {
		return errors.New(r.Why)
	}

	if r := verifyRecorded(shape); r != nil {
		return errors.New(r.Why)
	}

	// THE OWED FLUSHES, before the answer, settled already or not.
	if err := syncDirFD(dir); err != nil {
		return err
	}

	if err := syncDirFD(root.dir); err != nil {
		return err
	}

	if !shape.Guard.Preparing {
		return &exitError{code: exitRefused, msg: "the guard is already settled; nothing was written"}
	}

	if shape.Guard.Token != *token {
		return &exitError{code: exitRefused, msg: "the token is not this guard's; nothing was settled"}
	}

	record := shape.Guard
	record.Preparing = false

	if err := rewriteGuardRecord(root, dir, record); err != nil {
		return err
	}

	if *asJSON {
		return answerJSON(map[string]any{"outcome": "settled", "id": record.ID, "holder": record.Holder}, 0, "")
	}

	fmt.Printf("settled: %s\n", record.ID)

	return nil
}

// cleanupRelease is `release --cleanup --token`: the acquiring invocation
// releasing the guard it published, inside its own preparation window.
func cleanupRelease(root *txLock, dir *os.File, shape claimShape, holder, token string) error {
	if shape.RecordErr != "" {
		return fmt.Errorf("%w, and its record cannot be read (%s); nothing was released", errGuardHeld, shape.RecordErr)
	}

	if shape.Guard.Holder != holder {
		return fmt.Errorf("%w: it names %s and this release is by %s; nothing was released",
			errGuardHeld, shape.Guard.Holder, holder)
	}

	if _, r := removeStrayTemporary(dir); r != nil {
		return errors.New(r.Why)
	}

	if r := verifyRecorded(shape); r != nil {
		return errors.New(r.Why)
	}

	if !shape.Guard.Preparing {
		return &exitError{code: exitRefused, msg: fmt.Sprintf("the preparation window has closed; "+
			"`billet converge-guard release --holder %s` releases it", holder)}
	}

	if shape.Guard.Token != token {
		return &exitError{code: exitRefused, msg: "the token is not this guard's; nothing was released"}
	}

	if err := requireNoPointerAt(dir); err != nil {
		return err
	}

	return removeGuardAt(root, dir)
}

// prepareDryRun reports every shape, lock-free, writing nothing.
func prepareDryRun(ctx context.Context) error {
	shape, err := classifyClaim()
	if err != nil {
		return answerRefusal(couldNotTell(err.Error()))
	}

	report := prepareReport{Outcome: prepareReported, Shape: string(shape.Kind), Why: shape.Why,
		Managed: describeManaged(ctx)}

	if shape.Kind != claimGuard {
		return answerJSON(report, 0, "")
	}

	g := &prepareDryGuard{
		Holder: shape.Guard.Holder, ClaimedAt: shape.Guard.ClaimedAt, Hostname: shape.Guard.Hostname,
		Preparing: shape.Guard.Preparing, RecordError: shape.RecordErr,
		Record: prepareRecord{ReleaseExecutable: shape.Guard.ReleaseExecutable,
			ReleaseExecutableSHA256: shape.Guard.ReleaseExecutableSHA256},
		StrayTemporary: shape.StrayTemporary,
	}

	if shape.Guard.ID != "" {
		id := shape.Guard.ID
		g.ID = &id
	}

	switch {
	case shape.RecordErr != "":
		g.Record.Verified = "unknown"
	case shape.Guard.ReleaseExecutable == "" || shape.Guard.ReleaseExecutableSHA256 == "":
		g.Record.Verified = "unknown"
	default:
		sum, _, err := hashRegular(shape.Guard.ReleaseExecutable, maxExecutableBytes)
		if err != nil {
			g.Record.Verified = "unknown"
		} else {
			g.Record.Verified = sum == shape.Guard.ReleaseExecutableSHA256
		}
	}

	// THE POINTER, validated lock-free through a fresh descriptor of the root.
	rootDir, err := os.OpenFile(upgradeRoot, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err == nil {
		defer func() { _ = rootDir.Close() }()

		if active, err := openActiveOf(rootDir); err == nil {
			defer func() { _ = active.Close() }()

			pointer, target, r := validatePointer(rootDir, active)

			switch {
			case r != nil && r.Outcome == prepareRefused:
				// AN ENTRY EXISTS AND IS WRONG: a pointer nobody can follow is
				// still a pointer, reported as present with its problem, so a
				// dry run never shows a guard without its interrupted transaction.
				g.Pointer, g.PointerProblem = true, r.Why
			case r != nil:
				g.PointerProblem = r.Why
			default:
				g.Pointer, g.PointerTarget = pointer, target
			}
		} else {
			g.PointerProblem = fmt.Sprintf("examine the guard directory: %v", err)
		}
	} else {
		g.PointerProblem = fmt.Sprintf("open the upgrade root: %v", err)
	}

	report.Guard = g

	return answerJSON(report, 0, "")
}

// judgeStatusAnswer judges a `converge-guard status --json` answer as the
// status protocol's schema, in the three stages the Python module read it:
// the envelope, a record the command could not read, the healthy record's
// members. It answers the problem, or "" for an admitted answer.
func judgeStatusAnswer(rc int, stdout, stderr []byte, timedOut bool) string {
	if timedOut {
		return "envelope: the executable did not answer within the bound"
	}

	if rc != 0 {
		text := strings.TrimSpace(string(stderr))
		if text == "" {
			text = strings.TrimSpace(string(stdout))
		}

		if len(text) > 500 {
			text = text[len(text)-500:]
		}

		return fmt.Sprintf("envelope: the executable exited %d: %s", rc, text)
	}

	var report map[string]json.RawMessage

	dec := json.NewDecoder(bytes.NewReader(stdout))
	if err := dec.Decode(&report); err != nil {
		return "envelope: the answer is not JSON (" + err.Error() + ")"
	}

	if report == nil {
		return "envelope: the answer is not a JSON object"
	}

	var active string
	if err := json.Unmarshal(report["active"], &active); err != nil || report["active"] == nil {
		return "envelope: active is missing or not a string"
	}

	switch claimKind(active) {
	case claimNone, claimHostUpgrade, claimLegacyRole, claimGuard, claimUnpublished, claimUnknown:
	default:
		return fmt.Sprintf("envelope: active is %q, which is not a word this command knows", active)
	}

	var why string
	if raw, ok := report["why"]; ok {
		if err := json.Unmarshal(raw, &why); err != nil {
			return "envelope: why is not a string"
		}
	}

	if active == string(claimUnknown) && why == "" {
		return "envelope: why is missing or empty under active unknown"
	}

	if active != string(claimGuard) {
		return ""
	}

	var guard map[string]json.RawMessage
	if err := json.Unmarshal(report["guard"], &guard); err != nil || guard == nil {
		return "guard: missing or not an object under active converge-guard"
	}

	if raw, ok := guard["record_error"]; ok {
		var recordErr string
		if err := json.Unmarshal(raw, &recordErr); err != nil || recordErr == "" {
			return "guard: record_error is not a non-empty string"
		}

		return ""
	}

	var holder string
	if err := json.Unmarshal(guard["holder"], &holder); err != nil || guard["holder"] == nil {
		return "guard: holder is missing or not a string"
	}

	if err := checkHolder(holder); err != nil {
		return fmt.Sprintf("guard: holder %q is not a name", holder)
	}

	var claimed string
	if err := json.Unmarshal(guard["claimed_at"], &claimed); err != nil || guard["claimed_at"] == nil {
		return "guard: claimed_at is not an RFC 3339 time"
	}

	if _, err := time.Parse(time.RFC3339, claimed); err != nil {
		return "guard: claimed_at is not an RFC 3339 time"
	}

	var hostname string
	if err := json.Unmarshal(guard["hostname"], &hostname); err != nil || guard["hostname"] == nil {
		return "guard: hostname is missing or not a string"
	}

	var pointer bool
	if err := json.Unmarshal(guard["recovery_pointer"], &pointer); err != nil || guard["recovery_pointer"] == nil {
		return "guard: recovery_pointer is missing or not a boolean"
	}

	var exe string
	if err := json.Unmarshal(guard["release_executable"], &exe); err != nil || !strings.HasPrefix(exe, "/") {
		return "guard: release_executable is not an absolute path"
	}

	var digest string
	if err := json.Unmarshal(guard["release_executable_sha256"], &digest); err != nil || !sha256Hex.MatchString(digest) {
		return "guard: release_executable_sha256 is not 64 lowercase hex digits"
	}

	raw, ok := guard["release_executable_verified"]
	if !ok {
		return "guard: release_executable_verified is missing"
	}

	var verifiedBool bool
	if err := json.Unmarshal(raw, &verifiedBool); err == nil {
		return ""
	}

	var verifiedObj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &verifiedObj); err == nil && len(verifiedObj) == 1 {
		var reason string
		if err := json.Unmarshal(verifiedObj["unknown"], &reason); err == nil && reason != "" {
			return ""
		}
	}

	return "guard: release_executable_verified is neither true, false nor an object whose one member is a " +
		"non-empty string unknown"
}

// sha256Hex is the shape of a digest.
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
