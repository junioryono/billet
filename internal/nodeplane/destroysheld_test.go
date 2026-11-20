package nodeplane

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/nodeapi"
)

// A DESTROY THE NODE SAYS IT HOLDS IS NOT GIVEN BACK, and the command behind it is
// handed over instead (VersionDestroysBeside): a node running destroys beside its
// loop polls while one runs, and a plane that read that poll as a lost destroy gave
// the same one on every poll and never the launch behind it. One the node does not
// name is still lost, and given again.
func TestADestroyTheNodeHoldsIsNotGivenBackButTheNextCommandIs(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithClock(newTestClock().now), WithCommandTimeout(time.Hour))
	p.SetPollWindowForTest(50 * time.Millisecond)
	registerIncarnation(t, p, "holder", "holder-1")

	first := make(chan error, 1)
	go func() { first <- p.NewRunner().Destroy(t.Context(), 42) }()

	waitForQueued(t, p, "holder")

	held, ok, err := p.Poll(t.Context(), "holder", "holder-1")
	if err != nil || !ok || held.Kind != nodeapi.CommandDestroy {
		t.Fatalf("the first poll took %+v (ok %v, err %v), want the destroy", held, ok, err)
	}

	second := make(chan error, 1)
	go func() { second <- p.NewRunner().Destroy(t.Context(), 43) }()

	waitForQueued(t, p, "holder")

	next, ok, err := p.Poll(t.Context(), "holder", "holder-1", held.ID)
	if err != nil || !ok {
		t.Fatalf("a poll naming the held destroy was given nothing (ok %v, err %v)", ok, err)
	}

	if next.ID == held.ID || next.RequestID != 43 {
		t.Fatalf("a poll naming destroy %q was given %+v, want the next command, request 43", held.ID, next)
	}

	// NAMED BY NEITHER POLL NOW, the first is lost again, and given again.
	again, ok, err := p.Poll(t.Context(), "holder", "holder-1", next.ID)
	if err != nil || !ok || again.ID != held.ID {
		t.Fatalf("a poll that no longer names destroy %q was given %+v (ok %v, err %v), want it again",
			held.ID, again, ok, err)
	}

	// ANSWERED, both destroys' callers hear it.
	for _, id := range []string{held.ID, next.ID} {
		if err := p.Result("holder", "holder-1", nodeapi.CommandResult{ID: id, OK: true}); err != nil {
			t.Fatalf("report %s: %v", id, err)
		}
	}

	for _, ch := range []chan error{first, second} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("an answered destroy failed: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("an answered destroy's caller is still waiting")
		}
	}
}

// THE HEADER IS READ AS THE NODE SENT IT: comma-separated ids, blanks dropped, none
// when absent, and bounded.
func TestThePollReadsTheDestroysANodeHolds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		header string
		want   []string
	}{
		{"absent", "", nil},
		{"one", "a1", []string{"a1"}},
		{"several, with blanks", " a1 ,, b2 ,", []string{"a1", "b2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/nodes/n1/poll", http.NoBody)
			if tc.header != "" {
				r.Header.Set(nodeapi.HeaderDestroysRunning, tc.header)
			}

			if got := destroysRunning(r); !slices.Equal(got, tc.want) {
				t.Fatalf("read %q as %q, want %q", tc.header, got, tc.want)
			}
		})
	}

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/nodes/n1/poll", http.NoBody)
	many := make([]string, nodeapi.MaxDestroysRunning+50)
	for i := range many {
		many[i] = "x"
	}
	r.Header.Set(nodeapi.HeaderDestroysRunning, strings.Join(many, ","))

	if got := len(destroysRunning(r)); got != nodeapi.MaxDestroysRunning {
		t.Fatalf("read %d ids from a header of %d, want the bound of %d", got, len(many), nodeapi.MaxDestroysRunning)
	}
}
