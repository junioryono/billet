package launchd

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/lifeops"
)

// A MISSING DIRECTORY THIS ACCOUNT CAN CREATE IS PLANNED, NOT REFUSED (#181):
// a fresh account has no ~/Library/LaunchAgents, and a refusal saying `mkdir -p`
// asked the operator to type what billet could do. What the account cannot
// create, and what exists as something other than a directory, still refuses;
// the private directories are planned 0700.
//
// Mutations: dropping the creatable branch turns the planned cases into
// refusals; a creatableBy that always answers yes plans the read-only parent's
// child; a dirMode that ignores the request plans the state directory 0755.
func TestUpPlansTheDirectoriesThisAccountCanCreate(t *testing.T) {
	t.Parallel()

	if os.Getuid() == 0 {
		t.Skip("root writes a read-only directory, so the refusal cannot be staged")
	}

	home := t.TempDir()
	agents := filepath.Join(home, "Library", "LaunchAgents")
	logs := filepath.Join(home, "logs")
	state := filepath.Join(home, "state", "node")

	locked := filepath.Join(home, "locked")
	mustMkdir(t, locked, 0o500)
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	lockDir := filepath.Join(locked, "lock")

	file := filepath.Join(home, "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	c := &Converger{agentsDir: agents, logDir: logs}

	refusals, dirs := c.hostRefusals(lifeops.UpRequest{
		NodeStateDir: state,
		NodeLockDir:  lockDir,
		BinaryDir:    file,
	})

	want := map[string]fs.FileMode{agents: 0o755, logs: 0o755, state: 0o700}
	got := map[string]fs.FileMode{}
	for _, d := range dirs {
		got[d.Path] = d.Mode
	}
	if len(got) != len(want) {
		t.Errorf("planned %v, want %v", got, want)
	}
	for path, mode := range want {
		if got[path] != mode {
			t.Errorf("%s planned as %04o, want %04o (planned: %v)", path, got[path], mode, got)
		}
	}

	var refused []string
	for _, r := range refusals {
		refused = append(refused, r.What)
	}
	all := strings.Join(refused, "\n")
	for _, path := range []string{lockDir, file} {
		if !strings.Contains(all, path) {
			t.Errorf("%s was not refused:\n%s", path, all)
		}
	}
	if len(refusals) != 2 {
		t.Errorf("%d refusals, want 2:\n%s", len(refusals), all)
	}
}

// ApplyDirectories MAKES EVERY MISSING COMPONENT, WITH THE PLANNED MODE, AND A
// SECOND RUN IS A NO-OP; a component that is a file is refused, never created
// through.
func TestApplyDirectoriesCreatesThePlannedDirectories(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	deep := filepath.Join(home, "a", "b", "node")

	c := &Converger{}
	for range 2 {
		if err := c.ApplyDirectories([]lifeops.DirectoryCreation{{Path: deep, Mode: 0o700}}); err != nil {
			t.Fatalf("ApplyDirectories: %v", err)
		}
	}

	for _, p := range []string{filepath.Join(home, "a"), filepath.Join(home, "a", "b"), deep} {
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("%s was not created: %v", p, err)
		}
		if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is %v, want a directory no one else can open", p, info.Mode())
		}
	}

	file := filepath.Join(home, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{file, filepath.Join(file, "under")} {
		err := c.ApplyDirectories([]lifeops.DirectoryCreation{{Path: p, Mode: 0o755}})
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("%s, a file or a path through one, was not refused: %v", p, err)
		}
	}
}

func mustMkdir(t *testing.T, dir string, mode fs.FileMode) {
	t.Helper()

	if err := os.Mkdir(dir, mode); err != nil {
		t.Fatal(err)
	}
}
