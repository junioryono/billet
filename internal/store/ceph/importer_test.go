package ceph

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// importFake answers the commands an import issues, keyed on subcommand rather
// than on call order — the same rule the clone fake follows, for the same reason.
//
// THE DEVICE IT HANDS BACK IS A REAL FILE, which is what makes this test worth
// having: the write is exercised for real, so a test can assert the bytes that
// landed rather than merely that some command was issued.
type importFake struct {
	calls [][]string

	device   string // what `rbd device map` prints; a temp file here
	infoJSON string // what `rbd info` answers, or "" for an absent image
	failOn   string
	failErr  error

	// lockHeld is what `rbd lock ls` reports. Empty means the fake tracks the
	// lock itself: `lock add` succeeds and the listing then names this cookie,
	// which is what lets Release find and remove it.
	lockHeld         string
	lockTaken        string
	lockAddErr       error
	lockAddAmbiguous bool
	unmapErr         error
	poolMissing      bool

	// cancelOn fires the stored cancel when that subcommand is issued, which is how
	// a test reproduces the case that matters: a deadline expiring PARTWAY through
	// the import, after the lock has been taken. A context cancelled before the
	// import begins proves nothing -- the lock is never taken, so a leak and a
	// clean run look identical.
	cancelOn string
	cancel   context.CancelFunc

	// snapshots is what `rbd snap ls` reports. Empty means the image has none,
	// which is a real answer and not a broken one -- the existence check depends
	// on telling those apart.
	snapshots []string

	// heartbeat, if non-nil, is what `image-meta get` answers for a liveness key,
	// and it is called once per read so a test can make the counter move.
	heartbeat func() (string, bool)
}

func (f *importFake) run(ctx context.Context, _ string, args []string) ([]byte, error) {
	f.calls = append(f.calls, args)

	// THE CONTEXT IS HONOURED, because a real exec.CommandContext is. A fake that
	// ignores it makes every cancellation test vacuous: the first version of the
	// leaked-lock test passed a cancelled context, watched the whole import
	// succeed, and proved nothing about the code under test.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if f.cancelOn != "" && f.cancelOn == subcommandOf(args) && f.cancel != nil {
		f.cancel()
		f.cancel = nil
	}

	sub := subcommandOf(args)

	if f.failOn != "" && f.failOn == sub {
		err := f.failErr
		if err == nil {
			err = errors.New("exit status 1")
		}

		return nil, err
	}

	switch sub {
	case "lock":
		return f.lock(args)
	case "device unmap":
		if f.unmapErr != nil {
			return nil, f.unmapErr
		}

		return []byte(""), nil
	case "info":
		if f.infoJSON == "" {
			// What rbd says for an image that is not there, which the importer must
			// read as "first run" and not as a broken cluster.
			return nil, errors.New("rbd: error opening image nope: (2) No such file or directory")
		}

		return []byte(f.infoJSON), nil
	case "device map":
		return []byte(f.device + "\n"), nil
	case "device list":
		return []byte("[]"), nil
	case "ls":
		// The pool listing, which the importer uses to tell an absent IMAGE from an
		// absent POOL -- rbd answers ENOENT for both.
		if f.poolMissing {
			return nil, errors.New("rbd: error opening pool 'billet-images': (2) No such file or directory")
		}

		return []byte("[]"), nil
	case "image-meta":
		// A LIVENESS READ IS ANSWERED SEPARATELY from a write, because the break path
		// distinguishes "no counter" from "a counter that did not move" and a fake
		// that answered both with success would collapse the two.
		if f.heartbeat != nil && len(args) > 2 && args[len(args)-2] != "" &&
			strings.Contains(args[len(args)-1], "billet.heartbeat.") {
			value, present := f.heartbeat()
			if !present {
				return nil, errors.New("rbd: (2) No such file or directory")
			}

			return []byte(value + "\n"), nil
		}

		return []byte(""), nil
	case "snap":
		// `snap ls` is asked for json; `snap create` and `snap rm` are not.
		for _, a := range args {
			if a == "ls" {
				entries := make([]string, 0, len(f.snapshots))
				for i, name := range f.snapshots {
					entries = append(entries, fmt.Sprintf(`{"id":%d,"name":%q}`, i+1, name))
				}

				return []byte("[" + strings.Join(entries, ",") + "]"), nil
			}
		}

		return []byte(""), nil
	case "create", "resize":
		return []byte(""), nil
	default:
		return []byte(""), nil
	}
}

// lock models the three lock verbs against a single in-memory holder, so a test
// can assert that a lock taken is a lock released rather than merely that some
// lock command was issued.
func (f *importFake) lock(args []string) ([]byte, error) {
	verb, cookie := "", ""

	for i, a := range args {
		if a != "lock" {
			continue
		}

		if i+1 < len(args) {
			verb = args[i+1]
		}

		if i+3 < len(args) {
			cookie = args[i+3]
		}

		break
	}

	switch verb {
	case "add":
		if f.lockAddAmbiguous && f.lockTaken == "" && f.lockHeld == "" {
			f.lockTaken = cookie

			return nil, errors.New("the successful lock response was lost")
		}
		if f.lockAddErr != nil {
			return nil, f.lockAddErr
		}

		if f.lockTaken != "" || f.lockHeld != "" {
			return nil, errors.New("exit status 16")
		}

		f.lockTaken = cookie

		return []byte(""), nil
	case "ls":
		held := f.lockTaken
		if f.lockHeld != "" {
			held = f.lockHeld
		}

		if held == "" {
			return []byte("[]"), nil
		}

		return []byte(fmt.Sprintf(`[{"id":%q,"locker":"client.4242"}]`, held)), nil
	case "rm":
		if cookie == f.lockTaken {
			f.lockTaken = ""
		}

		if cookie == f.lockHeld {
			f.lockHeld = ""
		}

		return []byte(""), nil
	default:
		return []byte(""), nil
	}
}

// ranWith reports whether any invocation carried every fragment as a WHOLE
// ARGUMENT, or as a prefix of one.
//
// NOT strings.Contains OVER THE JOINED LINE, which is the trap the clone fake
// documents and which this helper walked straight into: "rm" is a substring of
// "--format", so every assertion that billet did NOT run `lock rm` passed
// against the `--format json` on the listing call and reported that a live
// holder's lock had been broken. Two tests failed for a reason that was entirely
// the test's.
//
// Prefix matching is kept for the one case that needs it -- naming a lock cookie
// by its stable leading portion, since the trailing timestamp is not knowable.
func (f *importFake) ranWith(fragments ...string) bool {
	for _, call := range f.calls {
		matched := true

		for _, fragment := range fragments {
			found := false

			for _, arg := range call {
				if arg == fragment || strings.HasPrefix(arg, fragment) {
					found = true

					break
				}
			}

			if !found {
				matched = false

				break
			}
		}

		if matched {
			return true
		}
	}

	return false
}

// stageRaw writes a stand-in filesystem image and the file the "device" maps to.
func stageRaw(t *testing.T, content string) (raw, device string) {
	t.Helper()

	dir := t.TempDir()

	raw = filepath.Join(dir, "rootfs.img")
	if err := os.WriteFile(raw, []byte(content), 0o600); err != nil {
		t.Fatalf("stage: %v", err)
	}

	device = filepath.Join(dir, "rbd0")

	// PRE-CREATED, because the real destination is a block device that already
	// exists — which is exactly why the importer opens it without O_CREATE.
	if err := os.WriteFile(device, nil, 0o600); err != nil {
		t.Fatalf("stage device: %v", err)
	}

	return raw, device
}

func importClient(t *testing.T, f *importFake) *Client {
	t.Helper()

	// THE LIVENESS WINDOW IS SHORTENED, NOT SKIPPED. Ninety seconds is right in
	// production and unusable here, but a decision that is only ever exercised with
	// the window stubbed to zero is one nobody has tested -- so this still waits,
	// briefly, and the fake still has to answer the two observations.
	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		withRunner(f.run), withObservation(10*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return c
}

var importAt = time.Date(2026, 8, 15, 4, 17, 9, 0, time.UTC)

func TestALostLockResponseRecognisesTheCallersOwnCookie(t *testing.T) {
	f := &importFake{lockAddAmbiguous: true}
	lock, err := importClient(t, f).TakePublishLock(t.Context(), importAt)
	if err != nil {
		t.Fatalf("TakePublishLock: %v", err)
	}
	if f.lockTaken == "" {
		t.Fatal("the ambiguous lock was not recorded by the fake")
	}
	if err := lock.Release(t.Context()); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestImportGenerationWritesTheImageAndPublishesIt(t *testing.T) {
	raw, device := stageRaw(t, "a filesystem, in spirit")

	f := &importFake{device: device}

	gen, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if gen != "g20260815041709" {
		t.Errorf("generation = %q", gen)
	}

	// THE BYTES ACTUALLY LANDED, not merely that a command was issued.
	got, err := os.ReadFile(device)
	if err != nil {
		t.Fatalf("read device: %v", err)
	}

	if string(got) != "a filesystem, in spirit" {
		t.Errorf("device holds %q", got)
	}

	if !f.ranWith("snap", "create", "ubuntu-2404-x64@"+gen) {
		t.Errorf("no snapshot was taken; billet ran %v", f.calls)
	}

	if !f.ranWith("image-meta", "set", RunnerVersionKey+"."+gen, "2.336.0") {
		t.Errorf("the runner version was not recorded per generation; billet ran %v", f.calls)
	}
}

// AN ABSENT HEAD IS THE FIRST RUN, and must be created rather than reported.
func TestImportGenerationCreatesTheHeadWhenItIsAbsent(t *testing.T) {
	raw, device := stageRaw(t, strings.Repeat("x", 3*1024*1024))

	f := &importFake{device: device} // infoJSON empty: rbd answers ENOENT

	if _, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt); err != nil {
		t.Fatalf("import: %v", err)
	}

	// THE HEAD, NOT THE LOCK IMAGE. The publish lock is also created with
	// `create --size`, so an assertion that does not name the image matches the
	// lock and passes even when the head was never created -- which is how the
	// first version of this test agreed with a bug.
	if !f.ranWith("create", "ubuntu-2404-x64", "--size", "515M", "--object-size", "4M") {
		t.Errorf("the head was not created at the image's size plus slack, with the object "+
			"size pinned; billet ran %v", f.calls)
	}
}

// A CLUSTER THAT CANNOT BE REACHED IS NOT AN ABSENT IMAGE. Treating every info
// failure as "first run" would answer an unreachable cluster with `rbd create`,
// which fails for a second reason and reports that one instead — so the operator
// is told the image could not be created rather than that the cluster is down.
func TestImportGenerationSeparatesAnAbsentImageFromABrokenCluster(t *testing.T) {
	raw, device := stageRaw(t, "content")

	f := &importFake{
		device:  device,
		failOn:  "info",
		failErr: errors.New("rbd: couldn't connect to the cluster"),
	}

	_, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt)
	if err == nil {
		t.Fatal("an unreachable cluster was treated as an absent image")
	}

	if f.ranWith("create", "ubuntu-2404-x64") {
		t.Error("billet tried to create the head against a cluster it could not reach")
	}
}

// GROWN, NEVER SHRUNK. Writing a larger filesystem into a head sized for the last
// one fails partway with ENOSPC — a corrupt image behind a successful-looking
// import, because the write is the only step that would have complained.
func TestImportGenerationGrowsAHeadThatIsTooSmall(t *testing.T) {
	raw, device := stageRaw(t, strings.Repeat("x", 5*1024*1024))

	f := &importFake{device: device, infoJSON: `{"size": 2097152}`} // 2 MiB

	if _, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt); err != nil {
		t.Fatalf("import: %v", err)
	}

	if !f.ranWith("resize", "--size", "517M") {
		t.Errorf("a head smaller than the image was not grown; billet ran %v", f.calls)
	}
}

// EXISTING SNAPSHOTS KEEP THEIR OWN SIZE, so shrinking reclaims nothing a
// generation still holds and would truncate the next write.
func TestImportGenerationLeavesALargerHeadAlone(t *testing.T) {
	raw, device := stageRaw(t, "small")

	f := &importFake{device: device, infoJSON: fmt.Sprintf(`{"size": %d}`, 8*1024*1024)}

	if _, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt); err != nil {
		t.Fatalf("import: %v", err)
	}

	if f.ranWith("resize") {
		t.Errorf("a head larger than the image was resized; billet ran %v", f.calls)
	}
}

// UNMAPPED BEFORE THE SNAPSHOT. A mapped device can still hold dirty pages, so
// snapshotting first captures the image as of a moment nobody chose — and the
// generation boots, or does not, on timing that never reproduces.
func TestImportGenerationUnmapsBeforeItSnapshots(t *testing.T) {
	raw, device := stageRaw(t, "content")

	f := &importFake{device: device}

	if _, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt); err != nil {
		t.Fatalf("import: %v", err)
	}

	unmapAt, snapAt := -1, -1

	for i, call := range f.calls {
		joined := strings.Join(call, " ")

		if unmapAt < 0 && strings.Contains(joined, "device unmap") {
			unmapAt = i
		}

		if snapAt < 0 && strings.Contains(joined, "snap create") {
			snapAt = i
		}
	}

	if unmapAt < 0 {
		t.Fatalf("the head was never unmapped; billet ran %v", f.calls)
	}

	if snapAt < 0 {
		t.Fatalf("no snapshot was taken; billet ran %v", f.calls)
	}

	if unmapAt > snapAt {
		t.Errorf("billet snapshotted at step %d and unmapped at step %d; the snapshot can "+
			"capture pages the kernel has not written back", snapAt, unmapAt)
	}
}

// A HEAD LEFT MAPPED IS NOT UNTIDINESS. The next import maps it a SECOND time
// rather than failing, which is how a host accumulates a dozen mappings of one
// image.
func TestImportGenerationUnmapsEvenWhenTheWriteFails(t *testing.T) {
	raw, device := stageRaw(t, "content")

	// A directory cannot be opened for writing, so the write fails after the map.
	f := &importFake{device: filepath.Dir(device)}

	if _, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt); err == nil {
		t.Fatal("writing to a directory was reported as a successful import")
	}

	if !f.ranWith("device", "unmap") {
		t.Errorf("the head was left mapped after a failed write; billet ran %v", f.calls)
	}
}

func TestImportGenerationRefusesAnEmptyImage(t *testing.T) {
	raw, device := stageRaw(t, "")

	f := &importFake{device: device}

	_, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt)
	if err == nil {
		t.Fatal("an empty file was published as a generation")
	}

	if f.ranWith("snap", "create") {
		t.Error("a snapshot was taken of nothing")
	}
}

// AN EMPTY VERSION READS BACK AS AN ABSENT KEY, so recording one produces a
// generation that silently opts out of every staleness check.
func TestSetRunnerVersionRefusesAnEmptyVersion(t *testing.T) {
	f := &importFake{}

	err := importClient(t, f).SetRunnerVersion(
		t.Context(), "ubuntu-2404-x64", "g20260815041709", "  ")
	if err == nil {
		t.Fatal("an empty runner version was recorded")
	}

	if f.ranWith("image-meta", "set") {
		t.Error("billet wrote the empty value anyway")
	}
}

// THE IMAGE NAME IS HALF OF A POSITIONAL pool/image ARGUMENT, so a name carrying
// a separator or a leading dash addresses something else entirely.
func TestImportGenerationRefusesAnUnaddressableImageName(t *testing.T) {
	raw, device := stageRaw(t, "content")

	for _, name := range []string{"", "other-pool/image", "image@gen", "-rf", " leading"} {
		f := &importFake{device: device}

		if _, err := importClient(t, f).ImportGeneration(
			t.Context(), name, raw, "2.336.0", "", importAt); err == nil {
			t.Errorf("%q was accepted as an image name", name)
		}
	}
}

// TWO PUBLISHERS WRITING ONE HEAD interleave their writes, and the first to
// finish unmaps and snapshots a filesystem that is half the other one. A
// same-second generation-name collision does not protect against this: the
// corruption happens before either `snap create`.
func TestImportGenerationStandsDownWhileAnotherPublisherHoldsTheLock(t *testing.T) {
	raw, device := stageRaw(t, "content")

	// Held by somebody else, taken one minute ago.
	f := &importFake{
		device:   device,
		lockHeld: fmt.Sprintf("billet-build-otherhost-123-%d", importAt.Add(-time.Minute).Unix()),
	}

	_, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt)
	if err == nil {
		t.Fatal("an import proceeded while another publisher held the lock")
	}

	if !strings.Contains(err.Error(), "otherhost") {
		t.Errorf("the refusal does not name the holder: %v", err)
	}

	if f.ranWith("device", "map") {
		t.Error("billet mapped the head despite standing down; two writers would have been " +
			"on one image")
	}
}

// A LEAKED LOCK IS OTHERWISE PERMANENT: rbd locks are not leases, so a killed
// publisher holds one forever and every later publish on every node refuses.
func TestImportGenerationBreaksALockNoRunCouldStillBeHolding(t *testing.T) {
	raw, device := stageRaw(t, "content")

	f := &importFake{
		device: device,
		lockHeld: fmt.Sprintf("billet-build-deadhost-9-%d",
			importAt.Add(-StaleLockAfter-time.Hour).Unix()),
	}

	if _, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt); err != nil {
		t.Fatalf("a lock older than any run could be was not broken: %v", err)
	}

	if !f.ranWith("lock", "rm", "billet-build-deadhost-9") {
		t.Errorf("the stale lock was not broken; billet ran %v", f.calls)
	}
}

// BREAKING A LIVE HOLDER'S LOCK PUTS TWO WRITERS ON ONE IMAGE, so the bound is
// deliberately far past any real run. This pins the boundary.
func TestImportGenerationDoesNotBreakALockJustUnderTheBound(t *testing.T) {
	raw, device := stageRaw(t, "content")

	f := &importFake{
		device: device,
		lockHeld: fmt.Sprintf("billet-build-livehost-9-%d",
			importAt.Add(-StaleLockAfter+time.Minute).Unix()),
	}

	if _, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt); err == nil {
		t.Fatal("a lock just inside the bound was broken")
	}

	if f.ranWith("lock", "rm") {
		t.Error("billet broke a lock whose holder could still be alive")
	}
}

// A COOKIE OF UNKNOWN SHAPE IS NEVER BROKEN. Something else took the lock, and
// breaking it because its name was unfamiliar is how two writers meet.
func TestImportGenerationNeverBreaksALockItCannotDate(t *testing.T) {
	raw, device := stageRaw(t, "content")

	f := &importFake{device: device, lockHeld: "somebody-elses-tool"}

	if _, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt); err == nil {
		t.Fatal("a lock of unknown age was broken")
	}

	if f.ranWith("lock", "rm") {
		t.Error("billet broke a lock it could not date")
	}
}

// THE LOCK IS RELEASED ON THE WAY OUT, including when the import failed -- a held
// lock outlives the process, so leaking one blocks every later publish for six
// hours.
func TestImportGenerationReleasesTheLockOnBothPaths(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		wantErr bool
	}{
		{"success", "content", false},
		{"failure", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, device := stageRaw(t, tc.content)

			f := &importFake{device: device}

			_, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt)

			if tc.wantErr && err == nil {
				t.Fatal("expected a failure")
			}

			if !tc.wantErr && err != nil {
				t.Fatalf("import: %v", err)
			}

			if f.lockTaken != "" {
				t.Errorf("the publish lock was still held as %q after the import; every later "+
					"publish on every node would refuse for %s", f.lockTaken, StaleLockAfter)
			}
		})
	}
}

// A DEFERRED FUNCTION RUNS AFTER THE RETURN VALUE IS COMPUTED. An earlier version
// set a local in the deferred cleanup and read it at the return site, so it was
// always nil and the branch that looked like error handling was dead code.
func TestImportGenerationReportsAnUnmapThatFailedAfterAGoodWrite(t *testing.T) {
	raw, device := stageRaw(t, "content")

	f := &importFake{device: device, unmapErr: errors.New("exit status 16")}

	_, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt)
	if err == nil {
		t.Fatal("an import whose head could not be unmapped was reported as a success; the " +
			"next import would map the same head a second time")
	}

	if !strings.Contains(err.Error(), "still mapped") {
		t.Errorf("the failure does not say the head is still mapped: %v", err)
	}

	if f.ranWith("snap", "create") {
		t.Error("a snapshot was taken of a head that was never unmapped, so it can capture " +
			"pages the kernel has not written back")
	}
}

// A SNAPSHOT WITH NO RECORDED RUNNER VERSION IS WORSE THAN NO SNAPSHOT:
// NewestGeneration finds it, so `billet images due` reports nothing needs
// rebuilding, while every staleness check reads its version as absent.
func TestImportGenerationWithdrawsASnapshotItCannotDescribe(t *testing.T) {
	raw, device := stageRaw(t, "content")

	f := &importFake{device: device, failOn: "image-meta"}

	gen, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt)
	if err == nil {
		t.Fatal("a generation whose runner version could not be recorded was published")
	}

	if gen != "" {
		t.Errorf("a withdrawn generation was still returned as %q", gen)
	}

	if !f.ranWith("snap", "rm", "ubuntu-2404-x64@g20260815041709") {
		t.Errorf("the snapshot was not withdrawn; billet ran %v", f.calls)
	}
}

// STATTED EARLY, OPENED LATER. If the file is truncated in between, io.Copy
// reports a clean EOF and a partial filesystem would be snapshotted as a
// published generation.
func TestWriteImageRefusesASourceThatShrankUnderIt(t *testing.T) {
	raw, device := stageRaw(t, "twelve bytes")

	err := writeImage(raw, device, 999)
	if err == nil {
		t.Fatal("a short copy was reported as a complete write")
	}

	if !strings.Contains(err.Error(), "changed underneath") {
		t.Errorf("the failure does not explain what happened: %v", err)
	}
}

// rbd ANSWERS ENOENT FOR AN ABSENT POOL AND AN ABSENT IMAGE ALIKE, so reaching
// the create path proves only that one of the two is missing. Creating into a
// pool that is not there fails for a second reason, and that second reason is
// what the operator would otherwise be shown.
func TestImportGenerationSaysWhenItIsThePoolThatIsMissing(t *testing.T) {
	raw, device := stageRaw(t, "content")

	f := &importFake{device: device, poolMissing: true}

	_, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt)
	if err == nil {
		t.Fatal("an import into a pool that does not exist reported success")
	}

	if !strings.Contains(err.Error(), "neither does the pool") {
		t.Errorf("the failure blames the image rather than the pool: %v", err)
	}

	if f.ranWith("create", "ubuntu-2404-x64") {
		t.Error("billet tried to create an image in a pool that does not exist")
	}
}

// THE SIZE IS ROUNDED UP TO WHOLE MEGABYTES. Rounding down truncates the end of
// the filesystem, which on ext4 is data rather than slack.
func TestImportGenerationRoundsUpToWholeMegabytes(t *testing.T) {
	for _, tc := range []struct {
		bytes int
		want  string
	}{
		{1, "513M"},                // a single byte still needs one megabyte
		{bytesPerMB, "513M"},       // exactly one
		{bytesPerMB + 1, "514M"},   // one byte over rounds up
		{3*bytesPerMB - 1, "515M"}, // just under three
		{3 * bytesPerMB, "515M"},   // exactly three
	} {
		t.Run(tc.want, func(t *testing.T) {
			raw, device := stageRaw(t, strings.Repeat("x", tc.bytes))

			f := &importFake{device: device}

			if _, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt); err != nil {
				t.Fatalf("import: %v", err)
			}

			if !f.ranWith("create", "ubuntu-2404-x64", "--size", tc.want) {
				t.Errorf("%d bytes was not sized as %s; billet ran %v", tc.bytes, tc.want, f.calls)
			}
		})
	}
}

// TWO IMPORTS IN ONE SECOND PRODUCE ONE NAME. The publish lock makes this
// unreachable in practice, but the failure has to be safe rather than merely
// unlikely: the existing generation is immutable and must not be replaced, and
// its recorded runner version must not be overwritten with the new one.
func TestImportGenerationRefusesToRepublishAnExistingGenerationName(t *testing.T) {
	raw, device := stageRaw(t, "content")

	f := &importFake{device: device, failOn: "snap"}

	_, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt)
	if err == nil {
		t.Fatal("a colliding snapshot name was reported as a successful publish")
	}

	if f.ranWith("image-meta", "set") {
		t.Error("the runner version was recorded against a snapshot that was not created; " +
			"on a real collision that would overwrite the existing generation's version")
	}
}

// THE HEAD IS UNMAPPED EVEN WHEN THE WRITE FAILED AND THE UNMAP ALSO FAILS, and
// the reported error is the write's -- a head left mapped is a consequence of the
// failure, not its cause, so substituting it sends an operator after the wrong
// thing.
func TestImportGenerationReportsTheWriteFailureNotTheCleanupFailure(t *testing.T) {
	raw, device := stageRaw(t, "content")

	f := &importFake{
		device:   filepath.Dir(device), // a directory: the write fails
		unmapErr: errors.New("exit status 16"),
	}

	_, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt)
	if err == nil {
		t.Fatal("writing to a directory was reported as a successful import")
	}

	// THE WRITE'S FAILURE COMES FIRST. Opening a directory fails at OpenFile rather
	// than at the copy, so the message is "cannot write to" -- asserting on the
	// copy's wording instead made this test fail against correct behaviour.
	if !strings.HasPrefix(err.Error(), "ceph: cannot write to") {
		t.Errorf("the cleanup's failure replaced the real one: %v", err)
	}

	if !strings.Contains(err.Error(), "left mapped") {
		t.Errorf("the failed cleanup is not mentioned at all: %v", err)
	}
}

// THE KEY IS `billet.runner_version.<generation>`, so a malformed generation
// writes a key nothing reads -- every reader arrives holding a name that came
// from `rbd snap ls` and therefore parses -- and nothing reaps it, because
// reaping walks generations and a key whose suffix is not one is invisible.
func TestSetRunnerVersionRefusesAGenerationBilletDidNotPublish(t *testing.T) {
	for _, generation := range []string{
		"",
		"   ",
		"latest",
		"20260815041709", // no prefix
		"gnotatimestamp",
		"g2026",
	} {
		f := &importFake{}

		err := importClient(t, f).SetRunnerVersion(
			t.Context(), "ubuntu-2404-x64", generation, "2.336.0")
		if err == nil {
			t.Errorf("%q was accepted as a generation", generation)
		}

		if f.ranWith("image-meta", "set") {
			t.Errorf("a key was written against %q", generation)
		}
	}

	// And the shape billet actually publishes is accepted.
	f := &importFake{}

	if err := importClient(t, f).SetRunnerVersion(
		t.Context(), "ubuntu-2404-x64", "g20260815041709", "2.336.0"); err != nil {
		t.Errorf("a real generation was refused: %v", err)
	}
}

// A CANCELLED CONTEXT MUST NOT LEAK THE PUBLISH LOCK.
//
// The raw write is deliberately unbounded and moves gigabytes, so a caller's
// deadline can expire during it. Releasing on that dead context fails immediately
// and leaves the lock held -- and nothing reclaims an rbd lock, so every publisher
// on every node then refuses for StaleLockAfter. That is the outage the bound
// exists to CAP, not a state to arrive at routinely.
//
// The fake records the context each call ran with, so this asserts the release
// actually ran rather than that some error came back -- an implementation that
// skipped the release entirely would also "not error".
func TestImportGenerationReleasesTheLockEvenWhenTheContextIsDone(t *testing.T) {
	raw, device := stageRaw(t, "content")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// CANCELLED AFTER THE LOCK IS TAKEN AND THE HEAD IS MAPPED, which is where a
	// real deadline expires: during the multi-gigabyte write that follows.
	f := &importFake{device: device, cancelOn: "device map", cancel: cancel}

	_, err := importClient(t, f).ImportGeneration(ctx, "ubuntu-2404-x64", raw, "2.336.0", "", importAt)
	if err == nil {
		t.Fatal("an import whose context expired partway through reported success")
	}

	if f.lockTaken == "" && !f.ranWith("lock", "add") {
		t.Fatal("the lock was never taken, so this test proves nothing about releasing it")
	}

	if f.lockTaken != "" {
		t.Errorf("the publish lock is still held as %q after an import on a cancelled "+
			"context; nothing reclaims an rbd lock, so every publisher on every node "+
			"would refuse for %s", f.lockTaken, StaleLockAfter)
	}
}

// THE WHOLE POINT OF THE HEARTBEAT: a holder that is still counting keeps its lock
// no matter how old the lock looks.
//
// Age alone cannot tell an abandoned lock from a publish that is genuinely taking
// a long time, or from an observer whose clock is ahead. The raw write is
// unbounded and imports up to a terabyte, so an old-but-live holder is not
// hypothetical -- and breaking its lock puts two writers on one image.
func TestImportGenerationWillNotBreakALockWhoseHolderIsStillAlive(t *testing.T) {
	raw, device := stageRaw(t, "content")

	var reads int

	f := &importFake{
		device: device,
		// Held far longer than the bound -- under the old rule this was breakable on
		// sight.
		lockHeld: fmt.Sprintf("billet-build-slowhost-9-%d",
			importAt.Add(-100*StaleLockAfter).Unix()),
		heartbeat: func() (string, bool) {
			reads++

			// The counter moves between the two observations, which is what a live
			// holder looks like.
			return fmt.Sprintf("%d", reads), true
		},
	}

	_, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt)
	if err == nil {
		t.Fatal("an import broke the lock of a holder that was still heartbeating; that " +
			"puts two writers on one head image")
	}

	if !strings.Contains(err.Error(), "moved") {
		t.Errorf("the refusal does not name the evidence of life: %v", err)
	}

	if reads < 2 {
		t.Errorf("the heartbeat was read %d times; the decision needs two observations "+
			"separated by a window, and one reading cannot show movement", reads)
	}

	if f.ranWith("lock", "rm") {
		t.Error("billet broke a live holder's lock")
	}

	if f.ranWith("device", "map") {
		t.Error("billet mapped the head despite standing down")
	}
}

// AND A HOLDER THAT HAS STOPPED COUNTING, past the bound, is still reclaimable --
// otherwise a leaked lock is permanent and every publisher refuses forever.
func TestImportGenerationStillBreaksASilentLockPastTheBound(t *testing.T) {
	raw, device := stageRaw(t, "content")

	f := &importFake{
		device: device,
		lockHeld: fmt.Sprintf("billet-build-deadhost-9-%d",
			importAt.Add(-StaleLockAfter-time.Hour).Unix()),
		heartbeat: func() (string, bool) { return "42", true }, // frozen: not counting
	}

	if _, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw, "2.336.0", "", importAt); err != nil {
		t.Fatalf("a silent lock past the bound was not reclaimed: %v", err)
	}

	if !f.ranWith("lock", "rm", "billet-build-deadhost-9") {
		t.Errorf("the silent lock was not broken; billet ran %v", f.calls)
	}
}

// VERIFICATION AND REAPING EXCLUDE EACH OTHER, and the exclusion has to be mutual
// or neither is safe.
//
// Verification proves a generation exists and then records against it; reaping
// removes generations. Without a shared lock a reap landing between those two
// steps leaves a generation VERIFIED BUT UNPAIRED -- every node takes it up
// through `@verified` and each boots it against its own kernel, which is the state
// the write order exists to prevent.
func TestRecordVerificationTakesThePublishLock(t *testing.T) {
	f := &importFake{}

	err := importClient(t, f).RecordVerification(t.Context(), "ubuntu-2404-x64",
		"g20260815033431", "vmlinux-6.1.155-ea1d42638d13", false, false, importAt)

	// The fake has no snapshots, so this fails on the existence check -- which is
	// itself the point of the next assertion.
	if err == nil {
		t.Fatal("recorded a verification for a generation with no snapshot")
	}

	if !f.ranWith("lock", "add") {
		t.Errorf("recording did not take the publish lock, so a reap could remove the "+
			"generation between the existence check and the writes; billet ran %v", f.calls)
	}
}

// AND IT PROVES THE GENERATION IS STILL THERE. Both writes validate only the NAME,
// so without this a reap that completed first left both keys recreated for a
// snapshot that no longer exists -- and `@verified` then names it.
func TestRecordVerificationRefusesAGenerationThatIsGone(t *testing.T) {
	f := &importFake{}

	err := importClient(t, f).RecordVerification(t.Context(), "ubuntu-2404-x64",
		"g20260815033431", "", true, false, importAt)
	if err == nil {
		t.Fatal("recorded a verification against a generation with no snapshot")
	}

	if !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("the refusal does not say the generation is gone: %v", err)
	}

	if f.ranWith("image-meta", "set") {
		t.Error("metadata was written for a generation that does not exist")
	}
}

// REAPING TAKES THE SAME LOCK. One-sided exclusion is not exclusion.
func TestReapTakesThePublishLock(t *testing.T) {
	f := &importFake{}

	plan := []Reapable{{Generation: Generation{Name: "g20260814072427"}}}

	if _, err := importClient(t, f).Reap(t.Context(), "ubuntu-2404-x64", plan); err != nil {
		t.Fatalf("reap: %v", err)
	}

	if !f.ranWith("lock", "add") {
		t.Errorf("reaping did not take the publish lock, so it could remove a generation a "+
			"verification was in the middle of recording; billet ran %v", f.calls)
	}

	if f.lockTaken != "" {
		t.Errorf("the reap left the publish lock held as %q", f.lockTaken)
	}
}

// EVERYTHING THAT DESCRIBES A GENERATION IS WRITTEN UNDER THE LOCK THAT EXCLUDES
// REAPING.
//
// The kernel pairing used to be written by the caller after the import returned,
// which put it outside the lock: a reap could remove the generation between the
// publish and the pairing, leaving either a key describing nothing or a generation
// published with no pairing -- and an unpaired generation is taken up by every
// node and booted against whatever each is configured with.
func TestImportGenerationRecordsTheKernelBeforeReleasingTheLock(t *testing.T) {
	raw, device := stageRaw(t, "content")

	f := &importFake{device: device}

	gen, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw,
		"2.336.0", "vmlinux-6.1.155-ea1d42638d13", importAt)
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	kernelAt, releaseAt := -1, -1

	for i, call := range f.calls {
		joined := strings.Join(call, " ")

		if kernelAt < 0 && strings.Contains(joined, KernelKey+"."+gen) {
			kernelAt = i
		}

		if releaseAt < 0 && strings.Contains(joined, "lock rm") {
			releaseAt = i
		}
	}

	if kernelAt < 0 {
		t.Fatalf("the kernel pairing was never recorded; billet ran %v", f.calls)
	}

	if releaseAt < 0 {
		t.Fatalf("the publish lock was never released; billet ran %v", f.calls)
	}

	if kernelAt > releaseAt {
		t.Errorf("the kernel was recorded at step %d and the lock released at step %d, so a "+
			"reap could run between the publish and the pairing", kernelAt, releaseAt)
	}
}

// AN IMPORT WHOSE PAIRING CANNOT BE RECORDED WITHDRAWS THE SNAPSHOT, for the same
// reason it does when the runner version cannot be: a generation nothing can
// describe is worse than no generation, because `images due` counts it as recent
// while every check that reads its metadata declines to judge it.
func TestImportGenerationWithdrawsASnapshotWhoseKernelCannotBeRecorded(t *testing.T) {
	raw, device := stageRaw(t, "content")

	f := &importFake{device: device, failOn: "image-meta"}

	if _, err := importClient(t, f).ImportGeneration(t.Context(), "ubuntu-2404-x64", raw,
		"2.336.0", "vmlinux-6.1.155-ea1d42638d13", importAt); err == nil {
		t.Fatal("a generation whose kernel could not be recorded was published")
	}

	if !f.ranWith("snap", "rm") {
		t.Errorf("the snapshot was not withdrawn; billet ran %v", f.calls)
	}
}
