package node

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/guestassets"
)

// THE GUEST WAITS A LITTLE LONGER THAN THE NODE WORKS, AND NO MORE. The agent
// starts the runner only after its image-store request is answered, so the
// guest's bound is how long GitHub can list the runner offline (#320). A guest
// that gave up first would go cold while the node finished an attach nobody
// mounts; one that waited far longer, as the 780 s it used to, holds the runner
// back for nothing. The guest's script is executed, not searched for a number:
// its stubs are shell functions, so the request it makes is the one observed.
func TestTheGuestWaitsJustPastTheNodesDockerStoreBound(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := filepath.Join(dir, "docker-cache.sh")
	if err := os.WriteFile(script, []byte(guestassets.DockerCacheScript), 0o600); err != nil {
		t.Fatalf("write the guest's script: %v", err)
	}
	curlLog := filepath.Join(dir, "curl.log")

	// curl fails, so prepare takes its cold path after the one request.
	const stubs = `
curl() { printf '%s\n' "$*" >>"$BILLET_TEST_CURL_LOG"; return 28; }
uname() { printf 'x86_64\n'; }
systemctl() { return 0; }
. "$0"
`
	run := exec.CommandContext(t.Context(), "/bin/sh", "-c", stubs, script, "prepare")
	run.Env = append(os.Environ(),
		"BILLET_CACHE_ENDPOINT=http://cache.test",
		"BILLET_CACHE_TOKEN=token",
		"BILLET_DOCKER_CACHE_STATE_DIR="+filepath.Join(dir, "state"),
		"BILLET_TEST_CURL_LOG="+curlLog,
	)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("prepare on its cold path: %v\n%s", err, out)
	}

	logged, err := os.ReadFile(curlLog)
	if err != nil {
		t.Fatalf("read the requests the guest made: %v", err)
	}
	var request string
	for line := range strings.SplitSeq(strings.TrimSpace(string(logged)), "\n") {
		if strings.HasSuffix(line, "http://cache.test/v1/docker-store") {
			request = line
		}
	}
	if request == "" {
		t.Fatalf("the guest never asked for its image store; it asked:\n%s", logged)
	}
	match := regexp.MustCompile(`--max-time (\d+) `).FindStringSubmatch(request)
	if match == nil {
		t.Fatalf("the image-store request has no --max-time: %s", request)
	}
	seconds, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("parse --max-time %q: %v", match[1], err)
	}
	wait := time.Duration(seconds) * time.Second
	if wait <= dockerStoreRequestLimit || wait > dockerStoreRequestLimit+time.Minute {
		t.Fatalf("the guest waits %s for an answer the node bounds at %s; want longer, by at most a minute",
			wait, dockerStoreRequestLimit)
	}
}
