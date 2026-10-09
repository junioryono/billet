package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	opsimages "github.com/junioryono/billet/internal/ops/images"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/deploymentid"
	"github.com/junioryono/billet/internal/ops/cache"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/ec2"
	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/provider/tart"
	"github.com/junioryono/billet/internal/store/ceph"
	"github.com/junioryono/billet/internal/store/ebss3"
	"github.com/junioryono/billet/internal/wirecert"
)

// checkEC2Credentials proves this machine can act on its ec2 configuration.
//
// THE SAME DISTINCTION github.ReadPrivateKeyFile MAKES, one credential over: config
// validation proves the block is coherent, and coherence is not the question an
// operator running `billet check` is asking. A node whose credentials do not
// resolve validates perfectly and then fails on the first job of the day, with a
// 403 that names neither the missing environment variable nor the absent instance
// role.
//
// It costs a link-local request on a machine with no AWS environment variables,
// bounded by the metadata client's own short timeout, because the common failure
// is that this is not an EC2 instance at all.
func checkEC2Credentials(
	ctx context.Context, env cli.Env, cfg *config.Config, bundle *wirecert.Bundle,
	authorize, maintenanceProbe bool,
) error {
	// FIRST, before credentials resolve or anything dials AWS: during the
	// upgrade transaction's stopped-service window, an AWS or IMDS blip must
	// not read as a broken host and roll the upgrade back. The reachability
	// call used to survive the skip; it is a network call like the rest, and
	// the probe's job is the ledger and the config, not the cloud.
	if maintenanceProbe {
		fmt.Fprintf(env.Stdout, "aws      (all AWS checks skipped during maintenance)\n")

		return nil
	}

	ec2cfg := *cfg.Node.EC2
	ec2cfg.NodeName = cfg.Node.Name

	creds, err := awscreds.Default().Credentials(ctx)
	if err != nil {
		return fmt.Errorf("node.ec2: this host cannot resolve aws credentials, so it could not "+
			"launch anything: %w", err)
	}

	// RESOLVING IS NOT WORKING, so the credentials are then USED. One read-only
	// DescribeInstances proves the region and endpoint answer and that this
	// identity is permitted to ask — which is the difference between a config that
	// parses and a node that can do its job, and the same distinction
	// github.ReadPrivateKeyFile makes by parsing the key rather than stat-ing it.
	//
	// The same credentials that were just reported, so what is proved is what was
	// named rather than whatever a second resolution might return.
	if err := ec2.CheckReachable(ctx, ec2cfg,
		ec2.WithCredentials(awscreds.Static(creds))); err != nil {
		// MEASURED TRAP: a region that is not enabled on the account answers
		// AuthFailure with credential-shaped prose, so an operator whose key
		// works elsewhere would rotate credentials that were never wrong.
		if ec2.RegionMayBeDisabled(err) {
			return fmt.Errorf("node.ec2: the api in %s refused with a credential-shaped error, "+
				"which is ALSO exactly what a region that is not enabled on this account "+
				"returns. If these credentials work in another region, enable %s on the "+
				"account (or pick an enabled region) before rotating anything: %w",
				ec2cfg.Region, ec2cfg.Region, err)
		}

		return fmt.Errorf("node.ec2: credentials for %s resolved but could not call the ec2 api "+
			"in %s: %w", creds.AccessKeyID, ec2cfg.Region, err)
	}

	// THE ACCESS KEY ID AND NOTHING ELSE. It is an identifier rather than a
	// secret, and printing it is the difference between "billet is using the wrong
	// role" and an operator staring at a working config. The secret and the
	// session token are never rendered anywhere.
	// THE ACCOUNT'S OWN CEILING, once the credentials are known to work. It is
	// read with the SAME credentials that were just proved, so what it reports is
	// what this node will run as. Advisory; see reportQuotas.
	if p, err := ec2.New(deploymentid.Preflight, ec2cfg,
		ec2.WithCredentials(awscreds.Static(creds))); err == nil {
		reportQuotas(ctx, env, cfg, p)
	}

	fmt.Fprintf(env.Stdout, "aws      %s in %s, subnet %s, %d instance shape(s), credentials %s (can describe)\n",
		spotLabel(ec2cfg.Spot), ec2cfg.Region, ec2cfg.SubnetID, len(ec2cfg.InstanceTypes),
		creds.AccessKeyID)

	// SAID, BECAUSE THE CHECK IS NARROWER THAN IT LOOKS. A read-only call says
	// nothing about permission to LAUNCH, and an operator who reads "ok" and then
	// watches every job fail on an IAM denial has been misled by this line.
	fmt.Fprintf(env.Stdout, "         (describe only — launching also needs at least ec2:RunInstances, "+
		"ec2:TerminateInstances, ec2:CreateTags and ec2:DescribeImages, plus iam:PassRole "+
		"if node.ec2.instance_profile is set)\n")
	if ec2cfg.Spot {
		fmt.Fprintf(env.Stdout, "         spot warnings are consumed from interruption_queue_url; the role also "+
			"needs sqs:ReceiveMessage, sqs:DeleteMessage and sqs:GetQueueAttributes on that queue\n")
	}

	// SAID OUT LOUD RATHER THAN INFERRED FROM AN ABSENT KEY. A deployment that
	// expected to run fork pull requests on rented machines and finds them queuing
	// forever has no other way to see why.
	if len(ec2cfg.UntrustedSecurityGroupIDs) == 0 {
		fmt.Fprintf(env.Stdout, "         untrusted work will be refused: no untrusted_security_group_ids\n")
	}

	return ec2Preflight(ctx, env, cfg, ec2cfg, awscreds.Static(creds), bundle, authorize)
}

// ec2Preflight proves the subnet, security groups and tier AMIs a launch depends
// on actually exist and fit together, which CheckReachable's single
// DescribeInstances cannot. Read-only describes only: a dry-run launch is a
// write-shaped call a diagnostic should not make by default.
//
// A wrong subnet, a security group in another VPC, or a cache zone that does not
// match the subnet's are FATAL — they are misconfigurations a job would fail on.
// An AMI that does not resolve is a WARNING, because the staged flow deliberately
// writes a placeholder for `billet ami build` to replace, so a not-yet-built image
// is an expected intermediate state rather than a broken config.
func ec2Preflight(
	ctx context.Context, env cli.Env, cfg *config.Config, ec2cfg config.EC2Config, creds awscreds.Source,
	bundle *wirecert.Bundle, authorize bool,
) error {
	region, endpoint := ec2cfg.Region, ec2cfg.Endpoint

	subnet, err := ec2.DescribeSubnet(ctx, region, endpoint, creds, ec2cfg.SubnetID)
	if err != nil {
		return fmt.Errorf("node.ec2: %w", err)
	}

	fmt.Fprintf(env.Stdout, "subnet   %s in vpc %s, zone %s (%s)\n",
		subnet.SubnetID, subnet.VPCID, subnet.AvailabilityZone, subnet.State)

	// AN EBS CACHE VOLUME CANNOT ATTACH ACROSS ZONES, so the cache's zone must be
	// the subnet's. config proves the AZ is IN the region; only the API knows which
	// zone the subnet is actually in.
	if cfg.Node.EBSS3 != nil && subnet.AvailabilityZone != cfg.Node.EBSS3.AvailabilityZone {
		return fmt.Errorf("node.ec2.subnet_id %s is in zone %s, but node.ebs_s3.availability_zone "+
			"is %s; an EBS cache volume cannot attach to an instance in another zone",
			subnet.SubnetID, subnet.AvailabilityZone, cfg.Node.EBSS3.AvailabilityZone)
	}

	groupIDs := slices.Concat(ec2cfg.SecurityGroupIDs, ec2cfg.UntrustedSecurityGroupIDs)
	groups, err := ec2.DescribeSecurityGroups(ctx, region, endpoint, creds, groupIDs)
	if err != nil {
		return fmt.Errorf("node.ec2: %w", err)
	}

	for _, g := range groups {
		if g.VPCID != subnet.VPCID {
			return fmt.Errorf("node.ec2 security group %s is in vpc %s, but the subnet is in vpc "+
				"%s; a launch's security groups must be in the subnet's vpc", g.GroupID, g.VPCID,
				subnet.VPCID)
		}
	}

	fmt.Fprintf(env.Stdout, "groups   %d security group(s), all in vpc %s\n", len(groups), subnet.VPCID)

	amis := distinctEC2TierAMIs(cfg)

	var images []ec2.ImageInfo
	if len(amis) == 0 {
		// A FLEET NODE FILE HAS NO TIERS — they live on the control plane — so there
		// is no AMI to resolve here. Said, rather than passing silently as if the
		// images had been checked. The authorization dry-runs below still run: a
		// fleet node launches and tears down compute too.
		fmt.Fprintf(env.Stdout, "images   no tiers in this file, so no AMI to check "+
			"(a fleet node's tiers live on the control plane)\n")
	} else {
		resolved, err := ec2.DescribeImageStates(ctx, region, endpoint, creds, amis)
		if err != nil {
			return fmt.Errorf("node.ec2: %w", err)
		}
		images = resolved
	}

	for _, img := range images {
		switch {
		case !img.Found:
			reason := img.State
			if reason == "" {
				reason = "not found in this account or region"
			}
			fmt.Fprintf(env.Stdout, "image    %s is not resolvable yet (%s) — build it with `billet ami build` "+
				"and paste the id\n", img.ImageID, reason)
		case img.State != "available":
			fmt.Fprintf(env.Stdout, "image    %s is %s, not yet available\n", img.ImageID, img.State)
		case img.Contract < ec2.AMIContract:
			// A WARNING, NOT A REFUSAL. An image below the contract still runs jobs
			// correctly; what it loses is the Docker cache, silently — every job
			// re-pulls and nothing errors. Failing closed on a performance property
			// would strand a working fleet over a cold cache, so this names the
			// problem and the remedy and lets the deployment run.
			// QUOTED, BECAUSE THIS IS AN OPERATOR-EDITABLE TAG. EC2 permits newlines
			// and control characters in a tag value, and this line is printed into a
			// report an operator reads as billet's own output — so an unquoted value
			// can forge additional report lines. Provenance, not authentication:
			// quoting stops it lying about the REPORT, and nothing stops a tag
			// lying about itself.
			built := strconv.Quote(img.BuiltBy)
			if img.BuiltBy == "" {
				built = "a billet that did not record itself"
			}

			// EVERY GAP IT HAS, BECAUSE THEY ARE DIFFERENT PROBLEMS. Naming only the
			// oldest would send an operator looking for a Docker problem in an image
			// whose Docker is fine and whose toolcache is absent, and naming only
			// the newest would present a credential exposure as a performance note.
			var gaps []string
			if img.Contract < 1 {
				gaps = append(gaps, "its Docker image store may be the containerd one, "+
					"which makes the cache publish with no images in it so every job re-pulls")
			}
			if img.Contract < 2 {
				gaps = append(gaps, "it carries no toolcache, so every setup-node, setup-go, "+
					"setup-python and setup-java step downloads a runtime that a microVM "+
					"tier of this deployment already has baked in")
			}
			if img.Contract < 3 {
				gaps = append(gaps, "it starts the runner with the registration and the "+
					"cache token in an argument list, which any process in the instance "+
					"can read until the runner starts")
			}
			missing := strings.Join(gaps, "; ")

			fmt.Fprintf(env.Stdout, "image    %s meets AMI contract %d and this billet wants %d (built by "+
				"%s) — %s; rebuild with `billet ami build`\n",
				img.ImageID, img.Contract, ec2.AMIContract, built, missing)
		default:
			fmt.Fprintf(env.Stdout, "image    %s available (AMI contract %d)\n", img.ImageID, img.Contract)
		}
	}

	// THE SPOT QUEUE, READ WITHOUT CONSUMING: a deployment whose interruption
	// queue is missing or unreadable silently loses every two-minute warning,
	// so the probe is fatal — and it is GetQueueAttributes, never
	// ReceiveMessage, which would consume a real warning some node needed.
	if ec2cfg.Spot {
		arn, err := ec2.CheckInterruptionQueue(ctx, region, creds, ec2cfg.InterruptionQueueURL)
		switch {
		case err == nil:
			fmt.Fprintf(env.Stdout, "spot     interruption queue answers (%s)\n", arn)
		case ec2.QueueProbeInconclusive(err):
			// A fact about the CHECKING identity: a role provisioned before
			// sqs:GetQueueAttributes joined the generated grant refuses this
			// probe while consuming warnings perfectly well.
			fmt.Fprintf(env.Stdout, "spot     queue probe INCONCLUSIVE: %v\n", err)
			fmt.Fprintf(env.Stdout, "         (this identity may not read queue attributes — a role from "+
				"before the probe existed lacks sqs:GetQueueAttributes; regenerate it with "+
				"`billet init iam`)\n")
		default:
			return fmt.Errorf("node.ec2: %w", err)
		}
	}

	// The instance profile a trusted job would carry, three-valued: Missing is
	// a misconfiguration the launch WILL fail on; Unknown means this checking
	// identity may not read IAM, which says nothing about the profile and is
	// reported as exactly that.
	if ec2cfg.InstanceProfile != "" {
		verdict, reason, err := ec2.CheckInstanceProfile(ctx, region, iamEndpointOverride, creds,
			ec2cfg.InstanceProfile)
		switch {
		case err != nil:
			fmt.Fprintf(env.Stdout, "profile  %s UNVERIFIED: %v\n", ec2cfg.InstanceProfile, err)
		case verdict == ec2.ProfileFound:
			fmt.Fprintf(env.Stdout, "profile  %s exists\n", ec2cfg.InstanceProfile)
		case verdict == ec2.ProfileMissing:
			return fmt.Errorf("node.ec2.instance_profile %q does not exist in this account (%s); "+
				"a trusted job's launch will fail on it", ec2cfg.InstanceProfile, reason)
		default:
			fmt.Fprintf(env.Stdout, "profile  %s could not be checked (%s) — this says the CHECKING identity "+
				"may not read IAM, not that the profile is wrong. billet's own generated node "+
				"policy deliberately grants no iam:GetInstanceProfile; run check with operator "+
				"credentials to verify the profile\n", ec2cfg.InstanceProfile, reason)
		}
	}

	// The cache bucket, probed under the deployment's own prefix — the grant a
	// job will actually use. Skipped when no identity is minted yet, because
	// the prefix is derived from it.
	if cfg.Node.EBSS3 != nil {
		owner, err := app.AuthorizeOwner(cfg, bundle)
		switch {
		case err != nil:
			return fmt.Errorf("node.ebs_s3: resolve the deployment identity: %w", err)
		case owner == "":
			fmt.Fprintf(env.Stdout, "cache    bucket probe skipped: no deployment identity minted yet "+
				"(it is minted on the server's first start)\n")
		default:
			// The SAME namespace the runtime and decommission use, or the probe
			// reads a prefix no job ever touches.
			store, err := ebss3.New(*cfg.Node.EBSS3, app.CacheNamespace(owner, cfg.Node.Site), creds)
			if err != nil {
				return fmt.Errorf("node.ebs_s3: %w", err)
			}
			// THE VERDICT IS judgeCacheProbe's, not this switch's. What each
			// answer means is in cacheprobe.go, where a test can reach it.
			probeErr := store.CheckAccess(ctx)
			switch cache.JudgeProbe(probeErr) {
			case cache.ProbeAnswered:
				// A BUCKET THAT ANSWERS IS NOT A CACHE. Without a node.cache
				// listener nothing on this host ever reads or writes that prefix,
				// and this line read as though the cache were working — which is
				// most of why the whole shape was silent. The refusal is above;
				// this stops the report contradicting it three lines later.
				reachable := ""
				if cfg.Node.Cache == nil {
					reachable = " (but nothing on this node serves it — see the cache " +
						"line above)"
				}

				fmt.Fprintf(env.Stdout, "cache    bucket %s answers under this deployment's prefix%s\n",
					cfg.Node.EBSS3.Bucket, reachable)
			case cache.ProbeInconclusive:
				fmt.Fprintf(env.Stdout, "cache    bucket probe INCONCLUSIVE: %v\n", probeErr)
				fmt.Fprintf(env.Stdout, "         (a 403 here is EITHER a refused identity OR a healthy miss "+
					"under billet's minimal grant, whose prefix-conditioned ListBucket cannot "+
					"match a GetObject; a real job read will settle it)\n")
			case cache.ProbeFailed:
				return fmt.Errorf("node.ebs_s3: %w", probeErr)
			}
		}
	}

	if !authorize {
		fmt.Fprintf(env.Stdout, "         (launch authority not checked — pass --authorize to dry-run "+
			"RunInstances; a DryRun has no side effect)\n")

		return nil
	}

	return ec2Authorize(ctx, env, cfg, ec2cfg, creds, bundle, images)
}

// hasEC2Tier reports whether any tier in this file can run on the ec2 provider — a
// fleet node file has none, because its tiers live on the control plane.
func hasEC2Tier(cfg *config.Config) bool {
	for i := range cfg.Tiers {
		if cfg.Tiers[i].AcceptsProvider(config.ProviderEC2) {
			return true
		}
	}

	return false
}

func ec2Authorize(
	ctx context.Context, env cli.Env, cfg *config.Config, ec2cfg config.EC2Config, creds awscreds.Source,
	bundle *wirecert.Bundle, images []ec2.ImageInfo,
) error {
	// THE PROBE MUST TAG AS THIS DEPLOYMENT, or a per-deployment IAM policy — which
	// conditions ec2:CreateTags on the exact sh.billet.owner value — refuses the
	// launch's TagSpecification and the dry-run fails as UnauthorizedOperation
	// against the very policy `billet init iam` generates. The real launch tags with
	// the deployment id, so the probe must too. Peek, never mint: a diagnostic must
	// not create an identity.
	owner, err := app.AuthorizeOwner(cfg, bundle)
	if err != nil {
		return fmt.Errorf("node.ec2: %w", err)
	}

	if owner == "" {
		fmt.Fprintf(env.Stdout, "         (launch authority not checked — this deployment's identity is not "+
			"known here yet, so a dry-run cannot tag as a per-deployment IAM policy requires; "+
			"enroll this node, or run `billet server` once to mint it, then re-run with "+
			"--authorize)\n")

		return nil
	}

	p, err := ec2.New(owner, ec2cfg, ec2.WithCredentials(creds))
	if err != nil {
		return fmt.Errorf("node.ec2: %w", err)
	}

	available := make(map[string]bool)
	for _, img := range images {
		if img.Found && img.State == "available" {
			available[img.ImageID] = true
		}
	}

	fatal := false
	verdicts := 0 // dry-runs that reached a permission answer (authorized or not)
	probed := 0
	skipped := 0      // ec2 tiers deliberately not probed (untrusted with no network)
	unresolvable := 0 // ec2 tiers whose AMI is not resolvable yet
	seen := make(map[string]bool)

	// Dry-run every launchable combination the config expresses: each ec2 tier's
	// AMI, on the network its trust selects (a trusted launch also exercises
	// iam:PassRole when an instance profile is configured), at the tier's disk, for
	// every declared shape that fits the tier — the same fallback set a real launch
	// walks. Identical requests are asked once.
	for i := range cfg.Tiers {
		t := &cfg.Tiers[i]
		if !t.AcceptsProvider(config.ProviderEC2) {
			continue
		}

		trust := provider.TrustUntrusted
		if t.Trust.Effective() == config.WorkloadTrusted {
			trust = provider.TrustTrusted
		}

		// BOTH blockers are evaluated independently, because ONE tier can carry both
		// — an unresolvable AMI AND an untrusted trust with no untrusted network. If
		// the AMI check short-circuited first, that tier would count only as an AMI
		// problem and the summary would suppress the network remedy the operator also
		// needs. Each blocker prints its own line and bumps its own counter.
		amiBad := !available[t.ImageFor(config.ProviderEC2)]

		// An untrusted launch the node itself would REFUSE (no untrusted network) is
		// not a launch to prove: probing it would put the request on the VPC default
		// security group, so AWS answers about a launch billet never sends — a
		// misleading authorized, or a fatal false NOT-AUTHORIZED against a policy
		// scoped to the untrusted groups. Mirror ec2.Accepts.
		netBad := trust == provider.TrustUntrusted && len(ec2cfg.UntrustedSecurityGroupIDs) == 0

		if netBad {
			fmt.Fprintf(env.Stdout, "authz    %s runs untrusted work but node.ec2.untrusted_security_group_ids "+
				"is empty — the node refuses it, so its launch is not probed\n", t.Label)
			skipped++
		}
		if amiBad {
			unresolvable++ // the image probes above already named it as unresolvable
		}
		if amiBad || netBad {
			continue
		}

		ami := t.ImageFor(config.ProviderEC2)

		for _, shape := range ec2cfg.InstanceTypes {
			if shape.VCPU < t.VCPU || shape.Memory < t.Memory {
				continue // the launch would never pick a shape too small for the tier
			}

			key := ami + "|" + trustName(trust) + "|" + shape.Type + "|" +
				strconv.FormatInt(int64(t.Disk), 10)
			if seen[key] {
				continue
			}
			seen[key] = true

			probed++

			res, err := p.DryRunLaunch(ctx, ami, trust, shape, t.Disk)
			if err != nil {
				return fmt.Errorf("node.ec2: dry-run launch %s on %s: %w", t.Label, shape.Type, err)
			}

			hard, verdict := reportAuthz(env,
				fmt.Sprintf("launch %s on %s (%s)", t.Label, shape.Type, trustName(trust)), res)
			fatal = hard || fatal
			if verdict {
				verdicts++
			}
		}
	}

	switch {
	case probed == 0 && !hasEC2Tier(cfg):
		// A fleet node file: its tiers and AMIs live on the control plane, so there
		// is nothing here to dry-run. Launch authority is genuinely unproven — said,
		// not misdirected to `billet ami build`.
		fmt.Fprintf(env.Stdout, "         (launch authority not checked — this file declares no ec2 tiers, so "+
			"there is no launch to dry-run; a fleet node's tiers live on the control plane)\n")
	case probed == 0 && skipped > 0 && unresolvable == 0:
		// EVERY ec2 tier was SKIPPED (untrusted with no untrusted network), none
		// blocked by an AMI — the skip lines above already said why, so do not
		// misdirect to `billet ami build`.
		fmt.Fprintf(env.Stdout, "         (launch authority not checked — every ec2 tier was skipped above; "+
			"give them an untrusted network to probe)\n")
	case probed == 0 && skipped > 0:
		// A MIX: some tiers skipped for a missing network, some for an unresolvable
		// AMI. Name BOTH remedies rather than misattributing one cause to all.
		fmt.Fprintf(env.Stdout, "         (launch authority not checked — no ec2 tier could be dry-run: build "+
			"the AMIs named above (`billet ami build`) and give untrusted tiers a network)\n")
	case probed == 0:
		fmt.Fprintf(env.Stdout, "authz    no ec2 tier has a resolvable AMI to dry-run a launch with; build one "+
			"with `billet ami build`\n")
	case verdicts == 0:
		// Every dry-run was refused before AWS reached a permission answer (a shape
		// not offered in the zone, say), so launch authority is still unproven — said
		// rather than passing silently as if it had been checked.
		fmt.Fprintf(env.Stdout, "         (launch authority still unproven — every dry-run was refused for a "+
			"non-permission reason before AWS reached an authorization verdict)\n")
	}

	fmt.Fprintf(env.Stdout, "         (ec2:TerminateInstances cannot be dry-run — it validates the instance id "+
		"before the permission verdict, so it needs a real instance; grant it alongside "+
		"RunInstances)\n")

	if fatal {
		return errors.New("node.ec2: the ec2 role is NOT authorized for a launch it will need, " +
			"so jobs would be admitted and then fail — the runtime IAM policy is incomplete. " +
			"`billet init iam` prints exactly what it needs")
	}

	return nil
}

// trustName is the human word for a trust class, for the authz report lines.
func trustName(trust provider.TrustClass) string {
	if trust == provider.TrustTrusted {
		return "trusted"
	}

	return "untrusted"
}

// reportAuthz prints one dry-run outcome and returns (hard, verdict): whether it is
// a hard failure, and whether it reached a permission verdict at all (authorized or
// not).
func reportAuthz(env cli.Env, what string, res ec2.DryRunResult) (bool, bool) {
	switch res.Outcome {
	case ec2.DryRunUnauthorized:
		fmt.Fprintf(env.Stdout, "authz    %s: NOT AUTHORIZED (%s)\n", what, res.Code)

		return true, true
	case ec2.DryRunAuthorized:
		fmt.Fprintf(env.Stdout, "authz    %s: authorized\n", what)

		return false, true
	default:
		// DryRunInconclusive: AWS refused before it reached the permission answer —
		// a shape not offered in the zone, an invalid parameter. NOT a permission
		// verdict, so it is neither a pass nor a fail, and the caller reports that
		// nothing was proved if every probe landed here.
		fmt.Fprintf(env.Stdout, "authz    %s: inconclusive (%s — refused before a permission verdict)\n",
			what, res.Code)

		return false, false
	}
}

// distinctEC2TierAMIs is the set of AMIs the ec2 tiers in this file name, in
// first-seen order.
func distinctEC2TierAMIs(cfg *config.Config) []string {
	seen := make(map[string]bool)

	var amis []string
	for i := range cfg.Tiers {
		t := &cfg.Tiers[i]
		if !t.AcceptsProvider(config.ProviderEC2) {
			continue
		}

		ami := t.ImageFor(config.ProviderEC2)
		if ami == "" || seen[ami] {
			continue
		}

		seen[ami] = true
		amis = append(amis, ami)
	}

	return amis
}

// checkFirecrackerHost proves this machine can act on its microVM configuration.
//
// THE SAME DISTINCTION github.ReadPrivateKeyFile AND checkEC2Credentials MAKE. Config
// validation proves the block is coherent; it cannot prove firecracker is
// installed, that /dev/kvm can be opened, that the jail account exists or that the
// bridge does. A node that is wrong about any of those validates perfectly and
// then fails on the first job of the day.
//
// FATAL, because only a firecracker node reaches here, so this file describes a
// machine that is meant to run jobs and cannot. Reporting it and exiting zero would
// make `billet check` say a host is fine when nothing on it can launch.
//
// AND WITH node.monitoring, a host that cannot prove the jailer can account each
// microVM's memory and io is refused, as the node itself refuses to start on it.
// opts are added after the node's own, for a check of a staged host.
func checkFirecrackerHost(ctx context.Context, env cli.Env, cfg *config.Config, opts ...firecracker.Option) error {
	// A PROVIDER BUILT PURELY TO ASK, so the preflight exercises the constructor an
	// operator's node will use — including the two rules that are easiest to get
	// wrong and invisible afterwards: which directory the jailer will name after
	// this binary, and whether a socket under it would fit in a unix address.
	//
	// The storage is not consulted here; checkCephCluster does that on its own, and
	// a nil disk would make this refuse for the wrong reason.
	p, err := firecracker.New(deploymentid.Preflight, *cfg.Node.Firecracker, noRootDisk{},
		append(app.FirecrackerOptions(cfg), opts...)...)
	if err != nil {
		return err
	}

	// BEFORE THE HOST'S OWN CHECKS, because New has already read the answer and
	// nothing below changes it: a node with node.monitoring refuses to start on
	// this host whatever else is true of it.
	if err := p.RequireJobAccounting(); err != nil {
		return err
	}

	report, err := p.CheckHost(ctx, needsFirecrackerRootResize(cfg))
	if err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "microvm  %s, %s\n", report.Firecracker, report.Jailer)
	fmt.Fprintf(env.Stdout, "         jails in %s, one uid per guest from %d (%d available)\n",
		report.JailDir, report.JailUIDMin, report.JailUIDCount)

	untrusted := "untrusted work will be refused: no untrusted_bridge"
	if report.UntrustedBridge != "" {
		untrusted = "untrusted work runs on " + report.UntrustedBridge
	}

	fmt.Fprintf(env.Stdout, "         guests on %s; %s\n", report.Bridge, untrusted)
	fmt.Fprintf(env.Stdout, "         %s\n", report.Accounting.Summary())

	// SAID, BECAUSE THE CHECK IS NARROWER THAN IT LOOKS. Opening /dev/kvm says
	// nothing about the jailer's ability to chroot, mknod or place a cgroup, all of
	// which need root — and an operator who reads "ok" and then watches every
	// launch fail has been misled by this line.
	fmt.Fprintf(env.Stdout, "         (read only — launching also needs root, to chroot, to create the root "+
		"disk's device node inside the jail, and to attach a tap to the bridge)\n")

	return nil
}

// needsFirecrackerRootResize reports whether this deployment can send a tier
// with an explicit root capacity to this backend. A zero-disk catalogue keeps the
// image default and must not acquire resize2fs as a preflight dependency.
func needsFirecrackerRootResize(cfg *config.Config) bool {
	for i := range cfg.Tiers {
		tier := &cfg.Tiers[i]
		if tier.Disk > 0 && tier.AcceptsProvider(config.ProviderFirecracker) {
			return true
		}
	}

	return false
}

// checkTartHost proves this machine can act as a tart node.
//
// FATAL for the firecracker preflight's reason: only a tart node reaches here,
// so a failure describes a machine that is meant to run jobs and cannot, and
// reporting it while exiting zero would make `billet check` say a host is fine
// when nothing on it can launch.
func checkTartHost(ctx context.Context, env cli.Env, cfg *config.Config) error {
	var tartCfg config.TartConfig
	if cfg.Node.Tart != nil {
		tartCfg = *cfg.Node.Tart
	}

	// Normalized so what is REPORTED is what a launch would use. Load already
	// did this for a config read from a file; doing it again costs nothing and
	// keeps the report honest for one built any other way.
	tartCfg.Normalize()

	p, err := tart.New(deploymentid.Preflight, tart.WithConfig(tartCfg))
	if err != nil {
		return err
	}

	report, err := p.CheckHost(ctx)
	if err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "tart     %s, %d local VMs\n", report.Version, report.VMs)

	// SAID EVERY TIME, like softnet's grant. Which billets serialize against each
	// other is decided by TART_HOME, so two processes that disagree about the
	// store take different locks and exclude nothing — printing the path is what
	// makes that a comparison an operator can make rather than an inference.
	//
	// AND AN UNPROVED LOCK IS NOT REPORTED AS A PROVED ONE. A held lock is what a
	// busy node looks like and also what a wedged store looks like, so the line
	// says which of the two billet established.
	if report.StoreLockProved {
		fmt.Fprintf(env.Stdout, "         store    %s serializes every lease-name rename and delete\n",
			report.StoreLock)
	} else {
		fmt.Fprintf(env.Stdout, "         store    %s NOT PROVED: %s\n",
			report.StoreLock, report.StoreLockWhy)
	}

	// SAID EVERY TIME, not only when broken. softnet's grant is host
	// provisioning that survives nothing — `brew upgrade softnet` replaces the
	// binary and resets its ownership — so an operator has to be able to see its
	// state on an ordinary check rather than discover it on the first untrusted
	// job of the day.
	switch {
	case report.Softnet.GrantConfigured && report.Softnet.Why == "":
		// NOT "isolation available": the check proves a setuid bit and an owner,
		// which says softnet could start and nothing about what its policy then
		// permits. Only a probe from inside a guest can say that.
		fmt.Fprintf(env.Stdout, "         softnet  %s: setuid-root grant configured\n", report.Softnet.Path)
	case report.Softnet.GrantConfigured:
		fmt.Fprintf(env.Stdout, "         softnet  %s: grant configured, but it %s\n",
			report.Softnet.Path, report.Softnet.Why)
	case report.Softnet.Path != "":
		fmt.Fprintf(env.Stdout, "         softnet  %s %s\n", report.Softnet.Path, report.Softnet.Why)
	default:
		fmt.Fprintf(env.Stdout, "         softnet  %s\n", report.Softnet.Why)
	}

	if report.Softnet.Path != "" && !report.Softnet.HostBlockSupported {
		fmt.Fprintf(env.Stdout, "         softnet  %s\n", report.Softnet.HostBlockWhy)
	}

	// WHAT THIS NODE WILL DO WITH A FORK'S PULL REQUEST, in one line, because the
	// answer is a decision the operator made in config and not a property of the
	// host — and a node that silently ran untrusted work on the default NAT
	// would look exactly like one that refused it.
	switch {
	case tartCfg.UntrustedIsolation == "":
		fmt.Fprintf(env.Stdout, "         untrusted work will be refused: node.tart.untrusted_isolation "+
			"is not set, and tart's default NAT reaches the host\n")

	case !report.Softnet.GrantConfigured:
		// FATAL, and this is the case the whole block exists for: the config
		// says this node accepts untrusted work, and the host cannot confine
		// it. Reporting it and exiting zero is how a deployment believes it has
		// isolation it does not have.
		return fmt.Errorf("node.tart.untrusted_isolation is %q, so this node offers to run "+
			"untrusted work, but softnet %s — every untrusted launch would fail, and the "+
			"promise in the config is one this host cannot keep",
			tartCfg.UntrustedIsolation, report.Softnet.Why)

	case !report.Softnet.HostBlockSupported:
		// FATAL FOR THE SAME REASON: every untrusted launch passes
		// --net-softnet-block=@host, so a softnet that refuses the alias fails
		// each one, and a check that passed would be a promise the host breaks.
		return fmt.Errorf("node.tart.untrusted_isolation is %q, but softnet %s",
			tartCfg.UntrustedIsolation, report.Softnet.HostBlockWhy)

	default:
		fmt.Fprintf(env.Stdout, "         untrusted work runs under %s, resolving through %s\n",
			tartCfg.UntrustedIsolation, strings.Join(tartCfg.UntrustedDNS, ", "))
	}

	// THE IMAGES THIS NODE'S TIERS NAME, BY NAME. A launch REFUSES an image that
	// is not present rather than fetching one — tens of gigabytes must not travel
	// the node's single command queue — so "not pulled" is a tier that cannot run
	// a job, and it is worth saying before the first job rather than as its
	// failure. `billet images pull` is what fetches them.
	var missing []string

	// The SAME selection `billet images pull` makes, identity resolution
	// included — a check that listed different images from the command that
	// fetches them would send an operator in a circle.
	tierImages, err := opsimages.TartTierImages(cfg)
	if err != nil {
		return err
	}

	for _, image := range tierImages {
		if p.Pulled(ctx, image) {
			fmt.Fprintf(env.Stdout, "image    %-56s pulled\n", image)

			continue
		}

		missing = append(missing, image)

		fmt.Fprintf(env.Stdout, "image    %-56s NOT pulled; every job on its tier will fail to launch\n", image)
	}

	if len(missing) > 0 {
		fmt.Fprintf(env.Stdout, "         fetch them with `billet images pull` (each is tens of GB)\n")
	}

	// SAID, BECAUSE THE CHECK IS STILL NARROWER THAN IT LOOKS: a pulled image is
	// not proof that its guest carries the tart guest agent the registration
	// delivery needs, nor that a macOS guest slot is free under Apple's two-guest
	// licence. Both surface at launch, not here.
	fmt.Fprintf(env.Stdout, "         (read only — launching also needs the tart guest agent inside the "+
		"image and a free macOS guest slot under Apple's two-VM licence)\n")

	return nil
}

// noRootDisk stands in for the storage a preflight does not use.
//
// The provider refuses a nil one — every guest boots from a clone, so a nil
// interface would panic on the first job — and `billet check` proves the cluster
// separately, through the ceph client, where the diagnostic is about storage rather
// than about microVMs.
type noRootDisk struct{}

func (noRootDisk) ResolveGeneration(_ context.Context, image, _ string) (string, error) {
	return image, nil
}

func (noRootDisk) CloneRoot(context.Context, string, string, config.ByteSize) (string, error) {
	return "", errors.New("billet: the preflight does not clone a root disk")
}

func (noRootDisk) DiscardRoot(context.Context, string) error { return nil }

// KernelFor answers "nothing recorded", which is the truthful answer from a node
// with no cluster to have recorded anything in — and the caller treats it as the
// fallback case rather than an error.
func (noRootDisk) KernelFor(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}

// GenerationGone is false: a node with no cluster has no generations to lose, and
// answering true would have the launch re-resolve an alias forever.
func (noRootDisk) GenerationGone(error) bool { return false }

// checkCephCluster proves this machine can act on its storage configuration.
//
// THE SAME DISTINCTION github.ReadPrivateKeyFile AND checkEC2Credentials MAKE, one backend
// over. Config validation proves the block is coherent; it cannot prove the
// monitors answer, the keyring authenticates, or the pools were ever created. A
// node that is wrong about any of those validates perfectly and then fails on the
// first job of the day, with a librados error naming none of them.
//
// A MISSING rbd IS FATAL, and the reason is which configs reach here. Only a
// firecracker node may carry a ceph block, so this file describes a machine that
// is meant to run jobs — and one without the client package cannot map a single
// volume. A control plane is not affected: with no node section there is nothing
// to check. Reporting it and exiting zero would make `billet check` say a host is
// fine when nothing on it can launch.
func checkCephCluster(ctx context.Context, env cli.Env, cfg *config.CephConfig) error {
	client, err := ceph.New(*cfg)
	if err != nil {
		// WHAT THE CONFIG NAMES, and nothing the sentinel already says. Every
		// wrapper renders the message beneath it, so repeating the remedy here put
		// "install ceph-common" on the operator's terminal twice in one sentence.
		if errors.Is(err, ceph.ErrNoClient) {
			return fmt.Errorf("node.ceph names %s and %s, so this host is meant to run jobs and "+
				"cannot map a volume: %w", cfg.ImagePool, cfg.CachePool, err)
		}

		return err
	}

	// THE REPORT IS PRINTED EVEN WHEN THE CHECK FAILS, when there is one. A cluster
	// billet reached and then refused has told the operator something — which pools
	// it found, how they are replicated — and throwing that away because the last
	// question answered badly makes the diagnostic harder to act on, not easier.
	report, err := client.CheckReachable(ctx)
	if report.User != "" {
		printCephReport(env, cfg, report)
	}

	if err != nil {
		if errors.Is(err, ceph.ErrCloneV1) {
			return fmt.Errorf("node.ceph: %w", err)
		}

		// HONEST ABOUT WHAT FAILED, which is not always the pools. This wrapper said
		// "could not read the pools" for every failure, including ones where both
		// pools listed perfectly and it was a later cluster fact that billet could
		// not make sense of — telling an operator to go and look at the one thing
		// that worked. The inner error already names the step; this one says only
		// what the CLI knows, which is that the preflight did not finish.
		return fmt.Errorf("node.ceph: the ceph preflight did not complete, so billet cannot say "+
			"this host could launch anything: %w", err)
	}

	return nil
}

// printCephReport puts what the cluster said on the operator's terminal.
func printCephReport(env cli.Env, cfg *config.CephConfig, report ceph.Report) {
	fmt.Fprintf(env.Stdout, "ceph     client.%s -> %s\n", report.User, cfg.ConfPathOrDefault())

	for _, p := range report.Pools {
		// THE REPLICATION IS SHOWN RATHER THAN JUDGED, with one exception below.
		// How many copies a pool keeps is the operator's decision and billet has no
		// standing to refuse it — but it is invisible from the config file, and an
		// operator who believes their golden images are mirrored deserves to find
		// out here rather than after a drive dies.
		replication := "replication unknown"
		if p.Size > 0 {
			replication = fmt.Sprintf("size %d, min_size %d", p.Size, p.MinSize)
		}

		// THE CLONE FORMAT ONLY WHEN SOMEBODY HAS OVERRIDDEN IT. `auto` is the
		// default and the common case, and a column that says `auto` on every line
		// of every healthy deployment is a column nobody reads.
		override := ""
		if p.CloneFormat != "" && p.CloneFormat != "auto" {
			override = fmt.Sprintf("  clone format forced to %s", p.CloneFormat)
		}

		fmt.Fprintf(env.Stdout, "         %-16s %3d image(s)  %-24s %s%s\n",
			p.Name, p.Images, replication, p.Purpose, override)
	}

	for _, p := range report.Pools {
		if p.Size == 1 {
			fmt.Fprintf(env.Stdout, "         %s keeps ONE copy: a single drive failure loses everything in "+
				"it\n", p.Name)
		}
	}

	if report.CloneV2 {
		// BOTH SETTINGS, because either can be the one making it true. A cluster on
		// luminous with rbd_default_clone_format forced to 2 clones the new way, and
		// printing only the release would have an operator reading "luminous" beside
		// "clone v2" with no way to see why they agree.
		fmt.Fprintf(env.Stdout, "         clone v2 (require-min-compat-client %s), so a cache generation can "+
			"be reclaimed while a job still holds a clone of it\n", report.MinCompatClient)
	}

	// SAID, BECAUSE THE CHECK IS NARROWER THAN IT LOOKS. Listing a pool proves the
	// monitors answered and the keyring authenticated; it proves nothing about
	// permission to create, clone or remove an image, which is what a launch does.
	fmt.Fprintf(env.Stdout, "         (read only — launching also needs create, clone, snapshot and remove in "+
		"both pools; `ceph auth get-or-create client.<user> mon 'profile rbd' osd 'profile rbd "+
		"pool=<images>, profile rbd pool=<cache>'` grants exactly that)\n")
}

// iamEndpointOverride points the instance-profile probe at a fake for tests —
// production always derives the partition-global IAM endpoint from the region.
var iamEndpointOverride = ""
