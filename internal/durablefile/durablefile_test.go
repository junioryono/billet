package durablefile

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// THE ORDER IS THE WHOLE CONTENT OF THIS PACKAGE, so it is what gets asserted.
//
// Each step read as fine on its own in the code this replaced: the file was synced,
// its mode was set, it was renamed. The sequence was wrong in two places — the mode
// landed AFTER the sync that would have committed it, and nothing flushed the
// directory at all — and neither is visible from the finished file.
func TestTheStepsHappenInTheOnlyOrderThatCommits(t *testing.T) {
	t.Parallel()

	var order []string

	i := recording(&order)

	dir := t.TempDir()

	if _, err := i.Install(dir, "kernel", 0o644, write("bytes")); err != nil {
		t.Fatalf("Install: %v", err)
	}

	want := []string{"setmode", "syncfile", "rename", "syncdir"}

	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("the steps ran %v, want %v", order, want)
	}
}

// THE CONTENT IS WRITTEN BEFORE ANY OF IT, because the callback is what decides
// whether the bytes are the right ones — a digest checked after the rename is a
// digest checked after the name has already been published.
func TestTheCallerWritesBeforeAnythingIsCommitted(t *testing.T) {
	t.Parallel()

	var order []string

	i := recording(&order)

	dir := t.TempDir()

	path, err := i.Install(dir, "kernel", 0o644, func(w io.Writer) error {
		order = append(order, "write")

		_, err := io.WriteString(w, "bytes")

		return err
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	if len(order) == 0 || order[0] != "write" {
		t.Errorf("the steps ran %v; the content is written first", order)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read what was installed: %v", err)
	}

	if string(body) != "bytes" {
		t.Errorf("the installed file holds %q", body)
	}
}

// A REFUSAL FROM THE CALLER PUBLISHES NOTHING, and the error comes back unchanged so
// a digest mismatch reads as a digest mismatch rather than as "cannot install".
func TestARefusedWriteLeavesNothingAndKeepsItsError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("the bytes are not what the manifest published")

	dir := t.TempDir()

	_, err := Installer{}.Install(dir, "kernel", 0o644, func(w io.Writer) error {
		if _, err := io.WriteString(w, "partial"); err != nil {
			return err
		}

		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Install returned %v, want the caller's own error", err)
	}

	assertOnlyEntries(t, dir)
}

// EVERY STEP UP TO THE RENAME LEAVES NOTHING BEHIND. A staged file under a name
// nothing recognises is still a file on a disk that fills up, and a later reader
// must never find a half-installed artifact under the real name.
func TestAFailureBeforeTheRenameLeavesNothing(t *testing.T) {
	t.Parallel()

	boom := errors.New("no")

	for _, tc := range []struct {
		name string
		fail func(*Installer)
	}{
		{"the mode change", func(i *Installer) {
			i.SetMode = func(*os.File, fs.FileMode) error { return boom }
		}},
		{"the file sync", func(i *Installer) {
			i.SyncFile = func(*os.File) error { return boom }
		}},
		{"the rename", func(i *Installer) {
			i.Rename = func(string, string) error { return boom }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var i Installer

			tc.fail(&i)

			dir := t.TempDir()

			path, err := i.Install(dir, "kernel", 0o644, write("bytes"))
			if !errors.Is(err, boom) {
				t.Fatalf("Install returned %v, want the injected failure", err)
			}

			if path != "" {
				t.Errorf("Install returned %q for an install that did not happen", path)
			}

			assertOnlyEntries(t, dir)
		})
	}
}

// A DIRECTORY SYNC THAT FAILS IS A FAILED INSTALL, and the file STAYS.
//
// The two halves are deliberate. It is a failure because the entry may not survive a
// crash, and the caller must not go on to publish something remote that names it.
// The file stays because the entry may equally well be durable already, an unlink is
// no more durable than the rename was, and the retry re-verifies the content and
// flushes the directory again — which is the path that repairs this state.
func TestAFailedDirectorySyncIsAFailureThatKeepsTheFile(t *testing.T) {
	t.Parallel()

	boom := errors.New("no")

	i := Installer{SyncDir: func(string) error { return boom }}

	dir := t.TempDir()

	if _, err := i.Install(dir, "kernel", 0o644, write("bytes")); !errors.Is(err, boom) {
		t.Fatalf("Install returned %v, want the injected failure", err)
	}

	assertOnlyEntries(t, dir, "kernel")
}

// THE MODE IS THE ONE ASKED FOR, WHATEVER THE UMASK. A staged file is created 0600,
// and a guest that cannot read its kernel is the same outage as one whose kernel is
// missing.
func TestTheInstalledModeIsTheOneAskedFor(t *testing.T) {
	// NOT PARALLEL: umask is process-global.
	old := setUmask(0o077)
	t.Cleanup(func() { setUmask(old) })

	dir := t.TempDir()

	path, err := Installer{}.Install(dir, "kernel", 0o644, write("bytes"))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the installed file: %v", err)
	}

	if info.Mode().Perm() != 0o644 {
		t.Errorf("the installed file is %v, want 0644", info.Mode().Perm())
	}
}

// A NAME THAT IS NOT A NAME IS REFUSED. The caller composes the path, so a separator
// here writes outside the directory it chose.
func TestANameThatIsAPathIsRefused(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"", "sub/kernel", "../kernel", "."} {
		if _, err := (Installer{}).Install(t.TempDir(), name, 0o644, write("x")); err == nil {
			t.Errorf("%q was accepted as a file name", name)
		}
	}
}

// SyncDirectory IS THE REPAIR PATH, so it has to reach the real fsync and report a
// directory it cannot open rather than shrugging.
func TestSyncDirectoryReportsADirectoryItCannotFlush(t *testing.T) {
	t.Parallel()

	if err := (Installer{}).SyncDirectory(filepath.Join(t.TempDir(), "not-here")); err == nil {
		t.Fatal("flushing a directory that does not exist reported success")
	}

	if err := (Installer{}).SyncDirectory(t.TempDir()); err != nil {
		t.Errorf("flushing a real directory failed: %v", err)
	}
}

func recording(order *[]string) Installer {
	return Installer{
		SetMode: func(f *os.File, mode fs.FileMode) error {
			*order = append(*order, "setmode")

			return f.Chmod(mode)
		},
		SyncFile: func(f *os.File) error {
			*order = append(*order, "syncfile")

			return f.Sync()
		},
		Rename: func(from, to string) error {
			*order = append(*order, "rename")

			return os.Rename(from, to)
		},
		SyncDir: func(dir string) error {
			*order = append(*order, "syncdir")

			return syncDirectory(dir)
		},
	}
}

func write(body string) func(io.Writer) error {
	return func(w io.Writer) error {
		_, err := io.WriteString(w, body)

		return err
	}
}

// assertOnlyEntries names every file in dir, so "nothing was left behind" means the
// staged file too rather than only the final name.
func assertOnlyEntries(t *testing.T, dir string, want ...string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Name())
	}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s holds %v, want %v", dir, got, want)
	}
}

// FLUSHING A DIRECTORY DOES NOT COMMIT THE DIRECTORY, which is the same defect one
// level up from the one this package was written for.
//
// Install flushes the entries INSIDE dir, which is what makes the file it renamed
// durable. That says nothing about the entry naming dir in its PARENT — so a
// directory this operation created can vanish, taking the file with it, while every
// step of the install reported success.
func TestMkdirAllCommitsEveryAncestorItCreated(t *testing.T) {
	t.Parallel()

	var flushed []string

	i := Installer{
		SyncDir: func(dir string) error {
			flushed = append(flushed, dir)

			return syncDirectory(dir)
		},
	}

	root := t.TempDir()
	target := filepath.Join(root, "var", "lib", "billet", "kernels")

	if err := i.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("stat %s: %v", target, err)
	}

	// EVERY LEVEL FROM THE PARENT UPWARD. Committing the name of `kernels` means
	// flushing `billet`, and committing `billet`'s name means flushing `lib`, and so
	// on: the directory is no more durable than the shallowest entry nothing flushed.
	for at := filepath.Dir(target); ; {
		if !slices.Contains(flushed, at) {
			t.Errorf("%s was never flushed, so an entry below it may not survive (%v)",
				at, flushed)
		}

		parent := filepath.Dir(at)
		if parent == at || at == root {
			break
		}

		at = parent
	}
}

// AND THE PHYSICAL ANCESTORS TOO, WHEN A COMPONENT IS A SYMLINK.
//
// os.MkdirAll FOLLOWS symlinked components while a lexical walk climbs the NAME. A
// deployment whose kernel directory sits on another volume — /var/lib/billet a link
// to /mnt/vol/billet, which is the ordinary way a large artifact directory is moved
// — would have its lexical parents flushed while the entries that actually name the
// new directory, under /mnt/vol, were never committed. The directory can then vanish
// with the kernel in it, which is the whole failure this operation exists to close.
func TestMkdirAllCommitsThePhysicalAncestorsBehindASymlink(t *testing.T) {
	t.Parallel()

	var flushed []string

	i := Installer{
		SyncDir: func(dir string) error {
			flushed = append(flushed, dir)

			return syncDirectory(dir)
		},
	}

	root := t.TempDir()

	// The physical hierarchy, on its own "volume".
	volume := filepath.Join(root, "mnt", "vol")
	if err := os.MkdirAll(volume, 0o755); err != nil {
		t.Fatalf("make the volume: %v", err)
	}

	// And the name a deployment uses, which crosses into it.
	lexical := filepath.Join(root, "var", "lib")
	if err := os.MkdirAll(lexical, 0o755); err != nil {
		t.Fatalf("make the lexical parent: %v", err)
	}

	link := filepath.Join(lexical, "billet")
	if err := os.Symlink(volume, link); err != nil {
		t.Skipf("this filesystem does not do symlinks: %v", err)
	}

	target := filepath.Join(link, "kernels")

	if err := i.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// COMPARED BY DIRECTORY IDENTITY, NOT BY THE STRING THAT WAS PASSED. Opening a
	// path FOLLOWS its symlinks, so flushing ".../var/lib/billet" IS an fsync of
	// ".../mnt/vol" — the physical directory — and an assertion on the spelling
	// would demand a second, redundant flush of a directory already committed.
	// t.TempDir is itself behind /var -> /private/var on darwin, which is a second
	// reason the raw strings cannot be compared.
	committed := resolvedSet(t, flushed)

	// THE PHYSICAL GRANDPARENT IS WHAT THE LEXICAL WALK CANNOT REACH. It climbs the
	// NAME, so it goes .../var/lib/billet then .../var/lib and never visits .../mnt
	// — where the entry naming the volume itself lives.
	for _, want := range []string{
		filepath.Dir(volume), // .../mnt, reachable only through the resolved walk
		volume,               // .../mnt/vol, which holds the new directory
		lexical,              // .../var/lib, which holds the symlink
	} {
		resolved, err := filepath.EvalSymlinks(want)
		if err != nil {
			t.Fatalf("resolve %s: %v", want, err)
		}

		if !committed[resolved] {
			t.Errorf("%s (%s) was never flushed, so an entry it holds is not durable (%v)",
				want, resolved, flushed)
		}
	}
}

// AND NO DIRECTORY IS FLUSHED TWICE. The two walks meet below the symlink, and a
// redundant second fsync that failed would reject an operation whose directory the
// first fsync had already committed — a refusal of correct state.
func TestMkdirAllFlushesEachDirectoryOnce(t *testing.T) {
	t.Parallel()

	var flushed []string

	i := Installer{
		SyncDir: func(dir string) error {
			flushed = append(flushed, dir)

			return syncDirectory(dir)
		},
	}

	root := t.TempDir()

	volume := filepath.Join(root, "mnt", "vol")
	if err := os.MkdirAll(volume, 0o755); err != nil {
		t.Fatalf("make the volume: %v", err)
	}

	lexical := filepath.Join(root, "var", "lib")
	if err := os.MkdirAll(lexical, 0o755); err != nil {
		t.Fatalf("make the lexical parent: %v", err)
	}

	link := filepath.Join(lexical, "billet")
	if err := os.Symlink(volume, link); err != nil {
		t.Skipf("this filesystem does not do symlinks: %v", err)
	}

	if err := i.MkdirAll(filepath.Join(link, "kernels"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	seen := map[string]string{}

	for _, dir := range flushed {
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatalf("resolve %s: %v", dir, err)
		}

		if first, dup := seen[resolved]; dup {
			t.Errorf("%s was flushed twice, as %s and as %s; a redundant fsync that failed "+
				"would reject an operation whose directory was already committed",
				resolved, first, dir)
		}

		seen[resolved] = dir
	}
}

// resolvedSet is every directory a flush actually reached, by identity.
func resolvedSet(t *testing.T, dirs []string) map[string]bool {
	t.Helper()

	out := map[string]bool{}

	for _, dir := range dirs {
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatalf("resolve %s: %v", dir, err)
		}

		out[resolved] = true
	}

	return out
}

// AND AN EXISTING DIRECTORY IS FLUSHED TOO, which is the retry that repairs a
// previous run that created it and died before its parent was committed. Skipping
// the flush because "it is already there" would make the retry certify that state.
func TestMkdirAllFlushesADirectoryThatAlreadyExists(t *testing.T) {
	t.Parallel()

	var flushed []string

	i := Installer{
		SyncDir: func(dir string) error {
			flushed = append(flushed, dir)

			return nil
		},
	}

	root := t.TempDir()
	target := filepath.Join(root, "kernels")

	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("pre-create: %v", err)
	}

	if err := i.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	if !slices.Contains(flushed, root) {
		t.Errorf("an existing directory's parent was not flushed (%v); a retry after a "+
			"crash between the mkdir and the flush would then certify it", flushed)
	}
}

// A FLUSH THAT FAILS IS A FAILED CREATE, because the caller is about to publish
// something that names what is inside this directory.
func TestMkdirAllReportsAFlushItCouldNotDo(t *testing.T) {
	t.Parallel()

	boom := errors.New("no")

	i := Installer{SyncDir: func(string) error { return boom }}

	if err := i.MkdirAll(filepath.Join(t.TempDir(), "kernels"), 0o755); !errors.Is(err, boom) {
		t.Fatalf("MkdirAll returned %v, want the injected failure", err)
	}
}

// SetModeOn AND SyncFileHandle ARE THE SAME STEPS Install USES, exposed for a caller
// repairing an artifact rather than installing one — so a repair path cannot become
// a second implementation of this package's ordering, and a test that fails one for
// Install fails it there too.
func TestTheExposedStepsGoThroughTheSameSeams(t *testing.T) {
	t.Parallel()

	var saw []string

	i := Installer{
		SetMode: func(*os.File, fs.FileMode) error {
			saw = append(saw, "setmode")

			return nil
		},
		SyncFile: func(*os.File) error {
			saw = append(saw, "syncfile")

			return nil
		},
	}

	f, err := os.Create(filepath.Join(t.TempDir(), "f"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	t.Cleanup(func() { _ = f.Close() })

	if err := i.SetModeOn(f, 0o644); err != nil {
		t.Fatalf("SetModeOn: %v", err)
	}

	if err := i.SyncFileHandle(f); err != nil {
		t.Fatalf("SyncFileHandle: %v", err)
	}

	if strings.Join(saw, ",") != "setmode,syncfile" {
		t.Errorf("the exposed steps ran %v rather than going through the installer's seams",
			saw)
	}
}
