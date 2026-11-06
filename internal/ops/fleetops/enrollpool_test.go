package fleetops

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostsource"
)

// A POOL HOST SAYS ITS LINE ON THE CONSOLE, in the one spelling the plane reads, and
// appends it rather than replacing what the console holds; a console that is not there
// is a failure, never a file billet creates.
func TestAPoolHostSaysItsFingerprintOnTheConsole(t *testing.T) { //nolint:paralleltest // consolePath is shared
	dir := t.TempDir()
	console := filepath.Join(dir, "console")
	if err := os.WriteFile(console, []byte("boot output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	was := consolePath
	t.Cleanup(func() { consolePath = was })
	consolePath = console

	for range 2 {
		if err := announceOnConsole("pa", "SHA256:k"); err != nil {
			t.Fatalf("announce: %v", err)
		}
	}
	raw, err := os.ReadFile(console)
	if err != nil {
		t.Fatal(err)
	}
	want := "boot output\n" + hostsource.ConsoleLine("pa", "SHA256:k") + "\n" + hostsource.ConsoleLine("pa", "SHA256:k") + "\n"
	if string(raw) != want {
		t.Fatalf("the console holds %q, want %q", raw, want)
	}
	if got := hostsource.ConsoleFingerprints(string(raw), "pa"); len(got) != 1 || got[0] != "SHA256:k" {
		t.Fatalf("the plane would read %v", got)
	}

	consolePath = filepath.Join(dir, "absent")
	if err := announceOnConsole("pa", "SHA256:k"); err == nil {
		t.Fatal("a console that is not there was written")
	}
	if _, err := os.Stat(consolePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("announcing created %s (%v)", consolePath, err)
	}
}

// A HOST WITH NO IDENTITY DOCUMENT SENDS NONE AND SAYS SO; ONE WITH A DOCUMENT SENDS IT.
func TestAPoolHostSendsWhatItsPlatformSigned(t *testing.T) { //nolint:paralleltest // instanceIdentity is shared
	was := instanceIdentity
	t.Cleanup(func() { instanceIdentity = was })
	var out bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &out}

	instanceIdentity = func(context.Context) ([]byte, string, error) { return nil, "", errors.New("not on EC2") }
	if got := poolIdentity(t.Context(), env); got.Document != nil || got.Signature != "" ||
		!strings.Contains(out.String(), "not on EC2") {
		t.Fatalf("no identity: %+v, said %q", got, out.String())
	}
	instanceIdentity = func(context.Context) ([]byte, string, error) { return []byte(`{"a":1}`), "c2ln", nil }
	if got := poolIdentity(t.Context(), env); string(got.Document) != `{"a":1}` || got.Signature != "c2ln" {
		t.Fatalf("an identity: %+v", got)
	}
}

// A HOST JOINING FROM A POOL ORDER IS ITS ORDER'S NODE, and a config naming another is
// refused before anything is asked.
func TestAPoolHostsNameIsItsOrders(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Node: &config.NodeConfig{Name: "epyc-1", TLS: &config.NodeTLS{}}}
	err := EnrollNode(t.Context(), cli.Env{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}, cfg,
		"https://127.0.0.1:1", "SHA256:ca", "tok", "pa")
	if err == nil || !strings.Contains(err.Error(), "pool-pa") {
		t.Fatalf("a pool host under another name answered %v", err)
	}
}
