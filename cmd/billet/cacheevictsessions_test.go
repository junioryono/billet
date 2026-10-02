package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// EVICTION ASKS THE NODE'S SESSIONS AFRESH, AND AN ANSWER IT CANNOT READ IS NOT
// "NONE". A record written after the reader was built still protects its volume,
// and a cache node whose records are missing names nothing as free.
func TestEvictionReadsTheNodesCacheSessionsEachTime(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Node: &config.NodeConfig{
		StateDir: t.TempDir(), Cache: &config.NodeCacheConfig{},
	}}
	read := cacheSessionNames(cfg)

	if _, err := read(); err == nil {
		t.Error("a cache node whose sessions could not be read named none")
	}

	sessions := filepath.Join(cfg.Node.StateDir, "cache-sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatalf("make the sessions directory: %v", err)
	}

	const volume = "cache-v-1790048184-0123456789abcdef01234567"
	if err := os.WriteFile(filepath.Join(sessions, "record.json"),
		[]byte(`{"handle":"billet-cache/`+volume+`"}`), 0o600); err != nil {
		t.Fatalf("write a session record: %v", err)
	}

	inSession, err := read()
	if err != nil {
		t.Fatalf("read the sessions: %v", err)
	}

	if !inSession(volume) || inSession("cache-v-1790048184-fedcba9876543210fedcba98") {
		t.Error("the sessions did not name exactly the volume a record holds")
	}
}

// AND THE NODE'S CACHE STORE IS BUILT WITH THAT READER. Without it Evict keeps
// every writable volume, which fails safe and leaks them all; no run-time test
// reaches startNodeCache without a provider and a listener, so this one reads it.
func TestTheNodeCacheStoreEvictsWithTheNodesSessions(t *testing.T) {
	t.Parallel()

	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var found bool

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "startNodeCache" {
			continue
		}

		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !namesSelector(call.Fun, "ceph", "New") {
				return true
			}

			for _, arg := range call.Args {
				option, ok := arg.(*ast.CallExpr)
				if !ok || !namesSelector(option.Fun, "ceph", "WithCacheSessions") || len(option.Args) != 1 {
					continue
				}

				reader, ok := option.Args[0].(*ast.CallExpr)
				if !ok || len(reader.Args) != 1 {
					continue
				}

				name, nameOK := reader.Fun.(*ast.Ident)
				cfg, cfgOK := reader.Args[0].(*ast.Ident)
				if nameOK && cfgOK && name.Name == "cacheSessionNames" && cfg.Name == "cfg" {
					found = true
				}
			}

			return true
		})
	}

	if !found {
		t.Fatal("startNodeCache builds its ceph store without ceph.WithCacheSessions(cacheSessionNames(cfg))")
	}
}

func namesSelector(expr ast.Expr, pkg, name string) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
		return false
	}

	ident, ok := selector.X.(*ast.Ident)

	return ok && ident.Name == pkg
}
