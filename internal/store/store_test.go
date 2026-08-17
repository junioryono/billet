package store

import (
	"testing"
	"time"
)

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
