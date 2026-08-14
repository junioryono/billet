package runnerrelease

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// THE PIN IS ONE VALUE, and the guest image build script reads the same file.
//
// It was a Go constant in `billet ami` and a shell default in the image script,
// which made bumping the runner two edits in two languages. Doing one of them
// produces a fleet where the ec2 backend is current and the microVM backend is not,
// or the reverse — discovered on the day GitHub stops queueing to the stale half.
func TestThePinnedRunnerVersionLooksLikeAVersion(t *testing.T) {
	t.Parallel()

	got := Pinned()

	if got == "" {
		t.Fatal("nothing is pinned, so a build has no runner to install")
	}

	if strings.HasPrefix(got, "v") {
		t.Errorf("the pin is %q; it is used to build a download URL that already has its own "+
			"`v`, so this would ask for v v2.x", got)
	}

	if strings.ContainsAny(got, " \t\r\n") {
		t.Errorf("the pin %q carries whitespace, and it is interpolated into a URL and a "+
			"filename", got)
	}

	if strings.Count(got, ".") != 2 {
		t.Errorf("the pin %q is not a three-part release like 2.328.0", got)
	}
}

// A RELEASE THAT IS THE PINNED ONE IS NOT A DEADLINE.
func TestBeingCurrentIsNeverDue(t *testing.T) {
	t.Parallel()

	published := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	s := Status{
		Pinned: "2.328.0", Latest: "2.328.0",
		Published: published, Deadline: published.Add(Grace),
	}

	if !s.Current() {
		t.Fatal("the pinned release is the published one and Current says otherwise")
	}

	// A YEAR LATER, still not due: nothing has been released since, so there is
	// nothing to update to. Counting the age of the release billet already has
	// would report a fleet as expiring for standing still while GitHub did too.
	late := published.Add(365 * 24 * time.Hour)

	if s.Due(late) {
		t.Error("a fleet running the newest release was reported as needing a rebuild")
	}

	if s.Expired(late) {
		t.Error("a fleet running the newest release was reported as refused by github")
	}
}

// AND A RELEASE BILLET HAS NOT TAKEN UP IS A CLOCK.
func TestAnUnappliedReleaseBecomesDueAndThenExpires(t *testing.T) {
	t.Parallel()

	published := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	s := Status{
		Pinned: "2.328.0", Latest: "2.329.0",
		Published: published, Deadline: published.Add(Grace),
	}

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

			if got := s.Due(tc.at); got != tc.due {
				t.Errorf("Due = %v, want %v", got, tc.due)
			}

			if got := s.Expired(tc.at); got != tc.expired {
				t.Errorf("Expired = %v, want %v", got, tc.expired)
			}
		})
	}
}

// THE WARNING COMES BEFORE THE DEADLINE, WITH ROOM TO ACT.
//
// What this warns about is not a click: it is building an image, proving it boots
// and registers, and rolling a fleet onto it. A warning that arrived the day before
// would be an alarm about something already too late to do calmly.
func TestTheWarningLeavesTimeToActOnIt(t *testing.T) {
	t.Parallel()

	if Warn >= Grace {
		t.Fatalf("the warning at %v is not before the deadline at %v", Warn, Grace)
	}

	if left := Grace - Warn; left < 7*24*time.Hour {
		t.Errorf("the warning leaves %v to rebuild, verify and roll a fleet", left)
	}
}

// AND WHAT GITHUB SAYS IS READ CORRECTLY, including the `v` its tags carry.
func TestTheReleaseFeedIsRead(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeOrFail(t, w, []byte(`{"tag_name":"v2.336.0","published_at":"2026-07-20T17:45:55Z"}`))
	}))
	defer srv.Close()

	got, err := latestFrom(t.Context(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("latestFrom: %v", err)
	}

	if got.Latest != "2.336.0" {
		t.Errorf("the release is %q; the tag's `v` belongs to the tag, not the version", got.Latest)
	}

	want := time.Date(2026, 7, 20, 17, 45, 55, 0, time.UTC)
	if !got.Published.Equal(want) {
		t.Errorf("published at %v, want %v", got.Published, want)
	}

	if !got.Deadline.Equal(want.Add(Grace)) {
		t.Errorf("the deadline is %v, and it is counted from the release date", got.Deadline)
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

	if _, err := latestFrom(t.Context(), srv.Client(), srv.URL); err == nil {
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

	_, err := latestFrom(t.Context(), srv.Client(), srv.URL)
	if err == nil {
		t.Fatal("a 403 was treated as an answer")
	}

	if !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("the error does not say what a 403 usually means here: %v", err)
	}
}

// A RESPONSE WITH NO DATE IS REFUSED, because the deadline is counted from it and
// a zero time would put every fleet permanently past due.
func TestAReleaseWithNoDateIsRefused(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeOrFail(t, w, []byte(`{"tag_name":"v2.336.0"}`))
	}))
	defer srv.Close()

	_, err := latestFrom(t.Context(), srv.Client(), srv.URL)
	if err == nil {
		t.Fatal("a release with no publication date was accepted, and the deadline is counted " +
			"from that date")
	}

	if !strings.Contains(err.Error(), "published") {
		t.Errorf("the error does not name the missing field: %v", err)
	}
}

// writeOrFail writes a fixture response and fails the test if it cannot, so a
// broken fixture is reported as a broken fixture rather than as a broken subject.
func writeOrFail(t *testing.T, w http.ResponseWriter, body []byte) {
	t.Helper()

	if _, err := w.Write(body); err != nil {
		t.Errorf("write the fixture response: %v", err)
	}
}
