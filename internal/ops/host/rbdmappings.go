package host

import (
	"context"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/store/ceph"
)

// mappingNamesShown bounds how many unaccounted mappings a check names.
const mappingNamesShown = 5

// reportKernelMappings prints this host's kernel mappings of the site's pools by
// what accounts for each, and names the cache volumes no cache session does.
//
// REPORTED, NEVER FATAL: a leaked mapping costs a device and keeps its volume
// from eviction, and the node's own eviction pass releases it; the check is
// where a build-up is seen (#494). A table or a session record it cannot read
// (the records are root's, so a check run as another account cannot) is said
// as could-not-tell, never as no mappings.
func reportKernelMappings(ctx context.Context, env cli.Env, cfg *config.Config) {
	report, err := readKernelMappings(ctx, cfg)
	printKernelMappings(env, report, err)
}

func readKernelMappings(ctx context.Context, cfg *config.Config) (ceph.MappingReport, error) {
	inSession, err := app.CacheSessionNames(cfg)()
	if err != nil {
		return ceph.MappingReport{}, err
	}

	client, err := ceph.New(*cfg.Node.Ceph)
	if err != nil {
		return ceph.MappingReport{}, err
	}

	return client.KernelMappings(ctx, inSession)
}

// printKernelMappings is the check's account of one read of the mappings.
func printKernelMappings(env cli.Env, report ceph.MappingReport, err error) {
	if err != nil {
		fmt.Fprintf(env.Stdout, "rbd      could not tell which kernel mappings this host holds, so none is "+
			"counted: %v\n", err)

		return
	}

	var parts []string

	for _, s := range []struct {
		state ceph.MappingState
		what  string
	}{
		{ceph.MappingRootDisk, "guest root disk(s)"},
		{ceph.MappingInSession, "cache volume(s) a cache session names"},
		{ceph.MappingRecent, "cache volume(s) named too recently to judge"},
		{ceph.MappingGeneration, "generation check(s)"},
		{ceph.MappingUnaccounted, "cache volume(s) no cache session names"},
		{ceph.MappingOther, "other mapping(s) in the site's pools"},
	} {
		if n := report.Count(s.state); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s.what))
		}
	}

	// FROM THE TABLE, NOT FROM WHAT WAS SAID ABOUT IT: a state left out of the
	// list above must not turn a host's mappings into none.
	if len(report.Mappings) == 0 {
		fmt.Fprintln(env.Stdout, "rbd      this host maps nothing from the site's pools")

		return
	}

	if len(parts) == 0 {
		parts = []string{"none of a kind this check names"}
	}

	fmt.Fprintf(env.Stdout, "rbd      %d kernel mapping(s) here: %s\n", len(report.Mappings),
		strings.Join(parts, ", "))

	var unaccounted []string

	for _, m := range report.Mappings {
		if m.State == ceph.MappingUnaccounted {
			unaccounted = append(unaccounted, m.Pool+"/"+m.Name)
		}
	}

	if len(unaccounted) == 0 {
		return
	}

	shown := unaccounted
	if len(shown) > mappingNamesShown {
		shown = append(shown[:mappingNamesShown:mappingNamesShown],
			fmt.Sprintf("and %d more", len(unaccounted)-mappingNamesShown))
	}

	fmt.Fprintf(env.Stdout, "         mapped, named over %s ago and held by no job: %s; the node's eviction "+
		"pass unmaps them, never forced, unless the kernel holds one open\n", ceph.VolumeMapAge,
		strings.Join(shown, ", "))
}
