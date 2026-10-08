package images

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// Helpers this package's tests share with cmd/billet's, copied rather than
// imported: a test helper is not part of any package's API.

// captureStderr runs fn with stderr redirected and returns what it wrote.
//
// Restored with t.Cleanup rather than a bare reassignment, so a t.Fatal inside
// fn cannot leave every later test in the package writing into a dead pipe.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	saved := os.Stderr

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	os.Stderr = w
	t.Cleanup(func() { os.Stderr = saved })

	done := make(chan string, 1)

	go func() {
		var b strings.Builder

		_, _ = io.Copy(&b, r) //nolint:errcheck // the write end is closed below, ending the copy

		done <- b.String()
	}()

	fn()

	os.Stderr = saved

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}

	return <-done
}

// capture redirects stdout for the duration of fn and returns what was written.
//
// `billet check` REPORTS to an operator, so what it prints is the whole product
// and asserting only its error return would leave the interesting half untested.
func capture(t *testing.T, fn func()) string {
	t.Helper()

	saved := os.Stdout

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	os.Stdout = w

	// RESTORED BY CLEANUP, not only by the happy path below. A t.Fatal inside fn
	// unwinds past the restore, leaving every later test in the package writing
	// into a pipe nobody reads — which surfaces as an unrelated test hanging or
	// losing its output, a long way from the test that actually failed.
	t.Cleanup(func() { os.Stdout = saved })

	done := make(chan string, 1)

	go func() {
		var b strings.Builder

		_, _ = io.Copy(&b, r) //nolint:errcheck // the write end is closed below, ending the copy

		done <- b.String()
	}()

	fn()

	os.Stdout = saved

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}

	return <-done
}

// findFunc parses this package's non-test sources and returns one declaration.
func findFunc(t *testing.T, name string) *ast.FuncDecl {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	fset := token.NewFileSet()

	for _, entry := range entries {
		file := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(file, ".go") ||
			strings.HasSuffix(file, "_test.go") {
			continue
		}

		parsed, err := parser.ParseFile(fset, filepath.Join(".", file), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}

		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && fn.Name.Name == name {
				return fn
			}
		}
	}

	t.Fatalf("%s is not declared in this package", name)

	return nil
}

// stmtCondition decodes init iam's printed policy and returns one statement's
// Condition as a generic map.
func stmtCondition(t *testing.T, out, sid string) map[string]any {
	t.Helper()

	var doc struct {
		Statement []struct {
			Sid       string         `json:"Sid"`
			Condition map[string]any `json:"Condition"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("policy JSON: %v\n%s", err, out)
	}
	for _, s := range doc.Statement {
		if s.Sid == sid {
			return s.Condition
		}
	}

	t.Fatalf("no statement %q in the policy", sid)

	return nil
}

// nodeConfigFor writes the config a freshly enrolled host would have.
func nodeConfigFor(t *testing.T, name, stateDir, bundleDir string) *config.Config {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "billet.yaml")

	body := `
node:
  name: ` + name + `
  server_addr: 10.0.0.4:7717
  provider: docker
  state_dir: ` + stateDir + `
  tls:
    cert: ` + filepath.Join(bundleDir, "node.crt") + `
    key: ` + filepath.Join(bundleDir, "node.key") + `
    ca: ` + filepath.Join(bundleDir, "ca.crt") + `
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: /tmp/key.pem
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load node config: %v", err)
	}

	return cfg
}

// noRootDisk stands in for the storage a preflight does not use.
//
// The provider refuses a nil one — every guest boots from a clone, so a nil
// interface would panic on the first job — and `billet check` proves the cluster
// separately, through the ceph client, where the diagnostic is about storage rather
// than about microVMs.
type noRootDisk struct{}

func (noRootDisk) ResolveGeneration(_ context.Context, image, _ string) (string, error) {
	return image, nil
}

func (noRootDisk) CloneRoot(context.Context, string, string, config.ByteSize) (string, error) {
	return "", errors.New("billet: the preflight does not clone a root disk")
}

func (noRootDisk) DiscardRoot(context.Context, string) error { return nil }

// KernelFor answers "nothing recorded", which is the truthful answer from a node
// with no cluster to have recorded anything in — and the caller treats it as the
// fallback case rather than an error.
func (noRootDisk) KernelFor(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}

// GenerationGone is false: a node with no cluster has no generations to lose, and
// answering true would have the launch re-resolve an alias forever.
func (noRootDisk) GenerationGone(error) bool { return false }
