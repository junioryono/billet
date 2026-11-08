package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awsidentity"
	"github.com/junioryono/billet/internal/awssts"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostsource"
	"github.com/junioryono/billet/internal/hostsource/awsmetal"
	"github.com/junioryono/billet/internal/hostsource/phoenixnap"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/pool"
	"github.com/junioryono/billet/internal/provider/ec2"
	"github.com/junioryono/billet/internal/wirecert"
)

// maxBootPayload is EC2's user-data limit, which bounds every source's boot.
const maxBootPayload = 16384

// NewPool builds the pool server.pool configures, and the interval it steps at. The
// sources are built here, with the deployment's identity as the AWS owner, and the
// boot payload ends in the `billet join` this deployment's authority answers.
func NewPool(cfg *config.Config, ledger *alloc.Allocator, deployment string, log *slog.Logger) (*pool.Pool, time.Duration, error) {
	pc := cfg.Server.Pool
	if pc == nil {
		return nil, 0, errors.New("app: server.pool is not configured")
	}
	sources, err := poolSources(pc, deployment)
	if err != nil {
		return nil, 0, err
	}
	prelude, err := os.ReadFile(pc.BootPrelude)
	if err != nil {
		return nil, 0, fmt.Errorf("app: read server.pool.boot_prelude: %w", err)
	}
	// A BOOT THAT CANNOT BE RENDERED IS REFUSED HERE, at the start, rather than once per
	// order after the order is committed.
	if _, err := PoolBoot(cfg, string(prelude), "SHA256:"+strings.Repeat("a", 64), "p000000000000", tokenStandIn); err != nil {
		return nil, 0, err
	}
	archs := make(map[config.Arch]pool.Policy, len(pc.Architectures))
	for arch, pa := range pc.Architectures {
		choices := make([]pool.Choice, 0, len(pa.Order))
		for _, c := range pa.Order {
			choices = append(choices, pool.Choice{Source: c.Source, Shape: c.Shape, Location: c.Location})
		}
		archs[arch] = pool.Policy{Floor: pa.Floor, Max: pa.Max, Choices: choices}
	}
	p, err := pool.New(ledger, pool.Config{
		Archs:          archs,
		Sources:        sources,
		RefusalBackoff: pc.PoolRefusalBackoff(),
		IdleAfter:      idleAfter(pc),
		Tiers:          cfg.Tiers,
		Boot:           poolBootFor(cfg, string(prelude)),
	}, pool.WithLogger(log))
	if err != nil {
		return nil, 0, err
	}

	return p, pc.PoolInterval(), nil
}

// idleAfter is how long a joined host holds nothing before the pool asks for its
// release, zero when the config does not have the pool shrink itself.
func idleAfter(pc *config.PoolConfig) time.Duration {
	d, ok := pc.PoolIdleAfter()
	if !ok {
		return 0
	}

	return d
}

// poolJoins is what the node wire asks of a pool host's enrollment: the ledger's half
// from the allocator and the source's word from the judge.
type poolJoins struct {
	*alloc.Allocator
	*pool.Joins
}

var _ nodeplane.PoolJoins = poolJoins{}

// NewPoolJoins builds what admits server.pool's hosts on their source's word (ADR-030),
// over sources of its own built as the pool's are.
func NewPoolJoins(cfg *config.Config, ledger *alloc.Allocator, deployment string) (nodeplane.PoolJoins, error) {
	pc := cfg.Server.Pool
	if pc == nil {
		return nil, errors.New("app: server.pool is not configured")
	}
	sources, err := poolSources(pc, deployment)
	if err != nil {
		return nil, err
	}

	return poolJoins{Allocator: ledger, Joins: pool.NewJoins(ledger, sources, nil)}, nil
}

// poolWireOptions is the wire's options with a pool's joins added when the config names
// a pool (ADR-030), so its hosts join on their source's word through the sources the
// pool orders from.
//
// AND WITHOUT ONE: orders a pool placed, and their tokens, outlive a config that names
// no pool any more, so their hosts are still answered by the ledger, and with no source
// to ask, each waits for a person.
func poolWireOptions(cfg *config.Config, ledger *alloc.Allocator, deployment string, opts ...wireOption) ([]wireOption, error) {
	if cfg.Server.Pool == nil {
		return append(opts, withPoolJoins(poolJoins{Allocator: ledger, Joins: pool.NewJoins(ledger, nil, nil)})), nil
	}
	joins, err := NewPoolJoins(cfg, ledger, deployment)
	if err != nil {
		return nil, err
	}

	return append(opts, withPoolJoins(joins)), nil
}

// seesHostAddress says whether an enrollment's peer is the host itself: a router in
// front of the listeners is every enrollment's peer.
func seesHostAddress(cfg *config.Config) bool { return cfg.Server.TLSRouter == nil }

// schedulePool is the controller's pool loop, and false when the config names no pool
// or the run is dry, which orders nothing as it accepts no job.
func schedulePool(cfg *config.Config, ledger *alloc.Allocator, deployment string, dryRun bool) (func(context.Context), bool, error) {
	if cfg.Server.Pool == nil || dryRun {
		return nil, false, nil
	}
	hosts, interval, err := NewPool(cfg, ledger, deployment, slog.Default())
	if err != nil {
		return nil, false, err
	}

	return func(ctx context.Context) { hosts.Run(ctx, interval) }, true, nil
}

func poolSources(pc *config.PoolConfig, deployment string) (map[string]hostsource.Source, error) {
	out := map[string]hostsource.Source{}
	if m := pc.AWSMetal; m != nil {
		prov, err := ec2.New(deployment, config.EC2Config{
			Region: m.Region, SubnetID: m.SubnetID, SecurityGroupIDs: m.SecurityGroupIDs,
			InstanceProfile: m.InstanceProfile,
		})
		if err != nil {
			return nil, fmt.Errorf("app: server.pool.aws_metal: %w", err)
		}
		shapes := make([]awsmetal.Shape, 0, len(m.Shapes))
		for _, s := range m.Shapes {
			shapes = append(shapes, awsmetal.Shape{Type: s.Type, Arch: s.Arch, Image: m.Images[s.Arch]})
		}
		region := m.Region
		account := func(ctx context.Context) (string, error) {
			id, err := awssts.New(region, "", awscreds.Default(), nil).Whoami(ctx)

			return id.Account, err
		}
		identity, err := awsidentity.AWS()
		if err != nil {
			return nil, err
		}
		src, err := awsmetal.New(prov.Hosts(), awsmetal.Config{
			Region: m.Region, Location: m.AvailabilityZone, Shapes: shapes, Account: account, Identity: identity,
		})
		if err != nil {
			return nil, err
		}
		out[config.PoolSourceAWSMetal] = src
	}
	if n := pc.PhoenixNAP; n != nil {
		id, secret := os.Getenv(n.ClientIDEnv), os.Getenv(n.ClientSecretEnv)
		if id == "" || secret == "" {
			return nil, fmt.Errorf("app: server.pool.phoenixnap names %s and %s, and one of them is empty; export "+
				"both in the service's environment", n.ClientIDEnv, n.ClientSecretEnv)
		}
		shapes := make([]phoenixnap.Shape, 0, len(n.Shapes))
		for _, s := range n.Shapes {
			shapes = append(shapes, phoenixnap.Shape{Type: s.Type, Arch: s.Arch})
		}
		src, err := phoenixnap.New(phoenixnap.Credentials{ClientID: id, Secret: hostsource.NewSecret([]byte(secret))}, shapes)
		if err != nil {
			return nil, err
		}
		out[config.PoolSourcePhoenixNAP] = src
	}

	return out, nil
}

// poolBootFor renders each order's boot with the authority as it is when the order is
// sent, so a rotation between orders is followed.
func poolBootFor(cfg *config.Config, prelude string) func(order, token string) ([]byte, error) {
	return func(order, token string) ([]byte, error) {
		fingerprint, err := caFingerprint(cfg)
		if err != nil {
			return nil, err
		}

		return PoolBoot(cfg, prelude, fingerprint, order, token)
	}
}

// caFingerprint is the deployment authority's, read from the public certificate.
func caFingerprint(cfg *config.Config) (string, error) {
	raw, err := os.ReadFile(wirecert.CACertPath(cfg.Server.IdentityDir))
	if err != nil {
		return "", fmt.Errorf("app: read the deployment's authority for a pool host's join: %w", err)
	}

	return wirecert.FingerprintOfCAPEM(raw)
}

// PoolBoot is an ordered host's first boot: the operator's prelude, which installs
// billet and what a Firecracker node needs, then the `billet join` that enrolls it
// under its order's single-use token. The token is a credential, so the whole payload
// is one, and it fits EC2's user data.
//
// THE PRELUDE RUNS IN A SHELL OF ITS OWN, written to a file through a quoted heredoc,
// so a `set -x` it carries for its own debugging never traces the join, and tracing is
// switched off again before the token is ever in a command line.
func PoolBoot(cfg *config.Config, prelude, fingerprint, order, token string) ([]byte, error) {
	pc := cfg.Server.Pool
	join := []string{
		"billet", "join", "--server", pc.JoinServer, "--ca-fingerprint", fingerprint,
		"--token", token, "--node-name", alloc.PoolNodeName(order), "--pool-order", order,
		"--provider", string(config.ProviderFirecracker),
	}
	if addr := cfg.Server.PoolEnrollAddr(); addr != "" {
		join = append(join, "--bootstrap-addr", addr)
	}
	switch {
	case cfg.GitHub != nil && cfg.GitHub.Org != "":
		join = append(join, "--org", cfg.GitHub.Org)
	case cfg.GitHub != nil && cfg.GitHub.Repository != "":
		join = append(join, "--repository", cfg.GitHub.Repository)
	}
	// WHAT INIT NEEDS AND THE JOIN CANNOT KNOW: a Firecracker node's config names its
	// kernel image and Ceph identity, which the prelude prepared on the host.
	if len(pc.InitArgs) > 0 {
		join = append(append(join, "--"), pc.InitArgs...)
	}
	quoted := make([]string, len(join))
	for i, a := range join {
		// NO NUL IN A WORD OR THE PRELUDE: bash drops it, so what runs is not what was
		// configured.
		if strings.ContainsRune(a, 0) {
			return nil, fmt.Errorf("app: a pool host's join argument %d holds a NUL byte", i)
		}
		quoted[i] = singleQuoted(a)
	}
	if strings.ContainsRune(prelude, 0) {
		return nil, errors.New("app: server.pool.boot_prelude holds a NUL byte, which bash would drop")
	}
	body := strings.TrimRight(prelude, "\n")
	for _, line := range strings.Split(body, "\n") {
		if line == preludeEnd {
			return nil, fmt.Errorf("app: server.pool.boot_prelude has a line %q, which ends the heredoc it is "+
				"written through", preludeEnd)
		}
	}

	var b strings.Builder
	b.WriteString("#!/bin/bash\n# billet pool order " + order + "\nset -euo pipefail\n\n")
	// THE PRELUDE RUNS IN A BASH OF ITS OWN, under the same errexit, nounset and
	// pipefail as the boot, so whatever it sets, traces or exits cannot reach the join,
	// and a failure in it stops the boot before the token is used. Its file is removed
	// however the boot ends.
	b.WriteString("umask 077\ntrap 'rm -f /run/billet-pool-prelude.sh' EXIT\n")
	b.WriteString("cat >/run/billet-pool-prelude.sh <<'" + preludeEnd + "'\n")
	b.WriteString(body + "\n" + preludeEnd + "\n")
	b.WriteString("bash -euo pipefail /run/billet-pool-prelude.sh\nrm -f /run/billet-pool-prelude.sh\n\n")
	b.WriteString("set +o xtrace +o verbose\n")
	b.WriteString(strings.Join(quoted, " ") + "\n")
	if b.Len() > maxBootPayload {
		return nil, fmt.Errorf("app: a pool host's boot is %d bytes, past the %d a source carries; shorten "+
			"server.pool.boot_prelude", b.Len(), maxBootPayload)
	}

	return []byte(b.String()), nil
}

// preludeEnd closes the heredoc the prelude is written through.
const preludeEnd = "BILLET_POOL_PRELUDE_END"

// tokenStandIn is as long as a join token (20 random bytes in base32), so a boot
// rendered with it at assembly is as long as every order's.
const tokenStandIn = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// singleQuoted is one shell word holding s exactly: inside single quotes the shell
// reads every byte literally, and an embedded quote closes, escapes and reopens.
func singleQuoted(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
