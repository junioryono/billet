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

// WHAT THE PLANE ACCEPTS, IT READ IN FULL AND CAN SAY AGAIN. For any body a
// route's strict decoder accepts: an ordinary decode of the same bytes gives
// the same value, so the strict decoder neither lost nor invented anything; the
// value's encoding is accepted by the same decoder and encodes identically
// again, so nothing decodes to a value the wire cannot carry; and that
// encoding with an unknown field added, or read under a limit one byte short,
// is refused, so the decoder is still the strict one. Seeded with every golden
// wire fixture of every request type, and with bodies that carry something
// after their value.
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

		f.Add(uint8(i), []byte("{}\n"))
		f.Add(uint8(i), []byte("{}{}"))
		f.Add(uint8(i), []byte("{} x"))
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

		// AN ORDINARY DECODE OF THE SAME BYTES, the reference: it refuses
		// anything after the value, and keeps every field it knows.
		reference := r.fresh()
		if err := json.Unmarshal(body, reference); err != nil {
			t.Fatalf("%s accepted %q, which an ordinary decode refuses: %v", r.name, body, err)
		}

		if want, err := json.Marshal(reference); err != nil || !bytes.Equal(once, want) {
			t.Fatalf("%s read %q as %s, and an ordinary decode as %s (%v)", r.name, body, once, want, err)
		}

		// STILL THE STRICT DECODER: an unknown field, and a limit one byte
		// short of the body, are each refused.
		unknown := []byte(`{"billetFuzzUnknown":0}`)
		if len(once) > 2 {
			unknown = append([]byte(`{"billetFuzzUnknown":0,`), once[1:]...)
		}

		if strictly(t.Context(), unknown, r.fresh(), int64(len(unknown))+1) {
			t.Fatalf("%s accepted an unknown field: %s", r.name, unknown)
		}

		if strictly(t.Context(), once, r.fresh(), int64(len(once))-1) {
			t.Fatalf("%s accepted %s under a limit one byte short of it", r.name, once)
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
