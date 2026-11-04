package tart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"syscall"
	"time"

	"github.com/junioryono/billet/internal/regularfile"
)

// The record of what this node verified (ADR-021): three files under node.state_dir,
// and one lock beside them that serialises their reads and commits and is never held
// across a probe.
const (
	verifiedFile  = "tart-verified.json"
	withdrawnFile = "tart-withdrawn.json"
	deadlinesFile = "tart-runner-deadlines.json"
	recordLock    = "tart-verified.lock"

	verifiedSchema = 1
	// recordLimit bounds a record read: two bases per image and a small catalogue.
	recordLimit = 4 << 20
	// recordLockWindow bounds the wait for the record lock, whose holders do no slow work.
	recordLockWindow = 30 * time.Second
)

// baseFile is the identity of one file of a base. A change time no user can set is
// what makes it notice a file rewritten in place. NO DEVICE NUMBER: macOS numbers a
// volume by when it mounts, so one that moved at a reboot would mark every base invalid,
// and with them every tier, for a file nothing touched.
type baseFile struct {
	Name      string `json:"name"`
	Ino       uint64 `json:"ino"`
	Size      int64  `json:"size"`
	CtimeNsec int64  `json:"ctime_nsec"`
}

// VerifiedRecord is one verification's result.
type VerifiedRecord struct {
	// Image is the normalised key (config.TartImageKey).
	Image string `json:"image"`
	// Base is the local VM every launch clones.
	Base string `json:"base"`
	// Generation is unique per verification of this image and orders its records.
	Generation int64 `json:"generation"`
	// Digest is what a remote reference was pulled as, and empty for a local VM.
	Digest string `json:"digest,omitempty"`
	// ContentHash is the sha256 of the base's disk, configuration and NVRAM, kept for
	// the audit, which tells apart two Macs that recorded one digest for other bytes.
	ContentHash string `json:"content_hash"`
	// ContentKey leaves out what a clone regenerates, so a clone of an unchanged VM
	// never counts as new content.
	ContentKey string `json:"content_key"`
	// Runner is the version the guest's runner answered.
	Runner string `json:"runner"`
	// GuestOS is tart's own word for the base: darwin or linux.
	GuestOS    string    `json:"guest_os"`
	VerifiedAt time.Time `json:"verified_at"`
	// Files are the identities of the base's disk, configuration and NVRAM, never its
	// directory, whose change time tart touches on every clone.
	Files []baseFile `json:"files"`
	// Invalid says why a launch may no longer clone this base, and empty means it may.
	Invalid string `json:"invalid,omitempty"`
}

// imageProgress is what outlives a record of one image.
type imageProgress struct {
	NextGeneration int64 `json:"next_generation"`
	// UnpromoteEpoch advances with every unpromote of the image. A verification that
	// started before one commits nothing, so a probe in flight never undoes it.
	UnpromoteEpoch int64 `json:"unpromote_epoch"`
	// Yields counts verifications cut short by a job since the last one finished, and
	// `billet check` reports them beside the runner's deadline.
	Yields int `json:"yields,omitempty"`
}

// verifiedDocument is tart-verified.json.
type verifiedDocument struct {
	Schema     int                       `json:"schema"`
	Deployment string                    `json:"deployment"`
	Images     map[string]*imageProgress `json:"images"`
	Records    []VerifiedRecord          `json:"records"`
}

// withdrawnDocument is tart-withdrawn.json: digests no retention ever touches, by image.
type withdrawnDocument struct {
	Schema    int                 `json:"schema"`
	Withdrawn map[string][]string `json:"withdrawn"`
}

// deadlinesDocument is tart-runner-deadlines.json: the date GitHub stops sending jobs
// to a runner, by version, shared by every record of that version and never erased by
// a could-not-tell or by a base going away.
type deadlinesDocument struct {
	Schema    int                  `json:"schema"`
	Deadlines map[string]time.Time `json:"deadlines"`
}

// verifiedState is the three files, read under the record lock.
type verifiedState struct {
	verified  verifiedDocument
	withdrawn withdrawnDocument
	deadlines deadlinesDocument
}

func (s *verifiedState) image(key string) *imageProgress {
	if s.verified.Images[key] == nil {
		s.verified.Images[key] = &imageProgress{NextGeneration: 1}
	}

	return s.verified.Images[key]
}

func (s *verifiedState) isWithdrawn(key, digest string) bool {
	return digest != "" && slices.Contains(s.withdrawn.Withdrawn[key], digest)
}

// recordsOf is the image's records, newest first.
func (s *verifiedState) recordsOf(key string) []VerifiedRecord {
	var out []VerifiedRecord

	for _, r := range s.verified.Records {
		if r.Image == key {
			out = append(out, r)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Generation > out[j].Generation })

	return out
}

// removeRecords drops the image's records the predicate names and answers their bases,
// which the caller deletes after it has let go of the record lock.
func (s *verifiedState) removeRecords(key string, drop func(VerifiedRecord) bool) []string {
	var bases []string

	s.verified.Records = slices.DeleteFunc(s.verified.Records, func(r VerifiedRecord) bool {
		if r.Image == key && drop(r) {
			bases = append(bases, r.Base)

			return true
		}

		return false
	})

	return bases
}

// readJSONRecord reads one file. Absence is false with no error; one that is there and
// cannot be read is an error, never absence, because an absent record is a base no
// record names, which the next verification removes.
func readJSONRecord(path string, into any) (bool, error) {
	body, err := regularfile.ReadFile(path, recordLimit, regularfile.Options{NoFollow: true})
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	if err := json.Unmarshal(body, into); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}

	return true, nil
}

// checkSchema refuses a document this build does not know how to read.
func checkSchema(path string, found bool, schema int) error {
	if found && schema != verifiedSchema {
		return fmt.Errorf("%s has schema %d and this billet reads %d", path, schema, verifiedSchema)
	}

	return nil
}

// loadVerifiedState reads the three files. The caller holds the record lock.
func (p *Provider) loadVerifiedState() (*verifiedState, error) {
	st := &verifiedState{}

	path := filepath.Join(p.stateDir, verifiedFile)

	found, err := readJSONRecord(path, &st.verified)
	if err != nil {
		return nil, err
	}

	if err := checkSchema(path, found, st.verified.Schema); err != nil {
		return nil, err
	}

	// A RECORD OF ANOTHER DEPLOYMENT NAMES ITS BASES, which are not this one's to clone
	// or delete: a state directory moved between deployments starts again.
	if found && st.verified.Deployment != p.owner {
		return nil, fmt.Errorf("%s records deployment %s and this node is %s; verified bases are "+
			"per deployment, so move the file aside and verify again",
			path, st.verified.Deployment, p.owner)
	}

	path = filepath.Join(p.stateDir, withdrawnFile)

	if found, err = readJSONRecord(path, &st.withdrawn); err != nil {
		return nil, err
	}

	if err := checkSchema(path, found, st.withdrawn.Schema); err != nil {
		return nil, err
	}

	path = filepath.Join(p.stateDir, deadlinesFile)

	if found, err = readJSONRecord(path, &st.deadlines); err != nil {
		return nil, err
	}

	if err := checkSchema(path, found, st.deadlines.Schema); err != nil {
		return nil, err
	}

	st.verified.Schema, st.verified.Deployment = verifiedSchema, p.owner
	st.withdrawn.Schema, st.deadlines.Schema = verifiedSchema, verifiedSchema

	if st.verified.Images == nil {
		st.verified.Images = map[string]*imageProgress{}
	}

	if st.withdrawn.Withdrawn == nil {
		st.withdrawn.Withdrawn = map[string][]string{}
	}

	if st.deadlines.Deadlines == nil {
		st.deadlines.Deadlines = map[string]time.Time{}
	}

	return st, nil
}

// saveVerifiedState installs the three files, the withdrawals and deadlines first, so a
// crash between them can leave a digest withdrawn with its record still there, which
// resolution skips, and never a record removed with its digest still eligible.
func (p *Provider) saveVerifiedState(st *verifiedState) error {
	for _, doc := range []struct {
		name string
		body any
	}{
		{withdrawnFile, st.withdrawn},
		{deadlinesFile, st.deadlines},
		{verifiedFile, st.verified},
	} {
		body, err := json.MarshalIndent(doc.body, "", "  ")
		if err != nil {
			return fmt.Errorf("encode %s: %w", doc.name, err)
		}

		body = append(body, '\n')

		if _, err := p.installer.Install(p.stateDir, doc.name, 0o600, func(w io.Writer) error {
			_, err := w.Write(body)

			return err
		}); err != nil {
			return fmt.Errorf("record %s: %w", doc.name, err)
		}
	}

	return nil
}

// withRecords runs fn over the records under their lock, and installs them when fn
// answers true. fn does no slow work: a launch waits on this lock.
func (p *Provider) withRecords(ctx context.Context, fn func(*verifiedState) (bool, error)) error {
	if p.stateDir == "" {
		return errors.New("this provider was built without node.state_dir, where verified images are recorded")
	}

	held, err := lockPath(ctx, filepath.Join(p.stateDir, recordLock), syscall.LOCK_EX, recordLockWindow)
	if err != nil {
		return fmt.Errorf("lock the verified image records: %w", err)
	}

	defer held.release()

	st, err := p.loadVerifiedState()
	if err != nil {
		return err
	}

	write, err := fn(st)
	if err != nil {
		return err
	}

	if !write {
		return nil
	}

	return p.saveVerifiedState(st)
}
