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

// `ca token` PRINTS THE WHOLE `billet join` (#181): the address, the
// authority's fingerprint as `ca show` reports it, the token and the target,
// so the new machine runs one command; and the fingerprint is read, never made,
// so a control plane with no authority yet is told to ask and gains none.
func TestCATokenPrintsTheWholeJoinCommand(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)

	env, out := bufferEnv()
	if err := CAToken(t.Context(), env, []string{"--config", cfg}); err != nil {
		t.Fatalf("ca token: %v", err)
	}
	if !strings.Contains(out.String(), "--ca-fingerprint <from `billet ca show`>") {
		t.Errorf("with no authority the token does not say where the fingerprint comes from:\n%s", out)
	}
	if _, err := os.Stat(wirecert.CACertPath(stateDir)); !os.IsNotExist(err) {
		t.Errorf("printing the token created an authority (%v)", err)
	}

	// The authority `ca issue` creates, and the fingerprint `ca show` prints.
	if err := CAIssue(t.Context(), env, []string{"mac-1", "--config", cfg, "--out", filepath.Join(t.TempDir(), "b")}); err != nil {
		t.Fatalf("ca issue: %v", err)
	}
	raw, err := os.ReadFile(wirecert.CACertPath(stateDir))
	mustNil(t, err)
	fp, err := wirecert.FingerprintOfCAPEM(raw)
	mustNil(t, err)

	env, out = bufferEnv()
	if err := CAToken(t.Context(), env, []string{"--config", cfg}); err != nil {
		t.Fatalf("ca token: %v", err)
	}

	text := out.String()
	line := ""
	for l := range strings.SplitSeq(text, "\n") {
		if strings.Contains(l, "billet join --server") {
			line = strings.TrimSpace(l)
		}
	}

	for _, want := range []string{"billet join --server 127.0.0.1:7717", "--ca-fingerprint " + fp, "--token ",
		"--node-name <its name>", "--org acme"} {
		if !strings.Contains(line, want) {
			t.Errorf("the join command %q does not say %q:\n%s", line, want, text)
		}
	}
	if !strings.Contains(text, "billet node --enroll --ca-fingerprint "+fp) {
		t.Errorf("the form for a configured machine is gone:\n%s", text)
	}
}

func mustNil(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}
