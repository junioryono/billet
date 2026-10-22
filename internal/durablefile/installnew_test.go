package durablefile

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// InstallNew PUBLISHES ONCE AND NEVER REPLACES: the first install lands whole,
// a second fails matching fs.ErrExist and leaves the first untouched, and
// neither leaves the staging name behind, which a link (unlike a rename) would
// if the staged name were not removed.
//
// Mutations: publishing with the rename replaces the first file; skipping the
// staged name's removal leaves a .durable-* beside it; flushing the directory
// before the link records a sync that commits nothing.
func TestInstallNewPublishesOnceAndLeavesNoStagingName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	var steps []string
	i := Installer{
		Link: func(from, to string) error {
			steps = append(steps, "link")

			return os.Link(from, to)
		},
		SyncDir: func(d string) error {
			steps = append(steps, "sync")

			// THE FLUSH COMMITS THE REMOVAL TOO, so the staging name is gone
			// by the time it runs.
			entries, err := os.ReadDir(d)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if IsStagingName(e.Name()) {
					t.Errorf("the directory was flushed with %s still in it", e.Name())
				}
			}

			return syncDirectory(d)
		},
	}

	write := func(text string) func(io.Writer) error {
		return func(w io.Writer) error {
			_, err := io.WriteString(w, text)

			return err
		}
	}

	path, err := i.InstallNew(dir, "RECORD", 0o644, write("first\n"))
	if err != nil {
		t.Fatalf("InstallNew: %v", err)
	}
	if len(steps) != 2 || steps[0] != "link" || steps[1] != "sync" {
		t.Errorf("the install did %v, want the link and then the directory flush", steps)
	}

	_, err = i.InstallNew(dir, "RECORD", 0o644, write("second\n"))
	if !errors.Is(err, fs.ErrExist) {
		t.Errorf("a second install answered %v, want an error matching fs.ErrExist", err)
	}

	if got, err := os.ReadFile(path); err != nil || string(got) != "first\n" {
		t.Errorf("the record holds %q (%v), want the first install's", got, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("the record's mode is %v (%v), want 0644", info.Mode().Perm(), err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the directory holds %v, want only the record", names)
	}
}
