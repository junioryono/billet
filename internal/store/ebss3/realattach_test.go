package ebss3

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/store/blockfs"
)

// TestRealSelfAttach proves the self-attaching store on a real EC2 instance, as root,
// under whatever credential the instance holds: run it under the role `billet init iam`
// prints for a metal node to prove that policy too. It creates a cache volume attached
// here, formats and fills it, is refused a snapshot while the volume is held, publishes
// it, clones it back attached here and reads it, and leaves a fresh attachment to the
// sweep. Timings are logged for the record.
//
// Opt-in, on an instance in BILLET_LIVE_EBSS3_ZONE, and removes everything it made:
//
//	BILLET_LIVE_EBSS3_SELF_ATTACH=1 BILLET_LIVE_EBSS3_BUCKET=<bucket> BILLET_LIVE_EBSS3_REGION=<region> \
//	BILLET_LIVE_EBSS3_ZONE=<zone> BILLET_LIVE_EBSS3_PREFIX=<prefix> BILLET_LIVE_EBSS3_OWNER=<deployment>/<site> \
//	go test -run TestRealSelfAttach
func TestRealSelfAttach(t *testing.T) {
	if os.Getenv("BILLET_LIVE_EBSS3_SELF_ATTACH") != "1" {
		t.Skip("BILLET_LIVE_EBSS3_SELF_ATTACH is not 1; this test attaches real EBS volumes to this instance")
	}
	if os.Geteuid() != 0 {
		t.Fatal("this test formats and mounts a block device, so it runs as root")
	}

	ctx := t.Context()

	doc, _, err := awscreds.InstanceIdentity(ctx, "")
	if err != nil {
		t.Fatalf("read this instance's identity: %v", err)
	}
	var identity struct {
		InstanceID       string `json:"instanceId"`
		AvailabilityZone string `json:"availabilityZone"`
	}
	if err := json.Unmarshal(doc, &identity); err != nil {
		t.Fatal(err)
	}

	owner := os.Getenv("BILLET_LIVE_EBSS3_OWNER")
	if owner == "" {
		var nonce [8]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			t.Fatal(err)
		}
		owner = "live-test-" + hex.EncodeToString(nonce[:]) + "/site"
	}

	store, err := NewAttached(config.EBSS3Config{
		Region: os.Getenv("BILLET_LIVE_EBSS3_REGION"), AvailabilityZone: os.Getenv("BILLET_LIVE_EBSS3_ZONE"),
		Bucket: os.Getenv("BILLET_LIVE_EBSS3_BUCKET"), Prefix: os.Getenv("BILLET_LIVE_EBSS3_PREFIX"),
	}, owner, awscreds.Default(),
		Host{InstanceID: identity.InstanceID, AvailabilityZone: identity.AvailabilityZone},
		func() (func(string) bool, error) { return func(string) bool { return false }, nil })
	if err != nil {
		t.Fatalf("NewAttached: %v", err)
	}

	// EVERYTHING BUT THE STATE INDEX IS REMOVED HERE. A node's role deletes no state
	// object, by design (iam.go): only an operator's decommission does, so under the
	// node's role the purge's last step is refused, and the caller removes the index.
	defer func() {
		report, err := store.Purge(ctx)
		if err != nil && !strings.Contains(err.Error(), "S3 DELETE returned AccessDenied") {
			t.Errorf("purge what the test made: %v", err)
		}
		t.Logf("purged %+v", report)
		residue, err := store.Residue(ctx)
		residue.StateObjects = 0
		if err != nil || !residue.Empty() {
			t.Errorf("the test left %+v (%v)", residue, err)
		}
	}()

	const key = "live/sticky"

	started := time.Now()
	volume, err := store.Create(ctx, key, 1<<30)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Logf("create and attach of an empty 1 GiB volume: %s, %s on %s", time.Since(started).Round(time.Millisecond),
		volume.Handle, volume.Device)
	if !strings.HasPrefix(volume.Device, "/dev/") {
		t.Fatalf("the volume was handed over as %q, not a device here", volume.Device)
	}

	run(t, "mkfs.ext4", "-q", volume.Device)
	mount := t.TempDir()
	run(t, "mount", volume.Device, mount)
	if err := os.WriteFile(filepath.Join(mount, "probe"), []byte("written on the first volume\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "umount", mount)

	// HELD, IT IS NEVER DETACHED: a process with the device open is what a VMM is.
	held, err := os.Open(volume.Device)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Snapshot(ctx, volume); !errors.Is(err, blockfs.ErrHeld) {
		_ = held.Close()
		t.Fatalf("a snapshot of a held volume answered %v", err)
	}
	if attachment, _, err := store.attach.Attachment(ctx, volume.Handle); err != nil ||
		attachment.Instance != identity.InstanceID {
		_ = held.Close()
		t.Fatalf("the held volume is attached to %q (%v), not this instance", attachment.Instance, err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}

	started = time.Now()
	candidate, err := store.Snapshot(ctx, volume)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	t.Logf("verify, detach and snapshot: %s, %s (%s)", time.Since(started).Round(time.Millisecond),
		candidate.Generation, candidate.Filesystem.UUID)

	lease, fence, err := store.AcquireWriter(ctx, key, "live-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PublishCAS(ctx, key, "", candidate, lease, fence); err != nil {
		t.Fatalf("PublishCAS: %v", err)
	}

	started = time.Now()
	clone, err := store.Clone(ctx, key, "")
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	t.Logf("clone and attach from the snapshot: %s, %s on %s", time.Since(started).Round(time.Millisecond),
		clone.Handle, clone.Device)

	if device, err := store.MappedDevice(ctx, clone); err != nil || device != clone.Device {
		t.Fatalf("the clone's device is proved as %q (%v), not %q", device, err, clone.Device)
	}

	started = time.Now()
	run(t, "mount", "-o", "ro", clone.Device, mount)
	body, err := os.ReadFile(filepath.Join(mount, "probe"))
	run(t, "umount", mount)
	if err != nil || string(body) != "written on the first volume\n" {
		t.Fatalf("the clone holds %q (%v)", body, err)
	}
	t.Logf("mount and first read of the clone: %s", time.Since(started).Round(time.Millisecond))

	// A FRESH ATTACHMENT NO SESSION NAMES IS LEFT TO ITS SESSION, for now.
	if removed, err := store.SweepAttachments(ctx); err != nil || len(removed) != 0 {
		t.Fatalf("the sweep took %v (%v) of attachments a minute old", removed, err)
	}

	started = time.Now()
	if err := store.Discard(ctx, clone); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	t.Logf("detach and delete: %s", time.Since(started).Round(time.Millisecond))
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()

	if out, err := exec.CommandContext(t.Context(), name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
	}
}
