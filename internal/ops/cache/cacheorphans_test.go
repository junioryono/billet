package cache

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/store/ceph"
)

type fakeOrphanStore struct {
	opts   ceph.OrphanOptions
	report ceph.OrphanReport
}

func (f *fakeOrphanStore) ReclaimOrphans(_ context.Context, opts ceph.OrphanOptions) (ceph.OrphanReport, error) {
	f.opts = opts

	return f.report, nil
}

// THE COMMAND LISTS, AND SAYS WHY; ACTING IS ASKED FOR. Without --reclaim the
// pass is a listing, each judged volume is printed with its verdict, and a
// volume rbd could not answer for fails the command rather than reading as kept
// for a reason.
func TestCacheOrphansPrintsEachVerdictAndFailsOnCouldNotTell(t *testing.T) {
	t.Parallel()

	named := time.Date(2026, 9, 21, 8, 0, 0, 0, time.UTC)
	store := &fakeOrphanStore{report: ceph.OrphanReport{Images: []ceph.OrphanImage{
		{Name: "cache-v-1790000000-aaaaaaaaaaaaaaaaaaaaaaaa", Named: named, Verdict: ceph.OrphanReclaimable},
		{Name: "cache-v-1790000001-bbbbbbbbbbbbbbbbbbbbbbbb", Named: named, Verdict: ceph.OrphanUnknown,
			Err: errors.New("ceph: read the watchers: timed out")},
		{Name: "cache-v-1790000002-cccccccccccccccccccccccc", Named: named, Verdict: ceph.OrphanWatched},
		{Name: "cache-g-1790000003-dddddddddddddddddddddddd", Named: named, Verdict: ceph.OrphanGeneration},
		{Name: "cache-v-1790000004-eeeeeeeeeeeeeeeeeeeeeeee", Named: named, Verdict: ceph.OrphanMoveUnknown,
			Err: errors.New("ceph: move to the trash, which may have happened: deadline exceeded")},
	}}}

	var out bytes.Buffer

	opts := ceph.OrphanOptions{OlderThan: ceph.DefaultOrphanAge, Limit: 7, InSession: func(string) bool { return false }}
	err := reclaimCacheOrphans(t.Context(), &out, store, opts)

	var exit *cli.ExitError
	if !errors.As(err, &exit) || exit.Code != 1 ||
		!strings.Contains(exit.Msg, "1 cache volume(s) could not be judged") ||
		!strings.Contains(exit.Msg, "1 move(s) to the trash were not confirmed") {
		t.Errorf("a could-not-tell volume and an unconfirmed move returned %v, want exit status 1 "+
			"naming both", err)
	}

	if store.opts.Reclaim || store.opts.Limit != 7 || store.opts.InSession == nil {
		t.Errorf("the pass was asked with %+v", store.opts)
	}

	text := out.String()
	for _, want := range []string{
		"reclaimable  cache-v-1790000000-aaaaaaaaaaaaaaaaaaaaaaaa",
		"could not tell  cache-v-1790000001-bbbbbbbbbbbbbbbbbbbbbbbb",
		"read the watchers: timed out",
		"held open by a client",
		"a generation",
		"nothing was changed; run again with --reclaim to move the 1 reclaimable volume(s)",
	} {
		if !strings.Contains(squeeze(text), want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}

	if strings.Contains(text, "cache-g-1790000003") {
		t.Errorf("a generation was listed by name:\n%s", text)
	}
}

// A MOVE RBD DID NOT CONFIRM FAILS THE COMMAND ON ITS OWN, because it may have
// happened and the operator has to look.
func TestCacheOrphansFailsOnAnUnconfirmedMove(t *testing.T) {
	t.Parallel()

	store := &fakeOrphanStore{report: ceph.OrphanReport{Images: []ceph.OrphanImage{
		{Name: "cache-v-1790000000-aaaaaaaaaaaaaaaaaaaaaaaa", Verdict: ceph.OrphanMoved},
		{Name: "cache-v-1790000004-eeeeeeeeeeeeeeeeeeeeeeee", Verdict: ceph.OrphanMoveUnknown,
			Err: errors.New("deadline exceeded")},
	}}}

	var out bytes.Buffer

	err := reclaimCacheOrphans(t.Context(), &out, store, ceph.OrphanOptions{
		OlderThan: ceph.DefaultOrphanAge, Limit: 7, Reclaim: true, InSession: func(string) bool { return false },
	})

	var exit *cli.ExitError
	if !errors.As(err, &exit) || exit.Code != 1 || !strings.Contains(exit.Msg, "not confirmed") {
		t.Errorf("an unconfirmed move returned %v, want exit status 1 saying so", err)
	}

	if !strings.Contains(out.String(), "moved 1 volume(s) to the trash") {
		t.Errorf("output does not count the confirmed move:\n%s", out.String())
	}
}

// squeeze collapses tabwriter padding to two spaces, so an assertion names the
// columns and not their widths.
func squeeze(s string) string {
	for strings.Contains(s, "   ") {
		s = strings.ReplaceAll(s, "   ", "  ")
	}

	return s
}

// A NODE THAT KEEPS CACHE SESSIONS MUST HAVE THEM READ. Only a node with no cache
// listener, which never kept a session, may stand on a missing directory.
func TestCacheOrphansNeedTheSessionsOfANodeThatKeepsThem(t *testing.T) {
	t.Parallel()

	withoutListener := &config.Config{Node: &config.NodeConfig{StateDir: t.TempDir()}}
	if _, err := app.CacheSessionRecords(withoutListener); err != nil {
		t.Errorf("a node without a cache listener was refused: %v", err)
	}

	withListener := &config.Config{Node: &config.NodeConfig{
		StateDir: t.TempDir(), Cache: &config.NodeCacheConfig{},
	}}
	if _, err := app.CacheSessionRecords(withListener); err == nil {
		t.Error("a cache node whose sessions could not be read was judged anyway")
	}
}
