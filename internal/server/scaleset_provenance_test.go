package server

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

const testOrg = "acme"

// testTarget is the one target these control planes serve.
var testTarget = config.GitHubTarget{Name: config.DefaultTargetName, Org: testOrg}

// capturingHandler keeps the records a run emitted, so a test can assert on what
// an operator would actually read rather than on a boolean somewhere.
type capturingHandler struct {
	mu      *sync.Mutex
	records *[]slog.Record
	attrs   []slog.Attr
}

func newCapturingHandler() *capturingHandler {
	return &capturingHandler{mu: &sync.Mutex{}, records: &[]slog.Record{}}
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	rec := r.Clone()
	rec.AddAttrs(h.attrs...)

	*h.records = append(*h.records, rec)

	return nil
}

// Bound attributes are KEPT. Returning the same handler discards them, so a
// harmless change to logger.With("tier", …) would make this double observe
// different records from a real handler and the assertions below would stop
// meaning anything.
func (h *capturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)

	return &next
}

func (h *capturingHandler) WithGroup(string) slog.Handler { return h }

// BY GROUP AND LABEL, because the same label in two runner groups is two scale
// sets: filtering on the label alone cannot express the case where one is
// declared and the other is an orphan.
func (h *capturingHandler) warningsAbout(group, label string) []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()

	var out []slog.Record

	for i := range *h.records {
		r := &(*h.records)[i]
		if r.Level != slog.LevelWarn {
			continue
		}

		var gotGroup, gotLabel string

		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case "name":
				gotLabel = a.Value.String()
			case "group":
				gotGroup = a.Value.String()
			}

			return true
		})

		if gotGroup == group && gotLabel == label {
			out = append(out, *r)
		}
	}

	return out
}

// A SCALE SET BILLET MADE AND NO LONGER DECLARES IS REPORTED, and one it still
// declares is not.
//
// Removing a tier from the config is the ordinary way to stop offering a size,
// and the object it created stays on the organization advertising nothing — so a
// job aimed at that label queues rather than failing. Nothing said so, because
// the config was the only index and the service's client cannot enumerate a
// runner group.
func TestAScaleSetNoLongerDeclaredIsReported(t *testing.T) {
	declared := tier("billet-4vcpu-a")
	tiers := []config.Tier{declared}

	db := openState(t)

	// What a previous config declared and this one does not.
	if declared.RunnerGroup != "" {
		t.Fatalf("this test needs a tier naming no runner group, got %q", declared.RunnerGroup)
	}

	const group = "default"

	for _, rec := range []state.ScaleSetRecord{
		{Target: testOrg, RunnerGroup: group, Label: declared.Label, ID: 40},
		{Target: testOrg, RunnerGroup: group, Label: "billet-8vcpu", ID: 41},
	} {
		if err := db.RecordScaleSet(t.Context(), rec); err != nil {
			t.Fatalf("RecordScaleSet(%s): %v", rec.Label, err)
		}
	}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	// The report happens during reconcile, before any listener polls, so the
	// first poll is a safe place to stop.
	prov := &fakeProvisioner{
		newSession: func(string) Session {
			return &fakeSession{onPoll: func(int) { cancel() }}
		},
	}

	handler := newCapturingHandler()

	if err := New(a, prov, tiers, "test-owner", slog.New(handler),
		WithCompletionLedger(db), WithTargets(Target{Config: testTarget, Provisioner: prov})).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := handler.warningsAbout(group, "billet-8vcpu")
	if len(got) == 0 {
		t.Fatal("a scale set billet created and no longer declares was not reported")
	}

	// The message has to carry the way out, because the config no longer names
	// the tier and teardown needs its runner group.
	msg := got[0].Message
	for _, want := range []string{"no longer declared", "billet teardown"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the report does not mention %q: %s", want, msg)
		}
	}

	if warned := handler.warningsAbout(group, declared.Label); len(warned) != 0 {
		t.Errorf("a tier the config still declares was reported as an orphan: %v", warned)
	}
}

// AND THE RECORD IS WRITTEN BY AN ORDINARY RECONCILE, which is what makes the
// report above possible for a tier removed later. Without it nothing accumulates
// and the whole mechanism reports nothing, forever, while looking healthy.
func TestReconcilingATierRecordsItsScaleSet(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	db := openState(t)
	a := newAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	prov := &fakeProvisioner{
		newSession: func(string) Session {
			return &fakeSession{onPoll: func(int) { cancel() }}
		},
	}

	if err := New(a, prov, tiers, "test-owner", nil,
		WithCompletionLedger(db), WithTargets(Target{Config: testTarget, Provisioner: prov})).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	recorded, err := db.ScaleSets(t.Context(), testOrg)
	if err != nil {
		t.Fatalf("ScaleSets: %v", err)
	}

	if len(recorded) != 1 || recorded[0].Label != "billet-4vcpu-a" {
		t.Fatalf("ScaleSets = %v, want the reconciled tier", recorded)
	}

	if recorded[0].ID <= 0 {
		t.Errorf("recorded id is %d, want the one the service issued", recorded[0].ID)
	}
}

// runWithRecords runs a control plane over the given targets with these scale
// sets already recorded, stopping at the first poll, and returns what Run
// answered and what it logged.
func runWithRecords(t *testing.T, tiers []config.Tier, records []state.ScaleSetRecord,
	targets ...config.GitHubTarget,
) (*capturingHandler, *fakeProvisioner, error) {
	t.Helper()

	db := openState(t)

	for _, rec := range records {
		if err := db.RecordScaleSet(t.Context(), rec); err != nil {
			t.Fatalf("RecordScaleSet(%s): %v", rec.Label, err)
		}
	}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	prov := &fakeProvisioner{
		newSession: func(string) Session {
			return &fakeSession{onPoll: func(int) { cancel() }}
		},
	}

	served := make([]Target, 0, len(targets))
	for _, target := range targets {
		served = append(served, Target{Config: target, Provisioner: prov})
	}

	handler := newCapturingHandler()

	err := New(a, prov, tiers, "test-owner", slog.New(handler),
		WithCompletionLedger(db), WithTargets(served...)).Run(ctx)

	return handler, prov, err
}

// THE SAME NAME IN ANOTHER RUNNER GROUP OF ONE TARGET IS REFUSED (#140). A job
// for that name can be given to either scale set, and the one billet left
// behind is polled by nothing, so the tier's jobs routed there wait however much
// room it has. The refusal names the teardown that clears it, and the declared
// tier's own scale set is neither refused nor reported.
func TestAStaleScaleSetSharingATiersNameRefusesTheStart(t *testing.T) {
	declared := tier("billet-4vcpu-a")
	declared.RunnerGroup = "billet"

	// Same name, another group: a set an earlier config created elsewhere.
	handler, prov, err := runWithRecords(t, []config.Tier{declared}, []state.ScaleSetRecord{
		{Target: testOrg, RunnerGroup: "retired", Label: declared.Label, ID: 41},
	}, testTarget)

	if err == nil {
		t.Fatal("a control plane started beside a scale set it left behind under a declared tier's name")
	}

	want := "billet teardown --tier billet-4vcpu-a --runner-group retired --target " + config.DefaultTargetName
	if !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not name the teardown that clears it (%q): %v", want, err)
	}

	if got := handler.warningsAbout("billet", declared.Label); len(got) != 0 {
		t.Errorf("the declared tier's own scale set was reported as an orphan: %v", got)
	}

	prov.mu.Lock()
	created := len(prov.labels)
	prov.mu.Unlock()

	if created != 0 {
		t.Errorf("the refused start created %d scale sets before refusing", created)
	}
}

// AND A STALE SET AT THE ORGANIZATION TAKES A REPOSITORY'S JOBS. A repository
// target's jobs can also go to its organization's scale sets, so one billet
// left there under a name the repository's tier answers to is refused too, read
// from the ledger though this control plane no longer serves the organization.
func TestAStaleOrganizationScaleSetRefusesARepositorysTier(t *testing.T) {
	repo := config.GitHubTarget{Name: "app", Repository: testOrg + "/app"}

	declared := tier("billet-4vcpu")
	declared.Target = repo.Name

	_, _, err := runWithRecords(t, []config.Tier{declared}, []state.ScaleSetRecord{
		{Target: testOrg, RunnerGroup: "default", Label: "billet-4vcpu", ID: 51},
	}, repo)

	if err == nil || !strings.Contains(err.Error(), "organization "+testOrg) {
		t.Fatalf("a repository's tier started beside a stale organization scale set under its name: %v", err)
	}
}

// AND THE OTHER WAY ROUND, WITH BOTH SERVED: a set billet left on one of the
// organization's repositories under a name the organization's tier answers to
// takes that repository's jobs for it.
func TestAStaleRepositoryScaleSetRefusesTheOrganizationsTier(t *testing.T) {
	repo := config.GitHubTarget{Name: "app", Repository: testOrg + "/app"}

	declared := tier("billet-4vcpu")
	declared.Target = testTarget.Name
	other := tier("billet-2vcpu-app")
	other.Target = repo.Name

	_, _, err := runWithRecords(t, []config.Tier{declared, other}, []state.ScaleSetRecord{
		{Target: repo.Repository, RunnerGroup: "default", Label: "billet-4vcpu", ID: 52},
	}, testTarget, repo)

	if err == nil || !strings.Contains(err.Error(), `"billet-4vcpu" answers to on `+testOrg) {
		t.Fatalf("the organization's tier started beside a stale set on its repository under its name: %v", err)
	}
}

// AND ONE THAT TAKES NO DECLARED TIER'S NAME IS STILL ONLY REPORTED. A stale
// set under a name nothing answers to queues jobs aimed at that name, which is
// worth a warning, and refusing the whole control plane for it would be an
// outage over a size nobody asked for.
func TestAStaleScaleSetUnderAnotherNameOnlyWarns(t *testing.T) {
	declared := tier("billet-4vcpu-a")

	handler, _, err := runWithRecords(t, []config.Tier{declared}, []state.ScaleSetRecord{
		{Target: testOrg, RunnerGroup: "retired", Label: "billet-16vcpu", ID: 42},
	}, testTarget)
	if err != nil {
		t.Fatalf("Run refused over a stale set no tier's name is routed to: %v", err)
	}

	if len(handler.warningsAbout("retired", "billet-16vcpu")) == 0 {
		t.Error("the stale set under another name was not reported")
	}
}

// REMOVING THE LAST TIER IS THE EDIT MOST LIKELY TO STRAND ONE, and the refusal
// that follows used to return before anything was reported — so the one case
// that guarantees an orphan was the one case that never named it.
func TestRemovingTheLastTierStillReportsWhatItLeftBehind(t *testing.T) {
	db := openState(t)

	gone := state.ScaleSetRecord{
		Target: testOrg, RunnerGroup: "billet", Label: "billet-8vcpu", ID: 41,
	}
	if err := db.RecordScaleSet(t.Context(), gone); err != nil {
		t.Fatalf("RecordScaleSet: %v", err)
	}

	handler := newCapturingHandler()

	err := New(nil, nil, nil, "test-owner", slog.New(handler),
		WithCompletionLedger(db), WithTargets(Target{Config: testTarget})).Run(t.Context())
	if err == nil {
		t.Fatal("a control plane with no tiers was accepted")
	}

	if got := handler.warningsAbout("billet", "billet-8vcpu"); len(got) == 0 {
		t.Error("removing the last tier reported nothing, so the scale set it stranded " +
			"is invisible in exactly the case that guarantees one")
	}
}

// A TIER WITH runs_on IS ITS SCALE SET BY THAT NAME. The set is created,
// recorded and declared under runs_on, and one left behind under the tier's own
// label, as a tier moved onto a shared name leaves it, is reported like any
// other set no tier declares.
func TestATierIsReconciledUnderItsRunsOn(t *testing.T) {
	moved := tier("team-a-4vcpu")
	moved.RunsOn = "billet-4vcpu"
	tiers := []config.Tier{moved}

	db := openState(t)

	const group = "default"

	left := state.ScaleSetRecord{Target: testOrg, RunnerGroup: group, Label: moved.Label, ID: 40}
	if err := db.RecordScaleSet(t.Context(), left); err != nil {
		t.Fatalf("RecordScaleSet: %v", err)
	}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	prov := &fakeProvisioner{
		newSession: func(string) Session {
			return &fakeSession{onPoll: func(int) { cancel() }}
		},
	}

	handler := newCapturingHandler()

	if err := New(a, prov, tiers, "test-owner", slog.New(handler),
		WithCompletionLedger(db), WithTargets(Target{Config: testTarget, Provisioner: prov})).Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	prov.mu.Lock()
	created := slices.Collect(maps.Values(prov.labels))
	prov.mu.Unlock()

	if !slices.Equal(created, []string{"billet-4vcpu"}) {
		t.Errorf("created scale sets %v, want only the tier's runs_on", created)
	}

	recorded, err := db.ScaleSets(t.Context(), testOrg)
	if err != nil {
		t.Fatalf("ScaleSets: %v", err)
	}

	names := make([]string, 0, len(recorded))
	for _, rec := range recorded {
		names = append(names, rec.Label)
	}

	if !slices.Contains(names, "billet-4vcpu") {
		t.Errorf("recorded %v, missing the set reconciled under runs_on", names)
	}

	if len(handler.warningsAbout(group, "team-a-4vcpu")) == 0 {
		t.Error("the set left under the tier's old name was not reported as undeclared")
	}

	if warned := handler.warningsAbout(group, "billet-4vcpu"); len(warned) != 0 {
		t.Errorf("the set the tier answers to was reported as an orphan: %v", warned)
	}
}
