package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/durablefile"
)

// `node receipt` (fixtures-c4b.md, section C): evidence mode after a
// migration, refresh mode at the end of every ordinary converge, the record
// re-verified under the bracket, the configuration observation closed
// before the write or the shortcut, the file durable through the installer
// with the directory established and its parent flushed first.

// receiptCmdFixture is the endpoint fixture (a node, its record, the fake
// systemctl) with the receipt seams pointed under the same temporary root
// and the installer's steps observed.
type receiptCmdFixture struct {
	*endpointFixture
	*receiptFixture
	events   *[]string
	failSync map[string]error
}

const receiptRun = "ci-run-12"

func newReceiptCmdFixture(t *testing.T) *receiptCmdFixture {
	t.Helper()

	e := newEndpointFixture(t)
	r := newReceiptFixture(t)
	f := &receiptCmdFixture{endpointFixture: e, receiptFixture: r, events: &[]string{}, failSync: map[string]error{}}

	// The receipt lives under the endpoint fixture's own root, so one
	// temporary directory holds everything a case plants.
	mustOK(t, os.RemoveAll(r.dir))
	r.parent = filepath.Join(e.dir, "var", "lib", "billet")
	r.dir = filepath.Join(r.parent, "node")
	r.path = filepath.Join(r.dir, "endpoint-migration.json")
	mustOK(t, os.MkdirAll(r.parent, 0o755))
	receiptPath = r.path

	saved := struct {
		installer func() durablefile.Installer
		sync      func(string) error
		now       func() time.Time
		mkdir     func(string, os.FileMode) error
	}{receiptInstaller, receiptSyncDir, receiptNow, receiptMkdir}
	t.Cleanup(func() {
		receiptInstaller, receiptSyncDir, receiptNow, receiptMkdir = saved.installer, saved.sync, saved.now, saved.mkdir
	})

	record := func(ev string) { *f.events = append(*f.events, ev) }
	name := func(path string) string {
		switch path {
		case r.parent:
			return "parent"
		case r.dir:
			return "dir"
		}

		return filepath.Base(path)
	}

	receiptNow = func() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 500000000, time.UTC) }
	receiptMkdir = func(path string, mode os.FileMode) error {
		record("mkdir:" + name(path))

		return os.Mkdir(path, mode)
	}
	receiptSyncDir = func(path string) error {
		record("syncdir:" + name(path))

		if err := f.failSync[name(path)]; err != nil {
			return err
		}

		return durablefile.Installer{}.SyncDirectory(path)
	}
	receiptInstaller = func() durablefile.Installer {
		return durablefile.Installer{
			SyncFile: func(file *os.File) error {
				record("syncfile")

				if err := f.failSync["file"]; err != nil {
					return err
				}

				return file.Sync()
			},
			Rename: func(from, to string) error {
				record("rename")

				return os.Rename(from, to)
			},
			SyncDir: func(path string) error {
				record("syncdir:" + name(path))

				if err := f.failSync[name(path)]; err != nil {
					return err
				}

				return durablefile.Installer{}.SyncDirectory(path)
			},
		}
	}

	return f
}

// migrated plants the state after A7's migration: the configuration B
// installed, the unit under the new invocation, the record naming B under
// the new invocation and incarnation.
func (f *receiptCmdFixture) migrated(t *testing.T) {
	t.Helper()
	f.installB(t)
	f.touchBeforeStart(t, f.configPath)
	f.setNode(t, "active", "running", inspectPID, newInvocation, "mixed")
	f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalB,
		"invocation_id": newInvocation, "incarnation": newIncarnation}))
}

// evidenceObject is A7's answer as an object, with overrides.
func (f *receiptCmdFixture) evidenceObject(t *testing.T, overrides map[string]any) map[string]any {
	t.Helper()

	m := map[string]any{
		"schema": 1, "outcome": "migrated", "node": "node-a", "deployment": f.deployment, "from": canonicalA,
		"to": canonicalB, "config_path": f.configPath, "installed_sha256": f.installedSHA(t),
		"invocation_id": newInvocation, "incarnation": newIncarnation, "registered_at": "2026-09-10T11:59:00Z",
		"stopped": map[string]any{"active_state": "inactive", "sub_state": "dead", "result": "success"},
	}

	for k, v := range overrides {
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
	}

	return m
}

// confirmationObject is B1's answer as an object, with overrides.
func (f *receiptCmdFixture) confirmationObject(overrides map[string]any) map[string]any {
	m := map[string]any{
		"schema": 1, "outcome": "confirmed", "node": "node-a", "incarnation": newIncarnation, "epoch": 3, "live": true,
		"deployment": map[string]any{"bound": true, "id": f.deployment},
	}

	for k, v := range overrides {
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
	}

	return m
}

// scratch is where a case's own files go: the endpoint fixture's root,
// never the receipt directory, which the command establishes itself.
func (f *receiptCmdFixture) scratch() string { return f.endpointFixture.dir }

func (f *receiptCmdFixture) file(t *testing.T, name string, v any) string {
	t.Helper()

	path := filepath.Join(f.scratch(), name)

	body, ok := v.([]byte)
	if !ok {
		var err error

		body, err = json.Marshal(v)
		mustOK(t, err)
		body = append(body, '\n')
	}

	writeFile(t, path, string(body), 0o600)

	return path
}

// evidence runs evidence mode over the two answers.
func (f *receiptCmdFixture) evidence(t *testing.T, ev, conf any, extra ...string) endpointOut {
	t.Helper()

	args := []string{"--evidence", f.file(t, "evidence.json", ev), "--confirmation", f.file(t, "confirmation.json", conf)}
	if !contains(extra, "--config") {
		args = append(args, "--config", f.configPath)
	}

	if !contains(extra, "--run") {
		args = append(args, "--run", receiptRun)
	}

	return runEndpoint(t, cmdNodeReceipt, "", append(args, extra...)...)
}

// refresh runs refresh mode with the rendering on stdin.
func (f *receiptCmdFixture) refresh(t *testing.T, rendering string, extra ...string) endpointOut {
	t.Helper()

	args := []string{"--refresh", "--config", f.configPath, "--wait", "200ms"}
	if rendering != "" {
		args = append(args, "--desired", "-")
	}

	if !contains(extra, "--run") && !contains(extra, "--dry-run") {
		args = append(args, "--run", receiptRun)
	}

	return runEndpoint(t, cmdNodeReceipt, rendering, append(args, extra...)...)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}

	return false
}

// receiptOnDisk reads the receipt file as the reader does.
func (f *receiptCmdFixture) receiptOnDisk(t *testing.T) endpointReceipt {
	t.Helper()

	ev := readEndpointReceipt(f.path)
	if ev.presence != receiptPresent {
		t.Fatalf("the receipt on disk: %s %s", ev.presence, ev.why)
	}

	return *ev.receipt
}

func (f *receiptCmdFixture) noReceipt(t *testing.T) {
	t.Helper()

	if _, err := os.Lstat(f.path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a receipt exists: %v", err)
	}
}

func (f *receiptCmdFixture) eventsAre(t *testing.T, want ...string) {
	t.Helper()

	if got := strings.Join(*f.events, " "); got != strings.Join(want, " ") {
		t.Errorf("the write's steps:\n got %s\nwant %s", got, strings.Join(want, " "))
	}
}

func mustWritten(t *testing.T, o endpointOut) map[string]any {
	t.Helper()
	mustEndpointOutcome(t, o, outcomeWritten)

	return asMap(o.doc["receipt"])
}

// C1: evidence mode, happy: the ten members, the endpoints one, mode 0600,
// the directory created root 0700 with its parent flushed before anything
// is written into it, then the write's order and the two closing flushes.
func TestReceiptEvidenceModeWritesTheReceiptDurably(t *testing.T) {
	f := newReceiptCmdFixture(t)
	f.migrated(t)

	o := f.evidence(t, f.evidenceObject(t, nil), f.confirmationObject(nil))
	rec := mustWritten(t, o)

	if len(rec) != len(receiptFields) {
		t.Errorf("members %v", rec)
	}

	if rec["installed_endpoint"] != canonicalB || rec["effective_endpoint"] != canonicalB || rec["run"] != receiptRun ||
		rec["node"] != "node-a" || rec["deployment"] != f.deployment || rec["installed_sha256"] != f.installedSHA(t) ||
		rec["invocation_id"] != newInvocation || rec["incarnation"] != newIncarnation ||
		rec["written_at"] != "2026-09-10T12:00:00.5Z" || rec["schema"] != float64(1) {
		t.Errorf("receipt %v", rec)
	}

	disk := f.receiptOnDisk(t)
	if disk.WrittenAt != "2026-09-10T12:00:00.5Z" || disk.EffectiveEndpoint != canonicalB {
		t.Errorf("on disk %+v", disk)
	}

	info, err := os.Lstat(f.path)
	mustOK(t, err)

	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %04o", info.Mode().Perm())
	}

	dirInfo, err := os.Lstat(f.dir)
	mustOK(t, err)

	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("directory mode %04o", dirInfo.Mode().Perm())
	}

	f.eventsAre(t, "mkdir:dir", "syncdir:parent", "syncfile", "rename", "syncdir:dir", "syncdir:parent")

	// C1b, C1c, C1d: --config must be the evidence's path exactly.
	t.Run("--config spelled relatively", func(t *testing.T) {
		t.Helper()
		wd, err := os.Getwd()
		mustOK(t, err)
		rel, err := filepath.Rel(wd, f.configPath)
		mustOK(t, err)

		o := f.evidence(t, f.evidenceObject(t, nil), f.confirmationObject(nil), "--config", rel)
		mustWritten(t, o)
	})

	t.Run("--config other than the evidence's", func(t *testing.T) {
		t.Helper()
		other := filepath.Join(f.scratch(), "other.yaml")
		writeFile(t, other, f.rendering(endpointB), 0o644)

		o := f.evidence(t, f.evidenceObject(t, nil), f.confirmationObject(nil), "--config", other)
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonConfig)
	})

	t.Run("without --config", func(t *testing.T) {
		t.Helper()
		o := runEndpoint(t, cmdNodeReceipt, "", "--evidence", f.file(t, "e.json", f.evidenceObject(t, nil)),
			"--confirmation", f.file(t, "c.json", f.confirmationObject(nil)), "--run", receiptRun)
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonCombination)
	})
}

// C2: the two answers must agree and be the producers' own shapes.
func TestReceiptEvidenceModeRefusesAnswersThatDoNotAgree(t *testing.T) {
	f := newReceiptCmdFixture(t)
	f.migrated(t)

	for _, c := range []struct {
		name   string
		ev     map[string]any
		conf   map[string]any
		reason string
	}{
		{"another node in the confirmation", nil, map[string]any{"node": "node-b"}, endpointReasonAgreement},
		{"another incarnation", nil, map[string]any{"incarnation": testIncarnation}, endpointReasonAgreement},
		{"another deployment", nil, map[string]any{"deployment": map[string]any{"bound": true, "id": regDeployment2}}, endpointReasonAgreement},
		{"a confirmation that timed out", nil, map[string]any{"outcome": "timeout"}, endpointReasonConfirm},
		{"an evidence that is unknown", map[string]any{"outcome": "unknown"}, nil, endpointReasonEvidence},
		{"live as a string", nil, map[string]any{"live": "true"}, endpointReasonConfirm},
		{"bound false", nil, map[string]any{"deployment": map[string]any{"bound": false, "id": f.deployment}}, endpointReasonConfirm},
		{"an extra member", map[string]any{"note": "x"}, nil, endpointReasonEvidence},
		{"a member of the wrong type", map[string]any{"schema": "1"}, nil, endpointReasonEvidence},
		{"a stopped triple that is not the proof", map[string]any{"stopped": map[string]any{"active_state": "failed",
			"sub_state": "failed", "result": "timeout"}}, nil, endpointReasonEvidence},
		{"a relative config path", map[string]any{"config_path": "billet.yaml"}, nil, endpointReasonEvidence},
		{"a non-canonical endpoint", map[string]any{"to": endpointB}, nil, endpointReasonEvidence},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Helper()
			o := f.evidence(t, f.evidenceObject(t, c.ev), f.confirmationObject(c.conf))
			mustEndpointRefusal(t, o, outcomeRefused, c.reason)
			f.noReceipt(t)
		})
	}

	t.Run("malformed", func(t *testing.T) {
		t.Helper()
		o := f.evidence(t, []byte("{\n"), f.confirmationObject(nil))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonEvidence)
	})

	t.Run("a repeated member", func(t *testing.T) {
		t.Helper()
		body := mustMarshal(t, f.confirmationObject(nil))
		repeated := strings.TrimSuffix(string(body), "}") + `,"live":true}` + "\n"

		o := f.evidence(t, f.evidenceObject(t, nil), []byte(repeated))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonConfirm)
	})

	t.Run("over the bound", func(t *testing.T) {
		t.Helper()
		o := f.evidence(t, f.evidenceObject(t, map[string]any{"node": strings.Repeat("n", maxEvidenceBytes)}), f.confirmationObject(nil))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonEvidence)
	})

	t.Run("a FIFO", func(t *testing.T) {
		t.Helper()
		fifo := filepath.Join(f.scratch(), "fifo.json")
		mustOK(t, syscall.Mkfifo(fifo, 0o600))

		done := make(chan endpointOut, 1)
		go func() {
			done <- runEndpoint(t, cmdNodeReceipt, "", "--evidence", fifo, "--confirmation",
				f.file(t, "c.json", f.confirmationObject(nil)), "--config", f.configPath, "--run", receiptRun)
		}()

		select {
		case o := <-done:
			mustEndpointRefusal(t, o, outcomeRefused, endpointReasonEvidence)
		case <-time.After(5 * time.Second):
			t.Fatal("the evidence read waited on the FIFO")
		}
	})
}

// C3: the record re-verified now, under the bracket, and the configuration
// still the one the migration judged.
func TestReceiptEvidenceModeReVerifiesTheRecordAndTheConfiguration(t *testing.T) {
	for _, c := range []struct {
		name   string
		plant  func(t *testing.T, f *receiptCmdFixture)
		reason string
		next   string
	}{
		{"a restart since the migration", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			f.setNode(t, "active", "running", inspectPID+1, nodeInvocation, "mixed")
			f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalB,
				"invocation_id": nodeInvocation, "incarnation": testIncarnation}))
		}, endpointReasonRecord, "refresh"},
		{"another endpoint dialled", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalA,
				"invocation_id": newInvocation, "incarnation": newIncarnation}))
		}, endpointReasonRecord, ""},
		{"another incarnation", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalB,
				"invocation_id": newInvocation, "incarnation": testIncarnation}))
		}, endpointReasonRecord, ""},
		{"no record", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			mustOK(t, os.Remove(f.recordPath))
		}, endpointReasonRecord, ""},
		{"another node", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalB, "node": "node-b",
				"invocation_id": newInvocation, "incarnation": newIncarnation}))
		}, endpointReasonRecord, ""},
		{"another deployment", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			f.writeRecord(t, f.record(map[string]any{"deployment": regDeployment2, "endpoint": canonicalB,
				"invocation_id": newInvocation, "incarnation": newIncarnation}))
		}, endpointReasonRecord, ""},
		{"the node not running", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			f.setNode(t, "inactive", "dead", 0, "", "mixed")
		}, endpointReasonRecord, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Helper()
			f := newReceiptCmdFixture(t)
			f.migrated(t)
			ev := f.evidenceObject(t, nil)
			c.plant(t, f)

			o := f.evidence(t, ev, f.confirmationObject(nil))
			mustEndpointRefusal(t, o, outcomeRefused, c.reason)

			if c.next != "" && !strings.Contains(o.str("next"), c.next) {
				t.Errorf("next %q, want %q", o.str("next"), c.next)
			}

			f.noReceipt(t)
		})
	}

	t.Run("a bracket that never agrees", func(t *testing.T) {
		t.Helper()
		f := newReceiptCmdFixture(t)
		f.migrated(t)
		f.alternating(t, nodeUnitBody("active", "running", inspectPID, nodeInvocation, "mixed", "success"))

		o := f.evidence(t, f.evidenceObject(t, nil), f.confirmationObject(nil))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonRecord)
		f.noReceipt(t)
	})

	t.Run("the installed digest other than the evidence's", func(t *testing.T) {
		t.Helper()
		f := newReceiptCmdFixture(t)
		f.migrated(t)

		o := f.evidence(t, f.evidenceObject(t, map[string]any{"installed_sha256": strings.Repeat("ab", 32)}), f.confirmationObject(nil))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonConfig)
	})

	t.Run("the installed endpoint other than to", func(t *testing.T) {
		t.Helper()
		f := newReceiptCmdFixture(t)
		f.migrated(t)
		f.writeConfig(t, f.rendering(endpointA))
		ev := f.evidenceObject(t, nil)

		o := f.evidence(t, ev, f.confirmationObject(nil))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonConfig)
	})
}

// C4: refresh mode writes, or leaves a current receipt alone with both
// flushes done, and follows a restart, a digest change and the record's
// arrival after a start.
func TestReceiptRefreshKeepsTheReceiptCurrent(t *testing.T) {
	f := newReceiptCmdFixture(t)
	f.migrated(t)

	o := f.refresh(t, f.rendering(endpointB))
	rec := mustWritten(t, o)

	if rec["invocation_id"] != newInvocation || rec["incarnation"] != newIncarnation || rec["effective_endpoint"] != canonicalB {
		t.Errorf("receipt %v", rec)
	}

	f.eventsAre(t, "mkdir:dir", "syncdir:parent", "syncfile", "rename", "syncdir:dir", "syncdir:parent")

	before, err := os.Lstat(f.path)
	mustOK(t, err)

	t.Run("the same invocation again", func(t *testing.T) {
		t.Helper()
		*f.events = nil

		o := f.refresh(t, f.rendering(endpointB))
		mustEndpointOutcome(t, o, outcomeCurrent)

		after, err := os.Lstat(f.path)
		mustOK(t, err)

		if !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
			t.Error("a current receipt was rewritten")
		}

		f.eventsAre(t, "syncdir:dir", "syncdir:parent")
	})

	t.Run("another run alone", func(t *testing.T) {
		t.Helper()
		o := f.refresh(t, f.rendering(endpointB), "--run", "ci-run-13")
		mustEndpointOutcome(t, o, outcomeCurrent)

		if f.receiptOnDisk(t).Run != receiptRun {
			t.Error("the run alone rewrote the receipt")
		}
	})

	t.Run("a re-registration under the same incarnation", func(t *testing.T) {
		t.Helper()
		f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalB,
			"invocation_id": newInvocation, "incarnation": newIncarnation, "registered_at": "2026-09-10T12:30:00Z"}))

		o := f.refresh(t, f.rendering(endpointB))
		mustEndpointOutcome(t, o, outcomeCurrent)
	})

	t.Run("after a restart", func(t *testing.T) {
		t.Helper()
		other := "1111111111111111111111111111aaaa"
		f.setNode(t, "active", "running", inspectPID+1, other, "mixed")
		f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalB,
			"invocation_id": other, "incarnation": testIncarnation}))

		o := f.refresh(t, f.rendering(endpointB))
		rec := mustWritten(t, o)

		if rec["invocation_id"] != other || rec["incarnation"] != testIncarnation {
			t.Errorf("receipt %v", rec)
		}
	})

	t.Run("the digest changed alone", func(t *testing.T) {
		t.Helper()
		f.writeConfig(t, f.rendering(endpointB)+"# a comment\n")
		f.touchBeforeStart(t, f.configPath)

		o := f.refresh(t, f.rendering(endpointB))
		rec := mustWritten(t, o)

		if rec["installed_sha256"] != f.installedSHA(t) {
			t.Errorf("digest %v, want %s", rec["installed_sha256"], f.installedSHA(t))
		}
	})

	t.Run("the record after a start", func(t *testing.T) {
		t.Helper()
		f := newReceiptCmdFixture(t)
		f.migrated(t)
		mustOK(t, os.Remove(f.recordPath))

		f.later(t, 60*time.Millisecond, func() {
			f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalB,
				"invocation_id": newInvocation, "incarnation": newIncarnation}))
		})

		o := f.refresh(t, f.rendering(endpointB), "--wait", "3s")
		mustWritten(t, o)
	})

	t.Run("no record within the wait", func(t *testing.T) {
		t.Helper()
		f := newReceiptCmdFixture(t)
		f.migrated(t)
		mustOK(t, os.Remove(f.recordPath))

		o := f.refresh(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonRecord)

		if !strings.Contains(o.str("why"), "200ms") {
			t.Errorf("why %q names no wait", o.str("why"))
		}

		f.noReceipt(t)
	})

	t.Run("the previous invocation throughout", func(t *testing.T) {
		t.Helper()
		f := newReceiptCmdFixture(t)
		f.migrated(t)
		f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalB,
			"invocation_id": nodeInvocation, "incarnation": testIncarnation}))

		o := f.refresh(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonRecord)
		f.noReceipt(t)
	})
}

// C5: the refresh's refusals.
func TestReceiptRefreshRefusesWhatItCannotCertify(t *testing.T) {
	for _, c := range []struct {
		name      string
		plant     func(t *testing.T, f *receiptCmdFixture)
		rendering func(f *receiptCmdFixture) string
		outcome   string
		reason    string
		words     string
	}{
		{"effective other than installed", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalA,
				"invocation_id": newInvocation, "incarnation": newIncarnation}))
		}, func(f *receiptCmdFixture) string { return f.rendering(endpointB) }, outcomeRefused, endpointReasonMigration, "migrate-endpoint"},
		{"installed other than the rendering", nil, func(f *receiptCmdFixture) string { return f.rendering("127.0.0.1:7720") },
			outcomeRefused, endpointReasonDesired, ""},
		{"the rendering without a node", nil, func(f *receiptCmdFixture) string {
			return "server:\n  listen: 127.0.0.1:7717\n  state_dir: " + f.stateDir + "\n  max_vcpu: 8\n  max_memory: 32GiB\n" +
				"github:\n  org: acme\n  app_id: 1\n  installation_id: 2\n  private_key_path: " + filepath.Join(f.scratch(), "app.pem") +
				"\ntiers:\n  - label: billet-2vcpu\n    provider: docker\n    vcpu: 2\n    memory: 8GiB\n    image: ubuntu:24.04\n"
		}, outcomeRefused, endpointReasonDesired, ""},
		{"the node not running", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			f.setNode(t, "inactive", "dead", 0, "", "mixed")
		}, func(f *receiptCmdFixture) string { return f.rendering(endpointB) }, outcomeRefused, endpointReasonNotRunning, ""},
		{"a record naming another node", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalB, "node": "node-b",
				"invocation_id": newInvocation, "incarnation": newIncarnation}))
		}, func(f *receiptCmdFixture) string { return f.rendering(endpointB) }, outcomeRefused, endpointReasonRecord, ""},
		{"a record naming another deployment", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			f.writeRecord(t, f.record(map[string]any{"deployment": regDeployment2, "endpoint": canonicalB,
				"invocation_id": newInvocation, "incarnation": newIncarnation}))
		}, func(f *receiptCmdFixture) string { return f.rendering(endpointB) }, outcomeRefused, endpointReasonRecord, ""},
		{"a record with a non-canonical endpoint", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": endpointB,
				"invocation_id": newInvocation, "incarnation": newIncarnation}))
		}, func(f *receiptCmdFixture) string { return f.rendering(endpointB) }, outcomeRefused, endpointReasonRecord, ""},
		{"a record of mode 0644", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			mustOK(t, os.Chmod(f.recordPath, 0o644))
		}, func(f *receiptCmdFixture) string { return f.rendering(endpointB) }, outcomeRefused, endpointReasonRecord, ""},
		{"a record of another owner", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			info, err := os.Lstat(f.recordPath)
			mustOK(t, err)
			f.registrationFixture.owners[inodeOf(t, info)] = 1001
		}, func(f *receiptCmdFixture) string { return f.rendering(endpointB) }, outcomeRefused, endpointReasonRecord, ""},
		{"the rendering malformed", nil, func(*receiptCmdFixture) string { return "node: [" }, outcomeRefused, endpointReasonDesired, ""},
		{"the installed configuration absent", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			mustOK(t, os.Remove(f.configPath))
		}, func(f *receiptCmdFixture) string { return f.rendering(endpointB) }, outcomeRefused, endpointReasonConfig, ""},
		{"a bracket that never agrees", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			f.alternating(t, nodeUnitBody("active", "running", inspectPID, nodeInvocation, "mixed", "success"))
		}, func(f *receiptCmdFixture) string { return f.rendering(endpointB) }, outcomeUnknown, endpointReasonUnit, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Helper()
			f := newReceiptCmdFixture(t)
			f.migrated(t)

			if c.plant != nil {
				c.plant(t, f)
			}

			o := f.refresh(t, c.rendering(f))
			mustEndpointRefusal(t, o, c.outcome, c.reason)

			if c.words != "" && !strings.Contains(o.str("next"), c.words) {
				t.Errorf("next %q, want %q", o.str("next"), c.words)
			}

			f.noReceipt(t)
		})
	}

	t.Run("darwin", func(t *testing.T) {
		t.Helper()
		f := newReceiptCmdFixture(t)
		hostOS = "darwin"

		o := f.refresh(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonPlatform)
	})

	t.Run("no --run outside a dry run", func(t *testing.T) {
		t.Helper()
		f := newReceiptCmdFixture(t)

		o := runEndpoint(t, cmdNodeReceipt, "", "--refresh", "--config", f.configPath)
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonCombination)
	})

	t.Run("--refresh beside evidence", func(t *testing.T) {
		t.Helper()
		f := newReceiptCmdFixture(t)

		o := runEndpoint(t, cmdNodeReceipt, "", "--refresh", "--evidence", "x", "--config", f.configPath, "--run", receiptRun)
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonCombination)
	})
}

// C6: the existing file is validated whole before the shortcut; what is
// not a receipt is replaced, what is not a regular file refuses.
func TestReceiptRefreshValidatesTheExistingFileBeforeTheShortcut(t *testing.T) {
	current := func(f *receiptCmdFixture) endpointReceipt {
		rec := validReceipt()
		rec.Deployment = f.deployment
		rec.InstalledSHA256 = f.installedSHA(t)

		return rec
	}

	rewritten := map[string]func(t *testing.T, f *receiptCmdFixture){
		"an extra member": func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			m := members(t, current(f))
			m["note"] = "x"
			f.writeMembers(t, m)
		},
		"a duplicate member": func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			body := mustMarshal(t, current(f))
			f.write(t, strings.TrimSuffix(string(body), "}")+`,"run":"again"}`+"\n")
		},
		"a missing run": func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			m := members(t, current(f))
			delete(m, "run")
			f.writeMembers(t, m)
		},
		"a malformed written_at": func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			m := members(t, current(f))
			m["written_at"] = "yesterday"
			f.writeMembers(t, m)
		},
		"mode 0644": func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			f.writeReceipt(t, current(f))
			mustOK(t, os.Chmod(f.path, 0o644))
		},
		"another owner": func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			f.writeReceipt(t, current(f))
			f.owners[f.inode(t)] = 1001
		},
		"over 4096 bytes": func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			m := members(t, current(f))
			m["run"] = strings.Repeat("r", 5000)
			f.writeMembers(t, m)
		},
		"garbage": func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			f.write(t, "not json\n")
		},
	}

	for name, plant := range rewritten {
		t.Run(name, func(t *testing.T) {
			t.Helper()
			f := newReceiptCmdFixture(t)
			f.migrated(t)
			mustOK(t, os.Mkdir(f.dir, 0o700))
			plant(t, f)

			// THE JUDGED INODE IS HELD OPEN through the refresh: the owner
			// mapping is by inode, and a filesystem hands a freed inode to the
			// next file at once, which the command's own read-back would then
			// judge as the planted owner's.
			if held, err := os.Open(f.path); err == nil {
				defer func() { _ = held.Close() }()
			}

			o := f.refresh(t, f.rendering(endpointB))
			mustWritten(t, o)
			clear(f.owners)

			if disk := f.receiptOnDisk(t); disk.Run != receiptRun || disk.InvocationID != newInvocation {
				t.Errorf("on disk %+v", disk)
			}

			info, err := os.Lstat(f.path)
			mustOK(t, err)

			if info.Mode().Perm() != 0o600 {
				t.Errorf("mode %04o", info.Mode().Perm())
			}
		})
	}

	refused := map[string]func(t *testing.T, f *receiptCmdFixture){
		"a symlink at the name": func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			mustOK(t, os.Mkdir(f.dir, 0o700))
			mustOK(t, os.Symlink("/etc/hostname", f.path))
		},
		"a FIFO at the name": func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			mustOK(t, os.Mkdir(f.dir, 0o700))
			mustOK(t, syscall.Mkfifo(f.path, 0o600))
		},
		"a symlinked directory": func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			target := filepath.Join(f.parent, "target")
			mustOK(t, os.Mkdir(target, 0o700))
			mustOK(t, os.Symlink(target, f.dir))
		},
		"a symlinked parent": func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			target := filepath.Join(f.scratch(), "target-parent")
			mustOK(t, os.MkdirAll(target, 0o755))
			mustOK(t, os.RemoveAll(f.parent))
			mustOK(t, os.Symlink(target, f.parent))
		},
		"a parent of another owner": func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			info, err := os.Lstat(f.parent)
			mustOK(t, err)
			f.owners[inodeOf(t, info)] = 1001
		},
	}

	for name, plant := range refused {
		t.Run(name, func(t *testing.T) {
			t.Helper()
			f := newReceiptCmdFixture(t)
			f.migrated(t)
			plant(t, f)

			done := make(chan endpointOut, 1)
			go func() { done <- f.refresh(t, f.rendering(endpointB)) }()

			select {
			case o := <-done:
				mustEndpointRefusal(t, o, outcomeRefused, endpointReasonTrust)

				if contains(*f.events, "rename") {
					t.Error("a refused shape was written")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the refresh waited")
			}
		})
	}
}

// C7: the write's interruptions leave a classified remainder, and the
// next invocation completes the flush an earlier one owed.
func TestReceiptWriteInterruptionsAreCouldNotTell(t *testing.T) {
	t.Run("the file sync failing", func(t *testing.T) {
		t.Helper()
		f := newReceiptCmdFixture(t)
		f.migrated(t)
		f.failSync["file"] = syscall.EIO

		o := f.refresh(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonTrust)
		f.noReceipt(t)

		entries, err := os.ReadDir(f.dir)
		mustOK(t, err)

		if len(entries) != 0 {
			t.Errorf("the temporary was left: %v", entries)
		}
	})

	t.Run("the directory sync failing after the rename", func(t *testing.T) {
		t.Helper()
		f := newReceiptCmdFixture(t)
		f.migrated(t)
		f.failSync["dir"] = syscall.EIO

		o := f.refresh(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonTrust)

		if !strings.Contains(o.str("why"), "write the receipt") {
			t.Errorf("why %q", o.str("why"))
		}

		f.receiptOnDisk(t)

		// The next invocation finds the receipt current and flushes both
		// directories; a flush failing again fails again.
		delete(f.failSync, "dir")
		*f.events = nil

		o = f.refresh(t, f.rendering(endpointB))
		mustEndpointOutcome(t, o, outcomeCurrent)
		f.eventsAre(t, "syncdir:dir", "syncdir:parent")

		f.failSync["parent"] = syscall.EIO

		o = f.refresh(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonTrust)
	})

	t.Run("the parent's flush failing after the directory was created", func(t *testing.T) {
		t.Helper()
		f := newReceiptCmdFixture(t)
		f.migrated(t)
		f.failSync["parent"] = syscall.EIO

		o := f.refresh(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonTrust)
		f.noReceipt(t)

		if _, err := os.Lstat(f.dir); err != nil {
			t.Errorf("the directory was not left: %v", err)
		}

		f.eventsAre(t, "mkdir:dir", "syncdir:parent")

		delete(f.failSync, "parent")
		*f.events = nil

		o = f.refresh(t, f.rendering(endpointB))
		mustWritten(t, o)
		f.eventsAre(t, "syncfile", "rename", "syncdir:dir", "syncdir:parent")
	})
}

// C8: the refresh's dry run.
func TestReceiptRefreshDryRunWritesNothing(t *testing.T) {
	t.Run("everything equal", func(t *testing.T) {
		t.Helper()
		f := newReceiptCmdFixture(t)
		f.migrated(t)

		o := f.refresh(t, f.rendering(endpointB), "--dry-run")
		mustEndpointOutcome(t, o, outcomeReported)

		rec := asMap(o.doc["receipt"])
		if rec["effective_endpoint"] != canonicalB || rec["invocation_id"] != newInvocation || rec["run"] != "" {
			t.Errorf("receipt %v", rec)
		}

		f.noReceipt(t)

		if len(*f.events) != 0 {
			t.Errorf("a dry run touched the directory: %v", *f.events)
		}
	})

	t.Run("the node not running", func(t *testing.T) {
		t.Helper()
		f := newReceiptCmdFixture(t)
		f.migrated(t)
		f.setNode(t, "inactive", "dead", 0, "", "mixed")

		o := f.refresh(t, f.rendering(endpointB), "--dry-run")
		mustEndpointOutcome(t, o, outcomeReported)

		if !strings.Contains(o.str("why"), "writes no receipt") || o.doc["receipt"] != nil {
			t.Errorf("answer %v", o.doc)
		}
	})

	t.Run("effective other than installed", func(t *testing.T) {
		t.Helper()
		f := newReceiptCmdFixture(t)
		f.migrated(t)
		f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalA,
			"invocation_id": newInvocation, "incarnation": newIncarnation}))

		o := f.refresh(t, f.rendering(endpointB), "--dry-run")
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonMigration)
	})

	t.Run("installed other than the rendering", func(t *testing.T) {
		t.Helper()
		f := newReceiptCmdFixture(t)
		f.migrated(t)

		o := f.refresh(t, f.rendering("127.0.0.1:7720"), "--dry-run")
		mustEndpointOutcome(t, o, outcomeReported)
	})

	t.Run("no record yet on a running node", func(t *testing.T) {
		t.Helper()
		f := newReceiptCmdFixture(t)
		f.migrated(t)
		mustOK(t, os.Remove(f.recordPath))

		started := time.Now()

		o := f.refresh(t, f.rendering(endpointB), "--dry-run")
		mustEndpointOutcome(t, o, outcomeReported)

		if time.Since(started) < 200*time.Millisecond || !strings.Contains(o.str("why"), "no current record") {
			t.Errorf("why %q after %s", o.str("why"), time.Since(started))
		}
	})

	// C12: over a server-only installation.
	t.Run("no node installed", func(t *testing.T) {
		t.Helper()
		f := newReceiptCmdFixture(t)
		f.writeConfig(t, "server:\n  listen: 127.0.0.1:7717\n  state_dir: "+f.stateDir+"\n  max_vcpu: 8\n  max_memory: 32GiB\n"+
			"github:\n  org: acme\n  app_id: 1\n  installation_id: 2\n  private_key_path: "+filepath.Join(f.scratch(), "app.pem")+
			"\ntiers:\n  - label: billet-2vcpu\n    provider: docker\n    vcpu: 2\n    memory: 8GiB\n    image: ubuntu:24.04\n")

		o := f.refresh(t, f.rendering(endpointB), "--dry-run")
		mustEndpointOutcome(t, o, outcomeReported)

		if !strings.Contains(o.str("why"), "no node is installed") {
			t.Errorf("why %q", o.str("why"))
		}

		o = f.refresh(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonConfig)
	})
}

// C10, C11: the closing checks in both modes: a configuration replaced,
// modified or unexaminable, or a process that moved, after the record
// was read → could-not-tell, nothing written, an existing receipt untouched.
func TestReceiptClosesTheConfigurationAndTheProcessBeforeTheWrite(t *testing.T) {
	modes := map[string]func(t *testing.T, f *receiptCmdFixture) endpointOut{
		"evidence": func(t *testing.T, f *receiptCmdFixture) endpointOut {
			t.Helper()

			return f.evidence(t, f.evidenceObject(t, nil), f.confirmationObject(nil))
		},
		"refresh": func(t *testing.T, f *receiptCmdFixture) endpointOut {
			t.Helper()

			return f.refresh(t, f.rendering(endpointB))
		},
	}

	for mode, run := range modes {
		t.Run(mode+": the configuration replaced under the record read", func(t *testing.T) {
			t.Helper()
			f := newReceiptCmdFixture(t)
			f.migrated(t)
			f.afterRecordRead(t, func() {
				tmp := f.configPath + ".new"
				writeFile(t, tmp, f.rendering(endpointB), 0o644)
				mustOK(t, os.Rename(tmp, f.configPath))
			})

			o := run(t, f)
			mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)
			f.noReceipt(t)
		})

		t.Run(mode+": the configuration modified in place", func(t *testing.T) {
			t.Helper()
			f := newReceiptCmdFixture(t)
			f.migrated(t)
			f.afterRecordRead(t, func() {
				fh, err := os.OpenFile(f.configPath, os.O_APPEND|os.O_WRONLY, 0)
				mustOK(t, err)
				_, err = fh.WriteString("# late\n")
				mustOK(t, err)
				mustOK(t, fh.Close())
			})

			o := run(t, f)
			mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)
			f.noReceipt(t)
		})

		t.Run(mode+": the closing stat failing", func(t *testing.T) {
			t.Helper()
			f := newReceiptCmdFixture(t)
			f.migrated(t)

			prev := closingStat
			closingStat = func(string) (os.FileInfo, error) { return nil, syscall.EACCES }

			t.Cleanup(func() { closingStat = prev })

			o := run(t, f)
			mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)
			f.noReceipt(t)
		})

		t.Run(mode+": the process moved after the record read", func(t *testing.T) {
			t.Helper()
			f := newReceiptCmdFixture(t)
			f.migrated(t)
			// The bracket's shows agree; the closing show (after all
			// earlier ones) sees another process.
			shows := 2
			if mode == "refresh" {
				shows = 3
			}
			f.afterShows(t, shows, nodeUnitBody("active", "running", 9999, nodeInvocation, "mixed", "success"))

			o := run(t, f)
			mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonProcess)
			f.noReceipt(t)
		})

		t.Run(mode+": a MainPID missing at the close", func(t *testing.T) {
			t.Helper()
			f := newReceiptCmdFixture(t)
			f.migrated(t)
			shows := 2
			if mode == "refresh" {
				shows = 3
			}
			f.afterShows(t, shows, "LoadState=loaded\nActiveState=active\nSubState=running\nInvocationID="+newInvocation+"\nKillMode=mixed\n")

			o := run(t, f)
			mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonProcess)
			f.noReceipt(t)
		})

		t.Run(mode+": an existing receipt untouched", func(t *testing.T) {
			t.Helper()
			f := newReceiptCmdFixture(t)
			f.migrated(t)
			mustOK(t, os.Mkdir(f.dir, 0o700))
			rec := validReceipt()
			rec.Deployment = f.deployment
			rec.InstalledSHA256 = f.installedSHA(t)
			f.writeReceipt(t, rec)

			before, err := os.Lstat(f.path)
			mustOK(t, err)

			f.afterRecordRead(t, func() {
				tmp := f.configPath + ".new"
				writeFile(t, tmp, f.rendering(endpointB), 0o644)
				mustOK(t, os.Rename(tmp, f.configPath))
			})

			o := run(t, f)
			mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)

			after, err := os.Lstat(f.path)
			mustOK(t, err)

			if !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
				t.Error("the existing receipt was touched")
			}

			if contains(*f.events, "rename") {
				t.Error("something was written")
			}
		})
	}
}

// afterRecordRead runs fn once, after the record's read and before the
// bracket's closing observation, through the reader's seam.
func (f *receiptCmdFixture) afterRecordRead(t *testing.T, fn func()) {
	t.Helper()

	prev := inspectAfterRecordRead
	ran := false
	inspectAfterRecordRead = func() {
		if !ran {
			ran = true
			fn()
		}
	}

	t.Cleanup(func() { inspectAfterRecordRead = prev })
}

// C9: the fixtures the role's parser consumes are the command's own.
func TestTheReceiptFixturesAreTheCommandsOwn(t *testing.T) {
	shapes := map[string]func(t *testing.T, f *receiptCmdFixture) endpointOut{
		"written": func(t *testing.T, f *receiptCmdFixture) endpointOut {
			t.Helper()

			return f.evidence(t, f.evidenceObject(t, nil), f.confirmationObject(nil))
		},
		"current": func(t *testing.T, f *receiptCmdFixture) endpointOut {
			t.Helper()
			mustWritten(t, f.refresh(t, f.rendering(endpointB)))

			return f.refresh(t, f.rendering(endpointB))
		},
		"reported": func(t *testing.T, f *receiptCmdFixture) endpointOut {
			t.Helper()

			return f.refresh(t, f.rendering(endpointB), "--dry-run")
		},
		"reported-not-running": func(t *testing.T, f *receiptCmdFixture) endpointOut {
			t.Helper()
			f.setNode(t, "inactive", "dead", 0, "", "mixed")

			return f.refresh(t, f.rendering(endpointB), "--dry-run")
		},
		"reported-no-node": func(t *testing.T, f *receiptCmdFixture) endpointOut {
			t.Helper()
			f.writeConfig(t, "server:\n  listen: 127.0.0.1:7717\n  state_dir: "+f.stateDir+"\n  max_vcpu: 8\n  max_memory: 32GiB\n"+
				"github:\n  org: acme\n  app_id: 1\n  installation_id: 2\n  private_key_path: "+filepath.Join(f.scratch(), "app.pem")+
				"\ntiers:\n  - label: billet-2vcpu\n    provider: docker\n    vcpu: 2\n    memory: 8GiB\n    image: ubuntu:24.04\n")

			return f.refresh(t, f.rendering(endpointB), "--dry-run")
		},
		"refused-migration": func(t *testing.T, f *receiptCmdFixture) endpointOut {
			t.Helper()
			f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalA,
				"invocation_id": newInvocation, "incarnation": newIncarnation}))

			return f.refresh(t, f.rendering(endpointB))
		},
		"refused-record": func(t *testing.T, f *receiptCmdFixture) endpointOut {
			t.Helper()
			mustOK(t, os.Remove(f.recordPath))

			return f.refresh(t, f.rendering(endpointB))
		},
		"unknown-config": func(t *testing.T, f *receiptCmdFixture) endpointOut {
			t.Helper()
			f.afterRecordRead(t, func() {
				tmp := f.configPath + ".new"
				writeFile(t, tmp, f.rendering(endpointB), 0o644)
				mustOK(t, os.Rename(tmp, f.configPath))
			})

			return f.refresh(t, f.rendering(endpointB))
		},
	}

	for name, produce := range shapes {
		t.Run(name, func(t *testing.T) {
			t.Helper()
			f := newReceiptCmdFixture(t)
			f.migrated(t)
			digest := f.installedSHA(t)

			o := produce(t, f)
			compareFixture(t, "node-receipt", name, f.normalise(o.raw, digest))

			if name == "written" {
				body, err := os.ReadFile(f.path)
				mustOK(t, err)
				compareFixture(t, "node-receipt", "receipt", f.normalise(string(body), digest))
			}
		})
	}

	fixtureSetIs(t, "node-receipt", append(fixtureNames(shapes), "receipt"))
}

// normalise spells the host-specific values as the packaged host's.
func (f *receiptCmdFixture) normalise(out, digest string) string {
	out = strings.ReplaceAll(out, f.deployment, strings.Repeat("d", 32))
	out = strings.ReplaceAll(out, digest, strings.Repeat("ab", 32))
	out = strings.ReplaceAll(out, f.configPath, "/etc/billet/billet.yaml")
	out = strings.ReplaceAll(out, f.path, "/var/lib/billet/node/endpoint-migration.json")
	out = strings.ReplaceAll(out, f.recordPath, "/run/billet/registration/current")
	out = strings.ReplaceAll(out, f.endpointFixture.dir, "/var/lib/billet-fixture")

	return out
}
