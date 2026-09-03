package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider/ec2"
	"github.com/junioryono/billet/internal/store/ebss3"
)

// cmdDecommission tears down the cloud resources billet created OUTSIDE Terraform
// state — leftover EC2 instances and the EBS+S3 cache — so `terraform destroy` does
// not leave them billing. Terraform owns the VPC, IAM role, bucket and queue; it
// does not own the per-job instances or the cache blocks billet mints at runtime,
// which is why this exists as a distinct step BEFORE `terraform destroy`.
//
// It is destructive, so nothing is deleted without --yes; without it, the command
// only reports what it found. Live compute blocks the cache purge: a running
// instance may still be serving a job, so the cache it depends on must not be
// pulled out from under it.
func cmdDecommission(ctx context.Context, args []string) error {
	fs := newFlagSet("billet decommission")
	cfgPath := addConfigFlag(fs)
	yes := fs.Bool("yes", false, "actually delete (without this, only report what would be removed)")
	terminateInstances := fs.Bool("terminate-instances", false,
		"terminate leftover instances too — this FAILS any job still running on them, "+
			"so prefer stopping the node to drain them first")

	if err := parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Node == nil || cfg.Node.Provider != config.ProviderEC2 || cfg.Node.EC2 == nil {
		return errors.New("decommission tears down the ec2 backend's out-of-Terraform " +
			"resources; this config has no ec2 node, so there is nothing for it to remove")
	}

	// The REAL deployment id, resolved certificate-first then the state directory
	// (peeked, never minted). It scopes every List and delete to what THIS
	// deployment owns — decommission must never reach into another deployment's
	// resources, and an unresolved identity would do exactly that if defaulted.
	bundle, err := nodeBundle(cfg)
	if err != nil {
		return fmt.Errorf("node identity: %w", err)
	}

	owner, err := authorizeOwner(cfg, bundle)
	if err != nil {
		return fmt.Errorf("node.ec2: %w", err)
	}
	if owner == "" {
		return errors.New("decommission cannot resolve this deployment's identity (no certificate " +
			"and no id in the state directory), so it cannot tell which resources are yours; enroll " +
			"the node or run it once, then retry")
	}

	creds := awscreds.Default()

	if err := decommissionInstances(ctx, cfg, owner, creds, *yes, *terminateInstances); err != nil {
		return err
	}

	if err := decommissionCache(ctx, cfg, owner, creds, *yes); err != nil {
		return err
	}

	if *yes {
		fmt.Printf("decommission complete — `terraform destroy` can now remove the module's " +
			"VPC, IAM role, bucket and queue without leaving billet resources behind\n")
	} else {
		fmt.Printf("(nothing was deleted — re-run with --yes to remove the above)\n")
	}

	return nil
}

// decommissionInstances lists the deployment's instances and, with --yes and
// --terminate-instances, tears them down. Live instances without --terminate-instances
// are FATAL: they may be running jobs, and the cache purge below must not proceed
// while compute that depends on it is alive.
func decommissionInstances(
	ctx context.Context, cfg *config.Config, owner string, creds awscreds.Source,
	yes, terminateInstances bool,
) error {
	p, err := ec2.New(owner, *cfg.Node.EC2, ec2.WithCredentials(creds))
	if err != nil {
		return fmt.Errorf("node.ec2: %w", err)
	}

	instances, err := p.List(ctx)
	if err != nil {
		return fmt.Errorf("node.ec2: list instances: %w", err)
	}

	if len(instances) == 0 {
		fmt.Printf("instances none running for this deployment\n")

		return nil
	}

	for _, instance := range instances {
		fmt.Printf("instance %s (%s)\n", instance.ID, instance.Name)
	}

	if !terminateInstances {
		return fmt.Errorf("%d instance(s) are still live — stop the node so their jobs drain, then "+
			"re-run; or pass --terminate-instances to force-terminate them (which FAILS any job "+
			"still running)", len(instances))
	}
	if !yes {
		return errors.New("--terminate-instances deletes running compute, so it also needs --yes")
	}

	for _, instance := range instances {
		if _, err := p.Destroy(ctx, instance.ID); err != nil {
			return fmt.Errorf("node.ec2: terminate %s: %w", instance.ID, err)
		}
		fmt.Printf("instance %s terminated\n", instance.ID)
	}

	return nil
}

// decommissionCache purges the deployment's EBS+S3 cache once compute is gone.
func decommissionCache(
	ctx context.Context, cfg *config.Config, owner string, creds awscreds.Source, yes bool,
) error {
	if cfg.Node.EBSS3 == nil {
		return nil // a compute-only ec2 node has no cache to purge
	}

	if !yes {
		fmt.Printf("cache    would purge the ebs-s3 cache (owned snapshots, volumes and S3 state)\n")

		return nil
	}

	store, err := ebss3.New(*cfg.Node.EBSS3, cacheNamespace(owner, cfg.Node.Site), creds)
	if err != nil {
		return fmt.Errorf("node.ebs_s3: %w", err)
	}

	report, err := store.Purge(ctx)
	if err != nil {
		return fmt.Errorf("node.ebs_s3: %w", err)
	}

	fmt.Printf("cache    purged %d snapshot(s), %d volume(s), %d state object(s)",
		report.Snapshots, report.Volumes, report.StateObjects)
	if report.SkippedForeign > 0 {
		fmt.Printf("; skipped %d resource(s) owned by another deployment", report.SkippedForeign)
	}
	fmt.Println()

	return nil
}
