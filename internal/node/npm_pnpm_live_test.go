package node

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// npmPnpmSetupCommit is the pnpm/setup release the live test runs: v3.1.2, the
// one Platform's jobs ran when the npm cache failed them (2026-10-26).
const npmPnpmSetupCommit = "fa36a5ef7dcdefc3a7cb25655c122e00cf6e636e"

// npmPnpmWarmRuns is how many warm jobs the live test runs. Before the node
// left the close to its client, one warm job in about five failed on the shared
// test host (2026-10-26), so ten all passing is the claim.
const npmPnpmWarmRuns = 10

// PNPM/SETUP'S OWN INSTALLER, THROUGH THE NODE, AS JOBS RUN IT: the action's
// published bundle, at the commit Platform ran, installing pnpm 12.4.2 with the
// inputs Platform gives it, once on a cold node and then on the warm one, each
// job a fresh container. Its download verifies npm's signature and the
// tarball's checksum, so an answer the node changes on the way fails it, and its
// bundled undici asserts its parser is not paused when a socket ends, which a
// node that closed on an answer's last byte set off (closeAfterClient).
//
// The installer's refusals are thrown, and Node 24 crashes printing a thrown
// error from the bundle's one long line, so the job installs its own handlers
// and prints the error and its whole stack itself.
//
// Run it on the shared test host, only by name:
// BILLET_TEST_NPM_LIVE=1 go test -run '^TestLivePnpmSetupInstallsPnpmThroughTheNode$' ./internal/node
func TestLivePnpmSetupInstallsPnpmThroughTheNode(t *testing.T) {
	live := newNpmLive(t)
	step := strings.Join([]string{
		"mkdir -p /tmp/action /tmp/app /tmp/runner",
		"curl -fsSL -o /tmp/action/index.js https://raw.githubusercontent.com/pnpm/setup/" +
			npmPnpmSetupCommit + "/dist/index.js",
		"cd /tmp/app",
		"touch /tmp/runner/path /tmp/runner/env /tmp/runner/output /tmp/runner/state",
		"env GITHUB_PATH=/tmp/runner/path GITHUB_ENV=/tmp/runner/env GITHUB_OUTPUT=/tmp/runner/output " +
			"GITHUB_STATE=/tmp/runner/state GITHUB_WORKSPACE=/tmp/app RUNNER_TEMP=/tmp/runner " +
			"RUNNER_TOOL_CACHE=/tmp/runner/tools " +
			"INPUT_VERSION=12.4.2 INPUT_DEST=/tmp/setup-pnpm INPUT_CACHE=false INPUT_SAVE-CACHE=false " +
			"INPUT_INSTALL=false INPUT_WORKING-DIRECTORY=. INPUT_REQUIRE-LOCKFILE=false " +
			`node --stack-trace-limit=200 -e '
const show = (how) => (e) => { console.error("PNPM-SETUP " + how + ":", e && e.stack ? e.stack : e, e && e.cause ? "\ncause: " + (e.cause.stack || e.cause) : ""); process.exit(1); };
process.on("uncaughtException", show("uncaught"));
process.on("unhandledRejection", show("unhandled"));
require("/tmp/action/index.js");'`,
		"/tmp/setup-pnpm/pnpm --version",
		// THE RELAY OUTLIVES THE CLIENT, so a connection the client closed is
		// told from one the container's end closed: the node logs how long each
		// answered connection waited, and this is longer than the bound below.
		"sleep 5",
	}, "\n")
	failed := func(run npmLiveRun) bool {
		return run.err != nil || strings.Contains(run.output, "PNPM-SETUP ") || strings.Contains(run.output, "::error::") ||
			!strings.Contains(run.output, "\n12.4.2")
	}

	cold := live.run("pnpm-setup-cold", "node:24-bookworm", step)
	if failed(cold) {
		t.Fatalf("pnpm/setup on the cold node failed: %v\n%s\nnode log:\n%s", cold.err, cold.output,
			tail(live.logs.String(), 40))
	}
	if cold.upstream == 0 {
		t.Fatal("the cold job made no request of the registry: the node was not on its path")
	}
	warmFailures := 0
	for i := range npmPnpmWarmRuns {
		warm := live.run("pnpm-setup-warm-"+strconv.Itoa(i), "node:24-bookworm", step)
		if failed(warm) {
			warmFailures++
			t.Errorf("pnpm/setup on the warm node failed, run %d: %v\n%s", i+1, warm.err, warm.output)
		}
		// AND THE NODE ANSWERED IT: a tarball the relay fetched past the node would
		// pass as well.
		if warm.answers["tarball/warm/200"] == 0 {
			t.Errorf("warm run %d's answers from the node = %v; want its tarballs warm", i+1, warm.answers)
		}
		assertClientsClosed(t, "warm run "+strconv.Itoa(i+1), warm)
	}
	t.Logf("%d of %d warm runs failed", warmFailures, npmPnpmWarmRuns)
}

// npmClientClosed is how long a client may take to close an answered
// connection and still count as having closed it itself: well under the five
// seconds the job's container outlives its client.
const npmClientClosed = 2 * time.Second

// assertClientsClosed requires every connection the node answered in run to
// have been closed by its client, through the guest relay, within
// npmClientClosed of its answer: not by the wait running out, and not by the
// container's end.
func assertClientsClosed(t *testing.T, what string, run npmLiveRun) {
	t.Helper()

	answered := 0
	for _, count := range run.answers {
		answered += count
	}
	answered -= run.answers["spliced"]
	closed, slowest := 0, time.Duration(0)
	for _, line := range strings.Split(run.log, "\n") {
		if !strings.Contains(line, `msg="an npm client closed its answered connection"`) {
			continue
		}
		closed++
		waited, err := time.ParseDuration(logField(line, "waited"))
		if err != nil || waited > npmClientClosed {
			t.Errorf("%s: an answered connection waited %s for its client (%v); want its client's close within %s",
				what, logField(line, "waited"), err, npmClientClosed)
		}
		slowest = max(slowest, waited)
	}
	if closed != answered {
		t.Errorf("%s: %d of %d answered connections were closed by their client", what, closed, answered)
	}
	t.Logf("%s: %d answered connections closed by their client, the slowest after %s", what, closed, slowest)
}
