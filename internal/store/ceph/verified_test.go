package ceph

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// A VERIFICATION KEY IS NOT PROOF THE GENERATION IS STILL THERE.
//
// `@verified` resolved from metadata alone, so a key that outlived its snapshot --
// a reap that removed the snapshot and failed to remove the key, or a verify that
// raced one -- made every launch resolve to a generation that does not exist. The
// clone then fails, and the message is about a missing snapshot rather than about
// the alias that chose it.
func TestNewestVerifiedIgnoresGenerationsThatNoLongerExist(t *testing.T) {
	f := &verifiedFake{
		meta: []string{
			VerifiedKey + ".g20260814072427  2026-08-14T07:24:27Z",
			VerifiedKey + ".g20260815033431  2026-08-15T03:34:31Z",
		},
		// Only the older one still has a snapshot; the newer key is stale.
		snapshots: []string{"g20260814072427"},
	}

	got, found, err := verifiedClient(t, f).NewestVerified(t.Context(), "ubuntu-2404-x64")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if !found {
		t.Fatal("nothing resolved, though one verified generation still exists")
	}

	if got.Name != "g20260814072427" {
		t.Fatalf("@verified resolved to %q, which has no snapshot; every launch would fail "+
			"on a missing generation rather than on the alias that chose it", got.Name)
	}
}

// AND WITH EVERY VERIFIED GENERATION GONE, the alias resolves to nothing rather
// than to the newest corpse.
func TestNewestVerifiedFindsNothingWhenEveryVerifiedGenerationIsGone(t *testing.T) {
	f := &verifiedFake{
		meta:      []string{VerifiedKey + ".g20260814072427  2026-08-14T07:24:27Z"},
		snapshots: []string{},
	}

	_, found, err := verifiedClient(t, f).NewestVerified(t.Context(), "ubuntu-2404-x64")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if found {
		t.Fatal("@verified resolved to a generation whose snapshot is gone")
	}
}

// verifiedFake answers the two questions NewestVerified asks: which keys the image
// carries, and which snapshots actually exist.
type verifiedFake struct {
	meta      []string
	snapshots []string
}

func (f *verifiedFake) run(_ context.Context, _ string, args []string) ([]byte, error) {
	switch subcommandOf(args) {
	case "image-meta":
		return []byte("There are " + strconv.Itoa(len(f.meta)) + " metadata on this image.\n" +
			strings.Join(f.meta, "\n") + "\n"), nil
	case "snap":
		entries := make([]string, 0, len(f.snapshots))
		for i, name := range f.snapshots {
			entries = append(entries, fmt.Sprintf(`{"id":%d,"name":%q}`, i+1, name))
		}

		return []byte("[" + strings.Join(entries, ",") + "]"), nil
	default:
		return []byte(""), nil
	}
}

func verifiedClient(t *testing.T, f *verifiedFake) *Client {
	t.Helper()

	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		withRunner(f.run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return c
}
