package guestagent

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/guestreport"
)

// Every mark a line makes is one the node accepts: the guest writes the log, and a
// mark that broke a batch would cost the batch it rode in.
func FuzzStepLine(f *testing.F) {
	log, err := os.ReadFile("testdata/diag/Worker_20261010-020415-utc.log")
	if err != nil {
		f.Fatal(err)
	}

	for line := range strings.Lines(string(log)) {
		f.Add(strings.TrimSuffix(line, "\n"))
	}

	f.Add("[2026-10-10 02:04:20Z INFO StepsRunner] Processing step: DisplayName='\xff\x00'")
	f.Add("[9999-12-31 23:59:59Z INFO StepsRunner] Processing step: DisplayName='''")

	f.Fuzz(func(t *testing.T, line string) {
		mark, ok := parseStepLine(line)
		if !ok {
			return
		}

		b := guestreport.Batch{Seq: 1, AgentVersion: "v", TicksPerSecond: 100, Steps: []guestreport.StepMark{mark}}
		if _, err := guestreport.EncodeBatch(b); err != nil {
			t.Fatalf("the mark %+v of %q does not encode: %v", mark, line, err)
		}
	})
}

// A comm is the guest's text: whatever a stat line holds, a parsed one names the
// text between its first '(' and its last ')', and the row it makes encodes.
func FuzzPIDStat(f *testing.F) {
	for _, pid := range []string{"1", "2", "812", "900", "1033"} {
		data, err := os.ReadFile("testdata/proc/" + pid + "/stat")
		if err != nil {
			f.Fatal(err)
		}

		f.Add(data)
	}

	// utime and stime each at their bound, together past it.
	f.Add([]byte("1033 (billet) S 1 2 2 0 -1 4194560 10 0 0 0 9007199254740992 1 0 0 20 0 1 0 4100 1000 10 0"))

	f.Fuzz(func(t *testing.T, data []byte) {
		st, err := parsePIDStat(data)
		if err != nil {
			return
		}

		if open, closing := bytes.IndexByte(data, '('), bytes.LastIndexByte(data, ')'); st.comm != string(data[open+1:closing]) {
			t.Fatalf("comm %q, want the text between %d and %d", st.comm, open, closing)
		}

		rows, other, err := guestreport.Fold([]guestreport.ProcessRow{{
			Name:         textOrMark(guestreport.Clean(st.comm, guestreport.MaxNameBytes)),
			ProcessUsage: guestreport.ProcessUsage{Procs: 1, CPUTicks: st.cpu},
		}})
		if err != nil {
			t.Fatalf("the row of %q does not fold: %v", data, err)
		}

		b := guestreport.Batch{Seq: 1, AgentVersion: "v", TicksPerSecond: 100, Processes: []guestreport.ProcessSample{
			{AtMillis: 1, Rows: rows, Other: other},
		}}
		if _, err := guestreport.EncodeBatch(b); err != nil {
			t.Fatalf("the row of %q does not encode: %v", data, err)
		}
	})
}

// Whatever a result file holds, a parse keeps at most a batch's worth of suites,
// which fit makes a batch the node takes, and a parse that fails keeps none.
func FuzzJUnitParse(f *testing.F) {
	for _, name := range []string{
		"testdata/junit/repo/repo/report.xml", "testdata/junit/repo/repo/py/results.xml",
		"testdata/junit/repo/repo/pom.xml", "testdata/junit/a/b/c/d/e/ok.xml",
	} {
		data, err := os.ReadFile(name)
		if err != nil {
			f.Fatal(err)
		}

		f.Add(data)
	}

	// A batch's worth of suites whose failed names do not compress: within every
	// bound of the parse, and past what one batch carries until fit shrinks it.
	var many bytes.Buffer

	many.WriteString("<testsuites>")

	for i := range guestreport.MaxBatchSuites {
		fmt.Fprintf(&many, `<testsuite name="s%d">`, i)

		for j := range guestreport.MaxFailedNames {
			a, b, c := sha256.Sum256([]byte{byte(i), byte(j), 0}), sha256.Sum256([]byte{byte(i), byte(j), 1}),
				sha256.Sum256([]byte{byte(i), byte(j), 2})
			fmt.Fprintf(&many, `<testcase name="%x%x%x"><failure/></testcase>`, a, b, c)
		}

		many.WriteString("</testsuite>")
	}

	many.WriteString("</testsuites>")
	f.Add(many.Bytes())

	f.Fuzz(func(t *testing.T, data []byte) {
		w := &junitWalk{deadline: time.Now().Add(time.Minute), now: time.Now}

		suites, ok := w.parse(bytes.NewReader(data), "r\x01.xml")
		if !ok {
			if len(suites) != 0 {
				t.Fatalf("a failed parse kept %+v", suites)
			}

			return
		}

		if len(suites) > guestreport.MaxBatchSuites {
			t.Fatalf("a parse kept %d suites, past a batch's %d", len(suites), guestreport.MaxBatchSuites)
		}

		b := guestreport.Batch{Seq: 1, AgentVersion: "v", TicksPerSecond: 100, Tests: suites}

		body, err := fit(&b, guestreport.MaxBatchBytes)
		if err != nil {
			t.Fatalf("the suites of %q do not fit a batch: %v", data, err)
		}

		if _, err := guestreport.DecodeBatch(body); err != nil {
			t.Fatalf("the batch fit made of %q does not decode: %v", data, err)
		}
	})
}
