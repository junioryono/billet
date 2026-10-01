package ceph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The bounds on ReclaimOrphans' age and work.
//
// A JOB'S VOLUME IS MAPPED, AND SO WATCHED, FOR THE JOB'S WHOLE LIFE: the node
// maps it on attach and unmaps it only to snapshot or discard it at the end, so
// the unwatched windows are the seconds before the first map and the snapshot
// path after the last unmap, which the node's cache handler bounds at under
// thirteen minutes. DefaultOrphanAge still outlasts the five days GitHub documents
// (not measured) as the longest a job may run on a self-hosted runner, since
// billet imposes no limit of its own, so by default no job that could still be
// running named the volume. OrphanMinimumAge is the shortest an operator may
// choose; below the default the watcher check alone covers a job older than the
// bound.
const (
	OrphanMinimumAge   = 24 * time.Hour
	DefaultOrphanAge   = 6 * 24 * time.Hour
	DefaultOrphanLimit = 200
)

// OrphanVerdict is what ReclaimOrphans decided about one listed cache image.
type OrphanVerdict string

// The verdicts. Only OrphanReclaimable and OrphanMoved say an image is a
// discarded writable volume nothing needs; every other one keeps it.
const (
	OrphanReclaimable OrphanVerdict = "reclaimable"
	OrphanMoved       OrphanVerdict = "moved to the trash"
	OrphanForeign     OrphanVerdict = "not a name billet gives"
	OrphanGeneration  OrphanVerdict = "a generation"
	OrphanTooYoung    OrphanVerdict = "named too recently"
	OrphanInSession   OrphanVerdict = "named by a cache session on this node"
	OrphanInIndex     OrphanVerdict = "named by the cache index"
	OrphanDeferred    OrphanVerdict = "beyond this pass's limit"
	OrphanWatched     OrphanVerdict = "held open by a client"
	OrphanSnapshotted OrphanVerdict = "has snapshots"
	OrphanHalfRemoved OrphanVerdict = "half-removed, which the purge finishes"
	OrphanGone        OrphanVerdict = "gone"
	OrphanUnknown     OrphanVerdict = "could not tell"
)

// OrphanOptions shapes one ReclaimOrphans pass. Its zero value is refused.
type OrphanOptions struct {
	// OlderThan is how long ago a volume must have been named, at least
	// OrphanMinimumAge.
	OlderThan time.Duration
	// Limit bounds how many images one pass asks rbd about, and so how many it
	// can move.
	Limit int
	// Reclaim moves what qualifies to the trash; without it the pass only lists.
	Reclaim bool
	// InSession reports whether a cache session this node keeps names an image.
	InSession func(name string) bool
}

// OrphanImage is one listed cache image and the verdict on it.
type OrphanImage struct {
	Name    string
	Named   time.Time
	Verdict OrphanVerdict
	// Err is why rbd could not tell, for OrphanUnknown.
	Err error
}

// OrphanReport is what one pass found, every listed image in the order it was
// considered.
type OrphanReport struct {
	Images []OrphanImage
}

// Count reports how many images received a verdict.
func (r OrphanReport) Count(verdict OrphanVerdict) int {
	n := 0

	for _, image := range r.Images {
		if image.Verdict == verdict {
			n++
		}
	}

	return n
}

// ReclaimOrphans finds the writable cache volumes an interrupted `rbd rm` left
// intact and, with Reclaim, moves them to the trash for PurgeTrash to delete.
//
// ONLY ON POSITIVE PROOF that nothing needs the image, in this order: the exact
// name cacheName gives a writable volume (a generation is never a candidate);
// named at least OlderThan ago; named by no cache session this node keeps and by
// no record in the cache index; and then, asking rbd, no snapshot in any
// namespace, so no generation's lineage reads it; no mapping on this host; and no
// watcher, so no client anywhere has it open. Any answer rbd gives other than
// those keeps the image as OrphanUnknown.
//
// THE WATCHER CHECK IS THE ONLY GUARD AGAINST AN IMAGE IN USE. `rbd trash mv`
// refuses only an image whose exclusive lock it cannot take, which a volume Create
// makes does not have and a krbd mapping of a clone hands over on request, so it
// moves a mapped image (librbd Trash::move, Ceph v19.2.2, read rather than
// measured). Nothing maps a volume after the job that named it,
// so the age bound is what keeps a volume from being mapped between the check
// and the move. The purge's `trash rm` does refuse an image with watchers.
//
// INTO THE TRASH, NEVER `rbd rm`, for the reason discardCacheVolume gives.
func (c *Client) ReclaimOrphans(ctx context.Context, opts OrphanOptions) (OrphanReport, error) {
	if opts.OlderThan < OrphanMinimumAge {
		return OrphanReport{}, fmt.Errorf("ceph: an orphan must have been named at least %s ago, "+
			"not %s", OrphanMinimumAge, opts.OlderThan)
	}
	if opts.Limit <= 0 {
		return OrphanReport{}, fmt.Errorf("ceph: an orphan pass needs a positive limit, not %d", opts.Limit)
	}
	if opts.InSession == nil {
		return OrphanReport{}, errors.New("ceph: an orphan pass needs this node's cache sessions")
	}

	names, err := c.cacheImages(ctx)
	if err != nil {
		return OrphanReport{}, err
	}

	// A cold site has no index image, which is rbd saying no record exists; any
	// other failure is could-not-tell for every image.
	metadata, err := c.cacheIndexMetadata(ctx)
	if err != nil && !isNoSuchFile(err) {
		return OrphanReport{}, err
	}

	var records strings.Builder
	for key, value := range metadata {
		records.WriteString(key + " " + value + "\n")
	}

	index := records.String()

	images := make([]OrphanImage, 0, len(names))
	for _, name := range names {
		_, named, _ := cacheImageName(name)
		images = append(images, OrphanImage{Name: name, Named: named})
	}

	// OLDEST FIRST, so a bounded pass works through a backlog in the order it
	// was left.
	slices.SortStableFunc(images, func(a, b OrphanImage) int { return a.Named.Compare(b.Named) })

	now := c.now()
	asked := 0

	for i := range images {
		image := &images[i]
		kind, _, ok := cacheImageName(image.Name)

		switch {
		case !ok:
			image.Verdict = OrphanForeign
		case kind != "v":
			image.Verdict = OrphanGeneration
		case now.Sub(image.Named) < opts.OlderThan:
			image.Verdict = OrphanTooYoung
		case opts.InSession(image.Name):
			image.Verdict = OrphanInSession
		case strings.Contains(index, image.Name):
			image.Verdict = OrphanInIndex
		case asked >= opts.Limit:
			image.Verdict = OrphanDeferred
		default:
			if err := ctx.Err(); err != nil {
				image.Verdict, image.Err = OrphanUnknown, err

				continue
			}

			asked++
			image.Verdict, image.Err = c.judgeOrphan(ctx, c.cfg.CachePool+"/"+image.Name, opts.Reclaim)
		}
	}

	return OrphanReport{Images: images}, nil
}

// judgeOrphan asks rbd about one old, unnamed writable volume and, when reclaim
// is set and nothing holds it, moves it to the trash.
func (c *Client) judgeOrphan(ctx context.Context, handle string, reclaim bool) (OrphanVerdict, error) {
	out, err := c.rbdCmd(ctx, true, "snap", "ls", "--all", handle)
	if err != nil {
		if isNoSuchFile(err) {
			return OrphanHalfRemoved, nil
		}

		return OrphanUnknown, fmt.Errorf("ceph: list the snapshots of %s: %w", handle, err)
	}

	var snapshots []json.RawMessage
	if err := json.Unmarshal(out, &snapshots); err != nil || snapshots == nil {
		return OrphanUnknown, fmt.Errorf("ceph: %s did not answer with a json snapshot list for %s",
			c.bin, handle)
	}
	if len(snapshots) > 0 {
		return OrphanSnapshotted, nil
	}

	_, name, _ := strings.Cut(handle, "/")

	mapped, err := c.mappedDevices(ctx, name)
	if err != nil {
		return OrphanUnknown, err
	}
	if len(mapped) > 0 {
		return OrphanWatched, nil
	}

	// THE WATCHERS LAST, immediately before the move, so the window between the
	// answer and the action is one command long.
	out, err = c.rbdCmd(ctx, true, "status", handle)
	if err != nil {
		if isNoSuchFile(err) {
			return OrphanHalfRemoved, nil
		}

		return OrphanUnknown, fmt.Errorf("ceph: read the watchers of %s: %w", handle, err)
	}

	var status struct {
		Watchers *[]json.RawMessage `json:"watchers"`
	}
	if err := json.Unmarshal(out, &status); err != nil || status.Watchers == nil {
		return OrphanUnknown, fmt.Errorf("ceph: %s did not name the watchers of %s", c.bin, handle)
	}
	if len(*status.Watchers) > 0 {
		return OrphanWatched, nil
	}

	if !reclaim {
		return OrphanReclaimable, nil
	}

	if !c.isCacheVolume(handle) {
		return OrphanUnknown, fmt.Errorf("ceph: refusing to move %s to the trash: only a writable "+
			"cache volume billet named goes there", bounded(handle))
	}

	if _, err := c.rbdCmd(ctx, false, "trash", "mv", handle); err != nil {
		if isNoSuchFile(err) {
			return OrphanGone, nil
		}

		return OrphanUnknown, fmt.Errorf("ceph: move %s to the trash: %w", handle, err)
	}

	return OrphanMoved, nil
}
