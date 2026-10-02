package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/store/ceph"
)

// orphanReclaimer is the store half of `billet cache orphans`.
type orphanReclaimer interface {
	ReclaimOrphans(ctx context.Context, opts ceph.OrphanOptions) (ceph.OrphanReport, error)
}

// cmdCacheOrphans lists the intact writable cache volumes nothing needs and,
// with --reclaim, moves them to the cache pool's trash.
func cmdCacheOrphans(ctx context.Context, args []string) error {
	flags := newFlagSet("billet cache orphans")
	cfgPath := addConfigFlag(flags)
	olderThan := flags.Duration("older-than", ceph.DefaultOrphanAge,
		fmt.Sprintf("how long ago a volume must have been named, at least %s", ceph.OrphanMinimumAge))
	limit := flags.Int("limit", ceph.DefaultOrphanLimit, "how many volumes one pass moves, or lists as reclaimable")
	reclaim := flags.Bool("reclaim", false, "move what the listing calls reclaimable to the trash; "+
		"without it nothing is changed")
	if err := parse(flags, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.Node == nil || cfg.Node.Ceph == nil {
		return fmt.Errorf("%s has no node.ceph section: run this on a node of the site whose cache "+
			"pool it should read", *cfgPath)
	}

	sessions, err := cacheSessionRecords(cfg)
	if err != nil {
		return err
	}

	client, err := ceph.New(*cfg.Node.Ceph)
	if err != nil {
		return err
	}

	return reclaimCacheOrphans(ctx, os.Stdout, client, ceph.OrphanOptions{
		OlderThan: *olderThan, Limit: *limit, Reclaim: *reclaim, InSession: sessions.Mentions,
	})
}

// cacheSessionRecords reads this node's cache custody records. A node with no
// cache listener keeps none, so only there is a missing directory an empty set.
func cacheSessionRecords(cfg *config.Config) (node.CacheSessionRecords, error) {
	records, err := node.ReadCacheSessionRecords(cfg.Node.StateDir)
	if err != nil {
		if cfg.Node.Cache == nil && errors.Is(err, fs.ErrNotExist) {
			return node.CacheSessionRecords{}, nil
		}

		return node.CacheSessionRecords{}, fmt.Errorf("could not tell which cache volumes this node's "+
			"sessions hold, so nothing is judged: %w", err)
	}

	return records, nil
}

// orphanListed are the verdicts printed one line per image; the rest are counted.
var orphanListed = []ceph.OrphanVerdict{
	ceph.OrphanReclaimable, ceph.OrphanMoved, ceph.OrphanMoveUnknown, ceph.OrphanUnknown,
	ceph.OrphanWatched, ceph.OrphanSnapshotted, ceph.OrphanUsed, ceph.OrphanCreatedRecently, ceph.OrphanInSession,
	ceph.OrphanInIndex, ceph.OrphanHalfRemoved, ceph.OrphanGone,
}

var orphanCounted = slices.Concat(orphanListed, []ceph.OrphanVerdict{
	ceph.OrphanDeferred, ceph.OrphanTooYoung, ceph.OrphanGeneration, ceph.OrphanForeign,
})

func reclaimCacheOrphans(
	ctx context.Context, w io.Writer, store orphanReclaimer, opts ceph.OrphanOptions,
) error {
	report, err := store.ReclaimOrphans(ctx, opts)
	if err != nil {
		return err
	}

	fmt.Fprintf(w, "%d cache image(s) listed; a writable volume is judged when it was named at "+
		"least %s ago, and at most %d are taken per pass\n\n", len(report.Images), opts.OlderThan, opts.Limit)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, verdict := range orphanListed {
		for _, image := range report.Images {
			if image.Verdict != verdict {
				continue
			}

			detail := ""
			if image.Err != nil {
				detail = image.Err.Error()
			}
			fmt.Fprintf(tw, "%s\t%s\tnamed %s\t%s\n", verdict, image.Name,
				image.Named.UTC().Format(time.RFC3339), detail)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	for _, verdict := range orphanCounted {
		if n := report.Count(verdict); n > 0 {
			fmt.Fprintf(w, "%-40s %d\n", verdict, n)
		}
	}

	switch {
	case !opts.Reclaim:
		fmt.Fprintf(w, "\nnothing was changed; run again with --reclaim to move the %d reclaimable "+
			"volume(s) to the trash\n", report.Count(ceph.OrphanReclaimable))
	default:
		fmt.Fprintf(w, "\nmoved %d volume(s) to the trash; the node's purge deletes them from there\n",
			report.Count(ceph.OrphanMoved))
	}

	var failures []string
	if n := report.Count(ceph.OrphanUnknown); n > 0 {
		failures = append(failures, fmt.Sprintf("%d cache volume(s) could not be judged and were kept", n))
	}
	if n := report.Count(ceph.OrphanMoveUnknown); n > 0 {
		failures = append(failures, fmt.Sprintf("%d move(s) to the trash were not confirmed and may "+
			"have happened", n))
	}
	if len(failures) > 0 {
		return &exitError{code: 1, msg: strings.Join(failures, "; ")}
	}

	return nil
}
