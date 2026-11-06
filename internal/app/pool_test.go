package app

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/wirecert"
)

func poolBootConfig() *config.Config {
	return &config.Config{
		GitHub: &config.GitHubConfig{Org: "acme"},
		Server: &config.ServerConfig{Pool: &config.PoolConfig{
			JoinServer: "10.77.0.10:7717", BootstrapAddr: "10.77.0.10:7718",
		}},
	}
}

// runBoot runs a boot as a host would, with /run in a temporary directory and billet a
// function that records its arguments, and returns those arguments and what the boot
// wrote to stderr.
func runBoot(t *testing.T, boot []byte, env ...string) (args []string, stderr string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "boot.sh")
	if err := os.WriteFile(script, []byte(strings.ReplaceAll(string(boot), "/run/", dir+"/")), 0o700); err != nil {
		t.Fatalf("write the boot: %v", err)
	}
	argsFile := filepath.Join(dir, "args")
	cmd := exec.CommandContext(t.Context(), "bash", "-c",
		`billet() { printf '%s\n' "$@" >"$ARGS"; }; export -f billet; bash "$SCRIPT"`)
	cmd.Env = append(append(os.Environ(), "ARGS="+argsFile, "SCRIPT="+script), env...)
	var errs strings.Builder
	cmd.Stderr = &errs
	if err := cmd.Run(); err != nil {
		t.Fatalf("the boot failed: %v\n%s", err, errs.String())
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("billet join never ran: %v\n%s", err, errs.String())
	}

	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n"), errs.String()
}

// A PRELUDE THAT FAILS STOPS THE BOOT BEFORE THE JOIN: it runs under errexit, nounset
// and pipefail of its own, so a failed command, an unset variable or a failed stage of a
// pipeline ends it, the join never spends the token, and the prelude's file is removed.
func TestAPreludeThatFailsStopsTheBootBeforeTheJoin(t *testing.T) {
	t.Parallel()
	for name, prelude := range map[string]string{
		"a failed command":        "false\necho after >&2\n",
		"an unset variable":       "echo \"$BILLET_POOL_NEVER_SET\"\n",
		"a failed pipeline stage": "false | true\necho after >&2\n",
		"an exit with a failure":  "exit 3\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			boot, err := PoolBoot(poolBootConfig(), prelude, "SHA256:ab", "p01", "tok")
			if err != nil {
				t.Fatalf("boot: %v", err)
			}
			dir := t.TempDir()
			script := filepath.Join(dir, "boot.sh")
			if err := os.WriteFile(script, []byte(strings.ReplaceAll(string(boot), "/run/", dir+"/")), 0o700); err != nil {
				t.Fatalf("write the boot: %v", err)
			}
			argsFile := filepath.Join(dir, "args")
			cmd := exec.CommandContext(t.Context(), "bash", "-c",
				`billet() { printf '%s\n' "$@" >"$ARGS"; }; export -f billet; bash "$SCRIPT"`)
			cmd.Env = append(os.Environ(), "ARGS="+argsFile, "SCRIPT="+script)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("a boot whose prelude failed succeeded:\n%s", out)
			}
			if strings.Contains(string(out), "after") {
				t.Errorf("the prelude ran on past its failure:\n%s", out)
			}
			if _, err := os.Stat(argsFile); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("billet join ran after the prelude failed (%v)", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "billet-pool-prelude.sh")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the prelude's file was left behind (%v)", err)
			}
		})
	}
}

// A NUL CANNOT BE CARRIED: bash drops it, so a prelude or a join argument holding one is
// refused rather than run as something else.
func TestABootHoldingANULIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := PoolBoot(poolBootConfig(), "echo a\x00b\n", "SHA256:ab", "p01", "tok"); err == nil {
		t.Fatal("a prelude holding a NUL was rendered")
	}
	if _, err := PoolBoot(poolBootConfig(), "true\n", "SHA256:ab", "p01", "to\x00k"); err == nil {
		t.Fatal("a token holding a NUL was rendered")
	}
}

// AN ORDERED HOST'S BOOT RUNS THE PRELUDE, THEN JOINS, with its order's own token, the
// deployment's authority and a node name drawn from the order; the token is one word
// whatever it holds, and a prelude that traces itself traces nothing of the join.
func TestAPoolHostsBootRunsThePreludeThenJoins(t *testing.T) {
	t.Parallel()
	const joinToken = "tok'en $(x)"
	// A prelude that ends with exit is a script of its own: it ends itself, not the boot.
	boot, err := PoolBoot(poolBootConfig(), "set -x\necho prelude-ran >&2\nexit 0\n", "SHA256:ab", "p01", joinToken)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	args, stderr := runBoot(t, boot)
	want := []string{"join", "--server", "10.77.0.10:7717", "--ca-fingerprint", "SHA256:ab",
		"--token", joinToken, "--node-name", "pool-p01", "--provider", "firecracker",
		"--bootstrap-addr", "10.77.0.10:7718", "--org", "acme"}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		t.Fatalf("join ran as %q, want %q", args, want)
	}
	if !strings.Contains(stderr, "prelude-ran") {
		t.Fatalf("the prelude did not run:\n%s", stderr)
	}
	if strings.Contains(stderr, "tok'en") || strings.Contains(stderr, "--token") {
		t.Fatalf("the join was traced, token and all:\n%s", stderr)
	}
	// AND A BOOT STARTED WITH TRACING ALREADY ON, inherited through SHELLOPTS, still
	// switches it off before the join.
	_, stderr = runBoot(t, boot, "SHELLOPTS=xtrace")
	if strings.Contains(stderr, "tok'en") || strings.Contains(stderr, "--token") {
		t.Fatalf("an inherited trace traced the join:\n%s", stderr)
	}
}

func TestAPoolBootIsRefusedPastTheUserDataLimitOrWithTheHeredocsEnd(t *testing.T) {
	t.Parallel()
	ok, err := PoolBoot(poolBootConfig(), "", "fp", "p01", "tok")
	if err != nil {
		t.Fatalf("an empty prelude: %v", err)
	}
	room := maxBootPayload - len(ok)
	if _, err := PoolBoot(poolBootConfig(), strings.Repeat("x", room), "fp", "p01", "tok"); err != nil {
		t.Fatalf("a boot of exactly the limit was refused: %v", err)
	}
	if _, err := PoolBoot(poolBootConfig(), strings.Repeat("x", room+1), "fp", "p01", "tok"); err == nil ||
		!strings.Contains(err.Error(), "boot_prelude") {
		t.Fatalf("a boot one byte past the limit answered %v", err)
	}
	if _, err := PoolBoot(poolBootConfig(), "echo a\n"+preludeEnd+"\necho b\n", "fp", "p01", "tok"); err == nil ||
		!strings.Contains(err.Error(), preludeEnd) {
		t.Fatalf("a prelude carrying the heredoc's end answered %v", err)
	}
}

// EACH ORDER'S BOOT CARRIES THE AUTHORITY AS IT IS when the order is sent, read from the
// identity directory, under that order's own name and token.
func TestEachOrdersBootCarriesTheDeploymentsAuthority(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ca, err := wirecert.LoadOrCreateCA(dir, "dep-1")
	if err != nil {
		t.Fatalf("ca: %v", err)
	}
	cfg := poolBootConfig()
	cfg.Server.IdentityDir = dir
	boot, err := poolBootFor(cfg, "true")("p02", "tok-2")
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	args, _ := runBoot(t, boot)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--ca-fingerprint "+ca.Fingerprint()) || !strings.Contains(joined, "--token tok-2") ||
		!strings.Contains(joined, "--node-name pool-p02") {
		t.Fatalf("join ran as %q", args)
	}
	cfg.Server.IdentityDir = t.TempDir()
	if _, err := poolBootFor(cfg, "true")("p03", "tok-3"); err == nil {
		t.Fatal("a boot was rendered with no authority to name")
	}
}

// THE POOL IS THE CONTROLLER'S, AND A DRY RUN ORDERS NOTHING: schedulePool returns no
// loop without a pool or on a dry run, a loop otherwise, and refuses a prelude whose
// boot could never be sent, before any order is placed.
func TestThePoolIsScheduledOnlyWhenConfiguredAndNotDry(t *testing.T) {
	t.Parallel()
	prelude := filepath.Join(t.TempDir(), "prelude.sh")
	if err := os.WriteFile(prelude, []byte("true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := poolBootConfig()
	if loop, ok, err := schedulePool(&config.Config{Server: &config.ServerConfig{}}, nil, "dep", false); loop != nil || ok || err != nil {
		t.Fatalf("no pool: %v, %v", loop != nil, err)
	}
	cfg.Server.Pool.BootPrelude = prelude
	if loop, ok, err := schedulePool(cfg, nil, "dep", true); loop != nil || ok || err != nil {
		t.Fatalf("a dry run: %v, %v", loop != nil, err)
	}
	if loop, ok, err := schedulePool(cfg, nil, "dep", false); loop == nil || !ok || err != nil {
		t.Fatalf("a configured pool: %v, %v", loop != nil, err)
	}
	if err := os.WriteFile(prelude, []byte(strings.Repeat("x", maxBootPayload)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := schedulePool(cfg, nil, "dep", false); err == nil || !strings.Contains(err.Error(), "boot_prelude") {
		t.Fatalf("an oversized prelude answered %v", err)
	}
}

// AND SCHEDULE STARTS WHAT schedulePool RETURNS, as the "pool" background loop, so the
// pool is the controller's and nothing else starts one.
func TestScheduleStartsThePoolLoop(t *testing.T) {
	t.Parallel()
	f, err := parser.ParseFile(token.NewFileSet(), "controlplane.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var called, started bool
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Schedule" {
			return true
		}
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				if fun.Name == "schedulePool" {
					called = true
				}
			case *ast.SelectorExpr:
				if fun.Sel.Name == "Background" && len(call.Args) == 2 {
					if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Value == `"pool"` {
						if id, ok := call.Args[1].(*ast.Ident); ok && id.Name == "poolLoop" {
							started = true
						}
					}
				}
			}

			return true
		})

		return false
	})
	if !called || !started {
		t.Fatalf("Schedule calls schedulePool: %t; starts its loop as \"pool\": %t", called, started)
	}
}

// PHOENIXNAP'S CREDENTIALS ARE READ FROM THE ENVIRONMENT, and an empty one stops the
// assembly naming the variables, rather than a pool that fails every order.
func TestAPoolWithAnEmptyCredentialVariableIsRefused(t *testing.T) {
	prelude := filepath.Join(t.TempDir(), "prelude.sh")
	cfg := poolBootConfig()
	cfg.Server.Pool.BootPrelude = prelude
	cfg.Server.Pool.PhoenixNAP = &config.PoolPhoenixNAP{
		ClientIDEnv: "BILLET_TEST_POOL_ID", ClientSecretEnv: "BILLET_TEST_POOL_SECRET",
		Shapes: []config.PoolShape{{Type: "s2.c2.small", Arch: config.ArchX64}},
	}
	t.Setenv("BILLET_TEST_POOL_ID", "client")
	t.Setenv("BILLET_TEST_POOL_SECRET", "")
	if _, _, err := NewPool(cfg, nil, "dep", nil); err == nil || !strings.Contains(err.Error(), "BILLET_TEST_POOL_SECRET") {
		t.Fatalf("an empty secret variable answered %v", err)
	}
}
