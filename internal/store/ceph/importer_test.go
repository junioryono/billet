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
}

func (f *importFake) run(_ context.Context, _ string, args []string) ([]byte, error) {
	f.calls = append(f.calls, args)

	sub := subcommandOf(args)

	if f.failOn != "" && f.failOn == sub {
		err := f.failErr
		if err == nil {
			err = errors.New("exit status 1")
		}

		return nil, err
	}

	switch sub {
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
	case "create", "resize", "snap", "image-meta":
		return []byte(""), nil
	default:
		return []byte(""), nil
	}
}

func (f *importFake) ranWith(fragments ...string) bool {
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

	c, err := New(valid(), WithBinary("/usr/bin/rbd"), WithCephBinary("/usr/bin/ceph"),
		withRunner(f.run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return c
}

var importAt = time.Date(2026, 8, 15, 4, 17, 9, 0, time.UTC)

func TestImportGenerationWritesTheImageAndPublishesIt(t *testing.T) {
	raw, device := stageRaw(t, "a filesystem, in spirit")

	f := &importFake{device: device}

	gen, err := importClient(t, f).ImportGeneration(
		t.Context(), "ubuntu-2404-x64", raw, "2.336.0", importAt)
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

	if _, err := importClient(t, f).ImportGeneration(
		t.Context(), "ubuntu-2404-x64", raw, "2.336.0", importAt); err != nil {
		t.Fatalf("import: %v", err)
	}

	if !f.ranWith("create", "--size", "3M") {
		t.Errorf("the head was not created at the image's size; billet ran %v", f.calls)
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

	_, err := importClient(t, f).ImportGeneration(
		t.Context(), "ubuntu-2404-x64", raw, "2.336.0", importAt)
	if err == nil {
		t.Fatal("an unreachable cluster was treated as an absent image")
	}

	if f.ranWith("create", "--size") {
		t.Error("billet tried to create the head against a cluster it could not reach")
	}
}

// GROWN, NEVER SHRUNK. Writing a larger filesystem into a head sized for the last
// one fails partway with ENOSPC — a corrupt image behind a successful-looking
// import, because the write is the only step that would have complained.
func TestImportGenerationGrowsAHeadThatIsTooSmall(t *testing.T) {
	raw, device := stageRaw(t, strings.Repeat("x", 5*1024*1024))

	f := &importFake{device: device, infoJSON: `{"size": 2097152}`} // 2 MiB

	if _, err := importClient(t, f).ImportGeneration(
		t.Context(), "ubuntu-2404-x64", raw, "2.336.0", importAt); err != nil {
		t.Fatalf("import: %v", err)
	}

	if !f.ranWith("resize", "--size", "5M") {
		t.Errorf("a head smaller than the image was not grown; billet ran %v", f.calls)
	}
}

// EXISTING SNAPSHOTS KEEP THEIR OWN SIZE, so shrinking reclaims nothing a
// generation still holds and would truncate the next write.
func TestImportGenerationLeavesALargerHeadAlone(t *testing.T) {
	raw, device := stageRaw(t, "small")

	f := &importFake{device: device, infoJSON: fmt.Sprintf(`{"size": %d}`, 8*1024*1024)}

	if _, err := importClient(t, f).ImportGeneration(
		t.Context(), "ubuntu-2404-x64", raw, "2.336.0", importAt); err != nil {
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

	if _, err := importClient(t, f).ImportGeneration(
		t.Context(), "ubuntu-2404-x64", raw, "2.336.0", importAt); err != nil {
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

	if _, err := importClient(t, f).ImportGeneration(
		t.Context(), "ubuntu-2404-x64", raw, "2.336.0", importAt); err == nil {
		t.Fatal("writing to a directory was reported as a successful import")
	}

	if !f.ranWith("device", "unmap") {
		t.Errorf("the head was left mapped after a failed write; billet ran %v", f.calls)
	}
}

func TestImportGenerationRefusesAnEmptyImage(t *testing.T) {
	raw, device := stageRaw(t, "")

	f := &importFake{device: device}

	_, err := importClient(t, f).ImportGeneration(
		t.Context(), "ubuntu-2404-x64", raw, "2.336.0", importAt)
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
			t.Context(), name, raw, "2.336.0", importAt); err == nil {
			t.Errorf("%q was accepted as an image name", name)
		}
	}
}
