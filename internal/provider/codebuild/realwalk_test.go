package codebuild

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// THE INVENTORY WALK'S COST, MEASURED AGAINST A PROJECT WITH HISTORY. ADR-007 left one
// measurement open: what `List` costs on a busy fleet, since ListBuildsForProject has
// no status filter and keeps a year of builds. The arithmetic says two requests per
// hundred builds inside the window and nothing for the rest; this asks a real project.
//
// It RECORDS rather than asserts — a measurement is not a pass/fail — and skips unless
// BILLET_TEST_CODEBUILD_WALK names the project, so it never runs by accident against a
// project somebody is measuring something else on. Give the project its history first
// (a thousand `echo ok` builds is about $5), then:
//
//	BILLET_TEST_CODEBUILD_PROJECT=<project> BILLET_TEST_CODEBUILD_WALK=1 \
//	  go test ./internal/provider/codebuild/ -run TestARealInventoryWalk -v
func TestARealInventoryWalkOverABusyProject(t *testing.T) {
	project := realProject(t)
	if os.Getenv("BILLET_TEST_CODEBUILD_WALK") == "" {
		t.Skip("set BILLET_TEST_CODEBUILD_WALK=1 to time the inventory walk against " +
			"BILLET_TEST_CODEBUILD_PROJECT's history")
	}

	// TWO WINDOWS, because the walk's cost is the window's: the tightest ceilings the
	// service allows (what realProvider configures — a 70-minute window), and the
	// service maxima a config that says nothing gets (a 53-hour one).
	for _, tc := range []struct {
		name          string
		build, queued int
	}{
		{name: "tightest ceilings", build: config.CodeBuildBuildFloorMinutes, queued: config.CodeBuildQueuedFloorMinutes},
		{name: "service maxima", build: config.CodeBuildBuildCeilingMinutes, queued: config.CodeBuildQueuedCeilingMinutes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counter := &countingTransport{next: http.DefaultTransport, byTarget: map[string]int{}}

			p := realProvider(t, project)
			p.cfg.BuildTimeoutMinutes = tc.build
			p.cfg.QueuedTimeoutMinutes = tc.queued
			p.api.setHTTPClient(&http.Client{Transport: counter, Timeout: apiTimeout})

			started := time.Now()
			instances, err := p.List(t.Context())
			elapsed := time.Since(started)

			if err != nil {
				t.Fatalf("List: %v", err)
			}

			t.Logf("window %dm: List took %s, returned %d owned instance(s)",
				p.cfg.InventoryWindowMinutes(), elapsed.Round(time.Millisecond), len(instances))

			counter.mu.Lock()
			defer counter.mu.Unlock()

			for target, n := range counter.byTarget {
				t.Logf("  %s: %d request(s)", target, n)
			}

			t.Logf("  throttled responses: %d, other non-2xx: %d, total requests: %d",
				counter.throttled, counter.failed, counter.total)
		})
	}
}

// countingTransport counts requests by their X-Amz-Target and notices throttles.
type countingTransport struct {
	next      http.RoundTripper
	mu        sync.Mutex
	byTarget  map[string]int
	total     int
	throttled int
	failed    int
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.next.RoundTrip(req)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.total++
	target := strings.TrimPrefix(req.Header.Get("X-Amz-Target"), "CodeBuild_20161006.")
	c.byTarget[target]++

	if err == nil && resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusTooManyRequests ||
			strings.Contains(resp.Header.Get("X-Amzn-Errortype"), "Throttl") {
			c.throttled++
		} else {
			c.failed++
		}
	}

	return resp, err
}
