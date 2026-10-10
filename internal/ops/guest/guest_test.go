package guest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/guestreport"
)

const childEnv = "BILLET_GUEST_MONITOR_CHILD"

func testEnv() (cli.Env, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer

	return cli.Env{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader(""),
		Getenv: func(string) string { return "" }}, &stdout, &stderr
}

func TestGuestNamesItsTwoCommands(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{nil, {"watch"}} {
		env, _, _ := testEnv()
		if err := Run(t.Context(), env, args); err == nil || !strings.Contains(err.Error(), "monitor|flush") {
			t.Errorf("billet guest %v answered %v, want its usage", args, err)
		}
	}

	env, _, _ := testEnv()
	if err := Run(t.Context(), env, []string{"monitor", "--nice", "40"}); err == nil ||
		!strings.Contains(err.Error(), "not a nice value") {
		t.Errorf("--nice 40 answered %v, want it refused", err)
	}

	env, _, _ = testEnv()
	if err := Run(t.Context(), env, []string{"monitor", "extra"}); err == nil ||
		!strings.Contains(err.Error(), "unexpected argument") {
		t.Errorf("an argument answered %v, want it refused: the environment carries everything", err)
	}
}

// Where the agent cannot run, it says so; where it can, a guest without the
// node's endpoint and bearer has nothing to report to, and the agent exits 0.
func TestMonitorWithoutItsEnvironmentReportsNothingAndExitsZero(t *testing.T) {
	t.Parallel()

	env, _, stderr := testEnv()
	err := Run(t.Context(), env, []string{"monitor", "--socket", "", "--nice", "0"})

	if runtime.GOOS != "linux" {
		if err == nil || !strings.Contains(err.Error(), "Linux") {
			t.Fatalf("off Linux the monitor answered %v, want a refusal naming Linux", err)
		}

		return
	}

	if err != nil || !strings.Contains(stderr.String(), "BILLET_CACHE_ENDPOINT") {
		t.Fatalf("answered %v, logged %q; want nil and the reason", err, stderr.String())
	}
}

// The hook's client exits 0, quietly, with nobody to ask.
func TestFlushExitsZeroQuietlyWithNoMonitor(t *testing.T) {
	t.Parallel()

	env, stdout, stderr := testEnv()

	began := time.Now()
	err := Run(t.Context(), env, []string{"flush", "--socket", filepath.Join(t.TempDir(), "absent.sock")})

	if err != nil || stdout.Len() != 0 || stderr.Len() != 0 || time.Since(began) > time.Second {
		t.Fatalf("answered %v after %v, printed %q %q", err, time.Since(began), stdout.String(), stderr.String())
	}
}

// node is the guest-facing service, recording what it is sent.
type node struct {
	mu     sync.Mutex
	hellos int
	posts  []sent
	err    error
}

type sent struct {
	final bool
	batch guestreport.Batch
	err   error
}

func (n *node) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if r.Header.Get("Authorization") != "Bearer child-bearer" {
		w.WriteHeader(http.StatusUnauthorized)

		return
	}

	if r.URL.Path == "/v1/guest/limits" {
		n.hellos++

		if _, err := io.WriteString(w, `{"schemas":[1],"interval_ms":10000,"max_body":65536}`); err != nil {
			n.err = err
		}

		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, guestreport.MaxBatchBytes))
	if err != nil {
		n.err = err
	}

	b, err := guestreport.DecodeBatch(body)
	n.posts = append(n.posts, sent{final: r.URL.RawQuery == "final=1", batch: b, err: err})
	w.WriteHeader(http.StatusNoContent)
}

func (n *node) helloed() int {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.hellos
}

func (n *node) snapshot() (int, []sent, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.hellos, append([]sent(nil), n.posts...), n.err
}

// THE WHOLE PATH A SIGTERM TAKES, in a process of its own on Linux: the command
// line's signal handling cancels the agent, which reads the real /proc, sends one
// final batch and exits 0 within 3 s; the endpoint and the bearer travel in the
// environment alone; and every thread of the agent runs at the nice it set.
func TestSIGTERMEndsTheMonitorWithOneFinalBatchAndStatusZero(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the monitor runs on Linux alone")
	}

	t.Parallel()

	n := &node{}
	server := httptest.NewServer(n)
	t.Cleanup(server.Close)

	dir, err := os.MkdirTemp("/tmp", "billet-guest-") //nolint:usetesting // t.TempDir exceeds the unix socket address limit
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	args, err := json.Marshal([]string{"guest", "monitor", "--socket", filepath.Join(dir, "m.sock"),
		"--diag", filepath.Join(dir, "_diag"), "--junit-root", dir})
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestMonitorChild$")
	cmd.Env = append(os.Environ(), childEnv+"="+string(args),
		"BILLET_CACHE_ENDPOINT="+server.URL, "BILLET_CACHE_TOKEN=child-bearer")

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for n.helloed() == 0 {
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill() //nolint:errcheck // the test fails either way
			t.Fatalf("the monitor never said hello; its log: %s", stderr.String())
		}

		time.Sleep(20 * time.Millisecond)
	}

	assertNice(t, cmd.Process.Pid, defaultNice)

	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/cmdline")
	if err != nil || bytes.Contains(cmdline, []byte("child-bearer")) || bytes.Contains(cmdline, []byte(server.URL)) {
		t.Fatalf("the monitor's argv (%v) carries the endpoint or the bearer: %q", err, cmdline)
	}

	stopped := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	err = cmd.Wait()
	took := time.Since(stopped)

	if err != nil {
		t.Fatalf("the monitor exited %v; its log: %s", err, stderr.String())
	}

	if took > 3*time.Second {
		t.Errorf("the monitor took %v to exit after SIGTERM, past 3s", took)
	}

	t.Logf("the monitor's log: %s", stderr.String())

	_, posts, err := n.snapshot()
	if err != nil {
		t.Fatalf("the node failed to serve the monitor: %v", err)
	}

	finals := 0
	for _, p := range posts {
		if p.err != nil {
			t.Errorf("a batch did not decode: %v", p.err)
		}

		if p.final {
			finals++
		}
	}

	if finals != 1 || !posts[len(posts)-1].final {
		t.Fatalf("%d posts, %d final; want exactly one final, the last", len(posts), finals)
	}

	final := posts[len(posts)-1].batch
	// The agent's own CPU can round to no tick in a second's run; its processes
	// cannot be none.
	if final.TicksPerSecond != 100 || len(final.Processes) != 1 || len(final.Processes[0].Rows) == 0 {
		t.Errorf("the final batch read nothing real from /proc: %d ticks/s, %d process samples; its log: %s",
			final.TicksPerSecond, len(final.Processes), stderr.String())
	}
}

// TestMonitorChild is the monitor's process for the test above, run through the
// command line as the binary runs it.
func TestMonitorChild(t *testing.T) {
	raw := os.Getenv(childEnv)
	if raw == "" {
		t.Skip("run by TestSIGTERMEndsTheMonitorWithOneFinalBatchAndStatusZero alone")
	}

	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatal(err)
	}

	env := cli.Env{Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin, Getenv: os.Getenv}
	tree := func(*cli.Lifecycle) []cli.Command {
		return []cli.Command{{Name: "guest", Run: Run}}
	}

	os.Exit(cli.Main(args, env, tree, os.Exit))
}

// assertNice checks every thread of pid runs at nice.
func assertNice(t *testing.T, pid, nice int) {
	t.Helper()

	tasks, err := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/task")
	if err != nil {
		t.Fatal(err)
	}

	for _, task := range tasks {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/task/" + task.Name() + "/stat")
		if err != nil {
			continue
		}

		fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
		if len(fields) < 17 || fields[16] != strconv.Itoa(nice) {
			t.Errorf("thread %s runs at nice %v, want %d", task.Name(), fields[16:17], nice)
		}
	}
}
