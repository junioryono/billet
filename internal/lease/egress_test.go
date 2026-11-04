package lease

import (
	"fmt"
	"strings"
	"testing"
)

func anEgressReport() JobEgress {
	return JobEgress{
		Listed: true, Queries: 10, RefusedQueries: 3,
		RefusedNames:  []EgressRefusal{{Name: "evil.example", Count: 2}},
		Names:         []EgressNames{{Addr: "198.51.100.1", Names: []string{"github.com", "edge_1.cdn-x.example"}}},
		DeniedPackets: 9, OutsideResolverPackets: 2, IPv6Packets: 4,
		Denied: []EgressDenial{{Addr: "203.0.113.7", Protocol: "tcp", Port: 443, Packets: 7}},
	}
}

func TestAnEgressReportThatIsWhatItClaimsIsKept(t *testing.T) {
	t.Parallel()

	if err := anEgressReport().Validate(); err != nil {
		t.Fatal(err)
	}

	open := JobEgress{Queries: 1, Names: []EgressNames{{Addr: "2001:db8::1", Names: []string{"a.example"}}}}
	if err := open.Validate(); err != nil {
		t.Fatalf("an open guest's report: %v", err)
	}

	if err := (JobEgress{}).Validate(); err != nil {
		t.Fatalf("a report of nothing: %v", err)
	}
}

// EVERY CASE IS THE ONE GOOD REPORT WITH ONE THING BROKEN, AND SAYS WHICH REFUSAL IT
// EXPECTS. The names are the guest's to choose, so what they may hold is the rule
// that matters most here: each of these would otherwise reach a terminal.
func TestAnEgressReportTheLedgerShouldNotKeepIsRefused(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		change func(*JobEgress)
		want   string
	}{
		"negative queries":  {func(e *JobEgress) { e.Queries = -1 }, "negative count"},
		"negative denied":   {func(e *JobEgress) { e.DeniedPackets = -1 }, "negative count"},
		"negative IPv6":     {func(e *JobEgress) { e.IPv6Packets = -1 }, "negative count"},
		"more refused":      {func(e *JobEgress) { e.RefusedQueries = 11 }, "11 refused of 10"},
		"more outside":      {func(e *JobEgress) { e.OutsideResolverPackets = 10 }, "of 9 denied"},
		"open but refusing": {func(e *JobEgress) { e.Listed = false }, "no list"},
		"an escape in a refused name": {
			func(e *JobEgress) { e.RefusedNames[0].Name = "a\x1b[31m.example" }, "not one",
		},
		"a newline in a refused name": {
			func(e *JobEgress) { e.RefusedNames[0].Name = "a\nforged: line" }, "not one",
		},
		"an upper-case refused name": {func(e *JobEgress) { e.RefusedNames[0].Name = "EVIL.example" }, "not one"},
		"a refused name over 253":    {func(e *JobEgress) { e.RefusedNames[0].Name = strings.Repeat("a", 254) }, "not one"},
		"an empty refused name":      {func(e *JobEgress) { e.RefusedNames[0].Name = "" }, "not one"},
		"a refusal of no times":      {func(e *JobEgress) { e.RefusedNames[0].Count = 0 }, "0 times"},
		"an escape in a named name":  {func(e *JobEgress) { e.Names[0].Names[0] = "a\x1b.example" }, "not one"},
		"an address with a zone":     {func(e *JobEgress) { e.Names[0].Addr = "fe80::1%eth0" }, "canonical form"},
		"an address not canonical":   {func(e *JobEgress) { e.Names[0].Addr = "2001:DB8::1" }, "canonical form"},
		"an address named twice": {
			func(e *JobEgress) { e.Names = append(e.Names, e.Names[0]) }, "named twice",
		},
		"an address with no names":   {func(e *JobEgress) { e.Names[0].Names = nil }, "carries 0 names"},
		"an address with five names": {func(e *JobEgress) { e.Names[0].Names = make([]string, 5) }, "carries 5 names"},
		"a denied bad address":       {func(e *JobEgress) { e.Denied[0].Addr = "evil" }, "canonical form"},
		"a denied bad protocol":      {func(e *JobEgress) { e.Denied[0].Protocol = "icmp" }, `protocol "icmp"`},
		"a denied bad port":          {func(e *JobEgress) { e.Denied[0].Port = 65536 }, "port 65536"},
		"denied negative packets":    {func(e *JobEgress) { e.Denied[0].Packets = -1 }, "-1 packets"},
		"too many refused": {
			func(e *JobEgress) {
				e.RefusedNames = make([]EgressRefusal, MaxEgressRefusedNames+1)
			}, "at most 64",
		},
		"too many denied": {
			func(e *JobEgress) { e.Denied = make([]EgressDenial, MaxEgressDenied+1) }, "at most 64",
		},
		"too many addresses": {
			func(e *JobEgress) {
				e.Names = nil
				for i := range MaxEgressNamedAddrs + 1 {
					e.Names = append(e.Names, EgressNames{
						Addr:  fmt.Sprintf("10.0.%d.%d", i/256, i%256),
						Names: []string{"a.example"},
					})
				}
			}, "at most 256",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			report := anEgressReport()
			tc.change(&report)

			err := report.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want one naming %q", err, tc.want)
			}
		})
	}
}
