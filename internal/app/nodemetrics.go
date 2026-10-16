package app

import (
	"context"
	"fmt"

	"github.com/junioryono/billet/internal/metrics"
	"github.com/junioryono/billet/internal/store/ceph"
)

// familyNodeRBDMappings is the node's kernel RBD mappings by what accounts for
// them.
const familyNodeRBDMappings = "billet_node_rbd_mappings"

var nodeMappingFamilies = []metrics.Family{
	{Name: familyNodeRBDMappings, Labels: []string{"state"},
		Help: "This host's kernel mappings of images in the site's Ceph pools, by what accounts for each: " +
			"root_disk (a guest's root disk), generation (a read-only generation check), session (a cache " +
			"volume a cache session names), recent (a cache volume named too recently to judge), unaccounted " +
			"(a cache volume named long ago that no cache session names, which the node's eviction pass " +
			"unmaps unless the kernel holds it), or other (anything else in those pools)."},
}

// mappingStateLabels is the gauge's label for each state a read of the
// mappings gives, in the order the gauge reports them.
var mappingStateLabels = []struct {
	state ceph.MappingState
	label string
}{
	{ceph.MappingRootDisk, "root_disk"},
	{ceph.MappingGeneration, "generation"},
	{ceph.MappingInSession, "session"},
	{ceph.MappingRecent, "recent"},
	{ceph.MappingUnaccounted, "unaccounted"},
	{ceph.MappingOther, "other"},
}

// kernelMappingReader reads this host's kernel mappings: the Ceph store.
type kernelMappingReader interface {
	KernelMappings(ctx context.Context, inSession func(name string) bool) (ceph.MappingReport, error)
}

// KernelMappingMetrics is the node's kernel RBD mappings as a gauge, read at
// every scrape: the build-up of mappings no job holds (#494) is visible before
// it matters. A node with no node.ceph reports none.
func (n *Node) KernelMappingMetrics() MetricsSource {
	cfg := n.cfg

	return MetricsSource{
		name:     "rbd",
		families: nodeMappingFamilies,
		read: mappingSnapshot(cfg.Node.Ceph != nil, func() (kernelMappingReader, error) {
			client, err := ceph.New(*cfg.Node.Ceph)
			if err != nil {
				return nil, err
			}

			return client, nil
		}, CacheSessionNames(cfg)),
	}
}

// mappingSnapshot reads the mappings through open and judges them against the
// sessions records names, both afresh at every scrape. A read that fails at any
// step reports nothing, so a scrape never shows an empty table it did not see.
func mappingSnapshot(hasCeph bool, open func() (kernelMappingReader, error),
	records func() (func(string) bool, error),
) func(context.Context) (metrics.Snapshot, error) {
	return func(ctx context.Context) (metrics.Snapshot, error) {
		if !hasCeph {
			return metrics.Snapshot{}, nil
		}

		inSession, err := records()
		if err != nil {
			return nil, err
		}

		reader, err := open()
		if err != nil {
			return nil, err
		}

		report, err := reader.KernelMappings(ctx, inSession)
		if err != nil {
			return nil, err
		}

		snap := metrics.Snapshot{}
		counted := 0

		for _, s := range mappingStateLabels {
			n := report.Count(s.state)
			counted += n
			snap[familyNodeRBDMappings] = append(snap[familyNodeRBDMappings],
				metrics.Sample{Labels: []string{s.label}, Value: float64(n)})
		}

		// A STATE THE GAUGE HAS NO LABEL FOR would go uncounted, and the gauge
		// would add up to less than the table: could-not-tell, not a smaller table.
		if counted != len(report.Mappings) {
			return nil, fmt.Errorf("the kernel's mappings read in a state the gauge does not report "+
				"(%d of %d counted)", counted, len(report.Mappings))
		}

		return snap, nil
	}
}

var _ kernelMappingReader = (*ceph.Client)(nil)
