package unitpath

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExercisePerformsEachAccessAndRefusesWhatItCannotDo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a 0555 directory, so the refusal this stages needs an unprivileged run")
	}

	dir := t.TempDir()

	config := filepath.Join(dir, "billet.yaml")
	if err := os.WriteFile(config, []byte("node: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	readOnly := filepath.Join(dir, "read-only")
	if err := os.Mkdir(readOnly, 0o555); err != nil {
		t.Fatal(err)
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
		{"a directory it creates and writes", Need{Path: filepath.Join(dir, "kernels"), Access: Write, Create: true}, "billet-unit-paths-gate", ""},
		{"a directory it does not create, absent", Need{Path: filepath.Join(dir, "identity"), Access: Write}, "billet-unit-paths-gate", "write in"},
		{"a directory it cannot write", Need{Path: readOnly, Access: Write}, "billet-unit-paths-gate", "write in"},
		{"its private staging", Need{Path: filepath.Join(dir, "tmp", "billet-images"), Access: Private}, "", ""},
		{"a lock it takes", Need{Path: filepath.Join(dir, "x.lock"), Access: Lock, Take: func(context.Context) (func() error, error) {
			taken = true

			return NothingHeld, nil
		}}, "", ""},
		{"a lock it cannot take", Need{Path: filepath.Join(dir, "y.lock"), Access: Lock, Take: func(context.Context) (func() error, error) {
			return nil, errors.New("read-only file system")
		}}, "", "read-only file system"},
		{"a lock with no way to take it", Need{Path: filepath.Join(dir, "z.lock"), Access: Lock}, "", "declares no way"},
		{"a path it could not derive", Need{Path: dir, Access: Write, Unknown: "no identity yet"}, "", "no identity yet"},
		{"a directory with no name", Need{Path: "", Access: Write}, "billet-unit-paths-gate", "no name"},
	} {
		_, err := Exercise(t.Context(), tc.need, tc.marker)

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

	// A DIRECTORY THE COMMAND DOES NOT CREATE IS NOT CREATED by its exercise: an
	// identity directory a retirement moved stays moved.
	if _, err := os.Stat(filepath.Join(dir, "identity")); !os.IsNotExist(err) {
		t.Errorf("an exercise created a directory its command does not create (%v)", err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, "tmp", "billet-images"))
	if err != nil || len(entries) != 0 {
		t.Errorf("an unnamed marker was left behind (%v): %v", err, entries)
	}

	if _, err := Exercise(t.Context(), Need{Path: "", Access: Write}, "billet-unit-paths-gate"); !errors.Is(err, ErrUnknown) {
		t.Errorf("a directory with no name is not could-not-tell: %v", err)
	}

	if _, err := Exercise(t.Context(), Need{Path: dir, Access: Lock, Unknown: "x"}, ""); !errors.Is(err, ErrUnknown) {
		t.Errorf("an underived path is not ErrUnknown: %v", err)
	}
}

// A LOCK THE COMMAND HOLDS ACROSS ITS WORK IS HELD ACROSS THE EXERCISE OF IT,
// released last and in reverse; and an exclusion that is refused admits nothing
// after it.
func TestExerciseAllHoldsWhatAdmitsTheRestAndStopsWhenRefused(t *testing.T) {
	dir := t.TempDir()

	var events []string

	hold := func(name string) func(context.Context) (func() error, error) {
		return func(context.Context) (func() error, error) {
			events = append(events, "take "+name)

			return func() error {
				events = append(events, "release "+name)

				return nil
			}, nil
		}
	}

	needs := []Need{
		{Path: "authority", Access: Lock, Admits: true, Take: hold("authority")},
		{Path: "inner", Access: Lock, Take: hold("inner")},
		{Path: dir, Access: Write, Take: nil},
	}

	var reported []string

	err := ExerciseAll(t.Context(), needs, "billet-unit-paths-gate", func(n Need, err error) {
		reported = append(reported, n.Path)
		if n.Access == Write {
			events = append(events, "write")
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(events, ", "); got != "take authority, take inner, write, release inner, release authority" {
		t.Errorf("the exercise ran %s", got)
	}

	if len(reported) != 3 {
		t.Errorf("reported %v", reported)
	}

	// REFUSED, THE EXCLUSION ADMITS NOTHING: the directory it guards is not
	// written, and the refusal says why.
	guarded := filepath.Join(dir, "guarded")
	if err := os.Mkdir(guarded, 0o755); err != nil {
		t.Fatal(err)
	}

	err = ExerciseAll(t.Context(), []Need{
		{Path: "authority", Access: Lock, Admits: true, Take: func(context.Context) (func() error, error) {
			return nil, errors.New("the authority is closed")
		}},
		{Path: guarded, Access: Write},
	}, "billet-unit-paths-gate", func(Need, error) {})

	if !errors.Is(err, ErrNotAdmitted) || !strings.Contains(err.Error(), "the authority is closed") {
		t.Errorf("a refused exclusion answered %v", err)
	}

	if entries, err := os.ReadDir(guarded); err != nil || len(entries) != 0 {
		t.Errorf("a directory behind a refused exclusion was written (%v): %v", err, entries)
	}
}

func TestUniqueKeepsTheFirstOfEachPathAndAccess(t *testing.T) {
	got := Unique([]Need{
		{Path: "/var/lib/billet/node/", Access: Write, Create: true},
		{Path: "/etc/billet/tls/node.crt", Access: Read},
		{Path: "/var/lib/billet/node", Access: Write},
		{Path: "/var/lib/billet//node", Access: Read},
	})

	if len(got) != 3 || !got[0].Create || got[0].Path != "/var/lib/billet/node" ||
		got[2].Access != Read || got[2].Path != "/var/lib/billet//node" {
		t.Errorf("Unique answered %+v", got)
	}

	// A FILE IS OPENED AS SPELLED, and `..` after a link is not the link's
	// parent: neither is rewritten.
	kept := Unique([]Need{
		{Path: "/etc/billet/app-private-key.pem/", Access: Read},
		{Path: "/var/lib/link/../kernels", Access: Write},
	})

	if kept[0].Path != "/etc/billet/app-private-key.pem/" || kept[1].Path != "/var/lib/link/../kernels" {
		t.Errorf("Unique rewrote what it may not: %+v", kept)
	}
}

// A `..` AFTER A LINK NAMES TWO DIRECTORIES, the kernel's and a join's, and a
// command may write either: its exercise cannot tell which and refuses,
// writing in neither.
func TestADirectoryNamedWithDotDotIsCouldNotTell(t *testing.T) {
	base := t.TempDir()

	for _, dir := range []string{"real/child", "real/kernels", "kernels"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Symlink(filepath.Join(base, "real", "child"), filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}

	spelled := base + "/link/../kernels"

	need := Unique([]Need{{Path: spelled, Access: Write}})[0]
	if need.Path != spelled {
		t.Fatalf("Unique rewrote %s as %s", spelled, need.Path)
	}

	if _, err := Exercise(t.Context(), need, "billet-unit-paths-gate"); !errors.Is(err, ErrUnknown) {
		t.Errorf("a directory named through `..` was exercised as %v, want could-not-tell", err)
	}

	for _, dir := range []string{"real/kernels", "kernels"} {
		if _, err := os.Stat(filepath.Join(base, dir, "billet-unit-paths-gate")); !os.IsNotExist(err) {
			t.Errorf("a marker was written in %s (%v)", dir, err)
		}
	}
}

// A FIFO WHERE A DIRECTORY SHOULD BE IS REFUSED AT ONCE, never waited on: the
// open that would wait for a writer is one the exercise never makes.
func TestAFIFOWhereADirectoryShouldBeIsRefusedWithoutAWait(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "backups")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("this filesystem cannot make a FIFO: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := Exercise(t.Context(), Need{Path: fifo, Access: Write}, "billet-unit-paths-gate")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO was written in as a directory")
		}
	case <-time.After(10 * time.Second):
		// The writer this open releases the waiting exercise, so it ends before
		// the test does.
		if f, err := os.OpenFile(fifo, os.O_RDWR, 0); err == nil {
			<-done
			_ = f.Close()
		}

		t.Fatal("the exercise waited on a FIFO where a directory should be")
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
		if _, err := Exercise(t.Context(), Need{Path: dir, Access: Write}, marker); err == nil {
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
