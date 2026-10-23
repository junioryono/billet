package node

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// npmLiveImage is the client image the live test installs with: Node, npm and
// Python, as a guest has them.
const npmLiveImage = "node:22-bookworm"

// npmLivePackages is the workload: a real dependency tree of several hundred
// packages, large enough that its tarballs dwarf its documents.
const npmLivePackages = `{"name":"billet-npm-live","version":"1.0.0","private":true,"dependencies":` +
	`{"typescript":"5.6.3","webpack":"5.95.0","webpack-cli":"5.1.4","@babel/core":"7.25.8",` +
	`"eslint":"9.13.0","jest":"29.7.0"}}`

// lockedBuffer is a log the node's handlers write while the test reads it.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.String()
}

// countingTransport counts the requests the node makes of the registry.
type countingTransport struct {
	next  http.RoundTripper
	count *atomic.Int64
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.count.Add(1)

	return c.next.RoundTrip(req)
}

// THE NPM CACHE MEASURED UNDER A REAL INSTALL (ADR-020's deferred measurement,
// #631): the real node cache, against the real registry, reached by a real npm
// through the real guest relay on the npm loopback address, with the registry
// remapped to it as the guest remaps it. Each run is a fresh container, as each
// job is a fresh guest, sharing only the node.
//
// It is a container on the shared test host, not a Firecracker guest: what it
// proves is the relay, the node and the registry's own behaviour under npm and
// a pnpm/setup-shaped fetch; what it does not is a microVM's network path, sudo
// under the guest image's sudoers, or a host-network container in the guest.
//
// Run it there, never on the development Mac, and only by name:
// BILLET_TEST_NPM_LIVE=1 go test -run '^TestLiveAnNpmInstallIsServedFromTheNodeTheSecondTime$' ./internal/node
func TestLiveAnNpmInstallIsServedFromTheNodeTheSecondTime(t *testing.T) {
	if os.Getenv("BILLET_TEST_NPM_LIVE") != "1" {
		t.Skip("BILLET_TEST_NPM_LIVE=1 runs a real npm install against registry.npmjs.org in Docker")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker is not installed")
	}

	var dialer net.ListenConfig
	listener, err := dialer.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var logs lockedBuffer
	service, err := NewCacheService("http://"+listener.Addr().String(), "npm-live", t.TempDir(),
		&fakeCacheStore{}, &fakeVolumeAttacher{},
		slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	server := &http.Server{Handler: service, ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("the node's cache listener: %v", err)
		}
	})

	work := t.TempDir()
	script, err := os.ReadFile(filepath.Join("..", "guestassets", "actions-proxy.py"))
	if err != nil {
		t.Fatalf("read the guest relay: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "actions-proxy.py"), script, 0o644); err != nil {
		t.Fatalf("write the guest relay: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "package.json"), []byte(npmLivePackages), 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	addresses, err := net.DefaultResolver.LookupHost(t.Context(), npmRegistryHost)
	if err != nil || len(addresses) == 0 {
		t.Fatalf("resolve the registry for the relay's fallback: %v", err)
	}
	var fallback []string
	for _, address := range addresses {
		if ip := net.ParseIP(address); ip != nil && ip.To4() != nil {
			fallback = append(fallback, address)
		}
	}

	upstream := map[string]int64{}
	answers := map[string]map[string]int{}
	var requests atomic.Int64
	wrapped := false
	run := func(job, step string) {
		t.Helper()

		credentials, err := service.PrepareScoped("billet-npm-live-"+job, CacheSessionScope{
			Trust: provider.TrustUntrusted, Cache: &config.CacheSpec{Npm: true},
		})
		if err != nil {
			t.Fatalf("PrepareScoped: %v", err)
		}
		if !wrapped {
			npm := service.npmCache()
			npm.pooled.Transport = &countingTransport{next: npm.pooled.Transport, count: &requests}
			npm.fresh.Transport = &countingTransport{next: npm.fresh.Transport, count: &requests}
			wrapped = true
		}
		if err := os.WriteFile(filepath.Join(work, "ca.pem"), []byte(credentials.ActionsCAPEM), 0o644); err != nil {
			t.Fatalf("write the authority: %v", err)
		}
		shell := strings.Join([]string{
			"set -euo pipefail",
			// What the container writes under /work goes back to the test's own
			// account, so its temporary directory can be removed.
			`trap 'chown -R "$BILLET_OWNER" /work' EXIT`,
			"python3 /work/actions-proxy.py --mode passthrough --listen 127.0.2.1:443 " +
				"--upstream \"$BILLET_NPM_PROXY\" --origin registry.npmjs.org --fallback-addr \"$BILLET_NPM_FALLBACK\" &",
			"sleep 1",
			"cp -r /work/app-src /tmp/app 2>/dev/null || { mkdir -p /tmp/app && cp /work/package.json /tmp/app/; }",
			"cd /tmp/app",
			step,
		}, "\n")
		before := requests.Load()
		logged := len(logs.String())
		started := time.Now()
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
		defer cancel()
		// NAMED, AND REMOVED HOWEVER THE RUN ENDS: a cancelled docker CLI does not
		// stop the container the daemon runs, which would keep the npm loopback
		// address on the shared host.
		name := "billet-npm-live-" + strconv.Itoa(os.Getpid()) + "-" + job
		t.Cleanup(func() {
			// NOT the test's own context, which has ended by the time this runs.
			removeCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
			defer cancel()
			// `docker rm -f` of a container --rm already removed answers "no such
			// container", which is the ordinary case and not worth reporting.
			if output, err := exec.CommandContext(removeCtx, docker, "rm", "-f", name).CombinedOutput(); err != nil &&
				!strings.Contains(string(output), "No such container") {
				t.Logf("remove %s: %v: %s", name, err, output)
			}
		})
		command := exec.CommandContext(ctx, docker, "run", "--rm", "--name", name, "--network", "host",
			"--add-host", npmRegistryHost+":127.0.2.1",
			"-v", work+":/work",
			"-e", "NODE_EXTRA_CA_CERTS=/work/ca.pem",
			"-e", "BILLET_NPM_PROXY="+credentials.NpmProxy,
			"-e", "BILLET_NPM_FALLBACK="+strings.Join(fallback, ","),
			"-e", "BILLET_OWNER="+strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()),
			npmLiveImage, "bash", "-c", shell)
		output, err := command.CombinedOutput()
		elapsed := time.Since(started)
		upstream[job] = requests.Load() - before
		answers[job] = npmCounts(logs.String()[logged:])
		t.Logf("%s: %s, %d requests to the registry from the node; answers %s\n%s", job,
			elapsed.Round(time.Millisecond), upstream[job], npmTally(answers[job]),
			tail(string(output), 15))
		if err != nil {
			t.Fatalf("%s failed: %v\nnode log:\n%s", job, err, tail(logs.String(), 40))
		}
	}

	// THE FIRST JOB RESOLVES AND FILLS: documents and tarballs from the registry,
	// through the node. Its lockfile is kept for the second.
	run("cold-install", "npm install --no-audit --no-fund --loglevel=error && "+
		"mkdir -p /work/app-src && cp package.json package-lock.json /work/app-src/ && "+
		"du -sh node_modules && ls node_modules | wc -l")
	// THE SECOND JOB IS ANOTHER GUEST WITH NOTHING OF ITS OWN: an empty npm cache
	// and its own clean install from the lockfile. What it fetches should never
	// leave the node.
	run("warm-ci", "npm ci --no-audit --no-fund --loglevel=error && du -sh node_modules")
	// THE STEP #631 RECORDS FAILING: pnpm/setup's version document and the
	// pnpm executable's tarball, fetched twice, as two jobs would.
	pnpm := `node -e '
const started = Date.now();
(async () => {
  for (let i = 0; i < 2; i++) {
    const fetchStarted = Date.now();
    const document = await fetch("https://registry.npmjs.org/@pnpm/exe.linux-x64/12.4.2");
    if (document.status !== 200) throw new Error("the version document answered " + document.status);
    const doc = await document.json();
    const tarball = await fetch(doc.dist.tarball);
    if (tarball.status !== 200) throw new Error("the tarball answered " + tarball.status);
    const bytes = (await tarball.arrayBuffer()).byteLength;
    const took = Date.now() - fetchStarted;
    console.log("pnpm exe fetch", i + 1, "status", tarball.status, "bytes", bytes, "ms", took);
    // THE BUDGET pnpm/setup GIVES ITS DOWNLOAD, the one #631 records it missing.
    if (took > 30000) throw new Error("the download took " + took + " ms, past the 30 s pnpm/setup allows");
  }
})().catch((e) => { console.error(e); process.exit(1); });'`
	run("pnpm-setup", pnpm)

	if upstream["cold-install"] == 0 {
		t.Fatal("the cold install made no request of the registry: the node was not on its path")
	}
	// A lockfile's install needs only tarballs, and every one the cold install
	// fetched is in the store; a revalidation of a document npm still asks for is
	// a conditional request, which is still one leaving the node, so the bound is
	// a small fraction rather than zero.
	if limit := upstream["cold-install"] / 10; upstream["warm-ci"] > limit {
		t.Errorf("the second job made %d requests of the registry, more than a tenth of the first's %d",
			upstream["warm-ci"], upstream["cold-install"])
	}
	// AND THE SECOND JOB WAS ANSWERED BY THE NODE, not by a relay that failed
	// open past it: a refused tunnel would also make no request from the node.
	warm := answers["warm-ci"]
	if warm["tarball/warm/200"] == 0 || warm["tarball/filled/200"] != 0 || warm["spliced"] != 0 {
		t.Errorf("the second job's answers from the node = %v; want every tarball warm, none filled or spliced",
			warm)
	}
	t.Logf("node log (last lines):\n%s", tail(logs.String(), 20))
}

// npmCounts counts the node's answers in a stretch of its log by what was asked
// for and how it was answered, and the requests it passed to the registry.
func npmCounts(log string) map[string]int {
	counts := map[string]int{}
	for _, line := range strings.Split(log, "\n") {
		switch {
		case strings.Contains(line, `msg="an npm request was answered"`):
			counts[logField(line, "object")+"/"+logField(line, "how")+"/"+logField(line, "status")]++
		case strings.Contains(line, `msg="an npm request was passed to the registry"`):
			counts["spliced"]++
		}
	}

	return counts
}

// npmTally renders npmCounts in a stable order.
func npmTally(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+strconv.Itoa(counts[key]))
	}

	return strings.Join(parts, " ")
}

func logField(line, name string) string {
	_, rest, found := strings.Cut(line, " "+name+"=")
	if !found {
		return "?"
	}
	value, _, _ := strings.Cut(rest, " ")

	return value
}

func tail(text string, lines int) string {
	all := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}

	return strings.Join(all, "\n")
}
