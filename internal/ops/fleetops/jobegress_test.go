package fleetops

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/state"
)

func aListedEgress() *alloc.RecordedEgress {
	return &alloc.RecordedEgress{Node: "epyc-1", RecordedAt: "2026-11-04T00:00:00Z", JobEgress: alloc.JobEgress{
		Listed: true, Queries: 12, RefusedQueries: 4,
		RefusedNames: []alloc.EgressRefusal{{Name: "evil.example", Count: 3}},
		Names: []alloc.EgressNames{
			{Addr: "198.51.100.1", Names: []string{"github.com", "edge.cdn.example"}},
		},
		DeniedPackets: 9, OutsideResolverPackets: 2, IPv6Packets: 5,
		Denied: []alloc.EgressDenial{
			{Addr: "203.0.113.7", Protocol: "tcp", Port: 443, Packets: 7},
			{Addr: "8.8.8.8", Protocol: "udp", Port: 53, Packets: 2},
		},
	}}
}

func renderedEgress(e jobEgress, rec alloc.JobRecord) string {
	var out bytes.Buffer
	renderEgress(&out, e, rec)

	return out.String()
}

func TestAListedJobSaysWhatItWasRefused(t *testing.T) {
	t.Parallel()

	out := renderedEgress(jobEgress{row: aListedEgress()}, alloc.JobRecord{})

	for _, want := range []string{
		"egress     listed:",
		"GitHub's runner endpoints",
		"queries    12 asked of the node's resolver, 4 refused and never forwarded",
		// THE ONE REFUSAL THE REPORT HAS NO NAME FOR IS COUNTED, NOT DROPPED.
		"refused    evil.example 3, 1 not named",
		"denied     9 packets refused, 2 of them DNS to a resolver other than the node's: " +
			"203.0.113.7:443/tcp 7, 8.8.8.8:53/udp 2",
		"ipv6       5 packets refused",
		"complete   yes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}

func TestAnOpenJobDoesNotReadAsARefusedOne(t *testing.T) {
	t.Parallel()

	row := &alloc.RecordedEgress{JobEgress: alloc.JobEgress{Queries: 7}}
	out := renderedEgress(jobEgress{row: row}, alloc.JobRecord{})

	if !strings.Contains(out, "egress     open:") || !strings.Contains(out, "queries    7 asked") {
		t.Errorf("an open job says:\n%s", out)
	}

	for line := range strings.SplitSeq(out, "\n") {
		for _, label := range []string{"denied", "refused", "ipv6"} {
			if strings.HasPrefix(line, label) {
				t.Errorf("an open job has a %q line: %q", label, line)
			}
		}
	}
}

// NO REPORT, A ROW THAT COULD NOT BE READ AND A RETIRED ONE ARE THREE THINGS, and none of
// them reads as a job that was refused nothing.
func TestAJobWithNoEgressReportSaysWhy(t *testing.T) {
	t.Parallel()

	if out := renderedEgress(jobEgress{}, alloc.JobRecord{}); !strings.Contains(out, "no report recorded") {
		t.Errorf("no report says:\n%s", out)
	}

	unreadable := jobEgress{unreadable: "the ledger's row could not be read: \"bad\""}
	if out := renderedEgress(unreadable, alloc.JobRecord{}); !strings.Contains(out, "could not be read") {
		t.Errorf("an unreadable row says:\n%s", out)
	}

	retired := alloc.JobRecord{DetailRetiredAt: "2026-11-01T00:00:00Z"}
	if out := renderedEgress(jobEgress{}, retired); !strings.Contains(out, "retired 2026-11-01") {
		t.Errorf("a retired row says:\n%s", out)
	}
}

func TestAnIncompleteReportSaysItIsALowerBound(t *testing.T) {
	t.Parallel()

	row := aListedEgress()
	row.Incomplete = true

	if out := renderedEgress(jobEgress{row: row}, alloc.JobRecord{}); !strings.Contains(out, "lower bound") {
		t.Errorf("an incomplete report says:\n%s", out)
	}

	row = aListedEgress()
	row.DeniedPackets, row.OutsideResolverPackets, row.Denied = 0, 0, nil

	if out := renderedEgress(jobEgress{row: row}, alloc.JobRecord{}); !strings.Contains(out, "no packets refused") {
		t.Errorf("a job denied nothing says:\n%s", out)
	}
}

// NAMES SIT BESIDE ADDRESSES WHEN SOMETHING WATCHED, AND AN ADDRESS NOTHING ANSWERED FOR
// SAYS SO. With no report there is no column: a table of "no known name" would claim a
// resolver looked.
func TestDestinationsCarryTheirNamesWhenAResolverWatched(t *testing.T) {
	t.Parallel()

	d := &alloc.JobDestinations{Destinations: []alloc.JobDestination{
		{Addr: "198.51.100.1", SentBytes: 2048, Connections: 1},
		{Addr: "203.0.113.9", SentBytes: 1024, Connections: 1},
	}}

	var named bytes.Buffer

	renderDestinations(&named, d, jobEgress{row: aListedEgress()}.names())

	lines := strings.Split(named.String(), "\n")

	row := func(addr string) string {
		for _, l := range lines {
			if strings.Contains(l, addr) {
				return l
			}
		}

		t.Fatalf("no row for %s in:\n%s", addr, named.String())

		return ""
	}

	if got := row("198.51.100.1"); !strings.HasSuffix(got, "github.com, edge.cdn.example") {
		t.Errorf("row = %q", got)
	}

	if got := row("203.0.113.9"); !strings.HasSuffix(got, "no known name") {
		t.Errorf("row = %q", got)
	}

	if !strings.Contains(named.String(), "best effort") {
		t.Errorf("the column does not say what it is:\n%s", named.String())
	}

	var unwatched bytes.Buffer

	renderDestinations(&unwatched, d, jobEgress{}.names())

	if strings.Contains(unwatched.String(), "no known name") || strings.Contains(unwatched.String(), "best effort") {
		t.Errorf("a job nothing watched has a names column:\n%s", unwatched.String())
	}
}

func TestTheJobPageCarriesNamesAndRefusals(t *testing.T) {
	t.Parallel()

	m := &alloc.RecordedUsage{JobUsage: alloc.JobUsage{Destinations: &alloc.JobDestinations{
		Destinations: []alloc.JobDestination{{Addr: "198.51.100.1", SentBytes: 1, Connections: 1}},
	}}}
	network := jobEgress{row: aListedEgress()}

	page := jobPageOf(alloc.JobRecord{LeaseID: "l1"}, m, jobSteps{none: "x"}, nil, "no series")
	page.Destinations = destinationsSection(alloc.JobRecord{}, m, network.names())
	page.Egress = egressFields(network, alloc.JobRecord{})

	var out bytes.Buffer
	if err := renderJobPage(&out, page); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"github.com, edge.cdn.example", "evil.example 3", "203.0.113.7:443/tcp 7", `id="egress"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the page has no %q", want)
		}
	}
}

// DECODED INTO A MAP, because the field names are what a program reads: a round trip through
// the same struct would pass a rename.
func TestTheJSONRecordCarriesTheReportOrSaysWhyNot(t *testing.T) {
	t.Parallel()

	decode := func(network jobEgress) map[string]any {
		var out bytes.Buffer
		if err := renderJobJSON(&out, alloc.JobRecord{LeaseID: "l1"}, nil, nil, "", nil, network, jobTests{}, nil); err != nil {
			t.Fatal(err)
		}

		var got map[string]any
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("the record does not decode: %v", err)
		}

		return got
	}

	report, ok := decode(jobEgress{row: aListedEgress()})["egress"].(map[string]any)
	if !ok || report["node"] != "epyc-1" || report["listed"] != true || report["denied_packets"] != float64(9) {
		t.Fatalf("egress = %+v", report)
	}

	none := decode(jobEgress{})
	if value, present := none["egress"]; !present || value != nil {
		t.Fatalf("no report: egress is %v, present %v; want a null, never a report of zeros", value, present)
	}

	if _, present := none["egress_unreadable"]; present {
		t.Fatal("a job with no report says its row was unreadable")
	}

	unreadable := decode(jobEgress{unreadable: "bad"})
	if unreadable["egress"] != nil || unreadable["egress_unreadable"] != "bad" {
		t.Fatalf("unreadable = %+v", unreadable)
	}
}

// THROUGH THE LEDGER AND THE COMMAND: `billet jobs show` prints names beside addresses and
// what a list refused, its --html does too, and a row it cannot read says so rather than
// reading as a job nothing watched.
func TestJobsShowPrintsAJobsNetwork(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	report := aListedEgress().JobEgress
	lease := seedJob(t, stateDir, jobSeed{requestID: 91, egress: &report, usage: &alloc.JobUsage{
		Source: alloc.UsageSourceHost, Samples: 1, IntervalMillis: 1000, Unmeasured: []string{alloc.UsageEnergy},
		Destinations: &alloc.JobDestinations{
			Destinations: []alloc.JobDestination{{Addr: "198.51.100.1", SentBytes: 10, Connections: 1}},
		},
	}})

	out := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, lease}); err != nil {
			t.Fatalf("jobs show: %v", err)
		}
	})

	for _, want := range []string{"github.com, edge.cdn.example", "evil.example 3", "203.0.113.7:443/tcp 7", "egress     listed"} {
		if !strings.Contains(out, want) {
			t.Errorf("jobs show has no %q:\n%s", want, out)
		}
	}

	page := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, "--html", lease}); err != nil {
			t.Fatalf("jobs show --html: %v", err)
		}
	})

	for _, want := range []string{"github.com, edge.cdn.example", "evil.example 3", `id="egress"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the job page has no %q", want)
		}
	}

	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE job_egress SET report = 'nope' WHERE lease_id = $1`, lease)

		return err
	}); err != nil {
		t.Fatalf("stage the row: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close the ledger: %v", err)
	}

	broken := capture(t, func() {
		if err := Jobs(t.Context(), processEnv(), []string{"show", "--config", cfg, lease}); err != nil {
			t.Fatalf("jobs show: %v", err)
		}
	})

	if !strings.Contains(broken, "could not be read") || strings.Contains(broken, "no report recorded (one arrives") {
		t.Errorf("a row the ledger cannot read said:\n%s", broken)
	}
}
