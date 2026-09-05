package node

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/junioryono/billet/internal/provider"
)

const cacheSessionDirectory = "cache-sessions"

// errSessionFinished is what a write to a session's record gets once cleanup
// has removed it: the compute is gone, and a record written now would load on
// the next start as a session for compute that does not exist.
var errSessionFinished = errors.New("node: the cache session has finished and writes no record")

// validateSessionLease checks the lease a session claims to run under.
//
// THE LEASE MUST NAME THIS INSTANCE. Instances are named after their lease
// (provider.InstanceName), so a record whose lease does not produce its own
// instance name attributes what this guest saw to some other job; a record
// from before the session carried a lease names none and carries epoch zero.
// A fresh lease's epoch IS zero, so the epoch is checked for sign, not for
// being positive.
func validateSessionLease(instance, leaseID string, epoch int64) error {
	if leaseID == "" {
		if epoch != 0 {
			return fmt.Errorf("node: cache session for %s carries epoch %d with no lease", instance, epoch)
		}

		return nil
	}

	if provider.InstanceName(leaseID) != instance {
		return fmt.Errorf("node: cache session for %s claims lease %q, which names a different instance",
			instance, leaseID)
	}

	if epoch < 0 {
		return fmt.Errorf("node: cache session for %s carries a negative epoch %d", instance, epoch)
	}

	return nil
}

type durableCacheSession struct {
	Token       string                                `json:"token"`
	Instance    string                                `json:"instance"`
	Trust       provider.TrustClass                   `json:"trust"`
	Owner       string                                `json:"owner,omitempty"`
	Repository  string                                `json:"repository,omitempty"`
	WorkflowRef string                                `json:"workflow_ref,omitempty"`
	Intercept   bool                                  `json:"intercept,omitempty"`
	LeaseID     string                                `json:"lease_id,omitempty"`
	Epoch       int64                                 `json:"epoch,omitempty"`
	Observed    cacheObserved                         `json:"observed"`
	Closed      bool                                  `json:"closed"`
	Slots       [provider.MaxVolumes]*cacheAttachment `json:"slots"`
	Actions     map[string]*actionsArchive            `json:"actions,omitempty"`
	Receipts    map[string]*actionsReceipt            `json:"actions_receipts,omitempty"`
}

func (s *CacheService) loadSessions() error {
	if err := os.MkdirAll(s.stateDir, 0o700); err != nil {
		return fmt.Errorf("node: create cache custody directory: %w", err)
	}

	entries, err := os.ReadDir(s.stateDir)
	if err != nil {
		return fmt.Errorf("node: read cache custody directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		body, err := os.ReadFile(filepath.Join(s.stateDir, entry.Name()))
		if err != nil {
			return fmt.Errorf("node: read cache custody %s: %w", entry.Name(), err)
		}
		var record durableCacheSession
		if err := json.Unmarshal(body, &record); err != nil {
			return fmt.Errorf("node: cache custody %s is not valid json: %w", entry.Name(), err)
		}
		if err := record.valid(entry.Name()); err != nil {
			return err
		}
		if s.tokenExists(record.Token) {
			return fmt.Errorf("node: duplicate cache custody capability in %s", entry.Name())
		}
		if _, duplicate := s.byInstance[record.Instance]; duplicate {
			return fmt.Errorf("node: duplicate cache custody for instance %s", record.Instance)
		}

		session := &cacheSession{
			token: record.Token, instance: record.Instance, trust: record.Trust,
			owner: record.Owner, repository: record.Repository, workflowRef: record.WorkflowRef,
			intercept: record.Intercept,
			leaseID:   record.LeaseID, epoch: record.Epoch,
			observed:  record.Observed,
			recovered: true,
			closed:    record.Closed, slots: record.Slots, admit: make(chan struct{}, 1),
			actions:  record.Actions,
			receipts: record.Receipts,
		}
		if session.actions == nil {
			session.actions = make(map[string]*actionsArchive)
		}
		if session.receipts == nil {
			session.receipts = make(map[string]*actionsReceipt)
		}
		s.byToken[record.Token] = session
		s.byInstance[record.Instance] = record.Token
	}

	return nil
}

func (r durableCacheSession) valid(filename string) error {
	raw, err := hex.DecodeString(r.Token)
	if err != nil || len(raw) != 32 || filename != r.Token+".json" {
		return fmt.Errorf("node: cache custody file %s has an invalid token identity", filename)
	}
	if strings.TrimSpace(r.Instance) == "" {
		return fmt.Errorf("node: cache custody file %s names no instance", filename)
	}
	if r.Trust != provider.TrustTrusted && r.Trust != provider.TrustUntrusted {
		return fmt.Errorf("node: cache custody file %s has unknown trust %q", filename, r.Trust)
	}
	if err := validateSessionLease(r.Instance, r.LeaseID, r.Epoch); err != nil {
		return fmt.Errorf("node: cache custody file %s: %w", filename, err)
	}
	if r.Intercept {
		if err := validateActionsScope(CacheSessionScope{
			Trust: r.Trust, Intercept: true, Owner: r.Owner, Repository: r.Repository,
			WorkflowRef: r.WorkflowRef,
		}); err != nil {
			return fmt.Errorf("node: cache custody file %s has invalid interception scope: %w", filename, err)
		}
	}
	for id, archive := range r.Actions {
		if archive == nil || archive.ID != id || archive.valid() != nil {
			return fmt.Errorf("node: cache custody file %s has an invalid Actions archive %q",
				filename, id)
		}
	}
	for id, receipt := range r.Receipts {
		if receipt == nil || receipt.ID != id || receipt.valid() != nil {
			return fmt.Errorf("node: cache custody file %s has an invalid Actions receipt %q",
				filename, id)
		}
	}

	return nil
}

// persistSession is called while session.mu is held.
//
// A FINISHED SESSION WRITES NOTHING, whoever asks. Cleanup removes the record
// and the indexes together, and a handler that was still running -- a late
// finalize against a receipt cleanup does not clear -- would otherwise put the
// record back as an orphan. Refusing here, at the one place every write goes
// through, is what makes that impossible rather than merely guarded.
func (s *CacheService) persistSession(session *cacheSession) error {
	if session.finished {
		return errSessionFinished
	}

	record := durableCacheSession{
		Token: session.token, Instance: session.instance, Trust: session.trust,
		Owner: session.owner, Repository: session.repository, WorkflowRef: session.workflowRef,
		Intercept: session.intercept,
		LeaseID:   session.leaseID, Epoch: session.epoch,
		Observed: session.observed,
		Closed:   session.closed, Slots: session.slots, Actions: session.actions,
		Receipts: session.receipts,
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("node: encode cache custody for %s: %w", session.instance, err)
	}

	temporary, err := os.CreateTemp(s.stateDir, ".session-")
	if err != nil {
		return fmt.Errorf("node: stage cache custody for %s: %w", session.instance, err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	workErr := errors.Join(temporary.Chmod(0o600), writeAndSync(temporary, encoded), temporary.Close())
	if workErr != nil {
		return fmt.Errorf("node: write cache custody for %s: %w", session.instance, workErr)
	}

	target := filepath.Join(s.stateDir, session.token+".json")
	if err := os.Rename(temporaryPath, target); err != nil {
		return fmt.Errorf("node: install cache custody for %s: %w", session.instance, err)
	}

	return syncDirectory(s.stateDir)
}

func writeAndSync(file *os.File, body []byte) error {
	if _, err := file.Write(body); err != nil {
		return err
	}

	return file.Sync()
}

func (s *CacheService) removeSession(session *cacheSession) error {
	path := filepath.Join(s.stateDir, session.token+".json")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("node: remove cache custody for %s: %w", session.instance, err)
	}

	return syncDirectory(s.stateDir)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("node: open cache custody directory: %w", err)
	}
	defer directory.Close()

	if err := directory.Sync(); err != nil {
		return fmt.Errorf("node: sync cache custody directory: %w", err)
	}

	return nil
}
