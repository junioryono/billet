package ceph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// VolumeMapAge is how long ago a writable cache volume must have been named
// before a mapping of it that no cache session names is taken for one nothing
// will record.
//
// A WRITABLE VOLUME IS MAPPED ONCE, BY Create OR Clone, under a name they give
// it then by this host's clock, and is unmapped only to be snapshotted or
// discarded; nothing maps one again. Its custody record follows within the
// cache handler's bound (under 13 minutes), after at most a lineage copy
// (cacheCompactionLimit) and a map (MapTimeout), so a mapping this much older
// than its name that no record names will never be named by one. Six hours is
// the node's eviction cadence, which is what runs the sweep.
const VolumeMapAge = 6 * time.Hour

// mapSightingGap is how long, by this process's monotonic clock, a mapping must
// have stayed unaccounted across passes before the sweep unmaps it.
//
// THE NAME IS WALL-CLOCK, AND A CLOCK STEPPED FORWARD BY VolumeMapAge WOULD AGE
// A VOLUME STILL WAITING FOR ITS RECORD. A second sighting an hour later on the
// monotonic clock, which no step moves, outlasts the longest window between a
// map and its record (the cache handler's bound, under 13 minutes) several
// times over. It starts again with the process, so a restart only delays a
// release.
const mapSightingGap = time.Hour

// MappingState is what KernelMappings or SweepKernelMappings made of one kernel
// mapping of an image in billet's pools.
type MappingState string

// The states. Only MappingUnaccounted is a mapping nothing accounts for; the
// sweep turns each into MappingUnmapped, MappingHeld or MappingUnknown.
const (
	// MappingRootDisk is a guest's root disk, which its jail accounts for.
	MappingRootDisk MappingState = "root disk"
	// MappingGeneration is a read-only map of a generation's snapshot, which a
	// generation check holds under the host's generation-map lock.
	MappingGeneration MappingState = "generation check"
	// MappingInSession is a writable cache volume a cache custody record names.
	MappingInSession MappingState = "named by a cache session"
	// MappingRecent is a writable cache volume named within VolumeMapAge, whose
	// custody record may not be written yet.
	MappingRecent MappingState = "named too recently"
	// MappingUnaccounted is a writable cache volume named long ago that no cache
	// custody record names.
	MappingUnaccounted MappingState = "named by no cache session"
	// MappingOther is any other mapping in billet's pools: not one a node makes
	// for a job, so nothing here judges it.
	MappingOther MappingState = "not a mapping a node makes for a job"
	// MappingUnmapped is an unaccounted mapping the sweep released.
	MappingUnmapped MappingState = "unmapped"
	// MappingHeld is an unaccounted mapping the kernel refused to release because
	// something still has it open.
	MappingHeld MappingState = "held open"
	// MappingUnknown is an unaccounted mapping whose unmap failed otherwise, or
	// was not started.
	MappingUnknown MappingState = "could not tell"
)

// KernelMapping is one row of this host's kernel mapping table in billet's
// pools, and what was made of it.
type KernelMapping struct {
	Pool, Namespace, Name, Snap, Device string
	State                               MappingState
	// Err is why the sweep could not release it, for MappingHeld and
	// MappingUnknown.
	Err error
}

// MappingReport is every mapping of an image in billet's pools on this host, in
// the order the kernel listed them.
type MappingReport struct {
	Mappings []KernelMapping
}

// Count reports how many mappings are in a state.
func (r MappingReport) Count(state MappingState) int {
	n := 0

	for _, m := range r.Mappings {
		if m.State == state {
			n++
		}
	}

	return n
}

// KernelMappings reads this host's kernel mappings of billet's two pools and
// says what accounts for each, changing nothing. inSession reports whether a
// cache custody record on this node names an image. A mapping table it cannot
// read, or a row in billet's pools that does not say which snapshot it maps, is
// an error, never an empty report or a verdict.
//
// HOLDERS AND MOUNTS PROVE NOTHING HERE, so neither is read. On the reference
// deployment's Linux node, `/sys/block/rbdN/holders` was empty and the host's
// `/proc/mounts` listed nothing for every mapping, live jobs' volumes included
// (2026-10-16): the node's unit mounts in a mount namespace of its own and a VMM
// opens its device inside its jail. What does prove it is the kernel's own
// refusal to unmap a device something has open, which only SweepKernelMappings
// asks.
func (c *Client) KernelMappings(ctx context.Context, inSession func(name string) bool) (MappingReport, error) {
	if inSession == nil {
		return MappingReport{}, errors.New("ceph: the kernel's mappings cannot be judged without this " +
			"node's cache sessions")
	}

	out, err := c.rbdCmd(ctx, true, "device", "list")
	if err != nil {
		return MappingReport{}, fmt.Errorf("ceph: list the mapped rbd devices: %w", err)
	}

	// A `null` WOULD READ AS NOTHING MAPPED, and an entry without its pool or
	// name as somebody else's: both are could-not-tell, as in mappedHere.
	var devices []*mappedDevice
	if err := json.Unmarshal(trimSpace(out), &devices); err != nil || devices == nil {
		return MappingReport{}, fmt.Errorf("ceph: %s did not answer with a json device list", c.bin)
	}

	now := c.now()

	var report MappingReport

	for _, d := range devices {
		if d == nil || d.Pool == "" || d.Name == "" {
			return MappingReport{}, fmt.Errorf("ceph: %s listed a mapped device without its pool and image",
				c.bin)
		}

		if d.Pool != c.cfg.CachePool && d.Pool != c.cfg.ImagePool {
			continue
		}

		// WHETHER IT IS AN IMAGE'S HEAD IS PART OF THE PROOF, so a row that does
		// not say is could-not-tell rather than somebody else's.
		if d.Snap == "" {
			return MappingReport{}, fmt.Errorf("ceph: %s listed %s/%s without saying which snapshot it maps",
				c.bin, d.Pool, bounded(d.Name))
		}

		report.Mappings = append(report.Mappings, KernelMapping{
			Pool: d.Pool, Namespace: d.Namespace, Name: d.Name, Snap: d.Snap, Device: d.Device,
			State: c.mappingState(d, now, inSession),
		})
	}

	return report, nil
}

// mappingState judges one mapping in billet's pools. Only the pools' default
// namespace holds what billet names, and `pool/name` addresses nothing else.
func (c *Client) mappingState(d *mappedDevice, now time.Time, inSession func(string) bool) MappingState {
	head := d.Snap == "-"
	kind, named, ok := cacheImageName(d.Name)

	switch {
	case d.Pool != c.cfg.CachePool || d.Namespace != "":
		return MappingOther
	case head && strings.HasPrefix(d.Name, "billet-"):
		return MappingRootDisk
	case ok && kind == "g" && !head:
		return MappingGeneration
	case !ok || kind != "v" || !head:
		return MappingOther
	case inSession(d.Name):
		return MappingInSession
	case now.Sub(named) < VolumeMapAge:
		return MappingRecent
	}

	return MappingUnaccounted
}

// SweepKernelMappings unmaps, never forced, this host's kernel mappings of the
// writable cache volumes nothing accounts for, and reports every mapping in
// billet's pools with what became of it. A mapping the kernel holds open, or
// whose unmap failed otherwise, is kept and joined into the error by name; one
// seen unaccounted for the first time stays MappingUnaccounted until a later
// pass sees it again.
//
// ONLY ON POSITIVE PROOF, in this order: the exact name Create and Clone give a
// writable volume, in the cache pool's default namespace and not a snapshot;
// named at least VolumeMapAge ago by this host's clock; named by no cache
// custody record on this node, read afresh for this pass before the table, so a
// session that ends meanwhile unmaps its own volume rather than leaving it to be
// judged; found so by an earlier pass at least mapSightingGap before, by the
// monotonic clock; and then the kernel's agreement, since a plain unmap succeeds
// only when nothing holds the device open. A pass without a session reader, or
// whose records cannot be read, unmaps nothing.
//
// LEFT BEHIND FOR TEN DAYS ON THE REFERENCE NODE: eight such mappings, from the
// 2026-10-03 incident in which its kernel client was blocklisted, survived every
// restart, and each kept its volume from eviction (judgeOrphan keeps a mapped
// volume); each unmapped cleanly without force on 2026-10-14.
//
// BY IMAGE, NOT BY DEVICE: the kernel gives a released device's number to the
// next map, and the proof is about the image. No other billet path unmaps an
// image no session or jail names, so nothing else resolves the same spec
// meanwhile. ONE UNMAP PER IMAGE A PASS, because `rbd device unmap <spec>`
// releases whichever of its mappings comes first, and the next pass takes the
// next. ONE ATTEMPT, where unmapDevice retries: busy here is a verdict, not a
// teardown racing a VMM's exit. And never killed by the caller, for
// unmapDevice's reason: an unmap cut short leaves the image's lock with the
// host's one kernel client. No unmap starts once ctx is done, so a caller that
// cancels waits at most one MapTimeout for the pass to return.
func (c *Client) SweepKernelMappings(ctx context.Context) (MappingReport, error) {
	if c.sessions == nil {
		return MappingReport{}, errors.New("ceph: kernel mappings are kept: this client was given no way " +
			"to read the node's cache sessions")
	}

	inSession, err := c.sessions()
	if err == nil && inSession == nil {
		err = errors.New("the reader returned none")
	}
	if err != nil {
		return MappingReport{}, fmt.Errorf("ceph: kernel mappings are kept: could not tell which volumes "+
			"the node's cache sessions hold: %w", err)
	}

	report, err := c.KernelMappings(ctx, inSession)
	if err != nil {
		return MappingReport{}, fmt.Errorf("ceph: kernel mappings are kept: %w", err)
	}

	c.mapSightingsMu.Lock()
	defer c.mapSightingsMu.Unlock()

	// c.now carries the monotonic reading outside tests, which Sub uses, so no
	// wall-clock step moves a sighting.
	now := c.now()
	sighted := map[string]time.Time{}
	tried := map[string]bool{}

	var failures []error

	for i := range report.Mappings {
		m := &report.Mappings[i]
		if m.State != MappingUnaccounted {
			continue
		}

		spec := m.Pool + "/" + m.Name

		key := m.Device + " " + spec
		first, seen := c.mapSightings[key]
		if !seen {
			first = now
		}
		sighted[key] = first

		if now.Sub(first) < mapSightingGap || tried[spec] {
			continue
		}

		// NO UNMAP STARTS ONCE THE PASS IS CANCELLED; one already started runs to
		// its own bound.
		if err := ctx.Err(); err != nil {
			m.State, m.Err = MappingUnknown, fmt.Errorf("ceph: the sweep stopped before unmapping %s: %w",
				spec, err)
			failures = append(failures, m.Err)

			continue
		}

		tried[spec] = true

		err := c.rbdCmdWithin(context.WithoutCancel(ctx), MapTimeout, "device", "unmap", spec)

		switch {
		case err == nil:
			m.State = MappingUnmapped
			delete(sighted, key)
		case isDeviceBusy(err):
			m.State, m.Err = MappingHeld, fmt.Errorf("ceph: %s is mapped and named by no cache session, and "+
				"the kernel holds it open, so it is kept: %w", spec, err)
			failures = append(failures, m.Err)
		default:
			m.State, m.Err = MappingUnknown, fmt.Errorf("ceph: unmap %s, which no cache session names: %w",
				spec, err)
			failures = append(failures, m.Err)
		}
	}

	c.mapSightings = sighted

	return report, errors.Join(failures...)
}
