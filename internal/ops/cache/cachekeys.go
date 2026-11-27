package cache

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/store/ceph"
)

// keyGenerationsShown is how many keys the status names, most generations first.
const keyGenerationsShown = 10

// keyGenerationReader is what printKeyGenerations reads: the Ceph store, or a test's.
type keyGenerationReader interface {
	KeyGenerations(ctx context.Context) ([]ceph.KeyGenerations, error)
}

// openKeyGenerations opens the site's store for a node with a ceph block.
var openKeyGenerations = func(cfg *config.CephConfig) (keyGenerationReader, error) {
	client, err := ceph.New(*cfg)
	if err != nil {
		// NOT the typed nil: a nil *Client in the interface would pass a nil check.
		return nil, err
	}

	return client, nil
}

// reportKeyGenerations prints each key's generations on a node whose site stores
// its caches in Ceph (#799). A read it cannot make is said and fails nothing: the
// rest of the status does not depend on it.
func reportKeyGenerations(ctx context.Context, w io.Writer, cfg *config.Config, now time.Time) {
	if cfg.Node == nil || cfg.Node.Ceph == nil {
		return
	}

	client, err := openKeyGenerations(cfg.Node.Ceph)
	if err != nil {
		fmt.Fprintf(w, "\ncache generations UNCHECKED: %v\n", err)

		return
	}

	keys, err := client.KeyGenerations(ctx)
	if err != nil {
		fmt.Fprintf(w, "\ncache generations UNCHECKED: %v\n", err)

		return
	}

	printKeyGenerations(w, keys, cfg.Node.Ceph.CachePool, now)
}

// printKeyGenerations names the keys holding the most generations in the pool.
func printKeyGenerations(w io.Writer, keys []ceph.KeyGenerations, pool string, now time.Time) {
	total := 0
	for _, k := range keys {
		total += k.Generations
	}

	fmt.Fprintf(w, "\ncache generations in %s: %d across %d key(s)\n", pool, total, len(keys))

	if len(keys) == 0 {
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  GENERATIONS\tSUPERSEDED\tLAST USED\tKEY")

	for i, k := range keys {
		if i == keyGenerationsShown {
			break
		}

		used := "never"
		if !k.LastUsed.IsZero() {
			used = now.Sub(k.LastUsed).Round(time.Minute).String() + " ago"
		}

		fmt.Fprintf(tw, "  %d\t%d\t%s\t%s\n", k.Generations, k.Superseded, used, k.Key)
	}

	_ = tw.Flush()

	if len(keys) > keyGenerationsShown {
		fmt.Fprintf(w, "  (and %d more key(s))\n", len(keys)-keyGenerationsShown)
	}
}
