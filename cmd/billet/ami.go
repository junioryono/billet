package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
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
	if len(args) == 0 {
		return errors.New("usage: billet ami <build|verify> [flags]")
	}

	switch args[0] {
	case "build":
		return cmdAMIBuild(ctx, args)
	case "verify":
		return cmdAMIVerify(ctx, args[1:])
	default:
		return errors.New("usage: billet ami <build|verify> [flags]")
	}
}

func cmdAMIBuild(ctx context.Context, args []string) error {
	fs := newFlagSet("billet ami build")
	cfgPath := addConfigFlag(fs)
	base := fs.String("base-image", "", "AMI to provision from (an EBS-backed Ubuntu 24.04 image)")
	shape := fs.String("instance-type", "c7i.xlarge", "shape of the BUILDER, not of your jobs")
	disk := fs.Int64("builder-disk", 0,
		"root volume for the BUILDER in GiB, which becomes the image's root (default: "+
			strconv.Itoa(ec2.DefaultBuilderDiskGiB)+")")
	arch := fs.String("arch", "x64", "runner build to install: x64 or arm64")
	version := fs.String("runner-version", runnerrelease.Pinned(), "actions/runner release")
	name := fs.String("name", "", "name for the produced AMI (default: billet-runner-<timestamp>)")
	caCert := fs.String("ca-cert", "",
		"PEM file of CA(s) to trust in the image's host store — the cache endpoint's issuer")
	payloadBucket := fs.String("payload-bucket", "",
		"REQUIRED. S3 bucket to stage the shared installers in, fetched by the builder "+
			"through a presigned URL. They no longer fit EC2's 16 KiB user data and "+
			"cannot be embedded")
	region := fs.String("region", "", "override the region from the config")
	subnet := fs.String("subnet", "", "override the subnet from the config")
	group := fs.String("security-group", "", "override the security group from the config")
	// THE WHOLE BUILD: provisioning, CreateImage, and waiting for the image to
	// become launchable. Timed on a real build of the full toolcache path:
	//
	//	3m23s  provisioning, including the whole toolcache
	//	9m20s  snapshot and registration
	//	12m43s total
	//
	// The toolcache is the cheap half -- EC2's network pulls several GiB in about
	// three minutes -- and the snapshot of a larger image is what grew. 20 minutes
	// would have fitted this run with seven to spare, which is not enough margin
	// for a slower region or a larger image, and the cost of a generous bound is
	// nothing: it is a deadline, not a wait.
	timeout := fs.Duration("timeout", 60*time.Minute, "how long to wait for the whole build")
	public := fs.Bool("public-ip", false,
		"give the builder a public address, for a subnet with no NAT gateway")
	// ON BY DEFAULT, AND THE DEFAULT IS THE WHOLE POINT. A gate nobody remembers to
	// pass is a gate that does not exist, and the failure it catches — an image
	// whose daemon reads its configuration at start, or whose toolcache a job
	// cannot reach — is invisible until somebody's workflow fails.
	//
	// AND IT IS WHAT WRITES THE CONTRACT TAG. --verify=false does not merely skip
	// the check, it skips the claim: the image comes back unstamped, and `billet
	// ami verify` is what stamps it later.
	verify := fs.Bool("verify", true,
		"boot the produced AMI and assert the contract on it before stamping it")
	verifyShape := fs.String("verify-instance-type", "",
		"shape the VERIFIER runs on (default: a small Nitro shape for the image's arch)")

	if err := parse(fs, args[1:]); err != nil {
		return err
	}

	if *base == "" {
		return errors.New("--base-image is required: billet does not guess which distribution " +
			"you want, because the provisioning script assumes Ubuntu 24.04 and a wrong guess fails " +
			"halfway through a build you paid for")
	}

	// REFUSED HERE, BEFORE A MACHINE IS BOUGHT. Without a bucket the provisioning
	// script cannot be rendered at all, and finding that out after launching a
	// builder costs an instance and the time to notice. The reason is arithmetic:
	// the installers and the pinned declaration are 17077 bytes compressed against
	// EC2's 16384-byte cap, and parity only grows.
	if *payloadBucket == "" {
		return errors.New("--payload-bucket is required: the shared installers and the " +
			"pinned declaration no longer compress into EC2's 16 KiB user data, so they " +
			"are staged in S3 and fetched by a bootstrap. Any bucket the builder's region " +
			"can reach will do; billet writes one object and deletes it when the build ends")
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

	// READ BEFORE THE BUILD STARTS, so a missing or unreadable file fails now
	// rather than after a paid builder is running. The bytes are validated as a CA
	// bundle inside BuildImage, before any instance launches.
	var caCertPEM string
	if *caCert != "" {
		data, err := os.ReadFile(*caCert)
		if err != nil {
			return fmt.Errorf("read --ca-cert %s: %w", *caCert, err)
		}

		caCertPEM = string(data)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// AN OWNER PER BUILD, not a constant. The owner tag is what separates one
	// deployment's compute from another's in a shared account, and a fixed string
	// would put every build anyone ever runs under the same identity — so the
	// recovery story this tag exists for ("find the builder that leaked") would
	// return other people's machines. The image name is already unique per account
	// and region, which is exactly the property needed.
	p, err := ec2.New(ec2.BuilderOwnerPrefix+*name, cfg, ec2.WithLogger(log))
	if err != nil {
		return fmt.Errorf("configure the ec2 client: %w", err)
	}

	build, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	fmt.Printf("Building %s in %s from %s\n\n", *name, cfg.Region, *base)

	image, err := p.BuildImage(build, ec2.BuildSpec{
		BaseImage:          *base,
		InstanceType:       *shape,
		BuilderDiskGiB:     *disk,
		Arch:               *arch,
		RunnerVersion:      *version,
		Name:               *name,
		CACertPEM:          caCertPEM,
		PayloadBucket:      *payloadBucket,
		Verify:             *verify,
		VerifyInstanceType: *verifyShape,
	})
	if err != nil {
		return err
	}

	fmt.Printf("\n%s\n\n", image)

	if !*verify {
		// SAID HERE RATHER THAN LEFT TO `billet check`. An unstamped image runs jobs
		// perfectly well; what it loses is the one fact that says billet has looked
		// at it, and an operator who is not told will read the check's "needs a
		// rebuild" as a broken build.
		fmt.Printf("It was NOT verified, so it carries no AMI contract tag and `billet check`\n")
		fmt.Printf("will report it as needing a rebuild. To boot it and stamp it:\n\n")
		fmt.Printf("  billet ami verify %s\n\n", image)
	}
	fmt.Printf("Put it in a tier:\n\n")
	fmt.Printf("  - label: your-label\n")
	fmt.Printf("    provider: ec2\n")
	fmt.Printf("    image: %s\n", image)
	fmt.Printf("    command: [/usr/local/bin/billet-runner]\n\n")
	fmt.Printf("An AMI id is REGION-SCOPED: this one only works in %s.\n", cfg.Region)

	return nil
}

// cmdAMIVerify boots an AMI that already exists and stamps it if it proves itself.
//
// THE SAME FUNCTION A BUILD CALLS, which is what makes a failed verification
// recoverable. billet speaks no DeregisterImage, so a build whose image fails
// leaves that image behind; without this the only way to retry is to buy another
// builder and run the whole thing again. It is also the answer for an image built
// before verification existed, and for one built with --verify=false.
func cmdAMIVerify(ctx context.Context, args []string) error {
	fs := newFlagSet("billet ami verify")
	cfgPath := addConfigFlag(fs)
	shape := fs.String("instance-type", "",
		"shape the verifier runs on (default: a small Nitro shape for the image's arch)")
	region := fs.String("region", "", "override the region from the config")
	subnet := fs.String("subnet", "", "override the subnet from the config")
	group := fs.String("security-group", "", "override the security group from the config")
	public := fs.Bool("public-ip", false,
		"give the verifier a public address, for a subnet with no NAT gateway")
	// THE CONSOLE LAGS AND THE VERIFIER DWELLS, so this is a boot plus several
	// minutes rather than a boot. It is a deadline and not a wait: billet
	// terminates the verifier the moment it has read a report.
	timeout := fs.Duration("timeout", 20*time.Minute,
		"how long to wait for the image to report on itself")

	// parseWithName, BECAUSE THE FLAGS COME AFTER THE ID IN EVERY EXAMPLE ANYBODY
	// WRITES. Go's flag package stops at the first positional, so a plain parse
	// leaves `--config` sitting in the argument list, silently ignored — against a
	// command whose config decides which region it launches in.
	image, err := parseWithName(fs, args)
	if err != nil {
		return err
	}

	if image == "" {
		return errors.New("usage: billet ami verify <ami-id> [flags]")
	}

	cfg, cfgErr := ec2ConfigFor(*cfgPath, *region, *subnet, *group)
	if cfgErr != nil {
		return cfgErr
	}

	if *public {
		cfg.AssignPublicIP = true
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// AN OWNER PER IMAGE, for the same reason a build takes one per name: the owner
	// tag is what separates this verification's compute from any deployment's, and
	// a fixed string would put every verification anyone ever runs under one
	// identity. The image id is already unique per account and region.
	p, err := ec2.New(ec2.BuilderOwnerPrefix+image, cfg, ec2.WithLogger(log))
	if err != nil {
		return fmt.Errorf("configure the ec2 client: %w", err)
	}

	run, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	fmt.Printf("Booting %s in %s and asking it to prove itself\n\n", image, cfg.Region)

	if err := p.VerifyImage(run, ec2.VerifySpec{Image: image, InstanceType: *shape}); err != nil {
		return err
	}

	fmt.Printf("\n%s is verified and stamped with the AMI contract it proved.\n", image)

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
