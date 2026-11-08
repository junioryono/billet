package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/config"
)

// A POOL HOST'S BOOT GENERATES A FIRECRACKER NODE'S CONFIG WITH ITS KERNEL AND CEPH
// IDENTITY. The join line of the boot the control plane renders, its single-quoted
// words undone as bash undoes them (internal/app's tests run that boot under bash);
// the join's own parser over those words; the init arguments the join builds; and
// billet init writing the config, which loads as a Firecracker node named for its
// order, pointed at the join server, with server.pool.init_args' kernel image and Ceph
// identity. Nothing between the pool's config and the node's is stood in for but the
// join's network steps.
func TestAPoolHostsBootGeneratesAFirecrackerNodeWithItsKernelAndCeph(t *testing.T) {
	t.Parallel()
	pool := &config.Config{
		GitHub: &config.GitHubConfig{Org: "acme"},
		Server: &config.ServerConfig{Pool: &config.PoolConfig{
			JoinServer: "10.77.0.10:7717", BootstrapAddr: "10.77.0.10:7718",
			InitArgs: []string{
				"--kernel-image", "/var/lib/billet/vmlinux", "--ceph-user=pool-worker",
				"--ceph-keyring", "/etc/ceph/ceph.client.billet.keyring",
			},
		}},
	}
	boot, err := app.PoolBoot(pool, "true\n", "SHA256:"+strings.Repeat("a", 64), "p01", "tok")
	if err != nil {
		t.Fatalf("PoolBoot: %v", err)
	}

	lines := strings.Split(strings.TrimRight(string(boot), "\n"), "\n")
	words := singleQuotedWords(t, lines[len(lines)-1])
	if len(words) < 2 || words[0] != "billet" || words[1] != "join" {
		t.Fatalf("the boot's last line is %q, not its join", lines[len(lines)-1])
	}

	req, err := parseJoin(processEnv(), words[2:])
	if err != nil {
		t.Fatalf("the join refused the words the boot handed it: %v", err)
	}
	req.cfgPath = filepath.Join(t.TempDir(), "billet.yaml")
	if err := joinGenerate(t.Context(), processEnv(), joinInitArgs(req, testJoinDeployment)); err != nil {
		t.Fatalf("billet init refused what the join handed it: %v", err)
	}

	cfg, err := config.Load(req.cfgPath)
	if err != nil {
		t.Fatalf("the generated config does not load: %v", err)
	}
	n := cfg.Node
	switch {
	case n == nil || n.Provider != config.ProviderFirecracker:
		t.Fatalf("the generated node is not a Firecracker node: %+v", n)
	case n.Name != "pool-p01":
		t.Errorf("the node is named %q; want its order's, pool-p01", n.Name)
	case !strings.Contains(n.ServerAddr, "10.77.0.10:7717"):
		t.Errorf("the node dials %q; want the join server", n.ServerAddr)
	case n.Firecracker == nil || n.Firecracker.KernelImage != "/var/lib/billet/vmlinux":
		t.Errorf("the node's kernel image is %+v; want init_args' /var/lib/billet/vmlinux", n.Firecracker)
	case n.Ceph == nil || n.Ceph.User != "pool-worker" || n.Ceph.KeyringPath != "/etc/ceph/ceph.client.billet.keyring":
		t.Errorf("the node's Ceph identity is %+v; want init_args' pool-worker and its keyring", n.Ceph)
	}
}

// A VALUE THAT LOOKS LIKE A FLAG IS ITS FLAG'S VALUE. init_args that a config accepts,
// `--ceph-user --force`, reach init as a Ceph user named --force and force nothing: the
// join's existing config is not replaced.
func TestAPoolInitArgThatLooksLikeAFlagIsAValue(t *testing.T) {
	t.Parallel()
	args := []string{"--kernel-image", "/var/lib/billet/vmlinux", "--ceph-user", "--force"}
	req, err := parseJoin(processEnv(), append([]string{
		"--server", "10.77.0.10:7717", "--ca-fingerprint", "SHA256:" + strings.Repeat("a", 64), "--token", "tok",
		"--node-name", "pool-p01", "--pool-order", "p01", "--provider", "firecracker", "--org", "acme", "--",
	}, args...))
	if err != nil {
		t.Fatalf("parseJoin: %v", err)
	}
	req.cfgPath = filepath.Join(t.TempDir(), "billet.yaml")
	if err := os.WriteFile(req.cfgPath, []byte("# an operator's own config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := joinGenerate(t.Context(), processEnv(), joinInitArgs(req, testJoinDeployment)); err != nil {
		t.Fatalf("billet init: %v", err)
	}
	if body, err := os.ReadFile(req.cfgPath); err != nil || string(body) != "# an operator's own config\n" {
		t.Errorf("init replaced the existing config (%v): %q", err, body)
	}
	beside, err := config.Load(req.cfgPath + ".new")
	if err != nil {
		t.Fatalf("the config init wrote beside the existing one does not load: %v", err)
	}
	if beside.Node == nil || beside.Node.Ceph == nil || beside.Node.Ceph.User != "--force" {
		t.Errorf("the Ceph user is %+v; want the word after --ceph-user, --force", beside.Node)
	}
}

// singleQuotedWords splits a line of single-quoted words as bash reads them, a quote
// inside a word spelled as the boot spells it: a closing quote, an escaped one, and an
// opening one.
func singleQuotedWords(t *testing.T, line string) []string {
	t.Helper()
	var words []string
	var word strings.Builder
	in, started := false, false
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\'':
			in, started = !in, true
		case in:
			word.WriteByte(c)
		case c == '\\' && i+1 < len(line):
			i++
			word.WriteByte(line[i])
			started = true
		case c == ' ':
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		default:
			word.WriteByte(c)
			started = true
		}
	}
	if in {
		t.Fatalf("an unclosed quote in %q", line)
	}
	if started {
		words = append(words, word.String())
	}

	return words
}
