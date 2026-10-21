// Package unitpath is how a command says which host paths it needs, so a
// service manager's sandbox can be proved to permit them (#174).
//
// A UNIT'S SANDBOX IS JUDGED BY WHAT ITS COMMAND DOES, NEVER BY ITS TEXT.
// billet-images-refresh.service failed twice on a real host past every test that
// asserted its directives: once because it required a path under its own
// PrivateTmp, so systemd refused to start it, and once because ProtectSystem=
// strict left the lock it took under /run unwritable, after a five-gigabyte
// download. So each command family declares the paths its command touches with
// the same builders the command uses, `billet local unit-paths` reports them,
// and the same command, as a unit's stub ExecStart, performs each access inside
// the unit's real sandbox.
//
// A LEAF: the standard library and the one open of a file by pathname.
package unitpath

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/junioryono/billet/internal/regularfile"
)

// Access is what a command does at a path.
type Access string

const (
	// Read is a file the command opens for reading.
	Read Access = "read"
	// Write is a directory the command creates entries in.
	Write Access = "write"
	// Private is a directory in the unit's own private /tmp or /var/tmp
	// (PrivateTmp=true): the command creates it and writes there, and nothing it
	// writes may reach the host's.
	Private Access = "private"
	// Lock is a lock the command takes, which Take performs as the command does.
	Lock Access = "lock"
)

// Need is one path a command needs, and how. A command's needs are exercised in
// the order it declares them, which is the order it touches them.
type Need struct {
	Path   string `json:"path"`
	Access Access `json:"access"`
	// Why says what the command does there, in an operator's words.
	Why string `json:"why"`
	// Unknown says the path could not be derived on this host, and why: a lock
	// named for a deployment identity the host has not minted yet. Path is then
	// the directory the name would be in.
	Unknown string `json:"unknown,omitempty"`
	// Create says the command creates a Write directory when it is absent. A
	// directory it does not create (a state directory systemd makes, or one an
	// authority exclusion forbids recreating) must already be there, and an
	// exercise never makes one.
	Create bool `json:"create,omitempty"`
	// Admits says the needs after this one are what the command does only once
	// this one is held: an authority exclusion before the directory it guards.
	// When it is refused, nothing after it is tried.
	Admits bool `json:"admits,omitempty"`
	// Take performs a Lock's acquisition the way the command does. It answers
	// the lock's release, called once every need after it has been exercised,
	// so a lock the command holds across its work is held across the exercise of
	// that work; or nil, with nothing held, when another process holds the lock:
	// the path was reached and the lock is real, which is what a sandbox is asked.
	Take func(ctx context.Context) (release func() error, err error) `json:"-"`
}

// ErrUnknown is an exercise of a need whose path could not be derived.
var ErrUnknown = errors.New("the path could not be derived on this host")

// ErrNotAdmitted is a need not tried because a need it depends on was refused.
var ErrNotAdmitted = errors.New("not tried: the exclusion before it was refused")

// Unique is needs with every repeated path and access dropped, the first kept:
// a directory two declarations name (a certificate kept in the state directory)
// is one directory, written once.
func Unique(needs []Need) []Need {
	out := make([]Need, 0, len(needs))

	for _, n := range needs {
		if !slices.ContainsFunc(out, func(o Need) bool { return o.Path == n.Path && o.Access == n.Access }) {
			out = append(out, n)
		}
	}

	return out
}

// ExerciseAll performs every need in order, reports each to report, releases
// whatever was held in reverse order, and answers every refusal. A refused need
// that Admits the rest leaves them untried.
func ExerciseAll(ctx context.Context, needs []Need, marker string, report func(Need, error)) error {
	var (
		releases []func() error
		refused  []error
		blocked  bool
	)

	for _, n := range needs {
		if blocked {
			report(n, ErrNotAdmitted)
			refused = append(refused, fmt.Errorf("%s: %w", n.Path, ErrNotAdmitted))

			continue
		}

		release, err := Exercise(ctx, n, marker)
		report(n, err)

		if err != nil {
			refused = append(refused, err)
			blocked = n.Admits

			continue
		}

		if release != nil {
			releases = append(releases, release)
		}
	}

	for i := len(releases) - 1; i >= 0; i-- {
		if err := releases[i](); err != nil {
			refused = append(refused, fmt.Errorf("release: %w", err))
		}
	}

	return errors.Join(refused...)
}

// Exercise performs a need's access and answers what it holds. A writable
// directory is given a file named marker, left there so a caller outside the
// sandbox can see where the write landed; with no marker the file is created
// and removed. A directory the command creates is created first; one it does
// not must already exist. A lock the report could not name is still taken: its
// Take resolves the name the way the command does, minting what the command
// would mint, which is what a fresh host's first start does too.
func Exercise(ctx context.Context, n Need, marker string) (func() error, error) {
	if n.Unknown != "" && (n.Access != Lock || n.Take == nil) {
		return nil, fmt.Errorf("%s: %w: %s", n.Path, ErrUnknown, n.Unknown)
	}

	switch n.Access {
	case Read:
		f, _, err := regularfile.Open(n.Path, regularfile.Options{})
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", n.Path, err)
		}

		return nil, f.Close()

	case Write, Private:
		if n.Access == Private || n.Create {
			if err := os.MkdirAll(n.Path, 0o755); err != nil {
				return nil, fmt.Errorf("create %s: %w", n.Path, err)
			}
		}

		return nil, writeMarker(n.Path, marker)

	case Lock:
		if n.Take == nil {
			return nil, fmt.Errorf("lock %s: the command declares no way to take it", n.Path)
		}

		release, err := n.Take(ctx)
		if err != nil {
			return nil, fmt.Errorf("lock %s: %w", n.Path, err)
		}

		return release, nil

	default:
		return nil, fmt.Errorf("%s: unknown access %q", n.Path, n.Access)
	}
}

// MarkerPrefix begins every marker's name, so a marker can never be the name of
// a file a command keeps in the same directory (a ledger, a lock, an identity).
const MarkerPrefix = "billet-unit-paths-"

// ValidMarker reports whether name is one plain file name with MarkerPrefix.
func ValidMarker(name string) bool {
	return strings.HasPrefix(name, MarkerPrefix) && len(name) > len(MarkerPrefix) &&
		name == filepath.Base(name) && !strings.ContainsAny(name, "/\x00")
}

// writeMarker creates a file in dir, synced, and removes it unless it was named.
//
// IT NEVER WRITES OVER ANYTHING: a named marker is created exclusively and not
// through a link at its name, and an unnamed one has a name nobody else has, so
// the exercise cannot truncate a file a command keeps where it writes.
func writeMarker(dir, marker string) error {
	var (
		f   *os.File
		err error
	)

	if marker == "" {
		f, err = os.CreateTemp(dir, "."+MarkerPrefix+"*")
	} else if !ValidMarker(marker) {
		return fmt.Errorf("a marker is one plain file name beginning %s, not %q", MarkerPrefix, marker)
	} else {
		f, err = os.OpenFile(filepath.Join(dir, marker), os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	}

	if err != nil {
		return fmt.Errorf("write in %s: %w", dir, err)
	}

	path := f.Name()

	if _, err := f.WriteString("written by billet local unit-paths --exercise\n"); err != nil {
		_ = f.Close()

		return fmt.Errorf("write in %s: %w", dir, err)
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()

		return fmt.Errorf("flush %s: %w", path, err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}

	if marker == "" {
		return os.Remove(path)
	}

	return nil
}
