package ceph

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// A ROOT DISK IS PUBLISHED IN PLACE: snapshotted as a generation, then renamed to
// the golden's name, both in the cache pool, both keeping the kernel client the
// paused guest's device still belongs to. Asserted on the positional specs and
// their order, because the snapshot taken after the rename would name a clone that
// no longer exists, and one taken on the golden's name would be of nothing.
func TestARootDiskIsPublishedAsAGoldenInPlace(t *testing.T) {
	t.Parallel()

	f := newCloneFake()
	at := time.Date(2026, 10, 21, 3, 4, 5, 0, time.UTC)

	ref, err := cloneClient(t, f).publishRootAt(t.Context(), "billet-abc", "golden-app", at)
	if err != nil {
		t.Fatalf("PublishRoot: %v", err)
	}

	if ref != "golden-app@g20261021030405" {
		t.Errorf("PublishRoot answered %q, want the golden at the generation it made", ref)
	}

	snapshot, rename := callIndex(f, "snap create"), callIndex(f, "rename")
	if snapshot < 0 || rename < 0 || snapshot > rename {
		t.Fatalf("want the snapshot and then the rename; billet ran %v", f.calls)
	}

	if got := f.calls[snapshot]; got[len(got)-1] != "billet-cache/billet-abc@g20261021030405" {
		t.Errorf("the generation was taken as %q", got[len(got)-1])
	}

	if got := f.calls[rename]; !slices.Equal(got[len(got)-2:],
		[]string{"billet-cache/billet-abc", "billet-cache/golden-app"}) {
		t.Errorf("the rename was %v", got[len(got)-2:])
	}

	// THE KERNEL CLIENT IS KEPT (#394): both change an image the paused guest's
	// device still has mapped.
	for _, i := range []int{snapshot, rename} {
		if !strings.Contains(strings.Join(f.calls[i], " "), "--rbd_blocklist_on_break_lock false") {
			t.Errorf("%v may blocklist the kernel client that maps the clone", f.calls[i])
		}
	}
}

// A RENAME THAT FAILS LEAVES THE CLONE AS IT WAS: the generation it took is removed,
// so the builder's discard moves a clone with no snapshot to the trash, which the
// purge can delete.
func TestAPublicationWhoseRenameFailsRemovesItsGeneration(t *testing.T) {
	t.Parallel()

	f := newCloneFake()
	f.failOn = "rename"

	_, err := cloneClient(t, f).publishRootAt(t.Context(), "billet-abc", "golden-app",
		time.Date(2026, 10, 21, 3, 4, 5, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "rename billet-cache/billet-abc") {
		t.Fatalf("PublishRoot = %v, want the rename's failure", err)
	}

	removal := f.ran(t, "snap", "rm")
	if got := removal[len(removal)-1]; got != "billet-cache/billet-abc@g20261021030405" {
		t.Errorf("the failed publication removed %q, not the generation it took", got)
	}
}

// A NAME THAT IS NOT A GOLDEN'S IS REFUSED BEFORE ANYTHING RUNS: the trash purge
// deletes `billet-*` disks and `cache-*` volumes, so a golden under either name would
// be deleted under every restore of it.
func TestAGoldenNameThatASweepWouldTakeIsRefused(t *testing.T) {
	t.Parallel()

	for _, golden := range []string{"billet-app", "cache-v-app", "app", "golden-", "golden-a/b", "-golden-a"} {
		f := newCloneFake()

		if _, err := cloneClient(t, f).PublishRoot(t.Context(), "billet-abc", golden); err == nil {
			t.Errorf("PublishRoot accepted the golden name %q", golden)
		}

		if len(f.calls) != 0 {
			t.Errorf("a refused name %q still ran %v", golden, f.calls)
		}
	}
}

// A WITHDRAWAL PUTS THE CLONE BACK UNDER ITS NAME AND REMOVES THE GENERATION, and a
// rename already undone (a retry) still removes the generation.
func TestAWithdrawnGoldenIsTheBuildersCloneAgain(t *testing.T) {
	t.Parallel()

	t.Run("published", func(t *testing.T) {
		t.Parallel()

		f := newCloneFake()

		if err := cloneClient(t, f).WithdrawRoot(t.Context(), "golden-app@g20261021030405", "billet-abc"); err != nil {
			t.Fatalf("WithdrawRoot: %v", err)
		}

		rename, removal := callIndex(f, "rename"), callIndex(f, "snap rm")
		if rename < 0 || removal < 0 || rename > removal {
			t.Fatalf("want the rename back and then the snapshot's removal; billet ran %v", f.calls)
		}

		if got := f.calls[rename]; !slices.Equal(got[len(got)-2:],
			[]string{"billet-cache/golden-app", "billet-cache/billet-abc"}) {
			t.Errorf("the rename back was %v", got[len(got)-2:])
		}

		if got := f.calls[removal]; got[len(got)-1] != "billet-cache/billet-abc@g20261021030405" {
			t.Errorf("the withdrawal removed %q", got[len(got)-1])
		}
	})

	t.Run("already renamed back", func(t *testing.T) {
		t.Parallel()

		f := newCloneFake()
		f.failOn = "rename"
		f.failErr = errors.New("rbd: error opening image golden-app: (2) No such file or directory")

		if err := cloneClient(t, f).WithdrawRoot(t.Context(), "golden-app@g20261021030405", "billet-abc"); err != nil {
			t.Fatalf("WithdrawRoot after the rename was undone: %v", err)
		}

		f.ran(t, "snap", "rm", "billet-cache/billet-abc@g20261021030405")
	})
}

// A RESTORE'S DISK IS A CLONE OF THE GOLDEN'S GENERATION, from the cache pool, and
// only of one: an image-pool reference, a golden's unnamed head or a snapshot billet
// did not publish is refused before rbd is asked.
func TestARestoresDiskIsAGoldensGeneration(t *testing.T) {
	t.Parallel()

	f := newCloneFake()

	device, err := cloneClient(t, f).CloneGolden(t.Context(), "golden-app@g20261021030405", "billet-fork")
	if err != nil {
		t.Fatalf("CloneGolden: %v", err)
	}

	if device != "/dev/rbd0" {
		t.Errorf("CloneGolden returned %q, not the device rbd printed", device)
	}

	clone := f.ran(t, "clone")
	if !slices.Equal(clone[len(clone)-2:],
		[]string{"billet-cache/golden-app@g20261021030405", "billet-cache/billet-fork"}) {
		t.Errorf("the clone was %v", clone[len(clone)-2:])
	}

	for _, ref := range []string{"ubuntu-2404-x64@g20261021030405", "golden-app", "golden-app@cap",
		"golden-app@g1", "billet-abc@g20261021030405"} {
		refused := newCloneFake()

		if _, err := cloneClient(t, refused).CloneGolden(t.Context(), ref, "billet-fork"); err == nil {
			t.Errorf("CloneGolden accepted %q", ref)
		}

		if len(refused.calls) != 0 {
			t.Errorf("a refused reference %q still ran %v", ref, refused.calls)
		}
	}
}

// A GOLDEN THAT IS GONE IS DISTINGUISHABLE, as a missing image-pool generation is,
// and a clone whose map fails is removed rather than left with no jail naming it.
func TestAGoldenCloneThatCannotCompleteIsReported(t *testing.T) {
	t.Parallel()

	t.Run("gone", func(t *testing.T) {
		t.Parallel()

		f := newCloneFake()
		f.failOn = "clone"
		f.failErr = errors.New("rbd: error opening image golden-app: (2) No such file or directory")

		_, err := cloneClient(t, f).CloneGolden(t.Context(), "golden-app@g20261021030405", "billet-fork")
		if !errors.Is(err, ErrNoSuchGolden) {
			t.Errorf("CloneGolden of a missing golden = %v, want ErrNoSuchGolden", err)
		}
	})

	t.Run("unmappable", func(t *testing.T) {
		t.Parallel()

		f := newCloneFake()
		f.failOn = "device map"

		if _, err := cloneClient(t, f).CloneGolden(t.Context(), "golden-app@g20261021030405", "billet-fork"); err == nil {
			t.Fatal("CloneGolden reported success for a clone it could not map")
		}

		f.ran(t, "trash", "mv", "billet-cache/billet-fork")
	})
}

// The value firecracker's goldenDiskPrefix holds, which that package asserts too:
// it may not import the store, and a drift would publish a golden under a name this
// package's sweeps do not keep.
func TestTheGoldenPrefixIsTheOneTheProviderPublishes(t *testing.T) {
	t.Parallel()

	if GoldenPrefix != "golden-" {
		t.Errorf("GoldenPrefix is %q, and the Firecracker provider publishes goldens as golden-*", GoldenPrefix)
	}
}

// callIndex is the position of the first invocation of sub, or -1.
func callIndex(f *cloneFake, sub string) int {
	for i, call := range f.calls {
		if subcommandOf(call) == sub || strings.HasPrefix(strings.Join(dropFlags(call), " "), sub) {
			return i
		}
	}

	return -1
}

// dropFlags is an invocation's words without its flags and their values.
func dropFlags(args []string) []string {
	var words []string

	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			i++

			continue
		}

		words = append(words, args[i])
	}

	return words
}
