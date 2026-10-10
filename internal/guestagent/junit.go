package guestagent

import (
	"cmp"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path"
	"slices"
	"strings"
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
}

// collectJUnit is the test suites in the JUnit XML files under roots, at most a
// batch's worth: the ones with failures first, then the ones with the most tests.
// Nothing under a root is reached through a symlink out of it, and only a regular
// file is read.
func collectJUnit(roots []string, now func() time.Time, log *limitedLog) []guestreport.SuiteResult {
	w := &junitWalk{deadline: now().Add(junitBudget), now: now}

	for _, root := range roots {
		if w.stopped {
			break
		}

		r, err := os.OpenRoot(root)
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

	return w.suites
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
	d, err := r.Open(dir)
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
		w.keep(s)
	}
}

// parsedCase is the testcase being read.
type parsedCase struct {
	name                     string
	failed, errored, skipped bool
}

// parse reads one file's suites with a streaming decoder. A testcase belongs to
// the innermost suite around it; a suite with no testcase of its own is not kept.
func (w *junitWalk) parse(r io.Reader, file string) ([]guestreport.SuiteResult, bool) {
	dec := xml.NewDecoder(r)

	var (
		out    []guestreport.SuiteResult
		open   []guestreport.SuiteResult
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
			case t.Name.Local == "testcase" && len(open) > 0 && tcase == nil:
				tcase = &parsedCase{name: caseName(attr(t, "classname"), attr(t, "name"))}
			case tcase != nil && t.Name.Local == "failure":
				tcase.failed = true
			case tcase != nil && t.Name.Local == "error":
				tcase.errored = true
			case tcase != nil && t.Name.Local == "skipped":
				tcase.skipped = true
			}
		case xml.EndElement:
			depth--

			switch {
			case t.Name.Local == "testcase" && tcase != nil:
				countCase(&open[len(open)-1], tcase)
				tcase = nil
			case t.Name.Local == "testsuite" && len(open) > 0:
				done := open[len(open)-1]
				open = open[:len(open)-1]

				if done.Tests > 0 {
					out = append(out, done)
				}
			}
		}
	}
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

// keep adds s to the suites kept, holding only the batch's worth that rank first.
func (w *junitWalk) keep(s guestreport.SuiteResult) {
	w.suites = append(w.suites, s)
	slices.SortStableFunc(w.suites, func(a, b guestreport.SuiteResult) int {
		return cmp.Or(
			cmp.Compare(b.Failures+b.Errors, a.Failures+a.Errors),
			cmp.Compare(b.Tests, a.Tests),
			cmp.Compare(a.Name, b.Name),
		)
	})

	if len(w.suites) > guestreport.MaxBatchSuites {
		w.suites = slices.Clip(w.suites[:guestreport.MaxBatchSuites])
	}
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
