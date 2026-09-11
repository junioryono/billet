package retirement

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO AT A LOCK'S NAME IS REFUSED, NEVER WAITED ON. A plain open of a FIFO
// blocks until a writer appears, before any deadline logic runs; the locks open
// through the identity-first regular-file rule, so the acquisition answers.
func TestAFIFOAtALocksNameIsRefusedWithoutWaiting(t *testing.T) {
	root := useRoot(t)

	dir := filepath.Join(root, "server")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{GlobalLockPath(), InitLockPath(dir)} {
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	answered := make(chan error, 2)

	go func() {
		_, err := Acquire(t.Context(), AcquireOptions{Privileged: true})
		answered <- err
	}()

	go func() {
		_, err := AcquireInit(t.Context(), dir, nil, 0)
		answered <- err
	}()

	for range 2 {
		select {
		case err := <-answered:
			if err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a FIFO at a lock's name must refuse as not a regular file, got %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("an acquisition blocked inside the open of a FIFO")
		}
	}
}

// A HARD LINK AT A LOCK'S NAME IS NOT RE-OWNED, because the chown would reach
// the other name too; the link count is read on the open descriptor before any
// change, and both names keep their owner and mode.
func TestReowningALockRefusesAHardLinkAndAForeignFilesystem(t *testing.T) {
	root := useRoot(t)

	target := filepath.Join(root, "elsewhere")
	if err := os.WriteFile(target, nil, 0o640); err != nil {
		t.Fatal(err)
	}

	if err := os.Link(target, GlobalLockPath()); err != nil {
		t.Fatal(err)
	}

	changed, err := reownLock(GlobalLockPath(), os.Getuid(), os.Getgid(), 0o660)
	if err == nil || changed || !strings.Contains(err.Error(), "links") {
		t.Fatalf("a hard-linked lock must refuse naming the links, got changed=%v err=%v", changed, err)
	}

	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("the refusal must change nothing through either name, got %v %v", info, err)
	}
}

// A BOOTSTRAP PUBLISHES OVER AN ABSENT OR A PRESENT STATUS AND NOTHING ELSE: a
// status that could not be read OR could not be parsed may be a retired host's,
// and metadata published over it would admit ordinary writers into a directory
// a retirement is moving.
func TestABootstrapRefusesAStatusItCannotJudge(t *testing.T) {
	cause := errors.New("malformed")

	for presence, want := range map[StatusPresence]bool{
		StatusAbsent: true, StatusPresent: true, StatusUnreadable: false, StatusMalformed: false,
	} {
		err := admitBootstrapStatus(presence, cause)
		if (err == nil) != want {
			t.Errorf("presence %d: admitted=%v, want %v (%v)", presence, err == nil, want, err)
		}

		if err != nil && !errors.Is(err, cause) {
			t.Errorf("presence %d: the refusal must carry its cause, got %v", presence, err)
		}
	}
}

// THE IDENTITY DIRECTORY IS FRESH ONLY WHEN NO METADATA EXISTS BESIDE IT: a
// global lock or a status beside an absent directory is a retirement's move or
// damage, and a bootstrap that recreated the directory there would put a second
// identity beside the archived one.
func TestAnAbsentDirectoryBesideMetadataIsNotFresh(t *testing.T) {
	useRoot(t)

	fresh, err := metadataAbsent()
	if err != nil || !fresh {
		t.Fatalf("no metadata at all is fresh, got fresh=%v err=%v", fresh, err)
	}

	// A RECORD ALONE IS NOT FRESH EITHER: a prepared host whose directory moved
	// keeps its record, and a repair of its metadata must not recreate the
	// directory.
	acct := ServiceAccount{User: "billet", UID: 1000, Group: "billet", GID: 1000}
	if err := WriteServiceAccount(acct); err != nil {
		t.Fatal(err)
	}

	if fresh, err := metadataAbsent(); err != nil || fresh {
		t.Fatalf("a record beside an absent directory is never fresh, got fresh=%v err=%v", fresh, err)
	}

	if err := os.Remove(ServiceAccountPath()); err != nil {
		t.Fatal(err)
	}

	hold, err := Acquire(t.Context(), AcquireOptions{Privileged: true})
	if err != nil {
		t.Fatal(err)
	}

	if !hold.Created() {
		t.Fatal("the first acquisition creates the lock and must say so")
	}

	releaseLater(t, hold.Release)

	fresh, err = metadataAbsent()
	if err != nil || fresh {
		t.Fatalf("a global lock beside an absent directory is never fresh, got fresh=%v err=%v", fresh, err)
	}

	bounded, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()

	again, err := Acquire(bounded, AcquireOptions{Privileged: true, Poll: time.Millisecond})
	if err == nil {
		t.Fatal(errors.Join(errors.New("a second acquisition while the first is held must not succeed"), again.Release()))
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the wait for a held lock must end at its bound, got %v", err)
	}

	if err := hold.Release(); err != nil {
		t.Fatal(err)
	}

	second, err := Acquire(t.Context(), AcquireOptions{Privileged: true})
	if err != nil {
		t.Fatal(err)
	}

	releaseLater(t, second.Release)

	if second.Created() {
		t.Fatal("an acquisition of an existing lock file must not claim to have created it")
	}
}

// THE STRICT DECODER PROVES THE END OF THE DOCUMENT AND REFUSES A REPEATED
// MEMBER. Decoder.More answers false for a trailing bracket as well as for EOF,
// so a valid record followed by a stray `}` passed it; and encoding/json keeps
// the last of two values for one member silently.
func TestTheStrictDecoderProvesEOFAndRefusesARepeatedMember(t *testing.T) {
	type record struct {
		A string   `json:"a"`
		B int      `json:"b"`
		C []string `json:"c"`
		D struct {
			E string `json:"e"`
		} `json:"d"`
	}

	for name, raw := range map[string]string{
		"trailing brace":        `{"a":"x","b":1}}`,
		"trailing bracket":      `{"a":"x","b":1}]`,
		"second document":       `{"a":"x","b":1} {"a":"y"}`,
		"repeated member":       `{"a":"x","a":"y","b":1}`,
		"repeated nested":       `{"a":"x","d":{"e":"1","e":"2"}}`,
		"unknown member":        `{"a":"x","zzz":1}`,
		"repeated after nested": `{"d":{"e":"1"},"b":1,"d":{"e":"2"}}`,
	} {
		var into record
		if err := strictDecode([]byte(raw), &into); err == nil {
			t.Errorf("%s: %s was accepted", name, raw)
		}
	}

	var into record

	// Repeated STRINGS inside an array are values, not members, and a member
	// name reused in a nested object is that object's, not its parent's.
	raw := `{"a":"x","b":1,"c":["a","a","e"],"d":{"a":"nested"}}`
	if err := strictDecode([]byte(raw), &into); err == nil || !strings.Contains(err.Error(), "unknown field") {
		// "a" inside d is unknown to d's struct, which is the ordinary refusal;
		// what matters is that it was not refused as a REPEATED member.
		if err == nil || strings.Contains(err.Error(), "appears twice") {
			t.Fatalf("a nested member that reuses a parent's name is not a repeat, got %v", err)
		}
	}

	raw = `{"a":"x","b":1,"c":["a","a","e"],"d":{"e":"1"}}` + "\n"
	if err := strictDecode([]byte(raw), &into); err != nil {
		t.Fatalf("a well-formed record with repeated array strings must decode, got %v", err)
	}

	if len(into.C) != 3 || into.D.E != "1" {
		t.Fatalf("decoded %+v", into)
	}
}
