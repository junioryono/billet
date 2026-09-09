package main

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/hostupgrade"
)

// COMMIT 4a's GUARD FIXTURES (scratchpad/pr6b/fixtures-c4a.md, G1 to G4):
// the candidate's flushes before the record, the guardSync seam and its
// structural witness, the role journal a takeover proves complete, and the
// status answers the gate corrupts, produced by the command itself. The
// package seams are process-global, so nothing here is parallel.

// G1 (C4-D1): a hold with a candidate flushes the candidate, its directory
// and the root, in that order, after the digest and before the guard
// directory exists; each flush on the object's own device and inode; a hold
// of the managed binary adds no pre-publication flush.
func TestAHoldWithACandidateFlushesItBeforeTheRecord(t *testing.T) {
	f := newGuardFixture(t)
	mustOK(t, os.Mkdir(f.root, 0o700))

	candidate := stageGuardCandidate(t, f, "recovery-20260909T120000-0badcafe", []byte("#!/bin/sh\nexit 0\n"))

	var ops []guardOp

	guardHook = func(op guardOp) error { ops = append(ops, op); return nil }

	mustOK(t, guardRun(t, "hold", "--holder", "ci-1", "--candidate", candidate))

	guardHook = nil

	var kinds []string

	for _, op := range ops {
		kinds = append(kinds, op.Kind+" "+strings.TrimPrefix(op.Path, f.parent+"/"))
	}

	hash := slices.Index(kinds, "hash upgrades/recovery-20260909T120000-0badcafe/billet.candidate")
	mkdir := slices.Index(kinds, "mkdir upgrades/active")

	if hash < 0 || mkdir < 0 || hash > mkdir {
		t.Fatalf("the hash and the guard's mkdir are at %d and %d in %q", hash, mkdir, kinds)
	}

	want := []string{
		"fsync upgrades/recovery-20260909T120000-0badcafe/billet.candidate",
		"fsync upgrades/recovery-20260909T120000-0badcafe",
		"fsync upgrades",
		"lstat upgrades/active",
	}

	if got := kinds[hash+1 : mkdir]; !reflect.DeepEqual(got, want) {
		t.Errorf("between the hash and the guard's mkdir:\n got %q\nwant %q", got, want)
	}

	// EVERY FLUSH IS ON THE OBJECT IT NAMES, by device and inode.
	for _, op := range ops[hash+1 : mkdir] {
		if op.Kind != "fsync" {
			continue
		}

		info, err := os.Lstat(op.Path)
		if err != nil {
			t.Fatalf("lstat %s: %v", op.Path, err)
		}

		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Ino != op.Ino || devOf(st) != op.Dev {
			t.Errorf("the fsync of %s ran on dev %d inode %d, not the object's %d %d", op.Path, op.Dev, op.Ino, st.Dev, st.Ino)
		}
	}

	// A HOLD OF THE MANAGED BINARY flushes nothing before the guard's mkdir.
	f2 := newGuardFixture(t)
	ops = nil
	guardHook = func(op guardOp) error { ops = append(ops, op); return nil }

	mustHold(t, "ci-1")

	guardHook = nil

	for _, op := range ops {
		if op.Kind == "fsync" && !strings.HasPrefix(op.Path, f2.root) && op.Path != f2.parent {
			t.Errorf("a hold of the managed binary flushed %s", op.Path)
		}

		if op.Kind == "mkdir" && strings.HasSuffix(op.Path, "/active") {
			break
		}

		if op.Kind == "fsync" && op.Path != f2.parent {
			t.Errorf("a hold of the managed binary flushed %s before the guard's mkdir", op.Path)
		}
	}
}

// G2: EACH OF THE THREE NEW FLUSHES CAN FAIL, at the Sync itself (the seam,
// not the observation), and the failure publishes nothing and names the
// object; a seam that keeps the call and discards its error is caught here.
func TestAFailedCandidateFlushPublishesNothing(t *testing.T) {
	for _, object := range []string{"billet.candidate", "recovery-20260909T120000-0badcafe", "upgrades"} {
		t.Run(object, func(t *testing.T) {
			f := newGuardFixture(t)
			mustOK(t, os.Mkdir(f.root, 0o700))

			candidate := stageGuardCandidate(t, f, "recovery-20260909T120000-0badcafe", []byte("#!/bin/sh\nexit 0\n"))
			injected := errors.New("staged sync failure")

			saved := guardSync
			guardSync = func(file *os.File) error {
				if filepath.Base(file.Name()) == object {
					return injected
				}

				return syncFD(file)
			}
			t.Cleanup(func() { guardSync = saved })

			err := guardRun(t, "hold", "--holder", "ci-1", "--candidate", candidate)
			if !errors.Is(err, injected) {
				t.Fatalf("a failed flush of %s: err = %v, want the injected failure", object, err)
			}

			if _, err := os.Lstat(f.active()); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("a failed flush of %s left active (lstat err = %v)", object, err)
			}
		})
	}
}

// G1's WITNESS, structural: every `guardObserve("fsync", ...)` in the guard's
// source is followed, in the same block, by a `guardSync(<the same
// descriptor>)` whose error is checked; the seam's default is syncFD; and
// syncFD's body is exactly a return of the descriptor's Sync. A Sync deleted
// under a kept observation, a default rewritten to return nil, or a discarded
// error each fail here.
func TestEveryFsyncObservationIsFollowedByTheSync(t *testing.T) {
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "convergeguard.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	observations := 0

	ast.Inspect(file, func(n ast.Node) bool {
		block, ok := n.(*ast.BlockStmt)
		if !ok {
			return true
		}

		for i, stmt := range block.List {
			call, descriptor, ok := fsyncObservation(stmt)
			if !ok {
				continue
			}

			observations++

			if i+1 >= len(block.List) {
				t.Errorf("%s: the fsync observation is the block's last statement", fset.Position(call.Pos()))

				continue
			}

			if !checkedSync(block.List[i+1], descriptor) {
				t.Errorf("%s: the fsync observation of %s is not followed by `if err := guardSync(%s); err != nil`",
					fset.Position(call.Pos()), descriptor, descriptor)
			}
		}

		return true
	})

	if observations < 5 {
		t.Errorf("only %d fsync observations found; the tmp, the guard, the root, the candidate and its directory are five", observations)
	}

	// The seam's default, and the default's body.
	var (
		defaultIsSyncFD bool
		syncFDBody      bool
	)

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "guardSync" || len(vs.Values) != 1 {
					continue
				}

				if id, ok := vs.Values[0].(*ast.Ident); ok && id.Name == "syncFD" {
					defaultIsSyncFD = true
				}
			}
		case *ast.FuncDecl:
			if d.Name.Name != "syncFD" || d.Body == nil || len(d.Body.List) != 1 {
				continue
			}

			ret, ok := d.Body.List[0].(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 {
				continue
			}

			if call, ok := ret.Results[0].(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Sync" && len(call.Args) == 0 {
					syncFDBody = true
				}
			}
		}
	}

	if !defaultIsSyncFD {
		t.Error("guardSync's default is not syncFD")
	}

	if !syncFDBody {
		t.Error("syncFD's body is not exactly `return f.Sync()`")
	}
}

// fsyncObservation recognises `if err := guardObserve("fsync", P, D); err != nil { return ... }`
// and answers the descriptor's identifier.
func fsyncObservation(stmt ast.Stmt) (*ast.CallExpr, string, bool) {
	ifs, ok := stmt.(*ast.IfStmt)
	if !ok || ifs.Init == nil {
		return nil, "", false
	}

	assign, ok := ifs.Init.(*ast.AssignStmt)
	if !ok || len(assign.Rhs) != 1 {
		return nil, "", false
	}

	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok || len(call.Args) != 3 {
		return nil, "", false
	}

	fn, ok := call.Fun.(*ast.Ident)
	if !ok || fn.Name != "guardObserve" {
		return nil, "", false
	}

	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Value != `"fsync"` {
		return nil, "", false
	}

	// The descriptor may be a name or a field (`root.dir`); its source text is
	// what the sync must name.
	return call, types.ExprString(call.Args[2]), true
}

// checkedSync recognises `if err := guardSync(D); err != nil { ... }`.
func checkedSync(stmt ast.Stmt, descriptor string) bool {
	ifs, ok := stmt.(*ast.IfStmt)
	if !ok || ifs.Init == nil {
		return false
	}

	assign, ok := ifs.Init.(*ast.AssignStmt)
	if !ok || len(assign.Rhs) != 1 {
		return false
	}

	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}

	fn, ok := call.Fun.(*ast.Ident)
	if !ok || fn.Name != "guardSync" {
		return false
	}

	if types.ExprString(call.Args[0]) != descriptor {
		return false
	}

	cond, ok := ifs.Cond.(*ast.BinaryExpr)

	return ok && cond.Op == token.NEQ
}

// roleManifestFixture is the manifest the role's task writes, as testdata the
// shell gate holds the role's rendering to.
func roleManifestFixture(t *testing.T) []byte {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("testdata", "role-manifest.yml"))
	if err != nil {
		t.Fatal(err)
	}

	return body
}

func plantRoleJournal(t *testing.T, dir string, body []byte) {
	t.Helper()

	mustOK(t, os.WriteFile(filepath.Join(dir, roleJournalName), body, 0o600))
}

// G4 (a) to (e): a takeover proves the role's transaction complete through
// its manifest and re-labels the guard with the pointer and the executable
// kept; the Go readers refuse to LOAD a role journal (a resume over one
// refuses naming the role); holder recovery refuses while the pointer exists
// and succeeds once the role's finalizer removed it, never opening the
// retained journal; a directory with neither journal is no journal.
func TestATakeoverProvesTheRolesTransactionCompleteWithoutLoadingIt(t *testing.T) {
	seed := func(t *testing.T, holder string) (*guardFixture, string, string) {
		t.Helper()

		f := newGuardFixture(t)
		mustHold(t, holder)

		recovery := filepath.Join(f.root, "recovery-20260909T120000-0badcafe")
		mustOK(t, os.Mkdir(recovery, 0o700))
		plantRoleJournal(t, recovery, roleManifestFixture(t))

		pointer := filepath.Join(f.active(), guardPointerName)
		mustOK(t, os.Symlink(recovery, pointer))
		cleanScan(t)

		return f, recovery, pointer
	}

	t.Run("readJournalUnder refuses a role-only directory as the role's", func(t *testing.T) {
		f, recovery, _ := seed(t, "ci-1")
		root := openRootForTest(t)

		_, err := readJournalUnder(root, recovery)
		if !errors.Is(err, errRoleJournal) {
			t.Fatalf("readJournalUnder over a role journal: err = %v, want the role's refusal", err)
		}

		_ = f
	})

	t.Run("a role journal that cannot be examined is not no journal", func(t *testing.T) {
		f := newGuardFixture(t)
		mustHold(t, "ci-1")

		recovery := filepath.Join(f.root, "recovery-20260909T120000-0badcafe")
		mustOK(t, os.Mkdir(recovery, 0o700))

		saved := guardStatAt
		guardStatAt = func(dir *os.File, name string) (*unix.Stat_t, error) {
			if name == roleJournalName {
				return nil, syscall.EIO
			}

			return saved(dir, name)
		}
		t.Cleanup(func() { guardStatAt = saved })

		root := openRootForTest(t)

		_, err := readJournalUnder(root, recovery)
		switch {
		case err == nil:
			t.Fatal("readJournalUnder over an unexaminable role journal: no error")
		case errors.Is(err, hostupgrade.ErrNoJournal):
			t.Fatalf("readJournalUnder read an unexaminable role journal as no journal: %v", err)
		case !errors.Is(err, syscall.EIO) || !strings.Contains(err.Error(), "examine"):
			t.Fatalf("readJournalUnder: err = %v, want the examination's failure named", err)
		}
	})

	t.Run("a Go resume over a role-only claim refuses naming the role", func(t *testing.T) {
		g := newGuardedFixture(t)
		continued := observeResumeContinuation(t)
		mustOK(t, os.Mkdir(g.root, 0o700))

		recovery := filepath.Join(g.root, "recovery-20260909T120000-0badcafe")
		mustOK(t, os.Mkdir(recovery, 0o700))
		plantRoleJournal(t, recovery, roleManifestFixture(t))
		mustOK(t, os.Symlink(recovery, g.active()))

		err := resumeHostUpgrade(t.Context(), g.cfg)
		if !errors.Is(err, errRoleJournal) {
			t.Fatalf("a resume over a role journal: err = %v, want the role's refusal", err)
		}

		if continued() {
			t.Error("the resume reached its barrier over a role journal")
		}

		if _, err := os.Lstat(g.active()); err != nil {
			t.Errorf("the claim is gone: %v", err)
		}
	})

	t.Run("a takeover succeeds and keeps the pointer and the executable", func(t *testing.T) {
		f, _, pointer := seed(t, "ci-1")
		rec := f.record(t)
		pointerID := fileIdentityOf(t, pointer)

		mustOK(t, guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped"))

		got := f.record(t)
		rec.Holder = "ci-2"

		if got != rec {
			t.Errorf("after the takeover the record is %+v, want %+v", got, rec)
		}

		if fileIdentityOf(t, pointer) != pointerID {
			t.Error("the takeover moved the pointer")
		}
	})

	t.Run("holder recovery refuses while the pointer exists, naming the transaction", func(t *testing.T) {
		f, _, pointer := seed(t, "ci-1")

		err := guardRun(t, "recover", "--holder", "ci-1", "--old-driver-stopped")
		if err == nil || !strings.Contains(err.Error(), "recover-from ci-1") {
			t.Fatalf("holder recovery under a pointer: err = %v, want the transaction's refusal", err)
		}

		if _, err := os.Lstat(pointer); err != nil {
			t.Errorf("the pointer is gone: %v", err)
		}

		if _, err := os.Lstat(filepath.Join(f.active(), guardRecordName)); err != nil {
			t.Errorf("the record is gone: %v", err)
		}
	})

	t.Run("holder recovery succeeds once the pointer is gone, opening no journal", func(t *testing.T) {
		f, recovery, pointer := seed(t, "ci-1")
		mustOK(t, os.Remove(pointer))

		var opened []string

		guardHook = func(op guardOp) error {
			if op.Kind == "open" || op.Kind == "openat" {
				opened = append(opened, op.Path)
			}

			return nil
		}

		mustOK(t, guardRun(t, "recover", "--holder", "ci-1", "--old-driver-stopped"))

		guardHook = nil

		if _, err := os.Lstat(f.active()); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the guard remains (lstat err = %v)", err)
		}

		for _, path := range opened {
			if strings.HasPrefix(path, recovery) {
				t.Errorf("holder recovery opened %s; the retained journal is not its business", path)
			}
		}

		if _, err := os.Stat(filepath.Join(recovery, roleJournalName)); err != nil {
			t.Errorf("the retained journal is gone: %v", err)
		}
	})

	t.Run("a directory with neither journal is no journal", func(t *testing.T) {
		f := newGuardFixture(t)
		mustOK(t, os.Mkdir(f.root, 0o700))

		recovery := filepath.Join(f.root, "recovery-20260909T120000-0badcafe")
		mustOK(t, os.Mkdir(recovery, 0o700))

		root := openRootForTest(t)

		if _, err := validateRecoveryUnder(root, recovery); !errors.Is(err, hostupgrade.ErrNoJournal) {
			t.Errorf("validateRecoveryUnder over an empty directory: err = %v, want no journal", err)
		}

		if _, err := readJournalUnder(root, recovery); !errors.Is(err, hostupgrade.ErrNoJournal) {
			t.Errorf("readJournalUnder over an empty directory: err = %v, want no journal", err)
		}
	})

	t.Run("a Go journal is the Go kind", func(t *testing.T) {
		f := newGuardFixture(t)
		mustOK(t, os.Mkdir(f.root, 0o700))

		recovery := filepath.Join(f.root, "recovery-x")
		mustOK(t, os.Mkdir(recovery, 0o700))
		writeJournalFixture(t, recovery, "installed")

		root := openRootForTest(t)

		kind, err := validateRecoveryUnder(root, recovery)
		if err != nil || kind != recoveryGo {
			t.Errorf("validateRecoveryUnder over a Go journal = %v, %v", kind, err)
		}
	})
}

// G4's MANIFEST PROOF: one otherwise-valid fixture per violation, each refused
// naming the member; the takeover is what consumes it.
func TestTheRolesManifestIsJudgedMemberByMember(t *testing.T) {
	valid := string(roleManifestFixture(t))

	replace := func(old, repl string) string {
		if !strings.Contains(valid, old) {
			t.Fatalf("the fixture does not contain %q", old)
		}

		return strings.Replace(valid, old, repl, 1)
	}

	// indexOf is strings.Index that fails the test rather than answering -1.
	indexOf := func(s, sub string) int {
		t.Helper()

		i := strings.Index(s, sub)
		if i < 0 {
			t.Fatalf("the fixture does not contain %q", sub)
		}

		return i
	}

	inputs := indexOf(valid, "inputs:")
	head, tail := valid[:inputs], valid[inputs:]

	serverAt := indexOf(tail, "- path: /etc/systemd/system/billet-server.service")
	firstInput := tail[len("inputs:\n"):serverAt]
	secondOn := tail[serverAt:]
	nodeAt := indexOf(secondOn, "- path: /etc/systemd/system/billet-node.service")
	nodeInput := secondOn[nodeAt:]
	serverInput := secondOn[:nodeAt]

	cases := map[string]struct {
		body  string
		words string
	}{
		"not YAML":                             {"not: [yaml", "not YAML"},
		"empty":                                {"", "empty"},
		"version 1":                            {replace("version: 2", "version: 1"), "version is 1"},
		"version as a string":                  {replace("version: 2", "version: '2'"), "version is not a number"},
		"a missing member":                     {replace("service_user: billet\n", ""), "service_user is missing"},
		"an extra member":                      {replace("version: 2\n", "version: 2\nextra: 1\n"), "extra is not a member"},
		"a boolean as a string":                {replace("binary_existed: true", "binary_existed: 'true'"), "binary_existed is not a boolean"},
		"a directory outside":                  {replace("server_state_dir: /var/lib/billet/server", "server_state_dir: /srv/billet"), "not under /var/lib/billet/"},
		"a directory under the root":           {replace("server_state_dir: /var/lib/billet/server", "server_state_dir: /var/lib/billet/upgrades/x"), "lies under the upgrade root"},
		"a dot segment":                        {replace("server_state_dir: /var/lib/billet/server", "server_state_dir: /var/lib/billet/../server"), "dot segment"},
		"equal directories":                    {replace("node_state_dir: /var/lib/billet/node", "node_state_dir: /var/lib/billet/server"), "the same directory"},
		"nested directories":                   {replace("node_state_dir: /var/lib/billet/node", "node_state_dir: /var/lib/billet/server/node"), "nest"},
		"an account that is not one":           {replace("service_user: billet", "service_user: 'Bill Et'"), "not an account name"},
		"an enablement outside the vocabulary": {replace("server_enablement: enabled", "server_enablement: masked"), "not an enablement state"},
		"two inputs":                           {head + "inputs:\n" + firstInput + serverInput, "inputs has 2 entries"},
		"four inputs":                          {head + "inputs:\n" + firstInput + serverInput + nodeInput + nodeInput, "inputs has 4 entries"},
		"reordered inputs":                     {head + "inputs:\n" + serverInput + firstInput + nodeInput, "inputs[0] is /etc/systemd/system/billet-server.service"},
		"a substituted path":                   {replace("- path: /etc/billet/billet.yaml", "- path: /etc/billet/other.yaml"), "inputs[0] is /etc/billet/other.yaml"},
		"a substituted backup":                 {replace("backup: billet.yaml.previous", "backup: elsewhere"), "inputs[0] is /etc/billet/billet.yaml (elsewhere)"},
		"an input member missing":              {replace("  existed: true\n  owner: root\n  group: billet\n", "  owner: root\n  group: billet\n"), "existed is missing"},
		"an input's existed as a string":       {replace("  existed: true\n  owner: root\n  group: billet\n", "  existed: 'true'\n  owner: root\n  group: billet\n"), "existed is not a boolean"},
		"a three-digit mode":                   {replace("mode: '0640'", "mode: '640'"), "not four octal digits"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newGuardFixture(t)
			mustHold(t, "ci-1")

			recovery := filepath.Join(f.root, "recovery-20260909T120000-0badcafe")
			mustOK(t, os.Mkdir(recovery, 0o700))
			plantRoleJournal(t, recovery, []byte(c.body))
			mustOK(t, os.Symlink(recovery, filepath.Join(f.active(), guardPointerName)))
			cleanScan(t)

			rec := f.record(t)

			err := guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped")
			if err == nil || !strings.Contains(err.Error(), c.words) {
				t.Fatalf("%s: err = %v, want %q", name, err, c.words)
			}

			if got := f.record(t); got != rec {
				t.Errorf("%s: the record changed to %+v", name, got)
			}
		})
	}

	// The metadata refusals: a symlink, a group-writable file, one over the bound.
	for name, plant := range map[string]func(t *testing.T, recovery string){
		"a symlinked manifest": func(t *testing.T, recovery string) {
			t.Helper()

			elsewhere := filepath.Join(t.TempDir(), "manifest.yml")
			mustOK(t, os.WriteFile(elsewhere, []byte(valid), 0o600))
			mustOK(t, os.Symlink(elsewhere, filepath.Join(recovery, roleJournalName)))
		},
		"a group-writable manifest": func(t *testing.T, recovery string) {
			t.Helper()

			plantRoleJournal(t, recovery, []byte(valid))
			mustOK(t, os.Chmod(filepath.Join(recovery, roleJournalName), 0o660))
		},
		"a manifest over the bound": func(t *testing.T, recovery string) {
			t.Helper()

			plantRoleJournal(t, recovery, []byte(valid+"# "+strings.Repeat("x", maxRoleJournalBytes)+"\n"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newGuardFixture(t)
			mustHold(t, "ci-1")

			recovery := filepath.Join(f.root, "recovery-20260909T120000-0badcafe")
			mustOK(t, os.Mkdir(recovery, 0o700))
			plant(t, recovery)
			mustOK(t, os.Symlink(recovery, filepath.Join(f.active(), guardPointerName)))
			cleanScan(t)

			if err := guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped"); err == nil {
				t.Fatalf("%s: the takeover succeeded", name)
			}
		})
	}
}

// THE STATUS ANSWERS THE GATE CORRUPTS ARE THE COMMAND'S OWN: each shape is
// planted, `status --json` is run over it, and its output, with the fixture's
// paths spelled as the packaged host's, is compared with the fixture file the
// shell gate reads (BILLET_UPDATE_FIXTURES=1 rewrites them). A fake that
// answered a shape the command does not produce would drift from these.
func TestTheGuardStatusFixturesAreTheCommandsOwn(t *testing.T) {
	dir := filepath.Join("..", "..", "ansible_collections", "junioryono", "billet", "tests", "fixtures", "guard-status")
	update := os.Getenv("BILLET_UPDATE_FIXTURES") == "1"

	shapes := map[string]func(t *testing.T, f *guardFixture){
		"none": func(t *testing.T, _ *guardFixture) { t.Helper() },
		"healthy": func(t *testing.T, _ *guardFixture) {
			t.Helper()

			mustHold(t, "ci-1")
		},
		"healthy-with-pointer": func(t *testing.T, f *guardFixture) {
			t.Helper()

			mustHold(t, "ci-1")
			recovery := filepath.Join(f.root, "recovery-20260909T120000-0badcafe")
			mustOK(t, os.Mkdir(recovery, 0o700))
			mustOK(t, os.Symlink(recovery, filepath.Join(f.active(), guardPointerName)))
		},
		"candidate": func(t *testing.T, f *guardFixture) {
			t.Helper()

			candidate := stageGuardCandidate(t, f, "recovery-20260909T120000-0badcafe", []byte("#!/bin/sh\nexit 0\n"))
			mustOK(t, guardRun(t, "hold", "--holder", "ci-1", "--candidate", candidate))
		},
		"verification-false": func(t *testing.T, f *guardFixture) {
			t.Helper()

			candidate := stageGuardCandidate(t, f, "recovery-20260909T120000-0badcafe", []byte("#!/bin/sh\nexit 0\n"))
			mustOK(t, guardRun(t, "hold", "--holder", "ci-1", "--candidate", candidate))
			mustOK(t, os.WriteFile(candidate, []byte("#!/bin/sh\nexit 1\n"), 0o755))
		},
		"malformed-record": func(t *testing.T, f *guardFixture) {
			t.Helper()

			mustHold(t, "ci-1")
			mustOK(t, os.WriteFile(filepath.Join(f.active(), guardRecordName), []byte("not json\n"), 0o600))
		},
		"unpublished": func(t *testing.T, f *guardFixture) {
			t.Helper()

			mustOK(t, os.MkdirAll(f.active(), 0o700))
		},
		"legacy-file": func(t *testing.T, f *guardFixture) {
			t.Helper()

			mustOK(t, os.Mkdir(f.root, 0o700))
			mustOK(t, os.WriteFile(f.active(), []byte("/var/lib/billet/upgrades/20260909T120000000000000\n"), 0o600))
		},
		"host-upgrade": func(t *testing.T, f *guardFixture) {
			t.Helper()

			mustOK(t, os.Mkdir(f.root, 0o700))
			mustOK(t, os.Symlink(filepath.Join(f.root, "recovery-x"), f.active()))
		},
	}

	if update {
		mustOK(t, os.MkdirAll(dir, 0o755))
	}

	for name, plant := range shapes {
		t.Run(name, func(t *testing.T) {
			f := newGuardFixture(t)
			plant(t, f)

			out := capture(t, func() { mustOK(t, guardRun(t, "status", "--json")) })

			// The packaged host's spellings, so the fixture is the same on
			// every machine.
			out = strings.ReplaceAll(out, f.root, "/var/lib/billet/upgrades")
			out = strings.ReplaceAll(out, f.binary, "/usr/bin/billet")
			out = regexp.MustCompile(`"release_executable_sha256": "[0-9a-f]{64}"`).
				ReplaceAllString(out, `"release_executable_sha256": "`+strings.Repeat("ab", 32)+`"`)

			var parsed map[string]any
			if err := json.Unmarshal([]byte(out), &parsed); err != nil {
				t.Fatalf("%s: the status is not JSON: %v\n%s", name, err, out)
			}

			path := filepath.Join(dir, name+".json")

			if update {
				mustOK(t, os.WriteFile(path, []byte(out), 0o644))

				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: %v (run with BILLET_UPDATE_FIXTURES=1 to write the fixtures)", name, err)
			}

			if string(want) != out {
				t.Errorf("%s: the fixture differs from the command's answer:\n--- fixture\n%s\n--- command\n%s", name, want, out)
			}
		})
	}
}

// The parsed shapes the gate relies on hold: a malformed record answers
// converge-guard with record_error and EMPTY members, the fixture the gate's
// P5(z) consumes.
func TestAMalformedRecordsStatusHasEmptyMembersBesideItsError(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "ansible_collections", "junioryono", "billet", "tests", "fixtures", "guard-status", "malformed-record.json"))
	if err != nil {
		t.Fatal(err)
	}

	var report guardStatusReport
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}

	if report.Active != "converge-guard" || report.Guard == nil || report.Guard.RecordError == "" ||
		report.Guard.Holder != "" || report.Guard.ReleaseExecutable != "" || report.Guard.ReleaseExecutableSHA256 != "" {
		t.Errorf("the malformed record's status is %s", body)
	}
}
