package images

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/imageaccept"
)

// AN AMI IS COMPLETED WITH WHAT THE DEPLOYMENT ACCEPTS, by the rule a guest
// image's completion follows: the config's images.accept, with what the control
// plane accepts on a node that recorded it. No config accepts nothing and says
// so; a config that cannot be read, or a node that cannot tell what its control
// plane accepts, refuses rather than build an image with less.
func TestAnAMIIsCompletedWithWhatTheDeploymentAccepts(t *testing.T) {
	t.Parallel()

	run := func(t *testing.T, path string) ([]string, string, error) {
		t.Helper()

		var out bytes.Buffer

		got, err := amiAccept(cli.Env{Stdout: &out, Stderr: &out, Getenv: os.Getenv}, path)

		return got, out.String(), err
	}

	t.Run("no config", func(t *testing.T) {
		t.Parallel()

		got, said, err := run(t, filepath.Join(t.TempDir(), "absent.yaml"))
		if err != nil || got != nil {
			t.Fatalf("accepted %v (%v), want nothing", got, err)
		}

		if !strings.Contains(said, "none of the license-restricted components") {
			t.Errorf("a build with no config did not say it accepts nothing:\n%s", said)
		}
	})

	t.Run("a config that does not parse", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "billet.yaml")
		if err := os.WriteFile(path, []byte("images: [\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		if _, _, err := run(t, path); err == nil {
			t.Error("a config that does not parse was read as accepting nothing")
		}
	})

	t.Run("the node's and its control plane's", func(t *testing.T) {
		t.Parallel()

		path, nodeDir := acceptanceConfig(t, "images:\n  accept: [codeql]\n")
		if err := imageaccept.Write(nodeDir, acceptanceDeployment, true, []string{"firefox"}, time.Now()); err != nil {
			t.Fatal(err)
		}

		got, _, err := run(t, path)
		if err != nil || !slices.Equal(got, []string{"codeql", "firefox"}) {
			t.Errorf("accepted %v (%v), want codeql and firefox", got, err)
		}
	})

	t.Run("a node that has not recorded its control plane's", func(t *testing.T) {
		t.Parallel()

		path, _ := acceptanceConfig(t, "images:\n  accept: [codeql]\n")

		if _, _, err := run(t, path); err == nil {
			t.Error("a node that cannot tell what its deployment accepts built with its own list")
		}
	})
}
