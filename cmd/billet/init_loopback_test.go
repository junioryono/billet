package main

import (
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/wirecert"
)

// THE LOCAL PROFILE'S GUARANTEE, PROVED RATHER THAN DESCRIBED: a config
// `billet init` generates binds only loopback and the node wire serving it
// mints NO certificate authority. The config's own comment promises "nothing is
// exposed to the network and no certificates are involved" — this is the test
// that promise answers to, driven through the same serveNodeWire the real
// server runs.
func TestAGeneratedLocalConfigServesPlainLoopbackWithoutACA(t *testing.T) {
	body, _, err := initconfig.Generate(initconfig.Params{
		Org:         "acme",
		Provider:    config.ProviderDocker,
		VCPU:        8,
		Memory:      16 * config.GiB,
		RunnerGroup: "billet-trial",
		Workflows:   []string{"acme/repo/.github/workflows/ci.yml@refs/heads/main"},
		Listen:      freeLoopbackListen(t),
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	filled := strings.Replace(body, "app_id: 0", "app_id: 1", 1)
	filled = strings.Replace(filled, "installation_id: 0", "installation_id: 1", 1)
	cfg, err := config.Parse("generated", []byte(filled))
	if err != nil {
		t.Fatalf("the generated config does not load: %v", err)
	}

	// The generated address IS loopback — the same predicate serveNodeWire
	// branches on to decide whether an authority exists at all.
	if !nodeplane.LoopbackOnly(cfg.Server.Listen) {
		t.Fatalf("the generated listen %q is not loopback-only", cfg.Server.Listen)
	}

	// The generated state dir points at the real user config dir; the serving
	// path only needs SOME state dir to read the deployment id from, and a test
	// must not write outside its own tree. The listen address under proof stays
	// exactly what init generated.
	stateDir := t.TempDir()
	cfg.Server.IdentityDir = stateDir

	deploymentID := "0123456789abcdef0123456789abcdef"
	stop, err := serveNodeWire(t.Context(), cfg,
		nodeplane.New(slog.New(slog.DiscardHandler), deploymentID, time.Minute),
		nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("serving the node wire on the generated loopback address: %v", err)
	}

	t.Cleanup(stop.stop)

	// The listener answers PLAIN HTTP at the generated address. A raw TCP dial
	// would also succeed against a TLS listener before any handshake, so the
	// proof is a real HTTP exchange: any valid plaintext response — a 404 is
	// fine — is one no TLS listener could have produced.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://"+cfg.Server.Listen+"/", http.NoBody)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("a plain-HTTP request to the generated listen address failed: %v", err)
	}
	_ = resp.Body.Close()

	// And serving it minted nothing: no authority directory, no authority
	// marker, and no certificate material anywhere in the state dir — the
	// state dir is the only path the CA machinery writes under, so the walk
	// covers everything LoadOrCreateCA would have created on the non-loopback
	// path.
	if _, err := os.Stat(wirecert.CADir(stateDir)); !os.IsNotExist(err) {
		t.Errorf("serving loopback created the CA directory %s", wirecert.CADir(stateDir))
	}
	if _, err := os.Stat(filepath.Join(stateDir, "authority-created")); !os.IsNotExist(err) {
		t.Error("serving loopback recorded an authority-created marker: authority creation was attempted")
	}

	err = filepath.WalkDir(stateDir, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if base := filepath.Base(path); strings.HasSuffix(base, ".crt") ||
			strings.HasSuffix(base, ".key") || strings.HasSuffix(base, ".pem") {
			t.Errorf("serving loopback left certificate material at %s", path)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", stateDir, err)
	}
}

// freeLoopbackListen picks a loopback address with a currently free port. The
// port is released before use, so a rare collision fails the dial loudly
// rather than flaking silently.
func freeLoopbackListen(t *testing.T) string {
	t.Helper()

	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe a free port: %v", err)
	}

	addr := l.Addr().String()
	_ = l.Close()

	return addr
}
