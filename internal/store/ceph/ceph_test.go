package ceph

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
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

// recorder captures the invocations billet builds and answers each with a
// canned result. Keyed by call order, because the two pools are listed in a
// fixed order and a check that lost one would otherwise look identical.
type recorder struct {
	calls [][]string
	out   [][]byte
	err   error
}

func (r *recorder) run(_ context.Context, _ string, args []string) ([]byte, error) {
	r.calls = append(r.calls, args)
	if r.err != nil {
		return nil, r.err
	}

	if len(r.out) == 0 {
		return []byte(`[]`), nil
	}

	out := r.out[0]
	if len(r.out) > 1 {
		r.out = r.out[1:]
	}

	return out, nil
}

func client(t *testing.T, cfg config.CephConfig, rec *recorder) *Client {
	t.Helper()

	c, err := New(cfg, WithBinary("/usr/bin/rbd"), withRunner(rec.run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return c
}

// THE ARGUMENTS ARE THE PART WITH MISTAKES IN IT.
//
// Every one of these is a decision: --id is what stops rbd choosing client.admin
// for itself, --format json is what makes the output parseable rather than
// scraped, and -p is what confines the call to the pool this identity was granted.
func TestTheInvocationNamesTheIdentityAndThePool(t *testing.T) {
	t.Parallel()

	rec := &recorder{out: [][]byte{[]byte(`["ubuntu-2404-x64"]`), []byte(`["job-1","job-2"]`)}}

	report, err := client(t, valid(), rec).CheckReachable(t.Context())
	if err != nil {
		t.Fatalf("CheckReachable: %v", err)
	}

	if len(rec.calls) != 2 {
		t.Fatalf("rbd was invoked %d times, want one per pool", len(rec.calls))
	}

	// THE EXACT ARGV, not a joined substring match. Joining loses token
	// boundaries, so a client that built one argument reading
	// "--id billet --format json -p billet-images ls" would satisfy every
	// Contains check and produce an invocation rbd cannot parse.
	for i, want := range [][]string{
		{"--id", "site-reader", "--format", "json", "-p", "billet-images", "ls"},
		{"--id", "site-reader", "--format", "json", "-p", "billet-cache", "ls"},
	} {
		if !slices.Equal(rec.calls[i], want) {
			t.Errorf("call %d = %q, want %q", i, rec.calls[i], want)
		}
	}

	// The counts are the point of the report, and a check that returned zeroes
	// alongside a nil error would pass every other assertion here.
	if report.ImagePool != 1 || report.CachePool != 2 {
		t.Errorf("report = %+v, want 1 image and 2 caches", report)
	}

	if report.User != "site-reader" {
		t.Errorf("report names %q rather than the identity that answered", report.User)
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

	rec := &recorder{}
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

	rec := &recorder{}
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
		if !slices.Equal(rec.calls[i], want) {
			t.Errorf("call %d = %q, want %q", i, rec.calls[i], want)
		}
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

			c, err := New(cfg, WithBinary("/usr/bin/rbd"))
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

	rec := &recorder{err: errors.New("exit status 1: rbd: listing images failed: (1) Operation not permitted")}

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

	rec := &recorder{out: [][]byte{[]byte("key = " + secretish)}}

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
	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithTimeout(20*time.Millisecond),
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

	if !errors.Is(err, ErrNoRBD) {
		t.Errorf("the error is not ErrNoRBD, so a caller cannot tell it from an unreachable "+
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

	var ran string

	// NOT os.Args[0], not absolute, and with no separator: a WithBinary that
	// honoured only the test's own executable, only an absolute path, or only
	// something that looks like a path would pass in turn — and the option's whole
	// job is naming an rbd that is not the one on PATH.
	const elsewhere = "rbd-from-the-vendor"

	c, err := New(valid(), WithBinary(elsewhere),
		withRunner(func(_ context.Context, bin string, _ []string) ([]byte, error) {
			ran = bin

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
	if ran != elsewhere {
		t.Errorf("ran %q, want the binary the option named (%q)", ran, elsewhere)
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
