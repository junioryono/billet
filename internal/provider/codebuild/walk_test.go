package codebuild

import (
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// AN ID THE PROJECT LISTED AND THE BATCH CANNOT DESCRIBE FAILS THE WHOLE INVENTORY.
//
// This is the failure the provider contract warns about in its sharpest form: `List`
// frees the capacity of every lease ABSENT from it, so a build billet cannot describe
// must not be silently omitted — that hands back the capacity of compute which may
// still be running somebody's deploy. Build history is retained for a year, so an id
// the listing just produced and the batch cannot resolve is not the ordinary aged-out
// case; it is billet unable to say anything one API call after being told the build
// exists.
//
// THE FIRST VERSION DISCARDED THE NOT-FOUND LIST with `builds, _, err :=`, so this
// case returned a SHORT, EMPTY, SUCCESSFUL answer.
func TestAnUndescribableBuildFailsTheInventoryRatherThanShorteningIt(t *testing.T) {
	f := newFakeAWS(t)

	// The listing names a build the fake does not hold, which is exactly what
	// BatchGetBuilds reports as buildsNotFound.
	f.addOwnedBuild("live", provider.InstanceName("live"))
	f.listPages = []listPage{{ids: []string{"live", "vanished"}}}

	p := newTestProvider(t, f, nil)

	_, err := p.List(t.Context())
	if err == nil {
		t.Fatal("a build the project listed and the batch could not describe was silently " +
			"omitted from the inventory, which frees the capacity of compute that may still " +
			"be running")
	}

	for _, want := range []string{"vanished", "could not describe", "inventory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// AND THE SAME FOR Find, which shares the walk. A Find that quietly reported "no such
// build" here would tell the custody path that a launch left nothing behind.
func TestAnUndescribableBuildFailsFindRatherThanReportingAbsence(t *testing.T) {
	f := newFakeAWS(t)

	f.addOwnedBuild("live", provider.InstanceName("live"))
	f.listPages = []listPage{{ids: []string{"live", "vanished"}}}

	p := newTestProvider(t, f, nil)

	inst, found, err := p.Find(t.Context(), provider.InstanceName("other"))
	if err == nil {
		t.Fatal("Find reported an absence while billet could not describe a build in its own " +
			"project; the caller would read that as 'the launch started nothing'")
	}

	if inst != nil || found {
		t.Errorf("Find returned (%v, %v) alongside an error", inst, found)
	}
}

// A QUEUED BUILD STARTS OUT OF ORDER, AND THE WALK MUST NOT STOP BEFORE REACHING IT.
//
// The listing is ordered by build NUMBER, which is submission order. A build that
// queued for hours starts LATER than a build submitted after it — so a page of
// higher-numbered builds can all have started outside the live window while a
// lower-numbered one, still queued while they ran, is inside it and executing
// somebody's job.
//
// THE FIRST VERSION STOPPED ON THE FIRST SUCH PAGE, which dropped exactly that build.
// The stop condition now uses a cutoff older than the live window by the queued
// ceiling, which is the maximum a start can lag a submission.
func TestTheWalkDoesNotStopBeforeAQueueDelayedBuild(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, func(cfg *config.CodeBuildConfig) {
		cfg.BuildTimeoutMinutes = 60
		cfg.QueuedTimeoutMinutes = 480
	})

	now := time.Now()

	// Page 1: higher-numbered builds, all started well outside the live window —
	// but INSIDE the abandon cutoff, because the queued ceiling is 8 hours.
	//
	// The live window is 60 + 480 + 60 = 600 minutes, so the abandon cutoff is
	// 1080 minutes. These are 700 minutes old: outside the live window (so not
	// reported), inside the abandon cutoff (so the walk must continue).
	for _, id := range []string{"newer-1", "newer-2"} {
		f.addOwnedBuild(id, provider.InstanceName(id))
		f.builds[id].started = now.Add(-700 * time.Minute)
		f.builds[id].status = "SUCCEEDED"
	}

	// Page 2: the lower-numbered build that queued for hours and is running now.
	queued := provider.InstanceName("queued")
	f.addOwnedBuild("queued", queued)
	f.builds["queued"].started = now.Add(-10 * time.Minute)

	f.listPages = []listPage{
		{ids: []string{"newer-1", "newer-2"}, nextToken: "page2"},
		{ids: []string{"queued"}},
	}

	got, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(got) != 1 || got[0].Name != queued {
		t.Fatalf("List returned %+v; the walk stopped before reaching a queue-delayed build "+
			"that is still running, so its capacity would be freed underneath it", got)
	}

	// AND IT ACTUALLY PAGED. Without this the assertion above passes against a walk
	// that happened to see everything on page one.
	if f.listCalls < 2 {
		t.Errorf("the walk made %d listing calls, so it never reached page 2", f.listCalls)
	}
}

// BUT IT DOES STOP EVENTUALLY, or every sweep walks a year of history.
//
// The other direction, and it needs asserting: a stop condition that never fires is
// the same cost as no window at all, and this walk runs on every reap.
func TestTheWalkStopsOncePastTheAbandonCutoff(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, func(cfg *config.CodeBuildConfig) {
		cfg.BuildTimeoutMinutes = 60
		cfg.QueuedTimeoutMinutes = 5
	})

	now := time.Now()

	// The live window is 60 + 5 + 60 = 125 minutes; the abandon cutoff is 130. These
	// are a week old, so the walk must not ask for page 2.
	for _, id := range []string{"ancient-1", "ancient-2"} {
		f.addOwnedBuild(id, provider.InstanceName(id))
		f.builds[id].started = now.Add(-7 * 24 * time.Hour)
		f.builds[id].status = "SUCCEEDED"
	}

	f.addOwnedBuild("also-ancient", provider.InstanceName("also-ancient"))
	f.builds["also-ancient"].started = now.Add(-7 * 24 * time.Hour)

	f.listPages = []listPage{
		{ids: []string{"ancient-1", "ancient-2"}, nextToken: "page2"},
		{ids: []string{"also-ancient"}},
	}

	got, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("List reported builds a week old: %+v", got)
	}

	if f.listCalls != 1 {
		t.Errorf("the walk made %d listing calls; a page entirely past the abandon cutoff "+
			"should end it, or every sweep walks a year of history", f.listCalls)
	}
}

// A ZERO START TIME IS INSIDE BOTH CUTOFFS.
//
// Zero is what CodeBuild reports for a build that has not started — SUBMITTED or
// QUEUED — and those are the builds most likely to be about to run somebody's job.
// Treating zero as ancient would drop them from the inventory AND let the walk stop on
// a page full of them.
func TestABuildThatHasNotStartedIsNeitherDroppedNorEndsTheWalk(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	queued := provider.InstanceName("queued")
	f.addOwnedBuild("queued", queued)
	f.builds["queued"].started = time.Unix(0, 0)
	f.builds["queued"].phase = "QUEUED"

	f.addOwnedBuild("running", provider.InstanceName("running"))

	f.listPages = []listPage{
		{ids: []string{"queued"}, nextToken: "page2"},
		{ids: []string{"running"}},
	}

	got, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(got) != 2 {
		t.Errorf("List returned %d builds, want both the queued and the running one: %+v",
			len(got), got)
	}

	if f.listCalls < 2 {
		t.Errorf("a page of not-yet-started builds ended the walk after %d call(s)", f.listCalls)
	}
}
