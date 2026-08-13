package ceph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/junioryono/billet/internal/config"
)

// valid is a storage block that passes config.CheckCeph, so a case can change
// exactly the field it is about.
func valid() config.CephConfig {
	// DELIBERATELY NOT DefaultCephUser: a client that hard-coded the default
	// instead of reading the configured identity would pass every assertion that
	// used "billet" as the fixture.
	return config.CephConfig{
		User:      "site-reader",
		ImagePool: "billet-images",
		CachePool: "billet-cache",
	}
}

// recorder captures the invocations billet builds and answers each one.
//
// IT DISPATCHES ON WHAT IT WAS ASKED, not on call order. A fake keyed on
// position agrees with whatever sequence the code happens to make, so reordering
// two calls — or dropping one and adding another — leaves it green while the
// invocations have changed completely. Answering by binary and subcommand makes
// the fake state what it believes billet asks for, which is the thing under test.
type recorder struct {
	calls [][]string
	bins  []string

	rawImages string   // raw bytes for the image pool, overriding images
	images    []string // what `rbd -p <image pool> ls` returns
	caches    []string // what `rbd -p <cache pool> ls` returns
	minCompat string   // what `ceph osd get-require-min-compat-client` returns
	pools     string   // what `ceph osd pool ls detail` returns, as json

	cloneFormat      string // the rbd_default_clone_format `rbd config pool list` reports
	cacheCloneFormat string // ...for the cache pool, when it differs from the image pool
	cloneSource      string // where rbd says that value came from: "pool" or "config"
	rawConfig        string // raw bytes for that listing, overriding both

	err error // if set, every invocation fails with it
}

// answer is the default cluster: clone v2, both pools mirrored.
func answer() *recorder {
	return &recorder{
		minCompat:   "mimic",
		cloneFormat: "auto",
		pools: `[{"pool_name":"billet-images","size":2,"min_size":1},
		         {"pool_name":"billet-cache","size":2,"min_size":1}]`,
	}
}

func (r *recorder) run(_ context.Context, bin string, args []string) ([]byte, error) {
	r.calls = append(r.calls, args)
	r.bins = append(r.bins, bin)

	if r.err != nil {
		return nil, r.err
	}

	joined := strings.Join(args, " ")

	switch {
	case strings.Contains(joined, "get-require-min-compat-client"):
		return []byte(r.minCompat + "\n"), nil

	case strings.Contains(joined, "pool ls detail"):
		if r.pools == "" {
			return []byte(`[]`), nil
		}

		return []byte(r.pools), nil

	case strings.Contains(joined, "config pool list"):
		if r.rawConfig != "" {
			return []byte(r.rawConfig), nil
		}

		format := r.cloneFormat
		if strings.Contains(joined, "billet-cache") && r.cacheCloneFormat != "" {
			format = r.cacheCloneFormat
		}

		source := r.cloneSource
		if source == "" {
			source = "config"
		}

		return []byte(`[{"name":"rbd_default_clone_format","value":"` + format +
			`","source":"` + source + `"}]`), nil

	case strings.Contains(joined, "-p billet-images"):
		if r.rawImages != "" {
			return []byte(r.rawImages), nil
		}

		return jsonList(r.images), nil

	case strings.Contains(joined, "-p billet-cache"):
		return jsonList(r.caches), nil
	}

	return []byte(`[]`), nil
}

func jsonList(names []string) []byte {
	out, err := json.Marshal(names)
	if err != nil || names == nil {
		return []byte(`[]`)
	}

	return out
}

func client(t *testing.T, cfg config.CephConfig, rec *recorder) *Client {
	t.Helper()

	c, err := New(cfg, WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"), withRunner(rec.run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return c
}

// rbdCalls and cephCalls split the invocations by binary, because each has its
// own flag parsing and its own argv to get right.
func (r *recorder) rbdCalls() [][]string { return r.callsTo("rbd") }

func (r *recorder) cephCalls() [][]string { return r.callsTo("ceph") }

func (r *recorder) callsTo(name string) [][]string {
	var out [][]string

	for i, bin := range r.bins {
		if filepath.Base(bin) == name {
			out = append(out, r.calls[i])
		}
	}

	return out
}

// THE ARGUMENTS ARE THE PART WITH MISTAKES IN IT.
//
// Every one of these is a decision: --id is what stops rbd choosing client.admin
// for itself, --format json is what makes the output parseable rather than
// scraped, and -p is what confines the call to the pool this identity was granted.
func TestTheInvocationNamesTheIdentityAndThePool(t *testing.T) {
	t.Parallel()

	rec := answer()
	rec.images = []string{"ubuntu-2404-x64"}
	rec.caches = []string{"job-1", "job-2"}

	report, err := client(t, valid(), rec).CheckReachable(t.Context())
	if err != nil {
		t.Fatalf("CheckReachable: %v", err)
	}

	if got := len(rec.rbdCalls()); got != 4 {
		t.Fatalf("rbd was invoked %d times, want a listing and a clone-format read per pool", got)
	}

	// THE EXACT ARGV, not a joined substring match. Joining loses token
	// boundaries, so a client that built one argument reading
	// "--id billet --format json -p billet-images ls" would satisfy every
	// Contains check and produce an invocation rbd cannot parse.
	for i, want := range [][]string{
		{"--id", "site-reader", "--format", "json", "-p", "billet-images", "ls"},
		{"--id", "site-reader", "--format", "json", "-p", "billet-cache", "ls"},
		// THE POOL IS POSITIONAL for this subcommand. The first version of this
		// assertion carried `-p billet-images` — billet's own mistake, asserted
		// exactly, and green — until the real cluster answered
		// `rbd: unrecognised option '-p'`. An argv assertion pins what the code
		// does, which is only the same as what the tool accepts if somebody ran it.
		//
		// BOTH POOLS, because the clone format can be set on one and not the other.
		{"--id", "site-reader", "--format", "json", "config", "pool", "list", "billet-images"},
		{"--id", "site-reader", "--format", "json", "config", "pool", "list", "billet-cache"},
	} {
		if got := rec.rbdCalls()[i]; !slices.Equal(got, want) {
			t.Errorf("call %d = %q, want %q", i, got, want)
		}
	}

	// AND THE ceph INVOCATIONS, EXACTLY. Nothing asserted them, so deleting
	// `--format json` from the ceph call shipped green while breaking the pool
	// listing on every real cluster — the JSON parse fails and a healthy cluster is
	// reported as unreadable. The release query deliberately does NOT ask for json,
	// because the mon answers it as a bare string regardless.
	for i, want := range [][]string{
		{"--id", "site-reader", "osd", "get-require-min-compat-client"},
		{"--id", "site-reader", "--format", "json", "osd", "pool", "ls", "detail"},
	} {
		if got := rec.cephCalls()[i]; !slices.Equal(got, want) {
			t.Errorf("ceph call %d = %q, want %q", i, got, want)
		}
	}

	// The counts are the point of the report, and a check that returned zeroes
	// alongside a nil error would pass every other assertion here.
	if len(report.Pools) != 2 || report.Pools[0].Images != 1 || report.Pools[1].Images != 2 {
		t.Errorf("report = %+v, want 1 image and 2 caches", report.Pools)
	}

	// IN THE ORDER THE CONFIG NAMES THEM, because the two are printed with
	// different purposes beside them and a swap would attribute each to the other.
	if report.Pools[0].Name != "billet-images" || report.Pools[1].Name != "billet-cache" {
		t.Errorf("the pools are reported as %q then %q", report.Pools[0].Name, report.Pools[1].Name)
	}

	if report.User != "site-reader" {
		t.Errorf("report names %q rather than the identity that answered", report.User)
	}

	if report.MinCompatClient != "mimic" || !report.CloneV2 {
		t.Errorf("the cluster answered mimic and the report says %q / clone v2 %v",
			report.MinCompatClient, report.CloneV2)
	}
}

// AN OMITTED PATH IS NOT AN EMPTY ONE.
//
// Ceph's own search path is what finds /etc/ceph/ceph.conf and the matching
// keyring. Passing `--conf ""` overrides that search with a file that does not
// exist, so a config that deliberately says nothing would fail on a host where
// everything is exactly where Ceph puts it.
func TestAnOptionalPathIsOmittedRatherThanEmpty(t *testing.T) {
	t.Parallel()

	rec := answer()
	if _, err := client(t, valid(), rec).CheckReachable(t.Context()); err != nil {
		t.Fatalf("CheckReachable: %v", err)
	}

	for _, flag := range []string{"--conf", "--keyring"} {
		for i, call := range rec.calls {
			for _, arg := range call {
				if arg == flag {
					t.Errorf("call %d passes %s for a path the config left unset: %v", i, flag, call)
				}
			}
		}
	}
}

// ...AND A PATH THAT WAS SET IS PASSED.
//
// The other direction, because a client that dropped both flags entirely would
// satisfy the test above while ignoring the operator's cluster completely.
func TestAConfiguredPathIsPassed(t *testing.T) {
	t.Parallel()

	cfg := valid()
	cfg.ConfPath = "/etc/billet/ceph.conf"
	cfg.KeyringPath = "/etc/billet/billet.keyring"

	rec := answer()
	if _, err := client(t, cfg, rec).CheckReachable(t.Context()); err != nil {
		t.Fatalf("CheckReachable: %v", err)
	}

	// BOTH INVOCATIONS. Asserting only the first leaves a client that dropped the
	// flags from the second call green — and the second call is the cache pool,
	// which is the one billet writes to on every job.
	for i, pool := range []string{"billet-images", "billet-cache"} {
		want := []string{
			"--id", "site-reader",
			"--conf", "/etc/billet/ceph.conf",
			"--keyring", "/etc/billet/billet.keyring",
			"--format", "json", "-p", pool, "ls",
		}
		if got := rec.rbdCalls()[i]; !slices.Equal(got, want) {
			t.Errorf("call %d = %q, want %q", i, got, want)
		}
	}
}

// EVERY COMMAND AUTHENTICATES AS THE SAME CLIENT.
//
// rbd and ceph are separate binaries with separate flag parsing, and the identity
// is what confines both to the pools this node was granted. A ceph call that
// dropped --id would fall back to client.admin — which is the one answer the whole
// identity rule exists to prevent, arriving through the command nobody was
// watching.
func TestBothCommandsCarryTheSameIdentity(t *testing.T) {
	t.Parallel()

	cfg := valid()
	cfg.ConfPath = "/etc/billet/ceph.conf"
	cfg.KeyringPath = "/etc/billet/billet.keyring"

	rec := answer()
	if _, err := client(t, cfg, rec).CheckReachable(t.Context()); err != nil {
		t.Fatalf("CheckReachable: %v", err)
	}

	var sawCeph bool

	for i, call := range rec.calls {
		joined := strings.Join(call, " ")
		for _, must := range []string{
			"--id site-reader",
			"--conf /etc/billet/ceph.conf",
			"--keyring /etc/billet/billet.keyring",
		} {
			if !strings.Contains(joined, must) {
				t.Errorf("%s call %d does not carry %q: %s", rec.bins[i], i, must, joined)
			}
		}

		if strings.HasSuffix(rec.bins[i], "ceph") {
			sawCeph = true
		}
	}

	if !sawCeph {
		t.Error("no ceph invocation was made, so this asserts nothing about the second binary")
	}
}

// THE CONSTRUCTOR RE-APPLIES THE CONFIG RULES, because it is exported and cannot
// assume its argument came through config.Load.
//
// Two of them are load-bearing in this package specifically. A pool name is half
// of the positional `pool/image` specs billet builds, where rbd reads a leading
// dash as an option it does not recognise — measured, and NOT true of `-p`, which
// consumes whatever token follows it. And `admin` would put a key that can delete
// a pool in the hands of a process whose whole job is reading two of them.
func TestTheConstructorRefusesWhatTheConfigWould(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		mutate func(*config.CephConfig)
		want   string
	}{
		{
			name:   "an administrator",
			mutate: func(c *config.CephConfig) { c.User = "admin" },
			want:   "can delete the pools",
		},
		{
			name:   "a pool rbd would read as an option",
			mutate: func(c *config.CephConfig) { c.ImagePool = "-p" },
			want:   "starting with a dash",
		},
		{
			name:   "no identity at all",
			mutate: func(c *config.CephConfig) { c.User = "" },
			want:   "node.ceph.user",
		},
		{
			name:   "one pool serving as both",
			mutate: func(c *config.CephConfig) { c.CachePool = c.ImagePool },
			want:   "cannot share a pool",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := valid()
			tc.mutate(&cfg)

			c, err := New(cfg, WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"))
			if err == nil {
				t.Fatalf("New accepted %s", tc.name)
			}

			if c != nil {
				t.Error("New returned a client alongside an error")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not explain the refusal: %v", err)
			}
		})
	}
}

// A FAILURE HAS TO NAME WHAT WAS BEING TRIED.
//
// "permission denied" on its own sends an operator to the wrong file: the
// question is always which pool, and as whom. rbd's own last line is kept because
// it is the sentence that says what librados actually objected to.
func TestAFailedListNamesThePoolAndTheIdentity(t *testing.T) {
	t.Parallel()

	rec := answer()
	rec.err = errors.New("exit status 1: rbd: listing images failed: (1) Operation not permitted")

	_, err := client(t, valid(), rec).CheckReachable(t.Context())
	if err == nil {
		t.Fatal("CheckReachable reported success against a failing rbd")
	}

	for _, want := range []string{"billet-images", "client.site-reader", "Operation not permitted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not carry %q: %v", want, err)
		}
	}

	// AND IT IS NOT A TIMEOUT. Reporting every failure as a deadline would satisfy
	// the bounded-invocation test while telling an operator their cluster is
	// unreachable when rbd answered them clearly.
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a permission failure was reported as a deadline: %v", err)
	}
}

// OUTPUT BILLET CANNOT PARSE IS NOT ECHOED BACK.
//
// Unparseable stdout means billet is talking to something that is not the rbd it
// expected, and putting whatever that was on an operator's terminal is how an
// unrelated program's output — or a file it happened to print — becomes billet's
// error message.
func TestUnparseableOutputIsNotRendered(t *testing.T) {
	t.Parallel()

	const secretish = "AQDH631qo1CfBxAA9ZsjJleBiq9V2OqUCfIn9Q=="

	rec := answer()
	rec.rawImages = "key = " + secretish

	_, err := client(t, valid(), rec).CheckReachable(t.Context())
	if err == nil {
		t.Fatal("CheckReachable accepted output that is not a json image list")
	}

	if strings.Contains(err.Error(), secretish) {
		t.Errorf("the error rendered the command's output: %v", err)
	}

	if !strings.Contains(err.Error(), "is it the rbd command") {
		t.Errorf("the error does not say what billet concluded: %v", err)
	}
}

// A CLUSTER THAT IS NOT THERE DOES NOT REFUSE, IT WAITS.
//
// librados retries an unreachable monitor for minutes. Without a bound, `billet
// check` hangs with no output at exactly the moment an operator is trying to find
// out why nothing works.
func TestAnInvocationIsBounded(t *testing.T) {
	t.Parallel()

	// THE FAKE HAS ITS OWN ESCAPE, and it is not decoration. A runner that only
	// waits on ctx.Done() cannot answer at all once the bound under test is
	// removed, so the mutation that deletes the timeout makes this test HANG
	// rather than fail — which reads as a hung suite instead of a missing guard,
	// and is what actually happened. The safety timer is far longer than the
	// configured bound, so it can only fire when the bound did not.
	//
	// time.NewTimer rather than time.After, which forbidigo bans for leaking its
	// timer until it fires.
	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		WithTimeout(20*time.Millisecond),
		withRunner(func(ctx context.Context, _ string, _ []string) ([]byte, error) {
			safety := time.NewTimer(5 * time.Second)
			defer safety.Stop()

			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("rbd: %w", ctx.Err())
			case <-safety.C:
				return nil, errors.New("nothing bounded this invocation")
			}
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	start := time.Now()
	// errors.Is, not a substring: a %v somewhere in the wrapping chain would
	// leave the text intact and break every caller that asks what went wrong.
	if _, err := c.CheckReachable(t.Context()); err == nil {
		t.Fatal("CheckReachable waited out an unreachable cluster and reported success")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the failure is not the deadline: %v", err)
	}

	// The bound has to be the one that was configured. A client that gave up
	// because the TEST's context ended would satisfy every assertion above.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the configured timeout did not apply; the call took %s", elapsed)
	}
}

// LIBRADOS LOGS TO STDERR ON EVERY CONNECTION, so a successful listing routinely
// arrives with warnings beside it. Folding the two streams together makes valid
// JSON unparseable, which reads as a broken cluster.
func TestDiagnosticsDoNotCorruptTheResult(t *testing.T) {
	t.Parallel()

	// The real thing: execRunner, against a program that writes to both streams.
	out, err := execRunner(t.Context(), "/bin/sh",
		[]string{"-c", `echo "2026-08-13 -1 monclient: hunting for new mon" >&2; echo '["a","b"]'`})
	if err != nil {
		t.Fatalf("execRunner: %v", err)
	}

	if got := strings.TrimSpace(string(out)); got != `["a","b"]` {
		t.Errorf("stdout = %q; stderr leaked into the result", got)
	}
}

// A FAILURE'S LAST LINE IS THE ONE THAT SAYS WHY.
//
// librados prints a paragraph of connection logging in front of the sentence an
// operator can act on, and an error that carried the paragraph would bury it.
func TestAFailureKeepsTheLineThatExplainsIt(t *testing.T) {
	t.Parallel()

	_, err := execRunner(t.Context(), "/bin/sh",
		[]string{"-c", `echo "monclient: hunting for new mon" >&2; echo "rbd: error opening pool" >&2; exit 1`})
	if err == nil {
		t.Fatal("execRunner reported success for a command that exited 1")
	}

	if !strings.Contains(err.Error(), "rbd: error opening pool") {
		t.Errorf("the error dropped the line that explains it: %v", err)
	}

	if strings.Contains(err.Error(), "hunting for new mon") {
		t.Errorf("the error carries the connection log in front of the reason: %v", err)
	}
}

// THE REAL RUNNER MUST BE BOUNDED TOO, and the fake cannot say so.
//
// TestAnInvocationIsBounded proves the client derives a deadline; it says nothing
// about exec, so swapping exec.CommandContext for exec.Command would leave it
// green. This drives execRunner against a program that never exits.
//
// WaitDelay is what makes it terminate rather than merely be killed: Output reads
// through pipes and Wait blocks until they see EOF, so a process that leaves a
// descendant holding the write end keeps the CALL open after the process is dead.
// The subprocess here does exactly that.
// THE TEST BOUNDS ITSELF, and that is not belt-and-braces. A test for "this call
// returns" cannot wait for the call to return, or removing the bound makes it HANG
// until the whole package times out — which reads as a broken suite rather than as
// a missing guard. That mistake was made once already, one test up.
func TestTheRealRunnerIsBounded(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	type result struct {
		out []byte
		err error
	}

	// Buffered, so the goroutine finishes even when this test has already given
	// up: on the failure path it is waiting out the subprocess and must not be
	// left blocked on a send nobody will receive.
	done := make(chan result, 1)
	start := time.Now()

	go func() {
		// `sleep` in a subshell inherits stdout, so killing the shell leaves the
		// pipe open — which is the condition WaitDelay exists for.
		out, err := execRunner(ctx, "/bin/sh", []string{"-c", "sleep 30 & sleep 30"})
		done <- result{out, err}
	}()

	guard := time.NewTimer(15 * time.Second)
	defer guard.Stop()

	var got result

	select {
	case got = <-done:
	case <-guard.C:
		t.Fatal("execRunner had not returned 15s after a 200ms deadline: the process may have " +
			"been killed, but the call is not bounded")
	}

	if got.err == nil {
		t.Fatal("execRunner returned success for a command that never finished")
	}

	if !errors.Is(got.err, context.DeadlineExceeded) {
		t.Errorf("the failure is not the deadline: %v", got.err)
	}

	// AGAINST THE CONFIGURED BOUND, not a round number: a WaitDelay raised to nine
	// seconds would slip under a flat ten-second assertion while making every
	// unreachable cluster take nine seconds longer than it should. The slack is for
	// scheduling, not for a different constant.
	// AN INDEPENDENT THRESHOLD. Deriving the budget from waitDelay means raising
	// waitDelay raises the budget with it, so the mutation the comment claims to
	// catch stays green; the policy is asserted separately, below.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("execRunner took %s to give up on a process it had killed", elapsed)
	}

	if waitDelay > 3*time.Second {
		t.Errorf("waitDelay is %s: it bounds how long a dead process's pipes may hold `billet "+
			"check` open, and it is meant to be a couple of seconds", waitDelay)
	}
}

// A DIAGNOSTIC IS ONE LINE AND IT IS BOUNDED.
//
// rbd's stderr is another program's output, so what reaches a terminal or a log
// has to be limited by policy rather than by trust. Only the final line survives —
// librados prints a paragraph of connection logging in front of the sentence that
// matters — and it is capped, so a program that dumps a file cannot dump it
// through billet.
func TestOnlyTheLastDiagnosticLineSurvivesAndItIsCapped(t *testing.T) {
	t.Parallel()

	const secretish = "AQDH631qo1CfBxAA9ZsjJleBiq9V2OqUCfIn9Q=="

	script := "echo 'key = " + secretish + "' >&2; echo 'rbd: listing images failed: (1) Operation not permitted' >&2; exit 1"

	_, err := execRunner(t.Context(), "/bin/sh", []string{"-c", script})
	if err == nil {
		t.Fatal("execRunner reported success for a command that exited 1")
	}

	if strings.Contains(err.Error(), secretish) {
		t.Errorf("an earlier stderr line reached the error: %v", err)
	}

	if !strings.Contains(err.Error(), "Operation not permitted") {
		t.Errorf("the line that explains the failure was dropped: %v", err)
	}

	long, err := execRunner(t.Context(),
		"/bin/sh", []string{"-c", "head -c 5000 /dev/zero | tr '\\0' 'x' >&2; exit 1"})
	if long != nil {
		t.Errorf("execRunner returned output alongside an error: %q", long)
	}

	if err == nil {
		t.Fatal("execRunner reported success for a command that exited 1")
	}

	// BOTH BOUNDS, because deriving the threshold from the production constant
	// alone means raising that constant to 5000 leaves this green.
	if n := len(err.Error()); n > maxDiagnostic+100 {
		t.Errorf("a %d-byte diagnostic reached the caller; it is meant to be capped at %d",
			n, maxDiagnostic)
	} else if n > 400 {
		t.Errorf("a %d-byte diagnostic reached the caller; whatever maxDiagnostic says, this is "+
			"another program's output on an operator's terminal", n)
	}
}

// A MISSING rbd IS ITS OWN ANSWER, so a caller can tell "this host has no client"
// from "this host cannot reach the cluster" — the first is an install, the second
// is a network or a keyring.
func TestAMissingBinaryIsDistinguishable(t *testing.T) {
	// Not parallel: it edits the environment, which t.Setenv refuses to do in a
	// parallel test for exactly the reason it would be wrong here.
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))

	c, err := New(valid())
	if err == nil {
		t.Fatal("New found an rbd on an empty PATH")
	}

	if c != nil {
		t.Error("New returned a client alongside an error")
	}

	if !errors.Is(err, ErrNoClient) {
		t.Errorf("the error is not ErrNoClient, so a caller cannot tell it from an unreachable "+
			"cluster: %v", err)
	}

	if !strings.Contains(err.Error(), "ceph-common") {
		t.Errorf("the error does not say what to install: %v", err)
	}
}

// AN EXPLICIT BINARY SKIPS THE LOOKUP, which is what makes the tests above able to
// run on a machine with no ceph installed — and is worth asserting, because a
// constructor that looked the binary up anyway would make the whole suite depend
// on the developer's laptop.
func TestAnExplicitBinaryNeedsNoPath(t *testing.T) {
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))

	var ran []string

	// NOT os.Args[0], not absolute, and with no separator: a WithBinary that
	// honoured only the test's own executable, only an absolute path, or only
	// something that looks like a path would pass in turn — and the option's whole
	// job is naming an rbd that is not the one on PATH.
	const elsewhere = "rbd-from-the-vendor"

	c, err := New(valid(), WithBinary(elsewhere), WithCephBinary("ceph-from-the-vendor"),
		withRunner(func(_ context.Context, bin string, args []string) ([]byte, error) {
			ran = append(ran, bin)

			// Enough of a cluster to reach the end of the check; what this test is
			// about is which binaries were run, not what they said.
			if strings.Contains(strings.Join(args, " "), "get-require-min-compat-client") {
				return []byte("mimic\n"), nil
			}

			return []byte(`[]`), nil
		}))
	if err != nil {
		t.Fatalf("New refused an explicit binary on an empty PATH: %v", err)
	}

	if _, err := c.CheckReachable(t.Context()); err != nil {
		t.Fatalf("CheckReachable: %v", err)
	}

	// THE ONE THAT WAS SUPPLIED, because a constructor that stored the option and
	// then ran something else would satisfy every other assertion here.
	// BOTH NAMES, because two options set two binaries and honouring one while
	// falling back to PATH for the other is exactly the half-done implementation
	// this asserts against — and PATH is empty, so the fallback would fail loudly
	// only if it were reached at construction rather than here.
	for _, want := range []string{elsewhere, "ceph-from-the-vendor"} {
		if !slices.Contains(ran, want) {
			t.Errorf("ran %q, want the binaries the options named (%q missing)", ran, want)
		}
	}
}

// A PROCESS THAT ANSWERED KEEPS ITS ANSWER, even when the clock ran out while the
// pipes were still draining.
//
// The shape: rbd exits non-zero with the sentence that explains why, but a
// descendant is holding stdout, so Output waits out WaitDelay and the context
// expires during that wait. Consulting ctx.Err() first would replace
// `(2) No such file or directory` with a timeout — throwing away exactly what
// lastLine exists to carry, on the path where an operator most needs it.
func TestARealFailureSurvivesAnExpiredContext(t *testing.T) {
	t.Parallel()

	// TWO EXIT CODES, because one fixture lets the discriminator be `ExitCode() == 2`
	// rather than "the process exited at all", and that mutation would report every
	// other failure as a timeout.
	for _, tc := range []struct{ code, message string }{
		{code: "1", message: "rbd: listing images failed: (1) Operation not permitted"},
		{code: "2", message: "rbd: listing images failed: (2) No such file or directory"},
		{code: "13", message: "rbd: listing images failed: (13) Permission denied"},
	} {
		t.Run("exit "+tc.code, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
			defer cancel()

			type result struct {
				out []byte
				err error
			}

			done := make(chan result, 1)

			go func() {
				// The shell writes its diagnostic and exits immediately; the
				// background `sleep` inherits stdout and holds the pipe open well
				// past the deadline.
				out, err := execRunner(ctx, "/bin/sh", []string{"-c",
					"sleep 30 & echo '" + tc.message + "' >&2; exit " + tc.code})
				done <- result{out, err}
			}()

			guard := time.NewTimer(15 * time.Second)
			defer guard.Stop()

			var got result

			select {
			case got = <-done:
			case <-guard.C:
				t.Fatal("execRunner never returned")
			}

			if got.err == nil {
				t.Fatalf("execRunner reported success for a command that exited %s", tc.code)
			}

			if !strings.Contains(got.err.Error(), tc.message) {
				t.Errorf("the process exited with an explanation and the error does not carry "+
					"it: %v", got.err)
			}

			// The context DID expire — that is the whole setup — so this is the
			// assertion that the ordering is right rather than that the race did
			// not happen.
			if ctx.Err() == nil {
				t.Fatal("the context had not expired, so this case did not stage the race it " +
					"is about")
			}

			if errors.Is(got.err, context.DeadlineExceeded) {
				t.Errorf("a process that exited on its own was reported as a timeout: %v", got.err)
			}
		})
	}
}

// THE TAIL, NOT THE HEAD, and bounded as it is written rather than afterwards.
//
// Collecting all of another program's stderr bounds the time an invocation takes
// and not the memory, so a broken executable can allocate for the whole timeout.
// Keeping the tail is what makes the bound compatible with lastLine: librados
// prints its connection logging first and the sentence that matters last.
func TestStderrIsBoundedAsItArrives(t *testing.T) {
	t.Parallel()

	w := &tailWriter{limit: 8}

	for _, chunk := range []string{"aaaa", "bbbb", "cccc"} {
		n, err := w.Write([]byte(chunk))
		if err != nil {
			t.Fatalf("Write: %v", err)
		}

		// io.Writer's contract: a short count IS an error, and reporting one would
		// make exec conclude the pipe broke.
		if n != len(chunk) {
			t.Errorf("Write reported %d of %d bytes", n, len(chunk))
		}
	}

	if got := w.String(); got != "bbbbcccc" {
		t.Errorf("tail = %q, want the last 8 bytes", got)
	}

	// One write larger than the whole budget must not grow the buffer either.
	big := &tailWriter{limit: 4}
	oversized := []byte(strings.Repeat("x", 10000) + "END")

	n, err := big.Write(oversized)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// THE LENGTH WRITTEN, not the length kept. Reporting what survived the trim is
	// a short count, and io.Writer's contract makes that an error — exec would
	// conclude the pipe broke and abandon a command that was answering fine.
	if n != len(oversized) {
		t.Errorf("Write reported %d of %d bytes for an oversized write", n, len(oversized))
	}

	if got := big.String(); got != "xEND" {
		t.Errorf("tail = %q, want the last 4 bytes of one oversized write", got)
	}

	// A NON-POSITIVE LIMIT KEEPS NOTHING rather than panicking. Zero falls out of
	// the arithmetic on its own — which is why the guard's mutation survived a
	// zero-limit test — but the slice indexes from the END, so a negative limit is
	// a runtime panic, and a control plane must not carry one waiting for the next
	// caller who constructs this type.
	for _, limit := range []int{0, -1, -2} {
		none := &tailWriter{limit: limit}

		const payload = "anything at all"

		n, err := none.Write([]byte(payload))
		if err != nil {
			t.Fatalf("Write(limit %d): %v", limit, err)
		}

		// THE COUNT TOO. Returning 0 here is a short write, which io.Writer's
		// contract makes an error — exec would conclude the pipe broke.
		if n != len(payload) {
			t.Errorf("Write(limit %d) reported %d of %d bytes", limit, n, len(payload))
		}

		if got := none.String(); got != "" {
			t.Errorf("a tail with limit %d kept %q", limit, got)
		}
	}
}

// A CAPPED DIAGNOSTIC IS STILL TEXT.
//
// rbd's output is not guaranteed to be ASCII — a pool name is whatever the
// operator typed — and cutting a byte slice at a fixed offset splits a multi-byte
// rune, putting invalid UTF-8 into an error that is then printed, logged and
// marshalled.
func TestACappedDiagnosticIsValidUTF8(t *testing.T) {
	t.Parallel()

	// Repeated three-byte runes, so some cut offset inside maxDiagnostic must land
	// mid-rune whatever that constant is.
	// CONSTRUCTED SO THE CUT SPLITS A RUNE. A repeated multi-byte string is not
	// enough on its own: "é☃" is five bytes and maxDiagnostic is a multiple of it,
	// so the naive slice happened to land on a boundary and the mutation survived.
	// One filler byte short of the cap puts the boundary inside the next rune.
	noisy := strings.Repeat("x", maxDiagnostic-1) + strings.Repeat("é", 40)

	got := lastLine(noisy)
	if !utf8.ValidString(got) {
		t.Errorf("the capped diagnostic is not valid utf-8: %q", got)
	}

	if len(got) > maxDiagnostic+8 {
		t.Errorf("the diagnostic was not capped: %d bytes", len(got))
	}

	// AND THROUGH THE REAL COMPOSITION, which is where the first version of this
	// repair was unreachable: tailWriter cuts the tail to exactly maxDiagnostic
	// bytes, so lastLine's own length branch is false and the input arrives
	// already split. Testing lastLine alone proved a path production never takes.
	//
	// The payload is 100 three-byte runes followed by ONE ascii byte: 301 bytes, so
	// the 300-byte tail begins one byte into the first rune. A plain repetition is
	// not enough — 300 is a multiple of both 2 and 3, so "éé…" and "☃☃…" both cut
	// cleanly and the mutation survived.
	//
	// BUILT IN GO AND PASSED AS AN ARGUMENT, not generated by the shell. A fixture
	// that reached for `seq` would, on a host without it, emit an ascii "command not
	// found" and a valid-utf8 error — passing for the wrong reason, and passing with
	// the repair deleted.
	payload := strings.Repeat("☃", 100) + "x"

	_, err := execRunner(t.Context(), "/bin/sh",
		[]string{"-c", `printf '%s' "$1" >&2; exit 1`, "sh", payload})
	if err == nil {
		t.Fatal("execRunner reported success for a command that exited 1")
	}

	if !utf8.ValidString(err.Error()) {
		t.Errorf("the error is not valid utf-8 through tailWriter -> lastLine: %q", err.Error())
	}
}

// A PROCESS THAT EXITED ZERO ANSWERED, whatever else is holding the pipe.
//
// The successful half of the race the test above covers, and the half the first
// fix missed: an exit of zero never produces an *exec.ExitError at all — Wait
// returns ErrWaitDelay — so the completed-exit branch cannot see it and the call
// would be reported as a timeout with rbd's answer thrown away.
func TestASuccessfulExitSurvivesAHeldPipe(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()

	type result struct {
		out []byte
		err error
	}

	done := make(chan result, 1)

	go func() {
		// The listing is written and the shell exits 0; the background `sleep`
		// inherits stdout and holds the pipe well past both the deadline and the
		// WaitDelay that follows it.
		out, err := execRunner(ctx, "/bin/sh", []string{"-c", `echo '["a","b"]'; sleep 30 & sleep 0.05`})
		done <- result{out, err}
	}()

	guard := time.NewTimer(15 * time.Second)
	defer guard.Stop()

	var got result

	select {
	case got = <-done:
	case <-guard.C:
		t.Fatal("execRunner never returned")
	}

	if ctx.Err() == nil {
		t.Fatal("the context had not expired, so this case did not stage the race it is about")
	}

	if got.err != nil {
		t.Fatalf("a command that exited 0 was reported as a failure: %v", got.err)
	}

	if strings.TrimSpace(string(got.out)) != `["a","b"]` {
		t.Errorf("the answer was discarded: %q", got.out)
	}
}

// THE CLUSTER'S CLONE FORMAT IS A PRECONDITION, and the rule is stated in the
// direction that stays true.
//
// A set of releases at-or-after mimic goes stale on the next Ceph release and
// starts refusing correct clusters. The set BEFORE mimic can never grow — Ceph is
// not going to ship a release older than one from 2018 — so an unrecognised name
// is treated as newer, and the table below asserts both halves of that.
//
// The pre-mimic names are measured rather than remembered: every one is a value
// Ceph 20.2.3 accepts for `osd set-require-min-compat-client`, and a name it does
// not know is refused with "is not recognized".
func TestClonesV2(t *testing.T) {
	t.Parallel()

	for _, release := range []string{
		"argonaut", "bobtail", "cuttlefish", "dumpling", "emperor", "firefly",
		"giant", "hammer", "infernalis", "jewel", "kraken", "luminous",
	} {
		if got, err := EffectiveCloneFormat(release, "auto"); err != nil || got != 1 {
			t.Errorf("%s predates clone v2 and resolved to %d (err %v)", release, got, err)
		}
	}

	for _, release := range []string{
		"mimic", "nautilus", "octopus", "pacific", "quincy", "reef", "squid", "tentacle",
		// A release that does not exist yet. Refusing it would be the failure mode
		// this rule is written to avoid: billet going stale and refusing a cluster
		// that is newer than billet is.
		"someday",
	} {
		if got, err := EffectiveCloneFormat(release, "auto"); err != nil || got != 2 {
			t.Errorf("%s is at or after mimic and resolved to %d (err %v)", release, got, err)
		}
	}

	// Whatever the cluster's formatting happens to be.
	for _, release := range []string{"  mimic\n", "MIMIC", " Luminous "} {
		want := 2
		if strings.Contains(strings.ToLower(release), "luminous") {
			want = 1
		}

		if got, err := EffectiveCloneFormat(release, "auto"); err != nil || got != want {
			t.Errorf("EffectiveCloneFormat(%q) = %d (err %v), want %d", release, got, err, want)
		}
	}
}

// AN ANSWER THAT IS NOT A RELEASE MUST NOT BE READ AS ONE.
//
// "anything I do not recognise is newer than mimic" is a fail-OPEN rule, so what
// bounds it is the shape check. `unknown` is the case that matters and it is not
// hypothetical: it is the zero value of Ceph's release enum, and
// `osd set-require-min-compat-client unknown` is REFUSED — measured — so it can
// only arrive from Ceph itself. It means the cluster was never told, and a cluster
// that was never told clones the old way.
func TestAnAnswerThatIsNotAReleaseIsRefused(t *testing.T) {
	t.Parallel()

	for _, answer := range []string{
		"",
		"  ",
		"mimic luminous",          // two tokens
		"12",                      // a number, not a name
		"Mimic-RC1",               // punctuation
		strings.Repeat("x", 4096), // a binary that printed something else entirely
	} {
		got, err := EffectiveCloneFormat(answer, "auto")
		if err == nil {
			t.Errorf("EffectiveCloneFormat(%.20q) accepted it as format %d", answer, got)

			continue
		}

		if !errors.Is(err, ErrUnclassifiedRelease) {
			t.Errorf("EffectiveCloneFormat(%.20q) failed with %v, not ErrUnclassifiedRelease",
				answer, err)
		}

		// AND THE ANSWER IS NOT ECHOED WHOLE. It came from a binary billet did not
		// write, so the same rule that refuses to render unparseable output applies
		// to rendering a rejected one — the 4096-byte case is what a wrong program
		// at that path looks like.
		if len(err.Error()) > maxDiagnostic+200 {
			t.Errorf("EffectiveCloneFormat rendered a %d-byte answer", len(err.Error()))
		}
	}
}

// A CLUSTER THAT WAS NEVER TOLD CLONES THE OLD WAY, and gets the remedy for it.
//
// `unknown` is the zero value of Ceph's release enum and the setter refuses it —
// measured — so it can only arrive from Ceph, meaning nobody has run
// set-require-min-compat-client. That is not an answer billet fails to
// understand; it is an answer that means clone v1, and the operator needs the
// same one command as a cluster that says `luminous`. Routing it through
// "billet could not classify your cluster" fails closed and sends them nowhere
// useful.
//
// The override still wins where it is set, because it genuinely does: a cluster
// with rbd_default_clone_format forced to 2 clones the new way whatever its floor
// says, and refusing it would be billet being wrong about a working cluster.
func TestAClusterThatWasNeverToldTakesTheOldCloneFormat(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		format  string
		refused bool
	}{
		{"auto", true}, // the floor decides, and it was never set
		{"", true},     // absent means auto
		{"1", true},    // forced, and forced to the old one
		{"2", false},   // forced to the new one, which really does win
	} {
		rec := answer()
		rec.minCompat = "unknown"
		rec.cloneFormat = tc.format
		rec.cacheCloneFormat = tc.format

		report, err := client(t, valid(), rec).CheckReachable(t.Context())

		if !tc.refused {
			if err != nil {
				t.Errorf("clone format %q: refused a cluster whose override forces clone v2: %v",
					tc.format, err)
			}

			continue
		}

		if err == nil {
			t.Errorf("clone format %q: accepted a cluster that clones the old way", tc.format)

			continue
		}

		if !errors.Is(err, ErrCloneV1) {
			t.Errorf("clone format %q: the failure is %v, not ErrCloneV1", tc.format, err)
		}

		// THE REPORT SURVIVES, so the CLI prints what it found beside the refusal —
		// and on the `auto` path the remedy is the floor, which is the one command
		// this operator is missing.
		if len(report.Pools) != 2 {
			t.Errorf("clone format %q: the refusal discarded the report: %+v", tc.format, report)
		}

		// THE FLOOR REMEDY IS GIVEN WHATEVER ELSE IS WRONG, because unsetting a
		// forced format leaves the pool on `auto`, where the floor decides — an
		// operator told only about the override fixes it, re-runs, and is refused
		// again for a reason nobody mentioned.
		if !strings.Contains(err.Error(), "set-require-min-compat-client") {
			t.Errorf("clone format %q: the error does not give the remedy for a cluster that was "+
				"never told: %v", tc.format, err)
		}

		// AND IT SAYS WHAT `unknown` MEANS. Printing `require-min-compat-client is
		// "unknown"` reads as a value somebody chose; what it means is that nobody
		// has, which is the sentence that leads to the fix.
		if !strings.Contains(err.Error(), "never been told") {
			t.Errorf("clone format %q: the error renders unknown as though it were a release "+
				"the operator set: %v", tc.format, err)
		}
	}
}

// A CONFIGURED VALUE BILLET DOES NOT MODEL REACHES THE PIPELINE, and is refused
// there rather than only by the rule in isolation.
//
// Dropping the error from EffectiveCloneFormat at its call site — `format, _ :=`
// — leaves format 0, which reads as "not v2" and falls into the generic clone-v1
// refusal with the wrong remedy. Every direct test of the rule stays green,
// because none of them drives a bad value through CheckReachable.
func TestAnUnmodelledCloneFormatIsRefusedThroughThePipeline(t *testing.T) {
	t.Parallel()

	rec := answer()
	rec.cloneFormat = "3"

	_, err := client(t, valid(), rec).CheckReachable(t.Context())
	if err == nil {
		t.Fatal("CheckReachable accepted a clone format billet does not model")
	}

	if errors.Is(err, ErrCloneV1) {
		t.Errorf("an unmodelled value was reported as clone v1, which gives the wrong remedy: %v", err)
	}

	if !strings.Contains(err.Error(), "not auto, 1 or 2") {
		t.Errorf("the error does not say what billet could not make sense of: %v", err)
	}
}

// THE FLOOR IS A PROXY, AND ONE CONFIG KEY DEFEATS IT.
//
// `rbd_default_clone_format` overrides what the cluster's minimum client release
// implies. Measured: a cluster whose floor is mimic and whose clone format is
// forced to 1 refuses to clone an unprotected snapshot with
// `rbd: clone error: (22) Invalid argument`. Checking only the floor would have
// been a green preflight beside the exact failure it exists to prevent.
func TestTheConfiguredCloneFormatOverridesTheFloor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		release, configured string
		want                int
	}{
		{"mimic", "1", 1},    // the floor says v2 and the override says otherwise
		{"luminous", "2", 2}, // and the other way round
		{"mimic", "auto", 2}, // the default, where the floor decides
		{"mimic", "", 2},     // absent means auto
		{"luminous", "", 1},  // ...and the floor still decides
	} {
		got, err := EffectiveCloneFormat(tc.release, tc.configured)
		if err != nil {
			t.Errorf("EffectiveCloneFormat(%q, %q): %v", tc.release, tc.configured, err)

			continue
		}

		if got != tc.want {
			t.Errorf("EffectiveCloneFormat(%q, %q) = %d, want %d",
				tc.release, tc.configured, got, tc.want)
		}
	}

	// AN OVERRIDE BILLET CANNOT READ IS NOT ASSUMED TO BE THE DEFAULT. A value that
	// is neither auto nor 1 nor 2 means the cluster is configured in a way this
	// rule does not model, and guessing "auto" there is the fail-open direction.
	if _, err := EffectiveCloneFormat("mimic", "3"); err == nil {
		t.Error("an unrecognised rbd_default_clone_format was accepted")
	}
}

// A FORCED CLONE FORMAT IS REFUSED WITH ITS OWN REMEDY, because "raise
// require-min-compat-client" is the wrong advice when the floor is already high
// enough and a config key is what is overriding it.
func TestAForcedCloneFormatIsRefusedWithTheRightRemedy(t *testing.T) {
	t.Parallel()

	rec := answer()
	rec.cloneFormat = "1"

	report, err := client(t, valid(), rec).CheckReachable(t.Context())
	if err == nil {
		t.Fatal("CheckReachable accepted a cluster forced to clone format 1")
	}

	if !errors.Is(err, ErrCloneV1) {
		t.Errorf("the failure is not ErrCloneV1: %v", err)
	}

	if !strings.Contains(err.Error(), "ceph config rm client rbd_default_clone_format") {
		t.Errorf("the error does not name the setting that is overriding the floor: %v", err)
	}

	if strings.Contains(err.Error(), "set-require-min-compat-client") {
		t.Errorf("the error gives the remedy for the other cause, which is already satisfied: %v", err)
	}

	// THE REPORT SURVIVES THIS REFUSAL TOO, with the replication the CLI prints
	// beside it — the same property the release-refusal path has, and one a
	// refusal moved earlier would silently drop.
	if len(report.Pools) != 2 || report.Pools[0].Size == 0 {
		t.Errorf("the refusal discarded what the check had established: %+v", report.Pools)
	}
}

// THE REMEDY MATCHES WHERE THE OVERRIDE LIVES.
//
// rbd reports whether an effective value came from the pool or from the cluster
// config, and the two take different commands: `ceph config rm client …` does not
// remove an override set on a pool. Naming the wrong one sends an operator to a
// setting that is already absent, and leaves them believing billet is wrong about
// their cluster.
func TestTheRemedyMatchesWhereTheOverrideLives(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ source, want, avoid string }{
		{"pool", "rbd config pool rm billet-images rbd_default_clone_format", "ceph config rm"},
		{"config", "ceph config rm client rbd_default_clone_format", "rbd config pool rm"},
	} {
		rec := answer()
		rec.cloneFormat = "1"
		rec.cloneSource = tc.source

		_, err := client(t, valid(), rec).CheckReachable(t.Context())
		if err == nil {
			t.Fatalf("source %q: CheckReachable accepted a forced clone format", tc.source)
		}

		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("source %q: the error does not give %q: %v", tc.source, tc.want, err)
		}

		if strings.Contains(err.Error(), tc.avoid) {
			t.Errorf("source %q: the error gives %q, which would not remove it: %v",
				tc.source, tc.avoid, err)
		}
	}
}

// OUTPUT THE ceph COMMANDS CANNOT BE PARSED FROM IS NOT ECHOED EITHER.
//
// The image listing has followed this rule since the package existed; the cluster
// reads were added later and did not. A wrong binary at that path prints whatever
// it prints, and the release query in particular would have stored it, failed the
// pre-mimic lookup, passed the check, and printed it unbounded on the terminal.
func TestUnparseableClusterOutputIsNotRendered(t *testing.T) {
	t.Parallel()

	const secretish = "AQDH631qo1CfBxAA9ZsjJleBiq9V2OqUCfIn9Q=="

	t.Run("the release query", func(t *testing.T) {
		t.Parallel()

		rec := answer()
		rec.minCompat = "key = " + secretish

		_, err := client(t, valid(), rec).CheckReachable(t.Context())
		if err == nil {
			t.Fatal("CheckReachable accepted output that is not a release name")
		}

		if strings.Contains(err.Error(), secretish) {
			t.Errorf("the error rendered the command's output: %v", err)
		}

		if !strings.Contains(err.Error(), "is it the ceph command") {
			t.Errorf("the error does not say what billet concluded: %v", err)
		}
	})

	t.Run("the clone format listing", func(t *testing.T) {
		t.Parallel()

		rec := answer()
		rec.rawConfig = "key = " + secretish

		_, err := client(t, valid(), rec).CheckReachable(t.Context())
		if err == nil {
			t.Fatal("CheckReachable accepted output that is not a configuration list")
		}

		if strings.Contains(err.Error(), secretish) {
			t.Errorf("the error rendered the command's output: %v", err)
		}
	})

	t.Run("a json null pool listing", func(t *testing.T) {
		t.Parallel()

		// `null` unmarshals into a slice happily and would be read as "the cluster
		// described no pools" — reported as replication unknown beside a green check.
		rec := answer()
		rec.pools = "null"

		if _, err := client(t, valid(), rec).CheckReachable(t.Context()); err == nil {
			t.Fatal("CheckReachable accepted a null pool listing as an empty one")
		}
	})
}

// A CLUSTER THAT WOULD CLONE THE OLD WAY IS REFUSED, NOT NOTED.
//
// On clone v1 a snapshot must be protected before it can be cloned, and a
// protected snapshot with a live clone can be neither unprotected nor removed —
// so a cache generation any running job holds a clone of is undeletable and
// eviction is blocked by ordinary traffic. Nothing in billet clones yet, which is
// exactly why this is the moment: the fix is one command on an empty cluster.
func TestAClusterThatClonesTheOldWayIsRefused(t *testing.T) {
	t.Parallel()

	rec := answer()
	rec.minCompat = "luminous"

	report, err := client(t, valid(), rec).CheckReachable(t.Context())
	if err == nil {
		t.Fatal("CheckReachable accepted a cluster that would clone the old way")
	}

	if !errors.Is(err, ErrCloneV1) {
		t.Errorf("the failure is not ErrCloneV1, so a caller cannot tell it from an unreachable "+
			"cluster: %v", err)
	}

	// The state it found and the command that fixes it. An operator told only
	// "refused" has to go and learn what require-min-compat-client is.
	for _, want := range []string{"luminous", "ceph osd set-require-min-compat-client mimic"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not carry %q: %v", want, err)
		}
	}

	// AND THE REPORT SURVIVES THE REFUSAL. What billet learned before the last
	// question — which pools exist, how they are replicated — is what an operator
	// needs beside the refusal, and returning a zero value here would throw it away.
	if report.User == "" || len(report.Pools) != 2 {
		t.Fatalf("the refusal discarded what the check had already established: %+v", report)
	}

	// INCLUDING THE REPLICATION, which is the half the CLI prints beside the
	// refusal. Moving the refusal above the pool read would leave the assertions
	// above green and quietly drop it.
	for _, p := range report.Pools {
		if p.Size == 0 || p.MinSize == 0 {
			t.Errorf("%s reached the refusal with no replication: %+v", p.Name, p)
		}
	}

	if report.CloneV2 {
		t.Error("the report claims clone v2 on a cluster that was refused for not having it")
	}

	// THE RELEASE THE CLUSTER NAMED, not a constant. It is what `billet check`
	// prints beside the refusal, and hard-coding it would leave an operator
	// comparing their cluster against a value billet made up.
	if report.MinCompatClient != "luminous" {
		t.Errorf("the report names %q rather than what the cluster answered", report.MinCompatClient)
	}
}

// WHAT THE OPERATOR CHOSE IS REPORTED, because it is invisible from the config.
//
// billet does not refuse a pool that keeps one copy — how many copies to keep is
// the operator's decision and billet has no standing to overrule it — but an
// operator who believes their golden images are mirrored should find out here
// rather than after a drive dies.
func TestThePoolReplicationIsReported(t *testing.T) {
	t.Parallel()

	rec := answer()
	rec.pools = `[{"pool_name":"billet-images","size":1,"min_size":1},
	              {"pool_name":"billet-cache","size":3,"min_size":2},
	              {"pool_name":".mgr","size":2,"min_size":1}]`

	report, err := client(t, valid(), rec).CheckReachable(t.Context())
	if err != nil {
		t.Fatalf("CheckReachable: %v", err)
	}

	if got := report.Pools[0]; got.Size != 1 || got.MinSize != 1 {
		t.Errorf("image pool = %+v, want the size the cluster reported", got)
	}

	if got := report.Pools[1]; got.Size != 3 || got.MinSize != 2 {
		t.Errorf("cache pool = %+v, want the size the cluster reported", got)
	}

	// THE PURPOSE TOO, because it is printed beside the pool and swapping the two
	// strings would tell an operator their golden images are cache volumes — a
	// mutation nothing else here would notice.
	if !strings.Contains(report.Pools[0].Purpose, "golden images") {
		t.Errorf("the image pool's purpose is %q", report.Pools[0].Purpose)
	}

	if !strings.Contains(report.Pools[1].Purpose, "cache") {
		t.Errorf("the cache pool's purpose is %q", report.Pools[1].Purpose)
	}

	// A pool billet does not use must not be attributed to one it does.
	for _, p := range report.Pools {
		if p.Name == ".mgr" {
			t.Error("a pool billet was not pointed at is in the report")
		}
	}
}

// A CLUSTER THAT DID NOT SAY IS NOT GUESSED AT.
//
// The pool listing is a separate call from the image listing, and a cluster that
// answers one and not the other is a real state — a pool created between them, or
// caps that permit `rbd ls` and not `osd pool ls`. Reporting "size 0" would read
// as a pool with no copies; reporting nothing reads as what it is.
func TestAPoolTheClusterDidNotDescribeIsNotInvented(t *testing.T) {
	t.Parallel()

	rec := answer()
	rec.pools = `[{"pool_name":"something-else","size":2,"min_size":1}]`

	report, err := client(t, valid(), rec).CheckReachable(t.Context())
	if err != nil {
		t.Fatalf("CheckReachable: %v", err)
	}

	for _, p := range report.Pools {
		if p.Size != 0 || p.MinSize != 0 {
			t.Errorf("%s was given a replication the cluster never reported: %+v", p.Name, p)
		}
	}
}

// EACH BINARY IS LOOKED UP ON ITS OWN, so a host with one and not the other is
// refused rather than failing later.
//
// Every other missing-client test empties PATH, which fails on `rbd` first and
// proves nothing about the second lookup — deleting it entirely leaves them all
// green. This puts a real rbd on PATH and no ceph.
func TestAMissingCephIsRefusedEvenWhenRBDIsPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rbd"), []byte("#!/bin/sh\necho '[]'\n"), 0o700); err != nil {
		t.Fatalf("write the rbd stub: %v", err)
	}

	t.Setenv("PATH", dir)

	c, err := New(valid())
	if err == nil {
		t.Fatal("New succeeded with rbd present and ceph missing")
	}

	if c != nil {
		t.Error("New returned a client alongside an error")
	}

	if !errors.Is(err, ErrNoClient) {
		t.Errorf("the failure is not ErrNoClient: %v", err)
	}

	if !strings.Contains(err.Error(), "ceph") {
		t.Errorf("the error does not name the command that is missing: %v", err)
	}
}

// THE ceph CALLS ARE BOUNDED TOO, and the rbd ones cannot prove it.
//
// TestAnInvocationIsBounded stalls the first invocation, which is always an rbd
// list — so removing the timeout from the ceph path alone leaves it green while
// `billet check` can hang on a cluster that answers `rbd` and not `ceph`, which is
// the one thing the command must never do.
func TestACephInvocationIsBoundedToo(t *testing.T) {
	t.Parallel()

	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		WithTimeout(20*time.Millisecond),
		withRunner(func(ctx context.Context, bin string, args []string) ([]byte, error) {
			// The rbd half answers immediately, and so does the release query — so
			// what stalls is the JSON pool listing specifically. Stalling the FIRST
			// ceph call would leave "apply the timeout only to non-json ceph calls"
			// alive, which is a real one-line regression.
			if filepath.Base(bin) != "ceph" {
				return []byte(`[]`), nil
			}

			if strings.Contains(strings.Join(args, " "), "get-require-min-compat-client") {
				return []byte("mimic\n"), nil
			}

			safety := time.NewTimer(5 * time.Second)
			defer safety.Stop()

			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("ceph: %w", ctx.Err())
			case <-safety.C:
				return nil, errors.New("nothing bounded the ceph invocation")
			}
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	start := time.Now()

	if _, err := c.CheckReachable(t.Context()); err == nil {
		t.Fatal("CheckReachable waited out a cluster that never answered and reported success")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the failure is not the deadline: %v", err)
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the configured timeout did not apply to the ceph call; it took %s", elapsed)
	}
}

// THE CLONE FORMAT IS PER POOL, and both of these hold clones.
//
// Measured: `rbd config pool set billet-cache rbd_default_clone_format 1` leaves
// the image pool reporting `auto` while a clone of an unprotected snapshot in the
// cache pool fails with `(22) Invalid argument`. Reading one pool and calling it
// the cluster's answer is the same proxy mistake the round-one fix was about, one
// level down — and the cache pool is the one cache generations live in, so it is
// the pool where an undeletable generation actually costs something.
func TestTheCloneFormatIsCheckedOnEveryPool(t *testing.T) {
	t.Parallel()

	rec := answer()
	rec.cloneFormat = "auto" // the image pool is fine
	rec.cacheCloneFormat = "1"
	rec.cloneSource = "pool" // which is how a per-pool override reports itself

	report, err := client(t, valid(), rec).CheckReachable(t.Context())
	if err == nil {
		t.Fatal("CheckReachable accepted a cluster whose CACHE pool is forced to clone v1")
	}

	if !errors.Is(err, ErrCloneV1) {
		t.Errorf("the failure is not ErrCloneV1: %v", err)
	}

	// WHICH POOL, because the remedy is per pool and naming the wrong one sends an
	// operator to a setting that is already correct.
	if !strings.Contains(err.Error(), "billet-cache") {
		t.Errorf("the error does not name the pool that is forced: %v", err)
	}

	if !strings.Contains(err.Error(), "rbd config pool rm billet-cache") {
		t.Errorf("the error does not give the per-pool remedy: %v", err)
	}

	// The report still carries what each pool said, so the CLI can show it.
	if report.Pools[0].CloneFormat != "auto" || report.Pools[1].CloneFormat != "1" {
		t.Errorf("the per-pool clone formats were not reported: %+v", report.Pools)
	}
}

// A VALUE IS NORMALISED ONCE, AND EVERY CONSUMER SEES THE SAME ONE.
//
// This is the fourth time in this one check that a value was normalised for a
// DECISION while a consumer acted on the raw one. `"\n 1 \r"` resolved to clone
// v1 correctly, then failed a `== "1"` comparison — so billet refused the cluster
// and recommended raising a floor that was already high enough, and printed the
// raw bytes on the terminal. The fix that generalises is not another TrimSpace at
// the comparison; it is having one value.
func TestACloneFormatIsCanonicalisedOnce(t *testing.T) {
	t.Parallel()

	rec := answer()
	// BOTH FIELDS PADDED, because both are canonicalised and a fixture that pads
	// only the value leaves the source's trim untested.
	rec.rawConfig = "[{\"name\":\"rbd_default_clone_format\",\"value\":\"\\n  1  \\r\"," +
		"\"source\":\"  pool\\n\"}]"

	report, err := client(t, valid(), rec).CheckReachable(t.Context())
	if err == nil {
		t.Fatal("CheckReachable accepted a padded clone format of 1")
	}

	// The remedy must be the one for a forced format, not for the floor — which is
	// already high enough, and is what a raw comparison would have sent them to.
	// Its presence also proves the SOURCE was canonicalised: a padded source falls
	// through to the unrecognised branch, which gives a different sentence.
	if !strings.Contains(err.Error(), "rbd config pool rm") {
		t.Errorf("the error does not give the remedy for the forced format: %v", err)
	}

	// And the stored value is the canonical one, so nothing downstream — including
	// what the CLI prints — carries the padding.
	for _, p := range report.Pools {
		if p.CloneFormat != "1" {
			t.Errorf("%s stored the raw value %q rather than the canonical one", p.Name, p.CloneFormat)
		}
	}
}

// A POOL NAME IS NOT A WORD, and every remedy billet prints is something an
// operator will paste.
//
// Ceph creates pools called `a b` and `a/b` — measured, which is why billet's own
// validation stopped refusing interior whitespace — so concatenating one into a
// suggested command yields `rbd config pool rm billet cache …`, which addresses
// two arguments and neither of them the pool.
func TestAPoolNameInARemedyIsShellQuoted(t *testing.T) {
	t.Parallel()

	cfg := valid()
	cfg.ImagePool = "billet images"

	rec := answer()
	rec.cloneFormat = "1"
	rec.cloneSource = "pool"
	rec.rawImages = `[]`

	c, err := New(cfg, WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		withRunner(rec.run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.CheckReachable(t.Context())
	if err == nil {
		t.Fatal("CheckReachable accepted a forced clone format")
	}

	if !strings.Contains(err.Error(), `'billet images'`) {
		t.Errorf("the remedy does not quote a pool name containing a space, so pasting it would "+
			"address something else: %v", err)
	}
}

// EVERY CAUSE IS NAMED, because they are independent and fixing one can leave
// another. Both pools forced, plus a floor that predates mimic, is three reasons
// and three commands.
func TestEveryReasonTheClusterClonesTheOldWayIsNamed(t *testing.T) {
	t.Parallel()

	rec := answer()
	rec.minCompat = "luminous"
	rec.cloneFormat = "1"
	rec.cacheCloneFormat = "1"
	rec.cloneSource = "pool"

	_, err := client(t, valid(), rec).CheckReachable(t.Context())
	if err == nil {
		t.Fatal("CheckReachable accepted a cluster with three reasons to clone the old way")
	}

	for _, want := range []string{
		"rbd config pool rm billet-images",
		"rbd config pool rm billet-cache",
		"set-require-min-compat-client",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not carry %q, so fixing what it names leaves the cluster "+
				"refused: %v", want, err)
		}
	}
}

// A SOURCE BILLET DOES NOT RECOGNISE IS SAID, NOT GUESSED AT.
//
// Treating every non-`pool` source as cluster-wide hands the operator a command
// billet has no reason to believe removes anything. A command that changes
// nothing is worse than an honest "look here": they run it, see no change, and
// conclude billet is wrong about their cluster.
func TestAnUnrecognisedOverrideSourceIsNotGuessedAt(t *testing.T) {
	t.Parallel()

	rec := answer()
	rec.cloneFormat = "1"
	rec.cloneSource = "somewhere-else"

	_, err := client(t, valid(), rec).CheckReachable(t.Context())
	if err == nil {
		t.Fatal("CheckReachable accepted a forced clone format")
	}

	if strings.Contains(err.Error(), "ceph config rm client") {
		t.Errorf("an unrecognised source was reported as cluster-wide: %v", err)
	}

	if !strings.Contains(err.Error(), "somewhere-else") {
		t.Errorf("the error does not say what the cluster called the source: %v", err)
	}
}

// THE RENDERED LENGTH IS WHAT REACHES A TERMINAL, and quoting expands.
//
// Capping the input leaves the output four times the bound it was supposed to
// have: 300 NUL bytes render as 1,200 characters of `\x00`. The all-`x` fixture
// that came with the cap could not see this, because ascii quotes one-for-one.
func TestARenderedValueIsBoundedAfterQuoting(t *testing.T) {
	t.Parallel()

	for _, v := range []string{
		strings.Repeat("\x00", 4096),
		strings.Repeat("\t", 4096),
		strings.Repeat("x", 4096),
		strings.Repeat("é", 4096),

		// THE WINDOW WHERE THE TWO BOUNDS DIFFER, which is the only place the
		// mutation is visible: 100 NUL bytes are well under the cap as INPUT and
		// four times over it once quoted. Fixtures that are all far above the cap
		// truncate identically either way.
		strings.Repeat("\x00", 100),
		strings.Repeat("\t", 120),
	} {
		if got := bounded(v); len(got) > maxDiagnostic+8 {
			t.Errorf("bounded rendered %d bytes for a %d-byte value", len(got), len(v))
		}
	}

	// AND A SHORT VALUE IS STILL QUOTED, so a control byte cannot become a live
	// terminal control on its way through an error message.
	if got := bounded("a\nb"); strings.Contains(got, "\n") {
		t.Errorf("bounded passed a raw newline through: %q", got)
	}
}

// A TRUNCATED RENDERING IS STILL A VALID QUOTED VALUE.
//
// Length and "no raw newline" do not establish that: slicing the finished quoted
// string cuts inside an escape — 100 NUL bytes truncate to `"\x00\x00…\x0` — and
// trimming a trailing backslash only repairs the subset of cuts that landed on
// one. What proves it is unquoting the result, which fails on a partial escape.
func TestATruncatedRenderingIsStillValidlyQuoted(t *testing.T) {
	t.Parallel()

	for _, v := range []string{
		strings.Repeat("\x00", 100),
		strings.Repeat("\x00", 4096),
		strings.Repeat("\t", 120),
		strings.Repeat("a", 296) + "\x00\x00",
		strings.Repeat("a", 297) + "\x00\x00",
		strings.Repeat("a", 298) + "\x00\x00",
		strings.Repeat(`\`, 200),
		strings.Repeat("é", 4096),
		strings.Repeat("☃", 4096),
		strings.Repeat("a\nb", 200),
		strings.Repeat("\x1b[31m", 200), // an ansi escape, which a terminal would obey
	} {
		got := bounded(v)

		// The ellipsis marks the truncation and is not part of the value, so it
		// comes off before the result is unquoted.
		candidate := got
		if strings.HasSuffix(got, "…\"") {
			candidate = strings.TrimSuffix(got, "…\"") + `"`
		}

		if _, err := strconv.Unquote(candidate); err != nil {
			t.Errorf("bounded(%d bytes) is not a valid quoted value (%v): %q",
				len(v), err, got[max(0, len(got)-24):])
		}

		// AND NO CONTROL BYTE SURVIVES THE TRUNCATING PATH. Unquoting alone does not
		// establish it — Go's unquote tolerates a raw NUL or tab inside a literal —
		// so a truncation that stopped escaping would still parse. What must not
		// happen is a byte from another program reaching a terminal as a live
		// control, which is what the quoting is for.
		for _, r := range got {
			if r < 0x20 || r == 0x7f {
				t.Errorf("bounded(%d bytes) passed a raw control byte %q through", len(v), r)

				break
			}
		}
	}
}

// A REFUSAL ALWAYS NAMES A REASON.
//
// The cause list re-derives what EffectiveCloneFormat already decided, and two
// functions applying one rule is how they come to disagree. If they ever do, the
// operator must get a sentence rather than a colon with nothing after it — so
// this drives every combination that refuses and asserts the message says
// something.
func TestARefusalAlwaysNamesAReason(t *testing.T) {
	t.Parallel()

	for _, release := range []string{"luminous", "unknown", "mimic", "tentacle"} {
		for _, image := range []string{"auto", "1", "2"} {
			for _, cache := range []string{"auto", "1", "2"} {
				rec := answer()
				rec.minCompat = release
				rec.cloneFormat = image
				rec.cacheCloneFormat = cache

				_, err := client(t, valid(), rec).CheckReachable(t.Context())
				if err == nil || !errors.Is(err, ErrCloneV1) {
					continue
				}

				// Everything after the sentinel's own sentence is the reason.
				reason := strings.TrimPrefix(err.Error(), ErrCloneV1.Error())
				if len(strings.TrimSpace(strings.TrimPrefix(reason, ":"))) < 20 {
					t.Errorf("release %q, pools %q/%q: refused with no reason: %v",
						release, image, cache, err)
				}
			}
		}
	}
}

// A TRUNCATED RENDERING IS STILL TERMINATED.
//
// The cut can land inside an escape — `"aaa\x0` — and an odd number of trailing
// backslashes escapes the quote billet appends, so the value reads as though it
// were never closed. Probed rather than reasoned: two of five sample cuts
// produced one.
func TestATruncatedRenderingDoesNotEscapeItsOwnQuote(t *testing.T) {
	t.Parallel()

	for _, v := range []string{
		strings.Repeat("a", 296) + "\x00\x00",
		strings.Repeat("a", 297) + "\x00\x00",
		strings.Repeat("a", 298) + "\x00\x00",
		strings.Repeat(`\`, 200),
		strings.Repeat("\x00", 100),
	} {
		got := bounded(v)

		body := strings.TrimSuffix(got, "…\"")

		trailing := 0
		for i := len(body) - 1; i >= 0 && body[i] == '\\'; i-- {
			trailing++
		}

		if trailing%2 == 1 {
			t.Errorf("bounded(%d bytes) ends in an odd run of backslashes, so the closing quote "+
				"is escaped: %q", len(v), got[len(got)-16:])
		}
	}
}
