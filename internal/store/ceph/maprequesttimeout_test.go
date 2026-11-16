package ceph

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// EVERY DEVICE BILLET MAPS FAILS AN OSD REQUEST AFTER FIVE MINUTES RATHER THAN
// WAITING FOR EVER. On 2026-11-13 an OSD restart on a host that maps from its own
// OSDs deadlocked for nine hours: the OSD's activation read a device billet had
// mapped, whose writeback waited for the OSDs. The option is spelled as rbd takes
// it, comes before the image, and leaves the caller's own options (--read-only)
// in place.
func TestEveryMapCarriesTheOSDRequestTimeout(t *testing.T) {
	t.Parallel()

	if got := requestTimeoutOption(); got != "osd_request_timeout=300" {
		t.Fatalf("requestTimeoutOption() = %q, want osd_request_timeout=300 (five minutes, whole seconds)", got)
	}

	// mapped is the one map call among calls, which must name the option right
	// after -o and before the image, the last argument.
	mapped := func(t *testing.T, calls [][]string, image string) []string {
		t.Helper()

		var maps [][]string
		for _, call := range calls {
			if i := slices.Index(call, "device"); i >= 0 && i+1 < len(call) && call[i+1] == "map" {
				maps = append(maps, call)
			}
		}
		if len(maps) != 1 {
			t.Fatalf("%d maps ran, want one: %v", len(maps), maps)
		}

		call := maps[0]
		o := slices.Index(call, "-o")
		if o < 0 || o+1 >= len(call) || call[o+1] != "osd_request_timeout=300" {
			t.Fatalf("the map ran without -o osd_request_timeout=300: %v", call)
		}
		if last := call[len(call)-1]; last != image || o+1 >= len(call)-1 {
			t.Fatalf("the map's image is %q after its options, want %q: %v", last, image, call)
		}

		return call
	}

	t.Run("rbdMap keeps the caller's options", func(t *testing.T) {
		t.Parallel()

		f := newCacheFake()
		f.snapshots["billet-images/img@g1"] = true
		c := cacheClient(t, f)

		if _, err := c.rbdMap(t.Context(), "billet-images/img@g1", "--read-only"); err != nil {
			t.Fatalf("rbdMap: %v", err)
		}

		if call := mapped(t, f.calls, "billet-images/img@g1"); !slices.Contains(call, "--read-only") {
			t.Errorf("the map lost the caller's --read-only: %v", call)
		}
	})

	t.Run("a guest's root disk", func(t *testing.T) {
		t.Parallel()

		f := newCloneFake()
		if _, err := cloneClient(t, f).CloneRoot(t.Context(), "img@g1", "billet-abc", 0); err != nil {
			t.Fatalf("CloneRoot: %v", err)
		}

		mapped(t, f.calls, "billet-cache/billet-abc")
	})

	t.Run("a cache volume", func(t *testing.T) {
		t.Parallel()

		f := newCacheFake()
		c := cacheClient(t, f)
		volume, err := c.Create(t.Context(), "acme/api/go", 1<<30)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		mapped(t, f.calls, volume.Handle)
	})
}

// rbdMap IS THE ONLY CODE THAT MAPS, so the timeout above is on every map. A
// second `"device", "map"` anywhere in billet's own Go, in this package or out of
// it, would map without it.
func TestOnlyRbdMapBuildsADeviceMap(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..", "..")
	var sites []string

	for _, dir := range []string{"internal", "cmd", "ee"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}

			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok {
					return true
				}

				ast.Inspect(fn, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}

					for i := 0; i+1 < len(call.Args); i++ {
						if stringLit(call.Args[i]) == "device" && stringLit(call.Args[i+1]) == "map" {
							sites = append(sites, filepath.ToSlash(path)+":"+fn.Name.Name)
						}
					}

					return true
				})

				return false
			})

			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	if len(sites) != 1 || !strings.HasSuffix(sites[0], "internal/store/ceph/ceph.go:rbdMap") {
		t.Errorf("`device map` is built at %v, want only rbdMap in internal/store/ceph/ceph.go", sites)
	}
}

func stringLit(expr ast.Expr) string {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}

	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return ""
	}

	return s
}
