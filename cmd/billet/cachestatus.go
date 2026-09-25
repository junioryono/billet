package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// cmdCacheStatus prints every tier's caches and every kill-switch block.
func cmdCacheStatus(ctx context.Context, args []string) error {
	fs := newFlagSet("billet cache status")
	cfgPath := addConfigFlag(fs)
	if err := parse(fs, args); err != nil {
		return err
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

	return nil
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
