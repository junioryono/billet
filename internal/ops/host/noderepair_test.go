package host

import (
	"os"
	"strings"
	"testing"
)

// A RUNNING NODE WITHOUT A RECORD IS JUDGED BY WHAT ITS UNIT POSITIVELY SAYS
// (#168). A process positively before the guard is still `absent-pre-r`,
// whatever its unit; an R process whose loaded unit declares no directory for
// the record is `unit-cannot-hold` to the dry run and refused, naming the
// repair, to the action; and an R process whose unit declares the directory,
// or whose RuntimeDirectory systemd did not answer, is still could-not-tell,
// never a unit to repair.
func TestTheDecisionNamesAUnitThatCannotHoldTheRecord(t *testing.T) {
	cases := []struct {
		name    string
		stage   func(t *testing.T, f *endpointFixture)
		dryRun  bool
		record  string // the dry run's record kind, or "" for a refusal
		outcome string
		reason  string
	}{
		{"the unit from before, the dry run", func(t *testing.T, f *endpointFixture) {
			t.Helper()
			f.rBinary(t)
			f.legacyRuntimeDirectory(t)
		}, true, recordUnitCannotHold, "", ""},
		{"the unit from before, the action", func(t *testing.T, f *endpointFixture) {
			t.Helper()
			f.rBinary(t)
			f.legacyRuntimeDirectory(t)
		}, false, "", outcomeRefused, endpointReasonRepair},
		{"a process before the guard under the unit from before", func(t *testing.T, f *endpointFixture) {
			t.Helper()
			f.preRBinary(t)
			f.legacyRuntimeDirectory(t)
		}, true, recordAbsentPreR, "", ""},
		{"the unit declares the directory", func(t *testing.T, f *endpointFixture) {
			t.Helper()
			f.rBinary(t)
		}, true, "", outcomeUnknown, endpointReasonRecord},
		{"systemd did not answer RuntimeDirectory", func(t *testing.T, f *endpointFixture) {
			t.Helper()
			f.rBinary(t)
			path := f.unitFile(nodeUnit)

			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			unanswered := strings.Replace(string(body), "RuntimeDirectory=billet/locks billet/registration\n", "", 1)
			if unanswered == string(body) {
				t.Fatal("the node unit fixture declares no RuntimeDirectory to withhold")
			}

			writeFile(t, path, unanswered, 0o644)
		}, true, "", outcomeUnknown, endpointReasonRecord},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEndpointFixture(t)
			f.wait = endpointExpiryWait
			tc.stage(t, f)
			mustOK(t, os.Remove(f.recordPath))

			var extra []string
			if tc.dryRun {
				extra = []string{"--dry-run"}
			}

			o := f.migrate(t, f.rendering(endpointA), extra...)

			if tc.record == "" {
				mustEndpointRefusal(t, o, tc.outcome, tc.reason)

				if tc.reason == endpointReasonRepair && !strings.Contains(o.str("next"), "billet node repair-unit") {
					t.Errorf("the refusal's next step is %q, want the repair named", o.str("next"))
				}

				return
			}

			if o.code != 0 || o.str("outcome") != outcomeReported || o.str("record") != tc.record {
				t.Fatalf("code %d outcome %q record %q (why %q), want a report of %q", o.code, o.str("outcome"),
					o.str("record"), o.str("why"), tc.record)
			}

			if tc.record == recordUnitCannotHold && (o.boolean("planned") || o.doc["effective"] != nil) {
				t.Errorf("a unit that cannot hold the record was reported planned=%v effective=%v; the endpoint it "+
					"dials is unknown", o.boolean("planned"), o.doc["effective"])
			}
		})
	}
}
