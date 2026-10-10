package nodeclient_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/usage"
)

// A NODE NEGOTIATED BELOW THE USAGE VERSION SENDS NOTHING, and the call is
// success: the route does not exist on that plane and what is lost is the
// measurement, never a job. At the version, the report is sent.
func TestAUsageReportIsSentOnlyToAPlaneThatHasTheRoute(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		version int
		sent    int64
	}{
		{nodeapi.VersionJobUsage - 1, 0},
		{nodeapi.VersionJobUsage, 1},
	} {
		plane := &versionedPlane{version: tc.version}
		srv := httptest.NewServer(plane)
		t.Cleanup(srv.Close)

		c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		if err := c.Register(t.Context(), testRegistration()); err != nil {
			t.Fatalf("register: %v", err)
		}

		err = c.RecordLeaseUsage(t.Context(), "l1", 1,
			alloc.JobUsage{Source: alloc.UsageSourceHost, Samples: 1, IntervalMillis: 1000}, nil)
		if tc.sent == 0 && err != nil {
			t.Fatalf("RecordLeaseUsage against a wire-%d plane = %v, want nil", tc.version, err)
		}

		if n := plane.others.Load(); n != tc.sent {
			t.Errorf("on wire %d the node sent %d request(s), want %d", tc.version, n, tc.sent)
		}
	}
}

// usageRecordingPlane registers at version and keeps the last usage report it was
// sent.
type usageRecordingPlane struct {
	version int
	mu      sync.Mutex
	got     *nodeapi.UsageRequest
	raw     string
}

func (p *usageRecordingPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v1/register" {
		if err := json.NewEncoder(w).Encode(nodeapi.RegisterResponse{
			Version: p.version, LeaseTTLSeconds: 60, PollSeconds: 30,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req nodeapi.UsageRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	p.got, p.raw = &req, string(body)
	p.mu.Unlock()
	if _, err := w.Write([]byte("{}")); err != nil {
		return
	}
}

// A PLANE TOO OLD FOR A PROCESS'S USAGE IS TOLD LESS, NEVER SOMETHING ELSE:
// process energy becomes unmeasured, and memory with no OOM count becomes
// unmeasured whole, so what it validates is true. A plane at the version is
// told everything.
func TestAProcessUsageReportSaysOnlyWhatAnOlderPlaneCanKeep(t *testing.T) {
	t.Parallel()

	// SPARE CAPACITY, so a downgrade that edits the caller's slice in place
	// rather than a copy shows up in the caller's report.
	unmeasured := append(make([]string, 0, 8), alloc.UsageNet, alloc.UsageOOM)
	report := alloc.JobUsage{
		Source: alloc.UsageSourceHost, Unmeasured: unmeasured,
		Samples: 10, IntervalMillis: 1000, WindowMillis: 10_000,
		CPUUserMicros: 5, MemoryPeakBytes: 25 << 30, DiskReadBytes: 7,
		EnergyActiveMicrojoules: 9, EnergySource: alloc.EnergyProcess,
	}
	want := report
	want.Unmeasured = []string{alloc.UsageNet, alloc.UsageOOM}
	older := report
	older.Unmeasured = []string{alloc.UsageEnergy, alloc.UsageMemory, alloc.UsageNet}
	older.MemoryPeakBytes, older.EnergyActiveMicrojoules, older.EnergySource = 0, 0, ""

	for _, tc := range []struct {
		version int
		want    alloc.JobUsage
	}{
		{nodeapi.VersionProcessUsage - 1, older},
		{nodeapi.VersionProcessUsage, want},
	} {
		plane := &usageRecordingPlane{version: tc.version}
		srv := httptest.NewServer(plane)
		t.Cleanup(srv.Close)
		c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		if err := c.Register(t.Context(), testRegistration()); err != nil {
			t.Fatalf("register: %v", err)
		}
		if err := c.RecordLeaseUsage(t.Context(), "l1", 1, report, nil); err != nil {
			t.Fatalf("RecordLeaseUsage on wire %d: %v", tc.version, err)
		}
		plane.mu.Lock()
		got := plane.got
		plane.mu.Unlock()
		if got == nil {
			t.Fatalf("nothing reached the wire-%d plane", tc.version)
		}
		if !reflect.DeepEqual(got.Usage, tc.want) {
			t.Errorf("a wire-%d plane was told %+v,\nwant %+v", tc.version, got.Usage, tc.want)
		}
		if err := got.Usage.Validate(); err != nil {
			t.Errorf("what a wire-%d plane was told does not validate: %v", tc.version, err)
		}
	}
	if !slices.Equal(report.Unmeasured, want.Unmeasured) || report.MemoryPeakBytes == 0 {
		t.Error("the downgrade changed the caller's report")
	}
}

// A PLANE TOO OLD FOR HARDWARE COUNTERS IS SENT THE REST OF THE REPORT: its
// strict decoder would refuse the whole body for the one key it does not know,
// so below the version the key is not in the body at all. A plane at the
// version is sent the counters as they were measured.
func TestHardwareCountersReachOnlyAPlaneThatKnowsThem(t *testing.T) {
	t.Parallel()

	cycles, instructions := int64(4_000_000), int64(4_840_000)
	report := alloc.JobUsage{
		Source: alloc.UsageSourceHost, Samples: 10, IntervalMillis: 1000, WindowMillis: 10_000,
		Unmeasured: []string{alloc.UsageEnergy},
		Counters:   &alloc.JobCounters{Cycles: &cycles, Instructions: &instructions},
	}
	for _, tc := range []struct {
		version  int
		counters bool
	}{
		{nodeapi.VersionJobCounters - 1, false},
		{nodeapi.VersionJobCounters, true},
	} {
		plane := &usageRecordingPlane{version: tc.version}
		srv := httptest.NewServer(plane)
		t.Cleanup(srv.Close)
		c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		if err := c.Register(t.Context(), testRegistration()); err != nil {
			t.Fatalf("register: %v", err)
		}
		if err := c.RecordLeaseUsage(t.Context(), "l1", 1, report, nil); err != nil {
			t.Fatalf("RecordLeaseUsage on wire %d: %v", tc.version, err)
		}
		plane.mu.Lock()
		got, raw := plane.got, plane.raw
		plane.mu.Unlock()
		if got == nil {
			t.Fatalf("nothing reached the wire-%d plane", tc.version)
		}
		if sent := strings.Contains(raw, `"counters"`); sent != tc.counters {
			t.Errorf("a wire-%d plane was sent counters %v, want %v: %s", tc.version, sent, tc.counters, raw)
		}
		if tc.counters && !reflect.DeepEqual(got.Usage, report) {
			t.Errorf("a wire-%d plane was told %+v, want %+v", tc.version, got.Usage, report)
		}
		if !tc.counters && got.Usage.Samples != report.Samples {
			t.Errorf("a wire-%d plane lost the rest of the report: %+v", tc.version, got.Usage)
		}
	}
	if report.Counters == nil {
		t.Error("stripping the counters for an older plane took them from the caller's report")
	}
}

// A PLANE TOO OLD FOR THE CLOCKED SERIES IS SENT THE SAME POINTS WITHOUT THE
// CLOCK, in the codec it keeps, rather than a series its validation refuses
// and takes the report down with; a plane at the version gets the clocked
// series as it was. The caller's series is not changed either way.
func TestAClockedSeriesReachesOnlyAPlaneThatKeepsIt(t *testing.T) {
	t.Parallel()

	points := []usage.Point{{OffsetMillis: 0}, {OffsetMillis: 1000, CPUUsage: 5, AfterGap: true}}
	data, _, err := usage.EncodeSeriesAt(points, time.Now(), alloc.MaxUsageSeriesBytes)
	if err != nil {
		t.Fatalf("EncodeSeriesAt: %v", err)
	}
	clocked := &alloc.UsageSeries{Codec: usage.SeriesCodecClocked, Data: slices.Clone(data)}
	report := alloc.JobUsage{Source: alloc.UsageSourceHost, Samples: 2, IntervalMillis: 1000}

	for _, version := range []int{nodeapi.VersionSeriesClock - 1, nodeapi.VersionSeriesClock} {
		plane := &usageRecordingPlane{version: version}
		srv := httptest.NewServer(plane)
		t.Cleanup(srv.Close)
		c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
		if err != nil {
			t.Fatalf("new client: %v", err)
		}
		if err := c.Register(t.Context(), testRegistration()); err != nil {
			t.Fatalf("register: %v", err)
		}
		if err := c.RecordLeaseUsage(t.Context(), "l1", 1, report, clocked); err != nil {
			t.Fatalf("RecordLeaseUsage on wire %d: %v", version, err)
		}
		plane.mu.Lock()
		got := plane.got
		plane.mu.Unlock()
		if got == nil || got.Series == nil {
			t.Fatalf("no series reached the wire-%d plane", version)
		}
		if version == nodeapi.VersionSeriesClock {
			if got.Series.Codec != usage.SeriesCodecClocked || !bytes.Equal(got.Series.Data, data) {
				t.Errorf("a wire-%d plane was sent codec %d, not the clocked series", version, got.Series.Codec)
			}
			continue
		}
		if got.Series.Codec != usage.SeriesCodec {
			t.Fatalf("a wire-%d plane was sent codec %d, want %d", version, got.Series.Codec, usage.SeriesCodec)
		}
		decoded, err := usage.DecodeSeries(got.Series.Data)
		want := []usage.Point{{OffsetMillis: 0}, {OffsetMillis: 1000, CPUUsage: 5}}
		if err != nil || !slices.Equal(decoded, want) {
			t.Errorf("a wire-%d plane was sent %v (%v), want the same points without the clock", version,
				decoded, err)
		}
	}
	if clocked.Codec != usage.SeriesCodecClocked || !bytes.Equal(clocked.Data, data) {
		t.Error("the downgrade changed the caller's series")
	}
}
