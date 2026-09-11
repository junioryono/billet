package retirement

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// HostMode is what a caller about to touch an identity directory has found the
// host to be, from four observations made in this order: the service-account
// record, the global lock file, the status file, and the configured directory.
type HostMode int

const (
	// ModePrepared: an installer has recorded the service account, so the global
	// exclusion is in force and every writer acquires and admits through it.
	ModePrepared HostMode = iota
	// ModeLegacy: no record, positively no global lock, no status, and the
	// configured directory exists. The pre-change world in what it locks: the
	// directory's own `ca.lock` is the whole exclusion, taken by every privileged
	// entrypoint, and a retirement cannot be requested here.
	ModeLegacy
	// ModeFresh: no record, no global lock, no status, and the configured
	// directory is positively absent. A fresh initialisation creates it under
	// the init lock beside it.
	ModeFresh
)

func (m HostMode) String() string {
	switch m {
	case ModePrepared:
		return "prepared"
	case ModeLegacy:
		return "legacy"
	case ModeFresh:
		return "fresh"
	default:
		return fmt.Sprintf("HostMode(%d)", int(m))
	}
}

// Classification is the mode with the observations it rests on.
type Classification struct {
	Mode    HostMode
	Account ServiceAccount
}

// DamagedError is a host whose metadata contradicts itself: a global lock or a
// status beside a missing or unreadable record. An established exclusion is
// never downgraded to the legacy path, so every ordinary caller refuses on it,
// and only a privileged Bootstrap may restore the record.
type DamagedError struct{ Why string }

func (e DamagedError) Error() string {
	return "retirement: this host's authority metadata is damaged, and billet will not guess (" + e.Why +
		"); run `billet local up` or the host role as root to restore the record"
}

// Classify observes the host for identityDir. A failed observation is never
// read as absence: an unreadable record, lock or status is a DamagedError.
func Classify(identityDir string) (Classification, error) {
	acct, err := ReadServiceAccount()

	switch {
	case err == nil:
		return Classification{Mode: ModePrepared, Account: acct}, nil
	case !errors.Is(err, ErrNoServiceAccount):
		return Classification{}, DamagedError{Why: err.Error()}
	}

	present, err := exists(GlobalLockPath())
	if err != nil {
		return Classification{}, DamagedError{Why: err.Error()}
	}

	if present {
		return Classification{}, DamagedError{Why: fmt.Sprintf(
			"the global authority lock %s exists but no service account is recorded at %s",
			GlobalLockPath(), ServiceAccountPath())}
	}

	if _, presence, err := ReadStatus(); presence != StatusAbsent {
		why := fmt.Sprintf("an authority status exists at %s but no service account is recorded", StatusPath())
		if err != nil {
			why = err.Error()
		}

		return Classification{}, DamagedError{Why: why}
	}

	present, err = exists(identityDir)
	if err != nil {
		return Classification{}, fmt.Errorf("retirement: examine %s: %w", identityDir, err)
	}

	if present {
		return Classification{Mode: ModeLegacy}, nil
	}

	return Classification{Mode: ModeFresh}, nil
}

// GlobalLockPresent is the three-valued presence of the global lock file, for
// a legacy caller's recheck under its inner lock.
func GlobalLockPresent() (bool, error) { return exists(GlobalLockPath()) }

// exists is a three-valued stat: present, positively absent, or an error that
// is neither.
func exists(path string) (bool, error) {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}

		return false, err
	}

	return true, nil
}
