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
	// (PrivateTmp=true): the command writes there, and nothing it writes may
	// reach the host's.
	Private Access = "private"
	// Lock is a lock the command takes, which Take performs as the command does.
	Lock Access = "lock"
)

// Need is one path a command needs, and how.
type Need struct {
	Path   string `json:"path"`
	Access Access `json:"access"`
	// Why says what the command does there, in an operator's words.
	Why string `json:"why"`
	// Unknown says the path could not be derived on this host, and why: a lock
	// named for a deployment identity the host has not minted yet. Path is then
	// the directory the name would be in.
	Unknown string `json:"unknown,omitempty"`
	// Take performs a Lock's acquisition and release the way the command does,
	// answering nil when the lock is held by another process: the path was
	// reached and the lock is real, which is what a sandbox is asked.
	Take func(ctx context.Context) error `json:"-"`
}

// ErrUnknown is an exercise of a need whose path could not be derived.
var ErrUnknown = errors.New("the path could not be derived on this host")

// Exercise performs a need's access. A writable directory is given a file named
// marker, left there so a caller outside the sandbox can see where the write
// landed; with no marker the file is created and removed. A directory a command
// creates (a private one, or a write one that is absent) is created first, as
// the command creates it. A lock the report could not name is still taken: its
// Take resolves the name the way the command does, minting what the command
// would mint, which is what a fresh host's first start does too.
func Exercise(ctx context.Context, n Need, marker string) error {
	if n.Unknown != "" && (n.Access != Lock || n.Take == nil) {
		return fmt.Errorf("%s: %w: %s", n.Path, ErrUnknown, n.Unknown)
	}

	switch n.Access {
	case Read:
		f, _, err := regularfile.Open(n.Path, regularfile.Options{})
		if err != nil {
			return fmt.Errorf("read %s: %w", n.Path, err)
		}

		return f.Close()

	case Write, Private:
		if err := os.MkdirAll(n.Path, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", n.Path, err)
		}

		return writeMarker(n.Path, marker)

	case Lock:
		if n.Take == nil {
			return fmt.Errorf("lock %s: the command declares no way to take it", n.Path)
		}

		if err := n.Take(ctx); err != nil {
			return fmt.Errorf("lock %s: %w", n.Path, err)
		}

		return nil

	default:
		return fmt.Errorf("%s: unknown access %q", n.Path, n.Access)
	}
}

// writeMarker creates a file in dir, synced, and removes it unless it was named.
func writeMarker(dir, marker string) error {
	name := marker
	if name == "" {
		name = fmt.Sprintf(".billet-unit-paths-%d", os.Getpid())
	}

	path := filepath.Join(dir, name)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("write in %s: %w", dir, err)
	}

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
