package firecracker

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A VMM is proved exited only when the pid file names a process /proc says is no
// longer this jail's VMM; a gone socket alone leaves a running listener and guest.
func TestAVMMIsEndedOnlyWhenItsProcessIsProvedGone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pidFile string
		owns    bool
		ownsErr error
		want    bool
	}{
		{name: "no pid file", want: false},
		{name: "unparseable pid file", pidFile: "vmm\n", want: false},
		{name: "process is still the vmm", pidFile: "4321\n", owns: true, want: false},
		{name: "could not tell", pidFile: "4321\n", ownsErr: errors.New("permission denied"), want: false},
		{name: "process is gone", pidFile: "4321\n", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, withPidOwner(func(pid int, jailID string) (bool, error) {
				if pid != 4321 || jailID != theInstance {
					t.Errorf("asked about pid %d of %q, want 4321 of %q", pid, jailID, theInstance)
				}

				return tc.owns, tc.ownsErr
			}))

			j := h.p.jailFor(theInstance)

			if tc.pidFile != "" {
				if err := os.MkdirAll(filepath.Dir(j.pidFile()), 0o700); err != nil {
					t.Fatalf("make the pid directory: %v", err)
				}
				if err := os.WriteFile(j.pidFile(), []byte(tc.pidFile), 0o600); err != nil {
					t.Fatalf("write the pid file: %v", err)
				}
			}

			if got := h.p.vmmExited(j); got != tc.want {
				t.Fatalf("vmmExited = %v, want %v", got, tc.want)
			}
		})
	}
}
