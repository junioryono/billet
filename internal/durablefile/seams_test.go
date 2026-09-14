package durablefile

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// The fixtures PR 6b commit 3 adds for the record writer's dependency on this
// package: the two unexported seams (D2, D4, D5), the complete sequence with
// the close in it (D3), and the structural proof that the DEFAULT sync closures
// call Sync (D1), because the recording fixture replaces the file-sync default
// and nothing else here would see a deleted Sync.

// D1: THE DEFAULTS ARE THE REAL THING. The default file-sync closure's body
// calls Sync on the file it is handed, and syncDirectory calls Sync on the
// directory it opened; a recording fixture that replaces the seam proves the
// order and nothing about the defaults.
func TestTheDefaultSyncSeamsCallSync(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "durablefile.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	callsSync := func(body ast.Node) bool {
		found := false

		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Sync" {
				found = true
			}

			return true
		})

		return found
	}

	var fileDefault, dirDefault bool

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}

		switch fn.Name.Name {
		case "syncFile":
			// The default is the closure returned last: `return func(f *os.File)
			// error { return f.Sync() }`.
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if lit, ok := n.(*ast.FuncLit); ok && callsSync(lit.Body) {
					fileDefault = true
				}

				return true
			})
		case "syncDirectory":
			dirDefault = callsSync(fn.Body)
		}
	}

	if !fileDefault {
		t.Error("syncFile's default closure does not call Sync on the file")
	}

	if !dirDefault {
		t.Error("syncDirectory does not call Sync on the directory it opened")
	}

	// AND THE ZERO-VALUE INSTALLER DISPATCHES TO THEM: a Sync on a closed file
	// and a flush of a directory that does not exist each answer an error,
	// where a no-op default would answer nil.
	closed, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}

	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}

	if err := (Installer{}).syncFile()(closed); !errors.Is(err, os.ErrClosed) {
		t.Errorf("the default file-sync seam on a closed file answered %v, want the Sync error", err)
	}

	if err := (Installer{}).syncDir()(t.TempDir() + "/absent"); err == nil {
		t.Error("the default directory-sync seam on an absent directory answered nil")
	}
}

// D3: THE COMPLETE SEQUENCE, THE CLOSE INCLUDED: create, write, mode, sync,
// close, rename, directory sync, through the unexported seams beside the
// exported ones.
func TestTheStagedFileIsClosedBetweenItsSyncAndItsRename(t *testing.T) {
	// NOT PARALLEL: the package seams are process-global.
	var order []string

	savedCreate, savedClose := createStaged, closeStaged
	t.Cleanup(func() { createStaged, closeStaged = savedCreate, savedClose })

	createStaged = func(dir string) (*os.File, error) {
		order = append(order, "create")

		return os.CreateTemp(dir, ".durable-*")
	}
	closeStaged = func(f *os.File) error {
		order = append(order, "close")

		return f.Close()
	}

	i := recording(&order)

	if _, err := i.Install(t.TempDir(), "kernel", 0o644, func(w io.Writer) error {
		order = append(order, "write")

		_, err := io.WriteString(w, "bytes")

		return err
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	want := []string{"create", "write", "setmode", "syncfile", "close", "rename", "syncdir"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("the steps ran %v, want %v", order, want)
	}
}

// D4: THE PRODUCTION CLOSE CLOSES BEFORE THE RENAME, with the default
// unchanged: the write callback keeps the staged file, and the rename seam
// requires that file to answer ErrClosed before it renames. A default close
// that was a successful no-op would leave it writable here.
func TestTheProductionCloseClosesBeforeTheRename(t *testing.T) {
	t.Parallel()

	var staged *os.File

	i := Installer{
		Rename: func(from, to string) error {
			if _, err := staged.WriteString("late"); !errors.Is(err, os.ErrClosed) {
				return errors.New("the staged file was still open at the rename: " + errString(err))
			}

			return os.Rename(from, to)
		},
	}

	dir := t.TempDir()

	path, err := i.Install(dir, "kernel", 0o644, func(w io.Writer) error {
		f, ok := w.(*os.File)
		if !ok {
			return errors.New("the writer is not the staged file")
		}

		staged = f
		_, err := io.WriteString(w, "bytes")

		return err
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	if body, err := os.ReadFile(path); err != nil || string(body) != "bytes" {
		t.Errorf("the installed file holds %q, %v", body, err)
	}
}

func errString(err error) string {
	if err == nil {
		return "no error"
	}

	return err.Error()
}

// D2: A CLOSE THAT FAILS IS A FAILED INSTALL: reported, the previous file
// intact, no temporary left.
func TestAFailedCloseLeavesThePreviousFileAndNoTemporary(t *testing.T) {
	// NOT PARALLEL: the package seam is process-global.
	boom := errors.New("close refused")

	saved := closeStaged
	t.Cleanup(func() { closeStaged = saved })

	closeStaged = func(f *os.File) error {
		_ = f.Close()

		return boom
	}

	dir := t.TempDir()
	if err := os.WriteFile(dir+"/kernel", []byte("previous"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := (Installer{}).Install(dir, "kernel", 0o644, write("next")); !errors.Is(err, boom) {
		t.Fatalf("Install returned %v, want the close failure", err)
	}

	assertOnlyEntries(t, dir, "kernel")

	if body, err := os.ReadFile(dir + "/kernel"); err != nil || string(body) != "previous" {
		t.Errorf("the previous file reads %q, %v", body, err)
	}
}

// D5: A CREATE THAT FAILS IS REPORTED, and nothing is written anywhere.
func TestAFailedCreateIsReportedAndWritesNothing(t *testing.T) {
	// NOT PARALLEL: the package seam is process-global.
	boom := &fs.PathError{Op: "open", Err: errors.New("no space left on device")}

	saved := createStaged
	t.Cleanup(func() { createStaged = saved })

	createStaged = func(string) (*os.File, error) { return nil, boom }

	dir := t.TempDir()
	if err := os.WriteFile(dir+"/kernel", []byte("previous"), 0o644); err != nil {
		t.Fatal(err)
	}

	written := false

	_, err := (Installer{}).Install(dir, "kernel", 0o644, func(io.Writer) error {
		written = true

		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Install returned %v, want the create failure", err)
	}

	if written {
		t.Error("the callback wrote into nothing")
	}

	assertOnlyEntries(t, dir, "kernel")
}
