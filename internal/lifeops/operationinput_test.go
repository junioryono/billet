package lifeops

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRequiredRetainedInputsRejectVolatileRoots(t *testing.T) {
	for _, path := range []string{"/run/billet/locks/tls/node.key", "/tmp/billet.yaml", "/var/tmp/node.crt", "/var/run/billet/node.key"} {
		t.Run(path, func(t *testing.T) {
			if strings.HasPrefix(path, "/var/run/") && runtime.GOOS != "linux" {
				t.Skip("/var/run resolves to /run on the supported systemd host")
			}
			i := NewInspector()
			if err := i.AdmitRetainedInputs([]string{"/etc/billet/tls/node.crt", "/etc/billet/tls/node.key", "/etc/billet/tls/ca.crt"}); err != nil {
				t.Fatalf("persistent TLS bundle refused: %v", err)
			}
			if err := i.AdmitRetainedInputs([]string{path}); err == nil || !strings.Contains(err.Error(), "retained-input-volatile") {
				t.Fatalf("volatile input admitted: %v", err)
			}
		})
	}
}

func TestRequiredRetainedInputsProtectEveryTraversalEntry(t *testing.T) {
	root := t.TempDir()
	run, persistent := filepath.Join(root, "run"), filepath.Join(root, "etc", "billet", "tls")
	for _, dir := range []string{run, persistent} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	key := filepath.Join(persistent, "node.key")
	if err := os.WriteFile(key, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolvedKey, err := filepath.EvalSymlinks(key)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(run, "tls")
	if err := os.Symlink(persistent, link); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.Symlink(link, outside); err != nil {
		t.Fatal(err)
	}
	varRun := filepath.Join(root, "var-run")
	if err := os.Symlink(run, varRun); err != nil {
		t.Fatal(err)
	}
	i := NewInspector(WithRetainedInputRoots(run))
	if err := i.AdmitRetainedInputs([]string{key}); err != nil {
		t.Fatalf("persistent control refused: %v", err)
	}
	for _, path := range []string{filepath.Join(link, "node.key"), filepath.Join(outside, "node.key"), filepath.Join(varRun, "tls", "node.key")} {
		resolved, err := ResolveOperationPath(path)
		if err != nil || resolved != resolvedKey {
			t.Fatalf("traversal fixture does not end at the persistent key: %s %v", resolved, err)
		}
		if err := i.AdmitRetainedInputs([]string{path}); err == nil || !strings.Contains(err.Error(), "retained-input-volatile") {
			t.Fatalf("volatile intermediate entry admitted: %s: %v", path, err)
		}
	}
	cycle, outsideCycle := filepath.Join(run, "cycle"), filepath.Join(root, "outside-cycle")
	if err := os.Symlink(cycle, cycle); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cycle, outsideCycle); err != nil {
		t.Fatal(err)
	}
	if err := i.AdmitRetainedInputs([]string{outsideCycle}); err == nil || !strings.Contains(err.Error(), "retained-input-volatile") {
		t.Fatalf("incomplete walk discarded a proved volatile prefix: %v", err)
	}
	// A volatile root may itself be an alias to another filesystem location.
	i = NewInspector(WithRetainedInputRoots(varRun))
	if err := i.AdmitRetainedInputs([]string{filepath.Join(outside, "node.key")}); err == nil || !strings.Contains(err.Error(), "retained-input-volatile") {
		t.Fatalf("resolved volatile root ignored: %v", err)
	}
}

func TestRequiredRetainedInputsAreCheckedAtEveryAdmission(t *testing.T) {
	for _, verb := range []string{"", "enable", "disable", "stop", "start"} {
		t.Run(verb, func(t *testing.T) {
			f := newOperationFixture(t)
			f.unit(t, "billet-node.service")["RuntimeDirectory"] = "billet/locks billet/registration"
			p := OperationProtection{Units: []string{"billet-node.service"},
				UnitPaths:      map[string][]string{"billet-node.service": {"/run/billet/locks", "/run/billet/registration"}},
				RequiredInputs: map[string][]string{"billet-node.service": {"/etc/billet/tls/node.key"}}}
			var sequence []Operation
			if verb != "" {
				sequence = []Operation{{Verb: verb, Unit: "billet-node.service"}}
			}
			if err := f.inspector.AdmitOperations(t.Context(), sequence, p); err != nil {
				t.Fatalf("persistent input with disposable own records refused: %v", err)
			}
			p.RequiredInputs["billet-node.service"] = []string{"/run/billet/locks/tls/node.key"}
			if err := f.inspector.AdmitOperations(t.Context(), sequence, p); err == nil || !strings.Contains(err.Error(), "retained-input-volatile") {
				t.Fatalf("own runtime directory exempted required input: %v", err)
			}
		})
	}
}

func TestRequiredRetainedInputsRefuseUnknownTraversal(t *testing.T) {
	root := t.TempDir()
	loop := filepath.Join(root, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	i := NewInspector(WithRetainedInputRoots(filepath.Join(root, "volatile")))
	if err := i.AdmitRetainedInputs([]string{loop}); err == nil || !strings.Contains(err.Error(), "operation-path-unknown: symlink bound") {
		t.Fatalf("unresolved required input admitted: %v", err)
	}
}
