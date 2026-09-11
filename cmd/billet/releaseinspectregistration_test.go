package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/regularfile"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// The inspector's registration fixtures of PR 6b commit 3 (fixtures-c3.md,
// section I). They share the inspector's package-level seams and are serial.

const (
	nodeInvocation   = "0123456789abcdef0123456789abcdef"
	serverInvocation = "fedcba9876543210fedcba9876543210"
	testIncarnation  = "00112233445566778899aabbccddeeff"
)

// registrationFixture is the inspector fixture with a node running, the
// record's path pointed under the fixture, the record owned by root through
// the owner seam (a test does not run as root), and the server unit under a
// different invocation than the node's.
type registrationFixture struct {
	*inspectFixture
	recordDir, recordPath string
	// owners maps an inode to the uid the open seam reports for it; an inode
	// not in the map is reported as the file's real owner.
	owners map[uint64]uint32
	// ops counts the registration reader's filesystem operations by seam.
	ops map[string]int
}

func newRegistrationFixture(t *testing.T, configBody string) *registrationFixture {
	t.Helper()

	f := &registrationFixture{inspectFixture: newInspectFixture(t), owners: map[uint64]uint32{}, ops: map[string]int{}}
	f.recordDir = filepath.Join(f.dir, "registration")
	f.recordPath = filepath.Join(f.recordDir, "current")

	if err := os.Mkdir(f.recordDir, 0o750); err != nil {
		t.Fatal(err)
	}

	savedPath, savedLstat, savedOpen, savedRead := registrationRecordPath, registrationLstat, registrationOpen, registrationRead
	t.Cleanup(func() {
		registrationRecordPath, registrationLstat, registrationOpen, registrationRead = savedPath, savedLstat, savedOpen, savedRead
		inspectAfterRecordRead = nil
	})

	registrationRecordPath = f.recordPath
	registrationLstat = func(path string) (os.FileInfo, error) {
		f.ops["lstat"]++

		return os.Lstat(path)
	}
	registrationOpen = func(path string) (*os.File, os.FileInfo, error) {
		f.ops["open"]++

		file, info, err := regularfile.Open(path, regularfile.Options{NoFollow: true})
		if err != nil {
			return nil, nil, err
		}

		return file, f.owned(info), nil
	}
	registrationRead = func(file *os.File, path string, limit int64) ([]byte, error) {
		f.ops["read"]++

		return regularfile.ReadAllLimited(file, path, limit)
	}

	f.writeConfig(t, configBody)
	f.unitAbsent(t, "billet-server.service")
	f.unitRunning(t, "billet-node.service", "node", f.configPath, nil)
	f.process(t, []string{f.binPath, "node", "--config", f.configPath}, nil)
	f.touchBeforeStart(t, f.configPath)

	return f
}

// owned is the fstat the open seam hands the reader: the real one, its owner
// replaced by the fixture's mapping for that inode (root unless mapped).
func (f *registrationFixture) owned(info os.FileInfo) os.FileInfo {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info
	}

	uid, mapped := f.owners[st.Ino]
	if !mapped {
		uid = 0
	}

	copied := *st
	copied.Uid = uid

	return ownedInfo{FileInfo: info, sys: &copied}
}

// ownedInfo is a FileInfo whose Sys is a Stat_t of the fixture's choosing.
type ownedInfo struct {
	os.FileInfo
	sys any
}

func (o ownedInfo) Sys() any { return o.sys }

// record is a valid record for the fixture's node, with the given overrides.
func (f *registrationFixture) record(overrides map[string]any) map[string]any {
	rec := map[string]any{
		"schema": 1, "node": "node-a", "deployment": "dep-1234", "incarnation": testIncarnation,
		"invocation_id": nodeInvocation, "endpoint": "https://10.0.0.5:7717", "registered_at": "2026-09-09T12:00:00Z",
	}

	for k, v := range overrides {
		if v == nil {
			delete(rec, k)
		} else {
			rec[k] = v
		}
	}

	return rec
}

func (f *registrationFixture) writeRecord(t *testing.T, rec map[string]any) {
	t.Helper()

	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}

	f.writeRecordRaw(t, string(body)+"\n")
}

func (f *registrationFixture) writeRecordRaw(t *testing.T, body string) {
	t.Helper()

	// PUBLISHED AS THE NODE PUBLISHES IT: written beside the name and renamed
	// over it, so no reader ever sees an empty or partial file.
	tmp := f.recordPath + ".tmp"
	writeFile(t, tmp, body, 0o600)
	// The umask may have narrowed nothing; the mode is what the reader judges.
	if err := os.Chmod(tmp, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(tmp, f.recordPath); err != nil {
		t.Fatal(err)
	}
}

// nodeProperty rewrites one rendered property of the node unit.
func (f *registrationFixture) nodeProperty(t *testing.T, name, value string) {
	t.Helper()

	path := filepath.Join(f.unitsDir, "billet-node.service")

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var out []string

	for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if strings.HasPrefix(line, name+"=") {
			line = name + "=" + value
		}

		out = append(out, line)
	}

	writeFile(t, path, strings.Join(out, "\n")+"\n", 0o644)
}

// nodeTLSFixture is a TLS node whose bundle names node-a in deployment
// dep-1234, with or without a configured node.name.
func nodeTLSFixture(t *testing.T, withName bool) *registrationFixture {
	t.Helper()

	base := newInspectFixture(t)

	ca, err := wirecert.LoadOrCreateCA(base.stateDir, "dep-1234")
	if err != nil {
		t.Fatal(err)
	}

	bundle, err := ca.IssueNode("node-a")
	if err != nil {
		t.Fatal(err)
	}

	cert, key, caFile := filepath.Join(base.dir, "node.crt"), filepath.Join(base.dir, "node.key"), filepath.Join(base.dir, "ca.crt")
	writeFile(t, cert, string(bundle.CertPEM), 0o644)
	writeFile(t, key, string(bundle.KeyPEM), 0o600)
	writeFile(t, caFile, string(bundle.CAPEM), 0o644)

	body := base.nodeTLSConfig(cert, key, caFile)
	if !withName {
		body = strings.Replace(body, "  name: node-a\n", "", 1)
	}

	// The base fixture above is discarded for its state; the registration
	// fixture builds its own on the same body.
	f := newRegistrationFixture(t, body)
	f.stateDir = base.stateDir

	return f
}

func registrationOf(t *testing.T, r inspectReport) registrationReport {
	t.Helper()

	v := mustKnown(t, "host.registration", r.Host.Registration)

	rec, ok := v.(registrationReport)
	if !ok {
		t.Fatalf("host.registration is %T, want the report", v)
	}

	return rec
}

// I2: CURRENCY. The record is known only when its invocation is the node
// unit's, and every field of a known report is the record's.
func TestReleaseInspectRegistrationIsCurrentOnlyForTheNodesInvocation(t *testing.T) {
	f := nodeTLSFixture(t, true)
	f.writeRecord(t, f.record(nil))

	r := f.report(t)
	rec := registrationOf(t, r)

	if rec != (registrationReport{Node: "node-a", Deployment: "dep-1234", Incarnation: testIncarnation,
		InvocationID: nodeInvocation, Endpoint: "https://10.0.0.5:7717", RegisteredAt: "2026-09-09T12:00:00Z"}) {
		t.Errorf("host.registration = %+v", rec)
	}

	if mustKnown(t, "node invocation_id", r.Services["node"].InvocationID) != nodeInvocation {
		t.Errorf("services.node.invocation_id = %v", r.Services["node"].InvocationID.value)
	}

	// The server unit is absent here; its invocation is null.
	if mustKnown(t, "server invocation_id", r.Services["server"].InvocationID) != nil {
		t.Errorf("an absent server's invocation_id = %v, want null", r.Services["server"].InvocationID.value)
	}

	for name, c := range map[string]struct {
		record map[string]any
		unit   string
		want   string
	}{
		"the server's invocation": {f.record(map[string]any{"invocation_id": serverInvocation}), nodeInvocation, "written by invocation " + serverInvocation + " and the node unit is invocation " + nodeInvocation},
		"another invocation":      {f.record(map[string]any{"invocation_id": "abcdefabcdefabcdefabcdefabcdefab"}), nodeInvocation, "written by invocation abcdefabcdefabcdefabcdefabcdefab and the node unit is invocation " + nodeInvocation},
		"the unit's is empty":     {f.record(nil), "", "reports no invocation"},
		"the record's is empty":   {f.record(map[string]any{"invocation_id": ""}), nodeInvocation, `"invocation_id" is empty`},
	} {
		t.Run(name, func(t *testing.T) {
			f.writeRecord(t, c.record)
			f.nodeProperty(t, "InvocationID", c.unit)

			mustUnknown(t, "host.registration", f.report(t).Host.Registration, c.want)
		})
	}

	t.Run("the node is not running", func(t *testing.T) {
		f.nodeProperty(t, "InvocationID", nodeInvocation)
		f.writeRecord(t, f.record(nil))
		f.unitAbsent(t, "billet-node.service")

		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "the node is not running")
	})

	t.Run("a draining node is running", func(t *testing.T) {
		// systemd reports `deactivating` for the whole drain, during which the
		// process serves what it holds and can re-register; its sample binds.
		f := nodeTLSFixture(t, true)
		f.writeRecord(t, f.record(nil))
		f.nodeProperty(t, "ActiveState", "deactivating")
		f.nodeProperty(t, "SubState", "stop-sigterm")

		if rec := registrationOf(t, f.report(t)); rec.InvocationID != nodeInvocation {
			t.Errorf("a draining node's registration reads %+v", rec)
		}
	})

	t.Run("a failed unit with a stale main pid is not running", func(t *testing.T) {
		f := nodeTLSFixture(t, true)
		f.writeRecord(t, f.record(nil))
		f.nodeProperty(t, "ActiveState", "failed")
		f.nodeProperty(t, "SubState", "failed")

		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "the node is not running")
	})
}

// I2, THE SAMPLE BINDS EVERYTHING: the record is read between the identity
// reads, so an identity that moves after it discards the record with the
// process evidence, and evidence of two incarnations is never reported
// together.
func TestReleaseInspectRegistrationIsBoundToTheSample(t *testing.T) {
	t.Run("a restart after the record read is unknown throughout", func(t *testing.T) {
		f := nodeTLSFixture(t, true)
		f.writeRecord(t, f.record(nil))

		newInvocation := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		moved := false

		inspectAfterRecordRead = func() {
			if moved {
				return
			}

			moved = true
			// THE NODE RESTARTED: a new invocation, a new pid, a new image, a new
			// record, and nothing moves back.
			f.nodeProperty(t, "InvocationID", newInvocation)
			f.nodeProperty(t, "MainPID", strconv.Itoa(inspectPID+1))
			f.processAs(t, inspectPID+1, "IMAGE-B\n")
			f.writeRecord(t, f.record(map[string]any{"invocation_id": newInvocation, "incarnation": "ffffffffffffffffffffffffffffffff"}))
		}

		r := f.report(t)
		node := r.Services["node"]

		// EVIDENCE OF TWO INCARNATIONS IS NEVER REPORTED TOGETHER: the sample
		// that began on the old identity cannot agree with the new one, so the
		// process evidence and the registration are unknown alike, naming the
		// observation, and the record of the new incarnation is not reported
		// beside the old unit's identity.
		mustUnknown(t, "running_sha256", node.RunningSHA256, "restarted during the observation")
		mustUnknown(t, "host.registration", r.Host.Registration, "restarted during the observation")
	})

	t.Run("a move the retry sees undone settles on the identity the sample began with", func(t *testing.T) {
		f := nodeTLSFixture(t, true)
		f.writeRecord(t, f.record(nil))

		newInvocation := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		calls, opens := 0, 0
		exe := filepath.Join(f.procDir, strconv.Itoa(inspectPID), "exe")

		inspectAfterRecordRead = func() {
			calls++

			if calls == 1 {
				// Moved under the first sample, after its record read: another
				// invocation and its record.
				f.nodeProperty(t, "InvocationID", newInvocation)
				f.writeRecord(t, f.record(map[string]any{"invocation_id": newInvocation, "incarnation": "ffffffffffffffffffffffffffffffff"}))
			}
		}

		inspectAfterOpen = func(path string) {
			if path != exe {
				return
			}

			opens++

			if opens == 2 {
				// Moved back at the retry's start, before its record read: the
				// retry reads the restored record and agrees with the identity
				// the sample began with.
				f.nodeProperty(t, "InvocationID", nodeInvocation)
				f.writeRecord(t, f.record(nil))
			}
		}

		r := f.report(t)
		if mustKnown(t, "running_sha256", r.Services["node"].RunningSHA256) != shaOf("IMAGE-A\n") {
			t.Errorf("the settled sample reports %v", r.Services["node"].RunningSHA256.value)
		}

		// The retry read the restored record; the first sample's evidence, and
		// the other incarnation's record, went with the discarded sample.
		if rec := registrationOf(t, r); rec.Incarnation != testIncarnation || rec.InvocationID != nodeInvocation {
			t.Errorf("the settled registration is %+v", rec)
		}

		if calls != 2 {
			t.Errorf("the sample was taken %d times, want two", calls)
		}
	})

	t.Run("a record replaced after the sample read it is not the report's", func(t *testing.T) {
		f := nodeTLSFixture(t, true)
		f.writeRecord(t, f.record(nil))

		replaced := false

		inspectAfterRecordRead = func() {
			if replaced {
				return
			}

			replaced = true
			// THE RECORD MOVES AND THE IDENTITY DOES NOT: the unit confirms, the
			// sample stands, and the registration it carries is the one it
			// read inside the sample, never the file at the name afterwards
			// (which here names an invocation this unit is not).
			f.writeRecord(t, f.record(map[string]any{"invocation_id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "incarnation": "ffffffffffffffffffffffffffffffff"}))
		}

		r := f.report(t)
		if rec := registrationOf(t, r); rec.Incarnation != testIncarnation || rec.InvocationID != nodeInvocation {
			t.Errorf("the registration is %+v, want the one the sample read", rec)
		}
	})

	t.Run("a pid-only move is unknown throughout too", func(t *testing.T) {
		f := nodeTLSFixture(t, true)
		f.writeRecord(t, f.record(nil))

		moved := false

		inspectAfterRecordRead = func() {
			if moved {
				return
			}

			moved = true
			f.nodeProperty(t, "MainPID", strconv.Itoa(inspectPID+1))
			f.processAs(t, inspectPID+1, "IMAGE-B\n")
		}

		r := f.report(t)
		mustUnknown(t, "running_sha256", r.Services["node"].RunningSHA256, "restarted during the observation")
		mustUnknown(t, "host.registration", r.Host.Registration, "restarted during the observation")
	})

	t.Run("a confirming read that fails discards the sample whole", func(t *testing.T) {
		f := nodeTLSFixture(t, true)
		f.writeRecord(t, f.record(nil))

		failed := false

		inspectAfterRecordRead = func() {
			if failed {
				return
			}

			failed = true
			writeFile(t, systemctlBinary, "#!/bin/sh\nexit 1\n", 0o755)
		}

		r := f.report(t)
		mustUnknown(t, "running_sha256", r.Services["node"].RunningSHA256, "systemctl show")
		mustUnknown(t, "host.registration", r.Host.Registration, "systemctl show")
	})

	t.Run("an identity that moves on every attempt is unknown together", func(t *testing.T) {
		f := nodeTLSFixture(t, true)
		f.writeRecord(t, f.record(nil))

		n := 0

		inspectAfterRecordRead = func() {
			n++
			f.nodeProperty(t, "InvocationID", fmt.Sprintf("%032x", n))
		}

		r := f.report(t)
		mustUnknown(t, "running_sha256", r.Services["node"].RunningSHA256, "restarted during the observation")
		mustUnknown(t, "host.registration", r.Host.Registration, "restarted during the observation")
	})
}

// processAs lays down another pid's process files with the given image.
func (f *registrationFixture) processAs(t *testing.T, pid int, image string) {
	t.Helper()

	dir := filepath.Join(f.procDir, strconv.Itoa(pid))
	target := filepath.Join(dir, "image")
	writeFile(t, target, image, 0o755)
	_ = os.Remove(filepath.Join(dir, "exe"))

	if err := os.Symlink(target, filepath.Join(dir, "exe")); err != nil {
		t.Fatal(err)
	}

	f.processStat(t, pid, inspectStartTicks, "S")
	writeFile(t, filepath.Join(dir, "cmdline"), strings.Join([]string{f.binPath, "node", "--config", f.configPath}, "\x00")+"\x00", 0o644)
	writeFile(t, filepath.Join(dir, "environ"), "\x00", 0o644)

	link := filepath.Join(dir, "root")
	_ = os.Remove(link)

	if err := os.Symlink("/", link); err != nil {
		t.Fatal(err)
	}
}

// I3: VALIDITY, each case an otherwise-valid record with one thing wrong.
func TestReleaseInspectRegistrationValidity(t *testing.T) {
	f := nodeTLSFixture(t, true)

	valid, err := json.Marshal(f.record(nil))
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		body string
		want string
	}{
		"not JSON":                   {"not json\n", "malformed"},
		"schema 2":                   {string(mustJSON(t, f.record(map[string]any{"schema": 2}))), "schema is 2"},
		"schema as the string \"1\"": {strings.Replace(string(valid), `"schema":1`, `"schema":"1"`, 1), `schema is "1"`},
		"schema 1.0":                 {strings.Replace(string(valid), `"schema":1`, `"schema":1.0`, 1), "schema is 1.0"},
		"an unknown key":             {string(mustJSON(t, f.record(map[string]any{"extra": "x"}))), `"extra" is not one the node writes`},
		"a missing key":              {string(mustJSON(t, f.record(map[string]any{"endpoint": nil}))), `"endpoint" is missing`},
		"a null value":               {strings.Replace(string(valid), `"node":"node-a"`, `"node":null`, 1), `"node" is null`},
		"an empty value":             {string(mustJSON(t, f.record(map[string]any{"deployment": ""}))), `"deployment" is empty`},
		"a duplicate key":            {strings.Replace(string(valid), `"node":"node-a"`, `"node":"node-a","node":"node-a"`, 1), `"node" appears twice`},
		"trailing bytes":             {string(valid) + "{}", "bytes follow the object"},
		"a case variant":             {strings.Replace(string(valid), `"schema":1`, `"Schema":1`, 1), `"schema" is missing`},
		"a canonical member beside its case variant": {strings.Replace(string(valid), `"schema":1`, `"schema":1,"Schema":1`, 1), `"Schema" is not one the node writes`},
		"a time that is not RFC 3339":                {string(mustJSON(t, f.record(map[string]any{"registered_at": "yesterday"}))), "not RFC 3339"},
		"an incarnation of 31 hex":                   {string(mustJSON(t, f.record(map[string]any{"incarnation": testIncarnation[:31]}))), "incarnation is not 32 hex"},
		"an invocation with a non-hex character":     {string(mustJSON(t, f.record(map[string]any{"invocation_id": "0123456789abcdef0123456789abcdeg"}))), "invocation_id is not 32 hex"},
		"an endpoint that is not canonical":          {string(mustJSON(t, f.record(map[string]any{"endpoint": "https://CONTROL.example:8443"}))), "not a canonical spelling"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f.writeRecordRaw(t, c.body)
			f.nodeProperty(t, "InvocationID", nodeInvocation)
			mustUnknown(t, "host.registration", f.report(t).Host.Registration, c.want)
		})
	}

	t.Run("a malformed invocation equal to the unit's is the format diagnostic", func(t *testing.T) {
		malformed := "0123456789abcdef0123456789abcdeg"
		f.writeRecord(t, f.record(map[string]any{"invocation_id": malformed}))
		f.nodeProperty(t, "InvocationID", malformed)

		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "invocation_id is not 32 hex")
	})

	t.Run("a malformed invocation differing from the unit's is the format diagnostic too", func(t *testing.T) {
		f.writeRecord(t, f.record(map[string]any{"invocation_id": "0123456789abcdef0123456789abcdeg"}))
		f.nodeProperty(t, "InvocationID", nodeInvocation)

		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "invocation_id is not 32 hex")
	})

	t.Run("the decoder itself refuses each malformed identifier", func(t *testing.T) {
		for _, body := range []string{
			string(mustJSON(t, f.record(map[string]any{"incarnation": "short"}))),
			string(mustJSON(t, f.record(map[string]any{"invocation_id": strings.ToUpper(nodeInvocation)}))),
		} {
			if _, err := decodeRegistrationRecord([]byte(body)); err == nil || !strings.Contains(err.Error(), "not 32 hex") {
				t.Errorf("decode %s: err = %v", body, err)
			}
		}
	})

	t.Run("absent", func(t *testing.T) {
		_ = os.Remove(f.recordPath)
		f.nodeProperty(t, "InvocationID", nodeInvocation)
		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "no registration record")
	})

	t.Run("mode 0644", func(t *testing.T) {
		f.writeRecord(t, f.record(nil))
		mustOK(t, os.Chmod(f.recordPath, 0o644))
		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "mode 0644, want 0600")
	})

	t.Run("owned by uid 1000, judged on the admitted descriptor's own metadata", func(t *testing.T) {
		f.writeRecord(t, f.record(nil))
		f.owners[inodeOfPath(t, f.recordPath)] = 1000
		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "owned by uid 1000")
		delete(f.owners, inodeOfPath(t, f.recordPath))
		registrationOf(t, f.report(t))
	})

	t.Run("no owner this platform reports", func(t *testing.T) {
		f.writeRecord(t, f.record(nil))
		saved := registrationOpen
		registrationOpen = func(path string) (*os.File, os.FileInfo, error) {
			file, info, err := regularfile.Open(path, regularfile.Options{NoFollow: true})
			if err != nil {
				return nil, nil, err
			}

			return file, ownedInfo{FileInfo: info, sys: nil}, nil
		}
		t.Cleanup(func() { registrationOpen = saved })
		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "no owner this platform reports")
	})

	t.Run("5 KiB", func(t *testing.T) {
		f.writeRecordRaw(t, string(valid)+strings.Repeat(" ", 5*1024))
		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "larger than 4096 bytes")
	})

	t.Run("5 KiB at admission, truncated to a valid record before the read", func(t *testing.T) {
		// THE ADMITTED DESCRIPTOR'S SIZE IS THE JUDGEMENT: the bounded read
		// alone would accept the bytes that are there by the time it runs.
		f.writeRecordRaw(t, string(valid)+strings.Repeat(" ", 5*1024))

		saved := registrationRead
		registrationRead = func(file *os.File, path string, limit int64) ([]byte, error) {
			mustOK(t, os.WriteFile(path, valid, 0o600))

			return regularfile.ReadAllLimited(file, path, limit)
		}
		t.Cleanup(func() { registrationRead = saved })

		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "larger than 4096 bytes")
	})

	t.Run("a FIFO is never waited on", func(t *testing.T) {
		_ = os.Remove(f.recordPath)
		mustOK(t, syscall.Mkfifo(f.recordPath, 0o600))
		t.Cleanup(func() { _ = os.Remove(f.recordPath) })

		done := make(chan maybe, 1)
		go func() { done <- f.report(t).Host.Registration }()

		select {
		case reg := <-done:
			mustUnknown(t, "host.registration", reg, "not a regular file")
		case <-time.After(10 * time.Second):
			t.Fatal("the FIFO blocked the report")
		}
	})

	t.Run("a symlink at the name", func(t *testing.T) {
		_ = os.Remove(f.recordPath)
		other := filepath.Join(f.dir, "elsewhere.json")
		writeFile(t, other, string(valid)+"\n", 0o600)
		mustOK(t, os.Symlink(other, f.recordPath))
		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "not a regular file")
	})

	t.Run("a symlink at the directory", func(t *testing.T) {
		_ = os.Remove(f.recordPath)
		realDir := filepath.Join(f.dir, "real-registration")
		mustOK(t, os.Mkdir(realDir, 0o750))
		writeFile(t, filepath.Join(realDir, "current"), string(valid)+"\n", 0o600)
		mustOK(t, os.RemoveAll(f.recordDir))
		mustOK(t, os.Symlink(realDir, f.recordDir))
		t.Cleanup(func() {
			mustOK(t, os.Remove(f.recordDir))
			mustOK(t, os.Mkdir(f.recordDir, 0o750))
		})
		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "is a symlink")
	})

	t.Run("read failures name the operation, never absence", func(t *testing.T) {
		f.writeRecord(t, f.record(nil))

		for name, c := range map[string]struct {
			install func()
			want    string
		}{
			"the directory's Lstat": {func() {
				registrationLstat = func(string) (os.FileInfo, error) { return nil, &fs.PathError{Op: "lstat", Err: syscall.EACCES} }
			}, "examine the registration directory"},
			"the record's open": {func() {
				registrationOpen = func(string) (*os.File, os.FileInfo, error) {
					return nil, nil, &fs.PathError{Op: "open", Err: syscall.EIO}
				}
			}, "open the registration record"},
			"the descriptor's read": {func() {
				registrationRead = func(*os.File, string, int64) ([]byte, error) { return nil, errors.New("injected: read failed") }
			}, "read the registration record"},
			// A Mac's O_NOFOLLOW open answers ELOOP for the link itself, which
			// is invalid; Linux admits a link's identity, so an ELOOP there is a
			// loop on the way and could-not-tell.
			"an ELOOP at the open": {func() {
				registrationOpen = func(string) (*os.File, os.FileInfo, error) {
					return nil, nil, &fs.PathError{Op: "open", Err: syscall.ELOOP}
				}
			}, map[bool]string{true: "not a regular file", false: "open the registration record"}[runtime.GOOS == "darwin"]},
			"a reopen failure": {func() {
				registrationOpen = func(string) (*os.File, os.FileInfo, error) {
					return nil, nil, fmt.Errorf("%w: %w", regularfile.ErrReopen, os.ErrNotExist)
				}
			}, "could not reopen"},
		} {
			t.Run(name, func(t *testing.T) {
				savedL, savedO, savedR := registrationLstat, registrationOpen, registrationRead
				t.Cleanup(func() { registrationLstat, registrationOpen, registrationRead = savedL, savedO, savedR })
				c.install()

				reg := f.report(t).Host.Registration
				mustUnknown(t, "host.registration", reg, c.want)

				if strings.Contains(reg.why, "no registration record") {
					t.Errorf("a failed read was reported as absence: %s", reg.why)
				}
			})
		}
	})
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()

	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return body
}

func inodeOfPath(t *testing.T, path string) uint64 {
	t.Helper()

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no Stat_t")
	}

	return st.Ino
}

// I3, THE ADMITTED INODE OWNS THE VERDICT: ownership is the opened
// descriptor's, and the bytes read are the admitted inode's, whatever the name
// holds before the open or after it.
func TestReleaseInspectRegistrationJudgesTheAdmittedInode(t *testing.T) {
	f := nodeTLSFixture(t, true)

	rootBody := string(mustJSON(t, f.record(nil))) + "\n"
	foreignBody := string(mustJSON(t, f.record(map[string]any{"incarnation": "ffffffffffffffffffffffffffffffff"}))) + "\n"

	plant := func(t *testing.T, body string, uid uint32) string {
		t.Helper()

		path := filepath.Join(f.dir, fmt.Sprintf("planted-%d-%d", uid, time.Now().UnixNano()))
		writeFile(t, path, body, 0o600)
		mustOK(t, os.Chmod(path, 0o600))
		f.owners[inodeOfPath(t, path)] = uid

		return path
	}

	swapIn := func(t *testing.T, path string) {
		t.Helper()
		mustOK(t, os.Rename(path, f.recordPath))
	}

	production := registrationOpen

	t.Run("a root record swapped for a foreign one before the open", func(t *testing.T) {
		swapIn(t, plant(t, rootBody, 0))
		foreign := plant(t, foreignBody, 1000)

		registrationOpen = func(path string) (*os.File, os.FileInfo, error) {
			swapIn(t, foreign)

			return production(path)
		}
		t.Cleanup(func() { registrationOpen = production })

		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "owned by uid 1000")
	})

	t.Run("a foreign record swapped for a root one before the open", func(t *testing.T) {
		swapIn(t, plant(t, foreignBody, 1000))
		root := plant(t, rootBody, 0)

		registrationOpen = func(path string) (*os.File, os.FileInfo, error) {
			swapIn(t, root)

			return production(path)
		}
		t.Cleanup(func() { registrationOpen = production })

		if rec := registrationOf(t, f.report(t)); rec.Incarnation != testIncarnation {
			t.Errorf("the admitted root record reads %+v", rec)
		}
	})

	t.Run("a root record admitted and then swapped before the read", func(t *testing.T) {
		swapIn(t, plant(t, rootBody, 0))
		foreign := plant(t, foreignBody, 1000)

		saved := registrationRead
		registrationRead = func(file *os.File, path string, limit int64) ([]byte, error) {
			swapIn(t, foreign)

			return regularfile.ReadAllLimited(file, path, limit)
		}
		t.Cleanup(func() { registrationRead = saved })

		if rec := registrationOf(t, f.report(t)); rec.Incarnation != testIncarnation {
			t.Errorf("the admitted bytes are %+v, want the root record's", rec)
		}
	})

	t.Run("a foreign record admitted and then swapped before the read", func(t *testing.T) {
		swapIn(t, plant(t, foreignBody, 1000))
		root := plant(t, rootBody, 0)

		read := 0
		saved := registrationRead
		registrationRead = func(file *os.File, path string, limit int64) ([]byte, error) {
			read++
			swapIn(t, root)

			return regularfile.ReadAllLimited(file, path, limit)
		}
		t.Cleanup(func() { registrationRead = saved })

		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "owned by uid 1000")

		if read != 0 {
			t.Error("the foreign record's bytes were read")
		}
	})
}

// I3, THE IDENTITY SOURCES, positive and negative per source.
func TestReleaseInspectRegistrationIdentitySources(t *testing.T) {
	t.Run("a configured node.name", func(t *testing.T) {
		f := nodeTLSFixture(t, true)
		f.writeRecord(t, f.record(nil))
		registrationOf(t, f.report(t))

		f.writeRecord(t, f.record(map[string]any{"node": "node-b"}))
		mustUnknown(t, "host.registration", f.report(t).Host.Registration, `names the node "node-b" and this host's node is "node-a"`)
	})

	t.Run("a TLS node without node.name takes the CommonName", func(t *testing.T) {
		f := nodeTLSFixture(t, false)
		f.writeRecord(t, f.record(nil))
		registrationOf(t, f.report(t))

		f.writeRecord(t, f.record(map[string]any{"node": "node-elsewhere"}))
		mustUnknown(t, "host.registration", f.report(t).Host.Registration, `names the node "node-elsewhere" and this host's node is "node-a"`)
	})

	t.Run("a TLS node's deployment is the bundle's Organization", func(t *testing.T) {
		f := nodeTLSFixture(t, true)
		f.writeRecord(t, f.record(map[string]any{"deployment": "dep-other"}))
		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "names the deployment dep-other and this node's deployment is dep-1234")
	})

	t.Run("a configured bundle that cannot be read is never the certless fallback", func(t *testing.T) {
		f := nodeTLSFixture(t, true)
		f.writeRecord(t, f.record(nil))

		saved := readPublicFile
		readPublicFile = func(path string) ([]byte, error) {
			if strings.HasSuffix(path, "node.crt") {
				return nil, errors.New("injected: unreadable bundle")
			}

			return saved(path)
		}
		t.Cleanup(func() { readPublicFile = saved })

		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "injected: unreadable bundle")
	})

	t.Run("a certless colocated node judges against the server's identity", func(t *testing.T) {
		base := newInspectFixture(t)
		body := strings.Replace(base.serverConfig(), "tiers:", "node:\n  name: node-a\n  server_addr: 127.0.0.1:7717\n  provider: docker\n  state_dir: "+filepath.Join(base.dir, "node-state")+"\ntiers:", 1)
		f := newRegistrationFixture(t, body)

		id, err := state.DeploymentID(base.stateDir)
		if err != nil {
			t.Fatal(err)
		}

		f.writeRecord(t, f.record(map[string]any{"deployment": id, "endpoint": "http://127.0.0.1:7717"}))
		registrationOf(t, f.report(t))

		f.writeRecord(t, f.record(map[string]any{"deployment": "dep-other", "endpoint": "http://127.0.0.1:7717"}))
		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "names the deployment dep-other and this node's deployment is "+id)
	})

	t.Run("precedence: the certificate wins over a colocated server", func(t *testing.T) {
		f := nodeTLSFixture(t, true)
		// A server section beside the TLS node, whose identity directory
		// names another deployment.
		serverState := filepath.Join(f.dir, "server-state")
		if _, err := state.DeploymentID(serverState); err != nil {
			t.Fatal(err)
		}

		body, err := os.ReadFile(f.configPath)
		mustOK(t, err)
		// The server is bound off loopback, as a server a TLS node dials must be.
		f.writeConfig(t, "server:\n  listen: 10.0.0.5:7717\n  state_dir: "+serverState+"\n  max_vcpu: 8\n  max_memory: 32GiB\ngithub:\n  org: acme\n  app_id: 1\n  installation_id: 2\n  private_key_path: "+filepath.Join(f.dir, "app.pem")+"\n"+string(body))
		f.touchBeforeStart(t, f.configPath)
		f.unitRunning(t, "billet-node.service", "node", f.configPath, nil)
		f.process(t, []string{f.binPath, "node", "--config", f.configPath}, nil)

		f.writeRecord(t, f.record(nil))

		r := f.report(t)
		if !r.Config.Readable {
			t.Fatalf("the colocated configuration was not read: %s", r.Config.Error)
		}

		registrationOf(t, r)

		serverID, _, err := state.PeekDeploymentID(serverState)
		mustOK(t, err)

		f.writeRecord(t, f.record(map[string]any{"deployment": serverID}))
		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "this node's deployment is dep-1234")
	})

	t.Run("a certless node-only host reads its own state directory without minting", func(t *testing.T) {
		base := newInspectFixture(t)
		f := newRegistrationFixture(t, base.nodeOnlyConfig())
		nodeState := filepath.Join(base.dir, "node-state")

		f.writeRecord(t, f.record(map[string]any{"deployment": "dep-x", "endpoint": "http://127.0.0.1:7717"}))
		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "no deployment identity is minted in "+nodeState)

		if _, err := os.Stat(filepath.Join(nodeState, "deployment-id")); err == nil {
			t.Error("the inspector minted an identity")
		}

		id, err := state.DeploymentID(nodeState)
		mustOK(t, err)

		f.writeRecord(t, f.record(map[string]any{"deployment": id, "endpoint": "http://127.0.0.1:7717"}))
		registrationOf(t, f.report(t))

		f.writeRecord(t, f.record(map[string]any{"deployment": "dep-x", "endpoint": "http://127.0.0.1:7717"}))
		mustUnknown(t, "host.registration", f.report(t).Host.Registration, "names the deployment dep-x and this node's deployment is "+id)
	})
}

// I4: THE ENDPOINT. A record's endpoint is known only in its canonical
// spelling under its own scheme; the installed endpoint is the configuration's
// as the one representation, and the two are reported apart.
func TestReleaseInspectRegistrationEndpoints(t *testing.T) {
	f := nodeTLSFixture(t, true)

	for _, bad := range []string{"https://CONTROL.example:8443", "https://control.example", "https://control.example:08443",
		"https://[2001:db8:0:0:0:0:0:1]:8443", "https://[::ffff:192.0.2.1]:8443", "https://[fe80::1%25eth0]:8443"} {
		f.writeRecord(t, f.record(map[string]any{"endpoint": bad}))
		mustUnknown(t, "host.registration "+bad, f.report(t).Host.Registration, "not a canonical spelling")
	}

	for _, good := range []string{"https://control.example:8443", "https://[2001:db8::1]:8443", "https://192.0.2.1:8443"} {
		f.writeRecord(t, f.record(map[string]any{"endpoint": good}))

		if rec := registrationOf(t, f.report(t)); rec.Endpoint != good {
			t.Errorf("the record's endpoint %q reads %q", good, rec.Endpoint)
		}
	}

	// A current http record beside an installed https configuration: both
	// known, and different.
	f.writeRecord(t, f.record(map[string]any{"endpoint": "http://control.example:8080"}))

	r := f.report(t)
	if rec := registrationOf(t, r); rec.Endpoint != "http://control.example:8080" {
		t.Errorf("the record's endpoint reads %q", rec.Endpoint)
	}

	if got := mustKnown(t, "host.installed_endpoint", r.Host.InstalledEndpoint); got != "https://10.0.0.5:7717" {
		t.Errorf("installed_endpoint = %v", got)
	}

	t.Run("the installed endpoint's own cases", func(t *testing.T) {
		base := newInspectFixture(t)
		if got := mustKnown(t, "installed_endpoint without a node", base.report(t).Host.InstalledEndpoint); got != nil {
			t.Errorf("a controller's installed_endpoint = %v, want null", got)
		}

		// A certless node may dial only loopback, so the spelling the
		// configuration admits and the representation refuses is a TLS node's.
		g := nodeTLSFixture(t, true)
		body, err := os.ReadFile(g.configPath)
		mustOK(t, err)
		g.writeConfig(t, strings.Replace(string(body), "10.0.0.5:7717", "under_score.example:8443", 1))

		r := g.report(t)
		if !r.Config.Readable {
			t.Fatalf("the configuration with an underscore host was not read: %s", r.Config.Error)
		}

		mustUnknown(t, "installed_endpoint", r.Host.InstalledEndpoint, "underscore")
	})
}

// I5: DARWIN. The registration is unknown with the platform's reason, the
// installed endpoint is still computed, the invocation is unknown, and the
// reader's seams see no operation at all.
func TestReleaseInspectRegistrationOnDarwin(t *testing.T) {
	f := nodeTLSFixture(t, true)
	f.writeRecord(t, f.record(nil))
	hostOS = "darwin"

	r := f.report(t)
	mustUnknown(t, "host.registration", r.Host.Registration, "no runtime record on this platform")
	mustUnknown(t, "node invocation_id", r.Services["node"].InvocationID, "launchd")

	if got := mustKnown(t, "host.installed_endpoint", r.Host.InstalledEndpoint); got != "https://10.0.0.5:7717" {
		t.Errorf("installed_endpoint = %v", got)
	}

	if len(f.ops) != 0 {
		t.Errorf("the registration reader performed %v on darwin", f.ops)
	}
}
