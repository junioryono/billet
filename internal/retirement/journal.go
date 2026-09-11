package retirement

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"syscall"
	"time"

	"github.com/junioryono/billet/internal/regularfile"
)

// JournalSchema is the journal's shape number. A journal from another shape
// is refused whole, never read in part.
const JournalSchema = 1

// maxJournalBytes bounds a read of the journal: a few hundred bytes of fields
// and a node list; a longer file is not one this binary wrote.
const maxJournalBytes = 64 << 10

// Journal is a controller's retirement as the retiring host records it, at
// the fixed path under the private retirement directory, one phase at a time.
//
// EVERY LATER PHASE VALIDATES THIS RECORD and never re-derives eligibility from
// a configuration the transition has removed `server:` from: the backend, the
// controller mode, the survivor and the digests were decided before `intent`
// and are what a resume is held to.
type Journal struct {
	Schema  int     `json:"schema"`
	Phase   Phase   `json:"phase"`
	Variant Variant `json:"variant"`

	// Deployment is the identity being retired; Retiring the retiring host's
	// INVENTORY name.
	Deployment string `json:"deployment"`
	Retiring   string `json:"retiring"`

	Survivor JournalSurvivor `json:"survivor"`

	// Backend and Controllers are the eligibility as decided at intent.
	Backend     string `json:"backend"`
	Controllers string `json:"controllers"`

	IdentityDir string `json:"identity_dir"`
	Archive     string `json:"archive"`

	// InstalledSHA256 is the installed configuration's digest at intent;
	// StagedSHA256 the serverless rendering's, empty on a server-only host,
	// whose Config is "absent".
	InstalledSHA256 string `json:"installed_sha256"`
	StagedSHA256    string `json:"staged_sha256"`
	Config          string `json:"config"`

	Nodes                    []JournalNode `json:"nodes"`
	EndpointFailoverVerified bool          `json:"endpoint_failover_verified"`

	Locator JournalLocator `json:"locator"`

	// Provenance is immutable from intent on; Ownership moves with every
	// takeover and rebinding.
	Provenance Provenance `json:"provenance"`
	Ownership  Ownership  `json:"ownership"`

	// TimerStoppedAt is written while the journal is still at intent,
	// immediately after the timers are stopped and before the status closes,
	// so a resume recognises a backup that refused in that window.
	TimerStoppedAt string `json:"timer_stopped_at"`

	// RowDone acknowledges the ledger row's `done`, after which no phase check
	// opens a ledger; CompletedBy names who wrote it. Settled is the tail's
	// end, written after the guard's marker is cleared.
	RowDone     bool   `json:"row_done"`
	CompletedBy string `json:"completed_by"`
	Settled     bool   `json:"settled"`

	WrittenAt string `json:"written_at"`
	DoneAt    string `json:"done_at"`
}

// JournalSurvivor is the survivor as recorded at intent.
type JournalSurvivor struct {
	Host       string `json:"host"`
	Deployment string `json:"deployment"`
	CASHA256   string `json:"ca_sha256"`
}

// JournalNode is one node admitted at intent.
type JournalNode struct {
	Name        string `json:"name"`
	Incarnation string `json:"incarnation"`
	Endpoint    string `json:"endpoint"`
}

// JournalLocator is how a later phase reopens the ledger once `server:` is
// gone from the installed configuration: the backend, the DSN variable's NAME,
// the environment file that holds it, and where the identity lives.
type JournalLocator struct {
	Backend         string `json:"backend"`
	DSNEnv          string `json:"dsn_env"`
	EnvironmentFile string `json:"environment_file"`
	IdentityDir     string `json:"identity_dir"`
	Archive         string `json:"archive"`
}

// Provenance is who reserved the retirement and under which id: immutable.
type Provenance struct {
	ReservingHolder string `json:"reserving_holder"`
	TransitionID    string `json:"transition_id"`
	Reservation     string `json:"reservation"`
	Deployment      string `json:"deployment"`
	Retiring        string `json:"retiring"`
	Survivor        string `json:"survivor"`
}

// Ownership is who drives the retirement now, and everyone who did before.
type Ownership struct {
	Owner  string   `json:"owner"`
	Owners []string `json:"owners"`
}

// JournalPresence is the four-valued answer a journal read gives.
type JournalPresence int

const (
	JournalAbsent JournalPresence = iota
	JournalPresent
	JournalUnreadable
	JournalMalformed
)

// ReadJournal reads the journal at its fixed path, typed: a failed read is
// never absence, a file that does not parse or that another shape wrote is
// malformed, and a file not owned by this process's account with the mode
// billet writes is refused as untrusted (a planted journal names a different
// owner, or a mode a stranger could have written under).
func ReadJournal() (Journal, JournalPresence, error) {
	return readJournalAt(JournalPath())
}

func readJournalAt(path string) (Journal, JournalPresence, error) {
	f, info, err := regularfile.Open(path, regularfile.Options{NoFollow: true})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Journal{}, JournalAbsent, nil
		}

		return Journal{}, JournalUnreadable, fmt.Errorf("retirement: open %s: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	if err := trustJournal(path, info); err != nil {
		return Journal{}, JournalUnreadable, err
	}

	if info.Size() > maxJournalBytes {
		return Journal{}, JournalMalformed, fmt.Errorf("retirement: %s is %d bytes, longer than any journal billet writes", path, info.Size())
	}

	// THROUGH EOF, never to the size a stat reported: a file grown under the
	// read can hold a complete document in that prefix with more behind it,
	// and a prefix that decodes is not proof of a whole file.
	raw, err := io.ReadAll(io.LimitReader(f, maxJournalBytes+1))
	if err != nil {
		return Journal{}, JournalUnreadable, fmt.Errorf("retirement: read %s: %w", path, err)
	}

	if len(raw) > maxJournalBytes {
		return Journal{}, JournalMalformed, fmt.Errorf("retirement: %s is longer than %d bytes, longer than any journal billet writes", path, maxJournalBytes)
	}

	after, err := f.Stat()
	if err != nil {
		return Journal{}, JournalUnreadable, fmt.Errorf("retirement: examine %s after the read: %w", path, err)
	}

	if after.Size() != int64(len(raw)) || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return Journal{}, JournalUnreadable, fmt.Errorf("retirement: %s changed under the read", path)
	}

	var j Journal
	if err := strictDecode(raw, &j); err != nil {
		return Journal{}, JournalMalformed, fmt.Errorf("retirement: %s: %w", path, err)
	}

	if err := j.wellFormed(); err != nil {
		return Journal{}, JournalMalformed, fmt.Errorf("retirement: %s: %w", path, err)
	}

	return j, JournalPresent, nil
}

// trustJournal is the ownership rule: the file belongs to the account reading
// it (root in production) and nobody else may write it.
func trustJournal(path string, info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("retirement: %s: ownership could not be read", path)
	}

	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("retirement: %s is owned by uid %d, not by this process's account (uid %d); "+
			"billet does not trust a journal it did not write", path, st.Uid, os.Geteuid())
	}

	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("retirement: %s has mode %04o, and a journal another account could write is not "+
			"one billet trusts", path, info.Mode().Perm())
	}

	return nil
}

// wellFormed is the shape check every read applies before any field is used.
func (j *Journal) wellFormed() error {
	switch {
	case j.Schema != JournalSchema:
		return fmt.Errorf("journal schema %d is not %d, the one this billet reads", j.Schema, JournalSchema)
	case !j.Phase.Known():
		return fmt.Errorf("journal names the phase %q, which this billet does not know", j.Phase)
	case j.Variant != VariantServerOnly && j.Variant != VariantRetainedNode:
		return fmt.Errorf("journal names the variant %q, which this billet does not know", j.Variant)
	case j.Deployment == "" || j.Retiring == "" || j.Survivor.Host == "":
		return errors.New("journal names no deployment, retiring host or survivor")
	case j.Provenance.TransitionID == "" || j.Provenance.Reservation == "":
		return errors.New("journal carries no provenance (transition id and reservation)")
	case j.Ownership.Owner == "":
		return errors.New("journal names no owner")
	case j.Variant == VariantServerOnly && (j.StagedSHA256 != "" || j.Config != "absent"):
		return errors.New("a server-only journal has no stage and records config: absent")
	case j.Variant == VariantRetainedNode && (j.StagedSHA256 == "" || j.Config != "present"):
		return errors.New("a retained-node journal records its stage's digest and config: present")
	}

	if _, err := time.Parse(time.RFC3339, j.WrittenAt); err != nil {
		return fmt.Errorf("journal carries an unparseable written_at: %w", err)
	}

	return j.tailWellFormed()
}

// tailWellFormed is the order of the tail as invariants: done_at with done,
// the row acknowledged only at done and only by someone, settled only after
// the acknowledgement. A journal that says "settled" over a row nobody
// completed would authorise the release the acknowledgement exists to gate.
func (j *Journal) tailWellFormed() error {
	switch {
	case j.Phase != PhaseDone && (j.DoneAt != "" || j.RowDone || j.CompletedBy != "" || j.Settled):
		return fmt.Errorf("journal at %s carries the tail's members (done_at, row_done, completed_by, settled), which only done carries", j.Phase)
	case j.Phase == PhaseDone && j.DoneAt == "":
		return errors.New("journal at done carries no done_at")
	case j.RowDone != (j.CompletedBy != ""):
		return errors.New("journal's row_done and completed_by disagree: the row is acknowledged by someone or not at all")
	case j.Settled && !j.RowDone:
		return errors.New("journal is settled over a row nobody acknowledged; settlement follows the acknowledgement")
	}

	if j.Phase == PhaseDone {
		if _, err := time.Parse(time.RFC3339, j.DoneAt); err != nil {
			return fmt.Errorf("journal carries an unparseable done_at: %w", err)
		}
	}

	return nil
}

// Write publishes the journal durably at its fixed path: temp, fsync, rename,
// the directory flushed. Root only in production; the retirement directory is
// created 0700 and its parent flushed when it does not exist yet.
func (j *Journal) Write(now time.Time) error {
	j.Schema = JournalSchema
	j.WrittenAt = now.UTC().Format(time.RFC3339Nano)

	if err := j.wellFormed(); err != nil {
		return fmt.Errorf("retirement: refuse to write a journal that does not hold: %w", err)
	}

	if err := EnsureRetiredDir(); err != nil {
		return err
	}

	body, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("retirement: encode the journal: %w", err)
	}

	body = append(body, '\n')

	// THE WRITER IS HELD TO THE READER'S BOUND, before anything is published:
	// a journal its own reader refuses is a retirement nothing can resume.
	if len(body) > maxJournalBytes {
		return fmt.Errorf("retirement: refuse to write a %d-byte journal, longer than the %d bytes its reader "+
			"admits (%d nodes recorded); the previous journal is kept", len(body), maxJournalBytes, len(j.Nodes))
	}

	return publish(JournalPath(), body, 0o600)
}

// EnsureRetiredDir creates the private retirement directory 0700 when it is
// absent, and in every case examines it (this account's, 0700, not a link)
// and flushes its parent: an existing directory is never re-owned, and the
// parent is flushed on readmission too, because the invocation that created
// the directory may have died before its own flush and the obligation is
// still owed.
func EnsureRetiredDir() error {
	dir := RetiredDir()

	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("retirement: create %s: %w", dir, err)
	}

	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("retirement: examine %s: %w", dir, err)
	}

	if err := trustRetiredDir(dir, info); err != nil {
		return err
	}

	return syncDir(Root)
}

// trustRetiredDir is the retirement directory's ownership rule: a directory,
// not a link, owned by the account writing into it, with no group or other
// bits. A 0600 journal in a directory another account can write is a journal
// that account can remove or replace by its entry.
func trustRetiredDir(dir string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("retirement: %s exists and is not a directory", dir)
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("retirement: %s: ownership could not be read", dir)
	}

	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("retirement: %s is owned by uid %d, not by this process's account (uid %d); billet does "+
			"not write a retirement into a directory it does not own", dir, st.Uid, os.Geteuid())
	}

	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("retirement: %s has mode %04o, not 0700; a retirement directory another account could "+
			"write is not one billet trusts", dir, info.Mode().Perm())
	}

	return nil
}

// JournalExpectation is what a reader that will ACT on a journal knows from
// elsewhere and holds the journal to: the retiring host it was asked about,
// the guard's current holder and takeover chain, the ledger row when one was
// read, and the identity where the phase says it lives.
type JournalExpectation struct {
	Retiring string
	// Holder is the current guard holder; TakenOverFrom the record's list of
	// previous holders. An INCOMPLETE journal's owner must be one of them; a
	// SETTLED `done` journal is validated by provenance alone.
	Holder        string
	TakenOverFrom []string
	// Row is the ledger row when the caller read one; nil when none was read
	// (a settled `done` opens no ledger). A row at intent or later must carry
	// the journal's transition id; a reserved row its reservation and survivor.
	Row *RowFacts
	// Identity is the deployment id read where the phase admits it; empty
	// skips the comparison (a caller that could not read it refuses on its
	// own account, never by passing an empty expectation silently).
	Identity string
}

// RowFacts is the ledger row's binding fields as the caller read them.
type RowFacts struct {
	State        string
	Retiring     string
	Survivor     string
	ReservedAt   string
	TransitionID string
}

// ErrJournalMismatch is a journal that does not describe the retirement the
// caller is acting on; the wrapped message names the field.
var ErrJournalMismatch = errors.New("retirement: the journal does not describe this retirement")

// Validate holds a well-formed journal to what the caller knows. Individually
// mismatched fields are individually named, so a fixture can pin each.
func (j *Journal) Validate(exp JournalExpectation) error {
	if err := j.wellFormed(); err != nil {
		return fmt.Errorf("%w: %w", ErrJournalMismatch, err)
	}

	mismatch := func(field string, have, want any) error {
		return fmt.Errorf("%w: %s is %v, not %v", ErrJournalMismatch, field, have, want)
	}

	if exp.Retiring != "" && j.Retiring != exp.Retiring {
		return mismatch("retiring", j.Retiring, exp.Retiring)
	}

	if exp.Identity != "" && j.Deployment != exp.Identity {
		return mismatch("deployment", j.Deployment, exp.Identity)
	}

	switch {
	case j.Provenance.Deployment != j.Deployment:
		return mismatch("provenance.deployment", j.Provenance.Deployment, j.Deployment)
	case j.Provenance.Retiring != j.Retiring:
		return mismatch("provenance.retiring", j.Provenance.Retiring, j.Retiring)
	case j.Provenance.Survivor != j.Survivor.Host:
		return mismatch("provenance.survivor", j.Provenance.Survivor, j.Survivor.Host)
	}

	if row := exp.Row; row != nil {
		switch {
		case row.Retiring != j.Retiring:
			return mismatch("row.retiring", row.Retiring, j.Retiring)
		case row.Survivor != j.Survivor.Host:
			return mismatch("row.survivor", row.Survivor, j.Survivor.Host)
		case row.ReservedAt != j.Provenance.Reservation:
			return mismatch("row.reserved_at", row.ReservedAt, j.Provenance.Reservation)
		case row.TransitionID != j.Provenance.TransitionID:
			return mismatch("row.transition_id", row.TransitionID, j.Provenance.TransitionID)
		}
	}

	if j.Phase == PhaseDone && j.Settled {
		return nil
	}

	if exp.Holder != "" && j.Ownership.Owner != exp.Holder &&
		!slices.Contains(exp.TakenOverFrom, j.Ownership.Owner) {
		return fmt.Errorf("%w: the journal is owned by %q, and the guard's holder %q took over from none of %v",
			ErrJournalMismatch, j.Ownership.Owner, exp.Holder, exp.TakenOverFrom)
	}

	return nil
}

// Rebind makes holder the journal's owner and appends the previous owner to
// the chain, never overwriting it. A holder that already owns it changes
// nothing.
func (j *Journal) Rebind(holder string) {
	if holder == "" || j.Ownership.Owner == holder {
		return
	}

	j.Ownership.Owners = append(j.Ownership.Owners, j.Ownership.Owner)
	j.Ownership.Owner = holder
}
