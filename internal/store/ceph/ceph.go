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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
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

// Client runs the ceph client commands against one site's pools.
//
// TWO BINARIES, ONE PACKAGE. `rbd` addresses images and `ceph` answers for the
// cluster itself, and ceph-common ships both — so a host that has one has the
// other, and a missing binary is one diagnostic rather than two.
type Client struct {
	cfg  config.CephConfig
	bin  string // rbd
	ceph string
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

// WithCephBinary names the ceph executable, skipping the PATH lookup.
func WithCephBinary(path string) Option {
	return func(c *Client) {
		if strings.TrimSpace(path) != "" {
			c.ceph = strings.TrimSpace(path)
		}
	}
}

// ErrNoClient is returned when the ceph client commands are not installed.
var ErrNoClient = errors.New("the ceph client commands are not on PATH")

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

	for _, want := range []struct {
		name string
		into *string
	}{{"rbd", &c.bin}, {"ceph", &c.ceph}} {
		if *want.into != "" {
			continue
		}

		bin, err := exec.LookPath(want.name)
		if err != nil {
			return nil, fmt.Errorf("%w: billet drives ceph through them rather than linking "+
				"librados, so a node needs the ceph client package installed (ceph-common on "+
				"debian and ubuntu): %w", ErrNoClient, err)
		}

		*want.into = bin
	}

	return c, nil
}

// Report is what a reachability check learned.
type Report struct {
	// User is the identity the invocations authenticated as, without the `client.`
	// prefix. It comes from the configuration rather than from the cluster — Ceph
	// does not echo it back — so it says who billet asked AS, not who answered.
	User string
	// MinCompatClient is the oldest client release the cluster admits. It is one
	// half of what decides the clone format; the other half is per pool, in
	// Pool.CloneFormat.
	MinCompatClient string
	// CloneV2 reports whether a snapshot can be cloned WITHOUT being protected —
	// and, the half that matters, removed while a clone of it is still live.
	CloneV2 bool
	// Pools describes each pool billet was pointed at, in the order it names them.
	Pools []Pool
}

// Pool is one pool billet uses, and what the cluster says about it.
type Pool struct {
	// Name and Purpose are what the config called it and what billet keeps there.
	Name    string
	Purpose string
	// Images is a count rather than a list: this goes on an operator's terminal,
	// and a site with a thousand cache volumes should not print a thousand lines.
	Images int
	// Size and MinSize are the replication the operator chose. Zero means the
	// cluster did not say, which is reported rather than guessed at.
	Size    int
	MinSize int
	// CloneFormat is rbd_default_clone_format AS THIS POOL SEES IT.
	//
	// PER POOL, because it can be set per pool: `rbd config pool set billet-cache
	// rbd_default_clone_format 1` leaves the image pool reporting `auto` while
	// clones in the cache pool fail with `(22) Invalid argument` — measured. Both
	// pools hold clones (root clones in one, cache generations in the other), so
	// reading one and calling it the cluster's answer is the same proxy mistake one
	// level down.
	CloneFormat string
}

// ErrCloneV1 is returned when the cluster would clone a snapshot the old way.
//
// IT IS A REFUSAL RATHER THAN A NOTE, and the reason is when the cost lands. On a
// clone-v1 cluster a snapshot must be PROTECTED before it can be cloned, and a
// protected snapshot with a live clone can be neither unprotected nor removed —
// so a cache generation that any running job holds a clone of is undeletable, and
// eviction is blocked by ordinary traffic rather than by anything wrong. Nothing
// in billet clones yet, which is exactly why this is the moment to say so: the fix
// is one command on an empty cluster, and the same fix after a fleet has been
// built on it is a full pool and a debugging session that starts nowhere near
// here.
var ErrCloneV1 = errors.New("this cluster would clone snapshots the old way, which makes a cache " +
	"generation undeletable while any job holds a clone of it")

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
	report := Report{User: c.cfg.User}

	for _, p := range []Pool{
		{Name: c.cfg.ImagePool, Purpose: "golden images and per-job root clones"},
		{Name: c.cfg.CachePool, Purpose: "cache volumes"},
	} {
		images, err := c.list(ctx, p.Name)
		if err != nil {
			return Report{}, err
		}

		p.Images = images
		report.Pools = append(report.Pools, p)
	}

	// THE CLUSTER'S OWN CONFIGURATION, which is a different question from whether
	// this host can reach it — and the one that decides whether the storage layer
	// can ever reclaim anything.
	release, err := c.minCompatClient(ctx)
	if err != nil {
		return Report{}, err
	}

	report.MinCompatClient = release

	// EVERY POOL, not the first one. The clone format can be set per pool, and both
	// of these hold clones — root clones in one, cache generations in the other —
	// so a cluster where only the cache pool is forced to v1 has exactly the
	// undeletable-generation problem this check exists to catch.
	var v1 []string

	for i := range report.Pools {
		configured, err := c.cloneFormat(ctx, report.Pools[i].Name)
		if err != nil {
			return Report{}, err
		}

		report.Pools[i].CloneFormat = configured

		format, err := EffectiveCloneFormat(release, configured)
		if err != nil {
			return Report{}, fmt.Errorf("ceph: %w", err)
		}

		if format == 1 {
			v1 = append(v1, report.Pools[i].Name)
		}
	}

	report.CloneV2 = len(v1) == 0

	sizes, err := c.poolSizes(ctx)
	if err != nil {
		return Report{}, err
	}

	// NO PRESENCE CHECK, because the zero value already means what absence means. A
	// pool the cluster did not describe comes back size 0, which the report renders
	// as "replication unknown" — writing `if ok` around it would be a guard whose
	// mutation survives, because both branches produce the same answer.
	for i := range report.Pools {
		s := sizes[report.Pools[i].Name]
		report.Pools[i].Size, report.Pools[i].MinSize = s.Size, s.MinSize
	}

	if !report.CloneV2 {
		// WHICH CAUSE, because the remedies are different and giving the wrong one
		// sends an operator to a setting that is already correct. A forced format is
		// named per pool, since it can be set on one and not the other.
		for _, p := range report.Pools {
			if p.CloneFormat == "1" {
				return report, fmt.Errorf("%w: pool %s has rbd_default_clone_format set to 1, "+
					"which overrides the cluster's minimum client release (%s); unset it with "+
					"`rbd config pool rm %s rbd_default_clone_format`, or `ceph config rm client "+
					"rbd_default_clone_format` if it was set cluster-wide",
					ErrCloneV1, p.Name, release, p.Name)
			}
		}

		return report, fmt.Errorf("%w: require-min-compat-client is %q, and pools %s take their "+
			"clone format from it; raise it with `ceph osd set-require-min-compat-client mimic` "+
			"(which refuses while clients older than mimic are connected — `ceph features` lists "+
			"them)", ErrCloneV1, release, strings.Join(v1, " and "))
	}

	return report, nil
}

// beforeMimic is every Ceph release older than the one that introduced clone v2.
//
// THE CLOSED HALF OF THE LIST, deliberately. A set of releases at-or-after mimic
// would go stale on the next Ceph release and start refusing correct clusters,
// while the set BEFORE mimic can never grow — Ceph is not going to ship a release
// older than one from 2018. So an unrecognised NAME is treated as newer, which is
// the only direction that stays true without maintenance.
//
// Measured against Ceph 20.2.3 rather than remembered: every name here is one the
// cluster accepts for `osd set-require-min-compat-client`, and a name it does not
// know is refused with "is not recognized", so this is the complete set of answers
// that can mean "older than mimic".
var beforeMimic = map[string]bool{
	"argonaut": true, "bobtail": true, "cuttlefish": true, "dumpling": true,
	"emperor": true, "firefly": true, "giant": true, "hammer": true,
	"infernalis": true, "jewel": true, "kraken": true, "luminous": true,
}

// releaseName is the shape of a Ceph release: a short lowercase word. Every
// release from argonaut to tentacle is one, and the naming convention has not
// varied in fifteen years.
//
// A SHAPE CHECK IS NOT PEDANTRY HERE, because "anything I do not recognise is
// newer than mimic" is a fail-OPEN rule and this is what bounds what can walk
// through it. Without it, whatever the binary at that path happened to print —
// a megabyte of output, a usage message, an error from a different program — is
// read as a release newer than mimic and the check passes.
var releaseName = regexp.MustCompile(`^[a-z]{3,20}$`)

// ErrUnclassifiedRelease is returned when the cluster's answer is not a release
// billet can place relative to mimic.
//
// `unknown` is the case that matters and it is not hypothetical: it is the zero
// value of Ceph's release enum, and `osd set-require-min-compat-client unknown` is
// REFUSED — measured — so it is a value that can only arrive from Ceph itself,
// never from an operator who chose it. Which is exactly why it must not be read as
// "some release newer than mimic": it means the cluster has not been told, and a
// cluster that has not been told defaults to the old clone format.
var ErrUnclassifiedRelease = errors.New("the cluster's minimum client release is not one billet " +
	"can place relative to mimic")

// EffectiveCloneFormat resolves what rbd will ACTUALLY do from the two settings
// that decide it.
//
// THE FLOOR ALONE IS A PROXY, AND IT IS DEFEATED BY ONE CONFIG KEY. `auto` — the
// default — means "v2 if the cluster admits mimic or later", which is where the
// release rule applies. But `rbd_default_clone_format` can be set outright, and
// then it wins: measured, a cluster whose floor is mimic and whose clone format is
// forced to 1 refuses to clone an unprotected snapshot with
// `rbd: clone error: (22) Invalid argument`. Checking the floor and calling it
// "this cluster clones the new way" would have been a green preflight beside the
// exact failure the preflight exists to prevent.
func EffectiveCloneFormat(minCompatClient, configured string) (int, error) {
	switch strings.TrimSpace(configured) {
	case "1":
		return 1, nil
	case "2":
		return 2, nil
	case "", "auto":
		// The default, and the case where the floor decides.
	default:
		return 0, fmt.Errorf("rbd_default_clone_format is %q, which is not auto, 1 or 2", configured)
	}

	release := strings.ToLower(strings.TrimSpace(minCompatClient))
	if !releaseName.MatchString(release) || release == "unknown" {
		return 0, fmt.Errorf("%w: %q", ErrUnclassifiedRelease, release)
	}

	if beforeMimic[release] {
		return 1, nil
	}

	return 2, nil
}

// minCompatClient asks the cluster which client releases it admits.
//
// NO --format json FOR THIS ONE. The mon answers it as a bare string whatever the
// formatter says — measured — so asking for json buys nothing and would make a mon
// that DID honour it answer `"luminous"` with quotes, which the release shape then
// refuses. Refusing is the safe direction, but not asking is better than relying
// on it.
func (c *Client) minCompatClient(ctx context.Context) (string, error) {
	out, err := c.cephCmd(ctx, false, "osd", "get-require-min-compat-client")
	if err != nil {
		return "", fmt.Errorf("ceph: the cluster would not say which client releases it admits, "+
			"so billet cannot tell whether a cache generation could ever be reclaimed: %w", err)
	}

	release := strings.TrimSpace(string(out))
	if release == "" {
		return "", errors.New("ceph: the cluster named no minimum client release")
	}

	// NOT RENDERED IF IT IS NOT A RELEASE NAME, the same rule the image listing
	// follows: output billet cannot parse means it is talking to something that is
	// not the ceph it expects, and echoing that onto a terminal is how another
	// program's output — or a file it printed — becomes billet's error message.
	if !releaseName.MatchString(strings.ToLower(release)) {
		return "", fmt.Errorf("ceph: %s did not answer with a release name when asked which "+
			"client releases the cluster admits; is it the ceph command?", c.ceph)
	}

	return release, nil
}

// cloneFormat reads the setting that can override the cluster's floor.
//
// Read through `rbd` rather than `ceph config get`, because the scoped identity
// can do the first and not the second — measured, `ceph config get client
// rbd_default_clone_format` answers EACCES for a `profile rbd` key, while
// `rbd config pool list` answers with the effective value and where it came from.
func (c *Client) cloneFormat(ctx context.Context, pool string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.wait)
	defer cancel()

	// THE POOL IS POSITIONAL HERE, not the value of -p, and that is not a detail
	// this could be reasoned to: `rbd --format json -p <pool> config pool list`
	// answers `unrecognised option '-p'`, while `rbd config pool list <pool>`
	// works. `rbd help config pool list` states the grammar — `<pool-name>` as a
	// positional — and the unit test asserted billet's own mistake until the real
	// cluster refused it.
	args := append(c.identity(), "--format", "json", "config", "pool", "list", pool)

	out, err := c.run(ctx, c.bin, args)
	if err != nil {
		return "", fmt.Errorf("ceph: the rbd configuration for pool %q could not be read as "+
			"client.%s: %w", pool, c.cfg.User, err)
	}

	var options []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}

	if err := json.Unmarshal(bytes.TrimSpace(out), &options); err != nil || options == nil {
		return "", fmt.Errorf("ceph: %s did not answer with a json configuration list for pool "+
			"%q; is it the rbd command?", c.bin, pool)
	}

	for _, o := range options {
		if o.Name == "rbd_default_clone_format" {
			return o.Value, nil
		}
	}

	// ABSENT MEANS THE DEFAULT, which is `auto`. A cluster that does not list the
	// option has not overridden it.
	return "auto", nil
}

// poolSpec is the half of `ceph osd pool ls detail` billet reads.
type poolSpec struct {
	Name    string `json:"pool_name"`
	Size    int    `json:"size"`
	MinSize int    `json:"min_size"`
}

// poolSizes reads the replication the operator chose, for every pool at once.
//
// ONE CALL RATHER THAN ONE PER FIELD. `osd pool get <pool> size` answers a single
// parameter, so the targeted form is four invocations for two pools; the detail
// listing is one, and billet already knows which names it cares about.
func (c *Client) poolSizes(ctx context.Context) (map[string]poolSpec, error) {
	out, err := c.cephCmd(ctx, true, "osd", "pool", "ls", "detail")
	if err != nil {
		return nil, fmt.Errorf("ceph: the pool listing could not be read as client.%s: %w",
			c.cfg.User, err)
	}

	// `pools == nil` CATCHES A JSON `null`, which unmarshals into a slice happily
	// and would be read as "the cluster described no pools" — reported as
	// "replication unknown" beside a successful check. A cluster that answered
	// `null` is not one billet understood.
	var pools []poolSpec
	if err := json.Unmarshal(bytes.TrimSpace(out), &pools); err != nil || pools == nil {
		return nil, fmt.Errorf("ceph: %s did not answer with a json pool listing; is it the ceph "+
			"command?", c.ceph)
	}

	byName := make(map[string]poolSpec, len(pools))
	for _, p := range pools {
		byName[p.Name] = p
	}

	return byName, nil
}

// cephCmd runs one `ceph` invocation, bounded like every other.
//
// `asJSON` is a parameter rather than always true because one of the two commands
// billet runs does not answer in JSON whatever the formatter says — see
// minCompatClient — and asking anyway would make a mon that DID honour it return
// a quoted string the release shape then refuses.
func (c *Client) cephCmd(ctx context.Context, asJSON bool, command ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.wait)
	defer cancel()

	args := c.identity()
	if asJSON {
		args = append(args, "--format", "json")
	}

	return c.run(ctx, c.ceph, append(args, command...))
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
	args := append(c.identity(), "--format", "json", "-p", pool)

	return append(args, command...)
}

// identity is the half of an invocation that says who billet is, shared by both
// commands so they cannot come to authenticate as different clients.
func (c *Client) identity() []string {
	args := []string{"--id", c.cfg.User}

	if c.cfg.ConfPath != "" {
		args = append(args, "--conf", c.cfg.ConfPath)
	}

	if c.cfg.KeyringPath != "" {
		args = append(args, "--keyring", c.cfg.KeyringPath)
	}

	return args
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
