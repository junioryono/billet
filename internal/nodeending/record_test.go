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

// PROBES GO BEFORE ANY NODE PROCESS: sixteen readiness probes never push out
// the node's own unresolved ending, and the record says it was trimmed only
// once a node process's entry went.
func TestTheTrimDropsProbesFirst(t *testing.T) {
	dir := t.TempDir()
	now := clock(epoch)

	if _, err := Begin(dir, nodeEntry("inv-node", 1), now); err != nil {
		t.Fatal(err)
	}

	for i := range MaxEntries + 4 {
		p := nodeEntry("inv-probe", 100+i)
		p.Role = RoleProbe

		if _, err := Begin(dir, p, now); err != nil {
			t.Fatal(err)
		}
	}

	rec, _, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(rec.Entries) != MaxEntries || rec.Entries[0].InvocationID != "inv-node" || rec.Trimmed {
		t.Fatalf("kept %d entries, the first %q, trimmed %v; want %d with the node's first and not trimmed",
			len(rec.Entries), rec.Entries[0].InvocationID, rec.Trimmed, MaxEntries)
	}

	if got := AdmitStart(rec, nil, cleanUnit, "boot-a"); got.Verdict != Unproved {
		t.Fatalf("the node's killed process was admitted over: %s (%s)", got.Verdict, got.Why)
	}

	for i := range MaxEntries {
		if _, err := Begin(dir, nodeEntry("inv-later", 200+i), now); err != nil {
			t.Fatal(err)
		}
	}

	rec, _, err = Read(dir)
	if err != nil || !rec.Trimmed {
		t.Fatalf("after node entries were dropped the record is trimmed %v (%v), want true", rec.Trimmed, err)
	}
}

// A RECORD ANOTHER USER COULD HAVE WRITTEN IS NOT EVIDENCE.
func TestARecordOthersCanWriteIsRefused(t *testing.T) {
	dir := t.TempDir()

	if _, err := Begin(dir, nodeEntry("inv-1", 1), clock(epoch)); err != nil {
		t.Fatal(err)
	}

	if _, _, err := Read(dir); err != nil {
		t.Fatalf("the record as written was refused: %v", err)
	}

	for _, mode := range []os.FileMode{0o620, 0o602} {
		if err := os.Chmod(Path(dir), mode); err != nil {
			t.Fatal(err)
		}

		if _, _, err := Read(dir); err == nil || !strings.Contains(err.Error(), "lets another user write it") {
			t.Errorf("a record at %04o read as %v, want a refusal", mode, err)
		}
	}
}

// SYSTEMD'S ACCOUNT IS RECORDED ONLY WHERE THE NODE KEPT NONE, ONCE, AND A
// CLEAN STOP ONLY OVER A HISTORY THAT ADMITS A START, all read again under the
// record's lock: so a stale check cannot write a clean stop over a newer
// failure.
func TestASystemdObservationIsRecordedOnlyWhereItHolds(t *testing.T) {
	stopped := func(inv string) Entry {
		return Entry{InvocationID: inv, BootID: "boot-a", Ending: Stopped}
	}

	t.Run("the invocation has its own entry", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := Begin(dir, nodeEntry("inv-1", 1), clock(epoch)); err != nil {
			t.Fatal(err)
		}

		if _, err := observeIn(dir, stopped("inv-1"), false, clock(epoch)); !errors.Is(err, ErrRecordChanged) {
			t.Fatalf("Observe over the node's own entry returned %v, want ErrRecordChanged", err)
		}
	})

	t.Run("over a killed process", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := Begin(dir, nodeEntry("inv-1", 1), clock(epoch)); err != nil {
			t.Fatal(err)
		}

		if _, err := observeIn(dir, stopped("inv-stale"), false, clock(epoch)); !errors.Is(err, ErrRecordChanged) {
			t.Fatalf("a clean stop over a killed process was recorded: %v", err)
		}

		failed := stopped("inv-stale")
		failed.Ending = Failed

		if _, err := observeIn(dir, failed, false, clock(epoch)); err != nil {
			t.Fatalf("a failed stop was refused: %v", err)
		}
	})

	t.Run("once", func(t *testing.T) {
		dir := t.TempDir()
		now := clock(epoch)

		for range 2 {
			if _, err := observeIn(dir, stopped("inv-old"), false, now); err != nil {
				t.Fatal(err)
			}
		}

		rec, _, err := Read(dir)
		if err != nil || len(rec.Entries) != 1 || rec.Entries[0].Source != SourceSystemd {
			t.Fatalf("after two observations the record is %+v (%v), want one systemd entry", rec.Entries, err)
		}

		// The failure was read first and reached the lock second: it still wins,
		// and the clean account cannot take it back.
		failed := stopped("inv-old")
		failed.Ending = Failed

		if _, err := observeIn(dir, failed, false, now); err != nil {
			t.Fatalf("a failure over a recorded clean stop returned %v, want it recorded", err)
		}

		if _, err := observeIn(dir, stopped("inv-old"), false, now); !errors.Is(err, ErrRecordChanged) {
			t.Fatalf("a clean account over a recorded failure returned %v, want ErrRecordChanged", err)
		}

		rec, _, err = Read(dir)
		if err != nil || len(rec.Entries) != 1 || rec.Entries[0].Ending != Failed {
			t.Fatalf("the record is %+v (%v), want the one entry failed", rec.Entries, err)
		}

		if got := AdmitStart(rec, nil, cleanUnit, "boot-a"); got.Verdict != Unproved {
			t.Errorf("a start over the recorded failure is %s (%s), want refused", got.Verdict, got.Why)
		}
	})

	t.Run("a repeated failure names what is newest now", func(t *testing.T) {
		dir := t.TempDir()
		now := clock(epoch)

		failed := stopped("inv-a")
		failed.Ending = Failed

		if _, err := observeIn(dir, failed, false, now); err != nil {
			t.Fatal(err)
		}

		h, err := Begin(dir, nodeEntry("inv-b", 2), now)
		if err != nil {
			t.Fatal(err)
		}

		if err := h.End(LeftRunning, false); err != nil {
			t.Fatal(err)
		}

		after, err := observeIn(dir, failed, false, now)
		if err != nil || Remedy(after) != "--invocation inv-b" {
			t.Fatalf("a repeated observation returned %v and names %q, want the newest, inv-b", err,
				Remedy(after))
		}
	})

	t.Run("a failure over an acknowledged clean account", func(t *testing.T) {
		dir := t.TempDir()
		now := clock(epoch)

		if _, err := observeIn(dir, stopped("inv-x"), false, now); err != nil {
			t.Fatal(err)
		}

		if _, err := Acknowledge(dir, "inv-x", false, now); err != nil {
			t.Fatal(err)
		}

		failed := stopped("inv-x")
		failed.Ending = Failed

		after, err := observeIn(dir, failed, false, now)
		if err != nil || after.Entries[0].Acknowledged || after.Entries[0].Ending != Failed {
			t.Fatalf("a failure over an acknowledged clean stop left %+v (%v), want failed and not acknowledged",
				after.Entries, err)
		}

		if got := AdmitStart(after, nil, cleanUnit, "boot-a"); got.Verdict != Unproved {
			t.Errorf("a start over the failure is %s (%s), want refused: the acknowledgement was of a clean "+
				"stop", got.Verdict, got.Why)
		}
	})

	t.Run("a clean account over an acknowledged failure", func(t *testing.T) {
		dir := t.TempDir()
		now := clock(epoch)

		failed := stopped("inv-x")
		failed.Ending = Failed

		if _, err := observeIn(dir, failed, false, now); err != nil {
			t.Fatal(err)
		}

		if _, err := Acknowledge(dir, "inv-x", false, now); err != nil {
			t.Fatal(err)
		}

		if _, err := observeIn(dir, stopped("inv-x"), false, now); !errors.Is(err, ErrRecordChanged) {
			t.Fatalf("a clean account over an acknowledged failure returned %v, want ErrRecordChanged", err)
		}
	})

	t.Run("a clean stop where a drain is required", func(t *testing.T) {
		dir := t.TempDir()
		now := clock(epoch)

		if _, err := Begin(dir, nodeEntry("inv-1", 1), now); err != nil {
			t.Fatal(err)
		}

		if _, err := Acknowledge(dir, "inv-1", false, now); err != nil {
			t.Fatal(err)
		}

		if _, err := observeIn(dir, stopped("inv-2"), true, now); !errors.Is(err, ErrRecordChanged) {
			t.Fatalf("a clean stop over an acknowledgement, a drain required, returned %v", err)
		}

		if _, err := observeIn(dir, stopped("inv-2"), false, now); err != nil {
			t.Fatalf("a clean stop over an acknowledgement, no drain required, returned %v", err)
		}
	})

	t.Run("no invocation", func(t *testing.T) {
		if _, err := observeIn(t.TempDir(), stopped(""), false, clock(epoch)); err == nil {
			t.Fatal("a stop with no invocation was recorded")
		}
	})
}

// A DIRECTORY AT THE RECORD'S NAME IS RESET TOO: it cannot be linked, so it is
// moved aside, and a start may follow.
func TestADirectoryAtTheRecordsNameIsReset(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(Path(dir), 0o700); err != nil {
		t.Fatal(err)
	}

	aside, _, err := Reset(dir, clock(epoch))
	if err != nil {
		t.Fatalf("the reset of a directory returned %v", err)
	}

	if info, err := os.Stat(aside); err != nil || !info.IsDir() {
		t.Errorf("the directory set aside is %v (%v), want it kept", info, err)
	}

	rec, _, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}

	if got := AdmitStart(rec, nil, cleanUnit, "boot-a"); got.Verdict != Proved {
		t.Errorf("after the reset a start is %s (%s), want proved", got.Verdict, got.Why)
	}
}

// A RESET THAT CANNOT WRITE ITS REPLACEMENT LEAVES THE OLD RECORD AT ITS NAME:
// every judgement still answers could-not-tell, and the host never has no
// record, which systemd's clean triple alone would prove.
func TestAFailedResetLeavesTheRecordInPlace(t *testing.T) {
	t.Run("a directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(Path(dir), 0o700); err != nil {
			t.Fatal(err)
		}

		prev := resetInstall
		resetInstall = func(string, Record) error { return errors.New("the disk is full") }

		if _, _, err := Reset(dir, clock(epoch)); err == nil {
			t.Fatal("a reset whose install failed reported success")
		}

		resetInstall = prev

		// The directory is aside and nothing is at the record's name, which the
		// marker makes could-not-tell rather than absent.
		rec, present, err := Read(dir)
		if err == nil || present || !strings.Contains(err.Error(), "interrupted") {
			t.Fatalf("after a failed directory reset the record reads present %v, err %v; want interrupted",
				present, err)
		}

		if got := Standing(rec, err, cleanUnit, true, "boot-a"); got.Verdict != Unknown {
			t.Fatalf("a drain over an interrupted reset is %s (%s), want could-not-tell", got.Verdict, got.Why)
		}

		if _, _, err := Reset(dir, clock(epoch.Add(time.Hour))); err != nil {
			t.Fatalf("the reset run again returned %v", err)
		}

		if _, err := os.Lstat(filepath.Join(dir, resetMarker)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the marker outlived the reset: %v", err)
		}

		if rec, _, err := Read(dir); err != nil || AdmitStart(rec, nil, cleanUnit, "boot-a").Verdict != Proved {
			t.Errorf("after the reset completed the record reads %v and a start is not admitted", err)
		}
	})

	t.Run("a file", func(t *testing.T) { aFailedFileResetLeavesTheRecordInPlace(t) })
}

// TWO CHECKS OF ONE STOP ARE JUDGED ONE AFTER THE OTHER: the second asks
// systemd only once the first has written what it saw, so a clean account
// read after reset-failed cannot be proved while the first check's failure is
// still unwritten.
func TestTwoChecksOfOneStopAreJudgedInTurn(t *testing.T) {
	dir := t.TempDir()

	inSample, release := make(chan struct{}), make(chan struct{})

	first := make(chan Judgement, 1)

	go func() {
		j, _ := JudgeStop(dir, "inv-x", false, true, func() Unit {
			close(inSample)
			<-release

			return timedOut
		}, clock(epoch))
		first <- j
	}()

	<-inSample

	second := make(chan Judgement, 1)

	var sawFailure bool

	go func() {
		j, _ := JudgeStop(dir, "inv-x", false, true, func() Unit {
			rec, _, err := Read(dir)
			sawFailure = err == nil && len(rec.Entries) == 1 && rec.Entries[0].Ending == Failed

			return cleanUnit
		}, clock(epoch))
		second <- j
	}()

	// Long enough for the second check to reach systemd if nothing held it,
	// which the assertion below would then catch; never a condition for
	// passing.
	time.Sleep(50 * time.Millisecond)
	close(release)

	if j := <-first; j.Verdict != Unproved {
		t.Fatalf("the check that saw the timeout is %s (%s), want unproved", j.Verdict, j.Why)
	}

	j := <-second
	if !sawFailure {
		t.Fatal("the second check asked systemd before the first had written what it saw")
	}

	if j.Verdict != Unproved {
		t.Fatalf("the check that saw the clean triple after the failure is %s (%s), want unproved", j.Verdict,
			j.Why)
	}
}

func aFailedFileResetLeavesTheRecordInPlace(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	prev := resetInstall
	resetInstall = func(string, Record) error { return errors.New("the disk is full") }

	t.Cleanup(func() { resetInstall = prev })

	aside, _, err := Reset(dir, clock(epoch))
	if err == nil {
		t.Fatal("a reset whose install failed reported success")
	}

	if _, present, err := Read(dir); err == nil || present {
		t.Fatalf("after a failed reset the record reads present %v, err %v; want it still unreadable", present, err)
	}

	if body, err := os.ReadFile(aside); err != nil || string(body) != "{" {
		t.Errorf("the record kept beside itself holds %q (%v), want what it held", body, err)
	}
}

// AN UNREADABLE RECORD IS SET ASIDE, NEVER DELETED, by the reset alone, which
// leaves an acknowledgement a start may follow; a readable or absent record is
// refused, naming what to acknowledge instead.
func TestAnUnreadableRecordIsSetAsideByTheReset(t *testing.T) {
	dir := t.TempDir()

	if _, _, err := Reset(dir, clock(epoch)); err == nil || !strings.Contains(err.Error(), "no ending record") {
		t.Fatalf("a reset with no record returned %v, want a refusal", err)
	}

	if _, err := Begin(dir, nodeEntry("inv-1", 1), clock(epoch)); err != nil {
		t.Fatal(err)
	}

	if _, _, err := Reset(dir, clock(epoch)); err == nil || !strings.Contains(err.Error(), "can be read") {
		t.Fatalf("a reset of a readable record returned %v, want a refusal", err)
	}

	if err := os.WriteFile(Path(dir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	aside, e, err := Reset(dir, clock(epoch))
	if err != nil || !e.Acknowledged || e.Ending != Unrecorded {
		t.Fatalf("the reset returned %+v, %v", e, err)
	}

	if body, err := os.ReadFile(aside); err != nil || string(body) != "{" {
		t.Errorf("the unreadable record set aside holds %q (%v), want what it held", body, err)
	}

	rec, _, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}

	if got := AdmitStart(rec, nil, cleanUnit, "boot-a"); got.Verdict != Proved {
		t.Errorf("after the reset a start is %s (%s), want proved", got.Verdict, got.Why)
	}
}
