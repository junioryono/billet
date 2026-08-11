package scaleset

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/state"
)

// The whole path, over HTTP: a job GitHub OFFERS is claimed by its own request
// id, and the capacity billet escrowed is what it advertises.
//
// This is the test the project most needed and did not have. The worst defect so
// far was calling AcquireJobs with ids taken from JobAssigned rather than
// JobAvailable — billet's own types stayed self-consistent, every unit test
// passed, and the mistake lived entirely in the relationship between billet and
// the wire. Only something that speaks the wire can see it.
//
// So the assertions are on what left the process: the ids in the acquirejobs
// body, and the X-ScaleSetMaxCapacity header. Nothing here reads billet's
// internal state, because billet's internal state is what agreed with the bug.
func TestAnAvailableJobIsAcquiredByItsOwnRequestID(t *testing.T) {
	const (
		offered  = 4242 // arrives as JobAvailable, and is the id that must be claimed
		assigned = 9999 // arrives as JobAssigned, and must NOT be
	)

	var delivered atomic.Int32

	// Declared first: the handler needs the server's own URL to hand back as the
	// message queue address.
	var fake *fakeActions

	fake = newFakeActions(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "runnergroups"):
			writeJSON(t, w, listJSON(map[string]any{"id": 1, "name": "billet"}))

		case strings.Contains(r.URL.Path, "runnerscalesets") && strings.HasSuffix(r.URL.Path, "/sessions"):
			writeJSON(t, w, map[string]any{
				"sessionId":               "3a2b1c00-0000-4000-8000-000000000001",
				"ownerName":               "billet-test",
				"runnerScaleSet":          scaleSetJSON(1),
				"messageQueueUrl":         fake.URL + "/messages",
				"messageQueueAccessToken": "queue-token",
				"statistics":              map[string]any{},
			})

		// Checked BEFORE the poll case: acknowledging is DELETE /messages/{id},
		// which shares the prefix and would otherwise be answered as a poll.
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)

		case strings.HasPrefix(r.URL.Path, "/messages"):
			// One batch, then nothing. 202 is how the queue says "no message".
			if delivered.Add(1) > 1 {
				w.WriteHeader(http.StatusAccepted)

				return
			}

			writeJSON(t, w, jobMessages(t, 1,
				jobMessage("JobAvailable", offered),
				jobMessage("JobAssigned", assigned),
			))

		case strings.HasSuffix(r.URL.Path, "/acquirejobs"):
			// Grant whatever was asked for; the assertion is on what was ASKED.
			var ids []int64
			if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
				t.Errorf("decode acquirejobs body: %v", err)
			}

			writeJSON(t, w, map[string]any{"count": len(ids), "value": ids})

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "runnerscalesets"):
			writeJSON(t, w, listJSON(scaleSetJSON(1, "billet-4vcpu")))

		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	client, err := New(fake.config(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tiers := []config.Tier{{
		Label: "billet-4vcpu", Provider: config.ProviderFirecracker, GuestOS: config.GuestLinux,
		VCPU: 4, Memory: 16 * config.GiB, Image: "ubuntu-2404-x64",
	}}

	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	sess, err := client.Session(t.Context(), 1, "billet-test")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	// Stop once the batch has been handled and one more poll has come back empty,
	// so the acquisition has definitely been made.
	go func() {
		for ctx.Err() == nil {
			if delivered.Load() >= 2 {
				cancel()

				return
			}

			time.Sleep(10 * time.Millisecond)
		}
	}()

	l := server.NewListener(a, "billet-4vcpu", sess,
		// This test cancels while a job is running, and a cancel now begins a
		// DRAIN: the listener waits for a completion this fake GitHub never
		// sends. That is the drain working — the job genuinely has not finished —
		// so a test about the acquisition path says here that it is not testing
		// the drain, rather than the drain being shortened to suit it.
		server.WithDrainGrace(50*time.Millisecond))
	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	// The OUTER deadline is a failure, not an ending. A listener that wedges after
	// one acquisition would satisfy every assertion below and then sit until the
	// timeout, so accepting DeadlineExceeded let a broken run pass.
	if delivered.Load() < 2 {
		t.Fatalf("only %d polls completed; the run never got past the first batch, so the "+
			"assertions below describe a listener that stopped rather than one that worked",
			delivered.Load())
	}

	acquisitions := fake.calls("/acquirejobs")
	if len(acquisitions) == 0 {
		t.Fatal("billet never claimed the offered job; a scale set that advertises capacity " +
			"and acquires nothing looks exactly like GitHub never sending work")
	}

	var claimed []int64

	for _, c := range acquisitions {
		var ids []int64
		if err := json.Unmarshal([]byte(c.Body), &ids); err != nil {
			t.Fatalf("acquirejobs body is not a json array (%v): %s", err, c.Body)
		}

		claimed = append(claimed, ids...)
	}

	for _, id := range claimed {
		if id == assigned {
			t.Errorf("billet asked GitHub to claim request %d, which arrived as JobAssigned. "+
				"Assigned is the CONFIRMATION that a claim succeeded; acquiring from it asks "+
				"GitHub to claim work it has already handed over, and drops every real offer",
				assigned)
		}
	}

	if len(claimed) != 1 || claimed[0] != offered {
		t.Errorf("claimed %v, want exactly [%d] — the id that arrived as JobAvailable",
			claimed, offered)
	}
}

// The capacity billet escrowed is the number it puts on the wire.
//
// maxCapacity travels as a header rather than in the body, so it is invisible to
// any test that only inspects billet's types — and it is the single value GitHub
// uses to decide how much work to send.
func TestAdvertisedCapacityReachesTheWireAsAHeader(t *testing.T) {
	var (
		advertised atomic.Int64
		polls      atomic.Int32
		fake       *fakeActions
	)

	fake = newFakeActions(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "runnergroups"):
			writeJSON(t, w, listJSON(map[string]any{"id": 1, "name": "billet"}))

		case strings.HasSuffix(r.URL.Path, "/sessions"):
			writeJSON(t, w, map[string]any{
				"sessionId":               "3a2b1c00-0000-4000-8000-000000000002",
				"ownerName":               "billet-test",
				"runnerScaleSet":          scaleSetJSON(1),
				"messageQueueUrl":         fake.URL + "/messages",
				"messageQueueAccessToken": "queue-token",
				"statistics":              map[string]any{},
			})

		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)

		case strings.HasPrefix(r.URL.Path, "/messages"):
			// The LITERAL name, deliberately, not the dependency constant that
			// also writes it. Reading through the same constant means a rename
			// upstream moves both sides together and the test keeps passing while
			// the wire contract has changed underneath it.
			if n, err := strconv.ParseInt(r.Header.Get("X-Scalesetmaxcapacity"), 10, 64); err == nil {
				advertised.Store(n)
			}

			polls.Add(1)
			w.WriteHeader(http.StatusAccepted)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	client, err := New(fake.config(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Budget for exactly two runners of this tier.
	tiers := []config.Tier{{
		Label: "billet-4vcpu", Provider: config.ProviderFirecracker, GuestOS: config.GuestLinux,
		VCPU: 4, Memory: 16 * config.GiB, Image: "ubuntu-2404-x64",
	}}

	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	sess, err := client.Session(t.Context(), 1, "billet-test")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	go func() {
		for ctx.Err() == nil {
			if polls.Load() >= 2 {
				cancel()

				return
			}

			time.Sleep(10 * time.Millisecond)
		}
	}()

	l := server.NewListener(a, "billet-4vcpu", sess,
		// This test cancels while a job is running, and a cancel now begins a
		// DRAIN: the listener waits for a completion this fake GitHub never
		// sends. That is the drain working — the job genuinely has not finished —
		// so a test about the acquisition path says here that it is not testing
		// the drain, rather than the drain being shortened to suit it.
		server.WithDrainGrace(50*time.Millisecond))
	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	if polls.Load() < 2 {
		t.Fatalf("only %d polls completed; the listener wedged rather than advertising "+
			"repeatedly", polls.Load())
	}

	if got := advertised.Load(); got != 2 {
		t.Errorf("advertised %d runners; the budget is 8 vCPU and the tier is 4, so the "+
			"escrow is 2 and that is what GitHub must be told", got)
	}
}

// jobMessage builds one batched job message of the given type.
func jobMessage(messageType string, requestID int64) map[string]any {
	return map[string]any{
		"messageType":     messageType,
		"runnerRequestId": requestID,
		"repositoryName":  "platform",
		"ownerName":       "acme",
		"jobId":           "job-" + strconv.FormatInt(requestID, 10),
		"workflowRunId":   1000 + requestID,
		"acquireJobUrl":   "https://example.invalid/acquire",
		"requestLabels":   []string{"billet-4vcpu"},
	}
}

// jobMessages wraps batched job messages the way the queue returns them: the
// batch is a JSON STRING in `body`, not a nested array.
func jobMessages(t *testing.T, messageID int, msgs ...map[string]any) map[string]any {
	t.Helper()

	body, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal batched messages: %v", err)
	}

	return map[string]any{
		"messageId":   messageID,
		"messageType": "RunnerScaleSetJobMessages",
		"body":        string(body),
		"statistics":  map[string]any{},
	}
}
