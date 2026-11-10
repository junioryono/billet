// Package blockfs is what a store that hands this host a block device asks of the device
// itself: whether the ext4 filesystem on it is clean, and whether anything on the host
// still holds it. The Ceph store and the EBS store's self-attached volumes share it, so
// a cache published from either was proved the same way.
package blockfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	storecontract "github.com/junioryono/billet/internal/store"
)

// MaxDiagnostic bounds how much of another program's output reaches a terminal or a log.
const MaxDiagnostic = 300

// probeLimit bounds what is kept of blkid's and e2fsck's output.
const probeLimit = 8 << 10

// TailWriter keeps the last Limit bytes written to it and discards the rest.
//
// STDERR IS ANOTHER PROGRAM'S OUTPUT AND ITS SIZE IS ITS OWN CHOICE. Collecting it
// into a strings.Builder bounds the time an invocation may take and not the memory
// it may take, so a wrong or broken executable can allocate for the whole timeout.
// Keeping the TAIL rather than the head is what makes the bound useful: librados and
// e2fsck print their preamble first and the sentence that matters last.
type TailWriter struct {
	Limit int
	buf   []byte
}

func (w *TailWriter) Write(p []byte) (int, error) {
	n := len(p)

	// A LIMIT OF ZERO KEEPS NOTHING rather than panicking: the slice arithmetic below
	// indexes from the end, and a negative limit would be a runtime panic.
	if w.Limit <= 0 {
		w.buf = nil

		return n, nil
	}

	// TWO TRIMS, AND THE FIRST ONE IS NOT REDUNDANT even though deleting it leaves
	// every observable behaviour identical. It bounds the PEAK allocation of a single
	// write: without it a lone 10GB write is appended in full and only then trimmed,
	// which is the allocation this type exists to prevent. The second trim bounds what
	// ACCUMULATES across writes, which the first cannot.
	if len(p) > w.Limit {
		p = p[len(p)-w.Limit:]
	}

	w.buf = append(w.buf, p...)
	if len(w.buf) > w.Limit {
		w.buf = w.buf[len(w.buf)-w.Limit:]
	}

	// The length WRITTEN TO US, never the length kept: io.Writer's contract makes a
	// short count an error, so reporting what survived the trim would make exec
	// conclude the pipe broke and abandon a healthy command.
	return n, nil
}

func (w *TailWriter) String() string { return strings.TrimSpace(string(w.buf)) }

// Sanitize drops the bytes that are not text. Another program's output is not
// guaranteed to be UTF-8, and a byte-level truncation upstream can leave half a rune,
// which then reaches an error string that is printed, logged and marshalled. Dropping
// is right rather than replacing: the replacement character would be billet adding a
// glyph to somebody else's message.
func Sanitize(s string) string { return strings.ToValidUTF8(s, "") }

// Bounded renders a value that came from another program, capped and quoted.
//
// A value that parsed is not thereby safe: it arrived from a binary billet does not
// control, and a megabyte of it in an error string reaches the same terminal. QUOTED,
// so a control byte in it cannot become a live terminal control when billet prints it.
func Bounded(v string) string {
	clean := Sanitize(v)
	quoted := strconv.Quote(clean)

	// THE RENDERED LENGTH, not the input length. Quoting EXPANDS — 300 NUL bytes
	// become 1,200 characters of `\x00` — so capping what goes in leaves what comes
	// out four times the bound it was supposed to have.
	if len(quoted) <= MaxDiagnostic {
		return quoted
	}

	// THE LARGEST PREFIX WHOSE QUOTED FORM FITS, built up escape by escape rather than
	// sliced out of the finished string. Slicing cuts inside an escape — 100 NUL bytes
	// truncate to `"\x00\x00…\x0` — and assembling it cannot produce a partial escape.
	var b strings.Builder

	b.WriteByte('"')

	for _, r := range clean {
		piece := strconv.Quote(string(r))
		piece = piece[1 : len(piece)-1]

		if b.Len()+len(piece)+len("…\"") > MaxDiagnostic {
			break
		}

		b.WriteString(piece)
	}

	b.WriteString("…\"")

	return b.String()
}

// VerifyExt4 proves the filesystem on device is ext4 and clean, read-only: blkid names
// its type and identity, and `e2fsck -f -n` finds nothing to repair.
func VerifyExt4(ctx context.Context, device string) (storecontract.Filesystem, error) {
	blkid, err := exec.LookPath("blkid")
	if err != nil {
		return storecontract.Filesystem{}, fmt.Errorf("blkid is required to identify a cache filesystem: %w", err)
	}

	e2fsck, err := exec.LookPath("e2fsck")
	if err != nil {
		return storecontract.Filesystem{}, fmt.Errorf("e2fsck is required to verify an ext4 cache: %w", err)
	}

	output := &TailWriter{Limit: probeLimit}
	cmd := exec.CommandContext(ctx, blkid, "-p", "-s", "TYPE", "-s", "UUID", "-o", "export", device)
	cmd.Stdout = output
	if err := cmd.Run(); err != nil {
		return storecontract.Filesystem{}, fmt.Errorf("identify %s: %w", device, err)
	}

	filesystem := storecontract.Filesystem{}
	for _, line := range strings.Split(output.String(), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		switch key {
		case "TYPE":
			filesystem.Type = value
		case "UUID":
			filesystem.UUID = value
		}
	}

	if filesystem.Type != "ext4" {
		return storecontract.Filesystem{}, fmt.Errorf("cache device %s contains %q, want ext4",
			device, filesystem.Type)
	}

	// E2FSCK'S OWN WORDS ARE KEPT, the tail of them. Every Go cache publication on one
	// deployment was refused here from 2026-10-18 to 2026-10-26 with an exit status
	// alone, and a refusal that cannot say what e2fsck found cannot be told from a
	// journal awaiting replay, a damaged bitmap or a device that was not the volume (#689).
	report := &TailWriter{Limit: probeLimit}
	check := exec.CommandContext(ctx, e2fsck, "-f", "-n", device)
	check.Stdout, check.Stderr = report, report
	if err := check.Run(); err != nil {
		return storecontract.Filesystem{}, fmt.Errorf("the ext4 filesystem on %s is not clean: %w: %s",
			device, err, Bounded(strings.TrimSpace(report.String())))
	}

	filesystem.Clean = true

	return filesystem, filesystem.Valid()
}

// ErrHeld means something on this host still holds a block device: a mount, a device
// stacked on it, or a process with it open.
var ErrHeld = errors.New("blockfs: the device is still held")

// Unheld proves nothing on this host holds device. An error is either ErrHeld or
// could-not-tell, and both refuse: a caller about to take a device away from the host
// does it only on this proof.
//
// FIVE WAYS A DEVICE IS HELD, and an exclusive open answers only two of them. A mount,
// in any mount namespace, and a device mapper or md array stacked on it each claim the
// device, so an O_EXCL open of it fails with EBUSY. A process that opened it without
// O_EXCL — a VMM serving it to a guest — claims nothing, so every thread's open files are
// read for a file whose device number is this one, and every process's mappings, since a
// shared mapping keeps the file after its descriptor is closed. A loop device backed by
// it keeps the kernel's own reference after its setup closed its descriptor, so every
// configured loop's backing device is asked of the kernel. And a partition is a device
// of its own number, whose openers none of those see, so a device with partitions is
// refused outright: billet formats its cache volumes whole.
//
// NOT AN IO_URING'S REGISTERED FILE. A process that registers the device in a ring and
// closes its descriptor holds it through an anonymous inode no scan here can read, and
// refusing every ring would refuse Firecracker's own async block engine. On a pool host
// the only process that opens a cache device is billet's VMM, which keeps its descriptor
// for as long as it serves the drive, so the descriptor scan sees it. EBS DETACHES A MOUNTED VOLUME
// WITHOUT REFUSING: measured on 2026-11-09 (us-west-2, t3.small), a detach of an ext4
// volume mounted on the host completed in 4.4 s, and the kernel aborted its journal on
// the I/O errors that followed. Nothing on AWS's side stops it, so this is what does.
func Unheld(device string) error {
	return unheld(device, hostView{proc: "/proc", sysBlock: "/sys/block", sysDev: "/sys/dev/block",
		loopBacking: loopBacking})
}

// hostView is where Unheld reads the host: the process table, the block devices and the
// kernel's own answer for a loop device's backing (loopBacking), each a test's to stage.
type hostView struct {
	proc, sysBlock, sysDev string
	loopBacking            func(loop string) (rdev uint64, configured bool, err error)
}

func unheld(device string, host hostView) error {
	var st unix.Stat_t
	if err := unix.Stat(device, &st); err != nil {
		return fmt.Errorf("blockfs: stat %s: %w", device, err)
	}

	if st.Mode&unix.S_IFMT != unix.S_IFBLK {
		return fmt.Errorf("blockfs: %s is not a block device", device)
	}

	rdev := rdevOf(&st)

	if err := refusePartitions(host.sysDev, device, rdev); err != nil {
		return err
	}

	fd, err := unix.Open(device, unix.O_RDONLY|unix.O_EXCL|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("%w: %s is mounted or has a device stacked on it", ErrHeld, device)
		}

		return fmt.Errorf("blockfs: open %s exclusively: %w", device, err)
	}

	if err := unix.Close(fd); err != nil {
		return fmt.Errorf("blockfs: close %s: %w", device, err)
	}

	holder, err := openedBy(host.proc, rdev)
	if err != nil {
		return err
	}

	if holder != "" {
		return fmt.Errorf("%w: process %s has %s open", ErrHeld, holder, device)
	}

	loop, err := loopBackedBy(host.sysBlock, rdev, host.loopBacking)
	if err != nil {
		return err
	}

	if loop != "" {
		return fmt.Errorf("%w: %s is backed by %s", ErrHeld, loop, device)
	}

	return nil
}

// refusePartitions refuses a device with partitions, read from sysfs by its number: a
// partition is a device of its own number, held by openers the scans of this device's
// number never see. A device sysfs does not show is could-not-tell.
func refusePartitions(sysDev, device string, rdev uint64) error {
	dir := filepath.Join(sysDev, fmt.Sprintf("%d:%d", unix.Major(rdev), unix.Minor(rdev)))

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("blockfs: read %s's partitions: %w", device, err)
	}

	for _, entry := range entries {
		// A PARTITION IS A DIRECTORY with a "partition" file; the device's attributes
		// beside it are files, which stat answers ENOTDIR under (measured on a t3.small,
		// 2026-11-10: alignment_offset).
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, entry.Name(), "partition")); err == nil {
			return fmt.Errorf("%w: %s has a partition, %s, whose holders are not this device's", ErrHeld,
				device, entry.Name())
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("blockfs: read %s's partitions: %w", device, err)
		}
	}

	return nil
}

// loopBackedBy names a configured loop device whose backing device is the one with
// device number rdev, or "" when none is. Each loop's backing is the kernel's own answer
// (backing), never its pathname, which can name another file than the one the loop holds
// (a path unlinked since, or one spelled with trailing spaces); a loop the kernel cannot
// answer for is could-not-tell.
func loopBackedBy(
	sysBlock string, rdev uint64, backing func(loop string) (uint64, bool, error),
) (string, error) {
	entries, err := os.ReadDir(sysBlock)
	if err != nil {
		return "", fmt.Errorf("blockfs: list block devices: %w", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "loop") {
			continue
		}

		backed, configured, err := backing(name)
		if err != nil {
			return "", fmt.Errorf("blockfs: ask the kernel what %s is backed by: %w", name, err)
		}

		if configured && backed == rdev {
			return name, nil
		}
	}

	return "", nil
}

// openedBy names a process that has a block device with device number rdev open,
// or "" when none does. Every process is read: one that ends while it is read is
// gone and holds nothing, and any other failure is could-not-tell.
//
// EVERY THREAD'S TABLE, NOT THE PROCESS'S. /proc lists thread-group leaders, and a
// leader's fd directory is its own table: a thread that unshared its table, or one still
// running after its leader exited, holds descriptors only its task directory shows.
func openedBy(procRoot string, rdev uint64) (string, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return "", fmt.Errorf("blockfs: list processes: %w", err)
	}

	for _, entry := range entries {
		pid := entry.Name()
		if _, err := strconv.Atoi(pid); err != nil {
			continue
		}

		tasks, err := os.ReadDir(filepath.Join(procRoot, pid, "task"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
				continue
			}

			return "", fmt.Errorf("blockfs: list process %s's threads: %w", pid, err)
		}

		// EACH THREAD'S DESCRIPTORS AND MAPPINGS. A shared mapping keeps the file after its
		// descriptor is closed; the mappings are the thread group's, but a leader that has
		// exited shows none of them, so they are read through every thread that is left,
		// at /proc/<tid>, which resolves for any thread though /proc lists only leaders.
		for _, task := range tasks {
			for _, view := range []string{
				filepath.Join(procRoot, pid, "task", task.Name(), "fd"),
				filepath.Join(procRoot, task.Name(), "map_files"),
			} {
				held, err := tableHolds(view, rdev)
				if err != nil {
					return "", fmt.Errorf("blockfs: read process %s's thread %s (%s): %w", pid, task.Name(),
						filepath.Base(view), err)
				}
				if held {
					return pid, nil
				}
			}
		}
	}

	return "", nil
}

// tableHolds reports whether a descriptor table, one fd directory, holds the block
// device with device number rdev. A thread that ended while it is read holds nothing.
func tableHolds(fds string, rdev uint64) (bool, error) {
	names, err := os.ReadDir(fds)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
			return false, nil
		}

		return false, err
	}

	for _, name := range names {
		var st unix.Stat_t
		if err := unix.Stat(filepath.Join(fds, name.Name()), &st); err != nil {
			// A descriptor closed or a thread ended between the listing and the stat
			// holds nothing now: the link resolves to the open file itself, so a file
			// that is open always answers.
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) {
				continue
			}

			return false, fmt.Errorf("inspect open file %s: %w", name.Name(), err)
		}

		if st.Mode&unix.S_IFMT == unix.S_IFBLK && rdevOf(&st) == rdev {
			return true, nil
		}
	}

	return false, nil
}
