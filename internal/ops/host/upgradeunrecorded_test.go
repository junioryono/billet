package host

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/hostupgrade"
)

// unwritableJournalHost fails the transaction's first step and, at that moment,
// puts a directory where the journal's staging file goes, so the failure itself
// cannot be written: what a full disk did on 2026-11-08 (#802), made to happen even
// for root.
type unwritableJournalHost struct {
	hostupgrade.Host
	t   *testing.T
	dir string
}

func (h unwritableJournalHost) PreserveCurrent(context.Context, string) error {
	if err := os.Mkdir(filepath.Join(h.dir, hostupgrade.JournalName+".new"), 0o700); err != nil {
		h.t.Fatalf("block the journal: %v", err)
	}

	return errors.New("preserving the installed release failed")
}

// withInstalledBinary points installedBinary at path for the test.
func withInstalledBinary(t *testing.T, path string) {
	t.Helper()

	original := installedBinary
	installedBinary = path

	t.Cleanup(func() { installedBinary = original })
}

// A FAILURE THE JOURNAL COULD NOT RECORD IS SAID WITH THE WAY OUT, through the
// command's own finish: the binary on the disk, the configuration the command ran
// with, and that a resume continues rather than promises a rollback.
func TestAnUnrecordedFailureSaysHowToRecover(t *testing.T) {
	root := useRecoveryRoot(t)

	for i, tc := range []struct {
		name      string
		installed bool
	}{
		{"with the installed binary in place", true},
		{"with the installed binary hidden", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := stageRecovery(t, "upgrade-"+string(rune('a'+i)), hostupgrade.StepStaged, time.Now())

			journal, err := hostupgrade.ReadJournal(dir)
			if err != nil {
				t.Fatalf("read the journal: %v", err)
			}

			installed := filepath.Join(t.TempDir(), "billet")
			if tc.installed {
				if err := os.WriteFile(installed, []byte("binary"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			withInstalledBinary(t, installed)

			want := filepath.Join(dir, "preserved", "billet")
			if tc.installed {
				want = installed
			}

			var out bytes.Buffer

			env := cli.Env{Stdout: &out, Stderr: &out, Getenv: func(string) string { return "" }}
			host := unwritableJournalHost{t: t, dir: dir}

			err = finishHostUpgrade(t.Context(), env, root, journal, host, "/etc/billet/billet.yaml")
			if !errors.Is(err, hostupgrade.ErrUnrecorded) {
				t.Fatalf("finish returned %v, want ErrUnrecorded", err)
			}

			for _, line := range []string{
				"The failure itself could not be written to the journal.",
				"  " + want + " host-upgrade --resume --config /etc/billet/billet.yaml\n",
				"may finish the\nupgrade rather than roll it back",
			} {
				if !strings.Contains(out.String(), line) {
					t.Errorf("the cordon lacks %q:\n%s", line, out.String())
				}
			}
		})
	}
}

// A FULL DISK IS SAID AS THE ROOM TO FREE, AND A MISSING BINARY AS MISSING: the same
// full disk can stop the preserved copy being made, so neither file is named as if
// it were there.
func TestAFullDiskAndAMissingBinaryAreSaid(t *testing.T) {
	withInstalledBinary(t, filepath.Join(t.TempDir(), "billet"))

	journal := &hostupgrade.Journal{Dir: filepath.Join(t.TempDir(), "upgrade-1"), FromVersion: "v0.40.0"}
	full := &fs.PathError{Op: "write", Path: journal.Dir + "/journal.json.new", Err: syscall.ENOSPC}
	err := errors.Join(hostupgrade.ErrCordoned, hostupgrade.ErrUnrecorded, full)

	var out bytes.Buffer

	sayUnrecordedRecovery(cli.Env{Stdout: &out}, journal, "/etc/billet/billet.yaml", err)

	for _, line := range []string{
		"The filesystem holding " + journal.Dir + " is full. Free room on it first",
		"Neither " + installedBinary + " nor " + filepath.Join(journal.Dir, "preserved", "billet") + " is on the disk",
		"(v0.40.0)",
	} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("the report lacks %q:\n%s", line, out.String())
		}
	}
}
