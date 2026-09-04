package main

import (
	"errors"
	"fmt"

	"github.com/junioryono/billet/internal/config"
)

// judgeNodeCache says what this node's guests will get from the site's store,
// and refuses the one shape whose answer is "nothing, silently".
//
// A STORE AND A LISTENER ARE HALVES OF ONE THING, and config validates only one
// direction: an EC2 node.cache is refused without node.ebs_s3, and node.ceph is
// refused on a backend that cannot attach a block device. The mirror — a store
// with nothing serving it — loaded, started and ran jobs with no cache at all.
// Measured 2026-09-04: /var/lib/docker was the instance's root volume, the guest
// reported cache: COLD, the bucket held no state object, and the decommission
// that followed purged 0 snapshots, 0 volumes and 0 state objects.
//
// IT IS NOT A LOAD-TIME REFUSAL, because two commands legitimately read a store
// block on a config that has already lost its listener: `billet decommission`
// purges what the cache left behind, and `billet init iam` renders its grants.
//
// THE TWO STORES DO NOT WEIGH THE SAME, which is why the refusal is returned
// separately from the report. node.ebs_s3 exists only to back node.cache —
// nothing else on a running node reads it — so without a listener the whole
// block is inert, and that is a refusal. node.ceph also holds image_pool, which
// every microVM boots a clone of; it is REQUIRED for firecracker; and it is what
// `billet init --provider firecracker` writes when no cache endpoint was given.
// So there the unreachable cache_pool is reported and the deployment runs.
func judgeNodeCache(cfg *config.Config) ([]string, error) {
	if cfg.Node == nil {
		return nil, nil
	}

	n := cfg.Node

	// SAID WHEN IT IS RIGHT, TOO. A report that speaks only about a broken cache
	// leaves an operator with a working one unable to confirm the endpoint their
	// guests were handed, and it is this line that makes the absence below read
	// as a finding rather than as billet having nothing to say about caches.
	//
	// WHAT IT CLAIMS IS THE CONFIGURATION AND NOT A PROBE. Nothing here dials the
	// listener, and "guests reach X" would be read as proof that they do — the
	// distinction this command is careful about everywhere else. It says what
	// billet will hand each guest, and says that is all it says.
	if n.Cache != nil {
		return []string{"guests are handed " + n.Cache.GuestEndpoint + servedFrom(n) +
			" (configured, not probed)"}, nil
	}

	switch {
	case n.EBSS3 != nil:
		return []string{
				"NONE: node.ebs_s3 names a cache store and this node offers no node.cache " +
					"endpoint, so no guest can ask it for anything",
				// EVERY CLAIM SCOPED TO THIS NODE AND TO BILLET'S CACHE. Earlier
				// drafts said "pulls every image cold", which is false of an AMI
				// that baked images in, and "no state object is ever created in
				// <bucket>", which is a claim about a bucket other nodes and other
				// deployments also write to. A report an operator can disprove by
				// looking is one they stop believing.
				"every job placed on THIS NODE keeps /var/lib/docker on the instance's " +
					"ROOT VOLUME, starting at whatever the AMI baked in and pulling the " +
					"rest cold",
				// "THIS DEPLOYMENT'S PREFIX" IS THE REPORT'S OWN VOCABULARY, and it
				// is the true one: objects are keyed by deployment and site whether
				// or not node.ebs_s3.prefix is set, so naming that field instead
				// would describe nothing on a config that leaves it empty.
				"and this node publishes nothing for a later job: no cache volume, no " +
					"snapshot, and no state object under this deployment's prefix in " +
					n.EBSS3.Bucket,
				"add node.cache (listen, guest_endpoint, tls_cert and tls_key), or drop " +
					"node.ebs_s3",
			}, errors.New("node.ebs_s3 names a cache store nothing can reach: this " +
				"node offers no node.cache endpoint, so every job placed here runs on the " +
				"instance's root volume and publishes nothing. Add node.cache (listen, " +
				"guest_endpoint and the TLS pair an EC2 guest needs), or drop node.ebs_s3 — " +
				"refused here rather than at config load because `billet decommission` and " +
				"`billet init iam` both read this block on a config that has lost its listener")

	case n.Ceph != nil:
		return []string{
			fmt.Sprintf("node.ceph.cache_pool %s is a cache nothing can reach: this node "+
				"offers no node.cache endpoint, so no guest can ask for a volume",
				n.Ceph.CachePool),
			// WHAT IS LOST IS BILLET'S CACHE AND NOTHING ELSE. An earlier draft
			// said "the Actions cache is unavailable", which is false and would
			// send an operator to debug a step that works: interception is off
			// without this listener, so actions/cache reaches GitHub exactly as it
			// does on a hosted runner. The loss is the local one.
			"a job placed on THIS NODE gets no billet cache volume: no sticky disk, no " +
				"local Actions-cache interception, and no published generation " +
				"(actions/cache still reaches GitHub, and the guest's image store starts " +
				"at whatever the golden image baked in)",
			fmt.Sprintf("image_pool %s is unaffected: every guest still boots a clone of "+
				"it; add node.cache (listen and guest_endpoint on the guest bridge) to "+
				"serve a cache as well", n.Ceph.ImagePool),
		}, nil
	}

	return nil, nil
}

// servedFrom names the store behind a configured endpoint, so the positive line
// says which authority the guests reach rather than only where.
//
// THE MISSING CASE IS NAMED RATHER THAN LEFT BLANK. A loaded config always has a
// store — validateCacheNode admits only firecracker and ec2, and each of those
// requires its own — but this function does not get to assume its argument came
// through config.Load, which is the same reason alloc.New re-applies rules
// config has already checked. Rendering nothing there would turn a listener the
// node cannot even start ("provider %s has no cache store") into a line that
// reads like a working cache.
func servedFrom(n *config.NodeConfig) string {
	switch {
	case n.EBSS3 != nil:
		return fmt.Sprintf(", served from the ebs-s3 store in %s (bucket %s)",
			n.EBSS3.AvailabilityZone, n.EBSS3.Bucket)
	case n.Ceph != nil:
		return ", served from the ceph pool " + n.Ceph.CachePool
	}

	return ", served from NO STORE: this node declares neither node.ebs_s3 nor " +
		"node.ceph, and will refuse to start"
}

// printNodeCache renders the report in the column `billet check` uses.
//
// THE LABEL IS WRITTEN ONCE and the continuations are indented under it, because
// a finding that repeats its label on every line reads as several findings —
// which is how the surrounding report already treats a multi-sentence verdict.
func printNodeCache(lines []string) {
	for i, line := range lines {
		label := ""
		if i == 0 {
			label = "cache"
		}

		fmt.Printf("%-8s %s\n", label, line)
	}
}
