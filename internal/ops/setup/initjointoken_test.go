package setup

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
)

// THE ACCEPT TOKEN IS PRINTED ONLY BOUND TO A DEPLOYMENT, which `billet join`
// passes as --join-deployment from the authority it verified (#183): a plain
// `init --join` prints the half to merge by hand, and the flag is refused
// without --join or with an identity billet would not have minted.
//
// Mutation: printing the token on the node name alone prints one in the
// unbound case.
func TestInitJoinPrintsTheAcceptTokenOnlyBoundToADeployment(t *testing.T) {
	ownHome(t)

	const deployment = "0123456789abcdef0123456789abcdef"

	for _, c := range []struct {
		name  string
		extra []string
		token bool
	}{
		{"bound", []string{"--join-deployment", deployment}, true},
		{"unbound", nil, false},
	} {
		path := filepath.Join(t.TempDir(), "billet.yaml")
		args := joinArgs(path, append([]string{"--join", "controller.example:7717", "--node-name", "linux-2"},
			c.extra...)...)

		out := capture(t, func() {
			if err := Init(t.Context(), processEnv(), args); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
		})

		// THE NAME IS WRITTEN on a docker join too: the node enrolls by it.
		if cfg, err := config.Load(path); err != nil || cfg.Node == nil || cfg.Node.Name != "linux-2" {
			t.Errorf("%s: the joined config does not name the node linux-2 (%v)", c.name, err)
		}

		i := strings.Index(out, "billet join accept billet-join-1.")
		if (i >= 0) != c.token {
			t.Errorf("%s: printed a token: %v, want %v:\n%s", c.name, i >= 0, c.token, out)
		}
		if !c.token {
			continue
		}

		token, _, _ := strings.Cut(out[i+len("billet join accept "):], "\n")
		p, err := initconfig.ParseJoinProposal(token)
		if err != nil || p.Deployment != deployment || p.Node != "linux-2" {
			t.Errorf("the printed token is %+v (%v), want node linux-2 bound to %s", p, err, deployment)
		}
	}

	for _, extra := range [][]string{
		{"--join-deployment", deployment},
		{"--join", "controller.example:7717", "--join-deployment", "not-an-id"},
	} {
		path := filepath.Join(t.TempDir(), "billet.yaml")
		if err := Init(t.Context(), processEnv(), joinArgs(path, extra...)); err == nil ||
			!strings.Contains(err.Error(), "--join-deployment") {
			t.Errorf("%v was not refused by name: %v", extra, err)
		}
	}
}
