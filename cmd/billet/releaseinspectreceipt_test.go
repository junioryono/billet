package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/regularfile"
)

// THE RECEIPT'S READER (fixtures-c4b.md, section D): typed presence, never a
// failed read as absence, the exact member set, root 0600, bounded.

// receiptFixture points the reader at a receipt under a temporary root and
// stands root in for the test's account.
type receiptFixture struct {
	dir, parent, path string
	owners            map[uint64]uint32
}

func newReceiptFixture(t *testing.T) *receiptFixture {
	t.Helper()

	root := t.TempDir()
	f := &receiptFixture{parent: filepath.Join(root, "var", "lib", "billet"), owners: map[uint64]uint32{}}
	f.dir = filepath.Join(f.parent, "node")
	f.path = filepath.Join(f.dir, "endpoint-migration.json")
	mustOK(t, os.MkdirAll(f.dir, 0o700))

	saved := struct {
		os, path string
		lstat    func(string) (os.FileInfo, error)
		open     func(string) (*os.File, os.FileInfo, error)
		read     func(*os.File, string, int64) ([]byte, error)
		owner    func(os.FileInfo) (uint32, bool)
	}{hostOS, receiptPath, receiptLstat, receiptOpen, receiptRead, receiptOwnerOf}
	t.Cleanup(func() {
		hostOS, receiptPath, receiptLstat, receiptOpen, receiptRead, receiptOwnerOf = saved.os, saved.path, saved.lstat,
			saved.open, saved.read, saved.owner
	})

	hostOS = "linux"
	receiptPath = f.path
	receiptLstat = os.Lstat
	receiptOpen = func(path string) (*os.File, os.FileInfo, error) {
		return regularfile.Open(path, regularfile.Options{NoFollow: true})
	}
	receiptRead = regularfile.ReadAllLimited
	receiptOwnerOf = func(info os.FileInfo) (uint32, bool) {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, false
		}

		if uid, mapped := f.owners[st.Ino]; mapped {
			return uid, true
		}

		return 0, true
	}

	return f
}

// validReceipt is a receipt as the command writes it.
func validReceipt() endpointReceipt {
	return endpointReceipt{
		Schema: 1, Run: "ci-run-12", Node: "node-a", Deployment: strings.Repeat("d", 32),
		InstalledSHA256: strings.Repeat("ab", 32), InstalledEndpoint: "http://127.0.0.1:7719",
		EffectiveEndpoint: "http://127.0.0.1:7719", InvocationID: newInvocation, Incarnation: newIncarnation,
		WrittenAt: "2026-09-10T12:00:00.5Z",
	}
}

func (f *receiptFixture) write(t *testing.T, body string) {
	t.Helper()
	mustOK(t, os.WriteFile(f.path, []byte(body), 0o600))
	mustOK(t, os.Chmod(f.path, 0o600))
}

func (f *receiptFixture) writeReceipt(t *testing.T, rec endpointReceipt) {
	t.Helper()

	body, err := json.MarshalIndent(rec, "", "  ")
	mustOK(t, err)
	f.write(t, string(body)+"\n")
}

// members is the receipt as a mutable object.
func members(t *testing.T, rec endpointReceipt) map[string]any {
	t.Helper()

	body, err := json.Marshal(rec)
	mustOK(t, err)

	var m map[string]any
	mustOK(t, json.Unmarshal(body, &m))

	return m
}

func (f *receiptFixture) writeMembers(t *testing.T, m map[string]any) {
	t.Helper()

	body, err := json.Marshal(m)
	mustOK(t, err)
	f.write(t, string(body)+"\n")
}

func (f *receiptFixture) inode(t *testing.T) uint64 {
	t.Helper()

	info, err := os.Lstat(f.path)
	mustOK(t, err)

	return inodeOf(t, info)
}

// D1, D2, D8, D9: present with the ten members, absent on the positive
// ENOENT, unknown off Linux; the presence is the file's and no unit is
// consulted.
func TestTheReceiptReaderTypesPresenceAndAbsence(t *testing.T) {
	f := newReceiptFixture(t)
	f.writeReceipt(t, validReceipt())

	ev := readEndpointReceipt(f.path)
	if ev.presence != receiptPresent || ev.receipt == nil || *ev.receipt != validReceipt() {
		t.Fatalf("presence %s why %q receipt %+v", ev.presence, ev.why, ev.receipt)
	}

	host := hostEndpointReceipt()
	if !host.known {
		t.Fatalf("the host's receipt is unknown: %s", host.why)
	}

	report := asReport(t, host)
	if report.Presence != "present" || report.Receipt == nil || report.Why != "" {
		t.Errorf("report %+v", report)
	}

	// D7: the field set of the report and of the receipt, pinned.
	body, err := json.Marshal(report)
	mustOK(t, err)

	var raw map[string]json.RawMessage
	mustOK(t, json.Unmarshal(body, &raw))

	if len(raw) != 2 || raw["presence"] == nil || raw["receipt"] == nil {
		t.Errorf("the report's members: %s", body)
	}

	var rec map[string]json.RawMessage
	mustOK(t, json.Unmarshal(raw["receipt"], &rec))

	if len(rec) != len(receiptFields) {
		t.Errorf("the receipt's members: %s", raw["receipt"])
	}

	for _, name := range receiptFields {
		if rec[name] == nil {
			t.Errorf("the receipt lacks %q", name)
		}
	}

	t.Run("absent file", func(t *testing.T) {
		t.Helper()
		mustOK(t, os.Remove(f.path))

		if ev := readEndpointReceipt(f.path); ev.presence != receiptAbsent {
			t.Errorf("presence %s why %q", ev.presence, ev.why)
		}

		if r := asReport(t, hostEndpointReceipt()); r.Presence != "absent" || r.Receipt != nil {
			t.Errorf("report %+v", r)
		}
	})

	t.Run("absent directory", func(t *testing.T) {
		t.Helper()
		mustOK(t, os.RemoveAll(f.dir))

		if ev := readEndpointReceipt(f.path); ev.presence != receiptAbsent {
			t.Errorf("presence %s why %q", ev.presence, ev.why)
		}
	})

	t.Run("darwin", func(t *testing.T) {
		t.Helper()
		hostOS = "darwin"

		if host := hostEndpointReceipt(); host.known {
			t.Errorf("a Mac reported a receipt: %+v", host.value)
		}
	})
}

// D3: a failed examination, open, reopen or read is unknown and never
// absent.
func TestTheReceiptReaderNeverReadsAFailureAsAbsence(t *testing.T) {
	f := newReceiptFixture(t)
	f.writeReceipt(t, validReceipt())

	for _, c := range []struct {
		name string
		seam func()
	}{
		{"the directory's examination refused", func() {
			receiptLstat = func(string) (os.FileInfo, error) { return nil, syscall.EACCES }
		}},
		{"the directory's examination failing with EIO", func() {
			receiptLstat = func(string) (os.FileInfo, error) { return nil, syscall.EIO }
		}},
		{"the open failing", func() {
			receiptOpen = func(string) (*os.File, os.FileInfo, error) { return nil, nil, syscall.EACCES }
		}},
		{"the reopen failing on an absent name", func() {
			receiptOpen = func(string) (*os.File, os.FileInfo, error) {
				return nil, nil, fmt.Errorf("%w: %w", regularfile.ErrReopen, os.ErrNotExist)
			}
		}},
		{"the read failing", func() {
			receiptRead = func(*os.File, string, int64) ([]byte, error) { return nil, syscall.EIO }
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Helper()
			f := newReceiptFixture(t)
			f.writeReceipt(t, validReceipt())
			c.seam()

			ev := readEndpointReceipt(f.path)
			if ev.presence != receiptUnreadable || ev.why == "" {
				t.Errorf("presence %s why %q, want unreadable", ev.presence, ev.why)
			}

			if host := hostEndpointReceipt(); host.known {
				t.Errorf("the host reported %+v over a failed read", host.value)
			}
		})
	}
}

// D4: everything that is not a receipt billet wrote is invalid, with why.
func TestTheReceiptReaderRefusesEverythingThatIsNotAReceipt(t *testing.T) {
	cases := map[string]func(t *testing.T, f *receiptFixture){
		"garbage": func(t *testing.T, f *receiptFixture) {
			t.Helper()
			f.write(t, "not json\n")
		},
		"an array": func(t *testing.T, f *receiptFixture) {
			t.Helper()
			f.write(t, "[]\n")
		},
		"bytes after the object": func(t *testing.T, f *receiptFixture) {
			t.Helper()
			body := mustMarshal(t, validReceipt())
			f.write(t, string(body)+"\n{}\n")
		},
		"an extra member": func(t *testing.T, f *receiptFixture) {
			t.Helper()
			m := members(t, validReceipt())
			m["note"] = "x"
			f.writeMembers(t, m)
		},
		"a duplicate member": func(t *testing.T, f *receiptFixture) {
			t.Helper()
			body := mustMarshal(t, validReceipt())
			s := strings.TrimSuffix(string(body), "}") + `,"run":"again"}` + "\n"
			f.write(t, s)
		},
		"schema 2": func(t *testing.T, f *receiptFixture) {
			t.Helper()
			m := members(t, validReceipt())
			m["schema"] = 2
			f.writeMembers(t, m)
		},
		"a 31-hex invocation": func(t *testing.T, f *receiptFixture) {
			t.Helper()
			m := members(t, validReceipt())
			m["invocation_id"] = newInvocation[:31]
			f.writeMembers(t, m)
		},
		"a 31-hex incarnation": func(t *testing.T, f *receiptFixture) {
			t.Helper()
			m := members(t, validReceipt())
			m["incarnation"] = newIncarnation[:31]
			f.writeMembers(t, m)
		},
		"a short digest": func(t *testing.T, f *receiptFixture) {
			t.Helper()
			m := members(t, validReceipt())
			m["installed_sha256"] = "abcd"
			f.writeMembers(t, m)
		},
		"a non-canonical endpoint": func(t *testing.T, f *receiptFixture) {
			t.Helper()
			m := members(t, validReceipt())
			m["installed_endpoint"] = "127.0.0.1:7719"
			f.writeMembers(t, m)
		},
		"a malformed time": func(t *testing.T, f *receiptFixture) {
			t.Helper()
			m := members(t, validReceipt())
			m["written_at"] = "yesterday"
			f.writeMembers(t, m)
		},
		"an invalid run": func(t *testing.T, f *receiptFixture) {
			t.Helper()
			m := members(t, validReceipt())
			m["run"] = "has space"
			f.writeMembers(t, m)
		},
		"mode 0644": func(t *testing.T, f *receiptFixture) {
			t.Helper()
			f.writeReceipt(t, validReceipt())
			mustOK(t, os.Chmod(f.path, 0o644))
		},
		"another owner": func(t *testing.T, f *receiptFixture) {
			t.Helper()
			f.writeReceipt(t, validReceipt())
			f.owners[f.inode(t)] = 1001
		},
		"over the bound at admission": func(t *testing.T, f *receiptFixture) {
			t.Helper()
			m := members(t, validReceipt())
			m["run"] = strings.Repeat("r", 5000)
			f.writeMembers(t, m)
		},
		"grown after admission": func(t *testing.T, f *receiptFixture) {
			t.Helper()
			f.writeReceipt(t, validReceipt())
			receiptRead = func(*os.File, string, int64) ([]byte, error) { return nil, regularfile.ErrTooLarge }
		},
	}

	for _, name := range receiptFields {
		cases["missing "+name] = func(t *testing.T, f *receiptFixture) {
			t.Helper()
			m := members(t, validReceipt())
			delete(m, name)
			f.writeMembers(t, m)
		}
		cases["null "+name] = func(t *testing.T, f *receiptFixture) {
			t.Helper()
			m := members(t, validReceipt())
			m[name] = nil
			f.writeMembers(t, m)
		}
		cases["wrong type "+name] = func(t *testing.T, f *receiptFixture) {
			t.Helper()
			m := members(t, validReceipt())
			if name == "schema" {
				m[name] = "1"
			} else {
				m[name] = 7
			}
			f.writeMembers(t, m)
		}
		cases["empty "+name] = func(t *testing.T, f *receiptFixture) {
			t.Helper()
			m := members(t, validReceipt())
			if name == "schema" {
				m[name] = 0
			} else {
				m[name] = ""
			}
			f.writeMembers(t, m)
		}
	}

	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			t.Helper()
			f := newReceiptFixture(t)
			plant(t, f)

			ev := readEndpointReceipt(f.path)
			if ev.presence != receiptInvalid || ev.why == "" {
				t.Errorf("presence %s why %q, want invalid", ev.presence, ev.why)
			}

			r := asReport(t, hostEndpointReceipt())
			if r.Presence != "invalid" || r.Receipt != nil || r.Why == "" {
				t.Errorf("report %+v", r)
			}
		})
	}
}

// D5, D6: a symlink at the name or at the directory is not a receipt, and a
// FIFO is refused without a wait.
func TestTheReceiptReaderNeverFollowsALinkOrWaitsOnAFIFO(t *testing.T) {
	t.Run("a symlinked name", func(t *testing.T) {
		t.Helper()
		f := newReceiptFixture(t)
		target := filepath.Join(f.dir, "elsewhere.json")
		body := mustMarshal(t, validReceipt())
		mustOK(t, os.WriteFile(target, append(body, '\n'), 0o600))
		mustOK(t, os.Symlink(target, f.path))

		if ev := readEndpointReceipt(f.path); ev.presence == receiptPresent || ev.presence == receiptAbsent {
			t.Errorf("presence %s why %q", ev.presence, ev.why)
		}
	})

	t.Run("a symlinked directory", func(t *testing.T) {
		t.Helper()
		f := newReceiptFixture(t)
		mustOK(t, os.RemoveAll(f.dir))
		target := filepath.Join(f.parent, "target")
		mustOK(t, os.Mkdir(target, 0o700))
		mustOK(t, os.Symlink(target, f.dir))
		body := mustMarshal(t, validReceipt())
		mustOK(t, os.WriteFile(filepath.Join(target, "endpoint-migration.json"), append(body, '\n'), 0o600))

		if ev := readEndpointReceipt(f.path); ev.presence != receiptInvalid {
			t.Errorf("presence %s why %q", ev.presence, ev.why)
		}
	})

	t.Run("a FIFO", func(t *testing.T) {
		t.Helper()
		f := newReceiptFixture(t)
		mustOK(t, syscall.Mkfifo(f.path, 0o600))

		done := make(chan receiptEvidence, 1)
		go func() { done <- readEndpointReceipt(f.path) }()

		select {
		case ev := <-done:
			if ev.presence == receiptPresent || ev.presence == receiptAbsent {
				t.Errorf("presence %s why %q", ev.presence, ev.why)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the reader waited on the FIFO")
		}
	})

	t.Run("swapped for a link after the examination", func(t *testing.T) {
		t.Helper()
		f := newReceiptFixture(t)
		f.writeReceipt(t, validReceipt())

		// The name is replaced by a symlink between the directory's
		// examination and the open: the open refuses the link.
		prev := receiptLstat
		receiptLstat = func(path string) (os.FileInfo, error) {
			info, err := prev(path)
			if err == nil && path == f.dir {
				mustOK(t, os.Remove(f.path))
				mustOK(t, os.Symlink("/etc/hostname", f.path))
			}

			return info, err
		}

		if ev := readEndpointReceipt(f.path); ev.presence == receiptPresent {
			t.Errorf("a link swapped in after the examination was read: %+v", ev.receipt)
		}
	})

	t.Run("a regular file swapped over the link before the open", func(t *testing.T) {
		t.Helper()
		f := newReceiptFixture(t)
		mustOK(t, os.Symlink("/etc/hostname", f.path))

		prev := receiptLstat
		receiptLstat = func(path string) (os.FileInfo, error) {
			info, err := prev(path)
			if err == nil && path == f.dir {
				mustOK(t, os.Remove(f.path))
				f.writeReceipt(t, validReceipt())
			}

			return info, err
		}

		if ev := readEndpointReceipt(f.path); ev.presence != receiptPresent {
			t.Errorf("the regular file the open found: presence %s why %q", ev.presence, ev.why)
		}
	})
}

// The reopen error is what the reader relies on to tell a vanished file
// from a failed reopen.
func TestTheRegularFileReopenErrorIsDistinct(t *testing.T) {
	err := fmt.Errorf("%w: %w", regularfile.ErrReopen, os.ErrNotExist)
	if !errors.Is(err, regularfile.ErrReopen) || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the composed error lost one of its causes")
	}
}
