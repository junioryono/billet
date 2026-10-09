package app

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/deploymentid"
	"github.com/junioryono/billet/internal/provider/firecracker"
)

// stagedCgroupHost is a cgroup-v2 hierarchy staged under a temp dir and the
// mount table naming it, in one of the shapes a host can be in.
type stagedCgroupHost func(t *testing.T, root string)

// writeStaged writes body at root/name, making its directory.
func writeStaged(t *testing.T, root, name, body string) {
	t.Helper()

	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("stage %s: %v", name, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("stage %s: %v", name, err)
	}
}

var (
	// The reference host's shape (2026-09-25): the root offers memory and io,
	// and an io-enabled child shows io.weight.
	cgroupBothPresent stagedCgroupHost = func(t *testing.T, root string) {
		t.Helper()
		writeStaged(t, root, "cgroup.controllers", "cpuset cpu io memory pids\n")
		writeStaged(t, root, "cgroup.subtree_control", "cpu io memory pids\n")
		writeStaged(t, root, "system.slice/cgroup.type", "domain\n")
		writeStaged(t, root, "system.slice/io.weight", "default 100\n")
	}
	// A root that offers neither controller.
	cgroupBothMissing stagedCgroupHost = func(t *testing.T, root string) {
		t.Helper()
		writeStaged(t, root, "cgroup.controllers", "cpu pids\n")
		writeStaged(t, root, "cgroup.subtree_control", "cpu\n")
		if err := os.Mkdir(filepath.Join(root, "system.slice"), 0o700); err != nil {
			t.Fatalf("stage: %v", err)
		}
	}
	// A controller list that cannot be read: a directory where the file
	// should be, which fails a read whoever runs the test, root included.
	cgroupUnreadable stagedCgroupHost = func(t *testing.T, root string) {
		t.Helper()
		if err := os.Mkdir(filepath.Join(root, "cgroup.controllers"), 0o700); err != nil {
			t.Fatalf("stage: %v", err)
		}
	}
)

// stageMountTable stages host and returns the mount table naming it, and the
// root it is mounted at.
func stageMountTable(t *testing.T, host stagedCgroupHost) (string, string) {
	t.Helper()

	dir := t.TempDir()
	root := filepath.Join(dir, "cgroup")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("stage: %v", err)
	}
	host(t, root)
	mounts := filepath.Join(dir, "mounts")
	writeStaged(t, dir, "mounts", "cgroup2 "+root+" cgroup2 rw,nosuid 0 0\n")

	return mounts, root
}

// stagedFirecracker is a node.firecracker block New accepts without touching
// the host: a binary named as the jailer requires, and paths nothing reads.
func stagedFirecracker(t *testing.T) config.FirecrackerConfig {
	t.Helper()

	dir := t.TempDir()
	bin := filepath.Join(dir, "firecracker-v1.16.1")
	writeStaged(t, dir, "firecracker-v1.16.1", "")

	return config.FirecrackerConfig{
		BinaryPath:   bin,
		JailerPath:   filepath.Join(dir, "jailer"),
		KernelImage:  filepath.Join(dir, "vmlinux"),
		ChrootBase:   "/srv/jail",
		JailUIDMin:   900000,
		JailUIDCount: 8,
		Bridge:       "br0",
	}
}

// unusedDisk is the root disk a provider built only to be asked about its
// accounting never reaches.
type unusedDisk struct{}

func (unusedDisk) ResolveGeneration(context.Context, string, string) (string, error) {
	return "", errors.New("unused")
}

func (unusedDisk) CloneRoot(context.Context, string, string, config.ByteSize) (string, error) {
	return "", errors.New("unused")
}

func (unusedDisk) DiscardRoot(context.Context, string) error { return errors.New("unused") }

func (unusedDisk) KernelFor(context.Context, string, string) (string, bool, error) {
	return "", false, errors.New("unused")
}

func (unusedDisk) GenerationGone(error) bool { return false }

// A NODE WITH node.monitoring REFUSES A HOST THAT DID NOT PROVE BOTH memory AND
// io, naming each controller, whether it is missing or could not be told, and
// the jailer's parent cgroup to enable them for; a node without it starts on
// any of these hosts exactly as before. Built through FirecrackerOptions, the
// options NewProvider gives the node's provider, so a node.monitoring that
// stopped asking for accounting fails here too.
func TestANodeWithMonitoringRefusesAHostThatCannotAccountForAJob(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		host       stagedCgroupHost
		monitoring bool
		says       []string
	}{
		{"both present", cgroupBothPresent, true, nil},
		{"both missing", cgroupBothMissing, true, []string{"memory is missing and io is missing"}},
		{"an unreadable controller list", cgroupUnreadable, true,
			[]string{"could not tell whether memory", "could not tell whether io", "cgroup.controllers"}},
		{"both missing, monitoring off", cgroupBothMissing, false, nil},
		{"unreadable, monitoring off", cgroupUnreadable, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mounts, root := stageMountTable(t, tc.host)
			cfg := &config.Config{Node: &config.NodeConfig{Provider: config.ProviderFirecracker}}
			if tc.monitoring {
				cfg.Node.Monitoring = &config.NodeMonitoringConfig{}
			}

			p, err := firecracker.New(deploymentid.Preflight, stagedFirecracker(t), unusedDisk{},
				append(FirecrackerOptions(cfg), firecracker.WithMountTable(mounts))...)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			err = requireJobAccounting(p)
			if tc.says == nil {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}

				return
			}
			if !errors.Is(err, firecracker.ErrJobAccountingUnproved) {
				t.Fatalf("requireJobAccounting = %v, want the accounting refusal", err)
			}
			for _, want := range append(tc.says, filepath.Join(root, "firecracker-v1.16.1"),
				root+"/cgroup.subtree_control", "remove node.monitoring") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
		})
	}
}

// THE NODE REFUSES BEFORE IT ACCEPTS ITS PROVIDER: Node.open calls
// requireJobAccounting once, as a statement of its own body (not inside a
// condition or a closure), in the shape
// `if err := requireJobAccounting(p); err != nil { return err }`, after the
// statement that calls NewProvider and before the one that keeps the
// provider; and NewProvider's Firecracker case returns firecracker.New with
// FirecrackerOptions(cfg)..., the options the table above is built with.
// Asserted on the source because OpenNode cannot build a Firecracker provider
// without a Ceph cluster; the checkers are themselves tested against the
// bypasses below.
func TestTheNodeRefusesUnprovedAccountingBeforeKeepingItsProvider(t *testing.T) {
	t.Parallel()

	if err := refusesBeforeKeeping(nodeFunc(t, "node.go", "open").Body); err != nil {
		t.Errorf("Node.open: %v", err)
	}
	if err := returnsOptionedFirecracker(nodeFunc(t, "node.go", "NewProvider").Body); err != nil {
		t.Errorf("NewProvider: %v", err)
	}
}

// AND THE CHECKERS CATCH THE BYPASSES: a refusal inside a condition or a
// closure, a discarded error, and options built where nothing returns them.
func TestTheRefusalCheckersCatchTheirBypasses(t *testing.T) {
	t.Parallel()

	const keep = "\tn.client, n.provider = client, p\n\treturn nil\n}"
	const build = "func open() error {\n\tp, err := NewProvider(cfg, n.deployment)\n\tif err != nil {\n\t\treturn err\n\t}\n"
	const guard = "\tif err := requireJobAccounting(p); err != nil {\n\t\treturn err\n\t}\n"

	for _, tc := range []struct {
		name, src string
		ok        bool
	}{
		{"the production shape", build + guard + keep, true},
		{"inside a condition", build + "\tif cfg.Node.Monitoring == nil {\n" + guard + "\t}\n" + keep, false},
		{"inside a closure", build + "\t_ = func() error {\n" + guard + "\t\treturn nil\n\t}\n" + keep, false},
		{"the error discarded", build + "\t_ = requireJobAccounting(p)\n" + keep, false},
		{"after the provider is kept", build + "\tn.client, n.provider = client, p\n" + guard + "\treturn nil\n}", false},
		{"twice, once ignored", build + guard + "\t_ = requireJobAccounting(p)\n" + keep, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if err := refusesBeforeKeeping(parsedFunc(t, tc.src).Body); (err == nil) != tc.ok {
				t.Errorf("refusesBeforeKeeping = %v, want ok %v", err, tc.ok)
			}
		})
	}

	const optioned = "firecracker.New(deployment, *cfg.Node.Firecracker, store, FirecrackerOptions(cfg)...)"
	for _, tc := range []struct {
		name, src string
		ok        bool
	}{
		{"the production shape", "func NewProvider() {\n\tswitch cfg.Node.Provider {\n\tcase config.ProviderFirecracker:\n\t\treturn " +
			optioned + "\n\t}\n}", true},
		{"options built and not returned", "func NewProvider() {\n\tswitch cfg.Node.Provider {\n\tcase config.ProviderFirecracker:\n\t\t_ = func() { " +
			optioned + " }\n\t\treturn firecracker.New(deployment, *cfg.Node.Firecracker, store)\n\t}\n}", false},
		{"a decoy switch in an unused closure", "func NewProvider() {\n\t_ = func() {\n\t\tswitch cfg.Node.Provider {\n\t\tcase config.ProviderFirecracker:\n\t\t\treturn " +
			optioned + "\n\t\t}\n\t}\n\tswitch cfg.Node.Provider {\n\tcase config.ProviderFirecracker:\n\t\treturn firecracker.New(deployment, *cfg.Node.Firecracker, store)\n\t}\n}", false},
		{"options returned for another backend", "func NewProvider() {\n\tswitch cfg.Node.Provider {\n\tcase config.ProviderDocker:\n\t\treturn " +
			optioned + "\n\tcase config.ProviderFirecracker:\n\t\treturn firecracker.New(deployment, *cfg.Node.Firecracker, store)\n\t}\n}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if err := returnsOptionedFirecracker(parsedFunc(t, tc.src).Body); (err == nil) != tc.ok {
				t.Errorf("returnsOptionedFirecracker = %v, want ok %v", err, tc.ok)
			}
		})
	}
}

// parsedFunc parses one function's source.
func parsedFunc(t *testing.T, src string) *ast.FuncDecl {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", "package app\n\n"+src+"\n", 0)
	if err != nil {
		t.Fatalf("parse the fixture: %v\n%s", err, src)
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			return fn
		}
	}
	t.Fatalf("the fixture declares no function:\n%s", src)

	return nil
}

// refusesBeforeKeeping is nil when body calls requireJobAccounting exactly
// once, as a top-level `if err := requireJobAccounting(p); err != nil { return
// err }` between the top-level statement that calls NewProvider and the one
// that assigns the provider.
func refusesBeforeKeeping(body *ast.BlockStmt) error {
	calls := 0
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && calleeName(call) == "requireJobAccounting" {
			calls++
		}

		return true
	})
	if calls != 1 {
		return fmt.Errorf("requireJobAccounting is called %d times, want once", calls)
	}

	built, guarded, kept := -1, -1, -1
	for i, stmt := range body.List {
		switch x := stmt.(type) {
		case *ast.AssignStmt:
			for _, rhs := range x.Rhs {
				if call, ok := rhs.(*ast.CallExpr); ok && calleeName(call) == "NewProvider" && built < 0 {
					built = i
				}
			}
			for _, lhs := range x.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "provider" && kept < 0 {
					kept = i
				}
			}
		case *ast.IfStmt:
			if refusesWith(x, "requireJobAccounting", "p") {
				guarded = i
			}
		}
	}
	if built < 0 || guarded < 0 || kept < 0 || guarded < built || guarded > kept {
		return fmt.Errorf("want NewProvider, then `if err := requireJobAccounting(p); err != nil { "+
			"return err }`, then the provider kept, each a statement of the body (at %d, %d, %d)",
			built, guarded, kept)
	}

	return nil
}

// returnsOptionedFirecracker is nil when the switch on cfg.Node.Provider that
// is a statement of body itself (not one in a closure or a branch) has a case
// naming config.ProviderFirecracker whose own statements return
// firecracker.New(..., FirecrackerOptions(cfg)...).
func returnsOptionedFirecracker(body *ast.BlockStmt) error {
	for _, stmt := range body.List {
		sw, ok := stmt.(*ast.SwitchStmt)
		if !ok || !isProviderTag(sw.Tag) {
			continue
		}
		for _, c := range sw.Body.List {
			clause, ok := c.(*ast.CaseClause)
			if !ok || !slices.ContainsFunc(clause.List, func(e ast.Expr) bool {
				return namesSelector(e, "config", "ProviderFirecracker")
			}) {
				continue
			}
			if slices.ContainsFunc(clause.Body, returnsOptionedNew) {
				return nil
			}
		}
	}

	return errors.New("the Firecracker case of the switch on cfg.Node.Provider does not return " +
		"firecracker.New(..., FirecrackerOptions(cfg)...)")
}

// isProviderTag reports whether expr is cfg.Node.Provider.
func isProviderTag(expr ast.Expr) bool {
	outer, ok := expr.(*ast.SelectorExpr)
	if !ok || outer.Sel.Name != "Provider" {
		return false
	}

	return namesSelector(outer.X, "cfg", "Node")
}

// returnsOptionedNew reports whether stmt is
// `return firecracker.New(..., FirecrackerOptions(cfg)...)`.
func returnsOptionedNew(stmt ast.Stmt) bool {
	ret, ok := stmt.(*ast.ReturnStmt)
	if !ok || len(ret.Results) == 0 {
		return false
	}
	call, ok := ret.Results[0].(*ast.CallExpr)
	if !ok || !namesSelector(call.Fun, "firecracker", "New") || !call.Ellipsis.IsValid() {
		return false
	}
	last, ok := call.Args[len(call.Args)-1].(*ast.CallExpr)
	if !ok || calleeName(last) != "FirecrackerOptions" || len(last.Args) != 1 {
		return false
	}
	arg, ok := last.Args[0].(*ast.Ident)

	return ok && arg.Name == "cfg"
}

// refusesWith reports whether stmt is exactly `if err := fn(arg); err != nil {
// return err }`: the call's error bound, tested, and returned first.
func refusesWith(stmt *ast.IfStmt, fn, arg string) bool {
	assign, ok := stmt.Init.(*ast.AssignStmt)
	if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return false
	}
	bound, ok := assign.Lhs[0].(*ast.Ident)
	call, isCall := assign.Rhs[0].(*ast.CallExpr)
	if !ok || !isCall || calleeName(call) != fn || len(call.Args) != 1 {
		return false
	}
	if a, ok := call.Args[0].(*ast.Ident); !ok || a.Name != arg {
		return false
	}
	cond, ok := stmt.Cond.(*ast.BinaryExpr)
	if !ok || cond.Op != token.NEQ {
		return false
	}
	if x, ok := cond.X.(*ast.Ident); !ok || x.Name != bound.Name {
		return false
	}
	if y, ok := cond.Y.(*ast.Ident); !ok || y.Name != "nil" {
		return false
	}
	if len(stmt.Body.List) == 0 {
		return false
	}
	ret, ok := stmt.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return false
	}
	result, ok := ret.Results[0].(*ast.Ident)

	return ok && result.Name == bound.Name
}
