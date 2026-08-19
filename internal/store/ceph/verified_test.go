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

// @VERIFIED IS RELATIVE TO THE HOST/GUEST CONTRACT DURING A ROLLING UPGRADE.
// Publishing the new guest must not make nodes on the prior binary select it;
// both generations remain verified, and each binary resolves its own newest one.
func TestNewestVerifiedForContractKeepsOldNodesOnTheirCompatibleGeneration(t *testing.T) {
	f := &verifiedFake{
		meta: []string{
			VerifiedKey + ".g20260814072427  2026-08-14T07:24:27Z",
			GuestContractKey + ".g20260814072427  6",
			VerifiedKey + ".g20260815033431  2026-08-15T03:34:31Z",
			GuestContractKey + ".g20260815033431  7",
		},
		snapshots: []string{"g20260814072427", "g20260815033431"},
	}

	client := verifiedClient(t, f)
	old, found, err := client.NewestVerifiedForContract(t.Context(), "ubuntu-2404-x64", "6")
	if err != nil {
		t.Fatalf("resolve old contract: %v", err)
	}
	if !found || old.Name != "g20260814072427" {
		t.Fatalf("old contract resolved to %q, found %v", old.Name, found)
	}

	current, found, err := client.NewestVerifiedForContract(t.Context(), "ubuntu-2404-x64", "7")
	if err != nil {
		t.Fatalf("resolve current contract: %v", err)
	}
	if !found || current.Name != "g20260815033431" {
		t.Fatalf("current contract resolved to %q, found %v", current.Name, found)
	}
}

func TestNewestVerifiedForContractDoesNotTrustMissingContractMetadata(t *testing.T) {
	f := &verifiedFake{
		meta:      []string{VerifiedKey + ".g20260815033431  2026-08-15T03:34:31Z"},
		snapshots: []string{"g20260815033431"},
	}

	_, found, err := verifiedClient(t, f).NewestVerifiedForContract(
		t.Context(), "ubuntu-2404-x64", "7")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if found {
		t.Fatal("a legacy verification with no contract metadata was treated as compatible")
	}
}

func TestNewestForContractFindsAnUnverifiedImportedGeneration(t *testing.T) {
	f := &verifiedFake{
		meta: []string{
			GuestContractKey + ".g20260814072427  7",
			GuestContractKey + ".g20260815033431  7",
		},
		snapshots: []string{"g20260814072427", "g20260815033431"},
	}

	got, found, err := verifiedClient(t, f).NewestForContract(
		t.Context(), "ubuntu-2404-x64", "7")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !found || got.Name != "g20260815033431" {
		t.Fatalf("matching imported generation = %q, found %v", got.Name, found)
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
