package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// checkOptions is what a caller asks one check run to do. A struct rather than
// a parameter list because `billet check` is not the only caller any more, and
// a bool pair is exactly the shape a second caller gets silently wrong.
type checkOptions struct {
	configPath string
	// authorize additionally DRY-RUNs a launch against AWS, proving the ec2
	// role may RunInstances.
	authorize bool
	// maintenanceProbe is the host-upgrade transaction's quiescent probe: it
	// crosses the maintenance fence and skips every network call.
	maintenanceProbe bool
}

// githubVerdict is what the App verification ESTABLISHED, carried as a value
// rather than left for a caller to infer from printed text.
//
// FIVE STATES, BECAUSE FOUR OF THEM ARE NOT A PASS AND THEY ARE NOT THE SAME
// FACT. "Never attempted" (a maintenance run skips the network entirely),
// "attempted and could not reach a verdict" (offline), and "attempted and
// refused" are different things to tell an operator and different things for a
// caller to act on — and the reason this type exists at all is that a caller
// which only made the refusal fatal would accept "never attempted" as proof.
// The fifth is "there was nothing to verify", which only a node-only host
// reaches: config validation requires a github section for the server role.
//
// The zero value is githubNotConfigured, which is the safe direction: a caller
// that forgets to set it cannot read a pass out of it.
type githubVerdict int

const (
	// githubNotConfigured is a config with no github: section — a node-only
	// host, where there is nothing to verify.
	githubNotConfigured githubVerdict = iota
	// githubSkipped means the verification never ran.
	githubSkipped
	// githubUnverifiable means it ran and could not reach a verdict.
	githubUnverifiable
	// githubFailed means GitHub refused the credential.
	githubFailed
	// githubVerified means the App is installed on the configured org with the
	// exact permissions billet requested.
	githubVerified
)

// String names the verdict, so a diagnostic reads as a fact rather than an
// integer.
func (v githubVerdict) String() string {
	switch v {
	case githubNotConfigured:
		return "not configured"
	case githubSkipped:
		return "skipped"
	case githubUnverifiable:
		return "unverifiable"
	case githubFailed:
		return "failed"
	case githubVerified:
		return "verified"
	}

	return "unknown"
}

// checkReport is what a run ESTABLISHED, for a caller that has to act on it.
//
// It carries the GitHub verdict only. Everything else `billet check` reports is
// either fatal (returned as the error) or an advisory band it prints and does
// not prove — an unresolved AMI, an inconclusive SQS or instance-profile or
// cache probe, launch authority left unchecked without --authorize, and the
// provider host probes that say in their own output that they do not prove
// every launch requirement. A caller must not read this report as "everything
// check printed is established"; `billet local up` gates on the field below and
// says plainly what it did not prove.
type checkReport struct {
	github githubVerdict
}

// cmdCheck is the explicit "is this deployment sane" command.
//
// It opens the ledger through OpenAdmin, so it answers WHILE the control plane
// is running — which is exactly when an operator reaches for it, and when
// opening exclusively made it useless. It still migrates when nothing holds the
// directory, so a first run sets the schema up; against a live plane it verifies
// and refuses rather than upgrading a schema that plane is using.
func cmdCheck(ctx context.Context, args []string) error {
	fs := newFlagSet("billet check")
	cfgPath := addConfigFlag(fs)
	authorize := fs.Bool("authorize", false,
		"also DRY-RUN a launch against AWS to prove the ec2 role may RunInstances "+
			"(a DryRun has no side effect); teardown cannot be dry-run, so "+
			"ec2:TerminateInstances stays advisory")
	maintenanceProbe := fs.Bool("maintenance-probe", false,
		"this run is the host-upgrade transaction's quiescent probe: open the ledger "+
			"through the maintenance fence and skip the App verification and every AWS call "+
			"(local and cluster checks still run)")
	if err := parse(fs, args); err != nil {
		return err
	}

	_, err := runCheck(ctx, checkOptions{
		configPath:       *cfgPath,
		authorize:        *authorize,
		maintenanceProbe: *maintenanceProbe,
	})

	return err
}

// runCheck performs a check and reports what it established. It prints as it
// goes, because the report IS the product for an operator; the return value
// exists for a caller that has to make a decision on the outcome.
func runCheck(ctx context.Context, opts checkOptions) (checkReport, error) {
	var report checkReport

	cfg, err := config.Load(opts.configPath)
	if err != nil {
		return report, err
	}
	fmt.Printf("config   %s\n", opts.configPath)

	// The probe flag skips the network; so does the BILLET_MAINTENANCE
	// environment variable — for the SKIPS ONLY, never the fence. The env form
	// exists for binary-vintage compatibility: the Ansible upgrade transaction
	// sets it for whatever billet it is driving, and an older billet keys both
	// its fence bypass and its preflight skip on it, while in this billet it
	// can only make the check quieter — skipping probes cannot authorize a
	// write, which is why it is safe where the fence bypass was not.
	skipNetworkProbes := opts.maintenanceProbe || os.Getenv("BILLET_MAINTENANCE") == "1"

	// A fatal GitHub verdict is returned at the END, after every local section
	// has reported: check is the command an operator reaches for when something
	// is wrong, and a stale installation_id must not hide the local diagnostics.
	var githubFailure error

	if cfg.GitHub != nil {
		fmt.Printf("org      %s (app %d, installation %d)\n",
			cfg.GitHub.Org, cfg.GitHub.AppID, cfg.GitHub.InstallationID)

		// WHERE THE KEY CAME FROM, BECAUSE "THE APP KEY IS FINE" IS A DIFFERENT
		// FACT DEPENDING ON WHICH. An operator debugging a failover has to know
		// whether this host read a file of its own or the deployment's shared
		// store, and the two look identical in every other line of this report.
		fmt.Printf("app key  %s\n", appKeyLocation(cfg))

		key, err := resolveAppKey(ctx, cfg)
		if err != nil {
			return report, err
		}

		// PROVED LIVE, not just parsed: the key signs a JWT GitHub accepts, the
		// App is installed, the installation id matches, and the permissions are
		// exactly what billet requested — every mismatch fatal in both
		// directions. Offline is ADVISORY here (this command still has local
		// facts worth reporting) and it says so by name, because "check passed"
		// while unverified is exactly what a later `local up` must not build on.
		switch {
		case skipNetworkProbes:
			report.github = githubSkipped
			fmt.Printf("github   (verification skipped during maintenance)\n")
		default:
			inst, err := github.VerifyAppAt(ctx, nil, githubAPIBase, cfg.GitHub.AppID, key,
				cfg.GitHub.Org, cfg.GitHub.InstallationID)
			switch {
			case errors.Is(err, github.ErrAppUnverifiable):
				report.github = githubUnverifiable
				fmt.Printf("github   UNVERIFIED: %v\n", err)
				fmt.Printf("         (the App may still be fine — this says the check could not " +
					"reach a verdict, and nothing may treat it as one)\n")
			case err != nil:
				report.github = githubFailed
				fmt.Printf("github   FAILED: %v\n", err)
				githubFailure = err
			default:
				report.github = githubVerified
				fmt.Printf("github   verified: installation %d on %s, permissions exactly as "+
					"requested\n", inst.ID, cfg.GitHub.Org)
			}
		}
	}

	if cfg.Server != nil {
		fmt.Printf("listen   %s\n", cfg.Server.Listen)

		// SAID OUT LOUD BECAUSE ITS ABSENCE IS A DECISION. A control plane with no
		// enrollment address does not admit machines over the network at all, and
		// an operator meets that as a handshake failure on the far side of the
		// deployment unless something here says so first.
		if cfg.Server.BootstrapListen != "" {
			fmt.Printf("enroll   %s\n", cfg.Server.BootstrapListen)
		} else {
			fmt.Printf("enroll   none; issue bundles with `billet ca issue <node>`\n")
		}
		fmt.Printf("ceiling  %d vCPU, %s\n", cfg.Server.MaxVCPU, cfg.Server.MaxMemory)

		// `billet check` is the command an operator reaches for WHEN SOMETHING IS
		// WRONG, which is exactly when the server is running. Opening the ledger
		// exclusively made it unusable at that moment.
		open := openStateAdmin
		if opts.maintenanceProbe {
			// The TYPED fence crossing. The environment variable used to bypass
			// the fence for any command that inherited it; now only this
			// explicit request does, and only for the check's quiescent probe.
			open = openStateMaintenance
		}
		db, err := open(ctx, cfg)
		if err != nil {
			if errors.Is(err, state.ErrMaintenance) {
				return report, fmt.Errorf("server state: %w\n(if this run IS the host-upgrade "+
					"transaction's quiescent probe, pass --maintenance-probe)", err)
			}

			return report, fmt.Errorf("server state: %w", err)
		}
		defer db.Close()

		// ASKED FOR EXPLICITLY, because an ADMIN open does not scan. The probe's
		// open path already ran the identical quick_check (OpenMaintenance is a
		// non-admin open, the same path `server --upgrade-probe` relies on), so
		// scanning again there would read the unbounded ledger twice inside the
		// upgrade's stopped-service window for the same verdict.
		if !opts.maintenanceProbe {
			if err := db.IntegrityCheck(ctx); err != nil {
				return report, fmt.Errorf("server state: %w", err)
			}
		}

		fmt.Printf("state    %s (ok, integrity verified)\n", cfg.Server.IdentityDir)

		// AN UNWATCHED BACKUP IS NOT ONE EITHER, which is the sibling of the rule
		// this whole area is built on. A timer that stopped firing looks exactly
		// like one that is working, and the day an operator finds out is the day
		// the archive was needed. ADVISORY throughout: this reports, and nothing
		// here decides whether the deployment may run.
		reportBackupAge(ctx, cfg, skipNetworkProbes)

		a, err := alloc.New(db, alloc.Limits{
			MaxVCPU:   cfg.Server.MaxVCPU,
			MaxMemory: cfg.Server.MaxMemory,
			Nodes:     cfg.NodePolicies(),
		}, cfg.Tiers)
		if err != nil {
			return report, fmt.Errorf("capacity allocator: %w", err)
		}
		registered, err := a.RegisteredNodes(ctx)
		if err != nil {
			return report, err
		}
		if len(registered) == 0 {
			fmt.Println("fleet    no registered nodes")
		}
		for _, member := range registered {
			site := member.Site
			if site == "" {
				site = "local (implicit)"
			}
			liveness := "offline"
			if member.Live {
				liveness = "live"
			}
			fmt.Printf("fleet    %-24s at %s via %s (%s)\n", member.Name, site,
				member.Provider, liveness)
		}
	}

	if cfg.Node != nil {
		var bundle *wirecert.Bundle
		if cfg.Node.TLS != nil {
			b, err := nodeBundle(cfg)
			if err != nil {
				return report, fmt.Errorf("node identity: %w", err)
			}
			bundle = b
		}
		site := cfg.Node.Site
		if site == "" {
			site = "local (implicit)"
		}
		fmt.Printf("node     %s at %s via %s -> %s\n", cfg.Node.Name, site,
			cfg.Node.Provider, cfg.Node.ServerAddr)

		// Config validation proves the ec2 block is COHERENT. It cannot prove this
		// machine can act on it, and the difference is a deployment that validates
		// and then fails on the first job of the day.
		// THE COST BOUND IS PRINTED FOR EVERY REMOTE BACKEND, because every one of
		// them declares priced shapes; only the ec2 credential probe is ec2's.
		if !cfg.Node.Provider.RunsOnHost() {
			if err := printRemoteCost(cfg); err != nil {
				return report, err
			}
		}

		if cfg.Node.Provider == config.ProviderEC2 && cfg.Node.EC2 != nil {
			if err := checkEC2Credentials(ctx, cfg, bundle, opts.authorize, skipNetworkProbes); err != nil {
				return report, err
			}
		}

		// THE CEILINGS ARE PRINTED WHETHER OR NOT AWS CAN BE REACHED, and the split
		// is the point: they are facts about the configuration, and the backend's
		// acceptance requires them visible before work is admitted. A check that
		// reported them only when it could also reach AWS would say nothing on the
		// machine where somebody is still writing the config — which is the one
		// moment the sentence is useful.
		if cfg.Node.Provider == config.ProviderCodeBuild && cfg.Node.CodeBuild != nil {
			printCodeBuildCeilings(cfg)

			if !skipNetworkProbes {
				if err := checkCodeBuildLive(ctx, cfg, bundle); err != nil {
					return report, err
				}
			}
		}

		if cfg.Node.Ceph != nil {
			if err := checkCephCluster(ctx, cfg.Node.Ceph); err != nil {
				return report, err
			}
		}

		// AFTER THE STORAGE, because the storage belongs to the SITE and this belongs
		// to the machine: a cluster a host cannot reach is wrong for every node there,
		// while a missing /dev/kvm is wrong for this one. An operator reading a wall
		// of output acts on the first thing it names, and the shared fault is the more
		// useful one to name first.
		if cfg.Node.Provider == config.ProviderFirecracker && cfg.Node.Firecracker != nil {
			if err := checkFirecrackerHost(ctx, cfg); err != nil {
				return report, err
			}
		}

		if cfg.Node.Provider == config.ProviderTart {
			if err := checkTartHost(ctx, cfg); err != nil {
				return report, err
			}
		}
	}

	for i := range cfg.Sites {
		fmt.Printf("site     %-24s %s\n", cfg.Sites[i].Name, cfg.Sites[i].Store)
	}

	// Per-host policy decides what each machine may run and how many macOS
	// guests it may hold. Validating it without showing it means an operator who
	// restricted a host has no way to confirm billet read what they meant.
	for i := range cfg.Nodes {
		p := &cfg.Nodes[i]

		backend := "provider unset"
		if p.Provider != "" {
			backend = string(p.Provider)
		}

		guests := "no guest_os allowlist"
		if len(p.GuestOS) > 0 {
			guests = fmt.Sprintf("guest_os %v", p.GuestOS)
		}

		// Say WHERE the number came from, and do not report a macOS capability
		// the host cannot have. A Firecracker host printed "max 2 macOS (Apple
		// default)", which is true of the config field and false of the machine:
		// macOS guests need Apple hardware — tart on a Mac somebody owns, or a
		// codebuild MAC_ARM fleet AWS operates — and config owns that allowlist.
		// And a host whose allowlist excludes macOS has an effective limit of
		// zero — reporting that as "0 (default)" reads as though billet's
		// default were zero, sending the operator to the wrong field entirely.
		var macOS string

		switch {
		case p.Provider != "" && !p.Provider.ServesMacOS():
			macOS = fmt.Sprintf("macOS n/a (%s cannot run macOS guests)", p.Provider)
		case !p.AllowsGuestOS(config.GuestMacOS):
			macOS = "no macOS (excluded by guest_os)"
		case p.MacOSVMLimit == nil && cfg.MacOSFleetProvider(p.Name) != "":
			// A REMOTE BACKEND HAS NO APPLE DEFAULT. Its Macs are a fleet AWS
			// operates under its own agreement, so the number is required rather
			// than assumed the moment a macOS tier targets the node — and until
			// then the honest line is that nothing has been declared, not a "2"
			// attributed to Apple. Asked of config rather than of p.Provider so
			// there is one answer to "is this node a fleet"; for a config that
			// LOADED the two agree, because validation requires the limit the
			// moment the pinned tiers alone say the node is remote — which is
			// why a mutation back to p.Provider survives the rendering test and
			// is redundant rather than uncovered.
			macOS = "macOS limit undeclared (a macOS tier pinned here requires macos_vm_limit)"
		case p.MacOSVMLimit == nil:
			macOS = fmt.Sprintf("max %d macOS (Apple default)", p.MacOSLimit())
		default:
			macOS = fmt.Sprintf("max %d macOS", p.MacOSLimit())
		}

		fmt.Printf("  policy %-14s %-12s %-24s %s\n", p.Name, backend, guests, macOS)
	}

	fmt.Printf("tiers    %d\n", len(cfg.Tiers))

	for i := range cfg.Tiers {
		t := &cfg.Tiers[i]

		intercept := ""
		if t.Intercept {
			intercept = "  cache-intercept"
		}
		// The whole list, joined. Printing t.Provider alone showed a BLANK backend
		// for any tier written with `providers:` — and `billet check` is the one
		// place an operator looks to confirm what they configured, so a field that
		// silently reads empty is worse than no field at all.
		backends := make([]string, 0, 2)
		for _, kind := range t.AcceptableProviders() {
			backends = append(backends, string(kind))
		}

		reserved := ""
		if t.Reserved > 0 {
			reserved = fmt.Sprintf("  reserved:%d", t.Reserved)
		}

		fmt.Printf("  %-34s %2d vCPU  %8s  %s/%s%s%s\n",
			t.Label, t.VCPU, t.Memory, strings.Join(backends, ","), t.GuestOS,
			reserved, intercept)

		// THE SERVER REFUSES ON THIS AND CHECK USED TO PASS OVER IT.
		//
		// A trusted tier's runner group is verified at server startup, so a
		// deployment whose group is missing or misconfigured got a green check
		// and then failed to start — with `billet init` having printed that this
		// step confirms "any runner-group policy". Two operators hit exactly that
		// as their first failure on a fresh host.
		//
		// Reported per tier rather than aggregated: the group is a property of
		// the tier, and an operator with several needs to know WHICH one.
		// ONLY WHEN THE APP ITSELF WAS VERIFIED. An unreachable GitHub is
		// ADVISORY for the installation probe — check still has local facts
		// worth reporting — and a group lookup against the same unreachable API
		// cannot reach a verdict either. Running it anyway turned "could not
		// tell" into "failed", which is the distinction this command is careful
		// about everywhere else. Maintenance skips it for free: the verdict is
		// githubSkipped, not verified.
		if t.Trust == config.WorkloadTrusted && report.github == githubVerified {
			if err := checkTrustedGroup(ctx, cfg, t); err != nil {
				report.github = githubFailed
				fmt.Printf("           runner group FAILED: %v\n", err)

				if githubFailure == nil {
					githubFailure = err
				}
			} else {
				fmt.Printf("           runner group %q verified\n", t.RunnerGroup)
			}
		}
	}

	// HOW MUCH OF THE MACHINE IS SPOKEN FOR, shown whenever anything is reserved.
	//
	// A reservation is held back for as long as it is UNMET, which for a tier
	// that gets no work is forever — so an operator can quietly make their
	// machine smaller with no error and no log line, because from billet's point
	// of view nothing is wrong. This is the one place they look to confirm what
	// they configured, so the total belongs here rather than in a comment they
	// have already read.
	var (
		reservedVCPU   int
		reservedMemory config.ByteSize
	)

	for i := range cfg.Tiers {
		reservedVCPU += cfg.Tiers[i].ReservedVCPU()
		reservedMemory += cfg.Tiers[i].ReservedMemory()
	}

	if reservedVCPU > 0 && cfg.Server != nil && cfg.Server.MaxVCPU > 0 {
		// FLOAT, and one decimal place. The integer form multiplied before
		// dividing, so it could wrap on a large budget — and more usefully, it
		// printed 1 of 128 vCPU as "0%", which reads as "nothing is reserved"
		// when something is.
		share := float64(reservedVCPU) * 100 / float64(cfg.Server.MaxVCPU)

		fmt.Printf("reserved %d of %d vCPU (%.1f%%) and %s of %s, held back from other tiers "+
			"whether or not they are used\n",
			reservedVCPU, cfg.Server.MaxVCPU, share, reservedMemory, cfg.Server.MaxMemory)
	}

	// The deferred GitHub verdict: everything local has reported, and a check
	// with a broken App is still a failed check.
	return report, githubFailure
}

// checkTrustedGroup reaches the verdict the server reaches at startup, using
// only the App credentials `billet check` already holds.
//
// The server validates through the Actions-tenant client it builds anyway; this
// path exists because check builds no such client and should not have to. Both
// end in the same github.ValidateTrustedRunnerGroup, so the policy itself has
// one implementation — only the name-to-id lookup differs, and that difference
// is why this is a function rather than a copy of the server's block.
func checkTrustedGroup(ctx context.Context, cfg *config.Config, t *config.Tier) error {
	gh := cfg.GitHub

	key, err := resolveAppKey(ctx, cfg)
	if err != nil {
		return err
	}

	policy := github.NewRunnerGroupPolicyClientAt(githubAPIBase, gh.Org, gh.AppID,
		gh.InstallationID, key)

	id, isDefault, err := policy.FindRunnerGroupID(ctx, t.RunnerGroup)
	if err != nil {
		return err
	}

	// The server refuses this too: the default group cannot back a trusted pool,
	// because it admits every repository in the organization.
	if isDefault {
		return fmt.Errorf("runner group %q is the default group, which cannot back a trusted "+
			"pool", t.RunnerGroup)
	}

	return policy.ValidateTrustedRunnerGroup(ctx, id, t.Workflows)
}
