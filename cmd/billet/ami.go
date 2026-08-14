package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider/ec2"
	"github.com/junioryono/billet/internal/runnerrelease"
)

// defaultRunnerVersion is the actions/runner release a build installs unless told
// otherwise.
//
// PINNED, NOT "LATEST", because an image is a thing you reproduce. A build that
// silently tracked the newest release would make two runs of the same command
// produce different images, and the difference would surface as a job failing on
// one AMI and not another.
//
// ONE PIN FOR EVERY IMAGE BILLET BUILDS. This was a constant here and a shell
// default in the guest image script, so bumping the runner was two edits in two
// languages — and doing one of them leaves a fleet where the ec2 backend is current
// and the microVM backend is not, found on the day GitHub stops queueing to the
// stale half.

// cmdAMI builds the machine image the ec2 backend launches.
//
// THE BACKEND HAS ALWAYS NEEDED AN IMAGE NOTHING PRODUCED. A tier's `image:` is an
// AMI id, and until this existed an operator had to build one by hand — where the
// failure mode is the quiet one this project keeps running into: the instance
// boots, RunInstances reports success, billet logs a started runner, and the job
// sits queued forever because nothing consumed the registration.
func cmdAMI(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "build" {
		return errors.New("usage: billet ami build [flags]")
	}

	fs := newFlagSet("billet ami build")
	cfgPath := addConfigFlag(fs)
	base := fs.String("base-image", "", "AMI to provision from (an EBS-backed, dnf-based image)")
	shape := fs.String("instance-type", "c7i.xlarge", "shape of the BUILDER, not of your jobs")
	arch := fs.String("arch", "x64", "runner build to install: x64 or arm64")
	version := fs.String("runner-version", runnerrelease.Pinned(), "actions/runner release")
	name := fs.String("name", "", "name for the produced AMI (default: billet-runner-<timestamp>)")
	region := fs.String("region", "", "override the region from the config")
	subnet := fs.String("subnet", "", "override the subnet from the config")
	group := fs.String("security-group", "", "override the security group from the config")
	timeout := fs.Duration("timeout", 20*time.Minute, "how long to wait for the whole build")
	public := fs.Bool("public-ip", false,
		"give the builder a public address, for a subnet with no NAT gateway")

	if err := parse(fs, args[1:]); err != nil {
		return err
	}

	if *base == "" {
		return errors.New("--base-image is required: billet does not guess which distribution " +
			"you want, because the provisioning script assumes dnf and a wrong guess fails " +
			"halfway through a build you paid for")
	}

	cfg, err := ec2ConfigFor(*cfgPath, *region, *subnet, *group)
	if err != nil {
		return err
	}

	// A BUILDER WITH NO ROUTE OUT PROVISIONS NOTHING. It installs packages and
	// downloads the runner from github.com, so a subnet without a NAT gateway needs
	// this — and the failure without it is a machine that never stops itself,
	// reported as a timeout rather than as "no internet".
	if *public {
		cfg.AssignPublicIP = true
	}

	if *name == "" {
		*name = "billet-runner-" + time.Now().UTC().Format("20060102-150405")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// AN OWNER PER BUILD, not a constant. The owner tag is what separates one
	// deployment's compute from another's in a shared account, and a fixed string
	// would put every build anyone ever runs under the same identity — so the
	// recovery story this tag exists for ("find the builder that leaked") would
	// return other people's machines. The image name is already unique per account
	// and region, which is exactly the property needed.
	p, err := ec2.New("billet-ami-build-"+*name, cfg, ec2.WithLogger(log))
	if err != nil {
		return fmt.Errorf("configure the ec2 client: %w", err)
	}

	build, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	fmt.Printf("Building %s in %s from %s\n\n", *name, cfg.Region, *base)

	image, err := p.BuildImage(build, ec2.BuildSpec{
		BaseImage:     *base,
		InstanceType:  *shape,
		Arch:          *arch,
		RunnerVersion: *version,
		Name:          *name,
	})
	if err != nil {
		return err
	}

	fmt.Printf("\n%s\n\n", image)
	fmt.Printf("Put it in a tier:\n\n")
	fmt.Printf("  - label: your-label\n")
	fmt.Printf("    provider: ec2\n")
	fmt.Printf("    image: %s\n", image)
	fmt.Printf("    command: [/usr/local/bin/billet-runner]\n\n")
	fmt.Printf("An AMI id is REGION-SCOPED: this one only works in %s.\n", cfg.Region)

	return nil
}

// ec2ConfigFor assembles the client configuration a build needs.
//
// IT PREFERS THE OPERATOR'S OWN CONFIG, because a build wants the same subnet and
// security groups the jobs will use — an image built where nothing can reach
// GitHub is one that provisions fine and then fails at the only step that matters.
func ec2ConfigFor(path, region, subnet, group string) (config.EC2Config, error) {
	var cfg config.EC2Config

	loaded, err := config.Load(path)
	if err == nil && loaded.Node != nil && loaded.Node.EC2 != nil {
		cfg = *loaded.Node.EC2
	} else if region == "" || subnet == "" || group == "" {
		return cfg, fmt.Errorf("no ec2 configuration found in %s, so --region, --subnet and "+
			"--security-group are required: a builder has to be launched somewhere, and "+
			"billet does not fall back to a VPC's default group", path)
	}

	if region != "" {
		cfg.Region = region
	}

	if subnet != "" {
		cfg.SubnetID = subnet
	}

	if group != "" {
		cfg.SecurityGroupIDs = []string{group}
	}

	if cfg.Region == "" {
		return cfg, errors.New("a build needs a region")
	}

	// THE SHAPES A TIER MAY BUY ARE IRRELEVANT HERE and must not be empty, because
	// the client validates them. The builder's own shape is a flag.
	if len(cfg.InstanceTypes) == 0 {
		cfg.InstanceTypes = []config.EC2InstanceType{
			{Type: "c7i.xlarge", VCPU: 4, Memory: 8 * config.GiB},
		}
	}

	return cfg, nil
}
