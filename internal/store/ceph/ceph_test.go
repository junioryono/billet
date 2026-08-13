package ceph

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// valid is a storage block that passes config.CheckCeph, so a case can change
// exactly the field it is about.
func valid() config.CephConfig {
	return config.CephConfig{
		User:      "billet",
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

	for i, want := range []string{"billet-images", "billet-cache"} {
		args := strings.Join(rec.calls[i], " ")

		for _, must := range []string{"--id billet", "--format json", "-p " + want, "ls"} {
			if !strings.Contains(args, must) {
				t.Errorf("call %d is missing %q: %s", i, must, args)
			}
		}
	}

	// The counts are the point of the report, and a check that returned zeroes
	// alongside a nil error would pass every other assertion here.
	if report.ImagePool != 1 || report.CachePool != 2 {
		t.Errorf("report = %+v, want 1 image and 2 caches", report)
	}

	if report.User != "billet" {
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

	args := strings.Join(rec.calls[0], " ")
	for _, want := range []string{"--conf /etc/billet/ceph.conf", "--keyring /etc/billet/billet.keyring"} {
		if !strings.Contains(args, want) {
			t.Errorf("the invocation does not carry %q: %s", want, args)
		}
	}
}

// THE CONSTRUCTOR RE-APPLIES THE CONFIG RULES, because it is exported and cannot
// assume its argument came through config.Load.
//
// Two of them are load-bearing in this package specifically. A pool name is the
// value of `-p`, so one beginning with a dash is read by rbd as a flag — billet
// builds an argv rather than a shell command, and nothing in exec quotes a value
// out of being an option. And `admin` would put a key that can delete a pool in
// the hands of a process whose whole job is reading two of them.
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
			name:   "a pool rbd would read as a flag",
			mutate: func(c *config.CephConfig) { c.ImagePool = "-p" },
			want:   "as a flag",
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

	for _, want := range []string{"billet-images", "client.billet", "Operation not permitted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not carry %q: %v", want, err)
		}
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
	if _, err := c.CheckReachable(t.Context()); err == nil {
		t.Fatal("CheckReachable waited out an unreachable cluster and reported success")
	} else if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
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
