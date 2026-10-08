package host

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Helpers this package's tests share with cmd/billet's, copied rather than
// imported: a test helper is not part of any package's API.

// forkSafeWriteFile writes an executable a test is about to run.
//
// UNDER syscall.ForkLock, which every os/exec start holds while it creates its
// child: a fork by another parallel test while this file is open for writing
// inherits the descriptor, and exec of the file then fails with ETXTBSY ("text
// file busy") until that child execs (Go issue 22315). A shell that meets it on a
// shebang interpreter reports exit 126 and fails the test for nothing.
func forkSafeWriteFile(name string, data []byte, perm os.FileMode) error {
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()

	return os.WriteFile(name, data, perm)
}

// testKey returns a valid PEM-encoded App private key.
func testKey(t *testing.T) []byte {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// asLinux pins the generation to the systemd shape — /etc/billet, /var/lib, the
// billet group, mode 0640 — on the darwin machines billet is developed on. Not
// parallel-safe; none of these tests are parallel.
// asLinux says this host is Linux to EVERY seam that asks, not only to the
// command layer's own: the retirement's platform decides whether the global
// authority exclusion exists at all, and a test that pinned one and not the
// other proved its ordering against a host no fleet runs (found by CI, which
// runs where both are Linux for real, 2026-09-12).
func asLinux(t *testing.T) {
	t.Helper()
	useRetirementRoot(t)
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

// calleeName is the bare function or method name a call invokes.
func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	default:
		return ""
	}
}

// writeAdminConfig is writeCAConfig with a REAL App private key, which cmdCheck
// insists on before it ever reaches the state directory.
func writeAdminConfig(t *testing.T, stateDir string) string {
	t.Helper()

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "app.pem")

	if err := os.WriteFile(keyPath, testKey(t), 0o600); err != nil {
		t.Fatalf("write the app key: %v", err)
	}

	path := filepath.Join(dir, "billet.yaml")

	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: ` + stateDir + `
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: ` + keyPath + `
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

	return path
}
