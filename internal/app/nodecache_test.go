package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

func TestCacheNamespacesIncludeTheSite(t *testing.T) {
	t.Parallel()

	if got := CacheNamespace("deployment-1", "home"); got != "deployment-1/home" {
		t.Errorf("named site namespace = %q", got)
	}
	if got := CacheNamespace("deployment-1", ""); got != "deployment-1/local" {
		t.Errorf("implicit site namespace = %q", got)
	}
	if CacheNamespace("deployment-1", "home") == CacheNamespace("deployment-1", "aws-us-west-2") {
		t.Fatal("two sites share one cache namespace")
	}
}

// EVICTION ASKS THE NODE'S SESSIONS AFRESH, AND AN ANSWER IT CANNOT READ IS NOT
// "NONE". A record written after the reader was built still protects its volume,
// and a cache node whose records are missing names nothing as free.
func TestEvictionReadsTheNodesCacheSessionsEachTime(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Node: &config.NodeConfig{
		StateDir: t.TempDir(), Cache: &config.NodeCacheConfig{},
	}}
	read := CacheSessionNames(cfg)

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

	var found bool

	ast.Inspect(nodeFunc(t, "nodecache.go", "startNodeCache"), func(n ast.Node) bool {
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
			if nameOK && cfgOK && name.Name == "CacheSessionNames" && cfg.Name == "cfg" {
				found = true
			}
		}

		return true
	})

	if !found {
		t.Fatal("startNodeCache builds its ceph store without ceph.WithCacheSessions(CacheSessionNames(cfg))")
	}
}

// A HANDOVER LETS THE CACHE FINISH WHAT IT IS SENDING (#374), within what the
// leases can spare. The guests outlive a node that hands over, so a transfer cut at
// the old five seconds fails a job; but nothing renews their leases until the next
// process registers, so the grace stays within a sixth of the TTL. A drain stops
// the listener after its jobs, and keeps the short grace.
func TestTheNodeCacheStopsWithAHandoverGrace(t *testing.T) {
	t.Parallel()

	if got := nodeCacheStopGrace(true); got <= 5*time.Second || got > alloc.DefaultLeaseTTL/6 {
		t.Errorf("a handover gives in-flight cache requests %v, want more than 5s and at most %v",
			got, alloc.DefaultLeaseTTL/6)
	}
	if got := nodeCacheStopGrace(false); got != 5*time.Second {
		t.Errorf("a drain gives in-flight cache requests %v, want 5s", got)
	}
}

// AND THE NODE STARTS ITS CACHE WITH IT, the listener's Shutdown waits that
// long, and the cache serves only once the loop says the node is ready. No
// run-time test reaches startNodeCache without a provider and a listener, so
// this one reads the source.
func TestTheNodeCacheIsGivenItsStopGrace(t *testing.T) {
	t.Parallel()

	var passed, honoured, shutdown, gated bool

	ast.Inspect(nodeFunc(t, "node.go", "Run"), func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.KeyValueExpr:
			key, keyOK := node.Key.(*ast.Ident)
			value, valueOK := node.Value.(*ast.Ident)
			if keyOK && valueOK && key.Name == "Ready" && value.Name == "serveCache" {
				gated = true
			}
		case *ast.CallExpr:
			name, ok := node.Fun.(*ast.Ident)
			if !ok || name.Name != "startNodeCache" || len(node.Args) == 0 {
				return true
			}

			grace, ok := node.Args[len(node.Args)-1].(*ast.CallExpr)
			if !ok || len(grace.Args) != 1 {
				return true
			}

			fun, funOK := grace.Fun.(*ast.Ident)
			arg, argOK := grace.Args[0].(*ast.Ident)
			if funOK && argOK && fun.Name == "nodeCacheStopGrace" && arg.Name == "handOver" {
				passed = true
			}
		}

		return true
	})

	start := nodeFunc(t, "nodecache.go", "startNodeCache")

	ast.Inspect(start, func(inner ast.Node) bool {
		call, ok := inner.(*ast.CallExpr)
		if !ok {
			return true
		}

		if namesSelector(call.Fun, "context", "WithTimeout") && len(call.Args) == 2 {
			if grace, ok := call.Args[1].(*ast.Ident); ok && grace.Name == "stopGrace" {
				honoured = true
			}
		}

		if namesSelector(call.Fun, "srv", "Shutdown") && len(call.Args) == 1 {
			if arg, ok := call.Args[0].(*ast.Ident); ok && arg.Name == "shutdownCtx" {
				shutdown = true
			}
		}

		return true
	})

	if !passed {
		t.Error("the node starts its cache without nodeCacheStopGrace(handOver)")
	}
	if !gated {
		t.Error("the node loop is not given serveCache as Ready, so the cache answers before registration")
	}
	if !honoured || !shutdown {
		t.Error("startNodeCache's srv.Shutdown(shutdownCtx) does not wait stopGrace")
	}
}

// THE CACHE SERVES ONLY FROM ITS ONE READY PATH, AND ONLY AFTER ITS MOUNTS ARE
// RESTORED. startNodeCache calls srv.Serve exactly once, inside the function
// literal Ready runs, after service.RestoreMounts in that same literal: an eager
// Serve elsewhere, or one ahead of the remount, would answer a handed-over guest
// from an empty directory or from a process the control plane does not know.
func TestTheNodeCacheServesOnlyAfterRestoringItsMounts(t *testing.T) {
	t.Parallel()

	var (
		serves  int
		ordered bool
	)

	ast.Inspect(nodeFunc(t, "nodecache.go", "startNodeCache"), func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && namesSelector(call.Fun, "srv", "Serve") {
			serves++
		}

		// IN A GO STATEMENT, because Ready is called on the node loop's own
		// goroutine and a remount that stalls there would hold registration.
		statement, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}
		literal, ok := statement.Call.Fun.(*ast.FuncLit)
		if !ok {
			return true
		}

		var restoredAt, servedAt token.Pos
		for _, statement := range literal.Body.List {
			// RESTORED BY THE SERVING GOROUTINE ITSELF, as a statement of its own
			// before Serve: a restore in another goroutine, a deferred call or a
			// closure would still be running, or not yet run, when Serve answers.
			if expr, ok := statement.(*ast.ExprStmt); ok {
				if call, ok := expr.X.(*ast.CallExpr); ok && namesSelector(call.Fun, "service", "RestoreMounts") && restoredAt == 0 {
					restoredAt = call.Pos()
				}
			}

			ast.Inspect(statement, func(inner ast.Node) bool {
				if call, ok := inner.(*ast.CallExpr); ok && namesSelector(call.Fun, "srv", "Serve") && servedAt == 0 {
					servedAt = call.Pos()
				}

				return true
			})
		}
		if restoredAt != 0 && servedAt != 0 && restoredAt < servedAt {
			ordered = true
		}

		return true
	})

	if serves != 1 {
		t.Errorf("startNodeCache calls srv.Serve %d times, want exactly the one Ready runs", serves)
	}
	if !ordered {
		t.Error("startNodeCache does not restore the recovered mounts, then serve, in a goroutine of its own")
	}
}

// THE NODE LOOP IS GIVEN THE STOP POLICY AND THE DRAIN REQUEST. The loop's
// tests set both themselves, so only the source shows that Run passes what the
// config said and what the host says a draining stop asks.
func TestTheNodeLoopIsGivenItsStopPolicy(t *testing.T) {
	t.Parallel()

	found := map[string]bool{}

	ast.Inspect(nodeFunc(t, "node.go", "Run"), func(n ast.Node) bool {
		literal, ok := n.(*ast.CompositeLit)
		if !ok || !namesSelector(literal.Type, "nodeclient", "LoopOptions") {
			return true
		}
		for _, element := range literal.Elts {
			field, ok := element.(*ast.KeyValueExpr)
			if !ok {
				continue
			}

			key, ok := field.Key.(*ast.Ident)

			switch {
			case !ok:
			case key.Name == "HandOverOnStop":
				if value, ok := field.Value.(*ast.Ident); ok && value.Name == "handOver" {
					found[key.Name] = true
				}
			case key.Name == "DrainRequested", key.Name == "Hurry":
				if namesSelector(field.Value, "host", key.Name) {
					found[key.Name] = true
				}
			}
		}

		return true
	})

	for _, key := range []string{"HandOverOnStop", "DrainRequested", "Hurry"} {
		if !found[key] {
			t.Errorf("the node loop is not given %s from what the config and the host say", key)
		}
	}
}

// THE NODE SAYS IT IS READY ONCE, AFTER ITS RUNNER IS BUILT AND BEFORE THE
// LOOP (NewNodeRunner and RunNodeLoop, the runtime billet's harnesses run
// too), and the guest cache answers only from the loop's readiness. A READY=1
// sent before the runner exists tells the service manager a node is serving
// that cannot take a command; one never sent leaves the unit starting until
// systemd kills it. And serveCache called anywhere but as the loop's Ready
// answers a guest before this process is registered and recovered.
func TestTheNodeIsReadyBeforeItsLoopAndServesTheCacheOnlyFromIt(t *testing.T) {
	t.Parallel()

	run := nodeFunc(t, "node.go", "Run")

	var built, ready, looped token.Pos

	readies, eager := 0, 0

	// SAID WHERE IT STANDS: a host.Ready in a function literal, a deferred or
	// a go statement runs at another time than its place in the source.
	ast.Inspect(run.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit, *ast.DeferStmt, *ast.GoStmt:
			ast.Inspect(x, func(inner ast.Node) bool {
				if call, ok := inner.(*ast.CallExpr); ok && namesSelector(call.Fun, "host", "Ready") {
					t.Error("Node.Run calls host.Ready from a closure, a deferred or a go statement")
				}

				return true
			})

			return false
		}

		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		id, isIdent := call.Fun.(*ast.Ident)

		switch {
		case isIdent && id.Name == "NewNodeRunner" && !built.IsValid():
			built = call.Pos()
		case namesSelector(call.Fun, "host", "Ready"):
			readies++
			ready = call.Pos()
		case isIdent && id.Name == "RunNodeLoop" && !looped.IsValid():
			looped = call.Pos()
		}

		return true
	})

	if readies != 1 || !built.IsValid() || !looped.IsValid() || ready < built || ready > looped {
		t.Errorf("Node.Run calls host.Ready %d times, want once after NewNodeRunner and before RunNodeLoop", readies)
	}

	// EVERYWHERE IN Run, closures, deferred and go statements included: a
	// `go serveCache()` serves as early as a direct call.
	ast.Inspect(run.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "serveCache" {
				eager++
			}
		}

		return true
	})

	if eager != 0 {
		t.Errorf("Node.Run calls serveCache itself %d times; the cache is served only from the loop's Ready", eager)
	}

	// AND startNodeCache NEVER CALLS THE serve IT RETURNS: the one Serve is in
	// that callback (TestTheNodeCacheServesOnlyAfterRestoringItsMounts), so a
	// call of it here would serve before the loop is ready.
	ast.Inspect(nodeFunc(t, "nodecache.go", "startNodeCache").Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "serve" {
				t.Error("startNodeCache calls the serve callback it returns, so the cache answers before the loop is ready")
			}
		}

		return true
	})
}

// nodeFunc parses file, one of this package's sources, and returns its
// function or method named name.
func nodeFunc(t *testing.T, file, name string) *ast.FuncDecl {
	t.Helper()

	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}

	t.Fatalf("%s declares no %s", file, name)

	return nil
}

// namesSelector reports whether expr is pkg.name.
func namesSelector(expr ast.Expr, pkg, name string) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
		return false
	}

	ident, ok := selector.X.(*ast.Ident)

	return ok && ident.Name == pkg
}
