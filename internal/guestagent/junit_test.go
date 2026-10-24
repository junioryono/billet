package guestagent

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/guestreport"
)

// unhurried is a clock the search's time budget never runs out on, for the
// tests whose subject is something else: a loaded runner spent junitBudget's two
// seconds of wall clock after four of twenty files (#188, CI 2026-10-12). The
// budget itself is the "time" bound's case, on a clock that moves.
func unhurried() time.Time { return at(0) }

// The fixture holds a Go and a pytest result file three levels down, one six down
// and one seven, a pom.xml with a testsuite inside, and result files under
// node_modules and _actions that are other people's.
func TestTheJUnitSearchReadsTheJobsResultFiles(t *testing.T) {
	t.Parallel()

	log, _ := discardLog()
	got := collectJUnit([]string{"testdata/junit", filepath.Join(t.TempDir(), "absent")}, unhurried, log)

	want := []guestreport.SuiteResult{
		{Name: "github.com/acme/widget/internal/parse", Tests: 4, Failures: 1, Skipped: 1,
			Failed: []string{`github.com/acme/widget/internal/parse.TestParseQuoted/it's_"quoted"`}},
		{Name: "github.com/acme/widget/internal/render", Tests: 3, Failures: 1,
			Failed: []string{"github.com/acme/widget/internal/render.TestRenderWide"}},
		{Name: "pytest", Tests: 3, Errors: 1, Failed: []string{"tests.test_api.test_post"}},
		{Name: "six levels down", Tests: 1},
	}

	if len(got) != len(want) {
		t.Fatalf("suites %+v, want %+v", got, want)
	}

	for i := range want {
		if got[i].Name != want[i].Name || got[i].Tests != want[i].Tests || got[i].Failures != want[i].Failures ||
			got[i].Errors != want[i].Errors || got[i].Skipped != want[i].Skipped ||
			!slices.Equal(got[i].Failed, want[i].Failed) {
			t.Errorf("suite %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A root is the boundary: a symlink out of it, to a directory or to a file, is not
// followed, and anything but a regular file at a result's name is not read.
func TestTheJUnitSearchFollowsNoSymlinkOutOfARoot(t *testing.T) {
	t.Parallel()

	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.xml"), `<testsuite name="outside" tests="1"><testcase name="x"/></testsuite>`)

	root := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked-dir")); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(filepath.Join(outside, "secret.xml"), filepath.Join(root, "linked.xml")); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(root, "inside.xml"), `<testsuite name="inside" tests="1"><testcase name="x"/></testsuite>`)

	log, _ := discardLog()
	got := collectJUnit([]string{root}, unhurried, log)

	if len(got) != 1 || got[0].Name != "inside" {
		t.Fatalf("suites %+v, want the one inside the root alone", got)
	}
}

// Every bound of the search holds: files, size, depth and time.
func TestTheJUnitSearchKeepsItsBounds(t *testing.T) {
	t.Parallel()

	suite := func(name string) string {
		return `<testsuite name="` + name + `" tests="1"><testcase name="x"/></testsuite>`
	}

	t.Run("files", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		for i := range junitMaxFiles + 20 {
			writeFile(t, filepath.Join(root, fmt.Sprintf("r%03d.xml", i)), suite(fmt.Sprintf("s%03d", i)))
		}

		w := &junitWalk{deadline: time.Now().Add(time.Hour), now: time.Now}

		r, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { _ = r.Close() })
		w.walk(r, ".", 0)

		if w.files != junitMaxFiles || !w.stopped {
			t.Fatalf("read %d files (stopped %v), want %d and stopped", w.files, w.stopped, junitMaxFiles)
		}
	})

	t.Run("size", func(t *testing.T) {
		t.Parallel()

		root := t.TempDir()
		big := `<testsuite name="big" tests="1"><testcase name="x"/>` + strings.Repeat(" ", junitMaxBytes) + `</testsuite>`
		writeFile(t, filepath.Join(root, "big.xml"), big)
		writeFile(t, filepath.Join(root, "small.xml"), suite("small"))

		log, _ := discardLog()
		got := collectJUnit([]string{root}, unhurried, log)

		if len(got) != 1 || got[0].Name != "small" {
			t.Fatalf("suites %+v, want the small one alone", got)
		}
	})

	t.Run("time", func(t *testing.T) {
		t.Parallel()

		// A clock that moves a second each time it is read: the search's
		// budget is spent after two readings.
		clock := at(0)
		tick := func() time.Time {
			clock = clock.Add(time.Second)

			return clock
		}

		log, _ := discardLog()
		got := collectJUnit([]string{"testdata/junit"}, tick, log)

		if len(got) != 0 {
			t.Fatalf("a search past its budget kept %+v", got)
		}
	})
}

// A file that is not JUnit, or does not parse to its end, adds nothing: a count
// from half a file would be a claim.
func TestAResultFileThatDoesNotParseAddsNothing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, xml string }{
		{"cut short", `<testsuite name="a" tests="2"><testcase name="x"/><testcase name="y">`},
		{"not junit", `<project><testsuite name="a"><testcase name="x"/></testsuite></project>`},
		{"not xml", `{"tests": 3}`},
		{"empty", ``},
		{"mismatched", `<testsuite name="a"><testcase name="x"></testsuite></testcase>`},
		{"too deep", strings.Repeat("<testsuite>", junitMaxNesting+1) + strings.Repeat("</testsuite>", junitMaxNesting+1)},
		{"another encoding", `<?xml version="1.0" encoding="ISO-8859-1"?><testsuite name="a"><testcase name="x"/></testsuite>`},
	} {
		w := &junitWalk{deadline: time.Now().Add(time.Hour), now: time.Now}

		if suites, ok := w.parse(strings.NewReader(tc.xml), "r.xml"); ok || len(suites) != 0 {
			t.Errorf("%s: kept %+v (%v)", tc.name, suites, ok)
		}
	}

	w := &junitWalk{deadline: time.Now().Add(time.Hour), now: time.Now}

	suites, ok := w.parse(strings.NewReader(`<testsuite><testcase name="x"/></testsuite>`), "named-by-file.xml")
	if !ok || len(suites) != 1 || suites[0].Name != "named-by-file.xml" {
		t.Errorf("a suite without a name kept %+v (%v), want it named by its file", suites, ok)
	}
}

// A batch carries MaxBatchSuites suites, the failing ones first, and each at most
// MaxFailedNames failed names, every name cut to its bound.
func TestTheSuitesKeptAreABatchsWorthFailingFirst(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	for i := range guestreport.MaxBatchSuites + 4 {
		writeFile(t, filepath.Join(root, fmt.Sprintf("pass%02d.xml", i)),
			fmt.Sprintf(`<testsuite name="pass%02d"><testcase name="a"/><testcase name="b"/></testsuite>`, i))
	}

	var cases strings.Builder
	for i := range guestreport.MaxFailedNames + 5 {
		fmt.Fprintf(&cases, `<testcase classname="c" name="%s%d"><failure/></testcase>`, strings.Repeat("n", 300), i)
	}

	writeFile(t, filepath.Join(root, "fail.xml"), `<testsuite name="fail">`+cases.String()+`</testsuite>`)

	log, _ := discardLog()
	got := collectJUnit([]string{root}, unhurried, log)

	if len(got) != guestreport.MaxBatchSuites || got[0].Name != "fail" {
		first := ""
		if len(got) > 0 {
			first = got[0].Name
		}

		t.Fatalf("kept %d suites, the first %q; want %d, the failing one first", len(got), first,
			guestreport.MaxBatchSuites)
	}

	failing := got[0]
	if failing.Failures != int64(guestreport.MaxFailedNames+5) || len(failing.Failed) != guestreport.MaxFailedNames {
		t.Fatalf("the failing suite counts %d failures and names %d, want %d and %d", failing.Failures,
			len(failing.Failed), guestreport.MaxFailedNames+5, guestreport.MaxFailedNames)
	}

	for _, name := range failing.Failed {
		if len(name) > guestreport.MaxFailedNameBytes || !strings.HasPrefix(name, "nnn") {
			t.Fatalf("a failed name %q is past its bound or lost the test's name to its class", name)
		}
	}

	b := guestreport.Batch{Seq: 1, AgentVersion: "v", TicksPerSecond: 100, Tests: got}
	if _, err := guestreport.EncodeBatch(b); err != nil {
		t.Fatalf("the suites kept do not encode: %v", err)
	}
}
