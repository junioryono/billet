package guestagent

import (
	"bytes"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"
)

// fixtureProc is testdata/proc, a kernel 6.1 guest's /proc in miniature, as a tree
// a test can change.
func fixtureProc(t *testing.T) fstest.MapFS {
	t.Helper()

	out := fstest.MapFS{}

	err := fs.WalkDir(os.DirFS("testdata/proc"), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		data, err := os.ReadFile(path.Join("testdata/proc", name))
		if err != nil {
			return err
		}

		out[name] = &fstest.MapFile{Data: data}

		return nil
	})
	if err != nil {
		t.Fatalf("read the fixture /proc: %v", err)
	}

	return out
}

// set replaces one number of the space-separated line of name that starts with
// prefix: field i of it, counted from 0.
func set(t *testing.T, proc fstest.MapFS, name, prefix string, i int, value string) {
	t.Helper()

	file, ok := proc[name]
	if !ok {
		t.Fatalf("the fixture has no %s", name)
	}

	lines := strings.Split(string(file.Data), "\n")
	for j, line := range lines {
		if !strings.HasPrefix(line, prefix) {
			continue
		}

		fields := strings.Fields(line)
		if i >= len(fields) {
			t.Fatalf("%s's %q line has no field %d", name, prefix, i)
		}

		fields[i] = value
		lines[j] = strings.Join(fields, " ")
		proc[name] = &fstest.MapFile{Data: []byte(strings.Join(lines, "\n"))}

		return
	}

	t.Fatalf("%s has no line starting %q", name, prefix)
}

// watchedFS is a /proc tree that records every name opened, fails the test when
// one is a process's cmdline or environ, and fails the names in fail with their
// errors.
type watchedFS struct {
	t    *testing.T
	fsys fs.FS

	mu     sync.Mutex
	opened []string
	fail   map[string]error
}

func watch(t *testing.T, fsys fs.FS) *watchedFS {
	t.Helper()

	return &watchedFS{t: t, fsys: fsys, fail: map[string]error{}}
}

func (w *watchedFS) Open(name string) (fs.File, error) {
	w.mu.Lock()
	w.opened = append(w.opened, name)
	err := w.fail[name]
	w.mu.Unlock()

	if base := path.Base(name); base == "cmdline" || base == "environ" {
		w.t.Errorf("the agent opened %s, which can hold a secret", name)
	}

	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}

	return w.fsys.Open(name)
}

func (w *watchedFS) sawOpen(name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, n := range w.opened {
		if n == name {
			return true
		}
	}

	return false
}

// eacces is the error a read of another user's /proc/<pid>/io gives.
var eacces = syscall.EACCES

// discardLog is a log the test reads afterwards.
func discardLog() (*limitedLog, *syncBuffer) {
	var b syncBuffer

	return newLimitedLog(&b), &b
}

// syncBuffer is a bytes.Buffer two goroutines may write.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.String()
}

// at is a guest clock reading, 2026-10-10T02:04:20Z plus offset.
func at(offset time.Duration) time.Time {
	return time.Date(2026, 10, 10, 2, 4, 20, 0, time.UTC).Add(offset)
}
