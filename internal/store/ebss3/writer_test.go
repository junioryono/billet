package ebss3

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	storecontract "github.com/junioryono/billet/internal/store"
)

// A RELEASED WRITER FREES THE KEY WITHOUT MOVING THE FENCE. A settlement that
// failed at CreateSnapshot once left its lease standing for fifteen minutes, and
// the next writer waited them out.
func TestAReleasedWriterLetsTheNextWriterInWithoutWaitingOutTheLease(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 3, 2, 0, 55, 0, time.UTC)
	s, _, _ := testStore(&now)
	candidate := snapshotCandidate(t, s)
	first, firstFence, err := s.AcquireWriter(t.Context(), candidate.Key, "i-first", 15*time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter first: %v", err)
	}
	if _, _, err := s.AcquireWriter(t.Context(), candidate.Key, "i-second", 15*time.Minute); !errors.Is(err, storecontract.ErrConflict) {
		t.Fatalf("a second writer got in while the first held the key: %v", err)
	}

	if err := s.ReleaseWriter(t.Context(), first, firstFence); err != nil {
		t.Fatalf("ReleaseWriter: %v", err)
	}
	if err := s.PublishCAS(t.Context(), candidate.Key, "", candidate, first, firstFence); !errors.Is(err, storecontract.ErrConflict) {
		t.Fatalf("a released lease published: %v", err)
	}
	second, secondFence, err := s.AcquireWriter(t.Context(), candidate.Key, "i-second", 15*time.Minute)
	if err != nil {
		t.Fatalf("the next writer still had to wait out a released lease: %v", err)
	}
	if secondFence != firstFence+1 {
		t.Fatalf("fence after release = %d, want %d: the fence must move on, never back",
			secondFence, firstFence+1)
	}
	if err := s.PublishCAS(t.Context(), candidate.Key, "", candidate, second, secondFence); err != nil {
		t.Fatalf("PublishCAS with the writer acquired after the release: %v", err)
	}
	if current, err := s.Current(t.Context(), candidate.Key); err != nil || current != candidate.Generation {
		t.Fatalf("Current = %q, %v; want %q", current, err, candidate.Generation)
	}
}

// A release removes exactly the lease it is handed. Anything else — a newer
// writer's record, the same identity under another fence or expiry — is left
// standing, because the match is the triple PublishCAS checks.
func TestReleasingAWriterLeavesANewerWriterAndTheFenceStanding(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 3, 2, 0, 55, 0, time.UTC)
	s, _, _ := testStore(&now)
	candidate := snapshotCandidate(t, s)
	first, firstFence, err := s.AcquireWriter(t.Context(), candidate.Key, "job-1", time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter first: %v", err)
	}
	now = now.Add(2 * time.Minute)
	second, secondFence, err := s.AcquireWriter(t.Context(), candidate.Key, "job-2", time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter second: %v", err)
	}

	wrongExpiry := second
	wrongExpiry.Expires = second.Expires.Add(time.Second)
	for name, release := range map[string]func() error{
		"an expired predecessor": func() error { return s.ReleaseWriter(t.Context(), first, firstFence) },
		"the right identity under the wrong fence": func() error {
			return s.ReleaseWriter(t.Context(), second, secondFence+1)
		},
		"the right identity under the wrong expiry": func() error {
			return s.ReleaseWriter(t.Context(), wrongExpiry, secondFence)
		},
	} {
		if err := release(); err != nil {
			t.Fatalf("releasing %s returned %v, want a silent no-op", name, err)
		}
		state, _, err := s.load(t.Context(), candidate.Key)
		if err != nil {
			t.Fatalf("load state: %v", err)
		}
		if state.Writer == nil || state.Writer.Lease.ID != second.ID {
			t.Fatalf("releasing %s cleared the live writer: %+v", name, state.Writer)
		}
		if state.Fence != secondFence {
			t.Fatalf("releasing %s moved the fence to %d", name, state.Fence)
		}
	}
	if err := s.PublishCAS(t.Context(), candidate.Key, "", candidate, second, secondFence); err != nil {
		t.Fatalf("the live writer could not publish after the no-op releases: %v", err)
	}
}

func TestAWriterReleaseSurvivesALostConditionalWriteResponse(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 3, 2, 0, 55, 0, time.UTC)
	s, _, objects := testStore(&now)
	first, firstFence, err := s.AcquireWriter(t.Context(), "acme/api/npm", "job-1", time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	objects.afterPutErr = fmt.Errorf("%w: connection vanished after S3 committed the object",
		errObjectAmbiguous)
	if err := s.ReleaseWriter(t.Context(), first, firstFence); err != nil {
		t.Fatalf("an ambiguously acknowledged release was not recovered: %v", err)
	}
	state, _, err := s.load(t.Context(), "acme/api/npm")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.Writer != nil {
		t.Fatalf("the writer record survived the release: %+v", state.Writer)
	}
	if _, _, err := s.AcquireWriter(t.Context(), "acme/api/npm", "job-2", time.Minute); err != nil {
		t.Fatalf("the next writer was refused after the release: %v", err)
	}
}

// A release touches the writer record and nothing else: not the pointer, not a
// generation, not a candidate, not an active-clone lease, not the fence.
func TestAReleaseTouchesNothingButTheWriterRecord(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 3, 2, 0, 55, 0, time.UTC)
	s, _, _ := testStore(&now)
	one := snapshotCandidate(t, s)
	lease, fence, err := s.AcquireWriter(t.Context(), one.Key, "job-1", time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	if err := s.PublishCAS(t.Context(), one.Key, "", one, lease, fence); err != nil {
		t.Fatalf("PublishCAS: %v", err)
	}
	if _, err := s.Clone(t.Context(), one.Key, ""); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	two := snapshotCandidate(t, s)
	next, nextFence, err := s.AcquireWriter(t.Context(), one.Key, "job-2", time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter next: %v", err)
	}
	before, _, err := s.load(t.Context(), one.Key)
	if err != nil {
		t.Fatalf("load state before the release: %v", err)
	}
	if before.Pointer != one.Generation || len(before.Active) != 1 || len(before.Generations) != 1 {
		t.Fatalf("the fixture is missing something to protect: %+v", before)
	}
	if _, ok := before.Candidates[two.Generation]; !ok {
		t.Fatalf("the fixture has no candidate to protect: %+v", before)
	}

	if err := s.ReleaseWriter(t.Context(), next, nextFence); err != nil {
		t.Fatalf("ReleaseWriter: %v", err)
	}
	after, _, err := s.load(t.Context(), one.Key)
	if err != nil {
		t.Fatalf("load state after the release: %v", err)
	}
	before.Writer = nil
	want, err := json.Marshal(before)
	if err != nil {
		t.Fatalf("marshal expected state: %v", err)
	}
	got, err := json.Marshal(after)
	if err != nil {
		t.Fatalf("marshal actual state: %v", err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("a release changed more than the writer record:\nwant %s\ngot  %s", want, got)
	}
}

func TestAReleaseNeedsTheLeaseThatWasIssued(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 3, 2, 0, 55, 0, time.UTC)
	s, _, _ := testStore(&now)
	lease, fence, err := s.AcquireWriter(t.Context(), "acme/api/npm", "job-1", time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	if err := s.ReleaseWriter(t.Context(), storecontract.WriterLease{Key: lease.Key, Expires: lease.Expires}, fence); err == nil {
		t.Fatal("a release without a lease identity was accepted")
	}
	if err := s.ReleaseWriter(t.Context(), lease, 0); err == nil {
		t.Fatal("a release under fence zero was accepted")
	}
	state, _, err := s.load(t.Context(), "acme/api/npm")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state.Writer == nil || state.Writer.Lease.ID != lease.ID {
		t.Fatalf("a refused release cleared the writer: %+v", state.Writer)
	}
}

// The refusal for a live writer says who holds the key and until when — what
// the waiting writer announces — and is still ErrConflict to anyone asking
// only that.
func TestAHeldWriterRefusalNamesTheHolderAndItsExpiry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 3, 2, 0, 55, 0, time.UTC)
	s, _, _ := testStore(&now)
	first, _, err := s.AcquireWriter(t.Context(), "acme/api/npm", "i-0438d35e9edde2765", 15*time.Minute)
	if err != nil {
		t.Fatalf("AcquireWriter: %v", err)
	}
	_, _, err = s.AcquireWriter(t.Context(), "acme/api/npm", "i-0288803c710732024", 15*time.Minute)
	held, ok := errors.AsType[*storecontract.WriterHeldError](err)
	if !ok {
		t.Fatalf("a live writer was refused with %v, want a WriterHeldError", err)
	}
	if held.Key != "acme/api/npm" || held.Holder != "i-0438d35e9edde2765" || !held.Expires.Equal(first.Expires) {
		t.Fatalf("held = %+v, want the first writer's holder and expiry %s", held, first.Expires)
	}
	if !errors.Is(err, storecontract.ErrConflict) {
		t.Fatalf("a held writer is no longer a conflict: %v", err)
	}
	if !strings.Contains(err.Error(), "i-0438d35e9edde2765") ||
		!strings.Contains(err.Error(), first.Expires.UTC().Format(time.RFC3339)) {
		t.Fatalf("the refusal does not name the holder and expiry: %v", err)
	}
}
