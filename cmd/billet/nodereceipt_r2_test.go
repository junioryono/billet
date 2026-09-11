package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/regularfile"
	"github.com/junioryono/billet/internal/rollout"
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
		// THE ORDER THAT MATTERS: `live` first and `LIVE` last, so a
		// case-insensitive decoder ends with true and only the exact member
		// set refuses; a marshalled map would sort LIVE first.
		rest := mustMarshal(t, f.confirmationObject(map[string]any{"live": nil}))
		raw := []byte(`{"live": false, "LIVE": true, ` + string(rest[1:]))

		o := f.evidence(t, f.evidenceObject(t, nil), raw)
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonConfirm)

		if !strings.Contains(o.str("why"), `"LIVE"`) {
			t.Errorf("why %q does not name the member", o.str("why"))
		}

		f.noReceipt(t)
	})

	// A nil override deletes the member; the null is assigned afterwards so
	// the member is present and null.
	for _, c := range []struct {
		name  string
		epoch any
	}{{"a null epoch", nil}, {"an epoch that is a string", "3"}, {"a fractional epoch", 1.5}} {
		t.Run(c.name, func(t *testing.T) {
			conf := f.confirmationObject(nil)
			conf["epoch"] = c.epoch

			o := f.evidence(t, f.evidenceObject(t, nil), conf)
			mustEndpointRefusal(t, o, outcomeRefused, endpointReasonConfirm)

			if !strings.Contains(o.str("why"), "epoch is not an integer") {
				t.Errorf("why %q", o.str("why"))
			}
		})
	}

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

		// Nothing is removed: the invalid file, or whatever replaced it, is
		// replaced by this converge's durable rename after the closing
		// checks, so the other publisher's receipt is never the one removed
		// and the name holds a valid receipt throughout.
		o := f.refresh(t, f.rendering(endpointB))
		mustWritten(t, o)

		if disk := f.receiptOnDisk(t); disk.Run != receiptRun {
			t.Errorf("on disk %+v", disk)
		}

		if contains(*f.events, "remove") {
			t.Error("something was removed")
		}
	})

	t.Run("the written receipt removed after the read-back", func(t *testing.T) {
		f := newReceiptCmdFixture(t)
		f.migrated(t)

		// The only read of the receipt's path in a refresh with no existing
		// receipt is the read-back after the write.
		afterRead(t, f, func() { mustOK(t, os.Remove(f.path)) })

		o := f.refresh(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonTrust)

		if !strings.Contains(o.str("why"), "moved after it was written") {
			t.Errorf("why %q", o.str("why"))
		}
	})
}

// The receipt directory is root's and private, existing or just created,
// for the writer and the reader alike; the parent is writable by nobody
// else.
func TestReceiptDirectoryMustBeRootsAndPrivate(t *testing.T) {
	for _, c := range []struct {
		name  string
		plant func(t *testing.T, f *receiptCmdFixture)
	}{
		{"a directory of another owner", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			mustOK(t, os.Mkdir(f.dir, 0o700))
			info, err := os.Lstat(f.dir)
			mustOK(t, err)
			f.owners[inodeOf(t, info)] = 1001
		}},
		{"a directory that is not private", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			mustOK(t, os.Mkdir(f.dir, 0o755))
			mustOK(t, os.Chmod(f.dir, 0o755))
		}},
		{"a parent writable by others", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			mustOK(t, os.Chmod(f.parent, 0o777))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newReceiptCmdFixture(t)
			f.migrated(t)
			c.plant(t, f)

			o := f.refresh(t, f.rendering(endpointB))
			mustEndpointRefusal(t, o, outcomeRefused, endpointReasonTrust)

			if contains(*f.events, "rename") {
				t.Error("something was written")
			}
		})
	}

	t.Run("the reader", func(t *testing.T) {
		for _, c := range []struct {
			name  string
			plant func(t *testing.T, f *receiptFixture)
		}{
			{"a directory of another owner", func(t *testing.T, f *receiptFixture) {
				t.Helper()
				info, err := os.Lstat(f.dir)
				mustOK(t, err)
				f.owners[inodeOf(t, info)] = 1001
			}},
			{"a directory that is not private", func(t *testing.T, f *receiptFixture) {
				t.Helper()
				mustOK(t, os.Chmod(f.dir, 0o755))
			}},
		} {
			t.Run(c.name, func(t *testing.T) {
				f := newReceiptFixture(t)
				f.writeReceipt(t, validReceipt())
				c.plant(t, f)

				if ev := readEndpointReceipt(f.path); ev.presence != receiptInvalid {
					t.Errorf("presence %s why %q, want invalid", ev.presence, ev.why)
				}
			})
		}

		t.Run("a parent writable by others", func(t *testing.T) {
			f := newReceiptFixture(t)
			f.writeReceipt(t, validReceipt())
			mustOK(t, os.Chmod(f.parent, 0o777))

			if ev := readEndpointReceipt(f.path); ev.presence != receiptInvalid {
				t.Errorf("presence %s why %q, want invalid", ev.presence, ev.why)
			}
		})

		t.Run("a parent of another owner", func(t *testing.T) {
			f := newReceiptFixture(t)
			f.writeReceipt(t, validReceipt())
			info, err := os.Lstat(f.parent)
			mustOK(t, err)
			f.owners[inodeOf(t, info)] = 1001

			if ev := readEndpointReceipt(f.path); ev.presence != receiptInvalid {
				t.Errorf("presence %s why %q, want invalid", ev.presence, ev.why)
			}
		})
	})
}

// A failed read of an input establishes nothing: could-not-tell, never a
// refusal. The failures are injected through the read seams, because a
// chmod proves nothing under root.
func TestReceiptInputsThatCannotBeReadAreCouldNotTell(t *testing.T) {
	f := newReceiptCmdFixture(t)
	f.migrated(t)

	failing := func(t *testing.T, name string) {
		t.Helper()

		prev := answerReadFile
		answerReadFile = func(path string, limit int64, opts regularfile.Options) ([]byte, error) {
			if filepath.Base(path) == name {
				return nil, &os.PathError{Op: "read", Path: path, Err: syscall.EIO}
			}

			return prev(path, limit, opts)
		}

		t.Cleanup(func() { answerReadFile = prev })
	}

	t.Run("the confirmation unreadable", func(t *testing.T) {
		failing(t, "confirmation-unreadable.json")
		conf := f.file(t, "confirmation-unreadable.json", f.confirmationObject(nil))

		o := runEndpoint(t, cmdNodeReceipt, "", "--evidence", f.file(t, "evidence-a.json", f.evidenceObject(t, nil)),
			"--confirmation", conf, "--config", f.configPath, "--run", receiptRun)
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfirm)
	})

	t.Run("the evidence unreadable", func(t *testing.T) {
		failing(t, "evidence-unreadable.json")
		ev := f.file(t, "evidence-unreadable.json", f.evidenceObject(t, nil))

		o := runEndpoint(t, cmdNodeReceipt, "", "--evidence", ev, "--confirmation",
			f.file(t, "confirmation-b.json", f.confirmationObject(nil)), "--config", f.configPath, "--run", receiptRun)
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonEvidence)
	})

	t.Run("the record unreadable", func(t *testing.T) {
		prev := registrationRead
		registrationRead = func(*os.File, string, int64) ([]byte, error) { return nil, syscall.EIO }

		t.Cleanup(func() { registrationRead = prev })

		o := f.evidence(t, f.evidenceObject(t, nil), f.confirmationObject(nil))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonRecord)

		o = f.refresh(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonRecord)
		f.noReceipt(t)
	})

	t.Run("the evidence absent", func(t *testing.T) {
		o := runEndpoint(t, cmdNodeReceipt, "", "--evidence", filepath.Join(f.scratch(), "missing.json"), "--confirmation",
			f.file(t, "confirmation-c.json", f.confirmationObject(nil)), "--config", f.configPath, "--run", receiptRun)
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonEvidence)
	})

	t.Run("the rendering unreadable", func(t *testing.T) {
		rendering := f.file(t, "rendering.yaml", []byte(f.rendering(endpointB)))

		prev := renderingReadFile
		renderingReadFile = func(path string, _ int64, _ regularfile.Options) ([]byte, error) {
			return nil, &os.PathError{Op: "read", Path: path, Err: syscall.EACCES}
		}

		t.Cleanup(func() { renderingReadFile = prev })

		o := runEndpoint(t, cmdNodeMigrate, "", "--config", f.configPath, "--desired", rendering, "--dry-run")
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonDesired)
	})

	t.Run("a rendering over the bound on stdin", func(t *testing.T) {
		big := f.rendering(endpointB) + "# " + strings.Repeat("x", maxRenderingBytes) + "\n"

		o := runEndpoint(t, cmdNodeMigrate, big, "--config", f.configPath, "--desired", "-", "--dry-run")
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonDesired)
	})
}

// A special file that appeared at the receipt's name after the read is
// never renamed over: refused, and left where it is.
func TestReceiptNeverRenamesOverASpecialFileThatAppearedAfterTheRead(t *testing.T) {
	f := newReceiptCmdFixture(t)
	f.migrated(t)
	mustOK(t, os.Mkdir(f.dir, 0o700))
	f.write(t, "not json\n")

	prev := receiptRead
	ran := false
	receiptRead = func(file *os.File, path string, limit int64) ([]byte, error) {
		body, err := prev(file, path, limit)
		if path == f.path && !ran {
			ran = true
			mustOK(t, os.Remove(f.path))
			mustOK(t, syscall.Mkfifo(f.path, 0o600))
		}

		return body, err
	}

	t.Cleanup(func() { receiptRead = prev })

	o := f.refresh(t, f.rendering(endpointB))
	mustEndpointRefusal(t, o, outcomeRefused, endpointReasonTrust)

	info, err := os.Lstat(f.path)
	mustOK(t, err)

	if info.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("the FIFO was replaced: %v", info.Mode())
	}
}

// The wait bounds every poll: a ledger that holds a query past --wait
// answers the timeout with the last row a completed poll saw.
func TestRegistrationEndsAPollTheWaitOutlasts(t *testing.T) {
	l := newRegLedger(t, true)
	l.register(t, "node-a", regIncarnationOld)

	prev := registrationPoll
	n := 0
	registrationPoll = func(ctx context.Context, store *rollout.Store) (rollout.StatusSnapshot, error) {
		n++
		if n == 2 {
			<-ctx.Done()

			return rollout.StatusSnapshot{}, ctx.Err()
		}

		return store.StatusSnapshot(ctx)
	}

	t.Cleanup(func() { registrationPoll = prev })

	started := time.Now()

	o := l.run(t)
	mustTimeout(t, o)

	if took := time.Since(started); took > 3*time.Second {
		t.Errorf("the wait took %s", took)
	}

	if last := asMap(o.doc["last"]); last["incarnation"] != regIncarnationOld {
		t.Errorf("last %v", last)
	}
}

// A parent or a directory that moved between the writer's examination and
// the existing file's read is closed as could-not-tell, never read as a
// file billet cannot replace.
func TestReceiptClosesADirectoryJudgementFromTheRead(t *testing.T) {
	f := newReceiptCmdFixture(t)
	f.migrated(t)
	mustOK(t, os.Mkdir(f.dir, 0o700))

	lstats := 0
	prev := receiptLstat
	receiptLstat = func(path string) (os.FileInfo, error) {
		info, err := prev(path)
		if path == f.parent {
			lstats++
			// The writer's examination is the first; the reader's is the
			// second, and the parent is world-writable by then.
			if lstats == 1 {
				mustOK(t, os.Chmod(f.parent, 0o777))
			}
		}

		return info, err
	}

	o := f.refresh(t, f.rendering(endpointB))
	mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonTrust)

	if strings.Contains(o.str("next"), "remove") {
		t.Errorf("next %q advises a removal over a directory judgement", o.str("next"))
	}
}

// The shortcut is closed after its flushes: a receipt or a directory that
// moved under a flush is never answered `current`.
func TestReceiptShortcutClosesAfterItsFlushes(t *testing.T) {
	for _, c := range []struct {
		name string
		move func(t *testing.T, f *receiptCmdFixture)
	}{
		{"the receipt removed under the flush", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			mustOK(t, os.Remove(f.path))
		}},
		{"the receipt's mode changed under the flush", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			mustOK(t, os.Chmod(f.path, 0o644))
		}},
		{"the directory's mode changed under the flush", func(t *testing.T, f *receiptCmdFixture) {
			t.Helper()
			mustOK(t, os.Chmod(f.dir, 0o755))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newReceiptCmdFixture(t)
			f.migrated(t)
			mustWritten(t, f.refresh(t, f.rendering(endpointB)))

			prev := receiptSyncDir
			moved := false
			receiptSyncDir = func(path string) error {
				if !moved {
					moved = true
					c.move(t, f)
				}

				return prev(path)
			}

			o := f.refresh(t, f.rendering(endpointB))
			mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonTrust)
		})
	}
}

// The closing checks judge metadata too: a directory or a receipt whose
// owner or mode moved after it was judged is could-not-tell, whatever its
// inode, size and modification time say.
func TestReceiptClosingChecksJudgeOwnershipAndMode(t *testing.T) {
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

	t.Run("the directory's mode after the read-back", func(t *testing.T) {
		f := newReceiptCmdFixture(t)
		f.migrated(t)
		afterRead(t, f, func() { mustOK(t, os.Chmod(f.dir, 0o755)) })

		o := f.refresh(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonTrust)
	})

	t.Run("the receipt's mode after the read-back", func(t *testing.T) {
		f := newReceiptCmdFixture(t)
		f.migrated(t)
		afterRead(t, f, func() { mustOK(t, os.Chmod(f.path, 0o644)) })

		o := f.refresh(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonTrust)
	})

	t.Run("the receipt's owner before the shortcut", func(t *testing.T) {
		f := newReceiptCmdFixture(t)
		f.migrated(t)
		mustWritten(t, f.refresh(t, f.rendering(endpointB)))
		afterRead(t, f, func() { f.owners[f.inode(t)] = 1001 })

		o := f.refresh(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonTrust)
	})
}
