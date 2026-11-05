package app

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostsource"
	"github.com/junioryono/billet/internal/hostsource/awsmetal"
	"github.com/junioryono/billet/internal/hostsource/phoenixnap"
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
		Tiers:          cfg.Tiers,
		Boot: func(order, token string) ([]byte, error) {
			fingerprint, err := caFingerprint(cfg)
			if err != nil {
				return nil, err
			}

			return PoolBoot(cfg, string(prelude), fingerprint, order, token)
		},
	}, pool.WithLogger(log))
	if err != nil {
		return nil, 0, err
	}

	return p, pc.PoolInterval(), nil
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
		src, err := awsmetal.New(prov.Hosts(), m.AvailabilityZone, shapes)
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
func PoolBoot(cfg *config.Config, prelude, fingerprint, order, token string) ([]byte, error) {
	pc := cfg.Server.Pool
	join := []string{
		"billet", "join", "--server", pc.JoinServer, "--ca-fingerprint", fingerprint,
		"--token", token, "--node-name", "pool-" + order, "--provider", string(config.ProviderFirecracker),
	}
	if pc.BootstrapAddr != "" {
		join = append(join, "--bootstrap-addr", pc.BootstrapAddr)
	}
	switch {
	case cfg.GitHub != nil && cfg.GitHub.Org != "":
		join = append(join, "--org", cfg.GitHub.Org)
	case cfg.GitHub != nil && cfg.GitHub.Repository != "":
		join = append(join, "--repository", cfg.GitHub.Repository)
	}
	quoted := make([]string, len(join))
	for i, a := range join {
		quoted[i] = singleQuoted(a)
	}

	var b strings.Builder
	b.WriteString("#!/bin/bash\n# billet pool order " + order + "\nset -euo pipefail\n\n")
	b.WriteString(strings.TrimRight(prelude, "\n"))
	b.WriteString("\n\n" + strings.Join(quoted, " ") + "\n")
	if b.Len() > maxBootPayload {
		return nil, fmt.Errorf("app: a pool host's boot is %d bytes, past the %d a source carries; shorten "+
			"server.pool.boot_prelude", b.Len(), maxBootPayload)
	}

	return []byte(b.String()), nil
}

// singleQuoted is one shell word holding s exactly: inside single quotes the shell
// reads every byte literally, and an embedded quote closes, escapes and reopens.
func singleQuoted(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
