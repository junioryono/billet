package deployarchive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/version"
	"github.com/junioryono/billet/internal/wirecert"
)

// BackupRequest is everything Write needs that it must not go and find for
// itself.
//
// THE APP KEY ARRIVES AS BYTES, deliberately. Reading it correctly is a page of
// rules — one descriptor, O_NONBLOCK so a FIFO cannot hang the process, regular
// file, bounded, mode-checked, and actually parsed — and those rules already
// exist in the command layer, where `billet check` and `billet server` share
// them. A second implementation here would be a second thing to keep right.
//
// Snapshot is the ledger copy, injected for the same reason: DB.SnapshotInto is
// a method on an open handle, and which handle to open (admin, against a
// possibly-live control plane) is the command's decision rather than this
// package's.
type BackupRequest struct {
	// Dest is the directory to create. Must be absolute.
	Dest string
	// StateDir is the control plane's state directory.
	StateDir string
	// ConfigPath is where the deployment's billet.yaml lives.
	ConfigPath string
	// DeploymentID is what state.PeekDeploymentID answered. Required: a state
	// directory with no identity is not a deployment to back up.
	DeploymentID string
	// GitHub is the DEFAULT target's App identity from the config.
	GitHub GitHubIdentity
	// AppKeyPEM is the default target's App private key, already validated by
	// the caller.
	AppKeyPEM []byte
	// Targets are the further targets the deployment serves, each with its key.
	//
	// ALL OR NONE, like every other piece: a backup that captured one target's
	// key and not another's restores a control plane that serves half its
	// owners, and the half it does not serve fails hours later with a bare 401.
	Targets []TargetKey
	// ConfigBody is the billet.yaml as it stands. Copied for REFERENCE; restore
	// never installs it, because these paths are the source host's.
	ConfigBody []byte
	// Snapshot writes a consistent ledger copy to the absolute path it is given.
	// Nil exactly when ExternalLedger is set.
	Snapshot func(ctx context.Context, dest string) error
	// ExternalLedger says the ledger is not billet's to copy, and describes the
	// one this identity belongs to. Set exactly when Snapshot is nil.
	ExternalLedger *ExternalLedger
	// Now is the clock, so a test can pin the manifest's timestamp.
	Now func() time.Time
	// Hostname is recorded in the manifest as provenance.
	Hostname string
}

// TargetKey is one further target's identity and its App private key.
type TargetKey struct {
	Name      string
	GitHub    GitHubIdentity
	AppKeyPEM []byte
}

// String renders the target and never its key.
func (k TargetKey) String() string {
	return fmt.Sprintf("deployarchive.TargetKey{Name:%q GitHub:%s key:[redacted]}", k.Name, k.GitHub)
}

// GoString covers %#v, which does not consult String.
func (k TargetKey) GoString() string { return k.String() }

// Format makes every verb safe: fmt consults Stringer only for some verbs and
// otherwise formats the fields, key bytes included.
func (k TargetKey) Format(s fmt.State, _ rune) {
	//nolint:errcheck // fmt.State has no error channel; a failed write to it is the caller's output problem.
	io.WriteString(s, k.String())
}

// MarshalJSON renders the target and never its key.
func (k TargetKey) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Name   string `json:"name"`
		GitHub string `json:"github"`
		Key    string `json:"key"`
	}{Name: k.Name, GitHub: k.GitHub.String(), Key: "[redacted]"})
}

// LogValue renders the target and never its key.
func (k TargetKey) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", k.Name),
		slog.String("github", k.GitHub.String()),
		slog.String("key", "[redacted]"),
	)
}

// String renders the request and never a key.
func (req BackupRequest) String() string {
	return fmt.Sprintf("deployarchive.BackupRequest{Dest:%q StateDir:%q DeploymentID:%q GitHub:%s "+
		"targets:%d key:[redacted]}", req.Dest, req.StateDir, req.DeploymentID, req.GitHub,
		len(req.Targets))
}

// GoString covers %#v, which does not consult String.
func (req BackupRequest) GoString() string { return req.String() }

// Format makes every verb safe.
func (req BackupRequest) Format(s fmt.State, _ rune) {
	//nolint:errcheck // fmt.State has no error channel; a failed write to it is the caller's output problem.
	io.WriteString(s, req.String())
}

// MarshalJSON renders the request and never a key.
func (req BackupRequest) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Dest         string `json:"dest"`
		StateDir     string `json:"state_dir"`
		DeploymentID string `json:"deployment_id"`
		GitHub       string `json:"github"`
		Targets      int    `json:"targets"`
		Key          string `json:"key"`
	}{
		Dest: req.Dest, StateDir: req.StateDir, DeploymentID: req.DeploymentID,
		GitHub: req.GitHub.String(), Targets: len(req.Targets), Key: "[redacted]",
	})
}

// LogValue renders the request and never a key.
func (req BackupRequest) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("dest", req.Dest),
		slog.String("deployment_id", req.DeploymentID),
		slog.String("github", req.GitHub.String()),
		slog.Int("targets", len(req.Targets)),
		slog.String("key", "[redacted]"),
	)
}

// ExternalLedger describes a ledger the archive deliberately does not contain.
//
// THE CALLER ESTABLISHES ALL THREE, because each is a fact about the deployment
// rather than about the archive: which engine, which environment variable the
// config names for the connection string, and what the live ledger had applied
// when the backup ran.
//
// MIGRATIONS COME FROM THE LIVE DATABASE HERE, which is the one place this
// package reads a schema from something that is still moving. There is no
// snapshot to read back from, and the alternative — recording nothing — would
// leave a restore unable to refuse an archive whose ledger a newer billet had
// migrated past this one. See LedgerFacts for what that costs and why the
// direction is safe.
type ExternalLedger struct {
	// Backend names the engine, e.g. "postgres". Required: an external ledger
	// nothing can name is a claim with no content.
	Backend string
	// DSNEnv is the environment variable holding the connection string. The
	// NAME, never the value.
	DSNEnv string
	// Migrations is what the live ledger had applied when this ran.
	Migrations []state.AppliedMigration
}

// Write captures a deployment into a new archive directory.
//
// IT TAKES THE AUTHORITY LOCK ITSELF. `billet ca rotate` mutates five files in
// sequence, so a backup that read them without the lock could capture a key
// from one generation beside a certificate from another — an archive that loads
// cleanly and verifies nothing, discovered on the day it is restored. The lock
// is taken here rather than by the command because this is the exported entry
// point, and a rule enforced only at the CLI has a second way in that does not
// enforce it.
//
// The ledger snapshot is taken INSIDE the same lock, so the authority in the
// archive and the certificate records in the ledger describe one moment.
func Write(ctx context.Context, req BackupRequest) (Manifest, error) {
	if err := req.validate(); err != nil {
		return Manifest{}, err
	}

	if err := prepareDest(req.Dest, req.StateDir); err != nil {
		return Manifest{}, err
	}

	lock, err := wirecert.LockAuthority(req.StateDir)
	if err != nil {
		return Manifest{}, err
	}

	m, writeErr := writeLocked(ctx, req)

	return m, errors.Join(writeErr, lock.Release())
}

func (req BackupRequest) validate() error {
	switch {
	case req.Dest == "":
		return errors.New("deployarchive: a backup needs a destination directory")
	case req.StateDir == "":
		return errors.New("deployarchive: a backup needs the control plane's state directory")
	case req.DeploymentID == "":
		return errors.New(
			"deployarchive: this state directory has no deployment identity, so there is no " +
				"deployment here to back up. A control plane mints one the first time it starts")
	case len(req.AppKeyPEM) == 0:
		return errors.New(
			"deployarchive: the GitHub App private key is part of the deployment unit and none " +
				"was supplied; GitHub issues it exactly once and will not reissue it")
	case req.Snapshot == nil && req.ExternalLedger == nil:
		return errors.New(
			"deployarchive: a backup needs either a ledger snapshot function or a description " +
				"of the external ledger this deployment uses")
	case req.Snapshot != nil && req.ExternalLedger != nil:
		// BOTH IS THE ONE COMBINATION THAT COULD LIE. It would write a snapshot
		// into the archive AND declare the ledger external, producing a manifest
		// that says the file beside it is not there.
		return errors.New(
			"deployarchive: a backup carries its ledger or declares it external, never both")
	case req.ExternalLedger != nil && !externalBackends[req.ExternalLedger.Backend]:
		// THE CLOSED SET, NOT MERELY NON-EMPTY. An archive naming an engine
		// nothing can restore is one an operator finds out about on the day they
		// need it; refusing at capture is the only moment that costs nothing.
		return fmt.Errorf(
			"deployarchive: an external ledger has to name an engine billet can pair a "+
				"deployment with, and this one names %s",
			orUnnamed(req.ExternalLedger.Backend))
	case req.Now == nil:
		return errors.New("deployarchive: a backup needs a clock")
	}

	seen := make(map[string]bool, len(req.Targets))

	for _, target := range req.Targets {
		switch {
		case target.Name == "" || target.Name == defaultTargetName:
			return fmt.Errorf("deployarchive: a further target needs a name of its own; %q is the "+
				"github block's", target.Name)
		case seen[target.Name]:
			return fmt.Errorf("deployarchive: target %q is listed twice", target.Name)
		case target.GitHub.IsZero():
			return fmt.Errorf("deployarchive: target %q names no App", target.Name)
		case len(target.AppKeyPEM) == 0:
			return fmt.Errorf("deployarchive: target %q's App private key is part of the "+
				"deployment unit and none was supplied; GitHub issues it exactly once", target.Name)
		}

		seen[target.Name] = true
	}

	return nil
}

// defaultTargetName is the name the `github:` block's target carries in
// config, which a further target may not take.
const defaultTargetName = "default"

// PrepareDestination creates a directory an archive may be written into, and
// refuses the places one must not be.
//
// INSIDE THE STATE DIRECTORY IS THE ONE THAT MATTERS. A backup written there
// puts a second copy of the CA key and a second ledger under the path a restore
// scans, a `billet local uninstall` names as preserved, and the CA allowlist
// walks — and the ledger snapshot would then be inside the directory it is a
// snapshot of.
//
// EXPORTED FOR THE FETCH PATH, which puts an archive on disk without a backup
// having written it: an archive downloaded from a bucket is the same two private
// keys under the same rules, and giving it a second, laxer implementation is how
// one of them ends up inside the state directory a restore scans.
func PrepareDestination(dest, stateDir string) error { return prepareDest(dest, stateDir) }

// CheckDestinationPlace refuses a destination for what it IS rather than for
// what is in it: not absolute, inside the state directory, a symlink, or not a
// directory at all.
//
// SPLIT OUT SO ONE CALLER CAN KEEP THE PLACE RULES AND DROP THE EMPTINESS ONE. A
// fetch that is repeating itself — the operator's first attempt was interrupted
// and the archive is KEPT, deliberately — lands on a directory that already
// holds the very archive it is about to fetch, and refusing that is a retry with
// nowhere to go. Nothing here softens: whoever skips the emptiness check has to
// prove the contents are the same archive.
func CheckDestinationPlace(dest, stateDir string) error {
	if !filepath.IsAbs(dest) {
		return fmt.Errorf("deployarchive: the backup destination %s must be an absolute path",
			dest)
	}

	absState, err := filepath.Abs(stateDir)
	if err != nil {
		return fmt.Errorf("deployarchive: resolve %s: %w", stateDir, err)
	}

	clean := filepath.Clean(dest)

	if clean == absState ||
		strings.HasPrefix(clean, absState+string(filepath.Separator)) ||
		strings.HasPrefix(absState, clean+string(filepath.Separator)) {
		return fmt.Errorf(
			"deployarchive: %s is inside the state directory %s (or holds it). A backup written "+
				"there is a second copy of the CA key and the ledger under the path billet "+
				"scans, restores from and reports as preserved — put it somewhere else",
			clean, absState)
	}

	// A SYMLINK IS REFUSED RATHER THAN FOLLOWED. This directory is about to hold
	// a CA private key and an App key, and a link means they land somewhere the
	// operator did not name.
	switch info, err := os.Lstat(clean); {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("deployarchive: %s is a symlink; billet writes a backup only to the "+
			"path it was given", clean)
	case err == nil && !info.IsDir():
		return fmt.Errorf("deployarchive: %s exists and is not a directory", clean)
	case err == nil, errors.Is(err, os.ErrNotExist):
	default:
		return fmt.Errorf("deployarchive: inspect %s: %w", clean, err)
	}

	return nil
}

// prepareDest creates the archive directory, refusing the places one must not be
// and anything already in it.
func prepareDest(dest, stateDir string) error {
	if err := CheckDestinationPlace(dest, stateDir); err != nil {
		return err
	}

	clean := filepath.Clean(dest)

	switch _, err := os.Lstat(clean); {
	case err == nil:
		entries, readErr := os.ReadDir(clean)
		if readErr != nil {
			return fmt.Errorf("deployarchive: read %s: %w", clean, readErr)
		}

		if len(entries) > 0 {
			return fmt.Errorf(
				"deployarchive: %s is not empty. A backup is a whole unit and billet will not "+
					"mix one into a directory that already holds something — use a new directory",
				clean)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(clean, 0o700); err != nil {
			return fmt.Errorf("deployarchive: create %s: %w", clean, err)
		}
	default:
		return fmt.Errorf("deployarchive: inspect %s: %w", clean, err)
	}

	// 0700 EXPLICITLY, whether billet created the directory or the operator did.
	// MkdirAll leaves an existing directory's mode alone, and this one is about
	// to hold two private keys.
	if err := os.Chmod(clean, 0o700); err != nil {
		return fmt.Errorf("deployarchive: tighten %s: %w", clean, err)
	}

	return nil
}

// writeLocked is the body of Write, with the authority lock held.
func writeLocked(ctx context.Context, req BackupRequest) (Manifest, error) {
	authority, err := wirecert.ReadAuthority(req.StateDir)
	if err != nil {
		return Manifest{}, err
	}

	facts, err := authorityFacts(authority)
	if err != nil {
		return Manifest{}, err
	}

	// THE AUTHORITY MUST BE THIS DEPLOYMENT'S. An archive pairing one
	// installation's identity with another's CA restores a control plane whose
	// own certificate does not name it, and every node that connects is a node
	// the other installation admitted.
	cert, err := wirecert.ParseAuthorityPair(authority.Present["ca.key"], authority.Present["ca.crt"])
	if err != nil {
		return Manifest{}, err
	}

	named, err := wirecert.AuthorityDeployment(cert)
	if err != nil {
		return Manifest{}, err
	}

	if named != req.DeploymentID {
		return Manifest{}, fmt.Errorf(
			"%s says this installation is deployment %s, and the authority in %s was issued for "+
				"%s. Backing both up as one unit would record a deployment that does not exist; "+
				"resolve which is current before capturing it",
			filepath.Join(req.StateDir, "deployment-id"), req.DeploymentID,
			wirecert.CADir(req.StateDir), named)
	}

	files := []entry{
		{name: EntryIdentity, body: []byte(req.DeploymentID + "\n")},
		{name: EntryAppKey, body: req.AppKeyPEM},
	}

	targets := make([]TargetIdentity, 0, len(req.Targets))

	for _, target := range req.Targets {
		files = append(files, entry{name: EntryAppKeyFor(target.Name), body: target.AppKeyPEM})
		targets = append(targets, TargetIdentity{Name: target.Name, GitHubIdentity: target.GitHub})
	}

	for name, body := range authority.Present {
		files = append(files, entry{name: AuthorityEntry(name), body: body})
	}

	if len(req.ConfigBody) > 0 {
		files = append(files, entry{name: EntryConfig, body: req.ConfigBody})
	}

	records := make([]FileRecord, 0, len(files)+1)

	for _, f := range files {
		path, err := entryPath(req.Dest, f.name)
		if err != nil {
			return Manifest{}, err
		}

		// EVERYTHING 0600. Which of these is a secret varies — the CA
		// certificate is not, the CA key is — and giving them one mode removes a
		// class of mistake rather than teaching a distinction the archive has no
		// use for.
		if err := writeSmall(path, f.body); err != nil {
			return Manifest{}, err
		}

		records = append(records, FileRecord{
			Path:   f.name,
			SHA256: digest(f.body),
			Size:   int64(len(f.body)),
		})
	}

	ledger := LedgerFacts{}

	if req.ExternalLedger != nil {
		// NO ledger/ DIRECTORY IS CREATED AT ALL, not even an empty one. Open
		// refuses an external archive that has anything there, so leaving the
		// directory behind would make every such backup unreadable by the
		// package that wrote it.
		ledger = LedgerFacts{
			External:   true,
			Backend:    req.ExternalLedger.Backend,
			DSNEnv:     req.ExternalLedger.DSNEnv,
			Migrations: req.ExternalLedger.Migrations,
		}
	} else {
		ledgerPath, err := entryPath(req.Dest, EntryLedger)
		if err != nil {
			return Manifest{}, err
		}

		if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o700); err != nil {
			return Manifest{}, fmt.Errorf("deployarchive: create %s: %w",
				filepath.Dir(ledgerPath), err)
		}

		if err := req.Snapshot(ctx, ledgerPath); err != nil {
			return Manifest{}, err
		}

		ledgerDigest, ledgerSize, err := digestFile(ledgerPath)
		if err != nil {
			return Manifest{}, err
		}

		records = append(records, FileRecord{
			Path: EntryLedger, SHA256: ledgerDigest, Size: ledgerSize,
		})

		// FROM THE COMPLETED SNAPSHOT. Reading the schema from the live database
		// beside it would let a control plane restarted onto a newer binary
		// migrate in between, and the manifest would describe a schema the
		// archive does not carry.
		applied, err := state.PeekMigrations(ctx, ledgerPath)
		if err != nil {
			return Manifest{}, err
		}

		ledger.Migrations = applied
	}

	m := Manifest{
		Schema:        Schema,
		Kind:          Kind,
		CreatedAt:     req.Now().UTC().Format(time.RFC3339),
		BilletVersion: version.Version(),
		DeploymentID:  req.DeploymentID,
		Source: Source{
			Host:       req.Hostname,
			ConfigPath: req.ConfigPath,
			StateDir:   req.StateDir,
		},
		GitHub:    req.GitHub,
		Targets:   targets,
		Authority: facts,
		Ledger:    ledger,
		Files:     records,
	}

	body, err := m.encode()
	if err != nil {
		return Manifest{}, err
	}

	manifestPath, err := entryPath(req.Dest, EntryManifest)
	if err != nil {
		return Manifest{}, err
	}

	// THE MANIFEST IS WRITTEN LAST, so a run that dies partway leaves a
	// directory that is visibly NOT an archive rather than one claiming to
	// describe files it never wrote.
	if err := writeSmall(manifestPath, body); err != nil {
		return Manifest{}, err
	}

	if err := syncArchiveDirs(req.Dest, records); err != nil {
		return Manifest{}, err
	}

	return m, nil
}

type entry struct {
	name string
	body []byte
}

// authorityFacts summarises the captured authority for the manifest.
func authorityFacts(a wirecert.Authority) (AuthorityFacts, error) {
	cert, err := wirecert.ParseAuthorityPair(a.Present["ca.key"], a.Present["ca.crt"])
	if err != nil {
		return AuthorityFacts{}, err
	}

	out := AuthorityFacts{
		Fingerprint:            wirecert.FingerprintOfCert(cert),
		NotAfter:               cert.NotAfter.UTC().Format(time.RFC3339),
		Rotating:               a.Rotating(),
		UnexpectedFilesPresent: a.Unexpected,
	}

	if out.Rotating {
		prev, err := wirecert.ParseAuthorityPair(
			a.Present["ca-previous.key"], a.Present["ca-previous.crt"])
		if err != nil {
			return AuthorityFacts{}, fmt.Errorf("the previous authority does not hold together: %w", err)
		}

		out.PreviousFingerprint = wirecert.FingerprintOfCert(prev)
		out.PreviousNotAfter = prev.NotAfter.UTC().Format(time.RFC3339)
	}

	return out, nil
}

// syncArchiveDirs persists every directory entry the archive created.
func syncArchiveDirs(dest string, records []FileRecord) error {
	seen := map[string]bool{dest: true}

	for _, r := range records {
		path, err := entryPath(dest, r.Path)
		if err != nil {
			return err
		}

		seen[filepath.Dir(path)] = true
	}

	for dir := range seen {
		if err := syncDir(dir); err != nil {
			return err
		}
	}

	return nil
}
