// Package suppress implements billet's reason-required suppression directive, so
// a custom analyzer can be a hard CI gate even where a handful of sites are
// legitimately exempt.
//
// The directive goes on the flagged line, or on its own line directly above it:
//
//	//billet:ignore parallelshared // the map is guarded by mu, which the analyzer cannot see
//
// A REASON IS MANDATORY. A bare `//billet:ignore parallelshared` is itself
// reported, exactly as nolintlint rejects a bare //nolint -- so "0 violations"
// means "0 UNEXPLAINED violations", which is the only version of that sentence
// worth gating on. billetlint is its own binary and has no access to
// golangci-lint's //nolint machinery, which is why this exists at all.
//
// AN UNUSED DIRECTIVE IS ALSO REPORTED, which is the same rule .golangci.yml
// already sets for nolintlint (`allow-unused: false`). A directive that suppresses
// nothing today is a licence sitting in the source waiting for a finding nobody
// wrote a reason for: the code moves, a new race appears on that line, and the
// gate stays quiet. Deleting a stale one costs nothing; keeping it costs the
// guarantee.
//
// A TRAILING DIRECTIVE COVERS ONLY ITS OWN LINE. Reaching to the next line as
// well would silence a write nobody wrote a reason for either -- so the "line
// below" rule applies only to a directive alone on its line, where the intent is
// unambiguous.
package suppress

import (
	"go/ast"
	"go/token"
	"regexp"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// directive matches `//billet:ignore <analyzer> [// <reason>]`.
var directive = regexp.MustCompile(`^//\s*billet:ignore\s+(\S+)\s*(.*)$`)

// entry is one directive and what became of it.
type entry struct {
	pos token.Pos
	// ownLine is true when nothing but whitespace precedes the comment, which is
	// what lets it cover the line below.
	ownLine bool
	used    bool
}

// Lines is the directive index for one file.
type Lines struct {
	byLine map[int]map[string]*entry
}

// Present reports whether a file carries any directive at all.
//
// A CHEAP QUESTION ASKED FIRST, so a caller need not read the file's bytes to
// find out there is nothing to index. That matters because an analysis pass runs
// over every DEPENDENCY as well as the package under test -- facts require it --
// and analysis.Pass.ReadFile refuses a path outside the package it was given.
// Measured on Linux CI: reading runtime/cgo's source failed the whole run with
// "is not among OtherFiles, IgnoredFiles, or names of Files", on a package that
// has never heard of billet. No directive, no read, no failure.
func Present(f *ast.File) bool {
	for _, group := range f.Comments {
		for _, c := range group.List {
			if directive.MatchString(c.Text) {
				return true
			}
		}
	}

	return false
}

// Index builds the directive index for one file, reporting any bare directive.
//
// The zero Lines is usable: a caller that skipped indexing because Present said
// there was nothing can still call Skip and ReportUnused on it.
//
// src is the file's own bytes, and it is a PARAMETER rather than something read
// here: distinguishing a directive alone on its line from one trailing a
// statement needs the text before it, and a caller that cannot supply the bytes
// must not silently get the looser rule.
func Index(pass *analysis.Pass, f *ast.File, src []byte) Lines {
	out := Lines{byLine: map[int]map[string]*entry{}}

	for _, group := range f.Comments {
		for _, c := range group.List {
			m := directive.FindStringSubmatch(c.Text)
			if m == nil {
				continue
			}

			pos := pass.Fset.Position(c.Pos())
			name, rest := m[1], strings.TrimSpace(m[2])

			reason := strings.TrimSpace(strings.TrimPrefix(rest, "//"))
			if !strings.HasPrefix(rest, "//") || reason == "" {
				pass.Reportf(c.Pos(),
					"//billet:ignore %s needs a reason: write "+
						"`//billet:ignore %s // <why this site is exempt>`. An exemption "+
						"nobody can answer for is one nobody dares remove", name, name)

				continue
			}

			if out.byLine[pos.Line] == nil {
				out.byLine[pos.Line] = map[string]*entry{}
			}

			out.byLine[pos.Line][name] = &entry{
				pos:     c.Pos(),
				ownLine: aloneOnItsLine(src, pos),
			}
		}
	}

	return out
}

// aloneOnItsLine reports whether only whitespace precedes the comment.
func aloneOnItsLine(src []byte, pos token.Position) bool {
	start := pos.Offset - (pos.Column - 1)
	if start < 0 || pos.Offset > len(src) {
		return false
	}

	for _, b := range src[start:pos.Offset] {
		if b != ' ' && b != '\t' {
			return false
		}
	}

	return true
}

// Skip reports whether a diagnostic at pos is suppressed for this analyzer, and
// records that the directive was needed.
func (l Lines) Skip(pass *analysis.Pass, pos token.Pos, analyzer string) bool {
	line := pass.Fset.Position(pos).Line

	if e, ok := l.byLine[line][analyzer]; ok {
		e.used = true

		return true
	}

	if e, ok := l.byLine[line-1][analyzer]; ok && e.ownLine {
		e.used = true

		return true
	}

	return false
}

// ReportUnused reports every directive that suppressed nothing.
//
// CALLED AFTER THE ANALYZER HAS FINISHED WITH THE FILE, because a directive is
// only unused once everything that could have consumed it has run.
func (l Lines) ReportUnused(pass *analysis.Pass, analyzer string) {
	for _, byName := range l.byLine {
		e, ok := byName[analyzer]
		if !ok || e.used {
			continue
		}

		pass.Reportf(e.pos,
			"//billet:ignore %s suppresses nothing here; remove it. A directive that "+
				"covers no finding is a standing exemption for whatever appears on this "+
				"line next, with a reason written for something else", analyzer)
	}
}
