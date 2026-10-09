package app

import (
	"context"
	"errors"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
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
// requireJobAccounting on the provider NewProvider built, tests its error, and
// only then keeps it. Asserted on the source because OpenNode cannot build a
// Firecracker provider without a Ceph cluster.
func TestTheNodeRefusesUnprovedAccountingBeforeKeepingItsProvider(t *testing.T) {
	t.Parallel()

	open := nodeFunc(t, "node.go", "open")

	var built, required, kept token.Pos

	ast.Inspect(open.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			id, ok := x.Fun.(*ast.Ident)
			switch {
			case ok && id.Name == "NewProvider" && !built.IsValid():
				built = x.Pos()
			case ok && id.Name == "requireJobAccounting" && len(x.Args) == 1:
				if arg, ok := x.Args[0].(*ast.Ident); ok && arg.Name == "p" {
					required = x.Pos()
				}
			}
		case *ast.IfStmt:
			// The call must be the if's own initialiser whose error returns.
			if assign, ok := x.Init.(*ast.AssignStmt); ok && len(assign.Rhs) == 1 {
				if call, ok := assign.Rhs[0].(*ast.CallExpr); ok && calleeName(call) == "requireJobAccounting" {
					if len(x.Body.List) == 0 {
						t.Error("Node.open ignores requireJobAccounting's error")
					} else if _, ok := x.Body.List[0].(*ast.ReturnStmt); !ok {
						t.Error("Node.open does not return requireJobAccounting's error")
					}
				}
			}
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "provider" {
					kept = x.Pos()
				}
			}
		}

		return true
	})

	if !built.IsValid() || !required.IsValid() || !kept.IsValid() || required < built || required > kept {
		t.Errorf("Node.open must call requireJobAccounting(p) after NewProvider and before keeping "+
			"the provider (NewProvider %v, requireJobAccounting %v, n.provider %v)",
			built.IsValid(), required.IsValid(), kept.IsValid())
	}
}
