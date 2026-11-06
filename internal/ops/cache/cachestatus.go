package cache

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// cmdCacheStatus prints every tier's caches, every kill-switch block, and what
// each cache did for the jobs assigned recently.
func cmdCacheStatus(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet cache status", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	since := fs.Duration("since", 24*time.Hour, "how far back to count what the caches did")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}
	if *since <= 0 {
		return fmt.Errorf("--since must be positive, got %s", *since)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	printTierCaches(env.Stdout, cfg.Tiers)

	if cfg.Server == nil {
		fmt.Fprintln(env.Stdout, "\nno server section, so no central kill switch to read")

		return nil
	}
	db, err := app.OpenLedger(ctx, cfg, app.LedgerOperator)
	if err != nil {
		return err
	}
	defer db.Close()
	blocks, err := db.CacheBlocks(ctx)
	if err != nil {
		return err
	}
	printCacheBlocks(env.Stdout, blocks)
	if err := db.Close(); err != nil {
		return err
	}

	a, _, closeDB, err := app.OpenOperator(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()
	counts, jobs, err := a.CacheOutcomes(ctx, time.Now().Add(-*since), cacheOutcomeRows)
	if err != nil {
		return err
	}
	printCacheOutcomes(env.Stdout, counts, jobs, *since)
	for _, warning := range neverWarmed(counts, jobs, cacheOutcomeRows, *since) {
		fmt.Fprintln(env.Stdout, "warning: "+warning)
	}

	return nil
}

// cacheOutcomeRows bounds the jobs one report reads.
const cacheOutcomeRows = 100_000

// neverWarmAfter is how many cold jobs in a window, with none warm, make a build
// cache worth naming: enough that a tier's first jobs after a new key are not it.
const neverWarmAfter = 10

// What `billet check` reads to say so. Bounded in time as well as rows, because a
// limit bounds what comes back and not what is scanned: nothing indexes when a job
// was assigned, and this report must not hold up the rest of the check.
const (
	checkWarmthWindow = 24 * time.Hour
	checkWarmthRows   = 10_000
	checkWarmthWait   = 5 * time.Second
)

// neverWarmed names each tier's Go or Bazel cache that went cold in neverWarmAfter
// jobs or more and warm in none, over a read of jobs rows that stopped at limit if
// it reached it.
//
// A READ THAT REACHED ITS LIMIT SAYS NOTHING, because the warm job may be in the
// part it did not read, and the rows are newest first.
//
// WHAT THE NODE SAW, and no more: `cold` says no request found its object in the
// node's cache. The go command's own cache may still have served the job, and a key
// nobody has published yet goes cold honestly, so the causes are offered as things
// to look for. It is said because a cache that never warms costs its volume and its
// requests and returns nothing (#689).
func neverWarmed(counts map[string]map[string]map[string]int, jobs, limit int, since time.Duration) []string {
	if jobs >= limit {
		return nil
	}
	var warnings []string
	for _, tier := range slices.Sorted(maps.Keys(counts)) {
		for _, cache := range []string{"bazel", "go"} {
			outcomes := counts[tier][cache]
			if outcomes["warm"] > 0 || outcomes["cold"] < neverWarmAfter {
				continue
			}
			warnings = append(warnings, fmt.Sprintf("%s's %s cache was cold in %d job(s) and warm in none "+
				"in the last %s: nothing they asked for was in it. If that goes on, look in the node's "+
				"journal for `could not yet publish` and `a cache publication was abandoned`",
				tier, cache, outcomes["cold"], cli.ShortDuration(since)))
		}
	}

	return warnings
}

// ReportCacheWarmth prints what neverWarmed finds for `billet check`.
//
// REPORTED, NEVER FATAL, and a failed or slow read says it could not tell: this is
// a cache's effectiveness, and no deployment is wrong to run without it.
func ReportCacheWarmth(ctx context.Context, env cli.Env, a *alloc.Allocator) {
	ctx, cancel := context.WithTimeout(ctx, checkWarmthWait)
	defer cancel()

	counts, jobs, err := a.CacheOutcomes(ctx, time.Now().Add(-checkWarmthWindow), checkWarmthRows)
	if err != nil {
		fmt.Fprintf(env.Stdout, "caches   could not tell whether the build caches warm: %v\n", err)

		return
	}
	for _, warning := range neverWarmed(counts, jobs, checkWarmthRows, checkWarmthWindow) {
		fmt.Fprintln(env.Stdout, "caches   "+warning)
	}
}

// cacheReportOrder is the order caches are reported in.
var cacheReportOrder = []string{"image", "sticky", "actions", "git", "bazel", "go", "npm"}

// printCacheOutcomes renders what each cache did per tier. A cache with no
// observation for any of a tier's jobs is left out; within one that has some,
// the jobs it was not observed for are counted as such, never as a miss.
func printCacheOutcomes(w io.Writer, counts map[string]map[string]map[string]int, jobs int,
	since time.Duration,
) {
	fmt.Fprintf(w, "\nwhat the caches did for the %d job(s) assigned in the last %s:\n", jobs,
		cli.ShortDuration(since))
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
	for i := range tiers {
		tier := &tiers[i]
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
		part := string(kind)
		// The npm store is the node's and has no per-tier ceiling to print.
		if setting.MaxSize > 0 {
			part += "(" + setting.MaxSize.String() + ")"
		}
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
