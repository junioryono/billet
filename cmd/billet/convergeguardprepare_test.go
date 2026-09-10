package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The fixtures of `converge-guard prepare`, `settle` and `release --cleanup`
// (scratchpad/pr6b/fixtures-c4a2.md, section A): each names the wrong
// implementation it is written against, and every answer is read back as the
// JSON the role parses.

// prepareOut is a prepare answer as a test reads it: the parsed JSON and the
// exit status the command carried.
type prepareOut struct {
	doc  map[string]any
	code int
	err  error
}

func (o prepareOut) str(key string) string { return asString(o.doc[key]) }

func (o prepareOut) boolean(key string) bool { return asBool(o.doc[key]) }

func (o prepareOut) record() map[string]any { return asMap(o.doc["record"]) }

// The typed reads of a parsed answer: a member of another type reads as the
// zero value, which the assertions then refuse.
func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}

	return ""
}

func asBool(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}

	return false
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}

	return nil
}

// runPrepare runs `converge-guard prepare` with the arguments given, capturing
// its answer.
func runPrepare(t *testing.T, args ...string) prepareOut {
	t.Helper()

	var err error

	out := capture(t, func() { err = guardRun(t, append([]string{"prepare", "--json"}, args...)...) })

	o := prepareOut{err: err}
	if err != nil {
		o.code = exitStatus(err)
	}

	if strings.TrimSpace(out) == "" {
		if err == nil {
			t.Fatalf("prepare %v printed nothing and returned nil", args)
		}

		return o
	}

	if uerr := json.Unmarshal([]byte(out), &o.doc); uerr != nil {
		t.Fatalf("prepare %v printed something that is not JSON: %v\n%s", args, uerr, out)
	}

	return o
}

func mustOutcome(t *testing.T, o prepareOut, outcome string) {
	t.Helper()

	if o.str("outcome") != outcome {
		t.Fatalf("outcome %q (reason %q, why %q, err %v), want %q", o.str("outcome"), o.str("reason"),
			o.str("why"), o.err, outcome)
	}
}

func mustRefusal(t *testing.T, o prepareOut, reason string) {
	t.Helper()

	if o.str("outcome") != prepareRefused || o.str("reason") != reason || o.code != exitRefused {
		t.Fatalf("outcome %q reason %q code %d (why %q), want refused/%s/%d", o.str("outcome"), o.str("reason"),
			o.code, o.str("why"), reason, exitRefused)
	}
}

// candidateScript writes a candidate answering `version` and `converge-guard
// status --json` the way a capable release does (the status from the
// committed corpus's `none` shape), or as a pre-R release, or one that hangs.
func guardCandidateScript(t *testing.T, f *guardFixture, dir, version, mode string) string {
	t.Helper()

	var body string

	switch mode {
	case "capable":
		body = "#!/bin/sh\ncase \"$1\" in\n  version) echo \"billet " + version + " linux/amd64\";;\n" +
			"  converge-guard) printf '{\"active\": \"none\"}\\n';;\nesac\nexit 0\n"
	case "pre-r":
		body = "#!/bin/sh\ncase \"$1\" in\n  version) echo \"billet " + version + " linux/amd64\"; exit 0;;\nesac\n" +
			"echo 'unknown command \"converge-guard\"' >&2\nexit 2\n"
	case "hang":
		body = "#!/bin/sh\nsleep 30\n"
	case "garbage":
		body = "#!/bin/sh\ncase \"$1\" in\n  version) echo \"billet " + version + " linux/amd64\";;\n" +
			"  converge-guard) echo '{\"active\": \"held\"}';;\nesac\nexit 0\n"
	default:
		t.Fatalf("unknown candidate mode %s", mode)
	}

	return stageGuardCandidate(t, f, dir, []byte(body))
}

// managedScript replaces the fixture's managed binary with one answering
// `version`.
func managedScript(t *testing.T, f *guardFixture, version string) {
	t.Helper()

	body := "#!/bin/sh\ncase \"$1\" in\n  version) echo \"billet " + version + " linux/amd64\";;\n" +
		"  converge-guard) printf '{\"active\": \"none\"}\\n';;\nesac\nexit 0\n"

	mustOK(t, os.WriteFile(f.binary, []byte(body), 0o755))

	sum, _, err := hashRegular(f.binary, maxExecutableBytes)
	mustOK(t, err)

	f.binarySHA = sum
}

// A1: `--validate` over none acquires, with the id, the token, the flag and
// the publication's order; a dangling or absent managed binary refuses by
// what it is; the entropy's failure refuses before anything is written.
func TestPrepareValidateAcquiresAndAnswers(t *testing.T) {
	f := newGuardFixture(t)

	var ops []string

	guardHook = func(op guardOp) error {
		ops = append(ops, op.Kind+" "+strings.TrimPrefix(op.Path, f.parent+"/"))

		return nil
	}

	o := runPrepare(t, "--holder", "ci-1", "--validate")

	guardHook = nil

	mustOutcome(t, o, prepareAcquired)

	rec := f.record(t)

	switch {
	case !guardHex32.MatchString(o.str("id")) || o.str("id") != rec.ID:
		t.Errorf("id %q, record %q", o.str("id"), rec.ID)
	case !guardHex32.MatchString(o.str("token")) || o.str("token") != rec.Token:
		t.Errorf("token %q, record %q", o.str("token"), rec.Token)
	case !o.boolean("preparing") || !rec.Preparing:
		t.Errorf("preparing %v, record %v", o.boolean("preparing"), rec.Preparing)
	case o.boolean("pointer") || o.boolean("adopted"):
		t.Errorf("pointer %v adopted %v on a fresh acquisition", o.boolean("pointer"), o.boolean("adopted"))
	case o.record()["release_executable"] != f.binary || o.record()["release_executable_sha256"] != f.binarySHA:
		t.Errorf("record %v", o.record())
	case o.str("holder") != "ci-1" || o.str("claimed_at") != "2026-09-09T12:00:00Z" || o.str("hostname") != "billet-control-01":
		t.Errorf("answer %v", o.doc)
	}

	managed := asMap(o.doc["managed"])
	if managed["present"] != true || managed["sha256"] != f.binarySHA || managed["path"] != f.binary {
		t.Errorf("managed %v", managed)
	}

	// The publication's order is the hold's: the lock, the managed binary
	// hashed, the guard made, the record written and flushed, the root flushed.
	wantSeq := []string{"mkdir upgrades/active", "create upgrades/active/guard.json.tmp",
		"fsync upgrades/active/guard.json.tmp", "rename upgrades/active/guard.json", "fsync upgrades/active",
		"fsync upgrades", "unlock upgrades/transaction.lock"}

	var got []string

	for _, op := range ops {
		if slices.Contains(wantSeq, op) {
			got = append(got, op)
		}
	}

	if !slices.Equal(got, wantSeq) {
		t.Errorf("the publication's operations:\n got %q\nwant %q", got, wantSeq)
	}

	// THE TOKEN IS PRINTED BY NOTHING ELSE.
	status := capture(t, func() { mustOK(t, guardRun(t, "status", "--json")) })
	if strings.Contains(status, rec.Token) {
		t.Error("status printed the token")
	}

	if !strings.Contains(status, `"id": "`+rec.ID+`"`) || !strings.Contains(status, `"preparing": true`) {
		t.Errorf("status lacks the id or the flag:\n%s", status)
	}

	// A second --validate under the same holder validates and prints no token.
	o = runPrepare(t, "--holder", "ci-1", "--validate")
	mustOutcome(t, o, prepareValidated)

	if o.str("token") != "" || o.str("id") != rec.ID || !o.boolean("preparing") {
		t.Errorf("the validation's answer %v", o.doc)
	}
}

func TestPrepareValidateRefusesWhatItCannotHoldThrough(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		f := newGuardFixture(t)
		mustOK(t, os.Remove(f.binary))

		o := runPrepare(t, "--holder", "ci-1", "--validate")
		mustRefusal(t, o, reasonManaged)

		if !strings.Contains(o.str("why"), "absent") {
			t.Errorf("why %q", o.str("why"))
		}

		if _, err := os.Lstat(f.active()); !errors.Is(err, fs.ErrNotExist) {
			t.Error("a refused acquisition left active")
		}
	})

	t.Run("dangling link", func(t *testing.T) {
		f := newGuardFixture(t)
		mustOK(t, os.Remove(f.binary))
		mustOK(t, os.Symlink(filepath.Join(f.parent, "nowhere"), f.binary))

		o := runPrepare(t, "--holder", "ci-1", "--validate")
		mustRefusal(t, o, reasonManaged)

		if !strings.Contains(o.str("why"), "symlink") {
			t.Errorf("why %q", o.str("why"))
		}
	})

	t.Run("a directory", func(t *testing.T) {
		f := newGuardFixture(t)
		mustOK(t, os.Remove(f.binary))
		mustOK(t, os.Mkdir(f.binary, 0o755))

		o := runPrepare(t, "--holder", "ci-1", "--validate")
		mustRefusal(t, o, reasonManaged)
	})

	t.Run("no randomness", func(t *testing.T) {
		f := newGuardFixture(t)
		guardRandom = func(int) ([]byte, error) { return nil, errors.New("staged entropy failure") }

		o := runPrepare(t, "--holder", "ci-1", "--validate")
		mustRefusal(t, o, reasonRandomness)

		if _, err := os.Lstat(f.active()); !errors.Is(err, fs.ErrNotExist) {
			t.Error("a refused acquisition left active")
		}
	})
}

// A1b/A3b/A6: the candidate-first acquisition and its bootstrap premise.
func TestPrepareValidateWithACandidateAndTheBootstrapPremise(t *testing.T) {
	t.Run("bootstrap", func(t *testing.T) {
		f := newGuardFixture(t)
		mustOK(t, os.Remove(f.binary))
		mustOK(t, os.Mkdir(f.root, 0o700))
		mustOK(t, os.WriteFile(filepath.Join(f.root, txLockName), nil, 0o600))
		mustOK(t, os.Mkdir(filepath.Join(f.root, "20260908T120000000000000"), 0o700))
		cand := guardCandidateScript(t, f, "recovery-20260909T120000-0badcafe", "v0.10.1", "capable")

		var ops []string

		guardHook = func(op guardOp) error {
			ops = append(ops, op.Kind+" "+strings.TrimPrefix(op.Path, f.parent+"/"))

			return nil
		}

		o := runPrepare(t, "--holder", "ci-1", "--validate", "--candidate", cand, "--expect-bootstrap")

		guardHook = nil

		mustOutcome(t, o, prepareAcquired)

		if o.record()["release_executable"] != cand {
			t.Errorf("record %v", o.record())
		}

		// The candidate's flushes precede the record.
		var seq []string

		for _, op := range ops {
			switch op {
			case "fsync upgrades/recovery-20260909T120000-0badcafe/billet.candidate",
				"fsync upgrades/recovery-20260909T120000-0badcafe", "create upgrades/active/guard.json.tmp":
				seq = append(seq, op)
			}
		}

		if !slices.Equal(seq, []string{"fsync upgrades/recovery-20260909T120000-0badcafe/billet.candidate",
			"fsync upgrades/recovery-20260909T120000-0badcafe", "create upgrades/active/guard.json.tmp"}) {
			t.Errorf("the candidate's flushes against the record: %q", seq)
		}
	})

	t.Run("bootstrap with the managed path present", func(t *testing.T) {
		f := newGuardFixture(t)
		cand := guardCandidateScript(t, f, "recovery-20260909T120000-0badcafe", "v0.10.1", "capable")

		o := runPrepare(t, "--holder", "ci-1", "--validate", "--candidate", cand, "--expect-bootstrap")
		mustRefusal(t, o, reasonBootstrap)

		if _, err := os.Lstat(f.active()); !errors.Is(err, fs.ErrNotExist) {
			t.Error("a refused bootstrap left active")
		}
	})

	t.Run("bootstrap over a stray entry", func(t *testing.T) {
		for _, stray := range []struct{ name, kind string }{
			{"notes", "file"}, {"recovery-20260909T120000-0badcafe", "file"}, {"something", "dir"},
			{"20260908T120000000000000", "link"},
		} {
			f := newGuardFixture(t)
			mustOK(t, os.Remove(f.binary))
			cand := guardCandidateScript(t, f, "recovery-20260909T120000-1badcafe", "v0.10.1", "capable")

			path := filepath.Join(f.root, stray.name)

			switch stray.kind {
			case "file":
				mustOK(t, os.WriteFile(path, []byte("x"), 0o600))
			case "dir":
				mustOK(t, os.Mkdir(path, 0o700))
			case "link":
				mustOK(t, os.Symlink(f.parent, path))
			}

			o := runPrepare(t, "--holder", "ci-1", "--validate", "--candidate", cand, "--expect-bootstrap")
			mustRefusal(t, o, reasonBootstrap)

			if !strings.Contains(o.str("why"), stray.name) {
				t.Errorf("%s: why %q does not name the entry", stray.name, o.str("why"))
			}
		}
	})

	t.Run("pre-R managed binary with a candidate", func(t *testing.T) {
		f := newGuardFixture(t)
		cand := guardCandidateScript(t, f, "recovery-20260909T120000-0badcafe", "v0.10.1", "capable")

		o := runPrepare(t, "--holder", "ci-1", "--validate", "--candidate", cand)
		mustOutcome(t, o, prepareAcquired)

		if o.record()["release_executable"] != cand {
			t.Errorf("record %v", o.record())
		}
	})
}

// A1c: THE POINTER, validated under the lock.
func TestPrepareValidatesThePointer(t *testing.T) {
	plant := func(t *testing.T) *guardFixture {
		t.Helper()

		f := newGuardFixture(t)
		mustOK(t, guardRun(t, "hold", "--holder", "ci-1"))

		return f
	}

	t.Run("a valid pointer of each grammar", func(t *testing.T) {
		for _, name := range []string{"recovery-20260909T120000-0badcafe", "20260909T120000000000000"} {
			f := plant(t)
			target := filepath.Join(f.root, name)
			mustOK(t, os.Mkdir(target, 0o700))
			mustOK(t, os.Symlink(target, filepath.Join(f.active(), guardPointerName)))

			o := runPrepare(t, "--holder", "ci-1", "--validate")
			mustOutcome(t, o, prepareValidated)

			if !o.boolean("pointer") || o.str("pointer_target") != target {
				t.Errorf("%s: pointer %v target %q", name, o.boolean("pointer"), o.str("pointer_target"))
			}

			// --recovery admits it and names the directory; --no-change and
			// --candidate refuse naming --recovery.
			o = runPrepare(t, "--holder", "ci-1", "--recovery")
			mustOutcome(t, o, prepareValidated)

			if o.str("recovery_dir") != target {
				t.Errorf("recovery_dir %q", o.str("recovery_dir"))
			}

			o = runPrepare(t, "--holder", "ci-1", "--no-change")
			mustRefusal(t, o, reasonPointer)

			if !strings.Contains(o.str("next"), "--recovery") {
				t.Errorf("next %q", o.str("next"))
			}
		}
	})

	for _, c := range []struct {
		name  string
		plant func(t *testing.T, f *guardFixture)
		words string
	}{
		{"a regular file", func(t *testing.T, f *guardFixture) {
			t.Helper()
			mustOK(t, os.WriteFile(filepath.Join(f.active(), guardPointerName), []byte("x"), 0o600))
		}, "a regular file"},
		{"a directory", func(t *testing.T, f *guardFixture) {
			t.Helper()
			mustOK(t, os.Mkdir(filepath.Join(f.active(), guardPointerName), 0o700))
		}, "a directory"},
		{"dangling", func(t *testing.T, f *guardFixture) {
			t.Helper()
			mustOK(t, os.Symlink(filepath.Join(f.root, "recovery-20260909T120000-0badcafe"),
				filepath.Join(f.active(), guardPointerName)))
		}, "does not exist"},
		{"outside the root", func(t *testing.T, f *guardFixture) {
			t.Helper()
			mustOK(t, os.Mkdir(filepath.Join(f.parent, "recovery-20260909T120000-0badcafe"), 0o700))
			mustOK(t, os.Symlink(filepath.Join(f.parent, "recovery-20260909T120000-0badcafe"),
				filepath.Join(f.active(), guardPointerName)))
		}, "not a recovery directory directly under"},
		{"outside the grammars", func(t *testing.T, f *guardFixture) {
			t.Helper()
			mustOK(t, os.Mkdir(filepath.Join(f.root, "upgrade-x"), 0o700))
			mustOK(t, os.Symlink(filepath.Join(f.root, "upgrade-x"), filepath.Join(f.active(), guardPointerName)))
		}, "not a recovery directory"},
		{"a target that is a file", func(t *testing.T, f *guardFixture) {
			t.Helper()
			mustOK(t, os.WriteFile(filepath.Join(f.root, "recovery-20260909T120000-0badcafe"), []byte("x"), 0o600))
			mustOK(t, os.Symlink(filepath.Join(f.root, "recovery-20260909T120000-0badcafe"),
				filepath.Join(f.active(), guardPointerName)))
		}, "which is a regular file"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := plant(t)
			c.plant(t, f)

			o := runPrepare(t, "--holder", "ci-1", "--validate")
			mustRefusal(t, o, reasonPointer)

			if !strings.Contains(o.str("why"), c.words) {
				t.Errorf("why %q, want %q", o.str("why"), c.words)
			}

			o = runPrepare(t, "--holder", "ci-1", "--recovery")
			mustRefusal(t, o, reasonPointer)
		})
	}

	t.Run("no pointer refuses --recovery", func(t *testing.T) {
		plant(t)

		o := runPrepare(t, "--holder", "ci-1", "--recovery")
		mustRefusal(t, o, reasonPointer)
	})
}

// A1d/A1e: CONTINUITY AND ADOPTION.
func TestPrepareContinuityAndAdoption(t *testing.T) {
	t.Run("expect-id over none", func(t *testing.T) {
		f := newGuardFixture(t)

		o := runPrepare(t, "--holder", "ci-1", "--validate", "--expect-id", strings.Repeat("ab", 16))
		mustRefusal(t, o, reasonGone)

		if _, err := os.Lstat(f.active()); !errors.Is(err, fs.ErrNotExist) {
			t.Error("a refused continuity acquired a guard")
		}
	})

	t.Run("expect-id over another guard", func(t *testing.T) {
		f := newGuardFixture(t)
		first := runPrepare(t, "--holder", "ci-1", "--validate")
		mustOutcome(t, first, prepareAcquired)

		// Replaced by another guard under the same holder, the same executable,
		// within the same second: only the id tells them apart.
		mustOK(t, guardRun(t, "release", "--holder", "ci-1"))
		second := runPrepare(t, "--holder", "ci-1", "--validate")
		mustOutcome(t, second, prepareAcquired)

		if first.str("id") == second.str("id") {
			t.Fatal("two acquisitions minted one id")
		}

		o := runPrepare(t, "--holder", "ci-1", "--validate", "--expect-id", first.str("id"))
		mustRefusal(t, o, reasonReplaced)

		if f.record(t).ID != second.str("id") {
			t.Error("the continuity refusal touched the record")
		}

		o = runPrepare(t, "--holder", "ci-1", "--validate", "--expect-id", second.str("id"))
		mustOutcome(t, o, prepareValidated)
	})

	t.Run("expect-id over a replaced shape", func(t *testing.T) {
		f := newGuardFixture(t)
		first := runPrepare(t, "--holder", "ci-1", "--validate")
		mustOutcome(t, first, prepareAcquired)
		mustOK(t, guardRun(t, "release", "--holder", "ci-1"))
		mustOK(t, os.WriteFile(f.active(), []byte("legacy"), 0o600))

		o := runPrepare(t, "--holder", "ci-1", "--validate", "--expect-id", first.str("id"))
		mustRefusal(t, o, reasonReplaced)
	})

	t.Run("adoption of a five-member record", func(t *testing.T) {
		f := newGuardFixture(t)
		mustHold(t, "ci-1")

		// A record from before the protocol: the five members only.
		body, err := json.MarshalIndent(map[string]any{
			"holder": "ci-1", "claimed_at": "2026-09-09T11:00:00Z", "hostname": "billet-control-01",
			"release_executable": f.binary, "release_executable_sha256": f.binarySHA,
		}, "", "  ")
		mustOK(t, err)
		mustOK(t, os.WriteFile(filepath.Join(f.active(), guardRecordName), append(body, '\n'), 0o600))

		dry := runPrepare(t, "--dry-run")
		mustOutcome(t, dry, prepareReported)

		guard := asMap(dry.doc["guard"])
		if guard["id"] != nil {
			t.Errorf("a dry run over an unadopted record reports id %v", guard["id"])
		}

		if f.record(t).ID != "" {
			t.Error("a dry run adopted the record")
		}

		o := runPrepare(t, "--holder", "ci-1", "--validate")
		mustOutcome(t, o, prepareValidated)

		rec := f.record(t)

		switch {
		case !o.boolean("adopted"):
			t.Error("adopted is false")
		case !guardHex32.MatchString(rec.ID) || rec.ID != o.str("id"):
			t.Errorf("the adopted record's id %q, answer %q", rec.ID, o.str("id"))
		case rec.Preparing || rec.Token != "" || o.str("token") != "":
			t.Errorf("the adopted record %+v answer token %q", rec, o.str("token"))
		case rec.ClaimedAt != "2026-09-09T11:00:00Z" || rec.ReleaseExecutableSHA256 != f.binarySHA:
			t.Errorf("the adopted record's members moved: %+v", rec)
		}

		// No cleanup is possible over an adopted guard: no token.
		if err := guardRun(t, "release", "--holder", "ci-1", "--cleanup", "--token", strings.Repeat("ab", 16)); err == nil {
			t.Error("a cleanup release over an adopted guard succeeded")
		}

		if _, err := os.Lstat(f.active()); err != nil {
			t.Error("the adopted guard is gone")
		}

		again := runPrepare(t, "--holder", "ci-1", "--validate")
		mustOutcome(t, again, prepareValidated)

		if again.boolean("adopted") || again.str("id") != rec.ID {
			t.Errorf("the second validation %v", again.doc)
		}
	})

	t.Run("hold mints an id", func(t *testing.T) {
		f := newGuardFixture(t)
		mustHold(t, "ci-1")

		rec := f.record(t)
		if !guardHex32.MatchString(rec.ID) || rec.Preparing || rec.Token != "" {
			t.Errorf("hold's record %+v", rec)
		}
	})
}

// A2: THE RECORDED EXECUTABLE IS VERIFIED on every mutating branch and
// reported by the dry run.
func TestPrepareVerifiesTheRecordedExecutable(t *testing.T) {
	f := newGuardFixture(t)
	first := runPrepare(t, "--holder", "ci-1", "--validate")
	mustOutcome(t, first, prepareAcquired)

	mustOK(t, os.WriteFile(f.binary, []byte("#!/bin/sh\nexit 1\n"), 0o755))

	for _, args := range [][]string{
		{"--holder", "ci-1", "--validate"},
		{"--holder", "ci-1", "--no-change"},
		{"--holder", "ci-1", "--recovery"},
		{"--holder", "ci-1", "--candidate", filepath.Join(f.root, "recovery-20260909T120000-0badcafe", "billet.candidate")},
	} {
		o := runPrepare(t, args...)
		mustRefusal(t, o, reasonVerification)

		if !strings.Contains(o.str("why"), f.binarySHA) {
			t.Errorf("%v: why %q does not name the recorded digest", args, o.str("why"))
		}
	}

	if err := guardRun(t, "settle", "--holder", "ci-1", "--token", first.str("token")); err == nil ||
		!strings.Contains(err.Error(), "has digest") {
		t.Errorf("settle over an unverifiable record: %v", err)
	}

	if err := guardRun(t, "release", "--holder", "ci-1", "--cleanup", "--token", first.str("token")); err == nil ||
		!strings.Contains(err.Error(), "has digest") {
		t.Errorf("cleanup over an unverifiable record: %v", err)
	}

	dry := runPrepare(t, "--dry-run")
	mustOutcome(t, dry, prepareReported)

	guard := asMap(dry.doc["guard"])
	record := asMap(guard["record"])

	if record["verified"] != false {
		t.Errorf("the dry run reports verified %v, want false", record["verified"])
	}
}

// A4: THE SECOND CALL'S ROWS.
func TestPrepareJudgesTheSecondCall(t *testing.T) {
	stage := func(t *testing.T, f *guardFixture, dir, version, mode string) string {
		t.Helper()

		return guardCandidateScript(t, f, dir, version, mode)
	}

	t.Run("rebound over a managed-path record", func(t *testing.T) {
		f := newGuardFixture(t)
		managedScript(t, f, "v0.10.0")
		first := runPrepare(t, "--holder", "ci-1", "--validate")
		mustOutcome(t, first, prepareAcquired)
		cand := stage(t, f, "recovery-20260909T120000-0badcafe", "v0.10.1", "capable")

		var ops []string

		guardHook = func(op guardOp) error {
			ops = append(ops, op.Kind+" "+strings.TrimPrefix(op.Path, f.parent+"/"))

			return nil
		}

		o := runPrepare(t, "--holder", "ci-1", "--candidate", cand)

		guardHook = nil

		mustOutcome(t, o, prepareRebound)

		rec := f.record(t)

		switch {
		case rec.ReleaseExecutable != cand || o.record()["release_executable"] != cand:
			t.Errorf("record %+v answer %v", rec, o.record())
		case rec.ID != first.str("id") || rec.Token != first.str("token") || !rec.Preparing:
			t.Errorf("the re-binding changed the identity: %+v", rec)
		case rec.ClaimedAt != "2026-09-09T12:00:00Z":
			t.Errorf("claimed_at moved: %s", rec.ClaimedAt)
		case o.boolean("downgrade"):
			t.Error("a newer candidate is reported as a downgrade")
		}

		cand2 := asMap(o.doc["candidate"])
		if cand2["capable"] != true || cand2["version"] != "v0.10.1" {
			t.Errorf("candidate %v", cand2)
		}

		// The candidate's flushes before the temporary, the record by temporary
		// and rename, the directory and the root after.
		var seq []string

		for _, op := range ops {
			switch op {
			case "fsync upgrades/recovery-20260909T120000-0badcafe/billet.candidate",
				"fsync upgrades/recovery-20260909T120000-0badcafe",
				"create upgrades/active/guard.json.tmp", "fsync upgrades/active/guard.json.tmp",
				"rename upgrades/active/guard.json":
				seq = append(seq, op)
			}
		}

		want := []string{"fsync upgrades/recovery-20260909T120000-0badcafe/billet.candidate",
			"fsync upgrades/recovery-20260909T120000-0badcafe", "create upgrades/active/guard.json.tmp",
			"fsync upgrades/active/guard.json.tmp", "rename upgrades/active/guard.json"}
		if !slices.Equal(seq, want) {
			t.Errorf("the re-binding's operations:\n got %q\nwant %q", seq, want)
		}

		// Again with the same candidate: validated, nothing written.
		before := fileIdentityOf(t, filepath.Join(f.active(), guardRecordName))
		o = runPrepare(t, "--holder", "ci-1", "--candidate", cand)
		mustOutcome(t, o, prepareValidated)

		if fileIdentityOf(t, filepath.Join(f.active(), guardRecordName)) != before {
			t.Error("a validation over the recorded candidate rewrote the record")
		}

		// The same bytes from another path: validated.
		other := stage(t, f, "recovery-20260909T120000-1badcafe", "v0.10.1", "capable")
		o = runPrepare(t, "--holder", "ci-1", "--candidate", other)
		mustOutcome(t, o, prepareValidated)

		// Another candidate: refused, the record kept.
		another := stage(t, f, "recovery-20260909T120000-2badcafe", "v0.10.2", "capable")
		o = runPrepare(t, "--holder", "ci-1", "--candidate", another)
		mustRefusal(t, o, reasonIntent)

		if f.record(t).ReleaseExecutable != cand {
			t.Error("an intent refusal rewrote the record")
		}

		// --no-change over a candidate's record while the managed binary is
		// other bytes: refused; once the managed binary is the candidate's
		// bytes, validated.
		o = runPrepare(t, "--holder", "ci-1", "--no-change")
		mustRefusal(t, o, reasonIntent)

		body, err := os.ReadFile(cand)
		mustOK(t, err)
		mustOK(t, os.WriteFile(f.binary, body, 0o755))

		o = runPrepare(t, "--holder", "ci-1", "--no-change")
		mustOutcome(t, o, prepareValidated)
	})

	t.Run("no-change over a managed-path record", func(t *testing.T) {
		f := newGuardFixture(t)
		first := runPrepare(t, "--holder", "ci-1", "--validate")
		mustOutcome(t, first, prepareAcquired)

		before := fileIdentityOf(t, filepath.Join(f.active(), guardRecordName))
		o := runPrepare(t, "--holder", "ci-1", "--no-change")
		mustOutcome(t, o, prepareValidated)

		if fileIdentityOf(t, filepath.Join(f.active(), guardRecordName)) != before {
			t.Error("no-change rewrote the record")
		}
	})

	t.Run("the floor before any rewrite", func(t *testing.T) {
		f := newGuardFixture(t)
		managedScript(t, f, "v0.10.0")
		mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)
		before := fileIdentityOf(t, filepath.Join(f.active(), guardRecordName))

		for _, mode := range []string{"pre-r", "garbage"} {
			cand := stage(t, f, "recovery-20260909T120000-"+map[string]string{"pre-r": "0badcafe", "garbage": "1badcafe"}[mode], "v0.10.1", mode)
			o := runPrepare(t, "--holder", "ci-1", "--candidate", cand)
			mustRefusal(t, o, reasonFloor)

			if fileIdentityOf(t, filepath.Join(f.active(), guardRecordName)) != before {
				t.Errorf("%s: the floor's refusal rewrote the record", mode)
			}

			if f.record(t).ReleaseExecutable != f.binary {
				t.Errorf("%s: the record names %s after a floor refusal", mode, f.record(t).ReleaseExecutable)
			}
		}
	})

	t.Run("a hanging candidate is killed at the bound", func(t *testing.T) {
		f := newGuardFixture(t)
		mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)
		cand := stage(t, f, "recovery-20260909T120000-0badcafe", "v0.10.1", "hang")

		saved := guardCommandTimeout
		guardCommandTimeout = time.Second
		t.Cleanup(func() { guardCommandTimeout = saved })

		start := time.Now()
		o := runPrepare(t, "--holder", "ci-1", "--candidate", cand)
		mustRefusal(t, o, reasonFloor)

		if elapsed := time.Since(start); elapsed > 20*time.Second {
			t.Errorf("the bound did not hold: %s", elapsed)
		}
	})

	t.Run("the downgrade before any rewrite", func(t *testing.T) {
		f := newGuardFixture(t)
		managedScript(t, f, "0.10.1")
		mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)
		cand := stage(t, f, "recovery-20260909T120000-0badcafe", "v0.9.0", "capable")
		before := fileIdentityOf(t, filepath.Join(f.active(), guardRecordName))

		o := runPrepare(t, "--holder", "ci-1", "--candidate", cand)
		mustRefusal(t, o, reasonDowngrade)

		if !strings.Contains(o.str("why"), "v0.9.0") || !strings.Contains(o.str("why"), "v0.10.1") {
			t.Errorf("why %q does not name both spellings canonically", o.str("why"))
		}

		if fileIdentityOf(t, filepath.Join(f.active(), guardRecordName)) != before {
			t.Error("the downgrade's refusal rewrote the record")
		}

		o = runPrepare(t, "--holder", "ci-1", "--candidate", cand, "--allow-downgrade")
		mustOutcome(t, o, prepareRebound)

		if !o.boolean("downgrade") {
			t.Error("an admitted downgrade is not reported")
		}
	})

	t.Run("a candidate outside a recovery directory", func(t *testing.T) {
		f := newGuardFixture(t)
		mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)
		outside := filepath.Join(f.parent, "billet.candidate")
		mustOK(t, os.WriteFile(outside, []byte("#!/bin/sh\nexit 0\n"), 0o755))

		o := runPrepare(t, "--holder", "ci-1", "--candidate", outside)
		mustRefusal(t, o, reasonCandidate)
	})
}

// A3/A5: another holder's guard, the shapes with their next step, the
// candidate-less second call over none.
func TestPrepareRefusesTheShapesItDoesNotOwn(t *testing.T) {
	t.Run("another holder", func(t *testing.T) {
		newGuardFixture(t)
		mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)

		o := runPrepare(t, "--holder", "ci-2", "--validate")
		mustRefusal(t, o, reasonHeld)

		if !strings.Contains(o.str("why"), "ci-1") {
			t.Errorf("why %q", o.str("why"))
		}
	})

	for _, c := range []struct {
		name, shape, next string
		plant             func(t *testing.T, f *guardFixture)
	}{
		{"a Go transaction", string(claimHostUpgrade), "--status", func(t *testing.T, f *guardFixture) {
			t.Helper()
			mustOK(t, os.Mkdir(f.root, 0o700))
			mustOK(t, os.Symlink(filepath.Join(f.root, "recovery-x"), f.active()))
		}},
		{"a legacy pointer", string(claimLegacyRole), "upgrade-recover.yml", func(t *testing.T, f *guardFixture) {
			t.Helper()
			mustOK(t, os.Mkdir(f.root, 0o700))
			mustOK(t, os.WriteFile(f.active(), []byte("legacy"), 0o600))
		}},
		{"an unpublished directory", string(claimUnpublished), "recover --unpublished", func(t *testing.T, f *guardFixture) {
			t.Helper()
			mustOK(t, os.MkdirAll(f.active(), 0o700))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newGuardFixture(t)
			c.plant(t, f)

			o := runPrepare(t, "--holder", "ci-1", "--validate")
			mustRefusal(t, o, reasonShape)

			if o.str("shape") != c.shape || !strings.Contains(o.str("next"), c.next) {
				t.Errorf("shape %q next %q", o.str("shape"), o.str("next"))
			}
		})
	}

	t.Run("a second call over none", func(t *testing.T) {
		newGuardFixture(t)

		o := runPrepare(t, "--holder", "ci-1", "--no-change")
		mustRefusal(t, o, reasonShape)
	})
}

// A8: SETTLE AND THE CLEANUP RELEASE.
func TestSettleAndTheCleanupRelease(t *testing.T) {
	t.Run("settle closes the window", func(t *testing.T) {
		f := newGuardFixture(t)
		first := runPrepare(t, "--holder", "ci-1", "--validate")
		mustOutcome(t, first, prepareAcquired)
		token := first.str("token")

		if err := guardRun(t, "settle", "--holder", "ci-1", "--token", strings.Repeat("ab", 16)); err == nil {
			t.Error("settle with a wrong token succeeded")
		}

		if err := guardRun(t, "settle", "--holder", "ci-2", "--token", token); err == nil {
			t.Error("settle by another holder succeeded")
		}

		mustOK(t, guardRun(t, "settle", "--holder", "ci-1", "--token", token))

		rec := f.record(t)
		if rec.Preparing || rec.Token != token || rec.ID != first.str("id") {
			t.Errorf("the settled record %+v", rec)
		}

		err := guardRun(t, "settle", "--holder", "ci-1", "--token", token)
		if err == nil || exitStatus(err) != exitRefused || !strings.Contains(err.Error(), "already settled") {
			t.Errorf("settle over a settled guard: %v", err)
		}

		// THE STALE-TOKEN PROOF: cleanup after settlement refuses naming the
		// operator's release, and the guard stays.
		err = guardRun(t, "release", "--holder", "ci-1", "--cleanup", "--token", token)
		if err == nil || exitStatus(err) != exitRefused || !strings.Contains(err.Error(), "window has closed") {
			t.Errorf("cleanup over a settled guard: %v", err)
		}

		if _, err := os.Lstat(f.active()); err != nil {
			t.Error("a refused cleanup removed the guard")
		}

		mustOK(t, guardRun(t, "release", "--holder", "ci-1"))
	})

	t.Run("cleanup inside the window", func(t *testing.T) {
		f := newGuardFixture(t)
		first := runPrepare(t, "--holder", "ci-1", "--validate")
		mustOutcome(t, first, prepareAcquired)
		token := first.str("token")

		if err := guardRun(t, "release", "--holder", "ci-1", "--cleanup", "--token", strings.Repeat("ab", 16)); err == nil {
			t.Error("cleanup with a wrong token succeeded")
		}

		if err := guardRun(t, "release", "--holder", "ci-2", "--cleanup", "--token", token); err == nil {
			t.Error("cleanup by another holder succeeded")
		}

		if err := guardRun(t, "release", "--holder", "ci-1", "--cleanup"); err == nil {
			t.Error("cleanup without a token succeeded")
		}

		if err := guardRun(t, "release", "--holder", "ci-1", "--token", token); err == nil {
			t.Error("a token without --cleanup was admitted")
		}

		// A pointer refuses the cleanup.
		target := filepath.Join(f.root, "recovery-20260909T120000-0badcafe")
		mustOK(t, os.Mkdir(target, 0o700))
		mustOK(t, os.Symlink(target, filepath.Join(f.active(), guardPointerName)))

		if err := guardRun(t, "release", "--holder", "ci-1", "--cleanup", "--token", token); !errors.Is(err, errGuardPointer) {
			t.Errorf("cleanup over a pointer: %v", err)
		}

		mustOK(t, os.Remove(filepath.Join(f.active(), guardPointerName)))

		var ops []string

		guardHook = func(op guardOp) error {
			ops = append(ops, op.Kind+" "+strings.TrimPrefix(op.Path, f.parent+"/"))

			return nil
		}

		mustOK(t, guardRun(t, "release", "--holder", "ci-1", "--cleanup", "--token", token))

		guardHook = nil

		if _, err := os.Lstat(f.active()); !errors.Is(err, fs.ErrNotExist) {
			t.Error("the cleanup left the guard")
		}

		var seq []string

		for _, op := range ops {
			switch op {
			case "unlink upgrades/active/guard.json", "fsync upgrades/active", "rmdir upgrades/active", "fsync upgrades":
				seq = append(seq, op)
			}
		}

		if !slices.Equal(seq, []string{"unlink upgrades/active/guard.json", "fsync upgrades/active", "rmdir upgrades/active", "fsync upgrades"}) {
			t.Errorf("the release's order %q", seq)
		}
	})
}

// A9/A9b: THE INTERRUPTIONS of a re-binding and of a settlement, through the
// helper process killed where it stands.
func TestPrepareRewriteInterruptionsAreRecoverable(t *testing.T) {
	rewrites := []struct {
		name       string
		args       func(f *guardFixture, cand, token string) []string
		stopBefore string
		rebound    bool // whether the interrupted step happened after the rename
	}{
		{"re-binding before the temporary's sync", func(f *guardFixture, cand, token string) []string {
			return []string{"prepare", "--json", "--holder", "ci-1", "--candidate", cand}
		}, "fsync active/guard.json.tmp", false},
		{"re-binding before the rename", func(f *guardFixture, cand, token string) []string {
			return []string{"prepare", "--json", "--holder", "ci-1", "--candidate", cand}
		}, "rename active/guard.json", false},
		{"re-binding before the directory flush", func(f *guardFixture, cand, token string) []string {
			return []string{"prepare", "--json", "--holder", "ci-1", "--candidate", cand}
		}, "fsync active#2", true},
		{"settlement before the rename", func(f *guardFixture, cand, token string) []string {
			return []string{"settle", "--holder", "ci-1", "--token", token}
		}, "rename active/guard.json", false},
		{"settlement before the directory flush", func(f *guardFixture, cand, token string) []string {
			return []string{"settle", "--holder", "ci-1", "--token", token}
		}, "fsync active#2", true},
	}

	for _, c := range rewrites {
		t.Run(c.name, func(t *testing.T) {
			f := newGuardFixture(t)
			managedScript(t, f, "v0.10.0")
			first := runPrepare(t, "--holder", "ci-1", "--validate")
			mustOutcome(t, first, prepareAcquired)
			token := first.str("token")
			cand := guardCandidateScript(t, f, "recovery-20260909T120000-0badcafe", "v0.10.1", "capable")

			env := helperArgs(c.args(f, cand, token)...)
			env[guardHelperStopEnv] = c.stopBefore
			h := startGuardHelper(t, "command", env)
			h.await(t, "STOPPED")
			h.kill(t)

			shape, err := classifyClaim()
			mustOK(t, err)

			if shape.Kind != claimGuard {
				t.Fatalf("the remainder is %s", shape.Kind)
			}

			settling := strings.HasPrefix(c.name, "settlement")

			// The remainder: a stray temporary before the rename, the rewritten
			// record after it; status says which.
			if !c.rebound && !shape.StrayTemporary {
				t.Error("no stray temporary after an interruption before the rename")
			}

			if c.rebound && shape.StrayTemporary {
				t.Error("a stray temporary after the rename")
			}

			if !settling && c.rebound && shape.Guard.ReleaseExecutable != cand {
				t.Errorf("the record after the rename names %s", shape.Guard.ReleaseExecutable)
			}

			if settling && c.rebound && shape.Guard.Preparing {
				t.Error("the record after the settlement's rename is still preparing")
			}

			// The next --validate removes the temporary, completes the flushes,
			// and validates.
			var flushed []string

			guardHook = func(op guardOp) error {
				if op.Kind == "fsync" {
					flushed = append(flushed, strings.TrimPrefix(op.Path, f.parent+"/"))
				}

				return nil
			}

			o := runPrepare(t, "--holder", "ci-1", "--validate")

			guardHook = nil

			mustOutcome(t, o, prepareValidated)

			if !c.rebound != o.boolean("stray_removed") {
				t.Errorf("stray_removed %v after %s", o.boolean("stray_removed"), c.name)
			}

			if !slices.Contains(flushed, "upgrades/active") || !slices.Contains(flushed, "upgrades") {
				t.Errorf("the readmission flushed %q, want the guard directory and the root", flushed)
			}

			if _, err := os.Lstat(filepath.Join(f.active(), guardTmpName)); !errors.Is(err, fs.ErrNotExist) {
				t.Error("the stray temporary survived the validation")
			}

			// The cleanup's admission follows the record: possible while the
			// record is preparing, refused once the settlement's rename landed.
			err = guardRun(t, "release", "--holder", "ci-1", "--cleanup", "--token", token)

			if settling && c.rebound {
				if err == nil || !strings.Contains(err.Error(), "window has closed") {
					t.Errorf("cleanup after the settlement's rename: %v", err)
				}
			} else if err != nil {
				t.Errorf("cleanup inside the window: %v", err)
			}
		})
	}

	t.Run("a stray temporary of another shape refuses", func(t *testing.T) {
		f := newGuardFixture(t)
		mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)
		mustOK(t, os.Mkdir(filepath.Join(f.active(), guardTmpName), 0o700))

		o := runPrepare(t, "--holder", "ci-1", "--validate")
		mustRefusal(t, o, reasonStray)
	})

	t.Run("a flush that fails again refuses the readmission", func(t *testing.T) {
		f := newGuardFixture(t)
		mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)

		guardHook = func(op guardOp) error {
			if op.Kind == "fsync" && op.Path == f.active() {
				return errors.New("staged flush failure")
			}

			return nil
		}
		t.Cleanup(func() { guardHook = nil })

		o := runPrepare(t, "--holder", "ci-1", "--validate")
		if o.str("outcome") != prepareUnknown || o.code != exitUnknown {
			t.Errorf("a failed flush answered %v", o.doc)
		}
	})
}

// A8b: THE RELEASE'S BOUNDARIES, the remainder each leaves.
func TestCleanupReleaseInterruptionsLeaveClassifiedRemainders(t *testing.T) {
	for _, c := range []struct {
		stopBefore string
		kind       claimKind
	}{
		{"fsync active", claimUnpublished},
		{"rmdir active", claimUnpublished},
		{"fsync .", claimNone},
	} {
		t.Run(c.stopBefore, func(t *testing.T) {
			f := newGuardFixture(t)
			first := runPrepare(t, "--holder", "ci-1", "--validate")
			mustOutcome(t, first, prepareAcquired)

			stop := c.stopBefore
			if stop == "fsync ." {
				stop = "fsync " + f.root
			}

			env := helperArgs("release", "--holder", "ci-1", "--cleanup", "--token", first.str("token"))
			env[guardHelperStopEnv] = stop
			h := startGuardHelper(t, "command", env)
			h.await(t, "STOPPED")
			h.kill(t)

			shape, err := classifyClaim()
			mustOK(t, err)

			if shape.Kind != c.kind {
				t.Fatalf("the remainder is %s, want %s", shape.Kind, c.kind)
			}

			if c.kind == claimUnpublished {
				mustOK(t, guardRun(t, "recover", "--unpublished"))
			}
		})
	}
}

// A10: THE ANSWER LOST after publication: the guard exists preparing, the next
// validation carries no token, the operator's release works.
func TestPrepareAnswerLostLeavesAGuardNobodyInfersOwnershipOf(t *testing.T) {
	f := newGuardFixture(t)

	// Killed after the record was published and before the root's flush and
	// the answer: the guard is there, preparing, and nobody received a token.
	env := helperArgs("prepare", "--json", "--holder", "ci-1", "--validate")
	env[guardHelperStopEnv] = "fsync " + f.root
	h := startGuardHelper(t, "command", env)
	h.await(t, "STOPPED")
	h.kill(t)

	shape, err := classifyClaim()
	mustOK(t, err)

	if shape.Kind != claimGuard || !shape.Guard.Preparing {
		t.Fatalf("the remainder %s preparing=%v", shape.Kind, shape.Guard.Preparing)
	}

	o := runPrepare(t, "--holder", "ci-1", "--validate")
	mustOutcome(t, o, prepareValidated)

	if o.str("token") != "" {
		t.Error("a validation printed a token")
	}

	mustOK(t, guardRun(t, "release", "--holder", "ci-1"))
}

// A11: THE DRY RUN over every shape, lock-free, writing nothing.
func TestPrepareDryRunReportsEveryShape(t *testing.T) {
	report := func(t *testing.T, _ *guardFixture) map[string]any {
		t.Helper()

		var ops []guardOp

		guardHook = func(op guardOp) error { ops = append(ops, op); return nil }

		o := runPrepare(t, "--dry-run")

		guardHook = nil

		mustOutcome(t, o, prepareReported)

		for _, op := range ops {
			if op.Kind == "flock" || op.Kind == "mkdir" || op.Kind == "create" || op.Kind == "rename" || op.Kind == "unlink" {
				t.Errorf("the dry run performed %s %s", op.Kind, op.Path)
			}
		}

		return o.doc
	}

	t.Run("none", func(t *testing.T) {
		f := newGuardFixture(t)
		doc := report(t, f)

		if doc["shape"] != "none" || doc["guard"] != nil {
			t.Errorf("%v", doc)
		}

		if _, err := os.Lstat(f.root); !errors.Is(err, fs.ErrNotExist) {
			t.Error("the dry run created the root")
		}
	})

	t.Run("this holder's guard under a held lock", func(t *testing.T) {
		f := newGuardFixture(t)
		first := runPrepare(t, "--holder", "ci-1", "--validate")
		mustOutcome(t, first, prepareAcquired)

		end := startLockHolder(t)
		doc := report(t, f)
		end()

		guard := asMap(doc["guard"])
		if doc["shape"] != "converge-guard" || guard["id"] != first.str("id") || guard["preparing"] != true ||
			guard["holder"] != "ci-1" {
			t.Errorf("%v", doc)
		}

		record := asMap(guard["record"])
		if record["verified"] != true {
			t.Errorf("verified %v", record["verified"])
		}

		if strings.Contains(fmt.Sprint(doc), first.str("token")) {
			t.Error("the dry run printed the token")
		}
	})

	for _, c := range []struct {
		name, shape string
		plant       func(t *testing.T, f *guardFixture)
	}{
		{"a Go transaction", "host-upgrade", func(t *testing.T, f *guardFixture) {
			t.Helper()
			mustOK(t, os.Mkdir(f.root, 0o700))
			mustOK(t, os.Symlink(filepath.Join(f.root, "recovery-x"), f.active()))
		}},
		{"a legacy pointer", "legacy-role", func(t *testing.T, f *guardFixture) {
			t.Helper()
			mustOK(t, os.Mkdir(f.root, 0o700))
			mustOK(t, os.WriteFile(f.active(), []byte("legacy"), 0o600))
		}},
		{"an unpublished directory", "unpublished-guard", func(t *testing.T, f *guardFixture) {
			t.Helper()
			mustOK(t, os.MkdirAll(f.active(), 0o700))
		}},
		{"an unknown type", "unknown", func(t *testing.T, f *guardFixture) {
			t.Helper()
			mustOK(t, os.Mkdir(f.root, 0o700))
			mustOK(t, unix.Mkfifo(f.active(), 0o600))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newGuardFixture(t)
			c.plant(t, f)

			doc := report(t, f)
			if doc["shape"] != c.shape {
				t.Errorf("shape %v, want %s", doc["shape"], c.shape)
			}
		})
	}

	t.Run("an unreadable record and a pointer problem", func(t *testing.T) {
		f := newGuardFixture(t)
		mustHold(t, "ci-1")
		mustOK(t, os.WriteFile(filepath.Join(f.active(), guardRecordName), []byte("not json\n"), 0o600))
		mustOK(t, os.WriteFile(filepath.Join(f.active(), guardPointerName), []byte("x"), 0o600))

		doc := report(t, f)
		guard := asMap(doc["guard"])
		record := asMap(guard["record"])

		if guard["record_error"] == "" || record["verified"] != "unknown" || guard["pointer_problem"] == "" {
			t.Errorf("%v", doc)
		}
	})
}

// A7: THE JUDGE, over the corpus the Python module carried.
func TestTheStatusJudgeRefusesEachCorruptionByName(t *testing.T) {
	healthy := map[string]any{
		"active": "converge-guard",
		"guard": map[string]any{
			"holder": "ci-1", "claimed_at": "2026-09-09T12:00:00Z", "hostname": "billet-control-01",
			"recovery_pointer": false, "release_executable": "/usr/bin/billet",
			"release_executable_sha256": strings.Repeat("ab", 32), "release_executable_verified": true,
		},
	}

	corrupt := func(path []string, value any, del bool) []byte {
		body, err := json.Marshal(healthy)
		mustOK(t, err)

		var doc map[string]any
		mustOK(t, json.Unmarshal(body, &doc))

		if len(path) == 0 {
			return body
		}

		cur := doc
		for _, p := range path[:len(path)-1] {
			next, ok := cur[p].(map[string]any)
			if !ok {
				t.Fatalf("the healthy answer has no object at %s", p)
			}

			cur = next
		}

		if del {
			delete(cur, path[len(path)-1])
		} else {
			cur[path[len(path)-1]] = value
		}

		body, err = json.Marshal(doc)
		mustOK(t, err)

		return body
	}

	for _, c := range []struct {
		name  string
		body  []byte
		words string
	}{
		{"not JSON", []byte("nope"), "not JSON"},
		{"a list", []byte("[]"), "not JSON"},
		{"no active", corrupt([]string{"active"}, nil, true), "active is missing"},
		{"active a number", corrupt([]string{"active"}, 1, false), "active is missing or not a string"},
		{"active an unfamiliar word", corrupt([]string{"active"}, "held", false), "not a word this command knows"},
		{"unknown without why", []byte(`{"active": "unknown"}`), "why is missing"},
		{"unknown with why a number", []byte(`{"active": "unknown", "why": 1}`), "why is not a string"},
		{"unknown with why empty", []byte(`{"active": "unknown", "why": ""}`), "why is missing or empty"},
		{"converge-guard without guard", corrupt([]string{"guard"}, nil, true), "guard: missing"},
		{"guard a string", corrupt([]string{"guard"}, "x", false), "guard: missing or not an object"},
		{"record_error a number", corrupt([]string{"guard", "record_error"}, 1, false), "record_error is not a non-empty string"},
		{"holder missing", corrupt([]string{"guard", "holder"}, nil, true), "holder is missing"},
		{"holder a number", corrupt([]string{"guard", "holder"}, 1, false), "holder is missing or not a string"},
		{"holder empty", corrupt([]string{"guard", "holder"}, "", false), "is not a name"},
		{"holder with a space", corrupt([]string{"guard", "holder"}, "ci 1", false), "is not a name"},
		{"holder with a slash", corrupt([]string{"guard", "holder"}, "ci/1", false), "is not a name"},
		{"holder of 300 bytes", corrupt([]string{"guard", "holder"}, strings.Repeat("c", 300), false), "is not a name"},
		{"claimed_at not RFC 3339", corrupt([]string{"guard", "claimed_at"}, "yesterday", false), "claimed_at is not an RFC 3339 time"},
		{"hostname a number", corrupt([]string{"guard", "hostname"}, 7, false), "hostname is missing or not a string"},
		{"recovery_pointer missing", corrupt([]string{"guard", "recovery_pointer"}, nil, true), "recovery_pointer is missing"},
		{"recovery_pointer a string", corrupt([]string{"guard", "recovery_pointer"}, "false", false), "recovery_pointer is missing or not a boolean"},
		{"release_executable relative", corrupt([]string{"guard", "release_executable"}, "billet", false), "not an absolute path"},
		{"sha256 of 63 hex", corrupt([]string{"guard", "release_executable_sha256"}, strings.Repeat("a", 63), false), "not 64 lowercase hex"},
		{"sha256 uppercase", corrupt([]string{"guard", "release_executable_sha256"}, strings.Repeat("A", 64), false), "not 64 lowercase hex"},
		{"verified missing", corrupt([]string{"guard", "release_executable_verified"}, nil, true), "release_executable_verified is missing"},
		{"verified the string unknown", corrupt([]string{"guard", "release_executable_verified"}, "unknown", false), "neither true, false nor an object"},
		{"verified an empty object", corrupt([]string{"guard", "release_executable_verified"}, map[string]any{}, false), "neither true, false nor an object"},
		{"verified unknown a number", corrupt([]string{"guard", "release_executable_verified"}, map[string]any{"unknown": 1}, false), "neither true, false nor an object"},
		{"verified unknown with more", corrupt([]string{"guard", "release_executable_verified"}, map[string]any{"unknown": "x", "more": 1}, false), "neither true, false nor an object"},
	} {
		t.Run(c.name, func(t *testing.T) {
			problem := judgeStatusAnswer(0, c.body, nil, false)
			if !strings.Contains(problem, c.words) {
				t.Errorf("problem %q, want %q", problem, c.words)
			}
		})
	}

	for _, c := range []struct {
		name string
		body []byte
	}{
		{"healthy", corrupt(nil, nil, false)},
		{"a record error", []byte(`{"active": "converge-guard", "guard": {"record_error": "not JSON"}}`)},
		{"none", []byte(`{"active": "none"}`)},
		{"unknown with why", []byte(`{"active": "unknown", "why": "x"}`)},
		{"verified false", corrupt([]string{"guard", "release_executable_verified"}, false, false)},
		{"verified unknown", corrupt([]string{"guard", "release_executable_verified"}, map[string]any{"unknown": "x"}, false)},
	} {
		t.Run(c.name+" admitted", func(t *testing.T) {
			if problem := judgeStatusAnswer(0, c.body, nil, false); problem != "" {
				t.Errorf("refused: %s", problem)
			}
		})
	}

	if problem := judgeStatusAnswer(0, nil, nil, true); !strings.Contains(problem, "within the bound") {
		t.Errorf("a timeout: %q", problem)
	}

	if problem := judgeStatusAnswer(1, nil, []byte("boom"), false); !strings.Contains(problem, "exited 1: boom") {
		t.Errorf("a failure: %q", problem)
	}
}

// A13: THE FLAG TABLE, refused before the lock is taken.
func TestPrepareRefusesInvalidCombinationsBeforeTheLock(t *testing.T) {
	f := newGuardFixture(t)
	cand := filepath.Join(f.root, "recovery-20260909T120000-0badcafe", "billet.candidate")

	for _, args := range [][]string{
		{"--holder", "ci-1"},
		{"--holder", "ci-1", "--validate", "--no-change"},
		{"--holder", "ci-1", "--candidate", cand, "--no-change"},
		{"--holder", "ci-1", "--candidate", cand, "--recovery"},
		{"--holder", "ci-1", "--validate", "--expect-bootstrap"},
		{"--holder", "ci-1", "--candidate", cand, "--expect-bootstrap"},
		{"--dry-run", "--candidate", cand},
		{"--dry-run", "--expect-id", strings.Repeat("ab", 16)},
		{"--dry-run", "--allow-downgrade"},
		{"--holder", "ci-1", "--validate", "--allow-downgrade"},
		{"--holder", "ci-1", "--no-change", "--allow-downgrade"},
		{"--holder", "ci-1", "--validate", "--expect-id", "short"},
		{"--holder", "ci-1", "--no-change", "--token", "short"},
		{"--holder", "", "--validate"},
		{"--holder", "ci 1", "--validate"},
	} {
		var ops []guardOp

		guardHook = func(op guardOp) error { ops = append(ops, op); return nil }

		o := runPrepare(t, args...)

		guardHook = nil

		mustRefusal(t, o, reasonCombination)

		if len(ops) != 0 {
			t.Errorf("%v: the refusal touched the root: %v", args, ops)
		}
	}

	if err := guardRun(t, "prepare", "--holder", "ci-1", "--validate"); err == nil || !strings.Contains(err.Error(), "--json") {
		t.Errorf("prepare without --json: %v", err)
	}
}

// A12: THE CRASH SEAM'S NEGATIVE WITNESS: the only file that reads the
// variable carries the build constraint, nothing else references the seam,
// and this untagged binary completes with the variable set.
func TestTheCrashSeamIsOutOfOrdinaryBuilds(t *testing.T) {
	entries, err := filepath.Glob("*.go")
	mustOK(t, err)

	readers := 0

	for _, path := range entries {
		body, err := os.ReadFile(path)
		mustOK(t, err)

		if !strings.Contains(string(body), "BILLET_GUARD_CRASH_AT") {
			continue
		}

		if strings.HasSuffix(path, "_test.go") {
			continue
		}

		readers++

		if !regexp.MustCompile(`(?m)^//go:build billetgatecrash$`).Match(body) {
			t.Errorf("%s reads BILLET_GUARD_CRASH_AT without the billetgatecrash build constraint", path)
		}
	}

	if readers != 1 {
		t.Errorf("%d files read the crash variable, want exactly the tagged one", readers)
	}

	newGuardFixture(t)
	t.Setenv("BILLET_GUARD_CRASH_AT", "rename active/guard.json")

	o := runPrepare(t, "--holder", "ci-1", "--validate")
	mustOutcome(t, o, prepareAcquired)
}

// A14: THE COMMITTED CORPUS of the command's answers, written by this test
// from the command's own output over planted shapes (BILLET_UPDATE_FIXTURES=1
// rewrites it), so a fake or a corruption that answers a shape the command
// does not produce drifts from these.
func TestTheGuardPrepareFixturesAreTheCommandsOwn(t *testing.T) {
	dir := filepath.Join("..", "..", "ansible_collections", "junioryono", "billet", "tests", "fixtures", "guard-prepare")
	update := os.Getenv("BILLET_UPDATE_FIXTURES") == "1"

	const recovery = "recovery-20260909T120000-0badcafe"

	shapes := map[string]func(t *testing.T, f *guardFixture) []string{
		"acquired": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			managedScript(t, f, "v0.10.0")

			return []string{"--holder", "ci-1", "--validate"}
		},
		"validated-preparing": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			managedScript(t, f, "v0.10.0")
			mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)

			return []string{"--holder", "ci-1", "--validate"}
		},
		"validated-settled": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			managedScript(t, f, "v0.10.0")
			first := runPrepare(t, "--holder", "ci-1", "--validate")
			mustOutcome(t, first, prepareAcquired)
			mustOK(t, guardRun(t, "settle", "--holder", "ci-1", "--token", first.str("token")))

			return []string{"--holder", "ci-1", "--validate"}
		},
		"rebound": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			managedScript(t, f, "v0.10.0")
			mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)

			return []string{"--holder", "ci-1", "--candidate", guardCandidateScript(t, f, recovery, "v0.10.1", "capable")}
		},
		"no-change": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			managedScript(t, f, "v0.10.0")
			mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)

			return []string{"--holder", "ci-1", "--no-change"}
		},
		"adopted": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			managedScript(t, f, "v0.10.0")
			mustHold(t, "ci-1")
			body, err := json.MarshalIndent(map[string]any{
				"holder": "ci-1", "claimed_at": "2026-09-09T11:00:00Z", "hostname": "billet-control-01",
				"release_executable": f.binary, "release_executable_sha256": f.binarySHA,
			}, "", "  ")
			mustOK(t, err)
			mustOK(t, os.WriteFile(filepath.Join(f.active(), guardRecordName), append(body, '\n'), 0o600))

			return []string{"--holder", "ci-1", "--validate"}
		},
		"recovery": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			managedScript(t, f, "v0.10.0")
			mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)
			target := filepath.Join(f.root, recovery)
			mustOK(t, os.Mkdir(target, 0o700))
			mustOK(t, os.Symlink(target, filepath.Join(f.active(), guardPointerName)))

			return []string{"--holder", "ci-1", "--recovery"}
		},
		"refused-held": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			managedScript(t, f, "v0.10.0")
			mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)

			return []string{"--holder", "ci-2", "--validate"}
		},
		"refused-host-upgrade": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			mustOK(t, os.Mkdir(f.root, 0o700))
			mustOK(t, os.Symlink(filepath.Join(f.root, "recovery-x"), f.active()))

			return []string{"--holder", "ci-1", "--validate"}
		},
		"refused-legacy-role": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			mustOK(t, os.Mkdir(f.root, 0o700))
			mustOK(t, os.WriteFile(f.active(), []byte("legacy"), 0o600))

			return []string{"--holder", "ci-1", "--validate"}
		},
		"refused-unpublished": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			mustOK(t, os.MkdirAll(f.active(), 0o700))

			return []string{"--holder", "ci-1", "--validate"}
		},
		"refused-gone": func(t *testing.T, _ *guardFixture) []string {
			t.Helper()

			return []string{"--holder", "ci-1", "--validate", "--expect-id", strings.Repeat("ab", 16)}
		},
		"refused-pointer": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			managedScript(t, f, "v0.10.0")
			mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)
			target := filepath.Join(f.root, recovery)
			mustOK(t, os.Mkdir(target, 0o700))
			mustOK(t, os.Symlink(target, filepath.Join(f.active(), guardPointerName)))

			return []string{"--holder", "ci-1", "--no-change"}
		},
		"refused-floor": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			managedScript(t, f, "v0.10.0")
			mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)

			return []string{"--holder", "ci-1", "--candidate", guardCandidateScript(t, f, recovery, "v0.10.1", "pre-r")}
		},
		"refused-downgrade": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			managedScript(t, f, "v0.10.1")
			mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)

			return []string{"--holder", "ci-1", "--candidate", guardCandidateScript(t, f, recovery, "v0.9.0", "capable")}
		},
		"refused-intent": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			managedScript(t, f, "v0.10.0")
			mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)
			mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--candidate", guardCandidateScript(t, f, recovery, "v0.10.1", "capable")), prepareRebound)

			return []string{"--holder", "ci-1", "--candidate", guardCandidateScript(t, f, "recovery-20260909T120000-1badcafe", "v0.10.2", "capable")}
		},
		"refused-managed-absent": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			mustOK(t, os.Remove(f.binary))

			return []string{"--holder", "ci-1", "--validate"}
		},
		"refused-bootstrap": func(t *testing.T, f *guardFixture) []string {
			t.Helper()

			return []string{"--holder", "ci-1", "--validate", "--candidate", guardCandidateScript(t, f, recovery, "v0.10.1", "capable"), "--expect-bootstrap"}
		},
		"refused-combination": func(t *testing.T, _ *guardFixture) []string {
			t.Helper()

			return []string{"--holder", "ci-1", "--validate", "--no-change"}
		},
		"dry-run-none": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			managedScript(t, f, "v0.10.0")

			return []string{"--dry-run"}
		},
		"dry-run-guard": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			managedScript(t, f, "v0.10.0")
			mustOutcome(t, runPrepare(t, "--holder", "ci-1", "--validate"), prepareAcquired)

			return []string{"--dry-run"}
		},
		"dry-run-host-upgrade": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			managedScript(t, f, "v0.10.0")
			mustOK(t, os.Mkdir(f.root, 0o700))
			mustOK(t, os.Symlink(filepath.Join(f.root, "recovery-x"), f.active()))

			return []string{"--dry-run"}
		},
		"dry-run-legacy-role": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			managedScript(t, f, "v0.10.0")
			mustOK(t, os.Mkdir(f.root, 0o700))
			mustOK(t, os.WriteFile(f.active(), []byte("legacy"), 0o600))

			return []string{"--dry-run"}
		},
		"dry-run-unpublished": func(t *testing.T, f *guardFixture) []string {
			t.Helper()
			managedScript(t, f, "v0.10.0")
			mustOK(t, os.MkdirAll(f.active(), 0o700))

			return []string{"--dry-run"}
		},
	}

	if update {
		mustOK(t, os.MkdirAll(dir, 0o755))
	}

	digests := regexp.MustCompile(`"(sha256|release_executable_sha256)": "[0-9a-f]{64}"`)

	for name, plant := range shapes {
		t.Run(name, func(t *testing.T) {
			f := newGuardFixture(t)
			args := plant(t, f)

			// A refused shape answers by its exit; the answer is what is compared.
			out := capture(t, func() {
				if err := guardRun(t, append([]string{"prepare", "--json"}, args...)...); err != nil {
					t.Logf("%s: %v", name, err)
				}
			})

			// The packaged host's spellings and one digest, so the fixture is the
			// same on every machine.
			out = strings.ReplaceAll(out, f.root, "/var/lib/billet/upgrades")
			out = strings.ReplaceAll(out, f.binary, "/usr/bin/billet")
			out = digests.ReplaceAllString(out, `"$1": "`+strings.Repeat("ab", 32)+`"`)

			var parsed map[string]any
			if err := json.Unmarshal([]byte(out), &parsed); err != nil {
				t.Fatalf("%s: the answer is not JSON: %v\n%s", name, err, out)
			}

			path := filepath.Join(dir, name+".json")

			if update {
				mustOK(t, os.WriteFile(path, []byte(out), 0o644))

				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%s: %v (run with BILLET_UPDATE_FIXTURES=1 to write the fixtures)", name, err)
			}

			if string(want) != out {
				t.Errorf("%s: the fixture differs from the command's answer:\n--- fixture\n%s\n--- command\n%s", name, want, out)
			}
		})
	}
}
