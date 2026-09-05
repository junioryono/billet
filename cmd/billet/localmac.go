package main

import (
	"context"
	"fmt"
	"os"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/lifeops/launchd"
)

// macStatus reports what this Mac is actually running.
//
// A SEPARATE RENDERER RATHER THAN THE systemd ONE WITH DIFFERENT WORDS. The
// Linux report is built out of facts only systemd has — a fragment path,
// drop-ins, a pending daemon-reload, the `+` privilege prefix — and each line of
// it exists because that fact goes wrong silently. launchd has none of them, and
// it has one systemd does not: the durable disabled-override database, which is
// invisible from the plist on disk and decides whether the service starts at
// all. Rendering one manager's report in the other's vocabulary would print
// fields that are permanently empty and omit the one that matters.
//
// IT ANSWERS ON A HALF-BUILT HOST, which is when somebody runs it: a config that
// will not load, an agent that is not installed and a service launchd has never
// heard of are all things to REPORT rather than reasons to fail.
func macStatus(ctx context.Context, cfgPath string) error {
	fmt.Printf("config   %s\n", cfgPath)

	var keyPaths []string

	cfg, cfgErr := config.Load(cfgPath)

	switch {
	case cfgErr != nil:
		fmt.Printf("         UNREADABLE: %v\n", cfgErr)
	default:
		keyPaths = appKeyFilePaths(cfg)
	}

	printMacFile("config", cfgPath)

	for _, keyPath := range keyPaths {
		printMacFile("key", keyPath)
	}

	if self, err := os.Executable(); err != nil {
		fmt.Printf("binary   UNKNOWN: %v\n", err)
	} else {
		fmt.Printf("binary   %s\n", self)
	}

	c := launchd.New()
	server, node := c.Services()

	for _, s := range []struct {
		role  string
		label string
		want  bool
	}{
		{"server", server, cfg == nil || cfg.Server != nil},
		{"node", node, cfg == nil || cfg.Node != nil},
	} {
		if !s.want {
			continue
		}

		printMacService(ctx, c, s.role, s.label)
	}

	// THE TWO SCHEDULED AGENTS, reported for what they are: installed and
	// loaded, or not. Neither holds a process between runs, so a "not running"
	// here is the ordinary state and the enablement line is the one that matters.
	upgrade, images := c.Scheduled()

	printMacService(ctx, c, "upgrade", upgrade)
	printMacService(ctx, c, "images", images)

	return nil
}

// printMacService reports one launch agent.
func printMacService(ctx context.Context, c *launchd.Converger, role, label string) {
	facts, err := c.RunningOne(ctx, label)
	if err != nil {
		fmt.Printf("%-8s UNKNOWN: %v\n", role, err)

		return
	}

	snap, err := c.Snapshot(ctx, label)
	if err != nil {
		snap = fmt.Sprintf("unreadable (%v)", err)
	}

	fmt.Printf("%-8s %s — %s\n", role, label, snap)

	// THE OVERRIDE DATABASE IS REPORTED WHATEVER IT SAYS, because it is the one
	// fact about a launch agent that nothing else on the machine shows. A label
	// disabled there does not start, however good its plist is, and the plist is
	// where anybody would look.
	enablement, err := c.EnabledNow(ctx, label)
	if err != nil {
		fmt.Printf("         enablement UNKNOWN: %v\n", err)
	} else {
		fmt.Printf("         %s (%s)\n", enablementSentence(enablement), enablement.How)
	}

	switch facts.IsThisBuild {
	case lifeops.No:
		fmt.Printf("         RUNNING A DIFFERENT BUILD: %s\n", facts.Why)
	case lifeops.Unknown:
		if facts.Active == lifeops.Yes {
			fmt.Printf("         RUNNING BUILD UNCONFIRMED: %s\n", facts.Why)
		}
	case lifeops.Yes:
	}
}

// enablementSentence says what a verdict means for the next login.
func enablementSentence(e lifeops.Enablement) string {
	switch e.Enabled {
	case lifeops.Yes:
		return "starts at login"
	case lifeops.No:
		return "does NOT start at login"
	default:
		return "billet cannot tell whether it starts at login"
	}
}

// printMacFile reports one path's mode, which is what decides whether the agent
// can read it.
//
// NO OWNER COLUMN, deliberately: a launch agent runs as the operator, so every
// file it reads is one they own, and printing an owner would invite the Linux
// question — which account should this belong to — that has no answer here.
func printMacFile(label, path string) {
	info, err := os.Stat(path)

	switch {
	case err == nil:
		fmt.Printf("         %-6s %s (%04o)\n", label, path, info.Mode().Perm())
	case os.IsNotExist(err):
		fmt.Printf("         %-6s %s (MISSING)\n", label, path)
	default:
		fmt.Printf("         %-6s %s (UNREADABLE: %v)\n", label, path, err)
	}
}
