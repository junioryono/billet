package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/store/ceph"
)

type keyReader struct {
	report ceph.GenerationReport
	err    error
}

func (r keyReader) KeyGenerations(context.Context) (ceph.GenerationReport, error) {
	return r.report, r.err
}

// THE STATUS NAMES THE KEYS HOLDING THE MOST GENERATIONS, the ten biggest and how many
// more, on a node whose site keeps its caches in Ceph; a read it cannot make is said,
// and a host with no ceph block says nothing about it.
//
// Not parallel: it replaces the package's openKeyGenerations.
func TestTheCacheStatusNamesTheKeysHoldingTheMostGenerations(t *testing.T) {
	saved := openKeyGenerations
	t.Cleanup(func() { openKeyGenerations = saved })

	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	cfg := &config.Config{Node: &config.NodeConfig{Ceph: &config.CephConfig{CachePool: "billet-cache"}}}

	keys := make([]ceph.KeyGenerations, 0, 12)
	keys = append(keys, ceph.KeyGenerations{Key: "untrusted/o/r/any/go", Generations: 845, Superseded: 844,
		LastUsed: now.Add(-90 * time.Minute)})
	for i := range 11 {
		keys = append(keys, ceph.KeyGenerations{Key: fmt.Sprintf("key-%02d", i), Generations: 1})
	}

	for _, tc := range []struct {
		name   string
		cfg    *config.Config
		reader keyReader
		open   error
		want   []string
		absent []string
	}{
		{name: "the biggest keys", cfg: cfg, reader: keyReader{report: ceph.GenerationReport{Keys: keys, Generations: 850}},
			want: []string{
				"cache generations in billet-cache: 850 across 12 key(s)",
				"845          844         1h30m0s ago  untrusted/o/r/any/go",
				"never", "(and 2 more key(s))",
			},
			absent: []string{"key-09", "key-10", "UNCHECKED", "could not be read"}},
		{name: "records it could not read", cfg: cfg,
			reader: keyReader{report: ceph.GenerationReport{Keys: keys[:1], Generations: 845, Unreadable: 3}},
			want:   []string{"3 index record(s) could not be read, so these counts may be low"}},
		{name: "an index that cannot be read", cfg: cfg, reader: keyReader{err: errors.New("ceph: list the cache index: timeout")},
			want: []string{"cache generations UNCHECKED: ceph: list the cache index: timeout"}, absent: []string{"across"}},
		{name: "a store that cannot be opened", cfg: cfg, open: errors.New("ceph: no rbd"),
			want: []string{"cache generations UNCHECKED: ceph: no rbd"}},
		{name: "no ceph block", cfg: &config.Config{Node: &config.NodeConfig{}},
			absent: []string{"cache generations"}},
		{name: "no node", cfg: &config.Config{}, absent: []string{"cache generations"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opened := false
			openKeyGenerations = func(*config.CephConfig) (keyGenerationReader, error) {
				opened = true
				if tc.open != nil {
					return nil, tc.open
				}

				return tc.reader, nil
			}

			var out bytes.Buffer

			reportKeyGenerations(t.Context(), &out, tc.cfg, now)

			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("said:\n%s\nwant it to say %q", out.String(), w)
				}
			}

			for _, a := range tc.absent {
				if strings.Contains(out.String(), a) {
					t.Errorf("said:\n%s\nwhich must not include %q", out.String(), a)
				}
			}

			if (tc.cfg.Node == nil || tc.cfg.Node.Ceph == nil) && opened {
				t.Error("opened the store on a host with no ceph block")
			}
		})
	}
}

// billet cache status ASKS, unconditionally, with the configuration it loaded, BEFORE
// the return a node with no server section takes: a statement of its own in the
// command's body, not one nested in a branch that could skip it.
func TestTheCacheStatusAsksForTheKeyGenerations(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "cachestatus.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "cmdCacheStatus" {
			continue
		}

		asked, nodeReturn := -1, -1

		for i, stmt := range fn.Body.List {
			if expr, ok := stmt.(*ast.ExprStmt); ok && asked < 0 {
				call, ok := expr.X.(*ast.CallExpr)
				if !ok {
					continue
				}

				id, ok := call.Fun.(*ast.Ident)
				if !ok || id.Name != "reportKeyGenerations" {
					continue
				}

				if arg, ok := call.Args[2].(*ast.Ident); !ok || arg.Name != "cfg" {
					t.Errorf("reportKeyGenerations is called with %T, not the loaded cfg", call.Args[2])
				}

				asked = i
			}

			// THE RETURN A NODE-ONLY CONFIGURATION TAKES: if cfg.Server == nil.
			if ifStmt, ok := stmt.(*ast.IfStmt); ok && nodeReturn < 0 {
				cond, ok := ifStmt.Cond.(*ast.BinaryExpr)
				if !ok || cond.Op != token.EQL {
					continue
				}

				if sel, ok := cond.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "Server" {
					nodeReturn = i
				}
			}
		}

		if asked < 0 || nodeReturn < 0 || asked > nodeReturn {
			t.Fatalf("cmdCacheStatus calls reportKeyGenerations as statement %d and returns for a node with no "+
				"server section at %d; it must call it as a statement of its own body, before that return",
				asked, nodeReturn)
		}

		return
	}

	t.Fatal("cachestatus.go has no cmdCacheStatus")
}
