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
	keys []ceph.KeyGenerations
	err  error
}

func (r keyReader) KeyGenerations(context.Context) ([]ceph.KeyGenerations, error) {
	return r.keys, r.err
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
		{name: "the biggest keys", cfg: cfg, reader: keyReader{keys: keys},
			want: []string{
				"cache generations in billet-cache: 856 across 12 key(s)",
				"845          844         1h30m0s ago  untrusted/o/r/any/go",
				"never", "(and 2 more key(s))",
			},
			absent: []string{"key-09", "key-10", "UNCHECKED"}},
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

// billet cache status ASKS, unconditionally: a statement of its own in the command's
// body, not one nested in a branch that could skip it.
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

		for _, stmt := range fn.Body.List {
			expr, ok := stmt.(*ast.ExprStmt)
			if !ok {
				continue
			}

			if call, ok := expr.X.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "reportKeyGenerations" {
					return
				}
			}
		}

		t.Fatal("cmdCacheStatus does not call reportKeyGenerations as a statement of its own body")
	}

	t.Fatal("cachestatus.go has no cmdCacheStatus")
}
