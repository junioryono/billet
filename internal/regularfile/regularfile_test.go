package regularfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestOpenReadsARegularFileAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, info, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if !info.Mode().IsRegular() || info.Size() != 6 {
		t.Errorf("info = %v %d", info.Mode(), info.Size())
	}
	body, err := ReadFile(path, 16, Options{})
	if err != nil || string(body) != "hello\n" {
		t.Errorf("ReadFile = %q, %v", body, err)
	}
	if _, _, err := Open(dir, Options{}); !errors.Is(err, ErrNotRegular) || !strings.Contains(err.Error(), "a directory") {
		t.Errorf("a directory opened: %v", err)
	}
	if _, _, err := Open(filepath.Join(dir, "missing"), Options{}); !errors.Is(err, fs.ErrNotExist) || !os.IsNotExist(err) {
		t.Errorf("a missing file is not ErrNotExist: %v", err)
	}
}

// A FIFO WITH NO WRITER RETURNS AT ONCE. A plain open would block here until the
// test wrote to it; the writer below frees a blocked open after the deadline
// so the case fails naming what happened rather than hanging.
func TestOpenDoesNotWaitOnAFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("this platform will not make a fifo: %v", err)
	}
	blocked := unblockAfter(t, fifo, 5*time.Second)
	_, _, err := Open(fifo, Options{})
	if blocked() {
		t.Fatal("Open blocked on the FIFO until the test wrote to it")
	}
	if !errors.Is(err, ErrNotRegular) || !strings.Contains(err.Error(), "a FIFO") {
		t.Errorf("a FIFO was not refused as one: %v", err)
	}
	if pathErr, ok := errors.AsType[*os.PathError](err); !ok || pathErr.Path != fifo {
		t.Errorf("the refusal does not name the path: %v", err)
	}
}

func TestNoFollowRefusesASymlinkAndTheDefaultFollowsIt(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	body, err := ReadFile(link, 16, Options{})
	if err != nil || string(body) != "t\n" {
		t.Errorf("a symlink was not followed by default: %q %v", body, err)
	}
	if _, err := ReadFile(link, 16, Options{NoFollow: true}); err == nil {
		t.Error("NoFollow read through a symlink")
	}
}

func TestReadFileRefusesALongerFileRatherThanTruncating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	if body, err := ReadFile(path, 9, Options{}); !errors.Is(err, ErrTooLarge) || body != nil {
		t.Errorf("a longer file was read: %q %v", body, err)
	}
	if body, err := ReadFile(path, 10, Options{}); err != nil || len(body) != 10 {
		t.Errorf("a file exactly at the limit was refused: %q %v", body, err)
	}
}

// THE BYTES READ ARE THE FILE THAT WAS ADMITTED: a replacement renamed over the
// name after the identity open is not read for the original.
func TestOpenReadsTheFileItAdmittedNotTheNameAfterwards(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "next"), []byte("B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := openForIdentity(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = id.Close() }()
	if err := os.Rename(filepath.Join(dir, "next"), path); err != nil {
		t.Fatal(err)
	}
	f, err := Reopen(id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	got := make([]byte, 8)
	n, err := f.Read(got)
	if err != nil || string(got[:n]) != "A\n" {
		t.Errorf("read %q, %v; want the admitted file's bytes, not the replacement's", got[:n], err)
	}
}

// unblockAfter opens the FIFO for writing once the deadline passes, so an open
// that blocked returns and the test fails naming it; joined before it answers.
func unblockAfter(t *testing.T, fifo string, after time.Duration) func() bool {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	var acted atomic.Bool
	go func() {
		defer close(done)
		timer := time.NewTimer(after)
		defer timer.Stop()
		select {
		case <-stop:
			return
		case <-timer.C:
		}
		for {
			select {
			case <-stop:
				return
			default:
			}
			fd, err := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
			if err == nil {
				acted.Store(true)
				time.Sleep(50 * time.Millisecond)
				_ = syscall.Close(fd)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	var once atomic.Bool
	stopAndJoin := func() bool {
		if once.CompareAndSwap(false, true) {
			close(stop)
		}
		<-done
		return acted.Load()
	}
	t.Cleanup(func() { stopAndJoin() })
	return stopAndJoin
}
