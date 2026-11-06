package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// webhook is a destination on loopback that keeps every notice it is sent.
type webhook struct {
	mu      sync.Mutex
	notices []map[string]any
}

func (h *webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)

	var notice map[string]any
	if err == nil {
		err = json.Unmarshal(body, &notice)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if err != nil {
		notice = map[string]any{"unreadable": string(body)}
	}
	h.notices = append(h.notices, notice)
	w.WriteHeader(http.StatusNoContent)
}

func (h *webhook) received() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]map[string]any(nil), h.notices...)
}

// A FAILED JOB IS TOLD TO A WEBHOOK ONCE, AND A SECOND FAILURE INSIDE THE COOLDOWN IS
// NOT. Through the real listener, node, ledger and both alert loops, with the
// destination's address read from a file the way an operator writes one.
func TestAFailedJobIsToldToAWebhookOnceAndNotAgainInsideTheCooldown(t *testing.T) {
	receiver := &webhook{}
	destination := httptest.NewServer(receiver)
	t.Cleanup(destination.Close)

	addressFile := filepath.Join(t.TempDir(), "hook-url")
	if err := os.WriteFile(addressFile, []byte(destination.URL), 0o600); err != nil {
		t.Fatal(err)
	}

	s, _, _ := simulatedStack(t, overTheWire, withConfigure(func(cfg *config.Config) {
		cfg.Server.Alerts = &config.AlertsConfig{
			Destinations: []config.AlertDestination{{Name: "hook", Kind: config.AlertWebhook, URLFile: addressFile}},
			Rules: []config.AlertRule{{Name: "red", On: config.AlertConditions{Failed: true}, Cooldown: "1h",
				Notify: []string{"hook"}}},
		}
	}))

	// A NOTICE OWED TO A DESTINATION THE CONFIG NO LONGER NAMES, from before this start.
	if _, err := s.alloc.RecordAlertJudgement(t.Context(), &alloc.AlertJudgement{Rule: "old", LeaseID: "old",
		Condition: "failed", Key: `["failed","acme/old",".github/workflows/x.yml","x"]`, Outcome: alloc.AlertHit,
		InARow: 1, Notices: []alloc.AlertNotice{{Destination: "removed", Payload: []byte("{}")}}}); err != nil {
		t.Fatal(err)
	}

	stop := s.run(t)
	defer stop()

	first := runJobToItsEnd(t, s, redJob{request: 6201, run: 621, name: "deploy"})

	deadline := time.Now().Add(30 * time.Second)
	for len(receiver.received()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the webhook was never told about the failed job")
		}

		time.Sleep(50 * time.Millisecond)
	}

	notice := receiver.received()[0]
	if notice["billet_alert"] != float64(1) || notice["rule"] != "red" || notice["condition"] != "failed" ||
		notice["lease"] != first || notice["result"] != "failed" {
		t.Errorf("the notice = %v", notice)
	}

	// THE SAME JOB OF THE SAME WORKFLOW FAILS AGAIN, inside the hour: judged, counted,
	// and not sent.
	second := runJobToItsEnd(t, s, redJob{request: 6202, run: 622, name: "deploy"})

	deadline = time.Now().Add(30 * time.Second)
	for {
		report, err := s.alloc.AlertReport(t.Context(), 10)
		if err != nil {
			t.Fatal(err)
		}
		counted := false
		for _, streak := range report.Streaks {
			counted = counted || (streak.Rule == "red" && streak.Streak == 2)
		}
		if counted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the second failure (%s) was never judged: %+v", second, report.Streaks)
		}

		time.Sleep(50 * time.Millisecond)
	}

	// NOTHING IS OWED FOR IT: the notice would have been written in the transaction
	// that moved the streak, which the wait above saw.
	report, err := s.alloc.AlertReport(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range report.Notices {
		switch {
		case n.Destination == "hook" && n.State == "delivered" && n.Notices == 1:
		case n.Destination == "removed" && n.State == "failed" && n.Notices == 1:
		default:
			t.Errorf("the outbox holds %+v, want the one delivered notice and the removed one given up", report.Notices)
		}
	}
	if got := receiver.received(); len(got) != 1 {
		t.Errorf("the webhook was told %d times, want once: %v", len(got), got)
	}
}
