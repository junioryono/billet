package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/durablefile"
)

// The second review round's receipt and registration cases: the answers'
// exact member sets, the controller's identity at every poll, the receipt
// directory's identity across the write, and the existing file's identity
// across the shortcut and the removal.

// A member name that differs only in case is not the member: Go's decoder
// would match it, and the last one would win.
func TestReceiptRefusesMemberAliases(t *testing.T) {
	f := newReceiptCmdFixture(t)
	f.migrated(t)

	t.Run("live beside LIVE in the confirmation", func(t *testing.T) {
		o := f.evidence(t, f.evidenceObject(t, nil), f.confirmationObject(map[string]any{"live": false, "LIVE": true}))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonConfirm)
		f.noReceipt(t)
	})

	t.Run("Node beside node in the evidence", func(t *testing.T) {
		o := f.evidence(t, f.evidenceObject(t, map[string]any{"Node": "node-b"}), f.confirmationObject(nil))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonEvidence)
	})

	t.Run("Result beside result in the stopped triple", func(t *testing.T) {
		o := f.evidence(t, f.evidenceObject(t, map[string]any{"stopped": map[string]any{"active_state": "inactive",
			"sub_state": "dead", "result": "success", "Result": "timeout"}}), f.confirmationObject(nil))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonEvidence)
	})

	t.Run("Bound beside bound in the deployment", func(t *testing.T) {
		o := f.evidence(t, f.evidenceObject(t, nil), f.confirmationObject(map[string]any{"deployment": map[string]any{
			"bound": true, "Bound": false, "id": f.deployment}}))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonConfirm)
	})
}

// An absent identity proves nothing, so it confirms nothing, at whichever
// poll it is found absent.
func TestRegistrationRequiresTheIdentityAtEveryPoll(t *testing.T) {
	l := newRegLedger(t, true)
	l.polls(t, func(n int) {
		if n == 1 {
			l.register(t, "node-a", regIncarnationNew)
			mustOK(t, os.Remove(filepath.Join(l.stateDir, "deployment-id")))
		}
	})

	o := l.run(t)
	mustEndpointRefusal(t, o, outcomeRefused, endpointReasonTrust)

	if !strings.Contains(o.str("why"), "no deployment identity") {
		t.Errorf("why %q", o.str("why"))
	}
}

// The receipt directory examined is the one written into and the one the
// answer names: a directory swapped for a link after the examination is
// could-not-tell, whether the swap lands before the write or under it.
func TestReceiptWriteIsBoundToTheExaminedDirectory(t *testing.T) {
	swap := func(t *testing.T, f *receiptCmdFixture) {
		t.Helper()

		target := f.dir + "-real"
		mustOK(t, os.Rename(f.dir, target))
		mustOK(t, os.Symlink(target, f.dir))
	}

	t.Run("swapped after the existing file's read", func(t *testing.T) {
		f := newReceiptCmdFixture(t)
		f.migrated(t)
		mustOK(t, os.Mkdir(f.dir, 0o700))

		lstats := 0
		prev := receiptLstat
		receiptLstat = func(path string) (os.FileInfo, error) {
			info, err := prev(path)
			if path == f.dir {
				lstats++
				if lstats == 2 {
					swap(t, f)
				}
			}

			return info, err
		}

		o := f.refresh(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonTrust)

		if contains(*f.events, "rename") {
			t.Error("the receipt was written into a directory that was not the one examined")
		}
	})

	t.Run("swapped under the write", func(t *testing.T) {
		f := newReceiptCmdFixture(t)
		f.migrated(t)
		mustOK(t, os.Mkdir(f.dir, 0o700))

		prevInstaller := receiptInstaller
		receiptInstaller = func() durablefile.Installer {
			inst := prevInstaller()
			rename := inst.Rename
			inst.Rename = func(from, to string) error {
				swap(t, f)

				return rename(from, to)
			}

			return inst
		}

		o := f.refresh(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonTrust)

		if o.str("outcome") == outcomeWritten {
			t.Error("written, over a directory that moved under the write")
		}
	})
}

// The file judged is the file the shortcut and the removal act on: a
// receipt removed after its read is never `current`, and an invalid file
// replaced by a valid receipt after its read is never removed.
func TestReceiptShortcutAndRemovalActOnTheFileJudged(t *testing.T) {
	afterRead := func(t *testing.T, f *receiptCmdFixture, fn func()) {
		t.Helper()

		prev := receiptRead
		ran := false
		receiptRead = func(file *os.File, path string, limit int64) ([]byte, error) {
			body, err := prev(file, path, limit)
			if path == f.path && !ran {
				ran = true
				fn()
			}

			return body, err
		}
	}

	t.Run("a current receipt removed after its read", func(t *testing.T) {
		f := newReceiptCmdFixture(t)
		f.migrated(t)
		mustWritten(t, f.refresh(t, f.rendering(endpointB)))

		afterRead(t, f, func() { mustOK(t, os.Remove(f.path)) })

		o := f.refresh(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonTrust)

		if !strings.Contains(o.str("why"), "moved after it was read") {
			t.Errorf("why %q", o.str("why"))
		}
	})

	t.Run("an invalid file replaced by a receipt after its read", func(t *testing.T) {
		f := newReceiptCmdFixture(t)
		f.migrated(t)
		mustOK(t, os.Mkdir(f.dir, 0o700))
		f.write(t, "not json\n")

		valid := validReceipt()
		valid.Deployment = f.deployment
		valid.InstalledSHA256 = f.installedSHA(t)
		valid.Run = "ci-run-99"

		afterRead(t, f, func() {
			tmp := f.path + ".valid"
			body := mustMarshal(t, valid)
			writeFile(t, tmp, string(body)+"\n", 0o600)
			mustOK(t, os.Chmod(tmp, 0o600))
			mustOK(t, os.Rename(tmp, f.path))
		})

		o := f.refresh(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonTrust)

		if disk := f.receiptOnDisk(t); disk.Run != "ci-run-99" {
			t.Errorf("the receipt installed after the read was removed: %+v", disk)
		}
	})
}
