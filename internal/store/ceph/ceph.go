// Package ceph reaches the RBD pools a site keeps.
//
// billet drives Ceph through the `rbd` COMMAND rather than through librados, and
// that is a decision rather than an omission. The Go binding is cgo over
// librados, which would end the single static binary and the cross-build matrix
// in one move — the same reason `mattn/go-sqlite3` is banned here — and billet
// already treats Ceph the way it treats Docker and Tart: an external dependency
// an operator installs. What it costs is that every call is a process, so this
// package is for operations measured in tens per job, never per block.
//
// Nothing here mounts anything yet. #23 built the cluster and the configuration
// that names it; the Store interface, the commit protocol and eviction are #25.
package ceph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// DefaultTimeout bounds one rbd invocation.
//
// A CLUSTER THAT IS NOT THERE DOES NOT REFUSE, IT WAITS. librados retries a
// monitor it cannot reach for minutes, so an unreachable cluster with no bound
// here is `billet check` hanging with no output rather than telling an operator
// which pool it could not read.
const DefaultTimeout = 15 * time.Second

// Client runs rbd against one site's pools.
type Client struct {
	cfg  config.CephConfig
	bin  string
	run  runner
	wait time.Duration
}

// runner executes one rbd invocation. A seam, so a test can assert the ARGUMENTS
// billet builds — which is where the mistakes are — without a cluster.
type runner func(ctx context.Context, bin string, args []string) ([]byte, error)

// Option configures a Client.
type Option func(*Client)

// withRunner replaces process execution. Unexported because its parameter is:
// an exported option nothing outside this package can construct is a worse API
// than one that is honestly package-private.
func withRunner(r runner) Option {
	return func(c *Client) {
		if r != nil {
			c.run = r
		}
	}
}

// WithTimeout bounds one invocation.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.wait = d
		}
	}
}

// WithBinary names the rbd executable, skipping the PATH lookup.
func WithBinary(path string) Option {
	return func(c *Client) {
		if strings.TrimSpace(path) != "" {
			c.bin = strings.TrimSpace(path)
		}
	}
}

// ErrNoRBD is returned when the rbd command is not installed.
var ErrNoRBD = errors.New("the rbd command is not on PATH")

// New returns a client for the pools this configuration names.
//
// IT RE-APPLIES config.CheckCeph, because this constructor is exported and cannot
// assume its configuration came through config.Load. Two of those rules are load
// bearing HERE and not only there. A pool name is half of the POSITIONAL
// `pool/image` specs billet builds, where rbd reads a leading dash as an option it
// does not recognise — measured, and NOT true of `-p`, which consumes whatever
// token follows it. And an identity of `admin` would hand this process a key that
// can delete the pools it is only meant to read.
func New(cfg config.CephConfig, opts ...Option) (*Client, error) {
	if errs := config.CheckCeph(cfg); len(errs) > 0 {
		return nil, fmt.Errorf("ceph: %w", errors.Join(errs...))
	}

	c := &Client{cfg: cfg, run: execRunner, wait: DefaultTimeout}
	for _, opt := range opts {
		opt(c)
	}

	if c.bin == "" {
		bin, err := exec.LookPath("rbd")
		if err != nil {
			return nil, fmt.Errorf("%w: billet drives ceph through it rather than linking librados, "+
				"so a node needs the ceph client package installed (ceph-common on debian and "+
				"ubuntu): %w", ErrNoRBD, err)
		}

		c.bin = bin
	}

	return c, nil
}

// Report is what a reachability check learned.
type Report struct {
	// User is the identity that answered, without the `client.` prefix.
	User string
	// ImagePool and CachePool are how many images each pool holds. A count rather
	// than the names: this goes on an operator's terminal, and a site with a
	// thousand cache volumes should not print a thousand lines.
	ImagePool int
	CachePool int
}

// CheckReachable proves this host can act on its ceph configuration.
//
// THE SAME DISTINCTION checkEC2Credentials MAKES, one backend over. Config
// validation proves the block is coherent, and coherence is not what an operator
// running `billet check` is asking: a node whose keyring is missing, whose monitor
// is unreachable, or whose pool was never created validates perfectly and then
// fails on the first job of the day, with a librados error that names none of
// those.
//
// IT IS A READ, and the caller must say so. Listing a pool proves the monitors
// answered, the keyring authenticated and the pool exists; it proves nothing about
// permission to CREATE, clone or remove an image, which is what a launch actually
// does.
func (c *Client) CheckReachable(ctx context.Context) (Report, error) {
	images, err := c.list(ctx, c.cfg.ImagePool)
	if err != nil {
		return Report{}, err
	}

	caches, err := c.list(ctx, c.cfg.CachePool)
	if err != nil {
		return Report{}, err
	}

	return Report{User: c.cfg.User, ImagePool: images, CachePool: caches}, nil
}

// list counts the images in one pool.
func (c *Client) list(ctx context.Context, pool string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, c.wait)
	defer cancel()

	out, err := c.run(ctx, c.bin, c.args(pool, "ls"))
	if err != nil {
		return 0, fmt.Errorf("ceph: pool %q could not be listed as client.%s: %w", pool, c.cfg.User, err)
	}

	var names []string
	if err := json.Unmarshal(out, &names); err != nil {
		// The OUTPUT IS NOT RENDERED. rbd writes its diagnostics to stderr and its
		// data to stdout, so unparseable stdout means billet is talking to something
		// that is not the rbd it expects, and echoing whatever that was onto a
		// terminal is how an unrelated program's output becomes billet's error.
		return 0, fmt.Errorf("ceph: %s did not answer with a json image list for pool %q; is it "+
			"the rbd command?", c.bin, pool)
	}

	return len(names), nil
}

// args builds one rbd invocation.
//
// EVERY OPTIONAL PATH IS OMITTED WHEN EMPTY rather than passed as "". Ceph's own
// search path is what finds /etc/ceph/ceph.conf and the matching keyring, and
// passing an empty --conf overrides that search with a file that does not exist.
func (c *Client) args(pool string, command ...string) []string {
	args := []string{"--id", c.cfg.User}

	if c.cfg.ConfPath != "" {
		args = append(args, "--conf", c.cfg.ConfPath)
	}

	if c.cfg.KeyringPath != "" {
		args = append(args, "--keyring", c.cfg.KeyringPath)
	}

	args = append(args, "--format", "json", "-p", pool)

	return append(args, command...)
}

// waitDelay bounds how long the pipes may outlive the process.
//
// KILLING THE PROCESS IS NOT THE SAME AS THE CALL RETURNING. exec.CommandContext
// kills the direct child when the deadline passes, but Output reads through pipes
// and Wait blocks until they reach EOF — so a descendant holding the write end
// keeps the call open after the process billet started is already dead. WaitDelay
// closes them anyway; without it "this call is bounded" is true of the process and
// false of the function.
const waitDelay = 2 * time.Second

// execRunner runs rbd and returns its standard output.
//
// STDERR IS DROPPED INTO THE ERROR, not into the result. librados logs to stderr
// on every connection — a successful listing is routinely accompanied by warnings
// — so folding the two together makes valid JSON unparseable.
func execRunner(ctx context.Context, bin string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = waitDelay

	// BOUNDED AS IT ARRIVES, not capped afterwards. A strings.Builder here would
	// bound the TIME an invocation may take and not the MEMORY, so a wrong or broken
	// executable could allocate for the whole timeout. Note that no test can observe
	// the difference — lastLine caps the rendered result either way — so this
	// wiring is covered by review rather than by an assertion.
	stderr := &tailWriter{limit: maxDiagnostic}
	cmd.Stderr = stderr

	// STDOUT IS NOT BOUNDED, and that is a decision. Its size is proportional to how
	// many images the pool holds — billet's own data — so a cap would turn a large
	// site into a failure, and truncating a JSON array yields something that does
	// not parse rather than something partial. What is unbounded is therefore the
	// memory a WRONG executable at this path could make billet allocate inside one
	// timeout, which is a narrower risk than it first reads: c.bin is either what
	// exec.LookPath found for "rbd" or a path a caller passed deliberately.
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}

	// WHAT THE PROCESS SAID WINS OVER WHAT THE CLOCK SAYS, and the order here is
	// the whole point. A process the context killed comes back as `signal: killed`,
	// which has no exit code — but a process that EXITED on its own has one, and
	// the context can still expire afterwards while WaitDelay drains a pipe a
	// descendant is holding. Consulting the clock first would replace `(2) No such
	// file or directory` with a timeout, throwing away the sentence this whole
	// function exists to carry.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
		if msg := stderr.String(); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, lastLine(msg))
		}

		return nil, err
	}

	// THE SUCCESSFUL HALF OF THE SAME RACE. A process that exits ZERO with a
	// descendant still holding the pipe never produces an ExitError at all — Wait
	// returns ErrWaitDelay — so the branch above cannot see it, and it would fall
	// through to be reported as a timeout with its output discarded. rbd had
	// already written the listing and said it was fine; the only thing that went
	// wrong is that something else kept a file descriptor. If the bytes are short,
	// the caller's json.Unmarshal says so in a sentence naming the pool.
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0 {
		return out, nil
	}

	// THE DEADLINE IS REPORTED AS THE DEADLINE. Without this a caller asking
	// errors.Is(err, context.DeadlineExceeded) — the only way to tell "the cluster
	// never answered" from "rbd said no" — gets false for the one condition it
	// exists to name.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("%s did not answer: %w", filepath.Base(bin), ctxErr)
	}

	if msg := stderr.String(); msg != "" {
		return nil, fmt.Errorf("%w: %s", err, lastLine(msg))
	}

	return nil, err
}

// tailWriter keeps the last limit bytes written to it and discards the rest.
//
// STDERR IS ANOTHER PROGRAM'S OUTPUT AND ITS SIZE IS ITS OWN CHOICE. Collecting it
// into a strings.Builder bounds the time an invocation may take and not the memory
// it may take, so a wrong or broken executable can allocate for the whole timeout.
// Keeping the TAIL rather than the head is what makes the bound compatible with
// lastLine: librados prints its connection logging first and the sentence that
// matters last.
type tailWriter struct {
	limit int
	buf   []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	n := len(p)

	// A LIMIT OF ZERO KEEPS NOTHING rather than panicking. Nothing constructs one
	// today, but the slice arithmetic below indexes from the end and a negative
	// limit would be a runtime panic in a control plane — a guard is cheaper than
	// the next caller having to know that.
	if w.limit <= 0 {
		w.buf = nil

		return n, nil
	}

	// TWO TRIMS, AND THE FIRST ONE IS NOT REDUNDANT even though deleting it leaves
	// every observable behaviour identical — which is why its mutation survives, and
	// the redundancy is said out loud here rather than assumed. It bounds the PEAK
	// allocation of a single write: without it a lone 10GB write is appended in full
	// and only then trimmed, which is the allocation this type exists to prevent.
	// The second trim bounds what ACCUMULATES across writes, which the first cannot.
	if len(p) > w.limit {
		p = p[len(p)-w.limit:]
	}

	w.buf = append(w.buf, p...)
	if len(w.buf) > w.limit {
		w.buf = w.buf[len(w.buf)-w.limit:]
	}

	// The length WRITTEN TO US, never the length kept: io.Writer's contract makes a
	// short count an error, so reporting what survived the trim would make exec
	// conclude the pipe broke and abandon a healthy command.
	return n, nil
}

func (w *tailWriter) String() string { return strings.TrimSpace(string(w.buf)) }

// maxDiagnostic bounds how much of rbd's output reaches a terminal or a log.
const maxDiagnostic = 300

// lastLine keeps the line rbd ended on, bounded.
//
// librados prints a paragraph of connection logging in front of the sentence that
// says what went wrong, and that last line is the one an operator can act on:
// `rbd: listing images failed: (2) No such file or directory` is the difference
// between a mistyped pool and `exit status 2`. Dropping it costs most of what the
// preflight is for.
//
// RENDERING ANOTHER PROGRAM'S OUTPUT IS A CREDENTIAL RISK, so what makes it
// admissible is a measurement rather than an argument. Probed against Ceph 20.2.3:
// an unparseable keyring, a syntactically valid keyring holding the wrong key, and
// a corrupt ceph.conf each produce a structural diagnostic, and none of them
// echoes the file's contents. The residual is that this is one version's behaviour
// — which is why only the final line survives, and why it is capped.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	line := strings.TrimSpace(lines[len(lines)-1])

	if len(line) > maxDiagnostic {
		return sanitize(line[:maxDiagnostic]) + "…"
	}

	// UNCONDITIONALLY, NOT ONLY WHEN THIS FUNCTION TRUNCATES. tailWriter has
	// already cut the tail to exactly maxDiagnostic bytes by the time this runs, so
	// the branch above is FALSE on the real path and a repair that lived only there
	// would never execute — the input arrives already split mid-rune. Found by
	// review after the first fix, which was correct and unreachable.
	return sanitize(line)
}

// sanitize drops the bytes that are not text.
//
// rbd's diagnostics are not guaranteed to be ASCII — a pool name is whatever the
// operator typed — and a byte-level truncation anywhere upstream can leave half a
// rune, which then reaches an error string that is printed, logged and marshalled.
// Dropping is right rather than replacing: the replacement character would be
// billet adding a glyph to somebody else's message.
func sanitize(s string) string { return strings.ToValidUTF8(s, "") }
