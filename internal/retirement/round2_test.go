package retirement

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// THE TAIL IS ORDERED: done_at with done and nowhere else, the row
// acknowledged only at done and only by someone, settled only after the
// acknowledgement. A journal that says "settled" over a row nobody completed
// would authorise the release the acknowledgement exists to gate, so the
// writer refuses it and the reader refuses one written by hand.
func TestTheTailIsOrderedInTheJournal(t *testing.T) {
	useRoot(t)

	done := func(j *Journal) {
		j.Phase, j.DoneAt = PhaseDone, "2026-09-11T09:00:00Z"
	}

	refused := map[string]func(*Journal){
		"settled without the acknowledgement": func(j *Journal) { done(j); j.Settled = true },
		"row_done without completed_by":       func(j *Journal) { done(j); j.RowDone = true },
		"completed_by without row_done":       func(j *Journal) { done(j); j.CompletedBy = "control-b" },
		"settled without done":                func(j *Journal) { j.Settled = true },
		"row_done before done":                func(j *Journal) { j.RowDone, j.CompletedBy = true, "control-b" },
		"done_at before done":                 func(j *Journal) { j.DoneAt = "2026-09-11T09:00:00Z" },
		"done without done_at":                func(j *Journal) { j.Phase = PhaseDone },
		"done with an unparseable done_at":    func(j *Journal) { j.Phase, j.DoneAt = PhaseDone, "yesterday" },
	}

	for name, mutate := range refused {
		j := sampleJournal()
		mutate(&j)

		if err := j.Write(time.Now()); err == nil {
			t.Errorf("%s: written", name)
		}

		if _, presence, err := ReadJournal(); presence != JournalAbsent || err != nil {
			t.Fatalf("%s: a refused write left a journal behind (%d %v)", name, presence, err)
		}
	}

	admitted := map[string]func(*Journal){
		"done":                  done,
		"done and acknowledged": func(j *Journal) { done(j); j.RowDone, j.CompletedBy = true, "control-b" },
		"done, acknowledged, settled": func(j *Journal) {
			done(j)
			j.RowDone, j.CompletedBy, j.Settled = true, "control-b", true
		},
	}

	for name, mutate := range admitted {
		j := sampleJournal()
		mutate(&j)

		if err := j.Write(time.Now()); err != nil {
			t.Errorf("%s: %v", name, err)
		}

		if _, presence, err := ReadJournal(); presence != JournalPresent {
			t.Errorf("%s: read back as %d: %v", name, presence, err)
		}
	}

	// And a hand-written settled journal over an unacknowledged row is refused
	// by the reader as malformed, never validated by the settled shortcut.
	j := sampleJournal()
	done(&j)
	j.Settled = true

	body := mustJSON(t, j)
	if err := os.WriteFile(JournalPath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, presence, err := ReadJournal(); presence != JournalMalformed || err == nil ||
		!strings.Contains(err.Error(), "settled over a row nobody acknowledged") {
		t.Fatalf("a settled journal over an unacknowledged row must be malformed, got %d %v", presence, err)
	}
}

// THE RETIREMENT DIRECTORY IS THIS ACCOUNT'S, 0700 AND NOT A LINK, or nothing
// is written into it: a 0600 journal in a directory another account can
// write is a journal that account can remove or replace by its entry.
func TestTheRetiredDirectoryIsTrustedBeforeAnythingIsWritten(t *testing.T) {
	root := useRoot(t)
	dir := RetiredDir()

	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	j := sampleJournal()

	err := j.Write(time.Now())
	if err == nil || !strings.Contains(err.Error(), "0755") || !strings.Contains(err.Error(), "not 0700") {
		t.Fatalf("a group- or world-accessible retirement directory must refuse naming its mode, got %v", err)
	}

	if _, statErr := os.Lstat(JournalPath()); statErr == nil {
		t.Fatal("the refused write published a journal")
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := j.Write(time.Now()); err != nil {
		t.Fatalf("the directory at 0700 is admitted, got %v", err)
	}

	// A LINK where the directory should be is refused, wherever it points.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	elsewhere := filepath.Join(root, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Skipf("this filesystem cannot make a symlink: %v", err)
	}

	err = j.Write(time.Now())
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("a link at the retirement directory must refuse, got %v", err)
	}

	if entries, err := os.ReadDir(elsewhere); err != nil || len(entries) != 0 {
		t.Fatalf("the refused write followed the link: %v %v", entries, err)
	}
}

// THE WRITER IS HELD TO THE READER'S BOUND: a journal longer than its reader
// admits is refused before publication and the previous journal is kept,
// because a journal its own reader refuses is a retirement nothing can resume.
func TestTheJournalWriterIsHeldToTheReadersBound(t *testing.T) {
	useRoot(t)

	first := sampleJournal()
	if err := first.Write(time.Now()); err != nil {
		t.Fatal(err)
	}

	huge := sampleJournal()
	for i := range 2000 {
		huge.Nodes = append(huge.Nodes, JournalNode{
			Name: fmt.Sprintf("node-%04d", i), Incarnation: strings.Repeat("i", 32), Endpoint: "https://10.0.0.2:7717",
		})
	}

	err := huge.Write(time.Now())
	if err == nil || !strings.Contains(err.Error(), "longer than the") || !strings.Contains(err.Error(), "2001 nodes") {
		t.Fatalf("a journal over the reader's bound must refuse naming the bound and the node count, got %v", err)
	}

	read, presence, err := ReadJournal()
	if err != nil || presence != JournalPresent || len(read.Nodes) != 1 {
		t.Fatalf("the previous journal must be kept, got %d %v (%d nodes)", presence, err, len(read.Nodes))
	}

	entries, err := os.ReadDir(RetiredDir())
	if err != nil || len(entries) != 1 || entries[0].Name() != "journal.json" {
		t.Fatalf("the refused write left something beside the journal: %v %v", entries, err)
	}
}

// A JOURNAL IS READ THROUGH EOF, NOT TO THE SIZE A STAT REPORTED: a file that
// grows between the reader's stat and its read is refused (the size moved),
// and a size-based read that found a complete document in the stat's prefix
// would have admitted it with the growth unread.
func TestAJournalIsReadThroughEOF(t *testing.T) {
	useRoot(t)

	j := sampleJournal()
	if err := j.Write(time.Now()); err != nil {
		t.Fatal(err)
	}

	appendTrailer := func() {
		f, err := os.OpenFile(JournalPath(), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := f.WriteString(`{"schema":1}` + "\n"); err != nil {
			t.Fatal(err)
		}

		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// Grown UNDER the read: the stat saw the whole document, the read finds
	// more, and the closing stat finds a size that moved.
	journalBetweenStatAndRead = appendTrailer

	_, presence, err := ReadJournal()
	journalBetweenStatAndRead = nil

	if presence != JournalUnreadable || err == nil || !strings.Contains(err.Error(), "changed under the read") {
		t.Fatalf("a journal grown under the read must be unreadable, got %d %v", presence, err)
	}

	// Grown BEFORE the read: content behind the document is malformed.
	if _, presence, err := ReadJournal(); presence != JournalMalformed || err == nil ||
		!strings.Contains(err.Error(), "trailing content") {
		t.Fatalf("a document with content behind it must be malformed, got %d %v", presence, err)
	}

	// Rewritten in place under the read with the same size: the time moved.
	if err := j.Write(time.Now()); err != nil {
		t.Fatal(err)
	}

	journalBetweenStatAndRead = func() {
		time.Sleep(20 * time.Millisecond)

		body, err := os.ReadFile(JournalPath())
		if err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(JournalPath(), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	_, presence, err = ReadJournal()
	journalBetweenStatAndRead = nil

	if presence != JournalUnreadable || err == nil || !strings.Contains(err.Error(), "changed under the read") {
		t.Fatalf("a journal rewritten under the read must be unreadable, got %d %v", presence, err)
	}
}

// THE TRANSITION ID HAS THE MARKER'S GRAMMAR: a journal whose provenance names
// an id no guard reader admits is malformed, so no marker is ever restored
// from it.
func TestTheJournalsTransitionIDHasTheMarkersGrammar(t *testing.T) {
	useRoot(t)

	for _, bad := range []string{"bad", strings.Repeat("D", 32), strings.Repeat("d", 31), strings.Repeat("d", 33)} {
		j := sampleJournal()
		j.Provenance.TransitionID = bad

		if err := j.Write(time.Now()); err == nil || !strings.Contains(err.Error(), "32 lowercase hex digits") {
			t.Errorf("%q: written, or refused for another reason: %v", bad, err)
		}
	}
}
