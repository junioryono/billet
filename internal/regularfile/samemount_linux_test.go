//go:build linux

package regularfile

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// THE KERNELS BILLET RUNS ON REPORT A MOUNT IDENTITY (Linux 5.8 onwards), and a
// file in a temporary directory is on that directory's mount by it.
func TestLinuxReportsTheMountIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	m, err := MountOf(f)
	if err != nil {
		t.Fatal(err)
	}

	if !m.Known || m.ID == 0 {
		t.Errorf("this kernel reported no mount identity: %+v", m)
	}
}

// A KERNEL THAT DOES NOT FILL STATX_MNT_ID, OR HAS NO STATX AT ALL, is answered
// with the device alone, which Holds then compares.
func TestAKernelWithoutAMountIdentityAnswersWithTheDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		t.Fatal(err)
	}

	kernel := statx
	t.Cleanup(func() { statx = kernel })

	for name, fake := range map[string]func(int, string, int, int, *unix.Statx_t) error{
		"no STATX_MNT_ID": func(fd int, p string, flags, mask int, stx *unix.Statx_t) error {
			err := kernel(fd, p, flags, mask, stx)
			stx.Mask &^= unix.STATX_MNT_ID
			stx.Mnt_id = 42

			return err
		},
		"no statx": func(int, string, int, int, *unix.Statx_t) error { return unix.ENOSYS },
	} {
		statx = fake

		m, err := MountOf(f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		if m.Known || m.ID != 0 || m.Dev != st.Dev {
			t.Errorf("%s: got %+v, want only the device %d", name, m, st.Dev)
		}
	}
}
