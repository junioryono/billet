// Package regularfile opens a file named by a pathname only when it is a regular
// file on a filesystem that stores bytes, without waiting on it and without
// opening a device.
//
// ONE OPEN, IN ONE PLACE. A plain open of a FIFO blocks until somebody writes to
// it, before any stat a caller could make, and no timeout in billet bounds that
// wait; a plain open of a device is an operation on the device (a watchdog is
// armed by its open); a plain read of a regular-mode file on procfs can wait for
// the kernel's next message. Every file billet reads by a pathname it did not
// create this instant, a configuration, a lock file, an environment file systemd
// names, a certificate, a record of its own under a root-owned directory, is a
// name a host could put something else at, and a check that hangs or arms a
// device before it can refuse is neither yes, no nor could-not-tell. On Linux the
// name is opened for its identity alone (O_PATH), the descriptor is fstat'ed and
// its filesystem asked for, and only a regular file on an ordinary filesystem is
// reopened for reading, non-blocking, through /proc/self/fd on the inode already
// held, so nothing is read or opened before the refusal and no pathname is
// resolved a second time. Elsewhere there is no identity-only open: the file is
// opened read-only and non-blocking, which returns at once on a FIFO, and the
// descriptor is fstat'ed before anything is read; a device there IS opened and
// then refused, which the caller's platform decides is acceptable.
//
// FSTAT ON THE DESCRIPTOR, NOT STAT ON THE PATH, so what is refused and what is
// read are the same object. A stat of the name answers about whatever the name
// meant at that instant, and a replacement between the stat and the open is read
// for the original.
//
// WHAT THIS DOES NOT BOUND: a read from ordinary storage that stalls (a network
// filesystem, a failing disk) is not refused here; that residual is the
// filesystem's and is said to be.
package regularfile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// ErrNotRegular is the refusal of anything but a regular file. It is wrapped in
// an *os.PathError naming the path, beside the mode that was found.
var ErrNotRegular = errors.New("not a regular file")

// ErrUnsupportedFilesystem is the refusal of a regular-mode file on a kernel
// pseudo-filesystem (procfs, sysfs and their kind), whose reads are not reads of
// stored bytes and can wait.
var ErrUnsupportedFilesystem = errors.New("not on a filesystem that stores bytes")

// ErrReopen is the failure to turn a held identity descriptor into a readable
// one. IT NEVER WRAPS THE CAUSE'S ERRNO: a reopen that failed for want of
// /proc/self/fd is not the file's absence, and a caller that recognises
// fs.ErrNotExist as "absent" must never see it here.
var ErrReopen = errors.New("could not reopen the held file for reading")

// ErrTooLarge is ReadFile's refusal of a file longer than its limit; the limit is
// a statement about what billet wrote, and a longer file is not that.
var ErrTooLarge = errors.New("larger than the limit")

// Options shape one open.
type Options struct {
	// NoFollow refuses a symlink at the last component, for a file whose given
	// path is the only one that should be read (a lock, a key, a certificate
	// billet is about to trust).
	NoFollow bool
}

// reopenFailure, when set, fails every reopen with the error it returns: a test
// seam for the case a platform cannot produce on demand (procfs missing, a dup
// refused), so the rule that a reopen failure is never absence is proved.
var reopenFailure func() error

// Open opens path for reading only when it is a regular file on an ordinary
// filesystem, and returns the readable descriptor with the fstat that admitted
// it. A missing file is an *os.PathError wrapping fs.ErrNotExist, as os.Open
// returns; anything that is not a regular file is an *os.PathError wrapping
// ErrNotRegular; a pseudo-filesystem is one wrapping ErrUnsupportedFilesystem; a
// reopen that failed is one wrapping ErrReopen and nothing a caller reads as
// absence.
func Open(path string, opts Options) (*os.File, os.FileInfo, error) {
	id, err := openForIdentity(path, opts)
	if err != nil {
		return nil, nil, err
	}
	f, info, err := reopen(id)
	_ = id.Close()
	if err != nil {
		return nil, nil, err
	}
	return f, info, nil
}

// Reopen turns a descriptor a caller already holds for identity (an O_PATH
// descriptor from openat2, a non-blocking descriptor elsewhere) into a readable
// one on the same file, applying the regular-file and filesystem rules first.
func Reopen(f *os.File) (*os.File, error) {
	r, _, err := reopen(f)
	return r, err
}

// ReadFile reads the whole of a regular file of at most limit bytes through
// Open. A longer file is refused with ErrTooLarge, never truncated, because a
// caller's limit says what a file billet wrote can be, and the hash or the parse
// of a prefix would describe something that was never the file.
func ReadFile(path string, limit int64, opts Options) ([]byte, error) {
	f, _, err := Open(path, opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return ReadAllLimited(f, path, limit)
}

// ReadAllLimited reads the rest of an already admitted descriptor under the same
// rule as ReadFile: at most limit bytes, a longer file refused rather than cut.
func ReadAllLimited(f *os.File, path string, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, &os.PathError{Op: "read", Path: path, Err: err}
	}
	if int64(len(body)) > limit {
		return nil, &os.PathError{Op: "read", Path: path, Err: fmt.Errorf("%w of %d bytes", ErrTooLarge, limit)}
	}
	return body, nil
}

// requireRegular is the rule both platforms apply to the descriptor they hold.
func requireRegular(f *os.File) (os.FileInfo, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, &os.PathError{Op: "open", Path: f.Name(), Err: fmt.Errorf("%w (%s)", ErrNotRegular, describe(info.Mode()))}
	}
	return info, nil
}

// reopenError is the one shape a failed reopen has: the cause is quoted, never
// wrapped, so its errno cannot be read as the file's absence.
func reopenError(f *os.File, cause error) error {
	return &os.PathError{Op: "reopen", Path: f.Name(), Err: fmt.Errorf("%w: %v", ErrReopen, cause)} //nolint:errorlint // the cause is QUOTED, not wrapped: an ENOENT from a missing /proc/self/fd must not read as the file's absence
}

func describe(mode fs.FileMode) string {
	switch {
	case mode.IsDir():
		return "a directory"
	case mode&fs.ModeNamedPipe != 0:
		return "a FIFO"
	case mode&fs.ModeSocket != 0:
		return "a socket"
	case mode&fs.ModeDevice != 0 && mode&fs.ModeCharDevice != 0:
		return "a character device"
	case mode&fs.ModeDevice != 0:
		return "a block device"
	case mode&fs.ModeSymlink != 0:
		return "a symlink"
	default:
		return mode.Type().String()
	}
}
