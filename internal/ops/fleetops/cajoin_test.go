package fleetops

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/wirecert"
)

// bufferEnv is an env whose output a test reads.
func bufferEnv() (cli.Env, *bytes.Buffer) {
	var out bytes.Buffer

	return cli.Env{Stdout: &out, Stderr: &out, Getenv: os.Getenv}, &out
}

// `ca issue` NAMES BOTH PLATFORMS' PATHS (#181): the bundle is for another
// machine, and the Linux path alone told a Mac to write into /etc, which its
// services never read.
func TestCAIssueNamesWhereEachPlatformsNodeReadsItsBundle(t *testing.T) {
	t.Parallel()

	cfg := writeCAConfig(t, t.TempDir())
	env, out := bufferEnv()

	if err := CAIssue(t.Context(), env, []string{"mac-1", "--config", cfg, "--out", filepath.Join(t.TempDir(), "b")}); err != nil {
		t.Fatalf("ca issue: %v", err)
	}

	for _, want := range []string{"/etc/billet/tls/node.crt", "/usr/local/etc/billet/tls/node.crt",
		"/usr/local/etc/billet/tls/ca.crt", "# on a Mac"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("ca issue does not say %q:\n%s", want, out)
		}
	}
}

// listenAt rewrites a CA test config's node-wire listen.
func listenAt(t *testing.T, cfg, listen string) {
	t.Helper()

	raw, err := os.ReadFile(cfg)
	mustNil(t, err)
	mustNil(t, os.WriteFile(cfg, []byte(strings.Replace(string(raw), "listen: 127.0.0.1:7717", "listen: "+listen, 1)), 0o600))
}

// runCAToken runs `ca token` and returns its output and the token it minted.
func runCAToken(t *testing.T, cfg string) (string, string) {
	t.Helper()

	env, out := bufferEnv()
	if err := CAToken(t.Context(), env, []string{"--config", cfg}); err != nil {
		t.Fatalf("ca token: %v", err)
	}

	text := out.String()
	_, after, ok := strings.Cut(text, "use(s)):\n\n  ")
	if !ok {
		t.Fatalf("ca token printed no token:\n%s", text)
	}
	token, _, _ := strings.Cut(after, "\n")

	return text, token
}

// joinLine is the `billet join` line of a `ca token` output.
func joinLine(text string) string {
	for l := range strings.SplitSeq(text, "\n") {
		if strings.Contains(l, "billet join --server") {
			return strings.TrimSpace(l)
		}
	}

	return ""
}

// `ca token` PRINTS THE WHOLE `billet join` (#181): the address, the
// authority's fingerprint as `ca show` reports it, the token it minted and the
// target, so the new machine runs one command; and the fingerprint is read,
// never made, so a control plane with no authority yet is told to ask and
// gains none.
func TestCATokenPrintsTheWholeJoinCommand(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	listenAt(t, cfg, "10.0.0.1:7717")

	text, _ := runCAToken(t, cfg)
	if !strings.Contains(text, "--ca-fingerprint <from `billet ca show`>") {
		t.Errorf("with no authority the token does not say where the fingerprint comes from:\n%s", text)
	}
	if _, err := os.Stat(wirecert.CACertPath(stateDir)); !os.IsNotExist(err) {
		t.Errorf("printing the token created an authority (%v)", err)
	}

	// The authority `ca issue` creates, and the fingerprint `ca show` prints,
	// read from its own output rather than the helper the token's code calls.
	env, out := bufferEnv()
	if err := CAIssue(t.Context(), env, []string{"mac-1", "--config", cfg, "--out", filepath.Join(t.TempDir(), "b")}); err != nil {
		t.Fatalf("ca issue: %v", err)
	}
	env, out = bufferEnv()
	mustNil(t, CA(t.Context(), env, []string{"show", "--config", cfg}))
	fp := ""
	for l := range strings.SplitSeq(out.String(), "\n") {
		if rest, ok := strings.CutPrefix(l, "fingerprint "); ok {
			fp = strings.TrimSpace(rest)
		}
	}
	if fp == "" {
		t.Fatalf("ca show printed no fingerprint:\n%s", out)
	}

	text, token := runCAToken(t, cfg)
	line := joinLine(text)
	for _, want := range []string{"billet join --server 10.0.0.1:7717", "--ca-fingerprint " + fp,
		"--token " + token + " ", "--node-name <its name>", "--org acme"} {
		if !strings.Contains(line, want) {
			t.Errorf("the join command %q does not say %q:\n%s", line, want, text)
		}
	}
	if !strings.Contains(text, "billet node --enroll --ca-fingerprint "+fp+" --join-token "+token) {
		t.Errorf("the form for a configured machine is gone:\n%s", text)
	}
}

// A WILDCARD LISTEN LEAVES THE HOST TO THE OPERATOR, AND A LOOPBACK ONE PRINTS
// NO JOIN AT ALL: printed as --server, a loopback address names the joining
// machine itself, so the bind that makes the control plane reachable is said
// instead.
func TestCATokenPrintsAJoinOnlyForAnAddressAnotherMachineHas(t *testing.T) {
	t.Parallel()

	wild := writeCAConfig(t, t.TempDir())
	listenAt(t, wild, "0.0.0.0:7717")
	if text, _ := runCAToken(t, wild); !strings.Contains(joinLine(text), "--server <this control plane>:7717") {
		t.Errorf("a wildcard listen printed:\n%s", text)
	}

	loop := writeCAConfig(t, t.TempDir())
	text, _ := runCAToken(t, loop)
	if joinLine(text) != "" || !strings.Contains(text, "listens only on loopback") {
		t.Errorf("a loopback listen printed a join, or not why it cannot:\n%s", text)
	}
}

func mustNil(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}
