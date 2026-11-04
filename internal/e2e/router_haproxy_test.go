package e2e

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// haproxyRequiredEnv makes a missing haproxy a failure rather than a skip, which is how
// CI says this is one of its gates: a skip that quietly passes would let the file billet
// ships stop working with nothing saying so.
const haproxyRequiredEnv = "BILLET_TEST_HAPROXY"

// A REFERENCE CONFIGURATION THAT WAS NOT RUN IS A GUESS. This runs HAProxy on the file
// billet ships, edited only for the ports this test was given, through the whole scenario.
func TestTheReferenceRouterConfigurationRoutesTwoDeployments(t *testing.T) {
	binary, err := exec.LookPath("haproxy")
	if err != nil {
		if os.Getenv(haproxyRequiredEnv) == "required" {
			t.Fatalf("%s=required and haproxy is not installed: %v", haproxyRequiredEnv, err)
		}

		t.Skipf("haproxy is not installed; set %s=required to make that a failure", haproxyRequiredEnv)
	}

	resolveRouterNames()

	a, b := twoRoutedDeployments(t)

	addr := startHAProxy(t, binary, renderRouterConfig(t, readRouterFile(t), map[string]string{
		"127.0.0.1:17001": a.stack.assembled.wire.Addr,
		"127.0.0.1:17002": a.stack.assembled.wire.Bootstrap,
		"127.0.0.1:17003": b.stack.assembled.wire.Addr,
		"127.0.0.1:17004": b.stack.assembled.wire.Bootstrap,
	}))

	exerciseTwoDeployments(t, a, b, addr)
}

// THE REFERENCE FILE KEEPS THE RULES IT STATES, in the file an operator copies: no
// certificate, no default route, and nothing routed to a metrics port.
func TestTheReferenceRouterConfigurationHoldsNoKeyAndNoDefault(t *testing.T) {
	t.Parallel()

	body := readRouterFile(t)

	active := activeLines(body)

	for _, banned := range []string{" ssl", "crt ", "default_backend", "ca-file"} {
		for _, line := range active {
			if strings.Contains(line, banned) {
				t.Errorf("the reference configuration has %q, on: %s", banned, line)
			}
		}
	}

	for _, want := range []string{
		"mode tcp",
		"tcp-request content accept if { req.ssl_hello_type 1 }",
		"tcp-request content reject",
	} {
		found := false

		for _, line := range active {
			found = found || line == want
		}

		if !found {
			t.Errorf("the reference configuration has no line %q", want)
		}
	}
}

func activeLines(body string) []string {
	var lines []string

	for line := range strings.Lines(body) {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}

	return lines
}

// renderRouterConfig points the shipped file at this test's ports. Each placeholder must
// be there exactly once, or this test has stopped running the file it claims to.
func renderRouterConfig(t *testing.T, body string, ports map[string]string) string {
	t.Helper()

	for placeholder, port := range ports {
		if n := strings.Count(body, placeholder); n != 1 {
			t.Fatalf("the reference configuration names %s %d times, want once", placeholder, n)
		}

		body = strings.ReplaceAll(body, placeholder, port)
	}

	return body
}

var haproxyBind = regexp.MustCompile(`(?m)^(\s*bind) :443$`)

// startHAProxy runs HAProxy on a config, on a port of its own, and waits for it to listen.
func startHAProxy(t *testing.T, binary, config string) string {
	t.Helper()

	var lc net.ListenConfig

	probe, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a port: %v", err)
	}

	addr := probe.Addr().String()

	if err := probe.Close(); err != nil {
		t.Fatalf("free the port: %v", err)
	}

	if haproxyBind.FindString(config) == "" {
		t.Fatal("the reference configuration has no `bind :443` for this test to move")
	}

	config = haproxyBind.ReplaceAllString(config, "$1 "+addr)

	path := filepath.Join(t.TempDir(), "haproxy.cfg")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatalf("write the configuration: %v", err)
	}

	if out, err := exec.CommandContext(t.Context(), binary, "-c", "-f", path).CombinedOutput(); err != nil {
		t.Fatalf("haproxy refuses the reference configuration: %v\n%s", err, out)
	}

	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))

	cmd := exec.CommandContext(ctx, binary, "-db", "-f", path)

	logged := &lockedLog{}
	cmd.Stdout, cmd.Stderr = logged, logged

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start haproxy: %v", err)
	}

	t.Cleanup(func() {
		cancel()

		_ = cmd.Wait()

		if t.Failed() {
			t.Logf("haproxy said:\n%s", logged.String())
		}
	})

	deadline := time.Now().Add(30 * time.Second)

	for {
		var d net.Dialer

		conn, err := d.DialContext(t.Context(), "tcp", addr)
		if err == nil {
			_ = conn.Close()

			return addr
		}

		if time.Now().After(deadline) {
			t.Fatalf("haproxy never listened on %s: %v\n%s", addr, err, logged.String())
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// lockedLog collects what HAProxy writes, for a failure to show.
type lockedLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.String()
}
