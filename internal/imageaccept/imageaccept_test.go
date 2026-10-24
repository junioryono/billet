package imageaccept

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A RECORD READS BACK AS WRITTEN: sorted, without duplicates, under its
// deployment, mode 0600; and a second write replaces it whole.
func TestARecordReadsBackAsWritten(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

	if err := Write(dir, "dep-a", true, []string{"firefox", "android-sdk", "firefox"}, at); err != nil {
		t.Fatal(err)
	}

	rec, ok, err := Read(dir)
	if err != nil || !ok {
		t.Fatalf("Read answered ok %v, %v", ok, err)
	}

	if !slices.Equal(rec.Components, []string{"android-sdk", "firefox"}) || rec.Deployment != "dep-a" ||
		!rec.Carried || rec.ReceivedAt != at.Format(time.RFC3339Nano) {
		t.Fatalf("read back %+v", rec)
	}

	info, err := os.Stat(Path(dir))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the record's mode is %v (%v), want 0600", info.Mode().Perm(), err)
	}

	if err := Write(dir, "dep-a", true, nil, at); err != nil {
		t.Fatal(err)
	}

	if rec, _, err := Read(dir); err != nil || rec.Components == nil || len(rec.Components) != 0 || !rec.Carried {
		t.Fatalf("a record of none read back %+v (%v), want an empty list that was carried", rec, err)
	}

	// A CONTROL PLANE THAT DOES NOT SAY is recorded as not saying, never as none,
	// and names nothing.
	if err := Write(dir, "dep-a", false, nil, at); err != nil {
		t.Fatal(err)
	}

	if rec, _, err := Read(dir); err != nil || rec.Carried || len(rec.Components) != 0 {
		t.Fatalf("a silent control plane read back %+v (%v)", rec, err)
	}

	if err := Write(dir, "dep-a", false, []string{"codeql"}, at); err == nil ||
		!strings.Contains(err.Error(), "does not say what it accepts names no component") {
		t.Fatalf("a silent control plane naming a component was written: %v", err)
	}
}

// NO RECORD IS ABSENCE; ANYTHING ELSE THAT IS NOT A RECORD IS AN ERROR, never
// absence, so no reader completes a generation with less than was accepted.
func TestOnlyAMissingRecordIsAbsence(t *testing.T) {
	t.Parallel()

	if _, ok, err := Read(t.TempDir()); ok || err != nil {
		t.Fatalf("an empty directory answered ok %v, %v; want absent", ok, err)
	}

	for name, tc := range map[string]struct{ body, says string }{
		"not JSON": {"{", "is not a record this node wrote"},
		"an unknown field": {`{"schema":1,"deployment":"d","carried":true,"components":[],"received_at":"","extra":1}`,
			"is not a record this node wrote"},
		"another schema": {`{"schema":2,"deployment":"d","carried":true,"components":[],"received_at":""}`,
			"is schema 2"},
		"no deployment": {`{"schema":1,"deployment":"","carried":true,"components":[],"received_at":""}`,
			"names no deployment"},
		"no component list": {`{"schema":1,"deployment":"d","carried":true,"received_at":""}`,
			"names no component list"},
		"names from a silent plane": {`{"schema":1,"deployment":"d","carried":false,"components":["codeql"],"received_at":""}`,
			"from a control plane that does not say"},
		"a second value": {`{"schema":1,"deployment":"d","carried":true,"components":[],"received_at":""} {}`,
			"has more after its record"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			if err := os.WriteFile(Path(dir), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, ok, err := Read(dir); err == nil || ok || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("%s answered ok %v, %v; want an error saying %q", name, ok, err, tc.says)
			}
		})
	}

	t.Run("a symlink", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "elsewhere.json")
		if err := os.WriteFile(target,
			[]byte(`{"schema":1,"deployment":"d","carried":true,"components":[],"received_at":""}`), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := os.Symlink(target, Path(dir)); err != nil {
			t.Fatal(err)
		}

		if _, ok, err := Read(dir); err == nil || ok {
			t.Fatalf("a symlink at the record answered ok %v, %v; want an error", ok, err)
		}
	})
}

// A RECORD IS WRITTEN ONLY WITH THE DEPLOYMENT THAT SAID IT; a write that fails
// in the real installer leaves the earlier record as it was; and Remove takes it
// away, absent or not.
func TestAFailedWriteLeavesTheEarlierRecordAndRemoveTakesIt(t *testing.T) {
	dir := t.TempDir()
	at := time.Now()

	if err := Write(dir, "", true, []string{"codeql"}, at); err == nil ||
		!strings.Contains(err.Error(), "only with the deployment that said it") {
		t.Fatalf("a record with no deployment was written: %v", err)
	}

	if err := Write(dir, "dep-a", true, []string{"codeql"}, at); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}

	// THE REAL INSTALLER, failing after the new bytes are staged, so what keeps
	// the old record is the installer's ordering, not the seam.
	saved := install
	t.Cleanup(func() { install = saved })

	install = func(d, n string, write func(io.Writer) error) (string, error) {
		return saved(d, n, func(w io.Writer) error {
			if err := write(w); err != nil {
				return err
			}

			return errors.New("disk full")
		})
	}

	if err := Write(dir, "dep-a", true, nil, at); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("a failed install answered %v", err)
	}

	after, err := os.ReadFile(Path(dir))
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("the record changed from %q to %q (%v)", before, after, err)
	}

	install = saved

	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := Read(dir); ok || err != nil {
		t.Fatalf("a removed record answered ok %v, %v", ok, err)
	}

	if err := Remove(dir); err != nil {
		t.Fatalf("removing an absent record answered %v", err)
	}

	// A REMOVAL NOT MADE DURABLE IS NOT REPORTED AS DONE.
	if err := Write(dir, "dep-a", true, []string{"codeql"}, at); err != nil {
		t.Fatal(err)
	}

	savedSync := syncDir
	t.Cleanup(func() { syncDir = savedSync })

	syncDir = func(string) error { return errors.New("fsync failed") }

	if err := Remove(dir); err == nil || !strings.Contains(err.Error(), "make the removal") ||
		!strings.Contains(err.Error(), "fsync failed") {
		t.Fatalf("a removal whose directory sync failed answered %v", err)
	}
}
