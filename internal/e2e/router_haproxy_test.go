package e2e

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
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

	// A HELLO IN TWO SEGMENTS STILL ROUTES. A real one can cross a segment boundary, with a
	// post-quantum key share, and loopback's MTU would otherwise never show it; the router
	// has to wait for all of it rather than judge what arrived first.
	if err := handshakeSplit(t, addr, a.names.enroll); err != nil {
		t.Errorf("a ClientHello in two segments did not reach its deployment: %v", err)
	}

	exerciseTwoDeployments(t, a, b, addr)
}

// handshakeSplit completes a handshake through addr with its ClientHello split in two.
func handshakeSplit(t *testing.T, addr, name string) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	var d net.Dialer

	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	conf := insecureHello()
	conf.ServerName = name

	conn := tls.Client(&splitFirstWrite{Conn: raw}, conf)
	defer func() { _ = conn.Close() }()

	return conn.HandshakeContext(ctx)
}

// splitFirstWrite sends its first write as two segments, apart in time.
type splitFirstWrite struct {
	net.Conn

	split bool
}

func (c *splitFirstWrite) Write(p []byte) (int, error) {
	if c.split || len(p) < 2 {
		return c.Conn.Write(p)
	}

	c.split = true
	half := len(p) / 2

	if _, err := c.Conn.Write(p[:half]); err != nil {
		return 0, err
	}

	time.Sleep(200 * time.Millisecond)

	n, err := c.Conn.Write(p[half:])

	return half + n, err
}

// ONE SOURCE CANNOT HOLD THE BUDGET EVERY DEPLOYMENT SHARES. The shipped limit, lowered so a
// test can reach it: the sources holding their places keep them, and the next is refused
// as it connects, rather than waiting out the inspect delay in a place of its own.
func TestTheReferenceRouterRefusesASourcePastItsLimit(t *testing.T) {
	binary, err := exec.LookPath("haproxy")
	if err != nil {
		if os.Getenv(haproxyRequiredEnv) == "required" {
			t.Fatalf("%s=required and haproxy is not installed: %v", haproxyRequiredEnv, err)
		}

		t.Skipf("haproxy is not installed; set %s=required to make that a failure", haproxyRequiredEnv)
	}

	const shipped = "sc0_conn_cur gt 2000"

	body := readRouterFile(t)
	if strings.Count(body, shipped) != 1 {
		t.Fatalf("the reference configuration has no %q", shipped)
	}

	addr := startHAProxy(t, binary, strings.Replace(body, shipped, "sc0_conn_cur gt 2", 1))

	var d net.Dialer

	dial := func() net.Conn {
		t.Helper()

		conn, err := d.DialContext(t.Context(), "tcp", addr)
		if err != nil {
			t.Fatalf("dial the router: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })

		return conn
	}

	// waiting reports whether the router is still holding a connection open, inside its
	// inspect delay, rather than having closed it.
	waiting := func(conn net.Conn) bool {
		t.Helper()

		if err := conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
			t.Fatalf("set a deadline: %v", err)
		}

		var timeout net.Error
		_, err := conn.Read(make([]byte, 1))

		return errors.As(err, &timeout) && timeout.Timeout()
	}

	// TWO HELD, ONCE THE TABLE HAS LET GO OF startHAProxy's OWN PROBES. Until then a probe
	// still counts against this source, and the second of these is the one refused, which
	// says nothing about the limit. Asked again until it holds, never waited for by the clock.
	var held []net.Conn

	deadline := time.Now().Add(30 * time.Second)

	for {
		held = []net.Conn{dial(), dial()}
		if waiting(held[0]) && waiting(held[1]) {
			break
		}

		for _, conn := range held {
			_ = conn.Close()
		}

		if time.Now().After(deadline) {
			t.Fatal("two connections from one source were never both held inside a limit of 2")
		}

		time.Sleep(100 * time.Millisecond)
	}

	// REFUSED AS IT CONNECTS, which a client sees at the dial or at its first read,
	// whichever the reset reaches first.
	refused, err := d.DialContext(t.Context(), "tcp", addr)
	if err == nil {
		t.Cleanup(func() { _ = refused.Close() })

		if err := refused.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatalf("set a deadline: %v", err)
		}

		_, err = refused.Read(make([]byte, 1))
	}

	if !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("the connection past the limit answered %v, want it closed as it connected", err)
	}

	// AND THE TWO INSIDE IT ARE STILL HELD: the refusal was of the third alone.
	for i, conn := range held {
		if !waiting(conn) {
			t.Errorf("connection %d inside the limit was closed when the one past it was refused", i)
		}
	}
}

// THE REFERENCE FILE KEEPS THE RULES IT STATES, in the file an operator copies: no
// certificate and no default route.
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
		"tcp-request connection track-sc0 src",
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

// THE DOCUMENT SHOWS THE FILE THAT WAS TESTED, included and never copied, and its example
// names the names that file routes, so copying both as written works.
func TestTheGuideShowsTheTestedRouterFile(t *testing.T) {
	t.Parallel()

	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "deploying", "many-deployments.md"))
	if err != nil {
		t.Fatalf("read the guide: %v", err)
	}

	if !strings.Contains(string(doc), "```{literalinclude} ../../deploy/router/haproxy.cfg") {
		t.Error("the guide does not include deploy/router/haproxy.cfg")
	}

	if strings.Contains(string(doc), "use_backend deployment-") {
		t.Error("the guide holds a copy of the router's routes, which nothing tests")
	}

	body := readRouterFile(t)

	for _, name := range []string{"a.nodes.billet.example", "a.enroll.billet.example"} {
		if !strings.Contains(string(doc), name+":443") || !strings.Contains(body, name) {
			t.Errorf("%s is not named in both the guide's config and the router's file", name)
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

		_ = cmd.Wait() //nolint:errcheck // cancelled above, so its status only says it was killed

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
