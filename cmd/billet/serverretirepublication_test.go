package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
)

func armRetirePublicationWatcher(t *testing.T, f *requestFixture, path string) {
	t.Helper()
	installRetirePathWatcher(t, f, backupServiceUnit)
	f.manager.set("billet-backup.path", "ActiveState", "active")
	writeFile(t, filepath.Join(f.unitsDir, "watcher-source"), "[Path]\nPathChanged="+path+"\nUnit="+backupServiceUnit+"\n", 0o644)
}

func TestRetirementReceiptAdmitsAfterRegistrationWait(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "absent directory"
		if existing {
			name = "existing receipt"
		}
		t.Run(name, func(t *testing.T) {
			f, _ := retireProofHost(t, retirement.VariantRetainedNode, retirement.PhaseDone)
			path := useEndpointReceipt(t)
			dir := filepath.Dir(path)
			if existing {
				writeFile(t, path, "previous receipt", 0o600)
			} else {
				mustOK(t, os.Remove(dir))
			}
			savedOpen, savedPoll := registrationOpen, endpointPoll
			reads := 0
			registrationOpen = func(path string) (*os.File, os.FileInfo, error) {
				reads++
				if reads == 1 {
					return nil, nil, os.ErrNotExist
				}
				if reads == 2 {
					armRetirePublicationWatcher(t, f, dir)
				}
				return savedOpen(path)
			}
			endpointPoll = time.Millisecond
			t.Cleanup(func() { registrationOpen, endpointPoll = savedOpen, savedPoll })
			out, code := retiredRequest(t, f, requestRun)
			if reads < 2 || code != exitUnknown || !strings.Contains(out, "TriggeredBy") {
				t.Fatalf("watcher introduced during registration waiting was not refused: reads=%d %s", reads, out)
			}
			if existing {
				if mustRead(t, path) != "previous receipt" {
					t.Fatal("receipt replaced before admission")
				}
				entries, err := os.ReadDir(dir)
				mustOK(t, err)
				if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
					t.Fatalf("temporary created before admission: %v", entries)
				}
			} else if _, err := os.Lstat(dir); !os.IsNotExist(err) {
				t.Fatalf("receipt directory created before admission: %v", err)
			}
		})
	}
}

func TestRetirementCompletionDoesNotCreateArchiveLockAfterReceipt(t *testing.T) {
	f, j := retireProofHost(t, retirement.VariantRetainedNode, retirement.PhaseDone)
	path := useEndpointReceipt(t)
	// Absence must refuse; recreating the archived lock would notify the watcher.
	mustOK(t, os.Remove(state.DirectoryLockPath(j.Archive)))
	saved := receiptSyncDir
	armed := false
	receiptSyncDir = func(dir string) error {
		err := saved(dir)
		if err == nil && dir == filepath.Dir(filepath.Dir(path)) {
			armed = true
			armRetirePublicationWatcher(t, f, j.Archive)
		}
		return err
	}
	t.Cleanup(func() { receiptSyncDir = saved })
	out, code := retiredRequest(t, f, requestRun)
	if !armed || code != exitUnknown || !strings.Contains(out, "TriggeredBy") {
		t.Fatalf("completion did not validate existing local state after receipt: %s", out)
	}
	if _, err := os.Lstat(state.DirectoryLockPath(j.Archive)); !os.IsNotExist(err) {
		t.Fatalf("completion created an archive lock: %v", err)
	}
	if read := readEndpointReceipt(path); read.presence != receiptPresent {
		t.Fatalf("watcher was not introduced after receipt publication: %+v", read)
	}
	if requireRetireJournal(t).RowDone {
		t.Fatal("local-open refusal acknowledged the row")
	}
}

func TestRetirementCommandRecordsAdmissionForEveryStep(t *testing.T) {
	for _, retained := range []bool{false, true} {
		name := "server-only"
		if retained {
			name = "retained"
		}
		t.Run(name, func(t *testing.T) {
			f := newRequestFixture(t)
			if retained {
				f.retainANode(t)
				useEndpointReceipt(t)
				f.svc.onStart = func(unit string) {
					if unit == nodeUnit {
						restartedNode(t, f, useRegistrationRecord(t), retainedEndpoint)
					}
				}
			}
			f.reserve(t)
			saved, savedPublish := retireMutationEvent, retirement.Publishing
			last, lastPath := "", ""
			steps := make(map[string]int)
			waits := make(map[string]int)
			publications := make(map[string]int)
			submitted := 0
			f.manager.onSubmit = func(command string) {
				if last != "service-operation" || lastPath != command {
					t.Fatalf("command submission %s followed %s %s", command, last, lastPath)
				}
				submitted++
				// Consume the boundary: a second command needs its own admission.
				last, lastPath = "submitted", command
			}
			retireMutationEvent = func(event, path string) {
				switch event {
				case "admission":
				case "wait":
					waits[path]++
				default:
					steps[event]++
					if last != "admission" {
						t.Fatalf("step %s %s followed %s %s instead of current admission", event, path, last, lastPath)
					}
				}
				last, lastPath = event, path
			}
			// Observe the writer independently: deleting a command boundary or
			// adding an unrecorded publication must fail too.
			retirement.Publishing = func(path string) error {
				if lastPath != path || (last != "journal" && last != "status" && last != "stage" &&
					last != "acknowledgement" && last != "settlement") {
					t.Fatalf("publication %s has no recorded step: %s %s", path, last, lastPath)
				}
				publications[path]++
				if savedPublish != nil {
					return savedPublish(path)
				}
				return nil
			}
			t.Cleanup(func() { retireMutationEvent, retirement.Publishing = saved, savedPublish })
			var out string
			var code int
			if retained {
				out, code = f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
			} else {
				out, code = f.request(t, f.input(t, nil))
			}
			retiredAnswer(t, out, code)
			wantSubmitted := 6
			if retained {
				wantSubmitted = 9
			}
			if submitted != wantSubmitted || steps["service-operation"] != submitted {
				t.Fatalf("service boundaries=%d submissions=%d want=%d", steps["service-operation"], submitted, wantSubmitted)
			}
			for _, event := range []string{"transaction-lock", "global-lock", "identity-lock", "ledger-preparation",
				"ledger-handback", "identity-handback", "retired-directory", "marker", "journal", "status",
				"row-intent", "lifecycle-lock", "service-operation", "archive", "directory-flush", "rewrite",
				"completion-preparation", "row-completion", "acknowledgement", "settlement"} {
				if steps[event] == 0 {
					t.Errorf("full retirement omitted step %s: %v", event, steps)
				}
			}
			for _, event := range []string{"stage", "receipt", "rewrite-rename"} {
				if (steps[event] != 0) != retained {
					t.Errorf("retained=%v step %s count=%d", retained, event, steps[event])
				}
			}
			for _, wait := range []string{"transaction lock", "global lock", "identity lock", "ledger open", "lifecycle lock", "backup", "completion ledger open"} {
				if waits[wait] == 0 {
					t.Errorf("full retirement omitted wait boundary %s", wait)
				}
			}
			if retained && (waits["receipt registration"] == 0 || waits["configuration flush"] == 0) {
				t.Fatalf("retained wait boundaries missing: %v", waits)
			}
			if publications[retirement.JournalPath()] < 5 || publications[retirement.StatusPath()] != 3 || !requireRetireJournal(t).Settled {
				t.Fatalf("incomplete retirement: publications=%v %s", publications, out)
			}
		})
	}
}
