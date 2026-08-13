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
// bearing HERE and not only there: a pool name is passed to rbd as the value of
// -p, so one beginning with a dash would be read as a flag, and an identity of
// `admin` would hand this process a key that can delete the pools it is only
// meant to read.
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

// execRunner runs rbd and returns its standard output.
//
// STDERR IS DROPPED INTO THE ERROR, not into the result. librados logs to stderr
// on every connection — a successful listing is routinely accompanied by warnings
// — so folding the two together makes valid JSON unparseable.
func execRunner(ctx context.Context, bin string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)

	var stderr strings.Builder
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, lastLine(msg))
		}

		return nil, err
	}

	return out, nil
}

// lastLine keeps the line rbd ended on.
//
// librados prints a paragraph of connection logging before the sentence that says
// what went wrong, and the last line is the one an operator can act on.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")

	return strings.TrimSpace(lines[len(lines)-1])
}
