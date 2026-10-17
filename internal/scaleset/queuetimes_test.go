package scaleset

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/fakeactions"
)

// GITHUB'S TIMES SURVIVE THE WIRE: the vendored client decodes them from the
// JSON a message carries, under the names it declares, and translate hands
// them to billet. A unit test of translate alone builds the vendor's struct by
// hand and would agree with a misspelt field.
func TestAMessagesQueueTimesReachBilletOverTheWire(t *testing.T) {
	queued := time.Date(2026, 10, 16, 7, 43, 47, 0, time.UTC)

	var fake *fakeActions

	fake = newFakeActions(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "runnerscalesets") && strings.HasSuffix(r.URL.Path, "/sessions"):
			writeJSON(t, w, map[string]any{
				"sessionId":               "3a2b1c00-0000-4000-8000-000000000002",
				"ownerName":               "billet-test",
				"runnerScaleSet":          scaleSetJSON(1),
				"messageQueueUrl":         fake.URL + "/messages",
				"messageQueueAccessToken": "queue-token",
				"statistics":              map[string]any{},
			})
		case strings.HasPrefix(r.URL.Path, "/messages"):
			started := fakeactions.JobFields{RunnerID: 7, RunnerName: "billet-lease",
				QueuedAt: queued, ScaleSetAssignedAt: queued.Add(10 * time.Second),
				RunnerAssignedAt: queued.Add(52 * time.Minute)}.Apply(fakeactions.JobJSON("JobStarted", 11, "push"))
			writeJSON(t, w, jobMessages(t, 1, started))
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	client, err := New(fake.config(t), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sess, err := client.Session(t.Context(), 1, "billet-test")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	msg, err := sess.GetMessage(ctx, 0, 1)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if len(msg.Started) != 1 {
		t.Fatalf("started = %+v, want the one JobStarted", msg.Started)
	}

	j := msg.Started[0]
	if !j.QueuedAt.Equal(queued) || !j.ScaleSetAssignedAt.Equal(queued.Add(10*time.Second)) ||
		!j.RunnerAssignedAt.Equal(queued.Add(52*time.Minute)) {
		t.Errorf("the times arrived as %v, %v, %v; want 07:43:47, ten seconds later and 52 minutes later",
			j.QueuedAt, j.ScaleSetAssignedAt, j.RunnerAssignedAt)
	}
}
