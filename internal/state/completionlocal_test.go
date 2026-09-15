package state

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCompletionExistingLocalStateNeverPreparesTheArchive(t *testing.T) {
	for _, shape := range []string{"missing directory", "loose directory", "missing lock"} {
		t.Run(shape, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "archive")
			if shape != "missing directory" {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if shape == "loose directory" {
				if err := os.Chmod(dir, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			ops := sideEffects(t, func() {
				_, err = OpenPostgresCompletion(t.Context(), dir, "postgres://unused.invalid/billet", WithExistingLocalState())
			})
			if err == nil || !strings.Contains(err.Error(), "existing state") {
				t.Fatalf("local damage must refuse before connecting: %v", err)
			}
			if slices.Contains(ops, "mkdir") || slices.Contains(ops, "chmod") {
				t.Fatalf("completion prepared local state: %v", ops)
			}
			if _, err := os.Lstat(DirectoryLockPath(dir)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("completion created a lock: %v", err)
			}
			info, err := os.Lstat(dir)
			if shape == "missing directory" {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("completion created the archive: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if shape == "loose directory" && info.Mode().Perm() != 0o750 {
				t.Fatal("completion repaired the archive's mode")
			}
		})
	}
}

func TestCompletionExistingLocalStateStillCompletesHistory(t *testing.T) {
	dsn := requirePostgres(t)
	dir := t.TempDir()
	plane, err := OpenPostgres(t.Context(), dir, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plane.ClaimController(t.Context(), "control-a", "dddddddddddddddddddddddddddddddd"); err != nil {
		_ = plane.Close()
		t.Fatal(err)
	}
	if err := plane.Close(); err != nil {
		t.Fatal(err)
	}
	var db *DB
	ops := sideEffects(t, func() {
		db, err = OpenPostgresCompletion(t.Context(), dir, dsn, WithExistingLocalState())
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, forbidden := range []string{"mkdir", "chmod", "claim", "migrate", "watermark"} {
		if slices.Contains(ops, forbidden) {
			t.Fatalf("completion performed %s: %v", forbidden, ops)
		}
	}
	if _, _, err := db.CompleteRetirement(t.Context(), RetirementCompletion{
		Deployment: "dddddddddddddddddddddddddddddddd", Retiring: "control-a", Survivor: "control-b",
		TransitionID: "0123456789abcdef0123456789abcdef", ReservedAt: "2026-09-11T10:00:00Z",
		CompletedBy: "control-a", At: time.Now(),
	}); err != nil {
		t.Fatalf("complete historical row: %v", err)
	}
}
