package nodeending

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func clock(start time.Time) func() time.Time {
	at := start

	return func() time.Time {
		at = at.Add(time.Second)

		return at
	}
}

var epoch = time.Date(2026, 10, 17, 18, 0, 0, 0, time.UTC)

func nodeEntry(invocation string, pid int) Entry {
	return Entry{InvocationID: invocation, PID: pid, BootID: "boot-a", Release: "v0.17.0", Role: RoleNode,
		RunsOnHost: true}
}

// A PROCESS'S ENTRY IS WRITTEN AS RUNNING AND ENDED ONCE: the ending replaces
// running on the same entry, the file is 0600, and a second End changes
// nothing.
func TestAProcessIsRecordedRunningAndEndedOnce(t *testing.T) {
	dir := t.TempDir()

	h, err := Begin(dir, nodeEntry("inv-1", 41), clock(epoch))
	if err != nil {
		t.Fatal(err)
	}

	rec, present, err := Read(dir)
	if err != nil || !present || len(rec.Entries) != 1 || rec.Entries[0].Ending != Running ||
		rec.Entries[0].Source != SourceNode {
		t.Fatalf("after Begin: %+v present %v err %v, want one running node entry", rec, present, err)
	}

	if err := h.End(Drained, true); err != nil {
		t.Fatal(err)
	}

	if err := h.End(Failed, false); err != nil {
		t.Fatal(err)
	}

	rec, _, err = Read(dir)
	if err != nil {
		t.Fatal(err)
	}

	got := rec.Entries[0]
	if len(rec.Entries) != 1 || got.Ending != Drained || !got.DrainRequested || got.EndedAt == nil {
		t.Fatalf("after End: %+v, want the one entry drained with a drain requested and an end time", rec)
	}

	info, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Errorf("the record is %v, want 0600", info.Mode().Perm())
	}
}

// THE RECORD KEEPS THE NEWEST SIXTEEN: the oldest go, the newest stay.
func TestTheRecordKeepsTheNewestEntries(t *testing.T) {
	dir := t.TempDir()
	now := clock(epoch)

	for i := range MaxEntries + 3 {
		if _, err := Begin(dir, nodeEntry("inv-"+string(rune('a'+i)), i), now); err != nil {
			t.Fatal(err)
		}
	}

	rec, _, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(rec.Entries) != MaxEntries || rec.Entries[0].PID != 3 || rec.Entries[MaxEntries-1].PID != MaxEntries+2 {
		t.Fatalf("kept %d entries, first pid %d, last pid %d; want %d, 3 and %d", len(rec.Entries),
			rec.Entries[0].PID, rec.Entries[len(rec.Entries)-1].PID, MaxEntries, MaxEntries+2)
	}
}

// AN UNREADABLE RECORD IS NEVER OVERWRITTEN: a Begin over it refuses, and the
// file keeps what it held, because the evidence in it is what a later
// judgement needs.
func TestAnUnreadableRecordIsNeverOverwritten(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":       "{",
		"another schema": `{"schema":2,"entries":[]}`,
		"an unknown end": `{"schema":1,"entries":[{"ending":"vanished","role":"node","source":"node"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(Path(dir), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := Begin(dir, nodeEntry("inv-1", 1), clock(epoch)); err == nil {
				t.Fatal("Begin wrote over an unreadable record")
			}

			got, err := os.ReadFile(Path(dir))
			if err != nil || string(got) != body {
				t.Errorf("the record now holds %q (%v), want it untouched", got, err)
			}
		})
	}
}

// A MISSING RECORD IS ABSENT, NOT AN ERROR; a symlink at its name is an error.
func TestAMissingRecordIsAbsentAndALinkIsRefused(t *testing.T) {
	dir := t.TempDir()

	if _, present, err := Read(dir); err != nil || present {
		t.Fatalf("a missing record read present %v, err %v; want absent and no error", present, err)
	}

	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"schema":1,"entries":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(target, Path(dir)); err != nil {
		t.Fatal(err)
	}

	if _, _, err := Read(dir); err == nil {
		t.Error("a symlink at the record's name was read")
	}
}

// AN END WHOSE ENTRY IS GONE SAYS SO: the record was replaced under the
// process, and its ending is not written onto someone else's entry.
func TestAnEndWhoseEntryIsGoneSaysSo(t *testing.T) {
	dir := t.TempDir()

	h, err := Begin(dir, nodeEntry("inv-1", 7), clock(epoch))
	if err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(Record{Schema: Schema, Entries: []Entry{{InvocationID: "other", PID: 8,
		Role: RoleNode, Source: SourceNode, Ending: Running}}})
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(Path(dir), body, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := h.End(Drained, false); !errors.Is(err, ErrNoEntry) {
		t.Fatalf("End over a replaced record returned %v, want ErrNoEntry", err)
	}

	rec, _, err := Read(dir)
	if err != nil || rec.Entries[0].Ending != Running {
		t.Errorf("the other process's entry is now %+v (%v), want it still running", rec.Entries, err)
	}
}

// ONLY THE NEWEST STOP IS ACKNOWLEDGED, BY ITS EXACT INVOCATION; an
// unrecorded acknowledgement appends its own entry.
func TestOnlyTheNewestStopIsAcknowledged(t *testing.T) {
	dir := t.TempDir()
	now := clock(epoch)

	for _, inv := range []string{"inv-old", "inv-new"} {
		if _, err := Begin(dir, nodeEntry(inv, 1), now); err != nil {
			t.Fatal(err)
		}
	}

	probe := nodeEntry("inv-probe", 2)
	probe.Role = RoleProbe

	if _, err := Begin(dir, probe, now); err != nil {
		t.Fatal(err)
	}

	if _, err := Acknowledge(dir, "inv-old", false, now); err == nil ||
		!strings.Contains(err.Error(), `newest node entry is invocation "inv-new"`) {
		t.Fatalf("acknowledging an older stop returned %v, want a refusal naming the newest", err)
	}

	got, err := Acknowledge(dir, "inv-new", false, now)
	if err != nil || !got.Acknowledged || got.InvocationID != "inv-new" {
		t.Fatalf("acknowledging the newest stop returned %+v, %v", got, err)
	}

	got, err = Acknowledge(dir, "", true, now)
	if err != nil || got.Ending != Unrecorded || got.Source != SourceOperator || !got.Acknowledged {
		t.Fatalf("an unrecorded acknowledgement returned %+v, %v", got, err)
	}

	rec, _, err := Read(dir)
	if err != nil || len(rec.Entries) != 4 || rec.Entries[3].Ending != Unrecorded {
		t.Errorf("the record is %+v (%v), want the unrecorded acknowledgement appended", rec.Entries, err)
	}
}
