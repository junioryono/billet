package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/junioryono/billet/internal/config"
)

// initHeadroomVCPU and initHeadroomMemory are what a generated ceiling leaves
// for the machine itself.
//
// The ceiling is what the allocator escrows against, so setting it to everything
// the host has means billet will happily fill the machine and leave nothing for
// the kernel, the container runtime, or the operator's shell. These are a
// starting point an operator can raise, not a measurement.
const (
	initHeadroomVCPU   = 2
	initHeadroomMemory = 4 * config.GiB
)

// cmdInit writes a billet.yaml that runs.
//
// THE POINT IS THAT NOTHING HAS TO BE HAND-EDITED AFTERWARDS. Copying the
// example meant editing the provider, every tier's image, the state directories
// and the capacity ceiling before anything would start, and each of those is a
// step an operator can get wrong in a way that surfaces much later — a ceiling
// larger than the machine does not fail, it overcommits.
//
// What it cannot know is the GitHub App, because that does not exist yet. The
// file it writes names the org and leaves the ids at zero, and
// `billet github-app create` fills them in rather than printing a block to
// paste.
func cmdInit(_ context.Context, args []string) error {
	fs := newFlagSet("billet init")
	cfgPath := addConfigFlag(fs)
	org := fs.String("org", "", "the GitHub organization these runners serve")
	provider := fs.String("provider", string(config.ProviderDocker),
		"compute backend for this host")
	image := fs.String("image", defaultRunnerImage,
		"container image containing the GitHub runner")
	force := fs.Bool("force", false, "overwrite an existing config")

	if err := parse(fs, args); err != nil {
		return err
	}

	kind := config.ProviderKind(*provider)
	if !kind.Valid() {
		return fmt.Errorf("provider %q is not one of firecracker, tart, ec2, docker", *provider)
	}

	// REFUSED RATHER THAN WRITTEN AND THEN REFUSED AT STARTUP. Only docker is
	// built, and a generated file naming a backend that cannot run is a file the
	// operator has to debug rather than use.
	if kind != config.ProviderDocker {
		return fmt.Errorf("%w: the %s provider is not built yet, so `billet init` cannot write a "+
			"config that starts; docker is the only backend available today", errNotImplemented, kind)
	}

	// NEVER OVER AN EXISTING FILE without being asked. A config carries the state
	// directory and the App key path, so replacing one silently is how a working
	// deployment loses the only record of where its data is.
	if _, err := os.Stat(*cfgPath); err == nil && !*force {
		return fmt.Errorf("%s already exists; pass --force to replace it", *cfgPath)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check %s: %w", *cfgPath, err)
	}

	vcpu, memory, err := config.DetectHostCapacity()
	if err != nil {
		return fmt.Errorf("detect what this machine has: %w", err)
	}

	body := generatedConfig(*org, kind, *image, vcpu, memory)

	if err := os.MkdirAll(filepath.Dir(*cfgPath), 0o750); err != nil {
		return fmt.Errorf("create the directory for %s: %w", *cfgPath, err)
	}

	if err := os.WriteFile(*cfgPath, []byte(body), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", *cfgPath, err)
	}

	fmt.Printf("Wrote %s\n\n", *cfgPath)
	fmt.Printf("  this machine    %d vCPU, %s\n", vcpu, memory)
	fmt.Printf("  billet ceiling  %d vCPU, %s (the rest is left for the host)\n",
		ceilingVCPU(vcpu), ceilingMemory(memory))
	fmt.Printf("\nNext:\n\n")

	if *org == "" {
		fmt.Printf("  billet github-app create --org <your-org> --config %s\n", *cfgPath)
	} else {
		fmt.Printf("  billet github-app create --org %s --config %s\n", *org, *cfgPath)
	}

	fmt.Printf("  billet check --config %s\n", *cfgPath)
	fmt.Printf("\nThen run the control plane and a compute host, in two terminals:\n\n")
	fmt.Printf("  billet server --config %s\n", *cfgPath)
	fmt.Printf("  billet node   --config %s\n", *cfgPath)

	return nil
}

// defaultRunnerImage is a container image that already contains the runner.
//
// The tier's image is handed straight to `docker run`, so a golden-image name
// like `ubuntu-2404-x64` — which is what the Firecracker example uses — is not
// pullable and every job fails to launch with a message about the image rather
// than about the config.
const defaultRunnerImage = "ghcr.io/actions/actions-runner:latest"

// ceilingVCPU and ceilingMemory are what billet may spend, leaving the host
// enough to keep working.
func ceilingVCPU(detected int) int {
	return max(1, detected-initHeadroomVCPU)
}

func ceilingMemory(detected config.ByteSize) config.ByteSize {
	if detected <= initHeadroomMemory {
		return detected
	}

	return detected - initHeadroomMemory
}

// generatedConfig renders a config for this machine.
//
// Written as text rather than marshalled from the struct, because the comments
// are most of the value: an operator reading this file should be able to see
// which numbers billet measured, which it chose, and what changing one costs.
func generatedConfig(org string, kind config.ProviderKind, image string, vcpu int, memory config.ByteSize) string {
	if org == "" {
		org = "your-org"
	}

	stateBase := "/var/lib/billet"
	if dir, err := os.UserConfigDir(); err == nil {
		stateBase = filepath.Join(dir, "billet")
	}

	return fmt.Sprintf(`# billet — written by `+"`billet init`"+` for this machine.
#
# One file, both roles: `+"`billet server`"+` is the control plane and
# `+"`billet node`"+` is a compute host. On this machine you run both, as two
# processes talking over the loopback address below — nothing is exposed to the
# network and no certificates are involved.
#
# Add a second machine by running `+"`billet ca issue <name>`"+` here, copying the
# bundle to that host, and giving it a config with a node: section pointing at
# this one. Its node.name comes from the certificate.

server:
  # Loopback only, so billet opens nothing the network can reach. A control
  # plane that must serve other machines binds an address they can reach, and
  # then the wire requires the client certificates `+"`billet ca issue`"+` mints.
  listen: 127.0.0.1:7717

  state_dir: %s

  # THE CEILING BILLET ESCROWS AGAINST, and the one number worth reviewing.
  #
  # This machine has %d vCPU and %s. Left here is %d vCPU and %s, keeping
  # %d vCPU and %s for the kernel, the container runtime and your shell.
  # Raising it lets billet fill the machine; there is no error when it does,
  # only a host that is busier than you expected.
  max_vcpu: %d
  max_memory: %s

github:
  org: %s

  # Filled in by `+"`billet github-app create --config <this file>`"+`.
  app_id: 0
  installation_id: 0
  private_key_path: %s

# This machine, as a compute host. Delete this section on a control plane that
# should not run jobs itself.
node:
  # name is omitted: with a certificate the name comes from it, and on this
  # machine there is no certificate to disagree with.
  server_addr: 127.0.0.1:7717
  provider: %s
  state_dir: %s

  # What this host CONTRIBUTES, which need not be everything it has. Unset means
  # everything billet can detect, bounded by the ceiling above.
  #
  # max_vcpu: %d
  # max_memory: %s

# Tiers are yours to define. The label is what users put in `+"`runs-on`"+`, and the
# server is the only role that reads them — a node is told the shape of what it
# is launching with each job.
tiers:
  - label: billet-2vcpu
    provider: %s
    vcpu: 2
    memory: 8GiB
    image: %s

  - label: billet-4vcpu
    provider: %s
    vcpu: 4
    memory: 16GiB
    image: %s
`,
		filepath.Join(stateBase, "server"),
		vcpu, memory, ceilingVCPU(vcpu), ceilingMemory(memory),
		vcpu-ceilingVCPU(vcpu), memory-ceilingMemory(memory),
		ceilingVCPU(vcpu), ceilingMemory(memory),
		org,
		filepath.Join(stateBase, "app-private-key.pem"),
		kind,
		filepath.Join(stateBase, "node"),
		ceilingVCPU(vcpu), ceilingMemory(memory),
		kind, image,
		kind, image,
	)
}
