package host

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
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
		{"a process that cannot be identified under the unit from before", func(t *testing.T, f *endpointFixture) {
			t.Helper()
			f.rBinary(t)
			f.legacyRuntimeDirectory(t)
			writeFile(t, filepath.Join(f.procDir, strconv.Itoa(inspectPID), "image"), "#!/bin/sh\nexit 0\n", 0o755)
		}, true, "", outcomeUnknown, endpointReasonRecord},
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

			if tc.record == recordUnitCannotHold && o.doc["effective"] != nil {
				t.Errorf("a unit that cannot hold the record was reported effective=%v; the endpoint it dials is "+
					"unknown", o.doc["effective"])
			}
		})
	}
}

// THE VINTAGE PROBE RECOGNISES WHAT THE PREPARATION ANSWERS: every committed
// answer of `converge-guard prepare` (a report at 0, a refusal at 2) and the
// dry run's own output over a host with no guard and one with a guard held,
// in-process, so a change to the answer's shape that would make every R node
// could-not-tell fails here; and what is not that answer (another exit, no
// outcome, an outcome the preparation does not give, the shell's own error
// text) is never taken for it.
func TestTheVintageProbeRecognisesThePreparationsAnswer(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "ansible_collections", "junioryono", "billet", "tests", "fixtures",
		"guard-prepare")

	for pattern, rc := range map[string]int{"dry-run-*.json": 0, "refused-*.json": exitRefused} {
		fixtures, err := filepath.Glob(filepath.Join(dir, pattern))
		mustOK(t, err)

		if len(fixtures) == 0 {
			t.Fatalf("no committed answers %s under %s", pattern, dir)
		}

		for _, path := range fixtures {
			body, err := os.ReadFile(path)
			mustOK(t, err)

			if !guardAnswered(rc, body) {
				t.Errorf("the committed answer %s at exit %d is not recognised", filepath.Base(path), rc)
			}
		}
	}

	f := newGuardFixture(t)
	managedScript(t, f, "v0.11.0")

	dryRun := func(t *testing.T) (int, []byte) {
		t.Helper()

		var err error

		out := capture(t, func() { err = guardRun(t, "prepare", "--json", "--dry-run") })

		code := 0
		if err != nil {
			code = cli.ExitStatus(err)
		}

		return code, []byte(out)
	}

	if rc, out := dryRun(t); !guardAnswered(rc, out) {
		t.Errorf("the dry run over no guard (exit %d) is not recognised: %s", rc, out)
	}

	mustHold(t, "ci-1")

	if rc, out := dryRun(t); !guardAnswered(rc, out) {
		t.Errorf("the dry run over a held guard (exit %d) is not recognised: %s", rc, out)
	}

	// THE PREPARATION'S COULD-NOT-TELL is its answer too, at its own status.
	if !guardAnswered(exitUnknown, []byte(`{"outcome":"unknown","reason":"unreadable"}`)) {
		t.Errorf("the preparation's could-not-tell at exit %d is not recognised", exitUnknown)
	}

	for name, tc := range map[string]struct {
		rc  int
		out string
	}{
		"another exit":         {1, `{"outcome":"reported"}`},
		"no outcome":           {0, `{"shape":"none"}`},
		"another outcome":      {0, `{"outcome":"acquired"}`},
		"not an object":        {0, `"reported"`},
		"an unknown command":   {2, "billet: unknown command \"converge-guard\"\n"},
		"nothing printed":      {0, ""},
		"two objects":          {0, `{"outcome":"reported"}{"outcome":"reported"}`},
		"an outcome not words": {0, `{"outcome":null}`},
	} {
		if guardAnswered(tc.rc, []byte(tc.out)) {
			t.Errorf("%s (exit %d, %q) was taken for the preparation's answer", name, tc.rc, tc.out)
		}
	}
}
