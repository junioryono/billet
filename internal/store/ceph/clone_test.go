package ceph

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// cloneFake answers the four commands a root disk needs, by subcommand rather than
// by call order — the same rule the reachability fake follows, and for the same
// reason: a fake keyed on position agrees with whatever sequence the code happens
// to produce.
type cloneFake struct {
	calls [][]string

	device  string // what `rbd device map` prints
	mapped  string // what `rbd device list` reports, as json
	size    int64  // what `rbd info` reports, in bytes
	failOn  string // a subcommand that fails
	failErr error
}

func newCloneFake() *cloneFake {
	return &cloneFake{device: "/dev/rbd0", mapped: "[]", size: 4 * int64(config.GiB)}
}

func (f *cloneFake) run(_ context.Context, _ string, args []string) ([]byte, error) {
	f.calls = append(f.calls, args)

	// SUBCOMMANDS ARE MATCHED AS WHOLE ARGUMENTS, not as substrings of the joined
	// line — which is not fussiness. A fake keyed on `strings.Contains(joined,
	// "rm")` matches `--format`, so a case staging a failed REMOVE failed the
	// device listing instead and the test reported the wrong conclusion about the
	// code. Found by that test.
	sub := subcommandOf(args)

	if f.failOn != "" && f.failOn == sub {
		err := f.failErr
		if err == nil {
			err = errors.New("exit status 1")
		}

		return nil, err
	}

	switch sub {
	case "device map":
		return []byte(f.device + "\n"), nil
	case "device list":
		return []byte(f.mapped), nil
	case "info":
		return fmt.Appendf(nil, `{"size":%d}`, f.size), nil
	default:
		return nil, nil
	}
}

// subcommandOf is the rbd verb an invocation carries, ignoring the identity and
// formatting flags in front of it.
func subcommandOf(args []string) string {
	var words []string

	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			// Every flag billet passes to rbd here takes a value.
			i++

			continue
		}

		words = append(words, args[i])
	}

	switch {
	case len(words) >= 2 && words[0] == "device":
		return words[0] + " " + words[1]
	case len(words) >= 1:
		return words[0]
	default:
		return ""
	}
}

// ran reports the first invocation containing every fragment given.
func (f *cloneFake) ran(t *testing.T, fragments ...string) []string {
	t.Helper()

	for _, call := range f.calls {
		joined := strings.Join(call, " ")

		matched := true

		for _, fragment := range fragments {
			if !strings.Contains(joined, fragment) {
				matched = false

				break
			}
		}

		if matched {
			return call
		}
	}

	t.Fatalf("no invocation carried %v; billet ran %v", fragments, f.calls)

	return nil
}

func cloneClient(t *testing.T, f *cloneFake) *Client {
	t.Helper()

	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		withRunner(f.run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return c
}

// A ROOT DISK IS A CLONE OF A NAMED SNAPSHOT, PUT IN THE CACHE POOL.
//
// The direction matters and is easy to get backwards: the golden image lives in the
// IMAGE pool and the per-job copy lands in the CACHE pool, so that eviction can walk
// one without being able to reach the other.
func TestARootDiskIsClonedFromTheImagePoolIntoTheCachePool(t *testing.T) {
	t.Parallel()

	f := newCloneFake()

	device, err := cloneClient(t, f).CloneRoot(t.Context(), "ubuntu-2404-x64@g1", "billet-abc",
		20*config.GiB)
	if err != nil {
		t.Fatalf("CloneRoot: %v", err)
	}

	if device != "/dev/rbd0" {
		t.Errorf("CloneRoot returned %q, not the device rbd printed", device)
	}

	clone := f.ran(t, "clone")

	// EXACT SPECS, because these are POSITIONAL arguments: a source and a
	// destination the wrong way round would clone a job's disk over a golden image.
	if got := clone[len(clone)-2]; got != "billet-images/ubuntu-2404-x64@g1" {
		t.Errorf("the clone source is %q", got)
	}

	if got := clone[len(clone)-1]; got != "billet-cache/billet-abc" {
		t.Errorf("the clone destination is %q", got)
	}

	// AND NO `snap protect`, which is what the clone-v2 precondition buys. On a
	// clone-v1 cluster it is mandatory, and it is also what makes a generation
	// undeletable while any job holds a clone of it.
	for _, call := range f.calls {
		if strings.Contains(strings.Join(call, " "), "protect") {
			t.Errorf("billet protected a snapshot, which makes it undeletable while this clone "+
				"is live: %v", call)
		}
	}
}

// THE TIER'S DISK IS GUEST CAPACITY, not an accounting-only number. The immutable
// generation stays at its published size; only the per-job clone is grown, before
// it is mapped into the kernel and handed to Firecracker.
func TestARootDiskCloneIsGrownToTheTierCapacityBeforeItIsMapped(t *testing.T) {
	t.Parallel()

	f := newCloneFake()
	f.size = 4 * int64(config.GiB)

	if _, err := cloneClient(t, f).CloneRoot(t.Context(), "img@g1", "billet-abc",
		20*config.GiB); err != nil {
		t.Fatalf("CloneRoot: %v", err)
	}

	// JSON IS PART OF THE COMMAND CONTRACT. Without it rbd prints a human table,
	// json.Unmarshal fails, and every explicit-capacity launch stops here.
	f.ran(t, "--format", "json", "info", "billet-cache/billet-abc")

	resize := f.ran(t, "resize", "--size", "20480M", "billet-cache/billet-abc")
	if got := resize[len(resize)-1]; got != "billet-cache/billet-abc" {
		t.Errorf("resize targeted %q rather than the per-job clone", got)
	}

	var resizeAt, mapAt = -1, -1
	for i, call := range f.calls {
		switch subcommandOf(call) {
		case "resize":
			resizeAt = i
		case "device map":
			mapAt = i
		}
	}
	if resizeAt < 0 || mapAt < 0 || resizeAt >= mapAt {
		t.Fatalf("root sizing did not precede mapping: %v", f.calls)
	}
}

// A GOLDEN IMAGE MAY OUTGROW AN OLD TIER. Shrinking its clone would truncate a
// filesystem; the tier promise is a floor, so a larger image is left alone.
func TestARootDiskCloneIsNeverShrunk(t *testing.T) {
	t.Parallel()

	f := newCloneFake()
	f.size = 40 * int64(config.GiB)

	if _, err := cloneClient(t, f).CloneRoot(t.Context(), "img@g1", "billet-abc",
		20*config.GiB); err != nil {
		t.Fatalf("CloneRoot: %v", err)
	}

	for _, call := range f.calls {
		if subcommandOf(call) == "resize" {
			t.Errorf("billet tried to shrink a larger golden-image clone: %v", call)
		}
	}
}

// A CLONE THAT CANNOT BE SIZED IS REMOVED before it ever reaches the kernel. It
// otherwise holds pool space under a jail whose launch has already failed.
func TestACloneThatCannotBeGrownIsRemoved(t *testing.T) {
	t.Parallel()

	f := newCloneFake()
	f.failOn = "resize"

	if _, err := cloneClient(t, f).CloneRoot(t.Context(), "img@g1", "billet-abc",
		20*config.GiB); err == nil {
		t.Fatal("CloneRoot reported success although sizing failed")
	}

	f.ran(t, "rm", "billet-cache/billet-abc")
	for _, call := range f.calls {
		if subcommandOf(call) == "device map" {
			t.Errorf("billet mapped a clone whose promised capacity was not established: %v", call)
		}
	}
}

// ZERO MEANS THE BACKEND DEFAULT. Existing configurations may omit disk, so the
// clone keeps the generation's size and reaches mapping without an info or resize.
func TestAZeroRootCapacityKeepsTheGenerationSize(t *testing.T) {
	t.Parallel()

	f := newCloneFake()

	if _, err := cloneClient(t, f).CloneRoot(t.Context(), "img@g1", "billet-abc", 0); err != nil {
		t.Fatalf("CloneRoot: %v", err)
	}
	for _, call := range f.calls {
		if sub := subcommandOf(call); sub == "info" || sub == "resize" {
			t.Errorf("a backend-default capacity ran %s: %v", sub, call)
		}
	}
	f.ran(t, "device", "map", "billet-cache/billet-abc")
}

func TestANegativeRootCapacityIsRefusedBeforeItClones(t *testing.T) {
	t.Parallel()

	f := newCloneFake()

	if _, err := cloneClient(t, f).CloneRoot(t.Context(), "img@g1", "billet-abc", -1); err == nil {
		t.Fatal("CloneRoot accepted a negative root capacity")
	}
	if len(f.calls) != 0 {
		t.Errorf("a refused capacity still changed the cluster: %v", f.calls)
	}
}

// The MiB ceiling is exact at a boundary, rounds one byte up, and does not
// overflow at the largest value ByteSize can represent.
func TestRootCapacityIsRoundedToMiBWithoutOverflow(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		capacity config.ByteSize
		want     string
	}{
		{"exact MiB", 5 * config.MiB, "5M"},
		{"one byte over", 5*config.MiB + 1, "6M"},
		{"MaxInt64", config.ByteSize(1<<63 - 1), "8796093022208M"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newCloneFake()
			f.size = 1

			if _, err := cloneClient(t, f).CloneRoot(t.Context(), "img@g1", "billet-abc",
				tc.capacity); err != nil {
				t.Fatalf("CloneRoot: %v", err)
			}

			f.ran(t, "resize", "--size", tc.want, "billet-cache/billet-abc")
		})
	}
}

// AN IMAGE REFERENCE WITHOUT A SNAPSHOT IS REFUSED, and told what it is missing.
// Choosing a generation is the storage layer's job (#25), and inventing a rule here
// would have to be unpicked when it arrives.
func TestAnImageWithNoSnapshotIsRefused(t *testing.T) {
	t.Parallel()

	for _, image := range []string{
		"ubuntu-2404-x64",
		"ubuntu-2404-x64@",
		"@g1",
		"pool/ubuntu@g1",
		"-weird@g1",
		"",
	} {
		f := newCloneFake()

		if _, err := cloneClient(t, f).CloneRoot(t.Context(), image, "billet-abc",
			20*config.GiB); err == nil {
			t.Errorf("CloneRoot accepted %q", image)
		}

		if len(f.calls) != 0 {
			t.Errorf("CloneRoot ran %v for the refused reference %q", f.calls, image)
		}
	}
}

// A MISSING GOLDEN IMAGE IS ITS OWN ANSWER, because the caller's next move differs.
// "Publish an image" is an operator action; "the cluster is unreachable" is a
// transient the node should keep retrying. Both arrive as the same errno on stderr.
func TestAMissingGoldenImageIsDistinguishable(t *testing.T) {
	t.Parallel()

	f := newCloneFake()
	f.failOn = "clone"
	f.failErr = errors.New("exit status 2: rbd: clone error: (2) No such file or directory")

	_, err := cloneClient(t, f).CloneRoot(t.Context(), "ubuntu-2404-x64@g1", "billet-abc",
		20*config.GiB)
	if !errors.Is(err, ErrNoSuchImage) {
		t.Errorf("a missing golden image is not distinguishable from a cluster failure: %v", err)
	}

	// AND AN ORDINARY FAILURE IS NOT MISREPORTED AS ONE, or a node would stop
	// retrying a cluster that is merely busy.
	other := newCloneFake()
	other.failOn = "clone"
	other.failErr = errors.New("exit status 1: rbd: clone error: (110) Connection timed out")

	_, err = cloneClient(t, other).CloneRoot(t.Context(), "ubuntu-2404-x64@g1", "billet-abc",
		20*config.GiB)
	if errors.Is(err, ErrNoSuchImage) {
		t.Errorf("a timeout was reported as a missing image: %v", err)
	}
}

// A CLONE THAT COULD NOT BE MAPPED IS REMOVED. It is invisible to everything above
// — no jail carries its name — so it would hold pool space until somebody found it
// by hand.
func TestACloneThatCannotBeMappedIsRemoved(t *testing.T) {
	t.Parallel()

	f := newCloneFake()
	f.failOn = "device map"

	if _, err := cloneClient(t, f).CloneRoot(t.Context(), "ubuntu-2404-x64@g1", "billet-abc",
		20*config.GiB); err == nil {
		t.Fatal("CloneRoot reported success although the map failed")
	}

	f.ran(t, "rm", "billet-cache/billet-abc")
}

// A DEVICE PATH THAT IS NOT ONE IS REFUSED. The caller stats this value for a major
// and minor number and creates a device node in a jail from the answer, so a value
// that is not a path rbd just produced has no business reaching that.
func TestAMapThatDoesNotAnswerWithADeviceIsRefused(t *testing.T) {
	t.Parallel()

	f := newCloneFake()
	f.device = "rbd: sysfs write failed"

	_, err := cloneClient(t, f).CloneRoot(t.Context(), "ubuntu-2404-x64@g1", "billet-abc",
		20*config.GiB)
	if err == nil {
		t.Fatal("CloneRoot accepted an answer that is not a device path")
	}

	if !strings.Contains(err.Error(), "not a device path") {
		t.Errorf("the error does not say what was wrong with it: %v", err)
	}
}

// DISCARD UNMAPS EVERY MAPPING, NOT ONE.
//
// Measured: `rbd device map` is not idempotent — a second map of one image creates
// a SECOND device — and `rbd device unmap <spec>` on an image mapped twice reports
// `mapped more than once, unmapping /dev/rbd1 only`. A device left mapped pins the
// image, and the remove then fails for a reason that names neither.
func TestDiscardUnmapsEveryMappingOfAClone(t *testing.T) {
	t.Parallel()

	f := newCloneFake()
	f.mapped = `[{"id":"1","pool":"billet-cache","namespace":"","name":"billet-abc","snap":"-","device":"/dev/rbd1"},
	             {"id":"2","pool":"billet-cache","namespace":"","name":"billet-abc","snap":"-","device":"/dev/rbd2"},
	             {"id":"3","pool":"billet-cache","namespace":"","name":"someone-else","snap":"-","device":"/dev/rbd3"},
	             {"id":"4","pool":"billet-images","namespace":"","name":"billet-abc","snap":"-","device":"/dev/rbd4"}]`

	if err := cloneClient(t, f).DiscardRoot(t.Context(), "billet-abc"); err != nil {
		t.Fatalf("DiscardRoot: %v", err)
	}

	// BY DEVICE, because that names exactly one mapping where the spec does not.
	f.ran(t, "device", "unmap", "/dev/rbd1")
	f.ran(t, "device", "unmap", "/dev/rbd2")

	// AND NOT ANOTHER IMAGE'S, nor the same NAME in a different pool — which is the
	// case a match on the name alone would unmap out from under a golden image.
	for _, call := range f.calls {
		joined := strings.Join(call, " ")
		for _, forbidden := range []string{"/dev/rbd3", "/dev/rbd4"} {
			if strings.Contains(joined, "unmap") && strings.Contains(joined, forbidden) {
				t.Errorf("billet unmapped %s, which is not this clone: %v", forbidden, call)
			}
		}
	}

	f.ran(t, "rm", "billet-cache/billet-abc")
}

// AND IT IS IDEMPOTENT, because teardown runs on paths that have already failed
// once. Measured: removing an image that is not there answers `(2) No such file or
// directory` with a non-zero status, which means "already done" here.
func TestDiscardingAnAbsentCloneIsSuccess(t *testing.T) {
	t.Parallel()

	f := newCloneFake()
	f.failOn = "rm"
	f.failErr = errors.New("exit status 2: rbd: delete error: (2) No such file or directory")

	if err := cloneClient(t, f).DiscardRoot(t.Context(), "billet-abc"); err != nil {
		t.Errorf("discarding a clone that was already gone reported an error: %v", err)
	}
}

// A REMOVAL THAT FAILED FOR ANY OTHER REASON IS REPORTED. Swallowing it would leave
// pool space that nothing reclaims and nothing mentions.
func TestARemovalThatFailedForAnotherReasonIsReported(t *testing.T) {
	t.Parallel()

	f := newCloneFake()
	f.failOn = "rm"
	f.failErr = errors.New("exit status 39: rbd: error: image still has watchers")

	if err := cloneClient(t, f).DiscardRoot(t.Context(), "billet-abc"); err == nil {
		t.Error("a removal that failed for a real reason was reported as success")
	}
}

// A `null` DEVICE LIST IS NOT AN EMPTY ONE. It unmarshals into a slice happily and
// would read as "nothing is mapped" — which here means skipping the unmap and
// removing an image the kernel is still holding.
func TestADeviceListingBilletCannotReadIsRefused(t *testing.T) {
	t.Parallel()

	for _, answer := range []string{"null", "", "not json at all"} {
		f := newCloneFake()
		f.mapped = answer

		if err := cloneClient(t, f).DiscardRoot(t.Context(), "billet-abc"); err == nil {
			t.Errorf("DiscardRoot accepted a device listing of %q", answer)
		}
	}
}

// THE JSON FIELD IS `name`, THOUGH THE HUMAN-READABLE COLUMN IS HEADED `image`.
// Decoding the heading instead yields an empty string for every row, which reads as
// "nothing is mapped" and silently skips the unmap.
func TestTheMappedImageIsReadFromTheFieldRBDActuallyEmits(t *testing.T) {
	t.Parallel()

	f := newCloneFake()
	f.mapped = `[{"id":"0","pool":"billet-cache","image":"billet-abc","device":"/dev/rbd9"}]`

	if err := cloneClient(t, f).DiscardRoot(t.Context(), "billet-abc"); err != nil {
		t.Fatalf("DiscardRoot: %v", err)
	}

	for _, call := range f.calls {
		if strings.Contains(strings.Join(call, " "), "/dev/rbd9") {
			t.Error("billet unmapped a row keyed on `image`; rbd emits `name`, and reading the " +
				"wrong key makes every mapping invisible")
		}
	}
}

// A CLONE NAME BILLET CANNOT ADDRESS IS REFUSED, on both paths. These are half of a
// POSITIONAL pool/image argument, so the rules are the pool's own measured ones.
func TestACloneNameBilletCannotAddressIsRefused(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"", "a/b", "a@b", "-weird", " padded "} {
		f := newCloneFake()

		if _, err := cloneClient(t, f).CloneRoot(t.Context(), "img@g1", name,
			20*config.GiB); err == nil {
			t.Errorf("CloneRoot accepted the name %q", name)
		}

		if err := cloneClient(t, f).DiscardRoot(t.Context(), name); err == nil {
			t.Errorf("DiscardRoot accepted the name %q", name)
		}
	}
}

// EVERY INVOCATION AUTHENTICATES AS THE CONFIGURED IDENTITY. A command that fell
// back to rbd's own default would run as client.admin, which can delete a pool.
func TestEveryRootDiskCommandNamesTheConfiguredIdentity(t *testing.T) {
	t.Parallel()

	f := newCloneFake()
	f.mapped = `[{"id":"0","pool":"billet-cache","name":"billet-abc","device":"/dev/rbd0"}]`

	c := cloneClient(t, f)

	if _, err := c.CloneRoot(t.Context(), "img@g1", "billet-abc", 20*config.GiB); err != nil {
		t.Fatalf("CloneRoot: %v", err)
	}

	if err := c.DiscardRoot(t.Context(), "billet-abc"); err != nil {
		t.Fatalf("DiscardRoot: %v", err)
	}

	if len(f.calls) == 0 {
		t.Fatal("nothing ran")
	}

	for _, call := range f.calls {
		joined := strings.Join(call, " ")
		if !strings.Contains(joined, "--id site-reader") {
			t.Errorf("an invocation did not name the configured identity: %v", call)
		}
	}
}

// AND NONE OF THEM PASSES -p. These take POSITIONAL pool/image specs, and rbd
// answers `unrecognised option '-p'` for the subcommands that do — which is the
// grammar mistake this repository has already made once, in a unit test that
// asserted billet's own version of it.
func TestTheRootDiskCommandsUsePositionalSpecs(t *testing.T) {
	t.Parallel()

	f := newCloneFake()
	f.mapped = `[{"id":"0","pool":"billet-cache","name":"billet-abc","device":"/dev/rbd0"}]`

	c := cloneClient(t, f)

	if _, err := c.CloneRoot(t.Context(), "img@g1", "billet-abc", 20*config.GiB); err != nil {
		t.Fatalf("CloneRoot: %v", err)
	}

	if err := c.DiscardRoot(t.Context(), "billet-abc"); err != nil {
		t.Fatalf("DiscardRoot: %v", err)
	}

	for _, call := range f.calls {
		for _, arg := range call {
			if arg == "-p" {
				t.Errorf("a root-disk command passed -p, which these subcommands refuse: %v", call)
			}
		}
	}
}

// THE SNAPSHOT SPEC ACCEPTS WHAT CEPH ACTUALLY CREATES. Ceph is far more permissive
// than a validator expects, so this pins the accepting direction as well as the
// refusing one — a rule that only ever refuses is a rule nobody can distinguish
// from a broken one.
func TestOrdinaryGoldenImageReferencesAreAccepted(t *testing.T) {
	t.Parallel()

	for _, image := range []string{
		"ubuntu-2404-x64@g1",
		"ubuntu.2404@2026-08-14",
		"a@b",
		"image_with_underscores@snap_1",
	} {
		f := newCloneFake()

		if _, err := cloneClient(t, f).CloneRoot(t.Context(), image, "billet-abc",
			20*config.GiB); err != nil {
			t.Errorf("CloneRoot refused %q: %v", image, err)
		}
	}
}

// A BOUNDED DIAGNOSTIC, because a refused reference is rendered back and it came
// from a tier's configuration rather than from billet.
func TestARefusedImageReferenceIsRenderedBounded(t *testing.T) {
	t.Parallel()

	f := newCloneFake()

	_, err := cloneClient(t, f).CloneRoot(t.Context(), strings.Repeat("x", 5000), "billet-abc",
		20*config.GiB)
	if err == nil {
		t.Fatal("CloneRoot accepted a 5000-character image reference")
	}

	if len(err.Error()) > 1000 {
		t.Errorf("the error is %d characters, so the value was not bounded", len(err.Error()))
	}
}

// The mapped listing is read from the CACHE pool, so a client configured with
// different pools looks in the right place.
func TestTheMappingLookupIsScopedToTheConfiguredCachePool(t *testing.T) {
	t.Parallel()

	f := newCloneFake()
	f.mapped = fmt.Sprintf(
		`[{"id":"0","pool":%q,"name":"billet-abc","device":"/dev/rbd0"}]`, "billet-images")

	if err := cloneClient(t, f).DiscardRoot(t.Context(), "billet-abc"); err != nil {
		t.Fatalf("DiscardRoot: %v", err)
	}

	for _, call := range f.calls {
		if strings.Contains(strings.Join(call, " "), "unmap") {
			t.Errorf("billet unmapped a device in the image pool: %v", call)
		}
	}
}
