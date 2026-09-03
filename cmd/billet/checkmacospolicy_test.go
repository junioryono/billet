package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE POLICY LINE SAYS WHAT A codebuild NODE CAN RUN, NOT WHAT A tart NODE CAN.
//
// MEASURED 2026-09-02: `billet check` printed "macOS n/a (codebuild cannot run
// macOS guests)" beside a node policy whose MAC_ARM fleet had run an Xcode job
// eleven seconds after dispatch. The line asked `!= tart`, a second copy of the
// macOS allowlist written before codebuild joined it; it now asks config, which
// owns the list. Driven through runCheck, because the rendering is the thing that
// was wrong and a test on the predicate alone would leave it in place.
func TestTheCheckPolicyLineReportsACodeBuildMacOSLimit(t *testing.T) {
	t.Setenv("BILLET_MAINTENANCE", "")

	dir := t.TempDir()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}

	keyPath := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600); err != nil {
		t.Fatalf("write the key: %v", err)
	}

	// A control-plane config that DESCRIBES a codebuild node without BEING one, so
	// the policy line is rendered and no AWS call is made: the live CodeBuild check
	// runs only for a config whose own node is codebuild.
	cfgPath := filepath.Join(dir, "billet.yaml")
	body := fmt.Sprintf(`server:
  listen: 127.0.0.1:7717
  state_dir: %s
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 7
  installation_id: 42
  private_key_path: %s
nodes:
  - name: cb-mac
    provider: codebuild
    guest_os: [macos]
    macos_vm_limit: 1
  - name: cb-nolimit
    provider: codebuild
    guest_os: [macos]
  - name: fc-box
    provider: firecracker
    guest_os: [linux]
tiers:
  - label: billet-4vcpu
    provider: docker
    vcpu: 4
    memory: 16GiB
    image: ghcr.io/actions/actions-runner:latest
    trust: trusted
    runner_group: billet-trial
    workflows:
      - acme/repo/.github/workflows/ci.yml@refs/heads/main
`, filepath.Join(dir, "server"), keyPath)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	pointGitHubAt(t, exactInstallation)

	out := capture(t, func() {
		if _, err := runCheck(t.Context(), checkOptions{configPath: cfgPath}); err != nil {
			t.Errorf("runCheck: %v", err)
		}
	})

	var codebuild, undeclared, firecracker string

	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "policy cb-mac"):
			codebuild = line
		case strings.Contains(line, "policy cb-nolimit"):
			undeclared = line
		case strings.Contains(line, "policy fc-box"):
			firecracker = line
		}
	}

	if !strings.Contains(codebuild, "max 1 macOS") || strings.Contains(codebuild, "n/a") {
		t.Errorf("the codebuild policy line does not report its declared macOS limit:\n%s\n\n%s",
			codebuild, out)
	}

	// A REMOTE NODE WITH NO LIMIT HAS NO APPLE DEFAULT. Its Macs are AWS's, so
	// "max 2 macOS (Apple default)" attributes a number to a party that did not set
	// it; the line has to say the limit is undeclared and what would require it.
	if !strings.Contains(undeclared, "undeclared") || strings.Contains(undeclared, "Apple default") {
		t.Errorf("the codebuild policy with no limit is reported against Apple's default:\n%s\n\n%s",
			undeclared, out)
	}

	// AND THE OTHER DIRECTION HOLDS: a backend that cannot run macOS still says so,
	// or the fix is a predicate that answers true for everything.
	if !strings.Contains(firecracker, "cannot run macOS guests") {
		t.Errorf("the firecracker policy line no longer says it cannot run macOS:\n%s\n\n%s",
			firecracker, out)
	}
}
