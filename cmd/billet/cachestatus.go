package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// cmdCacheStatus prints every tier's caches, every kill-switch block, and what
// each cache did for the jobs assigned recently.
func cmdCacheStatus(ctx context.Context, args []string) error {
	fs := newFlagSet("billet cache status")
	cfgPath := addConfigFlag(fs)
	since := fs.Duration("since", 24*time.Hour, "how far back to count what the caches did")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *since <= 0 {
		return fmt.Errorf("--since must be positive, got %s", *since)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	printTierCaches(os.Stdout, cfg.Tiers)

	if cfg.Server == nil {
		fmt.Println("\nno server section, so no central kill switch to read")

		return nil
	}
	db, err := openStateAdmin(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	blocks, err := db.CacheBlocks(ctx)
	if err != nil {
		return err
	}
	printCacheBlocks(os.Stdout, blocks)
	if err := db.Close(); err != nil {
		return err
	}

	a, closeDB, err := controlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()
	counts, jobs, err := a.CacheOutcomes(ctx, time.Now().Add(-*since), cacheOutcomeRows)
	if err != nil {
		return err
	}
	printCacheOutcomes(os.Stdout, counts, jobs, *since)

	return nil
}

// cacheOutcomeRows bounds the jobs one report reads.
const cacheOutcomeRows = 100_000

// cacheReportOrder is the order caches are reported in.
var cacheReportOrder = []string{"image", "sticky", "actions", "git", "bazel", "go"}

// printCacheOutcomes renders what each cache did per tier. A cache with no
// observation for any of a tier's jobs is left out; within one that has some,
// the jobs it was not observed for are counted as such, never as a miss.
func printCacheOutcomes(w io.Writer, counts map[string]map[string]map[string]int, jobs int,
	since time.Duration,
) {
	fmt.Fprintf(w, "\nwhat the caches did for the %d job(s) assigned in the last %s:\n", jobs,
		shortDuration(since))
	if jobs == 0 {
		return
	}
	tiers := slices.Sorted(maps.Keys(counts))
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TIER\tCACHE\tOUTCOMES")
	for _, tier := range tiers {
		for _, cache := range cacheReportOrder {
			outcomes := counts[tier][cache]
			if len(outcomes) == 0 || len(outcomes) == 1 && outcomes[""] > 0 {
				continue
			}
			var parts []string
			for _, outcome := range slices.Sorted(maps.Keys(outcomes)) {
				name := outcome
				if name == "" {
					name = "not observed"
				}
				parts = append(parts, fmt.Sprintf("%s %d", name, outcomes[outcome]))
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", tier, cache, strings.Join(parts, ", "))
		}
	}
	_ = tw.Flush()
}

// printTierCaches renders each tier's effective cache configuration, every
// default applied, so what a tier gets is read here rather than worked out.
func printTierCaches(w io.Writer, tiers []config.Tier) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TIER\tPUBLISH\tSCOPE\tCACHES")
	for _, tier := range tiers {
		spec := tier.EffectiveCache()
		scope := "-"
		if spec.Owner != "" {
			scope = spec.Owner + "/" + spec.Repository
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", tier.Label, spec.Publish, scope, cacheSummary(spec))
	}
	_ = tw.Flush()
}

// cacheSummary names each enabled cache with its ceiling.
func cacheSummary(spec config.CacheSpec) string {
	var parts []string
	for _, kind := range config.CacheKinds {
		setting := spec.Setting(kind)
		if !setting.Enabled {
			continue
		}
		part := string(kind) + "(" + setting.MaxSize.String() + ")"
		if kind == config.CacheGo && spec.GoTestResults {
			part += "+tests"
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "none"
	}

	return strings.Join(parts, " ")
}

// printCacheBlocks renders the kill switch.
func printCacheBlocks(w io.Writer, blocks []state.CacheBlock) {
	if len(blocks) == 0 {
		fmt.Fprintln(w, "\nkill switch: nothing blocked")

		return
	}
	fmt.Fprintln(w, "\nkill switch:")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SCOPE\tCACHE\tSINCE")
	for _, block := range blocks {
		scope := "org " + block.Owner
		if block.Repository != "" {
			scope = block.Owner + "/" + block.Repository
		}
		kind := block.Kind
		if kind == state.AllCaches {
			kind = "all"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", scope, kind, block.DisabledAt)
	}
	_ = tw.Flush()
}
