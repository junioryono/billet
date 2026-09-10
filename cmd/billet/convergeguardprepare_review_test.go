package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
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
			// Past the umask: the mode the trust rule sees is the one set.
			mustOK(t, os.Chmod(filepath.Join(f.root, name), 0o777))

			return filepath.Join(f.root, name)
		}, "is mode 0777, want 0700"},
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
		{"a null id", `{"holder":"ci-1","claimed_at":"2026-09-09T12:00:00Z","hostname":"h","release_executable":"%s","release_executable_sha256":"%s","id":null}`, "id is null"},
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

// A managed path that cannot be examined is could-not-tell for the
// downgrade; only a positively absent one has nothing to compare.
func TestAnUnexaminableManagedPathJudgesNoDowngrade(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root examines everything")
	}

	f := newGuardFixture(t)
	managedScript(t, f, "v0.10.1")
	mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)

	// The record names the candidate, so the recorded executable verifies
	// and the judgement reaches the managed binary's part.
	cand := guardCandidateScript(t, f, "recovery-20260909T120000-0badcafe", "v0.9.0", "capable")
	mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--candidate", cand, "--allow-downgrade"), prepareRebound)

	// The managed binary's directory is closed, so its lstat is EACCES: the
	// path is neither present nor absent.
	parent := filepath.Dir(f.binary)
	mustOK(t, os.Chmod(parent, 0))
	t.Cleanup(func() {
		if err := os.Chmod(parent, 0o755); err != nil {
			t.Errorf("reopen %s: %v", parent, err)
		}
	})

	o := runPrepare(t, "--holder", "ci-1", "--candidate", cand)
	mustOutcome(t, o, prepareUnknown)

	if !strings.Contains(o.str("why"), "could not be examined") {
		t.Errorf("why %q", o.str("why"))
	}
}

// The record is read as a token stream: a null value, a repeated member and
// a case-aliased member are malformed, as the Python reader has them.
func TestARecordIsReadAsTheCommandWritesIt(t *testing.T) {
	for _, c := range []struct{ name, extra, want string }{
		{"a null preparing", `,"preparing":null`, "preparing is null"},
		{"a preparing that is not a boolean", `,"preparing":"yes"`, "preparing is not a boolean"},
		{"a repeated id", `,"id":"0123456789abcdef0123456789abcdef","id":null`, "repeats id"},
		{"a case-aliased member", `,"Token":"0123456789abcdef0123456789abcdef"`, "does not write: Token"},
		{"bytes after the object", `}{`, "bytes after its object"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newGuardFixture(t)
			mustHold(t, "ci-1")

			body := `{"holder":"ci-1","claimed_at":"2026-09-09T12:00:00Z","hostname":"h","release_executable":"` + f.binary +
				`","release_executable_sha256":"` + f.binarySHA + `"` + c.extra + `}`
			mustOK(t, os.WriteFile(filepath.Join(f.active(), guardRecordName), []byte(body), 0o600))

			o := runPrepare(t, "--holder", "ci-1", "--validate")
			mustRefusal(t, o, reasonRecord)

			if !strings.Contains(o.str("why"), c.want) {
				t.Errorf("why %q, want %q", o.str("why"), c.want)
			}
		})
	}
}

// A dry run over a guard whose pointer cannot be followed reports the
// pointer as present with its problem, never as a guard with no transaction.
// A stray `guard.json.tmp` is removed only when it is the leftover a rewrite
// leaves: a regular file the trust boundary accepts with one link. One
// another account could have written, or one that is another name of the
// record, refuses at the preparation, the settlement and the release, and
// survives each refusal untouched.
func TestAStrayTemporaryIsJudgedBeforeItIsRemoved(t *testing.T) {
	for _, c := range []struct {
		name  string
		plant func(t *testing.T, f *guardFixture)
	}{
		{"writable by others", func(t *testing.T, f *guardFixture) {
			t.Helper()
			mustOK(t, os.WriteFile(filepath.Join(f.active(), guardTmpName), []byte("{"), 0o600))
			mustOK(t, os.Chmod(filepath.Join(f.active(), guardTmpName), 0o666))
		}},
		{"another name of the record", func(t *testing.T, f *guardFixture) {
			t.Helper()
			mustOK(t, os.Link(filepath.Join(f.active(), guardRecordName), filepath.Join(f.active(), guardTmpName)))
		}},
		{"a symlink", func(t *testing.T, f *guardFixture) {
			t.Helper()
			mustOK(t, os.Symlink(filepath.Join(f.active(), guardRecordName), filepath.Join(f.active(), guardTmpName)))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newGuardFixture(t)
			first := runPrepare(t, "--holder", "ci-1", "--validate")
			mustOutcome(t, first, prepareAcquired)
			token := first.str("token")

			tmp := filepath.Join(f.active(), guardTmpName)
			c.plant(t, f)

			before, err := os.Lstat(tmp)
			mustOK(t, err)

			recordBefore, err := os.ReadFile(filepath.Join(f.active(), guardRecordName))
			mustOK(t, err)

			o := runPrepare(t, "--holder", "ci-1", "--validate")
			mustRefusal(t, o, reasonStray)

			if !strings.Contains(o.str("why"), "not the leftover a rewrite leaves") {
				t.Errorf("why %q does not say what the temporary is not", o.str("why"))
			}

			if err := guardRun(t, "settle", "--holder", "ci-1", "--token", token); err == nil ||
				!strings.Contains(err.Error(), "not the leftover a rewrite leaves") {
				t.Errorf("the settlement answered %v, want the temporary's refusal", err)
			}

			if err := guardRun(t, "release", "--holder", "ci-1", "--cleanup", "--token", token); err == nil ||
				!strings.Contains(err.Error(), "not the leftover a rewrite leaves") {
				t.Errorf("the cleanup answered %v, want the temporary's refusal", err)
			}

			after, err := os.Lstat(tmp)
			if err != nil {
				t.Fatalf("the temporary was removed: %v", err)
			}

			if !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Error("the temporary was replaced or changed by a refusal")
			}

			recordAfter, err := os.ReadFile(filepath.Join(f.active(), guardRecordName))
			mustOK(t, err)

			if !bytes.Equal(recordBefore, recordAfter) {
				t.Error("the record changed under a refusal")
			}
		})
	}
}

// A takeover retires the acquirer's window with the holder: the token and the
// preparing flag are the acquiring invocation's, and the new holder never
// acquired, so the old token releases nothing once the pointer is gone.
func TestATakeoverRetiresTheAcquirersWindow(t *testing.T) {
	f := newGuardFixture(t)

	first := runPrepare(t, "--holder", "ci-1", "--validate")
	mustOutcome(t, first, prepareAcquired)
	token := first.str("token")

	before := f.record(t)
	if before.Token == "" || !before.Preparing {
		t.Fatalf("the acquired record carries no window: %+v", before)
	}

	recovery := filepath.Join(f.root, "recovery-20260909T120000-0badcafe")
	mustOK(t, os.Mkdir(recovery, 0o700))
	writeJournalFixture(t, recovery, "installed")
	mustOK(t, os.Symlink(recovery, filepath.Join(f.active(), guardPointerName)))
	cleanScan(t)

	if err := guardRun(t, "hold", "--holder", "ci-2", "--recover-from", "ci-1", "--old-driver-stopped"); err != nil {
		t.Fatalf("the takeover: %v", err)
	}

	after := f.record(t)

	switch {
	case after.Holder != "ci-2":
		t.Errorf("holder %q after the takeover", after.Holder)
	case after.Token != "" || after.Preparing:
		t.Errorf("the takeover kept the acquirer's window: token %q, preparing %v", after.Token, after.Preparing)
	case after.ID != before.ID:
		t.Errorf("the takeover changed the id: %q, then %q", before.ID, after.ID)
	}

	if _, err := os.Lstat(filepath.Join(f.active(), guardPointerName)); err != nil {
		t.Errorf("the pointer after the takeover: %v", err)
	}

	mustOK(t, os.Remove(filepath.Join(f.active(), guardPointerName)))

	for _, holder := range []string{"ci-1", "ci-2"} {
		err := guardRun(t, "release", "--holder", holder, "--cleanup", "--token", token)
		if err == nil {
			t.Errorf("the old token released the guard for %s after the takeover", holder)
		}
	}

	if _, err := os.Lstat(filepath.Join(f.active(), guardRecordName)); err != nil {
		t.Errorf("the record after the refused cleanups: %v", err)
	}
}

// A candidate whose status answer overflows the bound is not capable: what
// was kept is a prefix, and a valid object followed by padding past the bound
// and garbage would otherwise read as one complete object.
func TestACandidateWhoseAnswerOverflowsTheBoundIsNotCapable(t *testing.T) {
	f := newGuardFixture(t)
	managedScript(t, f, "v0.10.1")
	mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)

	body := "#!/bin/sh\ncase \"$1\" in\n  version) echo \"billet v0.10.2 linux/amd64\";;\n" +
		"  converge-guard) printf '{\"active\": \"none\"}'; head -c " + strconv.Itoa(maxCommandOutput+16) +
		" /dev/zero | tr '\\0' ' '; printf 'garbage\\n';;\nesac\nexit 0\n"
	cand := stageGuardCandidate(t, f, "recovery-20260909T120000-0badcafe", []byte(body))

	o := runPrepare(t, "--holder", "ci-1", "--candidate", cand)
	mustRefusal(t, o, reasonFloor)

	if !strings.Contains(o.str("why"), "not read whole") {
		t.Errorf("why %q does not name the overflow", o.str("why"))
	}
}

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
