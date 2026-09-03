package runnerrelease

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// THE HEADLINE REGRESSION, WITH REAL DATES.
//
// 2.334.0 went out of date when 2.335.0 was published on 2026-06-08, so its ordinary
// window closed on 2026-07-08. When 2.336.0 landed on 2026-07-20 the old calculation
// — newest release's publication date plus thirty days — moved that deadline to
// 2026-08-19 and described a fleet github had been refusing for six weeks as having
// a month in hand. Every later release pushed it further out.
func TestTheDeadlineCountsFromTheFirstNewerReleaseRatherThanTheNewest(t *testing.T) {
	t.Parallel()

	srv := releaseServer(t, [][]fixture{{
		{tag: "v2.336.0", published: "2026-07-20T17:45:55Z"},
		{tag: "v2.335.0", published: "2026-06-08T09:12:00Z"},
		{tag: "v2.334.0", published: "2026-05-11T11:03:00Z"},
	}})

	fresh := resolve(t, srv, "2.334.0")

	if fresh.FirstNewer != "2.335.0" {
		t.Errorf("the window was started by %q; 2.335.0 is the first release newer than "+
			"2.334.0", fresh.FirstNewer)
	}

	want := time.Date(2026, 7, 8, 9, 12, 0, 0, time.UTC)
	if !fresh.Deadline().Equal(want) {
		t.Errorf("the deadline is %v, want %v — counting from the newest release instead "+
			"gives 2026-08-19, which is the defect", fresh.Deadline(), want)
	}

	// AND THE NEWEST RELEASE IS STILL REPORTED, because that is what a rebuild takes
	// up. The two are different questions and both are needed.
	if fresh.Latest != "2.336.0" {
		t.Errorf("the remediation target is %q, want 2.336.0", fresh.Latest)
	}

	// GITHUB HAD ALREADY STOPPED QUEUEING by the time the second release landed.
	if !fresh.Expired(time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)) {
		t.Error("a fleet six weeks past its window was reported as inside it")
	}
}

// A PATCH RELEASE IS AN AVAILABLE UPDATE. GitHub's rule names major, minor and patch
// alike, so the clock starts at whichever came first — and versions are ordered
// numerically, since "2.9.0" sorts after "2.10.0" as a string and is older in fact.
func TestSeveralNewerReleasesStartTheClockAtTheEarliest(t *testing.T) {
	t.Parallel()

	srv := releaseServer(t, [][]fixture{{
		{tag: "v2.10.0", published: "2026-08-01T00:00:00Z"},
		{tag: "v2.9.2", published: "2026-07-15T00:00:00Z"},
		{tag: "v2.9.1", published: "2026-06-20T00:00:00Z"},
		{tag: "v2.9.0", published: "2026-06-01T00:00:00Z"},
	}})

	fresh := resolve(t, srv, "2.9.0")

	if fresh.FirstNewer != "2.9.1" {
		t.Errorf("the window was started by %q, want the patch release 2.9.1", fresh.FirstNewer)
	}

	if fresh.Latest != "2.10.0" {
		t.Errorf("the newest release is %q; 2.10.0 is newer than 2.9.2 numerically", fresh.Latest)
	}
}

// A LATE BACKPORT DOES NOT MOVE A CLOCK THAT HAS ALREADY STARTED. "Earliest newer"
// is decided by publication date, not by version order: 2.334.1 is newer than the
// installed release and appeared after 2.335.0, which is what actually opened the
// window.
func TestAReleasePublishedLaterDoesNotStartTheClock(t *testing.T) {
	t.Parallel()

	srv := releaseServer(t, [][]fixture{{
		{tag: "v2.334.1", published: "2026-07-30T00:00:00Z"},
		{tag: "v2.335.0", published: "2026-06-08T00:00:00Z"},
		{tag: "v2.334.0", published: "2026-05-11T00:00:00Z"},
	}})

	fresh := resolve(t, srv, "2.334.0")

	if fresh.FirstNewer != "2.335.0" {
		t.Errorf("the window was started by %q, want 2.335.0 — the earliest by publication",
			fresh.FirstNewer)
	}
}

// A PRERELEASE IS NOT AN AVAILABLE UPDATE, and neither is a draft. Counting one
// starts a clock nobody is expected to answer and names it as the remediation.
func TestPrereleasesAndDraftsAreIgnored(t *testing.T) {
	t.Parallel()

	srv := releaseServer(t, [][]fixture{{
		{tag: "v2.337.0", published: "2026-08-20T00:00:00Z", draft: true},
		{tag: "v2.336.1-rc.1", published: "2026-08-10T00:00:00Z", prerelease: true},
		{tag: "v2.336.0", published: "2026-07-20T00:00:00Z"},
		{tag: "v2.335.0", published: "2026-06-08T00:00:00Z"},
	}})

	fresh := resolve(t, srv, "2.335.0")

	if fresh.Latest != "2.336.0" {
		t.Errorf("the newest STABLE release is %q, want 2.336.0", fresh.Latest)
	}

	if fresh.FirstNewer != "2.336.0" {
		t.Errorf("the window was started by %q; a prerelease and a draft are not updates",
			fresh.FirstNewer)
	}
}

// THE HISTORY IS PAGED UNTIL THE INSTALLED RELEASE IS FOUND, because a page holds a
// hundred releases and a fleet can be further back than that. Stopping at page one
// would report the earliest release on THAT page as the one that started the clock,
// which is the same defect as counting from the newest, one page down.
func TestTheHistoryIsPagedUntilTheInstalledReleaseIsFound(t *testing.T) {
	t.Parallel()

	first := make([]fixture, 0, perPage)
	for i := range perPage {
		first = append(first, fixture{
			tag:       fmt.Sprintf("v2.%d.0", 500-i),
			published: fmt.Sprintf("2026-%02d-01T00:00:00Z", 1+i%12),
		})
	}

	second := []fixture{
		{tag: "v2.400.0", published: "2025-03-04T00:00:00Z"},
		{tag: "v2.399.0", published: "2025-02-01T00:00:00Z"},
	}

	srv := releaseServer(t, [][]fixture{first, second})

	fresh := resolve(t, srv, "2.399.0")

	if !fresh.InstalledKnown {
		t.Fatal("the installed release is on the second page and was reported as absent")
	}

	if got := srv.pages(); got != 2 {
		t.Errorf("billet read %d pages; it must keep reading until it finds the installed "+
			"release", got)
	}

	if fresh.FirstNewer != "2.400.0" {
		t.Errorf("the window was started by %q, want the 2.400.0 on the second page",
			fresh.FirstNewer)
	}
}

// A HIGHER VERSION PUBLISHED EARLIER STILL OPENS THE WINDOW, and it is the case
// that separates the two orderings.
//
// MEASURED on one real page of actions/runner's history: v2.285.3 was published
// 2023-01-30, ELEVEN DAYS AFTER the higher v2.301.1 on 2023-01-19. So "the newest
// release" and "the earliest release newer than yours" are different questions with
// different answers, and a model taking the newest one's date reports a deadline a
// fortnight later than the truth. The walk stops at the installed RELEASE, never at
// a higher NUMBER, which is what keeps this reachable.
func TestAHigherVersionPublishedEarlierIsWhatStartsTheClock(t *testing.T) {
	t.Parallel()

	srv := releaseServer(t, [][]fixture{{
		{tag: "v2.302.0", published: "2023-02-10T00:00:00Z"},
		{tag: "v2.301.1", published: "2023-01-19T01:13:29Z"},
		{tag: "v2.285.3", published: "2023-01-30T20:39:53Z"},
	}})

	fresh := resolve(t, srv, "2.285.3")

	if fresh.Current() {
		t.Fatal("a fleet sixteen minor releases behind was reported as current; being " +
			"behind is a question about the newest VERSION, not about which release " +
			"opened a window")
	}

	if fresh.FirstNewer != "2.301.1" {
		t.Errorf("the window was started by %q; 2.301.1 is newer than 2.285.3 and was "+
			"published before 2.302.0", fresh.FirstNewer)
	}

	want := time.Date(2023, 1, 19, 1, 13, 29, 0, time.UTC).Add(Grace)
	if !fresh.Deadline().Equal(want) {
		t.Errorf("the deadline is %v, want %v", fresh.Deadline(), want)
	}
}

// AND THE WALK STOPS ONCE IT HAS LOOKED PAST THE INSTALLED RELEASE, which is what
// makes it terminate at all: actions/runner has published hundreds of releases and
// no budget could read them.
//
// TWO CASES, AND THE FIRST VERSION OF THIS TEST ENCODED THE WRONG ONE AS CORRECT.
// The floor settles the window question either way — everything published after the
// installed release is above it — but it does not settle which release is NEWEST,
// because a higher version published earlier sits BELOW. When the floor is mid-page
// the rest of that page is where such a release would be, and it is already fetched.
// When the floor is the page's LAST record there is nothing past it to have seen, so
// one more page is read; asserting one page there is asserting that `Current()` may
// be wrong at a page boundary.
func TestTheWalkStopsOnceItHasLookedPastTheInstalledRelease(t *testing.T) {
	t.Parallel()

	full := func(from int) []fixture {
		page := make([]fixture, 0, perPage)
		for i := range perPage {
			page = append(page, fixture{
				tag:       fmt.Sprintf("v2.%d.0", from-i),
				published: fmt.Sprintf("2026-%02d-01T00:00:00Z", 1+i%12),
			})
		}

		return page
	}

	for _, tc := range []struct {
		name      string
		installed string
		wantPages int
	}{
		{
			// MID-PAGE: the rest of the page is the evidence, and it is in hand.
			name:      "mid-page",
			installed: fmt.Sprintf("2.%d.0", 500-(perPage/2)),
			wantPages: 1,
		},
		{
			// LAST ON A FULL PAGE: nothing past it has been seen, so one more page.
			name:      "the last record of a full page",
			installed: fmt.Sprintf("2.%d.0", 500-(perPage-1)),
			wantPages: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := releaseServer(t, [][]fixture{
				full(500),
				{{tag: "v2.100.0", published: "2020-01-01T00:00:00Z"}},
			})

			fresh := resolve(t, srv, tc.installed)

			if !fresh.InstalledKnown {
				t.Fatal("the installed release is on page one and was reported as absent")
			}

			if got := srv.pages(); got != tc.wantPages {
				t.Errorf("billet read %d pages, want %d", got, tc.wantPages)
			}

			if !fresh.HistoryComplete {
				t.Error("a walk that found the installed release reported an incomplete " +
					"history; the floor settles the window question")
			}
		})
	}
}

// AND A HIGHER VERSION ON THE NEXT PAGE IS STILL FOUND, which is the whole reason
// that second case reads a page at all.
//
// The installed release is the last record of a full page and a higher version,
// published earlier, is the first record of the next. Stopping at the page boundary
// left Latest as the installed version, Current() answered TRUE, and `runner check`
// printed "which is the current release" and exited 0 for a fleet on an old
// maintenance branch.
func TestAHigherVersionOnTheNextPageStillCountsAsNewer(t *testing.T) {
	t.Parallel()

	first := make([]fixture, 0, perPage)
	for i := range perPage {
		first = append(first, fixture{
			tag:       fmt.Sprintf("v2.%d.0", 500-i),
			published: fmt.Sprintf("2026-%02d-01T00:00:00Z", 1+i%12),
		})
	}

	installed := fmt.Sprintf("2.%d.0", 500-(perPage-1))

	srv := releaseServer(t, [][]fixture{
		first,
		{{tag: "v2.999.0", published: "2020-01-01T00:00:00Z"}},
	})

	fresh := resolve(t, srv, installed)

	if fresh.Latest != "2.999.0" {
		t.Errorf("the newest release is %q; a higher version published earlier is below "+
			"the installed record, and here that is the next page", fresh.Latest)
	}

	if fresh.Current() {
		t.Error("a fleet with a higher release available was reported as current")
	}

	// AND IT DID NOT MOVE THE WINDOW. It was already available when the installed
	// release shipped, so it is not an update that became available afterwards.
	if fresh.FirstNewer == "2.999.0" {
		t.Error("a release published before the installed one was taken as opening its " +
			"window")
	}
}

// AND THE FLOOR IS THE RECORD, NOT THE PAGE. The scan stops AT the installed
// release, not after finishing the page it was on.
//
// Everything below the installed release was published earlier and was already
// available when it shipped, so under the rule the walk states none of it opened the
// window. Reading on lets a higher-numbered, EARLIER-published record further down
// the same page replace FirstNewer and move the deadline earlier than the model says
// — which would have `images pull` refuse a valid image.
//
// THE INSTALLED RELEASE IS MID-PAGE HERE, which is what makes the case reachable: a
// fixture with it last on the page cannot tell the two behaviours apart, and that is
// how this went unnoticed.
func TestTheScanStopsAtTheInstalledRecordRatherThanTheEndOfItsPage(t *testing.T) {
	t.Parallel()

	srv := releaseServer(t, [][]fixture{{
		{tag: "v2.340.0", published: "2026-08-01T00:00:00Z"},
		{tag: "v2.336.0", published: "2026-07-20T00:00:00Z"},
		// The installed release, with more records after it on the same page.
		{tag: "v2.335.0", published: "2026-06-08T00:00:00Z"},
		// Higher than installed and published EARLIER than everything above: read,
		// it becomes the opener and moves the deadline two years back.
		{tag: "v2.999.0", published: "2024-01-01T00:00:00Z"},
	}})

	fresh := resolve(t, srv, "2.335.0")

	if fresh.FirstNewer != "2.336.0" {
		t.Errorf("the window was started by %q; the scan read past the installed record "+
			"into releases that were already available when it shipped", fresh.FirstNewer)
	}

	want := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC).Add(Grace)
	if !fresh.Deadline().Equal(want) {
		t.Errorf("the deadline is %v, want %v", fresh.Deadline(), want)
	}

	// AND THE TAIL RECORD IS STILL EVIDENCE THAT THE FLEET IS BEHIND. Freezing
	// FirstNewer at the floor and stopping the record loop outright are different
	// things, and the first version did the second: 2.999.0 was never seen at all,
	// so Latest was wrong and Current() answered TRUE for a fleet 664 minor releases
	// behind — while `runner check` printed "which is the current release" and named
	// the wrong rebuild target.
	if fresh.Latest != "2.999.0" {
		t.Errorf("the newest release is %q; a higher version published before the "+
			"installed one is still the newest, and it is what a rebuild takes up",
			fresh.Latest)
	}

	if fresh.Current() {
		t.Error("a fleet with a higher release available was reported as current")
	}
}

// THE LAYOUTS AROUND THE FLOOR, each of which the walk answers differently and none
// of which the cases above reach.
//
// This is where the model has been wrong three times — at a page boundary, past the
// floor, and at the budget — so the shapes are enumerated rather than sampled.
func TestTheFloorIsAnsweredOnEveryLayoutAroundIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name           string
		pages          [][]fixture
		installed      string
		wantFirstNewer string
		wantLatest     string
		wantKnown      bool
		wantComplete   bool
	}{
		{
			// A SHORT FIRST PAGE is the end of the history AND holds the floor. Both
			// reasons to stop are true at once, which must not confuse either answer.
			name: "a short first page holding the floor",
			pages: [][]fixture{{
				{tag: "v2.336.0", published: "2026-07-20T00:00:00Z"},
				{tag: "v2.335.0", published: "2026-06-08T00:00:00Z"},
				{tag: "v2.334.0", published: "2026-05-11T00:00:00Z"},
			}},
			installed:      "2.335.0",
			wantFirstNewer: "2.336.0",
			wantLatest:     "2.336.0",
			wantKnown:      true,
			wantComplete:   true,
		},
		// A DUPLICATE INSTALLED RECORD IS DELIBERATELY NOT A CASE HERE.
		//
		// A release is keyed by its tag and github cannot publish two with the same
		// one, so a fixture containing two is not a history that exists. The first
		// version of this file had one and claimed the walk "handles" it — and the
		// handling was wrong in the one ordering that matters: a duplicate placed
		// ABOVE the real opener sets the floor early and the opener becomes
		// Latest-only, leaving FirstNewer empty. Claiming to handle an impossible
		// input, incorrectly, is worse than not claiming it.
		{
			// NOTHING BUT PRERELEASES PAST THE FLOOR. They are not updates and are
			// not evidence about the newest release either, so the answer is the
			// same as if they were absent.
			name: "prereleases past the floor",
			pages: [][]fixture{{
				{tag: "v2.336.0", published: "2026-07-20T00:00:00Z"},
				{tag: "v2.335.0", published: "2026-06-08T00:00:00Z"},
				{tag: "v2.999.0-rc.1", published: "2020-01-01T00:00:00Z", prerelease: true},
			}},
			installed:      "2.335.0",
			wantFirstNewer: "2.336.0",
			wantLatest:     "2.336.0",
			wantKnown:      true,
			wantComplete:   true,
		},
		{
			// THE FLOOR HAS NOTHING NEWER ABOVE IT: the installed release is the
			// newest thing published. Current, with no deadline at all.
			name: "the floor is the newest release",
			pages: [][]fixture{{
				{tag: "v2.336.0", published: "2026-07-20T00:00:00Z"},
				{tag: "v2.335.0", published: "2026-06-08T00:00:00Z"},
			}},
			installed:      "2.336.0",
			wantFirstNewer: "",
			wantLatest:     "2.336.0",
			wantKnown:      true,
			wantComplete:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fresh := resolve(t, releaseServer(t, tc.pages), tc.installed)

			if fresh.FirstNewer != tc.wantFirstNewer {
				t.Errorf("FirstNewer = %q, want %q", fresh.FirstNewer, tc.wantFirstNewer)
			}

			if fresh.Latest != tc.wantLatest {
				t.Errorf("Latest = %q, want %q", fresh.Latest, tc.wantLatest)
			}

			if fresh.InstalledKnown != tc.wantKnown {
				t.Errorf("InstalledKnown = %v, want %v", fresh.InstalledKnown, tc.wantKnown)
			}

			if fresh.HistoryComplete != tc.wantComplete {
				t.Errorf("HistoryComplete = %v, want %v", fresh.HistoryComplete, tc.wantComplete)
			}

			// AND Current() FOLLOWS FROM Latest, not from whether an opener was found.
			if got, want := fresh.Current(), tc.wantFirstNewer == ""; got != want {
				t.Errorf("Current() = %v, want %v", got, want)
			}
		})
	}
}

// BEHIND WITH NO WINDOW TO COUNT IS ITS OWN STATE, and it is reachable from the
// measured ordering.
//
// v2.285.3 was published 2023-01-30, eleven days AFTER the higher v2.301.1. Reduced
// to those two records the higher one sits BELOW the installed release: it is read
// as evidence the fleet is behind and deliberately not as an opener, because it was
// already available when the installed release shipped. So Current() is false while
// FirstNewer is empty — and every timed answer is meaningless, which is what
// BehindWithoutAWindow exists to let a caller ask before reaching for one.
func TestAHigherVersionPublishedBeforeTheInstalledOneIsBehindWithNoWindow(t *testing.T) {
	t.Parallel()

	srv := releaseServer(t, [][]fixture{{
		{tag: "v2.285.3", published: "2023-01-30T20:39:53Z"},
		{tag: "v2.301.1", published: "2023-01-19T01:13:29Z"},
	}})

	fresh := resolve(t, srv, "2.285.3")

	if fresh.Current() {
		t.Fatal("a fleet with a higher release available was reported as current")
	}

	if fresh.FirstNewer != "" {
		t.Errorf("the window was started by %q; that release predates the installed one "+
			"and was already available when it shipped", fresh.FirstNewer)
	}

	if !fresh.BehindWithoutAWindow() {
		t.Error("behind with no opener is the state this is in, and it did not say so")
	}

	// AND NO TIMED ANSWER IS OFFERED. Reaching for one printed the year 0001 and a
	// negative number of days.
	if !fresh.Deadline().IsZero() {
		t.Errorf("a deadline of %v was offered for a window that never opened",
			fresh.Deadline())
	}

	now := time.Now()

	if fresh.Due(now) || fresh.Expired(now) {
		t.Error("a timed verdict was reached for a window that never opened")
	}
}

// AND A BUDGET EXHAUSTED AFTER THE FLOOR IS NOT COMPLETE, which the first version of
// this got wrong by setting the flag the moment the floor was found and then
// continuing.
//
// The layouts that reach it are real: the floor as the last record of the LAST
// allowed page, or a following page holding nothing but prereleases. In both, a page
// that was never read is reported as one that did not need to be — and an unread
// page can hold a higher stable version published earlier, so Latest and Current()
// are not settled.
func TestABudgetExhaustedAfterTheFloorIsNotComplete(t *testing.T) {
	t.Parallel()

	fullFrom := func(from int) []fixture {
		page := make([]fixture, 0, perPage)
		for i := range perPage {
			page = append(page, fixture{
				tag:       fmt.Sprintf("v2.%d.0", from-i),
				published: fmt.Sprintf("2026-%02d-01T00:00:00Z", 1+i%12),
			})
		}

		return page
	}

	prereleases := make([]fixture, 0, perPage)
	for i := range perPage {
		prereleases = append(prereleases, fixture{
			tag:        fmt.Sprintf("v2.%d.0-rc.1", 800-i),
			published:  "2020-01-01T00:00:00Z",
			prerelease: true,
		})
	}

	for _, tc := range []struct {
		name      string
		pages     [][]fixture
		installed string
	}{
		{
			// THE FLOOR IS THE LAST RECORD OF THE LAST ALLOWED PAGE, so the walk goes
			// round once more and runs out of budget.
			name:      "the floor ends the last allowed page",
			pages:     [][]fixture{fullFrom(900), fullFrom(900 - perPage)},
			installed: fmt.Sprintf("2.%d.0", 900-(2*perPage-1)),
		},
		{
			// NOTHING PAST THE FLOOR IS A RELEASE, so the extra page produced no
			// evidence and the budget ends the walk.
			name:      "only prereleases follow the floor",
			pages:     [][]fixture{fullFrom(900), prereleases},
			installed: fmt.Sprintf("2.%d.0", 900-(perPage-1)),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fresh := resolve(t, releaseServer(t, tc.pages), tc.installed)

			if !fresh.InstalledKnown {
				t.Fatal("the installed release is on a fetched page and was reported absent")
			}

			if fresh.HistoryComplete {
				t.Error("the walk ran out of budget after the floor and reported that it " +
					"had read everything it needed; an unread page can hold a higher " +
					"version published earlier")
			}
		})
	}
}

// A WALK THAT RAN OUT OF BUDGET SAYS SO, rather than reading as the end of the
// history.
//
// The two are different facts and only one supports "nothing to do": older releases
// billet never read could include one newer than the installed release and published
// EARLIER than the opener it found, so the deadline it computed would be LATER than
// the truth. A fleet whose runner predates everything the budget covers is the case,
// and it is exactly the fleet most likely to be expired.
func TestAWalkThatExhaustsItsBudgetReportsAnIncompleteHistory(t *testing.T) {
	t.Parallel()

	// EVERY PAGE FULL AND THE INSTALLED RELEASE ON NONE OF THEM, so the walk has no
	// ending to find and no floor to stop at.
	pages := make([][]fixture, maxPages+1)

	for page := range pages {
		for i := range perPage {
			pages[page] = append(pages[page], fixture{
				tag:       fmt.Sprintf("v2.%d.0", 9000-(page*perPage+i)),
				published: fmt.Sprintf("2026-%02d-01T00:00:00Z", 1+i%12),
			})
		}
	}

	fresh := resolve(t, releaseServer(t, pages), "2.1.0")

	if fresh.InstalledKnown {
		t.Fatal("a version on none of the pages was reported as found")
	}

	if fresh.HistoryComplete {
		t.Error("a walk that stopped at its own budget reported that it had read the " +
			"whole history")
	}
}

// AND ONE THAT FINISHED FOR A REASON SAYS SO, in each of the three shapes that
// takes.
//
// THE INSTALLED RELEASE MUST BE ABSENT FROM THE PAGED CASES, which is what the first
// version of this got wrong: its fixture put the installed release on the first
// entry of the full page, so the walk stopped at the FLOOR and never asked for the
// empty page the case is named for. It passed, and proved the floor twice.
func TestAWalkThatFinishesForAReasonReportsACompleteHistory(t *testing.T) {
	t.Parallel()

	full := make([]fixture, 0, perPage)
	for i := range perPage {
		full = append(full, fixture{
			tag:       fmt.Sprintf("v2.%d.0", 9000-i),
			published: fmt.Sprintf("2026-%02d-01T00:00:00Z", 1+i%12),
		})
	}

	for _, tc := range []struct {
		name      string
		pages     [][]fixture
		installed string
		wantPages int
	}{
		{
			name: "a page shorter than the request",
			pages: [][]fixture{{
				{tag: "v2.336.0", published: "2026-07-20T00:00:00Z"},
				{tag: "v2.335.0", published: "2026-06-08T00:00:00Z"},
			}},
			installed: "2.334.0",
			wantPages: 1,
		},
		{
			// AN EXACTLY-FULL PAGE LOOKS LIKE MORE HISTORY, so the walk has to ask
			// again to find out; the empty answer is the ending.
			name:      "an empty page after a full one",
			pages:     [][]fixture{full, {}},
			installed: "2.1.0",
			wantPages: 2,
		},
		{
			// THE FLOOR, which is the ordinary case: everything below the installed
			// release was published earlier and was already available when it shipped.
			//
			// MID-PAGE, deliberately. With the installed release LAST on a full page
			// the walk reads one more — see TestTheWalkStopsOnceItHasLookedPast — and
			// a fixture built that way asserts the boundary where Current() was wrong.
			name:      "the installed release",
			pages:     [][]fixture{full, {{tag: "v2.50.0", published: "2020-01-01T00:00:00Z"}}},
			installed: fmt.Sprintf("2.%d.0", 9000-(perPage/2)),
			wantPages: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := releaseServer(t, tc.pages)

			fresh := resolve(t, srv, tc.installed)

			if !fresh.HistoryComplete {
				t.Error("a walk that finished for a reason said it had run out of budget")
			}

			// THE PAGE COUNT IS WHAT SEPARATES THESE CASES. Without it, a walk that
			// stopped at the floor satisfies every one of them.
			if got := srv.pages(); got != tc.wantPages {
				t.Errorf("billet read %d pages, want %d; this case is about how the walk "+
					"finished, and the page count is the only thing that says which",
					got, tc.wantPages)
			}
		})
	}
}

// A VERSION THE HISTORY DOES NOT NAME IS ITS OWN ANSWER, NOT "CURRENT".
//
// The installed release is older than everything billet fetched, so the window
// opened at or before the earliest release it CAN see. That makes the computed
// deadline an upper bound: "already past it" stays a proof, and the number of days
// remaining does not. Reporting such a fleet as current is the false negative this
// package exists to remove; refusing it outright would fail a fleet that is fine.
func TestAnInstalledVersionAbsentFromTheHistoryIsNotReportedAsCurrent(t *testing.T) {
	t.Parallel()

	srv := releaseServer(t, [][]fixture{{
		{tag: "v2.336.0", published: "2026-07-20T00:00:00Z"},
		{tag: "v2.335.0", published: "2026-06-08T00:00:00Z"},
	}})

	fresh := resolve(t, srv, "2.100.0")

	if fresh.InstalledKnown {
		t.Fatal("a version the history never named was reported as found in it")
	}

	if fresh.Current() {
		t.Error("a version older than the whole history was reported as the current release")
	}

	// CONCLUSIVE IN THE ONE DIRECTION THAT IS SOUND: the real window opened no later
	// than 2026-06-08, so by August it has certainly closed.
	if !fresh.Expired(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("a version older than a release published two months ago was not reported " +
			"as expired")
	}

	// AND NOT IN THE OTHER: a week after the earliest release billet can see, the
	// upper-bound deadline has not passed, so this must not claim expiry.
	if fresh.Expired(time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)) {
		t.Error("expiry was claimed from an upper bound that had not been reached")
	}
}

// A RELEASE THAT IS THE NEWEST ONE IS NOT A DEADLINE.
func TestBeingCurrentIsNeverDue(t *testing.T) {
	t.Parallel()

	srv := releaseServer(t, [][]fixture{{
		{tag: "v2.336.0", published: "2026-07-20T00:00:00Z"},
		{tag: "v2.335.0", published: "2026-06-08T00:00:00Z"},
	}})

	fresh := resolve(t, srv, "2.336.0")

	if !fresh.Current() {
		t.Fatal("the installed release is the newest one and Current says otherwise")
	}

	// A YEAR LATER, still not due: nothing has been released since, so there is
	// nothing to update to. Counting the age of the release billet already has would
	// report a fleet as expiring for standing still while github did too.
	late := time.Date(2027, 7, 20, 0, 0, 0, 0, time.UTC)

	if fresh.Due(late) {
		t.Error("a fleet running the newest release was reported as needing a rebuild")
	}

	if fresh.Expired(late) {
		t.Error("a fleet running the newest release was reported as refused by github")
	}

	if !fresh.Deadline().IsZero() {
		t.Errorf("a current fleet was given a deadline of %v", fresh.Deadline())
	}
}

// AND A RELEASE BILLET HAS NOT TAKEN UP IS A CLOCK.
func TestAnUnappliedReleaseBecomesDueAndThenExpires(t *testing.T) {
	t.Parallel()

	published := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	srv := releaseServer(t, [][]fixture{{
		{tag: "v2.329.0", published: "2026-01-01T00:00:00Z"},
		{tag: "v2.328.0", published: "2025-12-01T00:00:00Z"},
	}})

	fresh := resolve(t, srv, "2.328.0")

	for _, tc := range []struct {
		name    string
		at      time.Time
		due     bool
		expired bool
	}{
		{"the day it is published", published, false, false},
		{"a week later", published.Add(7 * 24 * time.Hour), false, false},
		{"the day before the warning", published.Add(Warn - time.Hour), false, false},
		{"at the warning", published.Add(Warn), true, false},
		{"the hour before the deadline", published.Add(Grace - time.Hour), true, false},
		{"at the deadline", published.Add(Grace), true, true},
		{"long past it", published.Add(90 * 24 * time.Hour), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := fresh.Due(tc.at); got != tc.due {
				t.Errorf("Due = %v, want %v", got, tc.due)
			}

			if got := fresh.Expired(tc.at); got != tc.expired {
				t.Errorf("Expired = %v, want %v", got, tc.expired)
			}
		})
	}
}

// AND WHAT GITHUB SAYS IS READ CORRECTLY, including the `v` its tags carry.
func TestTheReleaseHistoryIsRead(t *testing.T) {
	t.Parallel()

	srv := releaseServer(t, [][]fixture{{
		{tag: "v2.336.0", published: "2026-07-20T17:45:55Z"},
		{tag: "v2.335.0", published: "2026-06-08T09:12:00Z"},
	}})

	fresh := resolve(t, srv, "2.335.0")

	if fresh.Latest != "2.336.0" {
		t.Errorf("the release is %q; the tag's `v` belongs to the tag, not the version",
			fresh.Latest)
	}

	want := time.Date(2026, 7, 20, 17, 45, 55, 0, time.UTC)
	if !fresh.FirstNewerPublished.Equal(want) {
		t.Errorf("published at %v, want %v", fresh.FirstNewerPublished, want)
	}

	if !fresh.Deadline().Equal(want.Add(Grace)) {
		t.Errorf("the deadline is %v, and it is counted from that release", fresh.Deadline())
	}
}

// A FAILURE TO ASK IS NOT AN ANSWER.
//
// Every caller has to tell "billet is out of date" apart from "billet could not
// find out": the second happens to any machine without egress, and reporting it as
// a fleet about to stop working is the kind of false alarm that teaches people to
// ignore the true one.
func TestAnUnreachableFeedIsAnErrorRatherThanAVerdict(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		writeOrFail(t, w, []byte(`{"message":"upstream is having a day"}`))
	}))
	defer srv.Close()

	if _, err := resolveFrom(t.Context(), srv.Client(), srv.URL, "2.335.0"); err == nil {
		t.Fatal("a 500 was treated as an answer about the runner version")
	}
}

// AND ITS RATE LIMIT IS NAMED, because that is what an unauthenticated check gets
// on a busy machine and "403" alone sends somebody hunting a permission problem.
func TestTheRateLimitSaysWhatItIs(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		writeOrFail(t, w, []byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer srv.Close()

	_, err := resolveFrom(t.Context(), srv.Client(), srv.URL, "2.335.0")
	if err == nil {
		t.Fatal("a 403 was treated as an answer")
	}

	if !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("the error does not say what a 403 usually means here: %v", err)
	}
}

// A NEWER RELEASE WITH NO DATE IS REFUSED, because the deadline is counted from it.
// Skipping it silently would drop the release that started the window and answer
// with a later one — the direction that reports an expired fleet as healthy.
func TestAReleaseWithNoDateIsRefused(t *testing.T) {
	t.Parallel()

	srv := releaseServer(t, [][]fixture{{
		{tag: "v2.336.0"},
		{tag: "v2.335.0", published: "2026-06-08T00:00:00Z"},
	}})

	_, err := resolveFrom(t.Context(), srv.Client(), srv.URL, "2.335.0")
	if err == nil {
		t.Fatal("a release with no publication date was accepted, and the deadline is " +
			"counted from that date")
	}

	if !strings.Contains(err.Error(), "published") {
		t.Errorf("the error does not name the missing field: %v", err)
	}
}

// SOMETHING THAT IS NOT A RELEASE IS NOT PLACED IN THE HISTORY AT ALL. An image can
// record whatever was written on it, and guessing where an unorderable string sits
// either invents a deadline or hides one.
func TestAnInstalledVersionThatIsNotAReleaseIsRefused(t *testing.T) {
	t.Parallel()

	srv := releaseServer(t, [][]fixture{{
		{tag: "v2.336.0", published: "2026-07-20T00:00:00Z"},
	}})

	for _, installed := range []string{"", "recent", "2.336", "v2.x.0"} {
		if _, err := resolveFrom(t.Context(), srv.Client(), srv.URL, installed); err == nil {
			t.Errorf("%q was placed in the release history", installed)
		}
	}
}

// THE ORDER IS NUMERIC. "2.9.0" sorts after "2.10.0" as a string and is older in
// fact, and picking the wrong one means watching the tier that is fine while the
// stale one expires.
func TestTheOlderRunnerIsCompared(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"2.336.0", "2.337.0", true},
		{"2.337.0", "2.336.0", false},
		{"2.336.0", "2.336.0", false},
		// THE ONE A STRING COMPARISON GETS BACKWARDS.
		{"2.9.0", "2.10.0", true},
		{"2.10.0", "2.9.0", false},
		{"3.0.0", "2.999.0", false},
		{"2.336.0", "2.336.1", true},
		{"2.328", "2.328.0", true},
	} {
		if got := Older(tc.a, tc.b); got != tc.want {
			t.Errorf("Older(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// AND SOMETHING THAT IS NOT A VERSION DOES NOT PANIC OR CLAIM AN ORDER IT CANNOT
// KNOW. This runs over metadata recorded on a generation, which is whatever was
// written there, on a scheduled path where crashing is a worse answer than a stable
// guess.
func TestAnUnparseableRunnerVersionStillOrders(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ a, b string }{
		{"", "2.336.0"},
		{"unknown", "2.336.0"},
		{"2.x.0", "2.336.0"},
		{"2.336", "2.336.0"},
	} {
		// The assertion is that it answers at all, consistently, rather than what it
		// answers: there is no true order between a version and a non-version.
		if Older(tc.a, tc.b) == Older(tc.b, tc.a) {
			t.Errorf("Older is not antisymmetric for %q and %q", tc.a, tc.b)
		}
	}

	if Older("apples", "apples") {
		t.Error("an unparseable version is older than itself")
	}
}

// fixture is one release as github publishes it.
type fixture struct {
	tag        string
	published  string
	draft      bool
	prerelease bool
}

// historyServer serves paged release fixtures and counts what was asked for.
type historyServer struct {
	*httptest.Server

	mu    sync.Mutex
	asked map[int]bool
}

func (h *historyServer) pages() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return len(h.asked)
}

// releaseServer stands in for the releases endpoint, one slice of fixtures per page.
//
// IT COUNTS PAGES because "stops once it has found the installed release" and "keeps
// going until it does" are both properties of the requests rather than of the
// answer, and an assertion about the answer alone passes for a client that read
// every page every time.
func releaseServer(t *testing.T, pages [][]fixture) *historyServer {
	t.Helper()

	h := &historyServer{asked: map[int]bool{}}

	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil || page < 1 {
			t.Errorf("the client asked for page %q", r.URL.Query().Get("page"))
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		h.mu.Lock()
		h.asked[page] = true
		h.mu.Unlock()

		body := "[]"
		if page <= len(pages) {
			body = renderReleases(pages[page-1])
		}

		w.Header().Set("Content-Type", "application/json")
		writeOrFail(t, w, []byte(body))
	}))

	t.Cleanup(h.Close)

	return h
}

func renderReleases(releases []fixture) string {
	var b strings.Builder

	b.WriteString("[")

	for i, rel := range releases {
		if i > 0 {
			b.WriteString(",")
		}

		published := "null"
		if rel.published != "" {
			published = `"` + rel.published + `"`
		}

		fmt.Fprintf(&b, `{"tag_name":%q,"published_at":%s,"draft":%t,"prerelease":%t}`,
			rel.tag, published, rel.draft, rel.prerelease)
	}

	b.WriteString("]")

	return b.String()
}

// resolve asks the model and fails the test if it could not answer, so an
// assertion is never satisfied by a zero value that came back beside an error.
func resolve(t *testing.T, srv *historyServer, installed string) Freshness {
	t.Helper()

	fresh, err := resolveFrom(t.Context(), srv.Client(), srv.URL, installed)
	if err != nil {
		t.Fatalf("resolve %s: %v", installed, err)
	}

	return fresh
}

// A PAGE TOO BIG TO READ IS SAID SO, RATHER THAN PARSED IN PART.
//
// MEASURED: one page of a hundred actions/runner releases is 4.9 MB, because every
// record carries its whole release-notes body. A bound is still needed — this is a
// response from somewhere else — but reading exactly the bound and handing the
// prefix to a decoder turns "the page grew" into "unexpected end of JSON input",
// which names nothing and is PERMANENT, since the next check reads the same page.
func TestAPageLargerThanTheBoundIsRefusedRatherThanTruncated(t *testing.T) {
	// NOT PARALLEL: it shrinks a package-level bound, because proving this against
	// the real one would mean generating 32 MB.
	saved := maxPageBytes
	t.Cleanup(func() { maxPageBytes = saved })

	maxPageBytes = 256

	srv := releaseServer(t, [][]fixture{{
		{tag: "v2.336.0", published: "2026-07-20T00:00:00Z"},
		{tag: "v2.335.0", published: "2026-06-08T00:00:00Z"},
		{tag: "v2.334.0", published: "2026-05-11T00:00:00Z"},
		{tag: "v2.333.0", published: "2026-04-01T00:00:00Z"},
	}})

	_, err := resolveFrom(t.Context(), srv.Client(), srv.URL, "2.335.0")
	if err == nil {
		t.Fatal("a page too large to read was treated as an answer")
	}

	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("the error does not say the page was too big: %v", err)
	}
}
