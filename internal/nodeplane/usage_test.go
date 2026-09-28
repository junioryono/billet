package nodeplane_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/nodeapi"
)

func aUsage() alloc.JobUsage {
	return alloc.JobUsage{
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
func TestAnUnkeepableUsageReportIsRefusedAtTheWire(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"an unknown source":       `{"epoch":7,"usage":{"source":"guest","samples":1,"interval_ms":1000,"window_ms":0}}`,
		"a negative quantity":     `{"epoch":7,"usage":{"source":"host","samples":1,"interval_ms":1000,"window_ms":0,"cpu_user_us":-1}}`,
		"an unknown codec":        `{"epoch":7,"usage":{"source":"host","samples":1,"interval_ms":1000,"window_ms":0},"series":{"codec":9,"data":"AQ=="}}`,
		"an unknown usage group":  `{"epoch":7,"usage":{"source":"host","samples":1,"interval_ms":1000,"window_ms":0,"unmeasured":["gpu"]}}`,
		"energy claimed and none": `{"epoch":7,"usage":{"source":"host","samples":1,"interval_ms":1000,"window_ms":0,"unmeasured":["energy"],"energy_active_uj":5}}`,
	} {
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
			if resp.StatusCode != http.StatusBadRequest || refusal.Code != nodeapi.CodeRefused {
				t.Fatalf("answered %d %q (%s), want 400 %q", resp.StatusCode, refusal.Code,
					refusal.Message, nodeapi.CodeRefused)
			}

			store.mu.Lock()
			defer store.mu.Unlock()
			if len(store.usages) != 0 {
				t.Fatalf("the ledger was asked to record %+v from a refused request", store.usages)
			}
		})
	}
}
