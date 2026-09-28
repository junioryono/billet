package nodeclient_test

import (
	"net/http/httptest"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
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
