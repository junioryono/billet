package host

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
)

// ONLY THE ROLE'S FILTER, WITH NO DEVICES FILE, IS VOUCHED FOR: none is the warning,
// and everything else, another filter or an answer that could not be had, is
// could-not-tell.
//
// Not parallel: it replaces the package's lvmConfig.
func TestBilletCheckSaysWhetherLVMScansTheMappedImages(t *testing.T) {
	saved := lvmConfig
	t.Cleanup(func() { lvmConfig = saved })

	type answer struct {
		out, stderr string
		exit        int
		err         error
	}

	// What lvmconfig printed for the role's filter on the reference host, LVM 2.03.31
	// (2026-11-26).
	roleFilter := answer{out: `global_filter=["r|^/dev/rbd|","r|^/dev/nbd|"]` + "\n"}
	noDevicesFile := answer{out: "use_devicesfile=0\n"}

	const vouched = "the host role's global_filter keeps LVM's scans off"

	for _, tc := range []struct {
		name          string
		filter, files answer
		want          string
	}{
		{name: "the role's filter and no devices file", filter: roleFilter, files: noDevicesFile, want: vouched},
		{name: "the role's filter beside a devices file", filter: roleFilter, files: answer{out: "use_devicesfile=1\n"},
			want: "UNCHECKED: the host role's global_filter is set, but LVM uses a devices file"},
		{name: "the role's filter, the devices file unreadable", filter: roleFilter,
			files: answer{stderr: "  Failed to load config file /etc/lvm/lvm.conf\n", exit: 4},
			want:  "could not be read: exit 4: Failed to load config file"},
		{name: "the role's filter, the devices-file query not run", filter: roleFilter,
			files: answer{err: errors.New("signal: killed")}, want: "could not be read: signal: killed"},
		{name: "the role's filter, an answer that is neither", filter: roleFilter, files: answer{out: "use_devicesfile=2\n"},
			want: `lvmconfig answered "use_devicesfile=2"`},
		{name: "no filter", filter: answer{stderr: "  Configuration node devices/global_filter not found\n", exit: 5},
			want: "WARNING: LVM sets no global_filter"},
		{name: "a filter whose regex LVM reads differently",
			filter: answer{out: `global_filter=["r|^/dev/rbd[0-9]{1,}$|","r|^/dev/nbd[0-9]{1,}$|"]`},
			want:   "UNCHECKED: LVM applies"},
		// LVM 2.03.16 renders a one-entry list as a string.
		{name: "one protective pattern in 2.03.16's rendering", filter: answer{out: `global_filter="r|^/dev/(rbd|nbd)|"`},
			want: "UNCHECKED: LVM applies"},
		{name: "a configuration it could not read",
			filter: answer{stderr: "  Failed to load config file /etc/lvm/lvmlocal.conf\n", exit: 4},
			want:   "UNCHECKED: lvmconfig devices/global_filter exited 4: Failed to load config file"},
		{name: "exit 5 for anything but not found", filter: answer{stderr: "  something else\n", exit: 5},
			want: "UNCHECKED: lvmconfig devices/global_filter exited 5"},
		{name: "an lvmconfig that could not run", filter: answer{err: errors.New("signal: killed")},
			want: "UNCHECKED: could not run lvmconfig: signal: killed"},
		{name: "no lvmconfig found", filter: answer{err: errNoLVMConfig},
			want: "UNCHECKED: could not run lvmconfig: no lvmconfig on PATH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lvmConfig = func(_ context.Context, args ...string) (string, string, int, error) {
				a := tc.filter
				switch strings.Join(args, " ") {
				case "devices/global_filter":
				case "--typeconfig full devices/use_devicesfile":
					a = tc.files
				default:
					t.Fatalf("lvmconfig asked %v", args)
				}

				return a.out, a.stderr, a.exit, a.err
			}

			var out bytes.Buffer

			reportLVMFilter(t.Context(), cli.Env{Stdout: &out, Stderr: &out, Getenv: func(string) string { return "" }})

			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("said %q, want it to say %q", out.String(), tc.want)
			}
			if tc.want != vouched && strings.Contains(out.String(), vouched) {
				t.Errorf("vouched for the filter on this answer: %q", out.String())
			}
			if tc.name != "no filter" && strings.Contains(out.String(), "WARNING") {
				t.Errorf("warned on an answer that is not the missing filter: %q", out.String())
			}
		})
	}
}

// billet check ASKS, on every node with a ceph block, and BEFORE the cluster's
// preflight, whose failure returns early.
func TestBilletCheckAsksAboutLVMBeforeTheCluster(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "check.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	called := func(n ast.Node, name string) bool {
		found := false

		ast.Inspect(n, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name {
					found = true
				}
			}

			return !found
		})

		return found
	}

	blocks := 0

	ast.Inspect(file, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}

		cond, ok := ifStmt.Cond.(*ast.BinaryExpr)
		if !ok || cond.Op != token.NEQ {
			return true
		}

		sel, ok := cond.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Ceph" {
			return true
		}

		blocks++

		lvm, cluster := -1, -1

		for i, stmt := range ifStmt.Body.List {
			if lvm < 0 && called(stmt, "reportLVMFilter") {
				lvm = i
			}

			if cluster < 0 && called(stmt, "checkCephCluster") {
				cluster = i
			}
		}

		if lvm < 0 || cluster < 0 || lvm > cluster {
			t.Errorf("%s: the ceph block asks LVM at statement %d and the cluster at %d; it must ask LVM, "+
				"and first", fset.Position(ifStmt.Pos()), lvm, cluster)
		}

		return true
	})

	if blocks != 1 {
		t.Fatalf("found %d blocks testing a ceph section in check.go, want the one", blocks)
	}
}
