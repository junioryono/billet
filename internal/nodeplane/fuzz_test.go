package nodeplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/junioryono/billet/internal/nodeapi"
)

// planeRequests are the bodies the plane decodes strictly, by route, with the
// limit each route reads under.
var planeRequests = []struct {
	name  string
	limit int64
	fresh func() any
}{
	{"EnrollRequest", maxEnrollBody, func() any { return new(nodeapi.EnrollRequest) }},
	{"RenewRequest", maxBody, func() any { return new(nodeapi.RenewRequest) }},
	{"RegisterRequest", maxBody, func() any { return new(nodeapi.RegisterRequest) }},
	{"BindRequest", maxBody, func() any { return new(nodeapi.BindRequest) }},
	{"AdvanceRequest", maxBody, func() any { return new(nodeapi.AdvanceRequest) }},
	{"HeartbeatRequest", maxBody, func() any { return new(nodeapi.HeartbeatRequest) }},
	{"MarkFailureRequest", maxBody, func() any { return new(nodeapi.MarkFailureRequest) }},
	{"ReleaseRequest", maxBody, func() any { return new(nodeapi.ReleaseRequest) }},
	{"CacheObservationRequest", maxBody, func() any { return new(nodeapi.CacheObservationRequest) }},
	{"UsageRequest", maxBody, func() any { return new(nodeapi.UsageRequest) }},
	{"ResizeRequest", maxBody, func() any { return new(nodeapi.ResizeRequest) }},
	{"ReconcileRequest", maxBody, func() any { return new(nodeapi.ReconcileRequest) }},
	{"WithdrawRequest", maxBody, func() any { return new(nodeapi.WithdrawRequest) }},
	{"DescribeRequest", maxBody, func() any { return new(nodeapi.DescribeRequest) }},
	{"JITRequest", maxBody, func() any { return new(nodeapi.JITRequest) }},
	{"RemoveRunnerRequest", maxBody, func() any { return new(nodeapi.RemoveRunnerRequest) }},
	{"RecoverRunnerRequest", maxBody, func() any { return new(nodeapi.RecoverRunnerRequest) }},
	{"TrustedRunnerGroupRequest", maxBody, func() any { return new(nodeapi.TrustedRunnerGroupRequest) }},
}

// strictly decodes body into into the way a route does, through the plane's own
// decoder, and reports whether it was accepted.
func strictly(ctx context.Context, body []byte, into any, limit int64) bool {
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/", bytes.NewReader(body))

	return decodeLimited(httptest.NewRecorder(), req, into, limit)
}

// WHAT THE PLANE ACCEPTS, IT CAN SAY AGAIN. Any body a route's strict decoder
// accepts is a value whose encoding the same decoder accepts, and encoding that
// second value gives the same bytes: no input decodes to a value the wire
// cannot carry, and nothing is lost or invented on the way through. Seeded with
// every golden wire fixture of every request type.
func FuzzPlaneDecode(f *testing.F) {
	versions, err := filepath.Glob(filepath.Join("..", "nodeapi", "testdata", "wire", "v*"))
	if err != nil || len(versions) == 0 {
		f.Fatalf("no golden wire fixtures to seed from (%v)", err)
	}

	for i, r := range planeRequests {
		seeded := 0

		for _, dir := range versions {
			fixtures, err := filepath.Glob(filepath.Join(dir, r.name+"*.json"))
			if err != nil {
				f.Fatal(err)
			}

			for _, path := range fixtures {
				body, err := os.ReadFile(path)
				if err != nil {
					f.Fatal(err)
				}

				f.Add(uint8(i), body)

				seeded++
			}
		}

		if seeded == 0 {
			f.Fatalf("no golden fixture seeds %s", r.name)
		}
	}

	f.Fuzz(func(t *testing.T, kind uint8, body []byte) {
		r := planeRequests[int(kind)%len(planeRequests)]

		first := r.fresh()
		if !strictly(t.Context(), body, first, r.limit) {
			return
		}

		once, err := json.Marshal(first)
		if err != nil {
			t.Fatalf("%s accepted %q and could not encode the value: %v", r.name, body, err)
		}

		// THE DECODER'S RULES WITHOUT THE ROUTE'S LIMIT: an encoding may be
		// longer than the body it came from (json escapes <, > and &).
		second := r.fresh()
		if !strictly(t.Context(), once, second, int64(len(once))+1) {
			t.Fatalf("%s accepted %q but refused its own encoding of the value, %s", r.name, body, once)
		}

		twice, err := json.Marshal(second)
		if err != nil {
			t.Fatal(err)
		}

		if !bytes.Equal(once, twice) {
			t.Fatalf("%s does not encode the same value twice the same way:\n%s\n%s", r.name, once, twice)
		}
	})
}
