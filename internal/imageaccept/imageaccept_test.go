package imageaccept

import (
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

	if err := Write(dir, "dep-a", []string{"firefox", "android-sdk", "firefox"}, at); err != nil {
		t.Fatal(err)
	}

	rec, ok, err := Read(dir)
	if err != nil || !ok {
		t.Fatalf("Read answered ok %v, %v", ok, err)
	}

	if !slices.Equal(rec.Components, []string{"android-sdk", "firefox"}) || rec.Deployment != "dep-a" ||
		rec.ReceivedAt != at.Format(time.RFC3339Nano) {
		t.Fatalf("read back %+v", rec)
	}

	info, err := os.Stat(Path(dir))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the record's mode is %v (%v), want 0600", info.Mode().Perm(), err)
	}

	if err := Write(dir, "dep-a", nil, at); err != nil {
		t.Fatal(err)
	}

	if rec, _, err := Read(dir); err != nil || rec.Components == nil || len(rec.Components) != 0 {
		t.Fatalf("a record of none read back %+v (%v), want an empty list", rec, err)
	}
}

// NO RECORD IS ABSENCE; ANYTHING ELSE THAT IS NOT A RECORD IS AN ERROR, never
// absence, so no reader completes a generation with less than was accepted.
func TestOnlyAMissingRecordIsAbsence(t *testing.T) {
	t.Parallel()

	if _, ok, err := Read(t.TempDir()); ok || err != nil {
		t.Fatalf("an empty directory answered ok %v, %v; want absent", ok, err)
	}

	for name, body := range map[string]string{
		"not JSON":          "{",
		"an unknown field":  `{"schema":1,"deployment":"d","components":[],"received_at":"","extra":1}`,
		"another schema":    `{"schema":2,"deployment":"d","components":[],"received_at":""}`,
		"no deployment":     `{"schema":1,"deployment":"","components":[],"received_at":""}`,
		"no component list": `{"schema":1,"deployment":"d","received_at":""}`,
		"a second value":    `{"schema":1,"deployment":"d","components":[],"received_at":""} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			if err := os.WriteFile(Path(dir), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, ok, err := Read(dir); err == nil || ok {
				t.Fatalf("%s answered ok %v, %v; want an error", name, ok, err)
			}
		})
	}

	t.Run("a symlink", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "elsewhere.json")
		if err := os.WriteFile(target, []byte(`{"schema":1,"deployment":"d","components":[],"received_at":""}`),
			0o600); err != nil {
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

// A RECORD IS WRITTEN ONLY WITH THE DEPLOYMENT THAT SAID IT, and a failed write
// leaves the earlier record as it was.
func TestAFailedWriteLeavesTheEarlierRecord(t *testing.T) {
	dir := t.TempDir()
	at := time.Now()

	if err := Write(dir, "", []string{"codeql"}, at); err == nil || !strings.Contains(err.Error(), "deployment") {
		t.Fatalf("a record with no deployment was written: %v", err)
	}

	if err := Write(dir, "dep-a", []string{"codeql"}, at); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}

	saved := install
	t.Cleanup(func() { install = saved })

	install = func(string, string, func(io.Writer) error) (string, error) {
		return "", errors.New("disk full")
	}

	if err := Write(dir, "dep-a", nil, at); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("a failed install answered %v", err)
	}

	after, err := os.ReadFile(Path(dir))
	if err != nil || string(after) != string(before) {
		t.Fatalf("the record changed from %q to %q (%v)", before, after, err)
	}
}
