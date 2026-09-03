package codebuild

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// sweepPath is the path every sweep test lists under.
const sweepPath = "/billet/jit"

// sweepNow is the fixed instant the sweeper's clock reports, so "closed 46 hours
// ago" is arithmetic rather than a wall clock.
var sweepNow = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

// newTestSweeper wires a RegistrationSweeper to a fake Parameter Store.
//
// THROUGH THE PUBLIC CONSTRUCTOR, so the path and region rules it applies are the
// ones a control plane meets; only the endpoint is reached past it, through the
// same seam the e2e suite uses, because the endpoint is deliberately not
// configurable.
func newTestSweeper(t *testing.T, f *fakeAWS) (*RegistrationSweeper, *bytes.Buffer) {
	t.Helper()

	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	// A LOG THAT IS CAPTURED, so an assertion can say what was NOT written.
	var logged bytes.Buffer

	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	s, err := NewRegistrationSweeper("us-west-2", sweepPath, staticCreds{},
		SweepWithHTTPClient(srv.Client()),
		SweepWithClock(func() time.Time { return sweepNow }),
		SweepWithLogger(log))
	if err != nil {
		t.Fatalf("NewRegistrationSweeper: %v", err)
	}

	SetSweeperSSMEndpointForTest(s, srv.URL+"/")

	return s, &logged
}

// stage puts a parameter under the sweep path, written long enough ago that the
// parameter's own age never decides a test that is about the ledger's answer.
func (f *fakeAWS) stage(rel string) string {
	return f.stageWrittenAt(rel, sweepNow.Add(-ServiceInventoryWindow-2*time.Hour))
}

// stageWrittenAt puts a parameter under the sweep path with a chosen write time,
// returning its full name.
func (f *fakeAWS) stageWrittenAt(rel string, written time.Time) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	name := sweepPath + "/" + rel
	f.params[name] = theRegistration
	f.paramWritten[name] = written

	return name
}

// closureTable answers the ledger's question from a map, and refuses a lease it
// was not told about — so a test cannot pass on a zero value it never scripted.
func closureTable(t *testing.T, answers map[string]LeaseClosure) ClosureLookup {
	t.Helper()

	return func(_ context.Context, leaseID string) (LeaseClosure, error) {
		c, ok := answers[leaseID]
		if !ok {
			t.Errorf("the sweep asked about lease %q, which this test never staged", leaseID)

			return LeaseClosure{}, errors.New("unscripted lease")
		}

		return c, nil
	}
}

func closedAgo(d time.Duration) LeaseClosure {
	return LeaseClosure{Known: true, Terminal: true, FinishedAt: sweepNow.Add(-d)}
}

// THE RULE, ROW BY ROW. Only a lease the ledger holds terminal for longer than the
// service window loses its registration; everything else is kept and counted for
// what it is.
func TestTheSweepRemovesOnlyRegistrationsTheLedgerHasProvedDead(t *testing.T) {
	f := newFakeAWS(t)

	aged := f.stage("billet-aged")
	open := f.stage("billet-open")
	recent := f.stage("billet-recent")
	unknown := f.stage("billet-unknown")
	foreign := f.stage("somebody-elses-parameter")
	unstamped := f.stage("billet-unstamped")

	s, logged := newTestSweeper(t, f)

	report, err := s.Sweep(t.Context(), closureTable(t, map[string]LeaseClosure{
		"aged":   closedAgo(ServiceInventoryWindow + time.Minute),
		"open":   {Known: true, Terminal: false},
		"recent": closedAgo(ServiceInventoryWindow - time.Minute),
		// Known, but the ledger cannot say WHEN — a history row older than the
		// column. Never old enough.
		"unstamped": {Known: true, Terminal: true},
		"unknown":   {Known: false},
	}))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	want := SweepReport{Region: "us-west-2", Path: sweepPath, Removed: 1, Kept: 3, Unaccounted: 1, Foreign: 1}
	if report != want {
		t.Errorf("report = %+v, want %+v", report, want)
	}

	remaining := f.paramNames()
	for _, kept := range []string{open, recent, unknown, foreign, unstamped} {
		if !contains(remaining, kept) {
			t.Errorf("%s was removed; only a lease closed longer than the window may lose its "+
				"registration", kept)
		}
	}

	if contains(remaining, aged) {
		t.Errorf("%s survived; its lease closed longer ago than any build could still run", aged)
	}

	// THE UNKNOWN ONE IS NAMED, because it is a person's question, and the value
	// the fake handed back is nowhere in what was written.
	if !strings.Contains(logged.String(), unknown) {
		t.Errorf("the registration for a lease the ledger has never seen was not named in the log:\n%s",
			logged.String())
	}

	if strings.Contains(logged.String(), fakeCiphertext) {
		t.Errorf("the ciphertext the service returned reached the log")
	}
}

// A LEDGER THAT CANNOT ANSWER STOPS THE PASS. Every name before the failure was
// decided on an answer; every name after it would be decided on silence, so nothing
// after it is touched — and the error names the lease and says so.
func TestALedgerErrorStopsTheSweepWithNothingFurtherRemoved(t *testing.T) {
	f := newFakeAWS(t)

	// Sorted by the fake: a, b, c.
	first := f.stage("billet-a")
	f.stage("billet-b")
	third := f.stage("billet-c")

	s, _ := newTestSweeper(t, f)

	report, err := s.Sweep(t.Context(), func(_ context.Context, leaseID string) (LeaseClosure, error) {
		if leaseID == "b" {
			return LeaseClosure{}, errors.New("database is locked")
		}

		return closedAgo(ServiceInventoryWindow + time.Hour), nil
	})
	if err == nil {
		t.Fatal("a ledger error was swallowed; the sweep would go on deciding from silence")
	}

	for _, want := range []string{"lease b", "nothing further removed", "database is locked"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}

	if report.Removed != 1 {
		t.Errorf("Removed = %d, want 1: the name decided before the failure was proved dead", report.Removed)
	}

	remaining := f.paramNames()
	if contains(remaining, first) {
		t.Errorf("%s survived, but its lease was proved dead before the ledger failed", first)
	}

	if !contains(remaining, third) {
		t.Errorf("%s was removed AFTER the ledger stopped answering; that is a delete decided on silence",
			third)
	}
}

// EVERY PAGE IS WALKED. Parameter Store hands back ten names a page, and a sweep
// that stopped at the first page would leave everything past it forever — while
// reporting a clean pass.
func TestTheSweepPagesThroughEveryRegistration(t *testing.T) {
	f := newFakeAWS(t)

	answers := map[string]LeaseClosure{}

	for i := range 25 {
		id := "lease-" + string(rune('a'+i))
		f.stage("billet-" + id)
		answers[id] = closedAgo(ServiceInventoryWindow + time.Hour)
	}

	s, _ := newTestSweeper(t, f)

	report, err := s.Sweep(t.Context(), closureTable(t, answers))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if report.Removed != 25 {
		t.Errorf("Removed = %d, want 25", report.Removed)
	}

	if got := f.paramNames(); len(got) != 0 {
		t.Errorf("%d registration(s) survived a sweep that had proof against every one: %v", len(got), got)
	}

	f.mu.Lock()
	calls := f.paramListCalls
	f.mu.Unlock()

	if calls != 3 {
		t.Errorf("GetParametersByPath was called %d time(s) for 25 names at 10 a page, want 3", calls)
	}
}

// A LISTING THAT NEVER ENDS IS REFUSED, not followed forever.
func TestARepeatedPaginationTokenStopsTheSweep(t *testing.T) {
	f := newFakeAWS(t)
	f.paramCycle = true

	for i := range 12 {
		f.stage("billet-" + string(rune('a'+i)))
	}

	s, _ := newTestSweeper(t, f)

	_, err := s.Sweep(t.Context(), func(context.Context, string) (LeaseClosure, error) {
		return LeaseClosure{Known: true}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "revisited a pagination token") {
		t.Fatalf("a cycling token was followed: %v", err)
	}

	f.mu.Lock()
	calls := f.paramListCalls
	f.mu.Unlock()

	if calls > 3 {
		t.Errorf("the sweep followed the cycle %d times before refusing", calls)
	}
}

// THE SWEEP NEVER DECODES A VALUE, and the assertion is about the TYPE. A struct
// with a Value field would hold registration bytes wherever the response went; the
// one the sweep decodes into has exactly one field per parameter, its name.
func TestTheSweepNeverDecodesAValue(t *testing.T) {
	elem := reflect.TypeFor[stagedParameter]()

	var fields []string
	for field := range elem.Fields() {
		fields = append(fields, field.Name)
	}

	// THE NAME AND THE WRITE TIME, AND NOTHING ELSE. The write time is the second
	// age proof and carries nothing secret; a Value field, whatever the request
	// asked for, is a registration held where a report or a log can reach it.
	if !slices.Equal(fields, []string{"Name", "LastModifiedDate"}) {
		t.Fatalf("the page decodes %v per parameter; only the name and the write time may be "+
			"decoded, because anything else is a registration held where a report or a log "+
			"can reach it", fields)
	}

	// AND THE REQUEST SAYS SO. The fake fails the test on a request that asks for
	// decryption or recursion, so running one pass is the assertion.
	f := newFakeAWS(t)
	f.stage("billet-x")

	s, logged := newTestSweeper(t, f)

	if _, err := s.Sweep(t.Context(), closureTable(t, map[string]LeaseClosure{
		"x": {Known: true},
	})); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if strings.Contains(logged.String(), fakeCiphertext) {
		t.Errorf("the ciphertext the service returned reached the log")
	}
}

// THE PARAMETER'S OWN AGE IS A SECOND PROOF, AND THE LEDGER'S ALONE IS NOT ENOUGH.
//
// finished_at is written by whichever billet closed the lease, with that process's
// clock. A clock a window slow closes a lease with a finished_at that is already old
// enough, and a correctly timed controller would then release a registration whose
// build is still queued. AWS's own write time cannot be moved by any billet clock,
// so a young parameter is kept whatever the ledger says.
func TestAYoungParameterIsKeptWhateverTheLedgersClockSays(t *testing.T) {
	f := newFakeAWS(t)

	young := f.stageWrittenAt("billet-young", sweepNow.Add(-time.Hour))
	unstamped := f.stageWrittenAt("billet-unstamped", time.Time{})
	old := f.stage("billet-old")

	s, _ := newTestSweeper(t, f)

	// EVERY lease is closed long ago according to the ledger.
	report, err := s.Sweep(t.Context(), func(context.Context, string) (LeaseClosure, error) {
		return closedAgo(ServiceInventoryWindow + 40*time.Hour), nil
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if report.Removed != 1 || report.Kept != 2 {
		t.Fatalf("report = %+v; want only the old parameter removed", report)
	}

	remaining := f.paramNames()
	if !contains(remaining, young) || !contains(remaining, unstamped) {
		t.Errorf("a parameter AWS wrote recently, or one with no write time, was removed on the "+
			"ledger's word alone: %v", remaining)
	}

	if contains(remaining, old) {
		t.Errorf("%s survived with both proofs against it", old)
	}
}

// EXACTLY ONE WINDOW OLD IS NOT OLDER THAN THE WINDOW. The rule is "closed LONGER
// AGO than the window", and a boundary that fell through to deletion would be a
// stated invariant the code does not hold.
func TestAClosureExactlyOneWindowOldIsKept(t *testing.T) {
	f := newFakeAWS(t)
	f.stage("billet-edge")
	f.stageWrittenAt("billet-edge2", sweepNow.Add(-ServiceInventoryWindow))

	s, _ := newTestSweeper(t, f)

	report, err := s.Sweep(t.Context(), closureTable(t, map[string]LeaseClosure{
		"edge":  closedAgo(ServiceInventoryWindow),
		"edge2": closedAgo(ServiceInventoryWindow + time.Hour),
	}))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if report.Removed != 0 || report.Kept != 2 {
		t.Fatalf("report = %+v; a closure or a write exactly one window old is still covered "+
			"by the window", report)
	}
}

// A NAME LISTED TWICE IS COUNTED ONCE. The cursor is positional, so a name staged
// between two page requests that sorts before the cursor shifts the listing and
// the previous page's last name comes back — and a terminal lease's registration
// would otherwise be "removed" twice, the second time as ParameterNotFound, with
// the inflated count accumulated for good.
func TestANameRepeatedAcrossPagesIsRemovedAndCountedOnce(t *testing.T) {
	f := newFakeAWS(t)

	answers := map[string]LeaseClosure{}

	for i := range 12 {
		id := "lease-" + string(rune('m'+i))
		f.stage("billet-" + id)
		answers[id] = closedAgo(ServiceInventoryWindow + time.Hour)
	}

	// After page one is computed, a node stages a name that sorts BEFORE every
	// name on it, so page two's offset lands one short and repeats page one's last
	// name.
	f.onParamPage = func(page int) {
		if page == 1 {
			f.params[sweepPath+"/billet-lease-a"] = theRegistration
			f.paramWritten[sweepPath+"/billet-lease-a"] = sweepNow.Add(-time.Hour)
		}
	}

	answers["lease-a"] = LeaseClosure{Known: true}

	s, _ := newTestSweeper(t, f)

	report, err := s.Sweep(t.Context(), closureTable(t, answers))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if report.Removed != 12 {
		t.Errorf("Removed = %d, want 12: a name the cursor repeated was counted twice", report.Removed)
	}

	if got := f.paramNames(); !slices.Equal(got, []string{sweepPath + "/billet-lease-a"}) {
		t.Errorf("remaining = %v, want only the name staged mid-listing", got)
	}
}

// A PARAMETER THE NODE ALREADY REMOVED COUNTS AS REMOVED, because the two run
// where the other may already have acted and an absent credential is the outcome
// both wanted.
func TestAParameterAlreadyGoneCountsAsRemoved(t *testing.T) {
	f := newFakeAWS(t)
	f.stage("billet-gone")
	f.deleteParamErr = []apiFault{{status: http.StatusBadRequest, code: "ParameterNotFound"}}

	s, _ := newTestSweeper(t, f)

	report, err := s.Sweep(t.Context(), closureTable(t, map[string]LeaseClosure{
		"gone": closedAgo(ServiceInventoryWindow + time.Hour),
	}))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if report.Removed != 1 {
		t.Errorf("Removed = %d, want 1", report.Removed)
	}
}

// A REFUSED DELETE STOPS THE PASS AND SAYS WHY, BY CODE ALONE. The message is the
// service's prose about a request that names registrations, and a code is what an
// operator acts on.
func TestARefusedDeleteStopsTheSweepAndNamesTheCode(t *testing.T) {
	f := newFakeAWS(t)
	f.stage("billet-denied")
	f.deleteParamErr = []apiFault{{status: http.StatusBadRequest, code: "AccessDeniedException"}}

	s, _ := newTestSweeper(t, f)

	report, err := s.Sweep(t.Context(), closureTable(t, map[string]LeaseClosure{
		"denied": closedAgo(ServiceInventoryWindow + time.Hour),
	}))
	if err == nil {
		t.Fatal("a refused delete was reported as a clean pass")
	}

	if !strings.Contains(err.Error(), "AccessDeniedException") {
		t.Errorf("the error does not name the code: %v", err)
	}

	if strings.Contains(err.Error(), "scripted") {
		t.Errorf("the error carries the service's prose: %v", err)
	}

	if report.Removed != 0 {
		t.Errorf("Removed = %d after a refused delete", report.Removed)
	}
}

// AND A LISTING FAILURE IS THE SAME SHAPE: nothing removed, the code named.
func TestAListingFailureRemovesNothing(t *testing.T) {
	f := newFakeAWS(t)
	f.stage("billet-x")
	f.listParamsErr = []apiFault{
		{status: http.StatusBadRequest, code: "AccessDeniedException"},
		{status: http.StatusBadRequest, code: "AccessDeniedException"},
		{status: http.StatusBadRequest, code: "AccessDeniedException"},
	}

	s, _ := newTestSweeper(t, f)

	_, err := s.Sweep(t.Context(), func(context.Context, string) (LeaseClosure, error) {
		t.Error("the ledger was asked although the listing failed")

		return LeaseClosure{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "AccessDeniedException") {
		t.Fatalf("a failed listing was not reported by its code: %v", err)
	}

	if got := f.paramNames(); len(got) != 1 {
		t.Errorf("a failed listing removed something: %v", got)
	}
}

// THE CONSTRUCTOR RE-APPLIES THE PATH RULE, because the path arrived over the wire
// and is a prefix this process will delete under.
func TestTheSweeperRefusesAPathItMustNotSweep(t *testing.T) {
	for name, path := range map[string]string{
		"empty":    "",
		"wildcard": "/billet/*",
		"reserved": "/aws/billet",
		"relative": "billet/jit",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewRegistrationSweeper("us-west-2", path, staticCreds{}); err == nil {
				t.Fatalf("path %q was accepted", path)
			}
		})
	}

	if _, err := NewRegistrationSweeper("us-west-2", sweepPath, nil); err == nil {
		t.Fatal("a nil credential source was accepted")
	}

	if _, err := NewRegistrationSweeper("uswest2", sweepPath, staticCreds{}); err == nil {
		t.Fatal("a region that is not one was accepted")
	}

	s, err := NewRegistrationSweeper("us-west-2", sweepPath, staticCreds{})
	if err != nil {
		t.Fatalf("a valid sweeper was refused: %v", err)
	}

	if _, err := s.Sweep(t.Context(), nil); err == nil {
		t.Fatal("a sweep with no ledger lookup ran; nothing else may authorise a delete")
	}
}

func contains(names []string, want string) bool { return slices.Contains(names, want) }
