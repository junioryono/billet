package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The first code review of the preparation found five windows; each is a
// fixture here, written against the wrong implementation it refuses.

// A candidate whose `version` cannot be read is could-not-tell, never a
// candidate with no version that the downgrade judgement then waves through.
func TestACandidateWhoseReleaseCannotBeReadIsNotJudged(t *testing.T) {
	f := newGuardFixture(t)
	managedScript(t, f, "v0.10.1")
	mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)

	body := "#!/bin/sh\ncase \"$1\" in\n  version) echo broken >&2; exit 1;;\n" +
		"  converge-guard) printf '{\"active\": \"none\"}\\n';;\nesac\nexit 0\n"
	cand := stageGuardCandidate(t, f, "recovery-20260909T120000-0badcafe", []byte(body))

	o := runPrepare(t, "--holder", "ci-1", "--candidate", cand)
	mustOutcome(t, o, prepareUnknown)

	if !strings.Contains(o.str("why"), "release could not be read") {
		t.Errorf("why %q", o.str("why"))
	}

	if got := f.record(t).ReleaseExecutable; got != f.binary {
		t.Errorf("the record was re-bound to %s under an unread version", got)
	}

	// A development build that names no release is admitted with no
	// downgrade judged, because there is nothing to compare.
	body = "#!/bin/sh\ncase \"$1\" in\n  version) echo \"billet (devel) linux/amd64\";;\n" +
		"  converge-guard) printf '{\"active\": \"none\"}\\n';;\nesac\nexit 0\n"
	cand = stageGuardCandidate(t, f, "recovery-20260909T120000-1badcafe", []byte(body))

	o = runPrepare(t, "--holder", "ci-1", "--candidate", cand)
	mustOutcome(t, o, prepareRebound)

	if o.boolean("downgrade") {
		t.Error("a build with no release was judged a downgrade")
	}
}

// The candidate judged is the candidate recorded: bytes replaced after the
// probes ran and before the record is rewritten are refused.
func TestTheCandidateJudgedIsTheCandidateRecorded(t *testing.T) {
	f := newGuardFixture(t)
	managedScript(t, f, "v0.10.0")
	mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)

	cand := guardCandidateScript(t, f, "recovery-20260909T120000-0badcafe", "v0.10.1", "capable")

	hashes := 0

	guardHook = func(op guardOp) error {
		if op.Kind == "hash" && op.Path == cand {
			hashes++
			// The probes ran between the judgement's hash and the recording
			// hash; the bytes change as the recording hash is about to read.
			if hashes == 2 {
				mustOK(t, os.WriteFile(cand, []byte("#!/bin/sh\nexit 0\n"), 0o755))
			}
		}

		return nil
	}

	t.Cleanup(func() { guardHook = nil })

	o := runPrepare(t, "--holder", "ci-1", "--candidate", cand)
	mustRefusal(t, o, reasonCandidate)

	if !strings.Contains(o.str("why"), "changed while it was being judged") {
		t.Errorf("why %q", o.str("why"))
	}

	if hashes < 2 {
		t.Fatalf("the candidate was hashed %d times, want the judgement's and the recording's", hashes)
	}

	if got := f.record(t).ReleaseExecutable; got != f.binary {
		t.Errorf("the record names %s after a refused re-binding", got)
	}
}

// The pointer's target is its canonical absolute spelling, opened by name
// under the root without following anything and judged owned.
func TestThePointerTargetIsCanonicalAndOwned(t *testing.T) {
	const name = "recovery-20260909T120000-0badcafe"

	for _, c := range []struct {
		name  string
		plant func(t *testing.T, f *guardFixture) string
		want  string
	}{
		{"a target spelled through ..", func(t *testing.T, f *guardFixture) string {
			t.Helper()
			mustOK(t, os.Mkdir(filepath.Join(f.root, name), 0o700))

			// Concatenated, not joined: filepath.Join would clean the `..` away.
			return f.root + "/hop/../" + name
		}, "not a canonical absolute path"},
		{"a relative target", func(t *testing.T, f *guardFixture) string {
			t.Helper()
			mustOK(t, os.Mkdir(filepath.Join(f.root, name), 0o700))

			return name
		}, "not a canonical absolute path"},
		{"a target that is a symlink to a directory", func(t *testing.T, f *guardFixture) string {
			t.Helper()
			elsewhere := t.TempDir()
			mustOK(t, os.Symlink(elsewhere, filepath.Join(f.root, name)))

			return filepath.Join(f.root, name)
		}, "which is a symlink"},
		{"a target writable by others", func(t *testing.T, f *guardFixture) string {
			t.Helper()
			mustOK(t, os.Mkdir(filepath.Join(f.root, name), 0o777))

			return filepath.Join(f.root, name)
		}, "writable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newGuardFixture(t)
			mustHold(t, "ci-1")
			target := c.plant(t, f)
			mustOK(t, os.Symlink(target, filepath.Join(f.active(), guardPointerName)))

			o := runPrepare(t, "--holder", "ci-1", "--validate")

			if o.str("outcome") != prepareRefused {
				t.Fatalf("outcome %q, want refused: %v", o.str("outcome"), o.doc)
			}

			if !strings.Contains(o.str("why"), c.want) {
				t.Errorf("why %q, want %q", o.str("why"), c.want)
			}
		})
	}
}

// Adoption requires the five members every record has, typed; an `id`
// that is present but not 32 hex characters (null included) is malformed
// rather than a record from before the protocol.
func TestAdoptionRequiresTheFiveMembers(t *testing.T) {
	for _, c := range []struct {
		name   string
		record string
		want   string
	}{
		{"no claimed_at", `{"holder":"ci-1","hostname":"h","release_executable":"%s","release_executable_sha256":"%s"}`, "lacks claimed_at"},
		{"a claimed_at that is not a time", `{"holder":"ci-1","claimed_at":"yesterday","hostname":"h","release_executable":"%s","release_executable_sha256":"%s"}`, "not a time"},
		{"a null id", `{"holder":"ci-1","claimed_at":"2026-09-09T12:00:00Z","hostname":"h","release_executable":"%s","release_executable_sha256":"%s","id":null}`, "id is not 32 hex"},
		{"a relative executable", `{"holder":"ci-1","claimed_at":"2026-09-09T12:00:00Z","hostname":"h","release_executable":"billet","release_executable_sha256":"%s"}`, "not an absolute path"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newGuardFixture(t)
			mustHold(t, "ci-1")

			var body string
			if strings.Count(c.record, "%s") == 2 {
				body = strings.Replace(strings.Replace(c.record, "%s", f.binary, 1), "%s", f.binarySHA, 1)
			} else {
				body = strings.Replace(c.record, "%s", f.binarySHA, 1)
			}

			path := filepath.Join(f.active(), guardRecordName)
			mustOK(t, os.WriteFile(path, []byte(body), 0o600))

			o := runPrepare(t, "--holder", "ci-1", "--validate")
			mustRefusal(t, o, reasonRecord)

			if !strings.Contains(o.str("why"), c.want) {
				t.Errorf("why %q, want %q", o.str("why"), c.want)
			}

			after, err := os.ReadFile(path)
			mustOK(t, err)

			if string(after) != body {
				t.Errorf("the record was rewritten under a refusal:\n%s", after)
			}

			if strings.Contains(o.str("why"), "adopted") || o.boolean("adopted") {
				t.Error("a malformed record was adopted")
			}
		})
	}
}

// A dry run over a guard whose pointer cannot be followed reports the
// pointer as present with its problem, never as a guard with no transaction.
func TestADryRunReportsABrokenPointer(t *testing.T) {
	f := newGuardFixture(t)
	mustHold(t, "ci-1")
	mustOK(t, os.Symlink(filepath.Join(f.root, "recovery-20260909T120000-0badcafe"), filepath.Join(f.active(), guardPointerName)))

	o := runPrepare(t, "--dry-run")
	mustOutcome(t, o, prepareReported)

	guard := asMap(o.doc["guard"])
	if guard == nil {
		t.Fatalf("no guard in %v", o.doc)
	}

	if !asBool(guard["pointer"]) {
		t.Error("a dangling pointer was reported absent")
	}

	problem := asString(guard["pointer_problem"])
	if !strings.Contains(problem, "does not exist") {
		t.Errorf("pointer_problem %q", problem)
	}

	if _, ok := guard["pointer_target"]; ok {
		t.Error("a dangling pointer was given a target")
	}
}
