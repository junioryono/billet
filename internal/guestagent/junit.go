package guestagent

import (
	"cmp"
	"encoding/xml"
	"errors"
	"io"
	"math"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/junioryono/billet/internal/guestreport"
	"github.com/junioryono/billet/internal/regularfile"
)

// The bounds of the one search for test results, at the final flush: how deep
// below a root, how many result files, how large each, how many directory entries
// in all and how long, and how deeply a result file's elements may nest.
const (
	junitMaxDepth    = 6
	junitMaxFiles    = 200
	junitMaxBytes    = 4 << 20
	junitMaxEntries  = 100_000
	junitBudget      = 2 * time.Second
	junitMaxNesting  = 256
	junitTokenStride = 1024
)

// junitSkipped are directories that hold other people's files, not this job's
// results: the actions and tools the runner downloaded, a package manager's
// dependencies, and a repository's history.
var junitSkipped = map[string]bool{"_actions": true, "_tool": true, "node_modules": true, ".git": true}

// junitWalk is one search: the bounds it has spent, and the suites worth keeping.
type junitWalk struct {
	deadline time.Time
	now      func() time.Time
	files    int
	entries  int
	stopped  bool

	suites []guestreport.SuiteResult
	// cases are the suites with their cases, within a batch's bounds, and parsed
	// are those of the file being read, kept only once it has parsed to its end.
	cases  []guestreport.SuiteCases
	parsed []guestreport.SuiteCases
}

// collectJUnit is the test suites in the JUnit XML files under roots, at most a
// batch's worth: the ones with failures first, then the ones with the most tests;
// and their cases, within a batch's bounds. Nothing under a root is reached through
// a symlink out of it, and only a regular file is read.
func collectJUnit(roots []string, now func() time.Time, log *limitedLog,
) ([]guestreport.SuiteResult, []guestreport.SuiteCases) {
	w := &junitWalk{deadline: now().Add(junitBudget), now: now}

	for _, root := range roots {
		if w.stopped {
			break
		}

		r, err := openRoot(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			log.printf("junit", "a test-results root was not opened: %v", withoutPath(err))

			continue
		}

		w.walk(r, ".", 0)
		_ = r.Close()
	}

	return w.suites, w.cases
}

// openRoot is os.OpenRoot of a directory. os.OpenRoot opens the root itself
// without O_DIRECTORY, so a FIFO the job put at the name would hold it: the
// directory is proved first, without waiting.
func openRoot(dir string) (*os.Root, error) {
	d, err := openDir(dir)
	if err != nil {
		return nil, err
	}

	_ = d.Close()

	return os.OpenRoot(dir)
}

// spent reports whether the search has used one of its bounds, and stops it if so.
func (w *junitWalk) spent() bool {
	if !w.stopped && (w.files >= junitMaxFiles || w.entries >= junitMaxEntries || !w.now().Before(w.deadline)) {
		w.stopped = true
	}

	return w.stopped
}

// walk reads the directory dir of r, at depth below the root.
func (w *junitWalk) walk(r *os.Root, dir string, depth int) {
	d, err := r.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return
	}
	defer d.Close()

	for !w.spent() {
		entries, err := d.ReadDir(256)
		for _, e := range entries {
			w.entries++
			if w.spent() {
				return
			}

			name := e.Name()

			switch {
			case e.Type()&os.ModeSymlink != 0:
				continue
			case e.IsDir():
				if depth+1 < junitMaxDepth && !junitSkipped[name] {
					w.walk(r, path.Join(dir, name), depth+1)
				}
			case e.Type().IsRegular() && strings.HasSuffix(strings.ToLower(name), ".xml"):
				w.files++
				w.read(d, name)
			}
		}

		if err != nil || len(entries) == 0 {
			return
		}
	}
}

// read parses one candidate file, refusing anything but a regular file at the
// name and anything over junitMaxBytes. A file that is not JUnit, or does not
// parse to its end, adds nothing: a count from half a file would be a claim.
func (w *junitWalk) read(dir *os.File, name string) {
	f, info, err := regularfile.OpenAt(dir, name)
	if err != nil {
		return
	}
	defer f.Close()

	if info.Size() > junitMaxBytes {
		return
	}

	suites, ok := w.parse(io.LimitReader(f, junitMaxBytes), name)
	if !ok {
		return
	}

	for _, s := range suites {
		w.suites = keep(w.suites, s)
	}

	for _, s := range w.parsed {
		w.cases = keepCases(w.cases, s)
	}
}

// parsedCase is the testcase being read.
type parsedCase struct {
	name                     string
	failed, errored, skipped bool
	// millis is what its time attribute says, nil where it says nothing readable.
	millis *int64
	// message is the first failure's or error's message attribute, or the start
	// of its text where it has none, and inFailure says that text is what is
	// being read.
	message   string
	inFailure bool
}

// parse reads one file's suites with a streaming decoder, keeping a batch's worth
// of them as it goes, so a file of many small suites costs no more to hold than a
// file of a few. A testcase belongs to the innermost suite around it; a suite with
// no testcase of its own is not kept.
func (w *junitWalk) parse(r io.Reader, file string) ([]guestreport.SuiteResult, bool) {
	dec := xml.NewDecoder(r)
	w.parsed = nil

	var (
		out    []guestreport.SuiteResult
		open   []guestreport.SuiteResult
		cases  []guestreport.SuiteCases
		tcase  *parsedCase
		depth  int
		tokens int
		root   = true
	)

	for {
		tokens++
		if tokens%junitTokenStride == 0 && !w.now().Before(w.deadline) {
			w.stopped = true

			return nil, false
		}

		// A strict decoder answers io.EOF only after every element it opened has
		// closed; a file cut short is a syntax error.
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return out, !root
		}

		if err != nil {
			return nil, false
		}

		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth > junitMaxNesting {
				return nil, false
			}

			if root {
				if t.Name.Local != "testsuites" && t.Name.Local != "testsuite" {
					return nil, false
				}

				root = false
			}

			switch {
			case t.Name.Local == "testsuite":
				open = append(open, guestreport.SuiteResult{Name: suiteName(attr(t, "name"), file)})
				cases = append(cases, guestreport.SuiteCases{})
			case t.Name.Local == "testcase" && len(open) > 0 && tcase == nil:
				tcase = &parsedCase{name: caseName(attr(t, "classname"), attr(t, "name")),
					millis: caseMillis(attr(t, "time"))}
			case tcase != nil && t.Name.Local == "failure":
				tcase.failed = true
				tcase.beginFailure(t)
			case tcase != nil && t.Name.Local == "error":
				tcase.errored = true
				tcase.beginFailure(t)
			case tcase != nil && t.Name.Local == "skipped":
				tcase.skipped = true
			}
		case xml.EndElement:
			depth--

			switch {
			case t.Name.Local == "testcase" && tcase != nil:
				countCase(&open[len(open)-1], tcase)
				addCase(&cases[len(cases)-1], tcase)
				tcase = nil
			case tcase != nil && (t.Name.Local == "failure" || t.Name.Local == "error"):
				tcase.inFailure = false
			case t.Name.Local == "testsuite" && len(open) > 0:
				done := open[len(open)-1]
				open = open[:len(open)-1]

				kept := cases[len(cases)-1]
				cases = cases[:len(cases)-1]

				if done.Tests > 0 {
					out = keep(out, done)
					kept.Name, kept.Tests, kept.Failures, kept.Errors, kept.Skipped = done.Name, done.Tests,
						done.Failures, done.Errors, done.Skipped
					kept.Complete = int64(len(kept.Cases)) == done.Tests
					w.parsed = keepCases(w.parsed, kept)
				}
			}
		case xml.CharData:
			if tcase != nil && tcase.inFailure && tcase.message == "" {
				tcase.message = caseMessage(string(t))
			}
		}
	}
}

// beginFailure notes a failure or an error of the case: its message attribute, and
// that its text may stand in for one, unless an earlier one already gave a message.
func (c *parsedCase) beginFailure(e xml.StartElement) {
	if c.message != "" {
		return
	}

	c.message = caseMessage(attr(e, "message"))
	c.inFailure = true
}

// addCase adds one case to its suite's list. A list at its bound gives up what is
// least worth keeping first, so one result file of many cases costs no more to hold
// than a batch's worth; the suite is Complete only if its list ends as long as its
// count.
func addCase(s *guestreport.SuiteCases, c *parsedCase) {
	for len(s.Cases) >= guestreport.MaxBatchCases {
		one := []guestreport.SuiteCases{*s}
		guestreport.ShedCases(&one)
		*s = one[0]
	}

	status := guestreport.CasePassed

	switch {
	case c.failed:
		status = guestreport.CaseFailed
	case c.errored:
		status = guestreport.CaseErrored
	case c.skipped:
		status = guestreport.CaseSkipped
	}

	out := guestreport.CaseResult{Name: c.name, Status: status, DurationMillis: c.millis}
	if c.failed || c.errored {
		out.Message = c.message
	}

	s.Cases = append(s.Cases, out)
}

// keepCases adds s to suites, holding what a batch may: the suites that rank first,
// as keep ranks them, and past the bound on cases only what ShedCases does not give
// up.
func keepCases(suites []guestreport.SuiteCases, s guestreport.SuiteCases) []guestreport.SuiteCases {
	suites = append(suites, s)

	if len(suites) > guestreport.MaxBatchCaseSuites {
		slices.SortStableFunc(suites, func(a, b guestreport.SuiteCases) int {
			return cmp.Or(
				cmp.Compare(b.Failures+b.Errors, a.Failures+a.Errors),
				cmp.Compare(b.Tests, a.Tests),
				cmp.Compare(a.Name, b.Name),
			)
		})
		suites = slices.Clip(suites[:guestreport.MaxBatchCaseSuites])
	}

	for guestreport.CountCases(suites) > guestreport.MaxBatchCases {
		guestreport.ShedCases(&suites)
	}

	return suites
}

// caseMillis is a testcase's time attribute, decimal seconds, in milliseconds: nil
// for one that is absent, unreadable (some tools group digits with a comma),
// negative or past what a report holds, because no duration is not a duration of
// zero.
func caseMillis(s string) *int64 {
	seconds, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(seconds) || seconds < 0 || seconds*1000 > guestreport.MaxValue {
		return nil
	}

	millis := int64(math.Round(seconds * 1000))

	return &millis
}

// caseMessage is a failure's message as a report carries it, "" when it says
// nothing.
func caseMessage(s string) string {
	return guestreport.Clean(strings.TrimSpace(s), guestreport.MaxCaseMessageBytes)
}

// countCase adds one testcase to its suite.
func countCase(s *guestreport.SuiteResult, c *parsedCase) {
	s.Tests++

	switch {
	case c.failed:
		s.Failures++
	case c.errored:
		s.Errors++
	case c.skipped:
		s.Skipped++
	}

	if (c.failed || c.errored) && len(s.Failed) < guestreport.MaxFailedNames {
		s.Failed = append(s.Failed, c.name)
	}
}

// keep adds s to suites, holding only the batch's worth that rank first: the
// failing ones, then the ones with the most tests.
func keep(suites []guestreport.SuiteResult, s guestreport.SuiteResult) []guestreport.SuiteResult {
	suites = append(suites, s)
	slices.SortStableFunc(suites, func(a, b guestreport.SuiteResult) int {
		return cmp.Or(
			cmp.Compare(b.Failures+b.Errors, a.Failures+a.Errors),
			cmp.Compare(b.Tests, a.Tests),
			cmp.Compare(a.Name, b.Name),
		)
	})

	if len(suites) > guestreport.MaxBatchSuites {
		suites = slices.Clip(suites[:guestreport.MaxBatchSuites])
	}

	return suites
}

// suiteName is a suite's name as a report carries it, the file's when the suite
// has none.
func suiteName(name, file string) string {
	if s := guestreport.Clean(name, guestreport.MaxNameBytes); s != "" {
		return s
	}

	return textOrMark(guestreport.Clean(file, guestreport.MaxNameBytes))
}

// caseName is a failed test's name as a report carries it: its class and its
// name, or its name alone when both together are past the bound.
func caseName(class, name string) string {
	full := name
	if class != "" && len(class)+1+len(name) <= guestreport.MaxFailedNameBytes {
		full = class + "." + name
	}

	return textOrMark(guestreport.Clean(full, guestreport.MaxFailedNameBytes))
}

func attr(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if a.Name.Local == name && a.Name.Space == "" {
			return a.Value
		}
	}

	return ""
}
