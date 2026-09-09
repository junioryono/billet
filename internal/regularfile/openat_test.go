package regularfile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// OpenAt reads a regular entry of the directory it is given, refuses a link at
// the name as the link's own inode (never followed, never ELOOP: one answer on
// every platform), refuses a FIFO without waiting on it, and its identity
// descriptor reports a link as a link.
func TestOpenAtReadsAnEntryAndRefusesALinkAndAFIFOWithoutAWait(t *testing.T) {
	base := t.TempDir()
	dir, err := os.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()

	if err := os.WriteFile(filepath.Join(base, "plain"), []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	f, info, err := OpenAt(dir, "plain")
	if err != nil {
		t.Fatalf("OpenAt of a regular entry: %v", err)
	}
	defer func() { _ = f.Close() }()

	if body, err := ReadAllLimited(f, "plain", 16); err != nil || string(body) != "bytes" || info.Size() != 5 {
		t.Errorf("the entry read %q, %v (size %d)", body, err, info.Size())
	}

	if err := os.Symlink(filepath.Join(base, "plain"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := OpenAt(dir, "link"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("OpenAt of a link: err = %v, want ErrNotRegular", err)
	}

	id, err := OpenIdentityAt(dir, "link")
	if err != nil {
		t.Fatalf("OpenIdentityAt of a link: %v", err)
	}
	defer func() { _ = id.Close() }()

	if st, err := id.Stat(); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the identity descriptor of a link reports %v, %v; want a symlink", st, err)
	}

	if err := syscall.Mkfifo(filepath.Join(base, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := OpenAt(dir, "fifo")
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Errorf("OpenAt of a FIFO: err = %v, want ErrNotRegular", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OpenAt waited on a FIFO")
	}

	if _, _, err := OpenAt(dir, "absent"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("OpenAt of an absent entry: err = %v, want ErrNotExist", err)
	}
}
