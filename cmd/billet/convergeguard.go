package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/regularfile"
)

// `billet converge-guard` holds the upgrade root's ONE CLAIM for a converge.
//
// THE CLAIM IS ONE NAME, `<upgradeRoot>/active`, AND ITS SHAPE SAYS WHAT HOLDS
// IT: a symlink is a Go transaction (`billet host-upgrade`), a regular file a
// pre-R role transaction's pointer, and a DIRECTORY is a converge guard, chosen
// because every pre-R reader refuses a directory and none can release it. A
// guard is the directory holding `guard.json`; a directory without one is a
// hold that never returned from its publication, which `recover --unpublished`
// removes when it holds nothing but the temporary the publication writes. The
// role's own binary transaction publishes a pointer `active/recovery` inside a
// held guard after the hold, and a guard is not released while that pointer
// exists, dangling or not.
//
// EVERY MUTATOR TAKES THE TRANSACTION LOCK FIRST and holds it until it
// returns, so a hold, a release, a takeover and a Go transaction exclude each
// other through the one flock the kernel drops with its holder. `status` and
// `holder` take nothing and create nothing.
//
// PUBLICATION IS ONE ORDER, UNDER THE LOCK: `active` absent, `mkdir`, write
// `guard.json.tmp`, fsync it, rename it to `guard.json`, fsync `active`, fsync
// the root (the root's parent was flushed by the acquisition that created the
// root, before anything was written under it). Release is the reverse: unlink `guard.json`, fsync `active`, `rmdir`, fsync
// the root. Every interruption leaves a state a reader classifies, and the
// durability table in convergeguard_test.go names each with its answer.
//
// EVERY NAME A MUTATOR TOUCHES UNDER THE ROOT IS RESOLVED RELATIVE TO THE
// DESCRIPTOR OF THE ROOT THE LOCK VALIDATED (`openat`, `mkdirat`, `renameat`,
// `unlinkat`, `fstatat`, `readlinkat`, each O_NOFOLLOW), so a root or a guard
// directory replaced at its name after the trust boundary was proved is not
// the thing used; the lock-free readers open the root by name, without
// following a link, and run the same classifier on that descriptor.
//
// THE RECORDED EXECUTABLE IS ONE OF TWO KNOWN PATHS and never run by anything
// here: the candidate in a recovery directory under the root when the role is
// staging one (`--candidate`, validated through descriptors and digested by
// this command, never a digest the caller supplies), else the managed binary.
// Nothing expires a guard.

const (
	guardRecordName   = "guard.json"
	guardTmpName      = "guard.json.tmp"
	guardPointerName  = "recovery"
	recoveryDirPrefix = "recovery-"
	// maxGuardRecordBytes bounds a record read: a record is five short strings.
	maxGuardRecordBytes = 64 << 10
	// maxHolderBytes bounds a holder name, which a workflow run id or a person's
	// handle fits in many times over.
	maxHolderBytes = 200
)

// guardRecord is `guard.json`.
type guardRecord struct {
	Holder                  string `json:"holder"`
	ClaimedAt               string `json:"claimed_at"`
	Hostname                string `json:"hostname"`
	ReleaseExecutable       string `json:"release_executable"`
	ReleaseExecutableSHA256 string `json:"release_executable_sha256"`
}

// The guard's errors, each its own value because a caller decides on them.
var (
	// errGuardHeld means another holder's guard is on this host.
	errGuardHeld = errors.New("a converge guard is held on this host")
	// errNoGuard means no guard is held, which a release or a recover must say
	// out loud: a script that lost track of its guard must not read "nothing to
	// do" as success.
	errNoGuard = errors.New("no converge guard is held on this host")
	// errGuardUnpublished means `active` is a directory holding no record.
	errGuardUnpublished = errors.New("an unpublished converge guard is on this host")
	// errGuardPointer means the role's transaction pointer is inside the guard.
	errGuardPointer = errors.New("the guard carries a recovery pointer")
	// errTrustBoundary means the upgrade root is not the shape only root (or the
	// launch agent's account) could have made.
	errTrustBoundary = errors.New("the upgrade root is not trusted")
	// errScanRefused means the process scan found a driver or could not look.
	errScanRefused = errors.New("the process scan refuses")
	// errHostGuarded is what a transaction's entry meets on a host a converge
	// holds: a recognised guard, published or not, as distinct from a claim
	// that could not be classified.
	errHostGuarded = errors.New("this host is held by a converge")
)

// guardOp is one filesystem operation of the guard, as the hook sees it: its
// kind, the path it acts on, and the identity of the descriptor it acts
// through when it acts through one.
type guardOp struct {
	Kind string
	Path string
	Dev  uint64
	Ino  uint64
}

// guardHook observes every filesystem operation the guard makes, before it is
// made, and may fail it. Nil in production; a test sets it to trace the
// publication and release orders by descriptor identity and to fail one step,
// because the state afterwards cannot tell a flush that ran from one that did
// not.
var guardHook func(op guardOp) error

// guardSync is the one flush the guard makes, on the descriptor of the object
// named: the record's temporary, the guard directory, the root, and a hold's
// candidate with its directory. A test fails it for one object; production
// runs syncFD, whose body is exactly the Sync, which the structural witness in
// the tests holds it to.
var guardSync = syncFD

// guardStatAt is the seam through which the role journal's presence is
// examined, so a test can make the examination fail without a filesystem
// that fails one entry of a directory and not another.
var guardStatAt = statAt

// syncFD flushes one descriptor and returns what the kernel said.
func syncFD(f *os.File) error {
	return f.Sync()
}

// The other seams: the clock a record is stamped with, the hostname, and who
// the trust boundary expects to own the root.
var (
	guardNow      = time.Now
	guardHostname = os.Hostname
	// guardExpectedOwner is the uid the upgrade root, the guard and the lock
	// must be owned by.
	guardExpectedOwner = defaultGuardOwner
	// guardOwnerOf reads a file's owner from the metadata a stat produced; a
	// test overrides it to make a fixture's file look like another account's.
	guardOwnerOf = ownerFromInfo
)

// defaultGuardOwner is the process's own effective uid: root on a Linux host,
// where billet's transactions run as root and a root that is not root-owned was
// made by somebody else; the launch agent's account on a Mac, for the same
// reason under that account.
func defaultGuardOwner() uint32 { return uint32(os.Geteuid()) }

// linkCountOf is a file's link count from its stat, or zero when the platform
// reports none (which refuses, the way an unreported owner does).
func linkCountOf(info os.FileInfo) uint64 {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}

	return widen(st.Nlink)
}

// widen is the one spelling of a stat field's conversion to 64 bits: Nlink is
// 16 bits on darwin, 32 on linux/arm64 and 64 on linux/amd64, and a conversion
// written for one of them is flagged as unnecessary on another.
func widen[T ~uint16 | ~uint32 | ~uint64](v T) uint64 { return uint64(v) }

func ownerFromInfo(info os.FileInfo) (uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}

	return st.Uid, true
}

func guardObserve(kind, path string, f *os.File) error {
	if guardHook == nil {
		return nil
	}

	op := guardOp{Kind: kind, Path: path}

	if f != nil {
		if info, err := f.Stat(); err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				op.Dev, op.Ino = devOf(st), st.Ino
			}
		}
	}

	return guardHook(op)
}

// cmdConvergeGuard is the operator's and the role's entry to the guard.
func cmdConvergeGuard(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet converge-guard hold|release|status|recover|holder")
	}

	switch args[0] {
	case "hold":
		return cmdGuardHold(args[1:])
	case "release":
		return cmdGuardRelease(args[1:])
	case "status":
		return cmdGuardStatus(args[1:])
	case "recover":
		return cmdGuardRecover(args[1:])
	case "holder":
		return cmdGuardHolder(args[1:])
	}

	_ = ctx

	return fmt.Errorf("unknown converge-guard command %q; try hold, release, status, recover or holder", args[0])
}

// checkHolder refuses a holder that is not a name, before any lock is taken:
// empty, whitespace, a control character, a slash or more bytes than a name
// needs.
func checkHolder(holder string) error {
	switch {
	case holder == "":
		return errors.New("--holder names who holds the guard, and it is empty")
	case len(holder) > maxHolderBytes:
		return fmt.Errorf("--holder is %d bytes; a holder is a short name", len(holder))
	}

	for _, r := range holder {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '/' || r == unicode.ReplacementChar {
			return fmt.Errorf("--holder %q is not a name: no whitespace, control character or slash", holder)
		}
	}

	return nil
}

func cmdGuardHold(args []string) error {
	flags := newFlagSet("billet converge-guard hold")
	holder := flags.String("holder", "", "who holds the guard: the converge's run id, or an operator's handle")
	candidate := flags.String("candidate", "", "the release executable this guard records, staged by the role "+
		"in a recovery directory under the upgrade root; the managed binary when absent")
	recoverFrom := flags.String("recover-from", "", "take over a guard this holder left, with its transaction "+
		"pointer, after that holder's driver is stopped")
	oldStopped := flags.Bool("old-driver-stopped", false, "assert that the driver which held --recover-from is "+
		"stopped or cannot dispatch further work, and that its remote work has finished")

	if err := parse(flags, args); err != nil {
		return err
	}

	if err := checkHolder(*holder); err != nil {
		return err
	}

	switch {
	case *recoverFrom != "" && *candidate != "":
		return errors.New("a takeover keeps the executable the guard records; --candidate is refused beside --recover-from")
	case *recoverFrom != "" && !*oldStopped:
		return errors.New("--recover-from needs --old-driver-stopped, the assertion that the old holder's driver " +
			"is stopped or cannot dispatch further work and that its remote work has finished")
	case *recoverFrom == "" && *oldStopped:
		return errors.New("--old-driver-stopped is the assertion a takeover carries; it needs --recover-from")
	case *recoverFrom != "":
		if err := checkHolder(*recoverFrom); err != nil {
			return fmt.Errorf("--recover-from: %w", err)
		}
	}

	root, err := prepareUpgradeRoot()
	if err != nil {
		return err
	}

	defer root.close()

	if *recoverFrom != "" {
		return takeOverGuard(root, *recoverFrom, *holder)
	}

	return holdGuard(root, *holder, *candidate)
}

// holdGuard publishes a new guard, or validates an existing one this holder
// already holds. Everything it examines or makes under the root is reached
// through the root's descriptor, so a name replaced under the lock is not the
// thing used.
func holdGuard(root *txLock, holder, candidate string) error {
	dir, shape, err := openGuardForMutation(root)
	if err != nil {
		return err
	}

	if dir != nil {
		defer func() { _ = dir.Close() }()
	}

	switch shape.Kind {
	case claimNone:
	case claimGuard:
		if shape.RecordErr != "" || shape.Guard.Holder != holder {
			return refuseHeld(shape)
		}

		return validateSameHolder(root, dir, shape, candidate)
	default:
		return refuseShape(shape)
	}

	record := guardRecord{Holder: holder}

	record.ClaimedAt = guardNow().UTC().Format(time.RFC3339)

	hostname, err := guardHostname()
	if err != nil || hostname == "" {
		return fmt.Errorf("read this host's name for the guard: %w", err)
	}

	record.Hostname = hostname

	// THE CANDIDATE IS FLUSHED BEFORE THE RECORD NAMES IT: the file, its
	// directory and the root, through the descriptors the digest was taken
	// on, so a guard that survives a power loss records an executable that
	// survived it too. A hold of the managed binary flushes nothing extra.
	path, sum, err := recordExecutable(root, candidate, true)
	if err != nil {
		return err
	}

	record.ReleaseExecutable, record.ReleaseExecutableSHA256 = path, sum

	return publishGuard(root, record)
}

// publishGuard writes a guard durably, in the one order, relative to the root.
func publishGuard(root *txLock, record guardRecord) error {
	active := activePath()

	if err := guardObserve("lstat", active, nil); err != nil {
		return err
	}

	// ABSENT, POSITIVELY: any other answer, a failed read included, is not
	// "nothing is here".
	if _, err := statAt(root.dir, activePointer); !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("%s appeared while the guard was being prepared", active)
		}

		return fmt.Errorf("examine %s: %w", active, err)
	}

	if err := guardObserve("mkdir", active, nil); err != nil {
		return err
	}

	if err := unix.Mkdirat(int(root.dir.Fd()), activePointer, 0o700); err != nil {
		return fmt.Errorf("create the guard: %w", err)
	}

	dir, err := openActive(root)
	if err != nil {
		return err
	}

	defer func() { _ = dir.Close() }()

	if err := writeGuardRecordAt(dir, record, false); err != nil {
		return err
	}

	if err := syncDirFD(dir); err != nil {
		return err
	}

	return syncDirFD(root.dir)
}

// writeGuardRecordAt writes the record as `guard.json.tmp` inside the guard
// directory and renames it into place, the tmp fsynced before the rename, every
// name relative to the directory's descriptor. replace says an existing tmp may
// be replaced (a takeover retrying): it is examined through an identity
// descriptor, removed by its name only when it is a regular, owned, one-link
// file, and a fresh one is created exclusively; a tmp of any other shape
// refuses without touching it.
func writeGuardRecordAt(dir *os.File, record guardRecord, replace bool) error {
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the guard: %w", err)
	}

	body = append(body, '\n')
	tmp := filepath.Join(dir.Name(), guardTmpName)

	f, err := openGuardTmpAt(dir, replace)
	if err != nil {
		return err
	}

	defer func() { _ = f.Close() }()

	if err := guardObserve("write", tmp, f); err != nil {
		return err
	}

	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("write the guard: %w", err)
	}

	if err := guardObserve("fsync", tmp, f); err != nil {
		return err
	}

	if err := guardSync(f); err != nil {
		return fmt.Errorf("flush the guard: %w", err)
	}

	final := filepath.Join(dir.Name(), guardRecordName)

	if err := guardObserve("rename", final, nil); err != nil {
		return err
	}

	if err := unix.Renameat(int(dir.Fd()), guardTmpName, int(dir.Fd()), guardRecordName); err != nil {
		return fmt.Errorf("publish the guard: %w", err)
	}

	return nil
}

// openGuardTmpAt opens the temporary the record is written through, relative
// to the guard directory: created exclusively for a fresh guard; for a retried
// takeover that meets an existing one, that one is examined through an
// identity descriptor (never opened for writing), judged on it (a regular
// file the trust boundary accepts with exactly one link; a FIFO, a link, a
// loose mode, another owner or a second name refuses), removed by its name
// relative to the directory, and a fresh one is created exclusively in its
// place. No inode is ever truncated.
func openGuardTmpAt(dir *os.File, replace bool) (*os.File, error) {
	tmp := filepath.Join(dir.Name(), guardTmpName)

	if err := guardObserve("create", tmp, nil); err != nil {
		return nil, err
	}

	// A FRESH TEMPORARY IS CREATED EXCLUSIVELY, in both modes; only a takeover
	// meeting one that exists goes on to examine, remove and re-create it.
	fd, err := unix.Openat(int(dir.Fd()), guardTmpName,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)

	switch {
	case err == nil:
		return os.NewFile(uintptr(fd), tmp), nil
	case !errors.Is(err, unix.EEXIST):
		return nil, fmt.Errorf("stage the guard: %w", err)
	case !replace:
		return nil, fmt.Errorf("%s exists; the guard was not published cleanly", tmp)
	}

	// A STALE TEMPORARY IS EXAMINED, THEN REMOVED, NEVER OPENED FOR WRITING: the
	// entry is opened for its identity alone (a device's driver never invoked
	// on Linux, a FIFO never waited on), judged on that descriptor (regular,
	// owned, writable by nobody else, ONE LINK: a temporary that is another
	// name of the record, of a preserved binary or of a journal is not this
	// hold's leftover, whatever removing one name would do), and only then
	// unlinked by its name relative to the directory, so no inode is ever
	// truncated in place; a fresh temporary is then created exclusively.
	stale, opened, err := regularfile.OpenAt(dir, guardTmpName)
	if err != nil {
		return nil, fmt.Errorf("the stale %s cannot be replaced: %w: examine it: %w", guardTmpName, errTrustBoundary, err)
	}

	_ = stale.Close()

	if err := requireTrustedFile(tmp, opened); err != nil {
		return nil, fmt.Errorf("the stale %s cannot be replaced: %w", guardTmpName, err)
	}

	if links := linkCountOf(opened); links != 1 {
		return nil, fmt.Errorf("the stale %s cannot be replaced: %w: it has %d links, so it is another name of "+
			"something and not this hold's leftover", guardTmpName, errTrustBoundary, links)
	}

	if err := guardObserve("unlink", tmp, nil); err != nil {
		return nil, err
	}

	if err := unix.Unlinkat(int(dir.Fd()), guardTmpName, 0); err != nil {
		return nil, fmt.Errorf("remove the stale %s: %w", guardTmpName, err)
	}

	fd, err = unix.Openat(int(dir.Fd()), guardTmpName,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("stage the guard after removing the stale %s: %w", guardTmpName, err)
	}

	return os.NewFile(uintptr(fd), tmp), nil
}

// requireTrustedFile is the trust boundary for one regular file: regular, owned
// by the expected account, writable by nobody else.
func requireTrustedFile(path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file (%s)", errTrustBoundary, path, info.Mode().Type())
	}

	return requireTrustedOwner(path, info)
}

// requireTrustedDir is the trust boundary for one directory: a directory, owned
// by the expected account, with the mode given, or with mode 0 any mode that
// gives group and others no write (the role establishes the upgrade root
// 0700; what the boundary needs of it is that nobody else can put an entry in
// it, which 0755 also gives, and a guard directory is 0700 because this
// command made it so).
func requireTrustedDir(path string, info os.FileInfo, mode fs.FileMode) error {
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory (%s)", errTrustBoundary, path, info.Mode().Type())
	}

	perm := info.Mode().Perm()

	switch {
	case mode != 0 && perm != mode:
		return fmt.Errorf("%w: %s is mode %04o, want %04o", errTrustBoundary, path, perm, mode)
	case mode == 0 && perm&0o022 != 0:
		return fmt.Errorf("%w: %s is mode %04o, writable by group or others", errTrustBoundary, path, perm)
	}

	uid, ok := guardOwnerOf(info)
	if !ok {
		return fmt.Errorf("%w: %s carries no owner this platform reports", errTrustBoundary, path)
	}

	if want := guardExpectedOwner(); uid != want {
		return fmt.Errorf("%w: %s is owned by uid %d, want %d", errTrustBoundary, path, uid, want)
	}

	return nil
}

func requireTrustedOwner(path string, info os.FileInfo) error {
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("%w: %s is mode %04o, writable by group or others", errTrustBoundary, path, perm)
	}

	uid, ok := guardOwnerOf(info)
	if !ok {
		return fmt.Errorf("%w: %s carries no owner this platform reports", errTrustBoundary, path)
	}

	if want := guardExpectedOwner(); uid != want {
		return fmt.Errorf("%w: %s is owned by uid %d, want %d", errTrustBoundary, path, uid, want)
	}

	return nil
}

// syncDirFD flushes a directory through the descriptor already held on it.
func syncDirFD(dir *os.File) error {
	if err := guardObserve("fsync", dir.Name(), dir); err != nil {
		return err
	}

	if err := guardSync(dir); err != nil {
		return fmt.Errorf("flush %s: %w", dir.Name(), err)
	}

	return nil
}

// openActive opens the guard directory relative to the validated root, never
// following a link at its name; the descriptor is what every later operation
// on the guard goes through.
func openActive(root *txLock) (*os.File, error) {
	dir, err := openActiveOf(root.dir)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", activePath(), err)
	}

	return dir, nil
}

// statAt examines one entry of a directory through the directory's descriptor,
// never following a link at the entry's name.
func statAt(dir *os.File, name string) (*unix.Stat_t, error) {
	var st unix.Stat_t

	if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}

	return &st, nil
}

// readlinkAt reads a symlink's target relative to the directory's descriptor.
func readlinkAt(dir *os.File, name string) (string, error) {
	for size := 256; ; size *= 2 {
		buf := make([]byte, size)

		n, err := unix.Readlinkat(int(dir.Fd()), name, buf)
		if err != nil {
			return "", err
		}

		if n < size {
			return string(buf[:n]), nil
		}
	}
}

// fileTypeOf names a stat's file type for a diagnostic.
func fileTypeOf(mode uint32) string {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return "a regular file"
	case unix.S_IFDIR:
		return "a directory"
	case unix.S_IFLNK:
		return "a symlink"
	case unix.S_IFIFO:
		return "a named pipe"
	case unix.S_IFSOCK:
		return "a socket"
	case unix.S_IFCHR, unix.S_IFBLK:
		return "a device"
	}

	return "an entry of an unknown type"
}

// openGuardForMutation is what every mutator classifies through: `active`
// examined relative to the root; when it is a directory, opened relative to
// the root, VALIDATED as the guard directory this command would have made
// (0700, owned by the expected account), classified through that descriptor,
// and the descriptor returned so the mutation acts on the directory that was
// validated and classified. Any other shape is classified and returned with no
// descriptor, for the caller to refuse.
func openGuardForMutation(root *txLock) (*os.File, claimShape, error) {
	st, err := statAt(root.dir, activePointer)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, claimShape{Kind: claimNone}, nil
	case err != nil:
		return nil, claimShape{}, fmt.Errorf("examine %s: %w", activePath(), err)
	case modeOf(st)&unix.S_IFMT != unix.S_IFDIR:
		shape, err := classifyClaimAt(root.dir)

		return nil, shape, err
	}

	dir, err := openActive(root)
	if err != nil {
		return nil, claimShape{}, err
	}

	info, err := dir.Stat()
	if err != nil {
		_ = dir.Close()

		return nil, claimShape{}, fmt.Errorf("examine %s: %w", activePath(), err)
	}

	if err := requireTrustedDir(activePath(), info, 0o700); err != nil {
		_ = dir.Close()

		return nil, claimShape{}, err
	}

	shape, err := classifyGuardDirFrom(dir)
	if err != nil {
		_ = dir.Close()

		return nil, claimShape{}, err
	}

	return dir, shape, nil
}

// validateSameHolder is a hold by the holder that already holds: the guard was
// validated through the root's descriptor and nothing is written; what a retry
// still owes is the DURABILITY of a publication it may have interrupted (a hold
// killed after its rename and before its flushes leaves a record that is in the
// directory and not yet on the disk), so the guard directory and the root are
// flushed again. A stale temporary beside the record, or a candidate that is
// not the recorded executable, refuses.
func validateSameHolder(root *txLock, dir *os.File, shape claimShape, candidate string) error {
	active := activePath()

	if _, err := statAt(dir, guardTmpName); err == nil {
		return fmt.Errorf("%s holds a %s beside its record, which a hold does not leave; "+
			"`billet converge-guard status` shows what is there", active, guardTmpName)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("examine %s: %w", filepath.Join(active, guardTmpName), err)
	}

	if candidate != "" {
		cleaned, err := candidatePath(candidate)
		if err != nil {
			return err
		}

		if cleaned != shape.Guard.ReleaseExecutable {
			return fmt.Errorf("this guard records %s as its release executable and a hold by the same "+
				"holder cannot change it to %s", shape.Guard.ReleaseExecutable, cleaned)
		}
	}

	if err := syncDirFD(dir); err != nil {
		return err
	}

	return syncDirFD(root.dir)
}

// recordExecutable is the path and digest the guard records: the candidate
// when one is named, else the managed binary, each opened through descriptors
// and digested by this command.
func recordExecutable(root *txLock, candidate string, flush bool) (string, string, error) {
	if candidate == "" {
		return hashManagedBinary()
	}

	return hashCandidate(root, candidate, flush)
}

// candidatePath is the cleaned absolute path of a candidate, or a refusal: a
// candidate lives in a direct child of the upgrade root named `recovery-*`.
func candidatePath(candidate string) (string, error) {
	cleaned := filepath.Clean(candidate)
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("--candidate %s is not an absolute path", candidate)
	}

	dir := filepath.Dir(cleaned)
	if filepath.Dir(dir) != upgradeRoot || !strings.HasPrefix(filepath.Base(dir), recoveryDirPrefix) {
		return "", fmt.Errorf("--candidate %s is not inside a recovery directory (%s/%s*) of the upgrade root",
			candidate, upgradeRoot, recoveryDirPrefix)
	}

	if strings.HasPrefix(filepath.Base(cleaned), ".") || filepath.Base(cleaned) == "" {
		return "", fmt.Errorf("--candidate %s does not name a file", candidate)
	}

	return cleaned, nil
}

// hashCandidate opens the candidate relative to the validated root: the
// recovery directory relative to the root's descriptor, the file relative to
// the recovery directory's, each examined on its descriptor, the digest taken
// through the file's descriptor, and the name checked against that descriptor
// at the end.
func hashCandidate(root *txLock, candidate string, flush bool) (string, string, error) {
	cleaned, err := candidatePath(candidate)
	if err != nil {
		return "", "", err
	}

	dirName := filepath.Base(filepath.Dir(cleaned))

	if err := guardObserve("openat", filepath.Dir(cleaned), nil); err != nil {
		return "", "", err
	}

	dirFD, err := unix.Openat(int(root.dir.Fd()), dirName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", "", fmt.Errorf("open the recovery directory %s: %w", filepath.Dir(cleaned), err)
	}

	dir := os.NewFile(uintptr(dirFD), filepath.Dir(cleaned))
	defer func() { _ = dir.Close() }()

	dirInfo, err := dir.Stat()
	if err != nil {
		return "", "", fmt.Errorf("examine the recovery directory: %w", err)
	}

	if err := requireTrustedDir(filepath.Dir(cleaned), dirInfo, 0o700); err != nil {
		return "", "", err
	}

	if err := guardObserve("openat", cleaned, nil); err != nil {
		return "", "", err
	}

	f, info, err := regularfile.OpenAt(dir, filepath.Base(cleaned))
	if err != nil {
		return "", "", fmt.Errorf("open the candidate %s: %w", cleaned, err)
	}

	defer func() { _ = f.Close() }()

	if err := requireTrustedFile(cleaned, info); err != nil {
		return "", "", err
	}

	if err := guardObserve("hash", cleaned, f); err != nil {
		return "", "", err
	}

	sum, _, err := hashOpenFile(f, cleaned, maxExecutableBytes)
	if err != nil {
		return "", "", err
	}

	if guardAfterHash != nil {
		guardAfterHash(sum)
	}

	// THE NAME STILL HOLDS THE FILE THAT WAS HASHED: a replacement renamed over
	// the name between the open and here would be recorded under a digest that
	// is not its own.
	named, err := os.Lstat(cleaned)
	if err != nil {
		return "", "", fmt.Errorf("examine %s after hashing it: %w", cleaned, err)
	}

	if !os.SameFile(named, info) {
		return "", "", fmt.Errorf("%s changed while it was being recorded; nothing was recorded", cleaned)
	}

	if !flush {
		return cleaned, sum, nil
	}

	// THE FLUSHES, in the order the durability needs: the file's bytes, the
	// directory entry that names it, the root's entry that names the
	// directory; each on the descriptor already judged, before guard.json
	// exists anywhere.
	if err := guardObserve("fsync", cleaned, f); err != nil {
		return "", "", err
	}

	if err := guardSync(f); err != nil {
		return "", "", fmt.Errorf("flush the candidate %s: %w", cleaned, err)
	}

	if err := guardObserve("fsync", filepath.Dir(cleaned), dir); err != nil {
		return "", "", err
	}

	if err := guardSync(dir); err != nil {
		return "", "", fmt.Errorf("flush the recovery directory %s: %w", filepath.Dir(cleaned), err)
	}

	if err := guardObserve("fsync", upgradeRoot, root.dir); err != nil {
		return "", "", err
	}

	if err := guardSync(root.dir); err != nil {
		return "", "", fmt.Errorf("flush the upgrade root: %w", err)
	}

	return cleaned, sum, nil
}

// guardAfterHash is a seam a test uses to observe the digest the candidate's
// descriptor produced before the name is checked against it.
var guardAfterHash func(sum string)

// hashManagedBinary records the managed binary: opened by its name without
// following a link, examined on the descriptor, hashed through it.
func hashManagedBinary() (string, string, error) {
	if err := guardObserve("open", installedBinary, nil); err != nil {
		return "", "", err
	}

	f, info, err := regularfile.Open(installedBinary, regularfile.Options{NoFollow: true})
	if err != nil {
		return "", "", fmt.Errorf("open the managed binary %s: %w", installedBinary, err)
	}

	defer func() { _ = f.Close() }()

	if err := requireTrustedFile(installedBinary, info); err != nil {
		return "", "", err
	}

	if err := guardObserve("hash", installedBinary, f); err != nil {
		return "", "", err
	}

	sum, _, err := hashOpenFile(f, installedBinary, maxExecutableBytes)
	if err != nil {
		return "", "", err
	}

	return installedBinary, sum, nil
}

func cmdGuardRelease(args []string) error {
	flags := newFlagSet("billet converge-guard release")
	holder := flags.String("holder", "", "the holder releasing its guard")

	if err := parse(flags, args); err != nil {
		return err
	}

	if err := checkHolder(*holder); err != nil {
		return err
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

	switch shape.Kind {
	case claimGuard:
	case claimNone:
		return fmt.Errorf("%w; nothing was released", errNoGuard)
	default:
		return refuseShape(shape)
	}

	if shape.RecordErr != "" {
		return fmt.Errorf("%w, and its record cannot be read (%s); nothing was released", errGuardHeld, shape.RecordErr)
	}

	if shape.Guard.Holder != *holder {
		return fmt.Errorf("%w: it names %s and this release is by %s; nothing was released",
			errGuardHeld, shape.Guard.Holder, *holder)
	}

	if err := requireNoPointerAt(dir); err != nil {
		return err
	}

	return removeGuardAt(root, dir)
}

// requireNoPointerAt refuses while the role's transaction pointer is inside
// the guard, dangling or not, and refuses as could-not-tell when it cannot
// look.
func requireNoPointerAt(dir *os.File) error {
	pointer := filepath.Join(dir.Name(), guardPointerName)

	_, err := statAt(dir, guardPointerName)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("examine %s: %w; the guard is kept", pointer, err)
	}

	return fmt.Errorf("%w (%s): a binary transaction is recorded inside it; `billet host-upgrade --status` "+
		"says where it got to, and `hold --recover-from` takes it over", errGuardPointer, pointer)
}

// removeGuardAt is the release order, through the descriptors.
func removeGuardAt(root *txLock, dir *os.File) error {
	record := filepath.Join(dir.Name(), guardRecordName)

	if err := guardObserve("unlink", record, nil); err != nil {
		return err
	}

	if err := unix.Unlinkat(int(dir.Fd()), guardRecordName, 0); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the guard's record: %w", err)
	}

	return removeUnpublishedDirAt(root, dir)
}

// removeUnpublishedDirAt flushes and removes a guard directory whose record is
// gone.
func removeUnpublishedDirAt(root *txLock, dir *os.File) error {
	if err := syncDirFD(dir); err != nil {
		return err
	}

	if err := guardObserve("rmdir", dir.Name(), nil); err != nil {
		return err
	}

	if err := unix.Unlinkat(int(root.dir.Fd()), activePointer, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("remove the guard directory %s: %w", dir.Name(), err)
	}

	return syncDirFD(root.dir)
}

func cmdGuardRecover(args []string) error {
	flags := newFlagSet("billet converge-guard recover")
	holder := flags.String("holder", "", "remove the guard this holder left with no transaction pointer")
	unpublished := flags.Bool("unpublished", false, "remove a hold that never returned from its publication")
	oldStopped := flags.Bool("old-driver-stopped", false, "assert that the holder's driver is stopped or cannot "+
		"dispatch further work, and that its remote work has finished")

	if err := parse(flags, args); err != nil {
		return err
	}

	switch {
	case *unpublished && *holder != "":
		return errors.New("--unpublished and --holder are two different recoveries; name one")
	case *unpublished && *oldStopped:
		return errors.New("--old-driver-stopped is the assertion a holder's recovery carries; an unpublished " +
			"guard has no holder")
	case !*unpublished && *holder == "":
		return errors.New("usage: billet converge-guard recover --unpublished | --holder ID --old-driver-stopped")
	case *holder != "" && !*oldStopped:
		return errors.New("--holder needs --old-driver-stopped, the assertion that the holder's driver is " +
			"stopped or cannot dispatch further work and that its remote work has finished")
	}

	if *holder != "" {
		if err := checkHolder(*holder); err != nil {
			return err
		}
	}

	root, err := prepareUpgradeRoot()
	if err != nil {
		return err
	}

	defer root.close()

	if *unpublished {
		return recoverUnpublished(root)
	}

	return recoverHolder(root, *holder)
}

// recoverUnpublished removes a directory `active` that holds no record and
// nothing but the publication's temporary, every entry examined through the
// directory's descriptor before any is removed.
func recoverUnpublished(root *txLock) error {
	dir, shape, err := openGuardForMutation(root)
	if err != nil {
		return fmt.Errorf("%w; nothing was removed", err)
	}

	if dir != nil {
		defer func() { _ = dir.Close() }()
	}

	if shape.Kind != claimUnpublished {
		if shape.Kind == claimGuard {
			return fmt.Errorf("%s is a published guard held by %s, not an unpublished one; nothing was removed",
				activePath(), shape.Guard.Holder)
		}

		return refuseShape(shape)
	}

	entries, err := dir.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("list %s: %w; nothing was removed", dir.Name(), err)
	}

	for _, e := range entries {
		st, err := statAt(dir, e.Name())
		if err != nil {
			return fmt.Errorf("examine %s: %w; nothing was removed", filepath.Join(dir.Name(), e.Name()), err)
		}

		if e.Name() != guardTmpName || modeOf(st)&unix.S_IFMT != unix.S_IFREG {
			return fmt.Errorf("%s holds %s (%s), which a publication does not leave; nothing was removed",
				dir.Name(), e.Name(), fileTypeOf(modeOf(st)))
		}
	}

	for _, e := range entries {
		path := filepath.Join(dir.Name(), e.Name())

		if err := guardObserve("unlink", path, nil); err != nil {
			return err
		}

		if err := unix.Unlinkat(int(dir.Fd()), e.Name(), 0); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}

	return removeUnpublishedDirAt(root, dir)
}

// recoverHolder removes a guard naming holder that carries no pointer, under
// the operator's assertion and a clean process scan.
func recoverHolder(root *txLock, holder string) error {
	dir, shape, err := openGuardForMutation(root)
	if err != nil {
		return err
	}

	if dir != nil {
		defer func() { _ = dir.Close() }()
	}

	switch shape.Kind {
	case claimGuard:
	case claimNone:
		return fmt.Errorf("%w; nothing was removed", errNoGuard)
	default:
		return refuseShape(shape)
	}

	if shape.RecordErr != "" {
		return fmt.Errorf("%w, and its record cannot be read (%s); nothing was removed", errGuardHeld, shape.RecordErr)
	}

	if shape.Guard.Holder != holder {
		return fmt.Errorf("%w: it names %s, not %s; nothing was removed", errGuardHeld, shape.Guard.Holder, holder)
	}

	if err := requireNoPointerAt(dir); err != nil {
		return fmt.Errorf("%w; `hold --recover-from %s --old-driver-stopped` takes the transaction over "+
			"instead", err, holder)
	}

	if err := scanForDrivers(); err != nil {
		return err
	}

	return removeGuardAt(root, dir)
}

// takeOverGuard re-labels a guard another holder left with its transaction
// pointer, keeping the pointer and the recorded executable.
func takeOverGuard(root *txLock, old, holder string) error {
	dir, shape, err := openGuardForMutation(root)
	if err != nil {
		return err
	}

	if dir != nil {
		defer func() { _ = dir.Close() }()
	}

	switch shape.Kind {
	case claimGuard:
	case claimNone:
		return fmt.Errorf("%w; there is nothing to take over", errNoGuard)
	default:
		return refuseShape(shape)
	}

	if shape.RecordErr != "" {
		return fmt.Errorf("%w, and its record cannot be read (%s); nothing was taken over", errGuardHeld, shape.RecordErr)
	}

	if shape.Guard.Holder != old {
		return fmt.Errorf("%w: it names %s, not %s; nothing was taken over", errGuardHeld, shape.Guard.Holder, old)
	}

	pointer := filepath.Join(dir.Name(), guardPointerName)

	if _, err := statAt(dir, guardPointerName); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("the guard held by %s carries no transaction pointer, so there is no "+
				"transaction to take over; `recover --holder %s --old-driver-stopped` removes it", old, old)
		}

		return fmt.Errorf("examine %s: %w", pointer, err)
	}

	if err := guardObserve("readlink", pointer, nil); err != nil {
		return err
	}

	target, err := readlinkAt(dir, guardPointerName)
	if err != nil {
		return fmt.Errorf("read the transaction pointer %s: %w", pointer, err)
	}

	if err := underUpgradeRoot(target); err != nil {
		return fmt.Errorf("the transaction pointer names %s: %w", target, err)
	}

	// THE JOURNAL IS JUDGED THROUGH THE ROOT THE LOCK VALIDATED, the pointer's
	// target opened relative to it, so a root displaced at its name resolves
	// nothing here. A takeover proves the transaction COMPLETE, whichever
	// program journals it: a Go transaction's journal.json, or the role's
	// manifest.yml; it loads neither, because it resumes nothing itself.
	if _, err := validateRecoveryUnder(root.dir, target); err != nil {
		return fmt.Errorf("the transaction pointer names %s, whose journal cannot be read: %w; nothing "+
			"was taken over", target, err)
	}

	if err := scanForDrivers(); err != nil {
		return err
	}

	record := shape.Guard
	record.Holder = holder

	if err := writeGuardRecordAt(dir, record, true); err != nil {
		return err
	}

	if err := syncDirFD(dir); err != nil {
		return err
	}

	return syncDirFD(root.dir)
}

func cmdGuardStatus(args []string) error {
	flags := newFlagSet("billet converge-guard status")
	asJSON := flags.Bool("json", false, "print the claim's shape and record as JSON")

	if err := parse(flags, args); err != nil {
		return err
	}

	shape, err := classifyClaim()
	if err != nil {
		return err
	}

	if *asJSON {
		body, err := json.MarshalIndent(shape.report(), "", "  ")
		if err != nil {
			return err
		}

		fmt.Println(string(body))

		return nil
	}

	fmt.Println(shape.String())

	return nil
}

func cmdGuardHolder(args []string) error {
	flags := newFlagSet("billet converge-guard holder")

	if err := parse(flags, args); err != nil {
		return err
	}

	shape, err := classifyClaim()
	if err != nil {
		return err
	}

	if shape.Kind != claimGuard {
		return fmt.Errorf("%w: %s", errNoGuard, shape)
	}

	if shape.RecordErr != "" {
		return fmt.Errorf("%w, and its record cannot be read (%s)", errGuardHeld, shape.RecordErr)
	}

	fmt.Println(shape.Guard.Holder)

	return nil
}

// The shapes `active` can have.
type claimKind string

const (
	claimNone        claimKind = "none"
	claimHostUpgrade claimKind = "host-upgrade"
	claimLegacyRole  claimKind = "legacy-role"
	claimGuard       claimKind = "converge-guard"
	claimUnpublished claimKind = "unpublished-guard"
	claimUnknown     claimKind = "unknown"
)

// claimShape is `active` classified by Lstat, with what the shape carries.
type claimShape struct {
	Kind claimKind
	// Target is a symlink claim's target, and Dangling says whether it resolves.
	Target   string
	Dangling bool
	// Guard is the record of a published guard, and RecordErr why one could not
	// be read (Kind is still claimGuard: a directory with a guard.json is a guard
	// whatever the file says).
	Guard     guardRecord
	RecordErr string
	// Pointer says whether the guard carries the role's transaction pointer.
	Pointer bool
	// Why is the reason a shape is unknown.
	Why string
}

// classifyClaim reads `active` without a lock and without creating anything,
// for the lock-free readers (status, holder, check, the upgrade journal's
// report): the root is opened by its name, never following a link, and the
// one classifier runs on that descriptor. A root that does not exist is no
// claim; one that cannot be opened is could-not-tell.
func classifyClaim() (claimShape, error) {
	root, err := os.OpenFile(upgradeRoot, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return claimShape{Kind: claimNone}, nil
	case err != nil:
		return claimShape{}, fmt.Errorf("open %s: %w", upgradeRoot, err)
	}

	defer func() { _ = root.Close() }()

	return classifyClaimAt(root)
}

// classifyClaimAt reads `active` through the root's descriptor, so a mutator
// classifies the root it validated and locked and no other. It answers
// could-not-tell as an error only for a read that failed; every shape it can
// name is a value.
func classifyClaimAt(root *os.File) (claimShape, error) {
	active := activePath()

	st, err := statAt(root, activePointer)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return claimShape{Kind: claimNone}, nil
	case err != nil:
		return claimShape{}, fmt.Errorf("examine %s: %w", active, err)
	}

	switch modeOf(st) & unix.S_IFMT {
	case unix.S_IFLNK:
		shape := claimShape{Kind: claimHostUpgrade}

		if err := guardObserve("readlink", active, nil); err != nil {
			return claimShape{}, err
		}

		shape.Target, err = readlinkAt(root, activePointer)
		if err != nil {
			return claimShape{}, fmt.Errorf("read %s: %w", active, err)
		}

		// DANGLING IS A POSITIVE ABSENCE OF THE TARGET; any other failure to
		// examine it is could-not-tell.
		var followed unix.Stat_t

		switch err := unix.Fstatat(int(root.Fd()), activePointer, &followed, 0); {
		case errors.Is(err, fs.ErrNotExist):
			shape.Dangling = true
		case err != nil:
			return claimShape{}, fmt.Errorf("examine the claim's target %s: %w", shape.Target, err)
		}

		return shape, nil
	case unix.S_IFREG:
		return claimShape{Kind: claimLegacyRole}, nil
	case unix.S_IFDIR:
		return classifyGuardDirAt(root)
	}

	return claimShape{Kind: claimUnknown, Why: "the claim is neither a symlink, a file nor a directory: " +
		fileTypeOf(modeOf(st))}, nil
}

func classifyGuardDirAt(root *os.File) (claimShape, error) {
	dir, err := openActiveOf(root)
	if err != nil {
		return claimShape{}, fmt.Errorf("open the guard directory %s: %w", activePath(), err)
	}

	defer func() { _ = dir.Close() }()

	return classifyGuardDirFrom(dir)
}

// classifyGuardDirFrom classifies a guard directory through a descriptor the
// caller holds and keeps.
func classifyGuardDirFrom(dir *os.File) (claimShape, error) {
	record := filepath.Join(dir.Name(), guardRecordName)

	body, err := readRecordAt(dir)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return claimShape{Kind: claimUnpublished}, nil
	case err != nil:
		return claimShape{}, fmt.Errorf("read the guard's record %s: %w", record, err)
	}

	shape := claimShape{Kind: claimGuard}

	if err := json.Unmarshal(body, &shape.Guard); err != nil {
		shape.RecordErr = "the record is not JSON: " + err.Error()
	} else if shape.Guard.Holder == "" {
		shape.RecordErr = "the record names no holder"
	}

	_, err = statAt(dir, guardPointerName)

	switch {
	case err == nil:
		shape.Pointer = true
	case !errors.Is(err, fs.ErrNotExist):
		return claimShape{}, fmt.Errorf("examine the guard's pointer: %w", err)
	}

	return shape, nil
}

// openActiveOf is openActive for a root descriptor that is not a held lock's.
func openActiveOf(root *os.File) (*os.File, error) {
	fd, err := unix.Openat(int(root.Fd()), activePointer,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}

	return os.NewFile(uintptr(fd), activePath()), nil
}

// readRecordAt reads `guard.json` through the guard directory's descriptor:
// opened identity first relative to it, required regular, AND REQUIRED INSIDE
// THE TRUST BOUNDARY on the descriptor read (owned by the account, writable by
// nobody else), bounded. The directory's mode says who can name a record; it
// says nothing about a record that is another name of a file someone else
// owns, which a hard link makes, and a record another account can write is
// one that can name any holder it likes.
func readRecordAt(dir *os.File) ([]byte, error) {
	path := filepath.Join(dir.Name(), guardRecordName)

	f, info, err := regularfile.OpenAt(dir, guardRecordName)
	if err != nil {
		return nil, err
	}

	defer func() { _ = f.Close() }()

	if err := requireTrustedFile(path, info); err != nil {
		return nil, err
	}

	return regularfile.ReadAllLimited(f, path, maxGuardRecordBytes)
}

func (s claimShape) String() string {
	switch s.Kind {
	case claimNone:
		return "none: nothing claims this host"
	case claimHostUpgrade:
		if s.Dangling {
			return fmt.Sprintf("host-upgrade: a Go transaction's claim pointing at %s, which does not exist", s.Target)
		}

		return fmt.Sprintf("host-upgrade: a Go transaction's claim, its journal in %s", s.Target)
	case claimLegacyRole:
		return "legacy-role: a pre-R role transaction's pointer"
	case claimUnpublished:
		return "unpublished-guard: a hold that never returned from its publication; " +
			"`billet converge-guard recover --unpublished` removes it"
	case claimGuard:
		if s.RecordErr != "" {
			return "converge-guard: a guard whose record cannot be read: " + s.RecordErr
		}

		pointer := "no transaction pointer"
		if s.Pointer {
			pointer = "carrying a transaction pointer"
		}

		return fmt.Sprintf("converge-guard: held by %s since %s on %s, %s", s.Guard.Holder, s.Guard.ClaimedAt,
			s.Guard.Hostname, pointer)
	}

	return "unknown: " + s.Why
}

// guardStatusReport is `status --json`.
type guardStatusReport struct {
	Active   string          `json:"active"`
	Target   string          `json:"target,omitempty"`
	Dangling bool            `json:"dangling,omitempty"`
	Guard    *guardStatusRec `json:"guard,omitempty"`
	Why      string          `json:"why,omitempty"`
}

type guardStatusRec struct {
	Holder                    string `json:"holder"`
	ClaimedAt                 string `json:"claimed_at"`
	Hostname                  string `json:"hostname"`
	RecoveryPointer           bool   `json:"recovery_pointer"`
	ReleaseExecutable         string `json:"release_executable"`
	ReleaseExecutableSHA256   string `json:"release_executable_sha256"`
	ReleaseExecutableVerified maybe  `json:"release_executable_verified"`
	RecordError               string `json:"record_error,omitempty"`
}

func (s claimShape) report() guardStatusReport {
	out := guardStatusReport{Active: string(s.Kind), Target: s.Target, Dangling: s.Dangling, Why: s.Why}

	if s.Kind != claimGuard {
		return out
	}

	rec := &guardStatusRec{
		Holder: s.Guard.Holder, ClaimedAt: s.Guard.ClaimedAt, Hostname: s.Guard.Hostname,
		RecoveryPointer: s.Pointer, ReleaseExecutable: s.Guard.ReleaseExecutable,
		ReleaseExecutableSHA256: s.Guard.ReleaseExecutableSHA256, RecordError: s.RecordErr,
	}

	// VERIFIED, NEVER RUN: the recorded executable's digest now against the
	// digest the hold recorded.
	switch {
	case s.RecordErr != "":
		rec.ReleaseExecutableVerified = unknown(s.RecordErr)
	case s.Guard.ReleaseExecutable == "" || s.Guard.ReleaseExecutableSHA256 == "":
		rec.ReleaseExecutableVerified = unknown("the guard records no release executable")
	default:
		sum, _, err := hashRegular(s.Guard.ReleaseExecutable, maxExecutableBytes)
		if err != nil {
			rec.ReleaseExecutableVerified = unknown(err.Error())
		} else {
			rec.ReleaseExecutableVerified = known(sum == s.Guard.ReleaseExecutableSHA256)
		}
	}

	out.Guard = rec

	return out
}

// refuseHeld is the refusal of a hold or a release by another holder.
func refuseHeld(shape claimShape) error {
	age := "an unknown age"

	if at, err := time.Parse(time.RFC3339, shape.Guard.ClaimedAt); err == nil {
		age = guardNow().Sub(at).Truncate(time.Second).String() + " ago"
	}

	if shape.RecordErr != "" {
		return fmt.Errorf("%w, and its record cannot be read (%s); `billet converge-guard status` shows it",
			errGuardHeld, shape.RecordErr)
	}

	return fmt.Errorf("%w: held by %s since %s (%s) on %s. If that driver is stopped, "+
		"`billet converge-guard recover --holder %s --old-driver-stopped` removes it, or "+
		"`hold --recover-from %s --old-driver-stopped` takes over its transaction",
		errGuardHeld, shape.Guard.Holder, shape.Guard.ClaimedAt, age, shape.Guard.Hostname,
		shape.Guard.Holder, shape.Guard.Holder)
}

// refuseShape is the refusal of a mutator that met a claim it does not own.
func refuseShape(shape claimShape) error {
	switch shape.Kind {
	case claimHostUpgrade:
		return fmt.Errorf("a Go transaction (`billet host-upgrade`) claims this host: %s; "+
			"`billet host-upgrade --status` and `--resume` are its commands", shape)
	case claimLegacyRole:
		return errors.New("a pre-R role transaction's pointer claims this host; the role's own recovery " +
			"(upgrade-recover.yml) is what handles it, and a fresh converge follows")
	case claimUnpublished:
		return fmt.Errorf("%w; `billet converge-guard recover --unpublished` removes it", errGuardUnpublished)
	case claimGuard:
		return refuseHeld(shape)
	}

	return fmt.Errorf("the claim on this host is %s", shape)
}

// guardStaleAfter is how long a guard may be held before `billet check` says
// somebody has forgotten it; nothing expires it.
const guardStaleAfter = 24 * time.Hour

// printGuard is the one line `billet status` gives the host's guard.
func printGuard() {
	shape, err := classifyClaim()

	switch {
	case err != nil:
		fmt.Printf("guard     could not be read: %v\n", err)
	case shape.Kind == claimGuard:
		fmt.Printf("guard     %s\n", describeGuardAge(shape))
	case shape.Kind == claimUnpublished:
		fmt.Printf("guard     %s\n", shape)
	}
}

// checkGuard is `billet check`'s report of the host's guard: a fresh one is
// informational, one past guardStaleAfter is a warning naming the recovery, and
// a record whose time cannot be read is could-not-tell, never fresh.
func checkGuard() {
	shape, err := classifyClaim()

	switch {
	case err != nil:
		fmt.Printf("guard    could not be read: %v\n", err)

		return
	case shape.Kind == claimUnpublished:
		fmt.Printf("guard    WARNING: %s\n", shape)

		return
	case shape.Kind != claimGuard:
		return
	}

	if shape.RecordErr != "" {
		fmt.Printf("guard    WARNING: a guard whose record cannot be read (%s); `billet converge-guard status`\n",
			shape.RecordErr)

		return
	}

	at, err := time.Parse(time.RFC3339, shape.Guard.ClaimedAt)
	if err != nil {
		fmt.Printf("guard    WARNING: held by %s, claimed at %q which is not a time, so its age cannot be "+
			"told; `billet converge-guard status`\n", shape.Guard.Holder, shape.Guard.ClaimedAt)

		return
	}

	age := guardNow().Sub(at)
	if age >= guardStaleAfter {
		fmt.Printf("guard    WARNING: held by %s for %s (since %s); if that converge is over, "+
			"`billet converge-guard recover --holder %s --old-driver-stopped` removes it\n",
			shape.Guard.Holder, age.Truncate(time.Minute), shape.Guard.ClaimedAt, shape.Guard.Holder)

		return
	}

	fmt.Printf("guard    held by %s for %s (since %s)\n", shape.Guard.Holder, age.Truncate(time.Second),
		shape.Guard.ClaimedAt)
}

func describeGuardAge(shape claimShape) string {
	if shape.RecordErr != "" {
		return "a guard whose record cannot be read: " + shape.RecordErr
	}

	age := "an unknown age"
	if at, err := time.Parse(time.RFC3339, shape.Guard.ClaimedAt); err == nil {
		age = guardNow().Sub(at).Truncate(time.Second).String()
	}

	return fmt.Sprintf("held by %s for %s (since %s)", shape.Guard.Holder, age, shape.Guard.ClaimedAt)
}

// guardRefusal is the refusal a host under a guard gives a transaction: "guarded
// by H since T", the words a node's acknowledgement carries to the coordinator.
func guardRefusal(shape claimShape) error {
	return fmt.Errorf("%w: guarded by %s since %s (%s); `billet converge-guard status` on the host says more",
		errHostGuarded, shape.Guard.Holder, shape.Guard.ClaimedAt, shape.Guard.Hostname)
}

// refuseGuardedHost is what a transaction's entry runs immediately after the
// lock, through the lock's own root: a directory claim of either kind refuses.
func refuseGuardedHost(tx *txLock) error {
	shape, err := classifyClaimAt(tx.dir)
	if err != nil {
		return err
	}

	switch shape.Kind {
	case claimGuard:
		if shape.RecordErr != "" {
			return fmt.Errorf("%w: guarded by a converge whose record cannot be read (%s); "+
				"`billet converge-guard status` on the host says more", errHostGuarded, shape.RecordErr)
		}

		return guardRefusal(shape)
	case claimUnpublished:
		return fmt.Errorf("%w: %w", errHostGuarded, refuseShape(shape))
	}

	return nil
}
