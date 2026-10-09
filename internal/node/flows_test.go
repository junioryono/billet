package node

import (
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/usage/flows"
)

// recordingFlows is a FlowWatcher that records what it was asked.
type recordingFlows struct {
	mu        sync.Mutex
	watched   map[string]string // key -> lease file
	since     map[string]time.Time
	finals    []string
	forgotten []string
}

func newRecordingFlows() *recordingFlows {
	return &recordingFlows{watched: map[string]string{}, since: map[string]time.Time{}}
}

func (f *recordingFlows) Watch(key string, _ net.HardwareAddr, leaseFile string, since time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.watched[key], f.since[key] = leaseFile, since
}

func (f *recordingFlows) Final(key string) (flows.Result, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.finals = append(f.finals, key)

	return flows.Result{}, true, nil
}

func (f *recordingFlows) Forget(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.forgotten = append(f.forgotten, key)
}

// A GUEST'S FLOWS ARE FOLLOWED FROM THE LEASE FILE OF THE BRIDGE ITS TAP IS ON,
// from a moment before the launch, and an unusable MAC or bridge follows
// nothing rather than another guest's lease.
func TestStartFlowsFollowsTheGuestsOwnLease(t *testing.T) {
	t.Parallel()

	launched := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name   string
		target provider.UsageTarget
		want   string
	}{
		{"a guest on billet1", provider.UsageTarget{GuestMAC: "02:00:00:00:00:17", Bridge: "billet1"}, "/leases/billet1/dnsmasq.leases"},
		{"no MAC", provider.UsageTarget{Bridge: "billet1"}, ""},
		{"a MAC that does not parse", provider.UsageTarget{GuestMAC: "nope", Bridge: "billet1"}, ""},
		{"no bridge", provider.UsageTarget{GuestMAC: "02:00:00:00:00:17"}, ""},
		{"a bridge that is a path", provider.UsageTarget{GuestMAC: "02:00:00:00:00:17", Bridge: "../etc"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := newRecordingFlows()
			r := &Runner{log: slog.New(slog.DiscardHandler), flows: rec, leaseDir: "/leases"}

			r.startFlows("billet-1", tc.target, launched)

			got, watched := rec.watched["billet-1"]
			switch {
			case tc.want == "" && watched:
				t.Errorf("followed %q for a target with no usable lease", got)
			case tc.want != "" && got != tc.want:
				t.Errorf("followed %q, want %q", got, tc.want)
			case tc.want != "" && !rec.since["billet-1"].Equal(launched):
				t.Errorf("followed from %v, want the launch time %v", rec.since["billet-1"], launched)
			}
		})
	}
}

// WITHOUT FLOWS NOTHING IS FOLLOWED, and forgetting a job forgets its flows.
func TestFlowsAreOptionalAndForgottenWithTheJob(t *testing.T) {
	t.Parallel()

	r := &Runner{log: slog.New(slog.DiscardHandler)}
	r.startFlows("billet-1", provider.UsageTarget{GuestMAC: "02:00:00:00:00:17", Bridge: "billet1"}, time.Now())
	r.finalFlows("billet-1")
	r.forgetMonitoring("billet-1")

	rec := newRecordingFlows()
	r = &Runner{log: slog.New(slog.DiscardHandler), flows: rec, leaseDir: "/leases"}
	r.forgetMonitoring("billet-2")
	r.finalFlows("billet-3")

	if len(rec.forgotten) != 1 || rec.forgotten[0] != "billet-2" {
		t.Errorf("forgotten = %v, want billet-2", rec.forgotten)
	}

	if len(rec.finals) != 1 || rec.finals[0] != "billet-3" {
		t.Errorf("finals = %v, want billet-3", rec.finals)
	}
}
