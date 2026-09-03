package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// A waiting writer has to say who holds the key and until when, and a caller
// asking only "conflict?" must still get its answer.
func TestAHeldWriterIsStillAConflictAndNamesItsHolder(t *testing.T) {
	t.Parallel()

	expires := time.Date(2026, time.September, 3, 2, 6, 38, 0, time.UTC)
	var err error = &WriterHeldError{Key: "repo/npm", Holder: "i-0438d35e9edde2765", Expires: expires}

	if !errors.Is(err, ErrConflict) {
		t.Fatal("a held writer does not read as a conflict")
	}
	held, ok := errors.AsType[*WriterHeldError](err)
	if !ok || held.Holder != "i-0438d35e9edde2765" || !held.Expires.Equal(expires) {
		t.Fatalf("held = %+v, ok=%t", held, ok)
	}
	if !strings.Contains(err.Error(), "i-0438d35e9edde2765") ||
		!strings.Contains(err.Error(), "2026-09-03T02:06:38Z") ||
		!strings.Contains(err.Error(), `"repo/npm"`) {
		t.Fatalf("the message does not name the key, holder and expiry: %v", err)
	}
}

func TestAWriterLeaseIsValidOnlyForItsKeyAndLifetime(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	lease := WriterLease{Key: "repo/npm", ID: "writer-1", Expires: now.Add(time.Minute)}

	if err := lease.ValidAt("repo/npm", now); err != nil {
		t.Fatalf("a current lease for this key was refused: %v", err)
	}

	if err := lease.ValidAt("repo/go", now); err == nil {
		t.Fatal("a writer lease authorised a different cache key")
	}

	if err := lease.ValidAt("repo/npm", lease.Expires); err == nil {
		t.Fatal("a writer lease remained valid at its expiry boundary")
	}
}

func TestOnlyACompleteFilesystemDescriptionCanBePublished(t *testing.T) {
	t.Parallel()

	valid := Filesystem{Type: "ext4", UUID: "dcab7af5-4ae7-4cc1-8ddb-1db18956c389", Clean: true}
	if err := valid.Valid(); err != nil {
		t.Fatalf("a checked ext4 filesystem was refused: %v", err)
	}

	for name, filesystem := range map[string]Filesystem{
		"unknown type": {UUID: valid.UUID, Clean: true},
		"no identity":  {Type: "ext4", Clean: true},
		"not clean":    {Type: "ext4", UUID: valid.UUID},
	} {
		if err := filesystem.Valid(); err == nil {
			t.Errorf("%s was publishable", name)
		}
	}
}
