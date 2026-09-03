package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider/codebuild"
	"github.com/junioryono/billet/internal/wirecert"
)

// printCodeBuildCeilings puts the two limits a CodeBuild tier inherits, and what
// they cost, in front of an operator BEFORE work is admitted.
//
// The backend's acceptance turns on this: the 36-hour build cap and the 8-hour
// queued cap are the service's and billet cannot lift either, so an operator has to
// see them while they are still deciding whether to enable the tier. The
// alternative is meeting the first one as a build that died at hour 36 and the
// second as an unexplained red build on a busy fleet.
//
// IT NEEDS NO CREDENTIALS AND NO NETWORK, deliberately: these are facts about the
// configuration, and a `billet check` on a laptop with no AWS access must still
// report them. What needs the network is the project and the fleet, which
// checkCodeBuildLive asks about. The concurrency QUOTA is not reported by either:
// reading Service Quotas is the general cloud preflight's job, and this backend
// consumes that rather than growing a CodeBuild-specific probe beside it.
func printCodeBuildCeilings(cfg *config.Config) {
	c := codebuild.CeilingsFor(*cfg.Node.CodeBuild)

	shape := "on-demand compute"
	if c.Reserved {
		shape = "a reserved-capacity fleet"
	}

	fmt.Printf("codebuild %s in %s, %s\n",
		cfg.Node.CodeBuild.EnvironmentType, cfg.Node.CodeBuild.Region, shape)

	// BOTH CEILINGS, AND WHOSE THEY ARE. Saying "36 hours" without saying it is
	// CodeBuild's reads as billet imposing a limit, which is the opposite of true.
	fmt.Printf("  ceiling  a build is capped at %s and a queued build FAILS after %s — "+
		"both are CodeBuild's own limits, which billet cannot lift\n",
		minutesText(c.BuildMinutes), minutesText(c.QueuedMinutes))

	// THE THIRD CEILING IS THE ACCOUNT'S, not this node's, and it is the one that
	// turns a burst into failed jobs: past it StartBuild is refused outright, billet
	// reports a conclusive launch failure, and GitHub requeues at most three times.
	fmt.Printf("  ceiling  the account queues at most %d builds across every project — a "+
		"burst beyond the concurrency quota plus %d is refused at launch, so size "+
		"node.max_vcpu so this node cannot escrow more than that\n",
		config.CodeBuildAccountQueuedBuilds, config.CodeBuildAccountQueuedBuilds)

	if c.BuildMinutes >= c.ServiceBuildMinutes {
		fmt.Printf("  ceiling  that is the service maximum; work that can exceed %s belongs on "+
			"owned EC2 or Mac capacity, where billet imposes no job limit\n",
			minutesText(c.ServiceBuildMinutes))
	}

	// THE INVENTORY COST IS A CONSEQUENCE OF THE DECLARED CEILING, and worth
	// reporting because it is the one thing a shorter ceiling buys: CodeBuild cannot
	// list only active builds, so reconciliation walks recent history and stops once
	// every build it sees is older than the service could still be running.
	fmt.Printf("  inventory reconciliation walks %s of build history per sweep, which is what "+
		"the declared ceilings above imply; a tighter build_timeout_minutes makes it cheaper\n",
		minutesText(c.InventoryWindowMinutes))

	// UNTRUSTED WORK IS REFUSED, and it is said here rather than discovered at the
	// first fork pull request.
	fmt.Println("  trust    untrusted tiers are REFUSED on this backend: AWS documents a " +
		"reserved-capacity instance as staying alive between builds and sharing cached data " +
		"with other projects in the account. Run untrusted tiers on firecracker or ec2")

	if c.MacOS {
		fmt.Println("  macos    reserved capacity carries an initial per-instance charge and " +
			"bills while provisioned, whether or not anything is building; delete a fleet you " +
			"are not using")
	}
}

// judgeCodeBuildFleetConcurrency compares the macOS cap this deployment declared for
// the node with what its fleet can run at once.
//
// THREE CONDITIONS BEFORE ANY VERDICT, each of which turns a correct deployment into
// a refused one if dropped. The node must be MAC_ARM: a Linux fleet runs no macOS
// build, and a macos_vm_limit is legal to declare on a node no macOS tier targets.
// The config must DECLARE a policy for this node: a node-only file carries none.
// And that policy must set the limit EXPLICITLY: the fallback MacOSLimit answers
// Apple's per-host allowance, which describes a Mac somebody owns and not a fleet
// AWS operates. What is judged is the same number every macOS tier pinned to the
// node was validated against — measured, docs/aws-acceptance.md.
func judgeCodeBuildFleetConcurrency(cfg *config.Config, fleet codebuild.FleetReport) ([]string, []string) {
	if cfg.Node == nil || cfg.Node.CodeBuild == nil ||
		cfg.Node.CodeBuild.EnvironmentType != config.CodeBuildMacARM {
		return nil, nil
	}

	p, declared := cfg.NodePolicyFor(cfg.Node.Name)
	if !declared || p.MacOSVMLimit == nil {
		return nil, nil
	}

	return fleet.ConcurrencyProblems(p.MacOSLimit())
}

// minutesText renders a minute count the way an operator thinks about it.
func minutesText(minutes int) string {
	switch {
	case minutes <= 0:
		return "an unstated period"
	case minutes%60 == 0 && minutes >= 60:
		return fmt.Sprintf("%dh", minutes/60)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

// checkDeployment is the identity `billet check` compares a project's owner tag
// against, and it is a THREE-VALUED answer wearing a string.
//
// THE CERTIFICATE IS THE AUTHORITY on which deployment a node belongs to — a node
// JOINS one rather than founding one — so that is what a project's tag has to be
// compared with. A loopback deployment has no certificate at all, and there the
// comparison cannot be made.
//
// SAYING "I COULD NOT TELL" IS THE POINT, and it is why this does not fall back to
// the placeholder `deploymentForCheck` the firecracker and tart preflights use.
// Those construct a provider only to ask it about the HOST, where the identity marks
// nothing; here the identity is the thing being compared, and a placeholder would
// report a project as belonging to a deployment whose name billet invented. The
// second return says which case this is, so the caller reports rather than concludes.
func checkDeployment(bundle *wirecert.Bundle) (string, bool) {
	if bundle == nil {
		return "", false
	}

	deployment, err := bundle.Deployment()
	if err != nil || deployment == "" {
		return "", false
	}

	return deployment, true
}

// checkCodeBuildLive asks AWS what the configured project and fleet actually are.
//
// CONFIG VALIDATION PROVES THE BLOCK IS COHERENT. It cannot prove this machine can
// act on it, and the difference is a deployment that validates and then fails on the
// first job of the day — which is exactly the argument the ec2 branch of `billet
// check` already makes.
//
// EXACTLY ONE FINDING IS FATAL: a competing WORKFLOW_JOB_QUEUED webhook, which means
// CodeBuild's own runner integration is acquiring jobs as well as billet. Two
// schedulers on one job produce duplicate runners, and that reads as GitHub
// misbehaving rather than as a configuration mistake. Everything else is reported,
// because billet overrides it on every launch and refusing a working deployment over
// a mismatch billet already corrects is the failure ADR-005 names.
func checkCodeBuildLive(ctx context.Context, cfg *config.Config, bundle *wirecert.Bundle) error {
	deployment, known := checkDeployment(bundle)

	// THE PROVIDER STILL NEEDS A NON-EMPTY IDENTITY — New refuses an empty one,
	// correctly, because List feeds a loop that stops builds — so an unknown
	// deployment gets the placeholder AND the ownership comparison is skipped rather
	// than made against it.
	owner := deployment
	if !known {
		owner = deploymentForCheck
	}

	p, err := codebuild.New(owner, *cfg.Node.CodeBuild,
		codebuild.WithCredentials(awsCredentials()))
	if err != nil {
		return err
	}

	// THE ACCOUNT'S OWN CEILINGS, BEFORE THE PROJECT. They are what decides
	// whether a tier can use the capacity it is about to advertise — the
	// concurrency limit defaults to ONE per compute type — so an operator reading
	// this output top to bottom meets the binding constraint first. Advisory
	// throughout; see reportQuotas.
	reportQuotas(ctx, cfg, p)

	project, err := p.DescribeProject(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("  project  %s (%s), source %s\n",
		project.Name, project.EnvironmentType, project.SourceType)

	var (
		fatal    []string
		warnings []string
	)

	if known {
		fatal, warnings = project.Problems(deployment)
	} else {
		// REPORTED, NOT CONCLUDED. Without a certificate billet cannot say which
		// deployment this host belongs to, so it cannot say whether the project's
		// owner tag is right — and asserting either way would be inventing an
		// answer. The webhook finding does not depend on the identity, so it is
		// still made.
		fmt.Println("  note     this host has no node certificate, so billet cannot tell which " +
			"deployment it belongs to and did not check the project's owner tag")

		if project.RunnerWebhook {
			fatal = append(fatal, "the project carries a WORKFLOW_JOB_QUEUED webhook, so "+
				"CodeBuild's own runner integration is acquiring jobs as well as billet; two "+
				"schedulers on one job produce duplicate runners")
		}
	}

	fleet, hasFleet, err := p.DescribeFleet(ctx)
	if err != nil {
		return err
	}

	if hasFleet {
		fmt.Printf("  fleet    %s (%s, %s), capacity %d\n",
			fleet.Name, fleet.EnvironmentType, fleet.ComputeType, fleet.BaseCapacity)

		fleetFatal, fleetWarnings := fleet.Problems(*cfg.Node.CodeBuild)
		fatal = append(fatal, fleetFatal...)
		warnings = append(warnings, fleetWarnings...)

		concurrencyFatal, concurrencyWarnings := judgeCodeBuildFleetConcurrency(cfg, fleet)
		fatal = append(fatal, concurrencyFatal...)
		warnings = append(warnings, concurrencyWarnings...)
	}

	for _, w := range warnings {
		fmt.Printf("  warning  %s\n", w)
	}

	if len(fatal) > 0 {
		errs := make([]error, 0, len(fatal))
		for _, f := range fatal {
			errs = append(errs, errors.New(f))
		}

		return errors.Join(errs...)
	}

	return nil
}
