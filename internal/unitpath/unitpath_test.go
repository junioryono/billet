package unitpath

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExercisePerformsEachAccessAndRefusesWhatItCannotDo(t *testing.T) {
	dir := t.TempDir()

	config := filepath.Join(dir, "billet.yaml")
	if err := os.WriteFile(config, []byte("node: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	readOnly := filepath.Join(dir, "read-only")
	if err := os.Mkdir(readOnly, 0o555); err != nil {
		t.Fatal(err)
	}

	if os.Geteuid() == 0 {
		t.Skip("root writes a 0555 directory, so the refusal this stages needs an unprivileged run")
	}

	taken := false

	for _, tc := range []struct {
		name   string
		need   Need
		marker string
		want   string
	}{
		{"a file it reads", Need{Path: config, Access: Read}, "", ""},
		{"a file that is not there", Need{Path: filepath.Join(dir, "absent"), Access: Read}, "", "read"},
		{"a directory it writes", Need{Path: filepath.Join(dir, "kernels"), Access: Write}, "billet-unit-paths-gate", ""},
		{"a directory it cannot write", Need{Path: readOnly, Access: Write}, "billet-unit-paths-gate", "write in"},
		{"its private staging", Need{Path: filepath.Join(dir, "tmp", "billet-images"), Access: Private}, "", ""},
		{"a lock it takes", Need{Path: filepath.Join(dir, "x.lock"), Access: Lock, Take: func(context.Context) error {
			taken = true

			return nil
		}}, "", ""},
		{"a lock it cannot take", Need{Path: filepath.Join(dir, "y.lock"), Access: Lock, Take: func(context.Context) error {
			return errors.New("read-only file system")
		}}, "", "read-only file system"},
		{"a lock with no way to take it", Need{Path: filepath.Join(dir, "z.lock"), Access: Lock}, "", "declares no way"},
		{"a path it could not derive", Need{Path: dir, Access: Lock, Unknown: "no identity yet"}, "", "no identity yet"},
	} {
		err := Exercise(t.Context(), tc.need, tc.marker)

		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: got %v, want a refusal saying %q", tc.name, err, tc.want)
		}
	}

	if !taken {
		t.Error("the lock's own Take was never called")
	}

	if _, err := os.Stat(filepath.Join(dir, "kernels", "billet-unit-paths-gate")); err != nil {
		t.Errorf("the named marker was not left where the write landed: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, "tmp", "billet-images"))
	if err != nil || len(entries) != 0 {
		t.Errorf("an unnamed marker was left behind (%v): %v", err, entries)
	}

	if err := Exercise(t.Context(), Need{Path: dir, Access: Lock, Unknown: "x"}, ""); !errors.Is(err, ErrUnknown) {
		t.Errorf("an underived path is not ErrUnknown: %v", err)
	}
}

// A MARKER NEVER WRITES OVER ANYTHING: not an existing file at its name, not
// through a link at its name, and never a name a command keeps.
func TestAMarkerWritesOverNothing(t *testing.T) {
	dir := t.TempDir()

	existing := filepath.Join(dir, "billet-unit-paths-gate")
	if err := os.WriteFile(existing, []byte("the ledger"), 0o600); err != nil {
		t.Fatal(err)
	}

	elsewhere := filepath.Join(t.TempDir(), "kept")
	if err := os.WriteFile(elsewhere, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(elsewhere, filepath.Join(dir, "billet-unit-paths-linked")); err != nil {
		t.Fatal(err)
	}

	for _, marker := range []string{"billet-unit-paths-gate", "billet-unit-paths-linked", "billet.db", "../billet-unit-paths-x"} {
		if err := Exercise(t.Context(), Need{Path: dir, Access: Write}, marker); err == nil {
			t.Errorf("the marker %q was written", marker)
		}
	}

	for path, want := range map[string]string{existing: "the ledger", elsewhere: "kept"} {
		if body, err := os.ReadFile(path); err != nil || string(body) != want {
			t.Errorf("%s now holds %q (%v), want %q", path, body, err, want)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "billet.db")); !os.IsNotExist(err) {
		t.Errorf("a marker named for a file a command keeps was created (%v)", err)
	}
}
