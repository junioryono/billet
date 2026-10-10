package lease

import (
	"strings"
	"testing"
	"time"
)

// aGuestReport is a report at every bound this control plane keeps: the
// largest Data, the longest version, the highest schema.
func aGuestReport() GuestReport {
	first := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	return GuestReport{
		AgentVersion: strings.Repeat("v", MaxGuestAgentVersion), Schema: MaxGuestSchema,
		Codec: GuestReportCodec, Data: make([]byte, MaxGuestReportBytes),
		Accepted: 40, Refused: 2, DroppedBytes: 9, Hello: true, FinalSeen: true,
		FirstReceived: first, LastReceived: first.Add(time.Minute),
	}
}

// A GUEST REPORT THE LEDGER COULD NOT KEEP FAITHFULLY IS REFUSED, each by the
// clause that names its fault; every case is the one valid report with one
// thing broken, and that report and a report of nothing are both kept.
func TestAGuestReportTheLedgerCannotKeepIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		mutate func(*GuestReport)
		want   string
	}{
		{"no codec", func(g *GuestReport) { g.Codec = 0 }, "codec 0 is not one"},
		{"an unknown codec", func(g *GuestReport) { g.Codec = GuestReportCodec + 1 }, "codec 2 is not one"},
		{"data over its bound", func(g *GuestReport) { g.Data = make([]byte, MaxGuestReportBytes+1) },
			"over the 262144 byte bound"},
		{"a version over its bound", func(g *GuestReport) {
			g.AgentVersion = strings.Repeat("v", MaxGuestAgentVersion+1)
		}, "version of 65 bytes"},
		{"a version with a newline", func(g *GuestReport) { g.AgentVersion = "1.0\nforged" }, "not printable ASCII"},
		{"a version with a space", func(g *GuestReport) { g.AgentVersion = "1.0 forged" }, "not printable ASCII"},
		{"a version with DEL", func(g *GuestReport) { g.AgentVersion = "1.0\x7f" }, "not printable ASCII"},
		{"a version that is not ASCII", func(g *GuestReport) { g.AgentVersion = "1.0é" }, "not printable ASCII"},
		{"a negative schema", func(g *GuestReport) { g.Schema = -1 }, "schema is outside 0 to 65535"},
		{"a schema over its bound", func(g *GuestReport) { g.Schema = 123456789 }, "schema is outside 0 to 65535"},
		{"a schema one over its bound", func(g *GuestReport) { g.Schema = MaxGuestSchema + 1 },
			"schema is outside 0 to 65535"},
		{"a negative accepted count", func(g *GuestReport) { g.Accepted = -1 }, "negative count"},
		{"a negative refused count", func(g *GuestReport) { g.Refused = -1 }, "negative count"},
		{"negative dropped bytes", func(g *GuestReport) { g.DroppedBytes = -1 }, "negative count"},
		{"a first arrival alone", func(g *GuestReport) { g.LastReceived = time.Time{} }, "one arrival time"},
		{"a last arrival alone", func(g *GuestReport) { g.FirstReceived = time.Time{} }, "one arrival time"},
		{"arrivals out of order", func(g *GuestReport) {
			g.FirstReceived, g.LastReceived = g.LastReceived, g.FirstReceived
		}, "first batch arrived after its last"},
		{"an arrival past what RFC 3339 spells", func(g *GuestReport) {
			g.LastReceived = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		}, "in the year 10000"},
		{"an arrival before what RFC 3339 spells", func(g *GuestReport) {
			g.FirstReceived = time.Date(0, 12, 31, 0, 0, 0, 0, time.UTC)
		}, "in the year 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			g := aGuestReport()
			tc.mutate(&g)
			if err := g.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want it to say %q", err, tc.want)
			}
		})
	}

	if err := aGuestReport().Validate(); err != nil {
		t.Fatalf("a report at every bound was refused: %v", err)
	}
	// NOR THE SCHEMA THE GUEST CHOSE.
	schema := aGuestReport()
	schema.Schema = 123456789
	if err := schema.Validate(); err == nil || strings.Contains(err.Error(), "123456789") {
		t.Fatalf("Validate = %v, want a refusal that does not quote the schema", err)
	}
	// THE REFUSAL NAMES WHERE, NEVER WHAT: the guest wrote the version.
	g := aGuestReport()
	g.AgentVersion = "s3cret-token\t"
	if err := g.Validate(); err == nil || strings.Contains(err.Error(), "s3cret") ||
		!strings.Contains(err.Error(), "at byte 12") {
		t.Fatalf("Validate = %v, want a refusal naming byte 12 and not the version", err)
	}
	// AN AGENT THAT NEVER SPOKE is a report too: the node says so.
	if err := (GuestReport{Codec: GuestReportCodec}).Validate(); err != nil {
		t.Fatalf("a report of nothing was refused: %v", err)
	}
	// ONE INSTANT IS ORDERED: a single batch arrives first and last at once.
	g = aGuestReport()
	g.LastReceived = g.FirstReceived
	if err := g.Validate(); err != nil {
		t.Fatalf("a report whose one batch arrived once was refused: %v", err)
	}
}
