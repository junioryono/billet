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
	// OrphanCreatedRecently is a volume the cluster created within the bound,
	// by this node's clock or against the newest image it created.
	OrphanCreatedRecently OrphanVerdict = "created too recently"
	OrphanInSession       OrphanVerdict = "named by a cache session on this node"
	OrphanInIndex         OrphanVerdict = "named by the cache index"
	OrphanDeferred        OrphanVerdict = "beyond this pass's limit"
	OrphanWatched         OrphanVerdict = "held open by a client"
	OrphanSnapshotted     OrphanVerdict = "has snapshots"
	OrphanUsed            OrphanVerdict = "snapshotted for publication too recently"
	OrphanHalfRemoved     OrphanVerdict = "half-removed, which the purge finishes"
	OrphanGone            OrphanVerdict = "no longer listed under its name"
	OrphanUnknown         OrphanVerdict = "could not tell"
	// OrphanMoveUnknown is a move rbd did not confirm, which may have happened.
	OrphanMoveUnknown OrphanVerdict = "move not confirmed"
)

// OrphanOptions shapes one ReclaimOrphans pass. Its zero value is refused.
type OrphanOptions struct {
	// OlderThan is how long ago a volume must have been named, at least
	// OrphanMinimumAge.
	OlderThan time.Duration
	// Limit bounds how many volumes one pass moves, or lists as reclaimable,
	// and so what it hands the purge; every older volume it keeps is asked
	// about on the way.
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
// no record in the cache index; and then, asking rbd, created by the cluster at
// least OlderThan ago, by this node's clock and before the newest creation the
// cluster reports; no snapshot in any namespace, so no generation's lineage
// reads it; no mapping on this host; no
// watcher, so no client anywhere has it open; and no `used_at` Snapshot wrote
// within OlderThan. Any answer rbd gives other than those keeps the image as
// OrphanUnknown.
//
// THE WATCHER CHECK IS THE GUARD AGAINST AN IMAGE IN USE. `rbd trash mv` refuses
// only an image whose exclusive lock it cannot take, which a volume Create makes
// does not have and a krbd mapping of a clone hands over on request, so it moves
// a mapped image (librbd Trash::move, Ceph v19.2.2, read rather than measured).
// Nothing maps a volume after the job that named it, so the age bound is what
// keeps a volume from being mapped between the check and the move. The purge's
// `trash rm` does refuse an image with watchers.
//
// `used_at` IS READ AFTER THE WATCHERS. Snapshot, on any node, writes it on the
// volume before it unmaps it to take the publication's snapshot, so a volume
// whose watcher is gone because its job is publishing shows a fresh `used_at` to
// any read that follows the watcher check.
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

	index, err := c.cacheIndexText(ctx)
	if err != nil {
		return OrphanReport{}, err
	}

	images := make([]OrphanImage, 0, len(names))
	for _, name := range names {
		_, named, _ := cacheImageName(name)
		images = append(images, OrphanImage{Name: name, Named: named})
	}

	// OLDEST FIRST, so a bounded pass works through a backlog in the order it
	// was left.
	slices.SortStableFunc(images, func(a, b OrphanImage) int { return a.Named.Compare(b.Named) })

	now := c.now()
	taken := 0

	var (
		clusterNow time.Time
		clusterErr error
	)

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
		case taken >= opts.Limit:
			image.Verdict = OrphanDeferred
		default:
			if err := ctx.Err(); err != nil {
				image.Verdict, image.Err = OrphanUnknown, err

				continue
			}

			if clusterNow.IsZero() && clusterErr == nil {
				clusterNow, clusterErr = c.clusterNow(ctx, images)
			}
			if clusterErr != nil {
				image.Verdict, image.Err = OrphanUnknown, clusterErr

				continue
			}

			image.Verdict, image.Err = c.judgeOrphan(ctx, image.Name, opts, now, clusterNow)

			// ONLY WHAT IS TAKEN COUNTS, so the volumes a pass keeps (half-removed
			// ones the purge is still finishing, above all) never use up the
			// limit of every later pass ahead of the orphans behind them.
			switch image.Verdict {
			case OrphanReclaimable, OrphanMoved, OrphanMoveUnknown:
				taken++
			}
		}
	}

	return OrphanReport{Images: images}, nil
}

// cacheIndexText is every key and value the cache index holds, as one string to
// search. It asks for json, which rbd prints as one object, or nothing at all
// for an index with no metadata; a cold site with no index image holds no record.
func (c *Client) cacheIndexText(ctx context.Context) (string, error) {
	out, err := c.rbdCmd(ctx, true, "image-meta", "list", c.cacheIndex())
	if err != nil {
		if isNoSuchFile(err) {
			return "", nil
		}

		return "", fmt.Errorf("ceph: list the cache index: %w", err)
	}

	if len(trimSpace(out)) == 0 {
		return "", nil
	}

	var metadata map[string]string
	if err := json.Unmarshal(out, &metadata); err != nil || metadata == nil {
		return "", fmt.Errorf("ceph: %s did not answer with the cache index as a json object", c.bin)
	}

	var text strings.Builder
	for key, value := range metadata {
		text.WriteString(key + " " + value + "\n")
	}

	return text.String(), nil
}

// mappedHere reports whether this host maps an image of that name from any pool.
// An entry it cannot read is could-not-tell, never "not this one".
func (c *Client) mappedHere(ctx context.Context, name string) (bool, error) {
	out, err := c.rbdCmd(ctx, true, "device", "list")
	if err != nil {
		return false, fmt.Errorf("ceph: list the mapped rbd devices: %w", err)
	}

	var devices []*mappedDevice
	if err := json.Unmarshal(trimSpace(out), &devices); err != nil || devices == nil {
		return false, fmt.Errorf("ceph: %s did not answer with a json device list", c.bin)
	}

	for _, device := range devices {
		if device == nil || device.Pool == "" || device.Name == "" {
			return false, fmt.Errorf("ceph: %s listed a mapped device without its pool and image", c.bin)
		}

		if device.Name == name {
			return true, nil
		}
	}

	return false, nil
}

// createdAt is when the cluster created an image: create_timestamp, which the
// OSD that created the header sets (cls_rbd create) and rbd prints with ctime in
// this process's zone (Ceph v19.2.2, read rather than measured). An `rbd info`
// error is returned as it came, so a caller can tell ENOENT.
func (c *Client) createdAt(ctx context.Context, handle string) (time.Time, error) {
	out, err := c.rbdCmd(ctx, true, "info", handle)
	if err != nil {
		return time.Time{}, fmt.Errorf("ceph: describe %s: %w", handle, err)
	}

	var info struct {
		CreateTimestamp string `json:"create_timestamp"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return time.Time{}, fmt.Errorf("ceph: %s did not describe %s as json", c.bin, handle)
	}

	created, err := time.ParseInLocation(time.ANSIC, info.CreateTimestamp, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("ceph: %s did not say when the cluster created %s", c.bin, handle)
	}

	return created, nil
}

// clusterNowProbes bounds how many of the newest images a pass asks for a
// creation time before it gives up on the cluster's clock.
const clusterNowProbes = 8

// clusterNow is a lower bound on the cluster's clock: the creation time of the
// newest-named image billet named that the cluster will describe.
func (c *Client) clusterNow(ctx context.Context, images []OrphanImage) (time.Time, error) {
	probed := 0

	for i := len(images) - 1; i >= 0 && probed < clusterNowProbes; i-- {
		if _, _, ok := cacheImageName(images[i].Name); !ok {
			continue
		}

		probed++

		created, err := c.createdAt(ctx, c.cfg.CachePool+"/"+images[i].Name)
		if err == nil {
			return created, nil
		}
	}

	return time.Time{}, fmt.Errorf("ceph: none of the %d newest cache images said when the cluster "+
		"created it, so no creation can be dated against the cluster's clock", probed)
}

// judgeOrphan asks rbd about one old, unnamed writable volume and, when reclaim
// is set and nothing holds it, moves it to the trash.
func (c *Client) judgeOrphan(
	ctx context.Context, name string, opts OrphanOptions, now, clusterNow time.Time,
) (OrphanVerdict, error) {
	handle := c.cfg.CachePool + "/" + name

	// THE CLUSTER'S CLOCK, NOT ONLY A NODE'S. The name carries the creating
	// node's clock and now is this node's; either may be wrong by more than the
	// bound. The volume must also have been created OlderThan before the newest
	// creation the cluster reports for any listed image, which no node's clock
	// enters.
	created, err := c.createdAt(ctx, handle)
	if err != nil {
		if isNoSuchFile(err) {
			return OrphanHalfRemoved, nil
		}

		return OrphanUnknown, err
	}
	if now.Sub(created) < opts.OlderThan || clusterNow.Sub(created) < opts.OlderThan {
		return OrphanCreatedRecently, nil
	}

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

	mapped, err := c.mappedHere(ctx, name)
	if err != nil {
		return OrphanUnknown, err
	}
	if mapped {
		return OrphanWatched, nil
	}

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

	// AFTER THE WATCHERS, for the reason ReclaimOrphans gives, and as the whole
	// metadata list: a get answers ENOENT alike for a missing key and a missing
	// image, and only the first says the volume was never published from.
	out, err = c.rbdCmd(ctx, true, "image-meta", "list", handle)
	if err != nil {
		if isNoSuchFile(err) {
			return OrphanGone, nil
		}

		return OrphanUnknown, fmt.Errorf("ceph: read the metadata of %s: %w", handle, err)
	}

	metadata := map[string]string{}
	if len(trimSpace(out)) > 0 {
		if err := json.Unmarshal(out, &metadata); err != nil || metadata == nil {
			return OrphanUnknown, fmt.Errorf("ceph: %s did not answer with the metadata of %s as a json "+
				"object", c.bin, handle)
		}
	}

	if value, found := metadata[cacheMetaPrefix+"used_at"]; found {
		usedAt, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return OrphanUnknown, fmt.Errorf("ceph: %s records an unreadable use time %s", handle,
				bounded(value))
		}

		if now.Sub(usedAt) < opts.OlderThan || clusterNow.Sub(usedAt) < opts.OlderThan {
			return OrphanUsed, nil
		}
	}

	if !opts.Reclaim {
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

		return OrphanMoveUnknown, fmt.Errorf("ceph: move %s to the trash, which may have happened: %w",
			handle, err)
	}

	return OrphanMoved, nil
}
