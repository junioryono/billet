package nodeplane_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
)

func aUsage() alloc.JobUsage {
	cycles, instructions, stalls := int64(4_000_000_000), int64(4_840_000_000), int64(0)
	return alloc.JobUsage{
		Counters: &alloc.JobCounters{Cycles: &cycles, Instructions: &instructions,
			FrontendStallCycles: &stalls},
		Source: alloc.UsageSourceHost, Unmeasured: []string{alloc.UsageIO},
		Samples: 1200, IntervalMillis: 1000, WindowMillis: 1_200_000,
		CPUUserMicros: 3_600_000_000, CPUSystemMicros: 400_000_000,
		GuestCPUMicros: 3_900_000_000, VMMCPUMicros: 100_000_000,
		MemoryPeakBytes: 35_500_000_000, NetRxBytes: 1 << 30, NetTxBytes: 1 << 20,
		CPUSomeMicros: 5_000, EnergyActiveMicrojoules: 90_000_000_000,
		EnergyIdleMicrojoules: 4_000_000_000, EnergySource: alloc.EnergyRAPL,
	}
}

// A USAGE REPORT CROSSES THE WIRE WITH ITS EPOCH AND ITS SERIES, so the ledger
// fences it like every other write from the process holding the lease.
func TestAUsageReportCrossesTheNodeWire(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	_, base := serve(t, store)
	c := dial(t, base)

	usage := aUsage()
	series := &alloc.UsageSeries{Codec: alloc.UsageSeriesCodec, Data: []byte{1, 2, 3, 0, 255}}
	if err := c.RecordLeaseUsage(t.Context(), "l1", 7, usage, series); err != nil {
		t.Fatalf("RecordLeaseUsage: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.usages) != 1 {
		t.Fatalf("the ledger was asked to record %d usage reports, want 1", len(store.usages))
	}
	got := store.usages[0]
	if got.lease != "l1" || got.epoch != 7 || !reflect.DeepEqual(got.usage, usage) ||
		got.series == nil || !reflect.DeepEqual(*got.series, *series) {
		t.Fatalf("the ledger was asked to record %+v, want %+v with %+v for l1 at epoch 7",
			got, usage, series)
	}
}

// A REPORT THIS CONTROL PLANE CANNOT KEEP FAITHFULLY IS REFUSED AT THE
// BOUNDARY and never reaches the ledger, as a permanent refusal.
//
// EVERY CASE IS THE ONE VALID REPORT WITH ONE THING BROKEN, and says which
// refusal it expects, so a case cannot pass on a failure it was not written
// for.
func TestAnUnkeepableUsageReportIsRefusedAtTheWire(t *testing.T) {
	t.Parallel()

	const valid = `"source":"host","samples":1,"interval_ms":1000,"window_ms":0,"unmeasured":["energy"]`
	accepted := `{"epoch":7,"usage":{` + valid + `},"series":{"codec":1,"data":"AQ=="}}`
	for name, tc := range map[string]struct{ body, want string }{
		"an unknown source": {strings.Replace(accepted, `"source":"host"`, `"source":"guest"`, 1),
			"not one this control plane records"},
		"a negative quantity": {strings.Replace(accepted, `"window_ms":0`, `"window_ms":0,"cpu_user_us":-1`, 1),
			"cpu_user_us is negative"},
		"an unknown codec": {strings.Replace(accepted, `"codec":1`, `"codec":9`, 1),
			"codec 9"},
		"an unknown usage group": {strings.Replace(accepted, `["energy"]`, `["energy","gpu"]`, 1),
			`"gpu" is not a usage group`},
		"energy claimed and none": {strings.Replace(accepted, `"window_ms":0`, `"window_ms":0,"energy_active_uj":5`, 1),
			"names energy unmeasured"},
		"counters that count nothing": {strings.Replace(accepted, `"window_ms":0`, `"window_ms":0,"counters":{}`, 1),
			"count nothing"},
		"a negative counter": {strings.Replace(accepted, `"window_ms":0`,
			`"window_ms":0,"counters":{"cycles":5,"cache_misses":-2}`, 1), "cache_misses is negative"},
	} {
		body := tc.body
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			store := &fakeStore{}
			_, base := serve(t, store)
			c := dial(t, base)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				base+"/v1/nodes/n1/leases/l1/usage", strings.NewReader(body))
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			req.Header.Set(nodeapi.HeaderIncarnation, c.Incarnation())

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()

			var refusal nodeapi.ErrorResponse
			if err := json.NewDecoder(resp.Body).Decode(&refusal); err != nil {
				t.Fatalf("decode the refusal: %v", err)
			}
			if resp.StatusCode != http.StatusBadRequest || refusal.Code != nodeapi.CodeRefused ||
				!strings.Contains(refusal.Message, tc.want) {
				t.Fatalf("answered %d %q (%s), want 400 %q saying %q", resp.StatusCode, refusal.Code,
					refusal.Message, nodeapi.CodeRefused, tc.want)
			}

			store.mu.Lock()
			defer store.mu.Unlock()
			if len(store.usages) != 0 {
				t.Fatalf("the ledger was asked to record %+v from a refused request", store.usages)
			}
		})
	}
}

// The valid report every refusal case starts from is itself accepted, or the
// cases above could all be refusing it for the same unrelated reason.
func TestTheBaseUsageReportIsAccepted(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	_, base := serve(t, store)
	c := dial(t, base)
	usage := alloc.JobUsage{Source: alloc.UsageSourceHost, Samples: 1, IntervalMillis: 1000,
		Unmeasured: []string{alloc.UsageEnergy}}
	if err := c.RecordLeaseUsage(t.Context(), "l1", 7, usage,
		&alloc.UsageSeries{Codec: alloc.UsageSeriesCodec, Data: []byte{1}}); err != nil {
		t.Fatalf("the base report was refused: %v", err)
	}
}

// A REPORT FROM A PAIRING THAT NEGOTIATED BELOW THE COUNTERS' VERSION IS KEPT
// WITHOUT THEM: that wire does not carry them, so the plane records what it
// does carry rather than refusing the report or keeping what it should not
// have been sent.
func TestCountersFromAWireThatDoesNotCarryThemAreDropped(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		version  int
		counters bool
	}{
		{nodeapi.VersionJobCounters - 1, false},
		{nodeapi.VersionJobCounters, true},
	} {
		store := &fakeStore{}
		_, base := serve(t, store)
		incarnation := fmt.Sprintf("wire-%d", tc.version)
		post := func(path string, body any) int {
			t.Helper()
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+path, bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			req.Header.Set(nodeapi.HeaderIncarnation, incarnation)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("post %s: %v", path, err)
			}
			defer resp.Body.Close()

			return resp.StatusCode
		}

		if status := post("/v1/register", nodeapi.RegisterRequest{
			MinVersion: nodeapi.MinVersion, Version: tc.version, Incarnation: incarnation,
			Node: "n1", Provider: config.ProviderDocker, GuestOS: []config.GuestOS{config.GuestLinux},
			Deployment: deployment, VCPU: testNodeVCPU, Memory: testNodeMemory,
		}); status != http.StatusOK {
			t.Fatalf("register at wire %d answered %d", tc.version, status)
		}
		usage := aUsage()
		if status := post("/v1/nodes/n1/leases/l1/usage",
			nodeapi.UsageRequest{Epoch: 7, Usage: usage}); status != http.StatusNoContent {
			t.Fatalf("a usage report at wire %d answered %d", tc.version, status)
		}

		store.mu.Lock()
		got := store.usages
		store.mu.Unlock()
		if len(got) != 1 {
			t.Fatalf("at wire %d the ledger was asked to record %d reports, want 1", tc.version, len(got))
		}
		if kept := got[0].usage.Counters != nil; kept != tc.counters {
			t.Errorf("at wire %d the ledger kept counters %v, want %v", tc.version, kept, tc.counters)
		}
		usage.Counters = got[0].usage.Counters
		if !reflect.DeepEqual(got[0].usage, usage) {
			t.Errorf("at wire %d the rest of the report changed: %+v", tc.version, got[0].usage)
		}
	}
}
