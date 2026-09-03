package alloc

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// THE PROVIDER LIST IN THE COST QUERIES IS A LITERAL, AND THIS IS WHAT PINS IT.
//
// sqlc.slice() is not available on SQLite, so `provider IN (?, ?)` cannot be
// built from config.RemoteProviders() at run time the way the hand-written SQL
// did. The list is therefore written out in nodes.sql -- twice, once per query --
// which is two sources of truth unless something compares them.
//
// THE DIRECTION THAT HURTS IS A BACKEND MISSING FROM THE SQL. A remote provider
// this deployment supports and the query does not is a fleet whose cost line is
// simply absent, and an absent cost line is exactly what a fleet that costs
// nothing looks like -- the same could-not-tell/no collapse that made the
// original query say `provider = 'ec2'` and report nothing for a codebuild
// deployment. The other direction is checked too: a name in the SQL that is not a
// remote provider is either a typo, which silently narrows the report, or a host
// backend, which would charge owned hardware as if it were bought.
//
// EACH QUERY IS EXTRACTED BY NAME AND REQUIRED TO CARRY EXACTLY ONE LIST, and the
// first version of this test did neither. It collected every `provider IN (...)`
// in the file and compared each one it found -- so rewriting one query's filter as
// `provider = 'ec2'` REMOVED an occurrence rather than changing it, the remaining
// list still matched, and the test stayed green while codebuild nodes vanished
// from the cost report. Measured, on this tree, before the fix.
func TestTheRemoteProviderListMatchesTheQueries(t *testing.T) {
	t.Parallel()

	want := make([]string, 0)
	for _, p := range config.RemoteProviders() {
		want = append(want, string(p))
	}

	slices.Sort(want)

	if len(want) == 0 {
		t.Fatal("config.RemoteProviders() is empty, so this comparison would check nothing")
	}

	// BOTH QUERIES BY NAME. One lists the hosts whose compute is bought; the other
	// lists what those hosts are currently running. A filter that drops out of
	// EITHER breaks the report, and in opposite ways: the first omits a backend's
	// declared capacity, the second lets an owned host's leases into the remote
	// aggregation and fails the whole report with ErrRemoteCostUnavailable.
	for _, query := range []string{"ListRemoteCostNodes", "ListOutstandingRemoteShapes"} {
		got := providerListIn(t, query)

		if !slices.Equal(got, want) {
			t.Errorf("%s lists remote providers %v; config.RemoteProviders() says %v. "+
				"A backend missing here reports no cost at all, which reads as a fleet "+
				"that costs nothing", query, got, want)
		}
	}
}

// providerListIn extracts the one `provider IN (...)` list from a named query.
//
// SCOPED TO THE NAMED QUERY, from its `-- name:` line to the next one, so a list
// in a neighbouring statement can neither satisfy this nor be blamed for it. An
// absent or duplicated list is a failure rather than a skip: a query with no
// filter is the bug this exists for, and two filters mean the statement changed
// shape and a person needs to look.
func providerListIn(t *testing.T, query string) []string {
	t.Helper()

	body := namedQuery(t, "nodes.sql", query)

	lists := regexp.MustCompile(`provider IN \(([^)]*)\)`).FindAllStringSubmatch(body, -1)
	if len(lists) != 1 {
		t.Fatalf("%s carries %d `provider IN (...)` list(s), want exactly 1. A query "+
			"with none is a backend whose cost is never reported; if the statement was "+
			"deliberately reshaped, update this pin rather than deleting it",
			query, len(lists))
	}

	var got []string
	for _, raw := range strings.Split(lists[0][1], ",") {
		got = append(got, strings.Trim(strings.TrimSpace(raw), "'"))
	}

	slices.Sort(got)

	return got
}

// namedQuery returns the body of one `-- name: X :kind` statement from a query
// file, up to the next one.
//
// SHARED BY THE PINS IN THIS PACKAGE, because every one of them has the same
// hazard: a query file holds many statements and matching the first occurrence of
// anything means asserting about whichever statement happens to come first.
func namedQuery(t *testing.T, file, query string) string {
	t.Helper()

	src, err := os.ReadFile(filepath.Join("..", "state", "queries", file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}

	body := string(src)

	at := strings.Index(body, "-- name: "+query+" ")
	if at < 0 {
		t.Fatalf("%s has no %s; if the query was renamed, move this pin with it rather "+
			"than deleting it", file, query)
	}

	rest := body[at+len("-- name: "):]

	if end := strings.Index(rest, "\n-- name: "); end >= 0 {
		rest = rest[:end]
	}

	return rest
}
