package ebss3

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	storecontract "github.com/junioryono/billet/internal/store"
	"github.com/junioryono/billet/internal/store/blockfs"
)

const testInstance = "i-0123456789abcdef0"

// fakeAttach is EC2's attachment half, keeping which volume is on which instance.
type fakeAttach struct {
	// on is where each volume is attached; a missing entry is attached to none.
	on map[string]volumeAttachment
	// gone volumes EC2 says do not exist.
	gone map[string]bool
	// used device names, each refused with EC2's "already in use".
	used map[string]bool
	// attachErr refuses every attach with this error, after the used-name check.
	attachErr error
	// asked are the device names AttachVolume was asked for, in order.
	asked    []string
	detached []string
	now      *time.Time
}

func newFakeAttach(now *time.Time) *fakeAttach {
	return &fakeAttach{on: map[string]volumeAttachment{}, gone: map[string]bool{}, used: map[string]bool{}, now: now}
}

func (f *fakeAttach) AttachVolume(_ context.Context, id, instance, point string) error {
	f.asked = append(f.asked, point)
	if f.used[point] {
		return &awsAPIError{Code: "InvalidParameterValue", Status: 400,
			message: "Invalid value '" + point + "' for unixDevice. Attachment point " + point + " is already in use"}
	}
	if f.attachErr != nil {
		return f.attachErr
	}
	f.used[point] = true
	f.on[id] = volumeAttachment{Instance: instance, State: "attached", Attached: *f.now}

	return nil
}

func (f *fakeAttach) DetachVolume(_ context.Context, id, _ string) error {
	f.detached = append(f.detached, id)
	delete(f.on, id)

	return nil
}

func (f *fakeAttach) Attachment(_ context.Context, id string) (volumeAttachment, bool, error) {
	if f.gone[id] {
		return volumeAttachment{}, false, nil
	}

	return f.on[id], true, nil
}

func (f *fakeAttach) AttachedTo(_ context.Context, instance string) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	for id, a := range f.on {
		if a.Instance == instance {
			out[id] = a.Attached
		}
	}

	return out, nil
}

// fakeDisks is this host's devices: a volume attached here is a device, unless hidden.
type fakeDisks struct {
	attach *fakeAttach
	// hidden volumes never show up as a device, though attached.
	hidden map[string]bool
	// held devices are refused by Unheld.
	held map[string]bool
	// findErr fails every Find.
	findErr error
	// verified are the devices Verify was asked about.
	verified []string
	verify   storecontract.Filesystem
}

func (d *fakeDisks) Find(volume string) (string, bool, error) {
	if d.findErr != nil {
		return "", false, d.findErr
	}
	if a, ok := d.attach.on[volume]; !ok || a.Instance != testInstance || d.hidden[volume] {
		return "", false, nil
	}

	return "/dev/nvme-" + volume, true, nil
}

func (d *fakeDisks) Unheld(device string) error {
	if d.held[device] {
		return fmt.Errorf("%w: %s", blockfs.ErrHeld, device)
	}

	return nil
}

func (d *fakeDisks) Verify(_ context.Context, device string) (storecontract.Filesystem, error) {
	d.verified = append(d.verified, device)

	return d.verify, nil
}

func localTestStore(now *time.Time, named ...string) (*Store, *fakeBlocks, *fakeAttach, *fakeDisks) {
	s, blocks, _ := testStore(now)
	attach := newFakeAttach(now)
	disks := &fakeDisks{attach: attach, hidden: map[string]bool{}, held: map[string]bool{},
		verify: storecontract.Filesystem{Type: "ext4", UUID: "host-uuid", Clean: true}}
	s.host = &Host{InstanceID: testInstance, AvailabilityZone: "us-west-2a"}
	s.attach = attach
	s.disks = disks
	s.sessions = func() (func(string) bool, error) {
		return func(name string) bool { return slices.Contains(named, name) }, nil
	}

	return s, blocks, attach, disks
}

// A SELF-ATTACHED VOLUME IS HANDED OVER AS THE DEVICE ITS SERIAL PROVED, attached to this
// instance, under the next name EC2 does not already use.
func TestACreatedVolumeIsAttachedHereUnderAFreeName(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	s, _, attach, _ := localTestStore(&now)
	attach.used["/dev/sdf"] = true

	volume, err := s.Create(t.Context(), "sticky/a", 1<<30)
	if err != nil {
		t.Fatal(err)
	}

	if volume.Device != "/dev/nvme-"+volume.Handle {
		t.Fatalf("the volume was handed over as %q, not the device its serial proved", volume.Device)
	}
	if got := attach.on[volume.Handle]; got.Instance != testInstance {
		t.Fatalf("the volume is attached to %q, not this instance", got.Instance)
	}
	if !slices.Equal(attach.asked, []string{"/dev/sdf", "/dev/sdg"}) {
		t.Fatalf("names asked for: %v, want /dev/sdf refused and /dev/sdg taken", attach.asked)
	}
}

// THE NAMES OUTLAST EC2'S LIMIT: past /dev/sdz the store asks for /dev/xvdba, so a metal
// host's 30 attachments are reachable rather than stopped at 21 by billet's own list.
func TestTheNamesReachPastTwentyOneAttachments(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	s, _, attach, _ := localTestStore(&now)
	for c := 'f'; c <= 'z'; c++ {
		attach.used["/dev/sd"+string(c)] = true
	}

	if _, err := s.Create(t.Context(), "sticky/a", 1<<30); err != nil {
		t.Fatal(err)
	}
	if got := attach.asked[len(attach.asked)-1]; got != "/dev/xvdba" {
		t.Fatalf("with /dev/sdf to /dev/sdz in use the volume was attached as %s, not /dev/xvdba", got)
	}
	if len(attachmentPoints) < 31 {
		t.Fatalf("billet asks for %d names, fewer than the 31 attachments EC2 allows a metal host", len(attachmentPoints))
	}
}

// A CLONE IS ATTACHED HERE TOO, from the generation it was made from.
func TestACloneIsAttachedHere(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	s, blocks, attach, _ := localTestStore(&now)
	publishOne(t, s, "sticky/a")

	clone, err := s.Clone(t.Context(), "sticky/a", "")
	if err != nil {
		t.Fatal(err)
	}
	if clone.Device != "/dev/nvme-"+clone.Handle || attach.on[clone.Handle].Instance != testInstance {
		t.Fatalf("the clone %+v is not attached here as its device", clone)
	}
	if blocks.createdFrom[clone.Handle] != clone.Generation {
		t.Fatalf("the clone was made from %q, not its generation %q", blocks.createdFrom[clone.Handle], clone.Generation)
	}
}

// ANOTHER REFUSAL OF A NAME IS NOT RETRIED: one outside EC2's scheme shares the code
// "already in use" has, and retrying it under every name would only hide it. The volume
// is deleted and its active record dropped.
func TestARefusedAttachReleasesTheVolume(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	s, blocks, attach, _ := localTestStore(&now)
	attach.attachErr = &awsAPIError{Code: "InvalidParameterValue", Status: 400,
		message: "Value (/dev/sdf) for parameter device is invalid. /dev/sdf is not a valid EBS device name."}

	if _, err := s.Create(t.Context(), "sticky/a", 1<<30); err == nil {
		t.Fatal("a refused attach answered a volume")
	}
	if len(attach.asked) != 1 {
		t.Fatalf("a refusal that is not \"in use\" was retried under %v", attach.asked)
	}
	if !slices.Equal(blocks.deletedVolume, []string{"vol-1"}) {
		t.Fatalf("the unattached volume was not deleted: %v", blocks.deletedVolume)
	}
	assertNoActive(t, s, "sticky/a")
}

// A VOLUME EC2 ATTACHED AND THE HOST NEVER SHOWED is taken off and deleted, its device
// unseen being proof nothing here holds it.
func TestAVolumeThatNeverAppearsIsDetachedAndDeleted(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	s, blocks, attach, disks := localTestStore(&now)
	disks.hidden["vol-1"] = true

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	if _, err := s.Create(ctx, "sticky/a", 1<<30); err == nil {
		t.Fatal("a volume no device showed was answered")
	}
	if !slices.Equal(attach.detached, []string{"vol-1"}) || !slices.Equal(blocks.deletedVolume, []string{"vol-1"}) {
		t.Fatalf("detached %v, deleted %v; want the volume detached and then deleted", attach.detached, blocks.deletedVolume)
	}
}

// A HELD DEVICE IS NEVER DETACHED. EBS detaches a volume in use without refusing, so a
// discard that cannot prove nothing holds it leaves the volume where it is.
func TestADiscardNeverDetachesAHeldDevice(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	s, blocks, attach, disks := localTestStore(&now)

	volume, err := s.Create(t.Context(), "sticky/a", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	disks.held[volume.Device] = true

	err = s.Discard(t.Context(), volume)
	if !errors.Is(err, blockfs.ErrHeld) {
		t.Fatalf("a discard of a held device answered %v", err)
	}
	if len(attach.detached) != 0 || len(blocks.deletedVolume) != 0 {
		t.Fatalf("a held device was detached %v or deleted %v", attach.detached, blocks.deletedVolume)
	}

	delete(disks.held, volume.Device)
	if err := s.Discard(t.Context(), volume); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(attach.detached, []string{volume.Handle}) || !slices.Equal(blocks.deletedVolume, []string{volume.Handle}) {
		t.Fatalf("detached %v, deleted %v once nothing held it", attach.detached, blocks.deletedVolume)
	}
	assertNoActive(t, s, "sticky/a")
}

// A SNAPSHOT IS PUBLISHED UNDER THIS HOST'S PROOF, from the device itself, never the
// guest's word, and only after the volume is off the instance.
func TestASnapshotIsProvedHereAndDetachedFirst(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	s, blocks, attach, disks := localTestStore(&now)

	volume, err := s.Create(t.Context(), "sticky/a", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	// What the guest said, which is not what is published.
	volume.Filesystem = storecontract.Filesystem{Type: "ext4", UUID: "guest-uuid", Clean: true}

	candidate, err := s.Snapshot(t.Context(), volume)
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(disks.verified, []string{volume.Device}) {
		t.Fatalf("the host verified %v, want the volume's own device", disks.verified)
	}
	if candidate.Filesystem.UUID != "host-uuid" {
		t.Fatalf("the candidate carries %q, not the host's proof", candidate.Filesystem.UUID)
	}
	if !slices.Equal(attach.detached, []string{volume.Handle}) {
		t.Fatalf("the volume was not detached before its snapshot: %v", attach.detached)
	}
	if len(blocks.snapshotTokens) != 1 || !slices.Equal(blocks.deletedVolume, []string{volume.Handle}) {
		t.Fatalf("snapshots %v, deleted %v", blocks.snapshotTokens, blocks.deletedVolume)
	}
}

// A SNAPSHOT OF A HELD DEVICE IS REFUSED before anything leaves the instance.
func TestASnapshotOfAHeldDeviceIsRefused(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	s, blocks, attach, disks := localTestStore(&now)

	volume, err := s.Create(t.Context(), "sticky/a", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	disks.held[volume.Device] = true

	if _, err := s.Snapshot(t.Context(), volume); !errors.Is(err, blockfs.ErrHeld) {
		t.Fatalf("a snapshot of a held device answered %v", err)
	}
	if len(attach.detached) != 0 || len(blocks.snapshotTokens) != 0 {
		t.Fatalf("a held device was detached %v or snapshotted %v", attach.detached, blocks.snapshotTokens)
	}
}

// A VOLUME ON ANOTHER INSTANCE IS LEFT THERE, never detached from it.
func TestAVolumeOnAnotherInstanceIsLeftAlone(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	s, blocks, attach, _ := localTestStore(&now)
	attach.on["vol-9"] = volumeAttachment{Instance: "i-0fffffffffffffff0", State: "attached"}

	err := s.Discard(t.Context(), storecontract.Volume{Key: "sticky/a", Handle: "vol-9", Device: "/dev/nvme9n1"})
	if err == nil || !strings.Contains(err.Error(), "i-0fffffffffffffff0") {
		t.Fatalf("a discard of another instance's volume answered %v", err)
	}
	if len(attach.detached) != 0 || len(blocks.deletedVolume) != 0 {
		t.Fatalf("another instance's volume was detached %v or deleted %v", attach.detached, blocks.deletedVolume)
	}
}

// THE DEVICE IS RE-PROVED, and a volume EC2 says is gone is ErrVolumeGone while one on no
// device here is could-not-tell.
func TestMappedDeviceProvesOrSaysWhy(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	s, _, attach, disks := localTestStore(&now)

	volume, err := s.Create(t.Context(), "sticky/a", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if device, err := s.MappedDevice(t.Context(), volume); err != nil || device != volume.Device {
		t.Fatalf("an attached volume's device answered %q, %v", device, err)
	}

	disks.hidden[volume.Handle] = true
	if _, err := s.MappedDevice(t.Context(), volume); !errors.Is(err, storecontract.ErrDeviceUnknown) {
		t.Fatalf("a volume on no device here answered %v, want could-not-tell", err)
	}

	attach.gone[volume.Handle] = true
	if _, err := s.MappedDevice(t.Context(), volume); !errors.Is(err, storecontract.ErrVolumeGone) {
		t.Fatalf("a volume EC2 says is gone answered %v", err)
	}

	disks.findErr = errors.New("sysfs unreadable")
	if _, err := s.MappedDevice(t.Context(), volume); !errors.Is(err, storecontract.ErrDeviceUnknown) {
		t.Fatalf("an unreadable host answered %v, want could-not-tell", err)
	}
}

// A REMOTE STORE'S VOLUME IS ITS OWN NAME, as it was before the store could prove one.
func TestARemoteVolumesDeviceIsItsID(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	s, _, _ := testStore(&now)

	device, err := s.MappedDevice(t.Context(), storecontract.Volume{Handle: "vol-1", Device: "vol-1"})
	if err != nil || device != "vol-1" {
		t.Fatalf("a remote volume's device answered %q, %v", device, err)
	}
}

// THE SWEEP TAKES ONLY WHAT NO SESSION NAMES AND HAS BEEN ATTACHED A WHILE, and leaves a
// held one where it is.
func TestTheSweepTakesOnlyOldUnclaimedVolumes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	s, blocks, attach, disks := localTestStore(&now, "vol-named")
	old := now.Add(-time.Hour)
	for _, id := range []string{"vol-named", "vol-orphan", "vol-held"} {
		attach.on[id] = volumeAttachment{Instance: testInstance, State: "attached", Attached: old}
	}
	attach.on["vol-young"] = volumeAttachment{Instance: testInstance, State: "attached", Attached: now.Add(-time.Minute)}
	attach.on["vol-elsewhere"] = volumeAttachment{Instance: "i-0fffffffffffffff0", State: "attached", Attached: old}
	disks.held["/dev/nvme-vol-held"] = true

	removed, err := s.SweepAttachments(t.Context())
	if !errors.Is(err, blockfs.ErrHeld) {
		t.Fatalf("the sweep answered %v, want the held volume reported", err)
	}
	if !slices.Equal(removed, []string{"vol-orphan"}) || !slices.Equal(blocks.deletedVolume, []string{"vol-orphan"}) {
		t.Fatalf("the sweep removed %v and deleted %v, want only the old unclaimed one", removed, blocks.deletedVolume)
	}
}

// A STORE ON AN INSTANCE IN ANOTHER ZONE IS REFUSED, since none of its volumes could
// attach there.
func TestAnInstanceInAnotherZoneIsRefused(t *testing.T) {
	t.Parallel()

	cfg := config.EBSS3Config{Region: "us-west-2", AvailabilityZone: "us-west-2a", Bucket: "billet-cache-example"}
	_, err := NewAttached(cfg, "example/home", staticCredentials{},
		Host{InstanceID: testInstance, AvailabilityZone: "us-west-2b"},
		func() (func(string) bool, error) { return func(string) bool { return false }, nil })
	if err == nil || !strings.Contains(err.Error(), "us-west-2b") {
		t.Fatalf("an instance in another zone answered %v", err)
	}
}

// ONLY EC2'S "ALREADY IN USE" IS A NAME TO MOVE PAST.
func TestOnlyAnAttachmentPointInUseIsRetried(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&awsAPIError{Code: "InvalidParameterValue", message: "Invalid value '/dev/sdf' for unixDevice. Attachment point /dev/sdf is already in use"}, true},
		{&awsAPIError{Code: "InvalidParameterValue", message: "Value (/dev/nvme5n1) for parameter device is invalid. /dev/nvme5n1 is not a valid EBS device name."}, false},
		{&awsAPIError{Code: "VolumeInUse", message: "vol-1 is already attached to an instance"}, false},
		{errors.New("Attachment point /dev/sdf is already in use"), false},
	} {
		if got := attachmentPointInUse(tc.err); got != tc.want {
			t.Errorf("attachmentPointInUse(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// A VOLUME IS FOUND BY ITS SERIAL in sysfs, and two devices with one serial are refused
// rather than chosen between.
func TestSysDisksFindsAVolumeBySerial(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	stage := func(name, serial string) {
		t.Helper()
		dir := filepath.Join(root, name, "device")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if serial != "" {
			if err := os.WriteFile(filepath.Join(dir, "serial"), []byte(serial+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	stage("nvme0n1", "vol0037520decafda6ab")
	stage("nvme1n1", "vol0f294da5b97802846")
	stage("loop0", "")

	disks := sysDisks{root: root}

	device, found, err := disks.Find("vol-0f294da5b97802846")
	if err != nil || !found || device != "/dev/nvme1n1" {
		t.Fatalf("Find answered %q, %v, %v", device, found, err)
	}

	if _, found, err := disks.Find("vol-0aaaaaaaaaaaaaaaa"); err != nil || found {
		t.Fatalf("an absent volume answered found=%v, %v", found, err)
	}

	stage("nvme2n1", "vol0f294da5b97802846")
	if _, _, err := disks.Find("vol-0f294da5b97802846"); err == nil {
		t.Fatal("two devices with one serial were chosen between")
	}
}

func assertNoActive(t *testing.T, s *Store, key string) {
	t.Helper()

	state, _, err := s.load(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Active) != 0 {
		t.Fatalf("cache %q still records active leases %v", key, state.Active)
	}
}

// publishOne publishes a first generation of key, so a clone has one to come from.
func publishOne(t *testing.T, s *Store, key string) {
	t.Helper()

	volume, err := s.Create(t.Context(), key, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.Snapshot(t.Context(), volume)
	if err != nil {
		t.Fatal(err)
	}
	lease, fence, err := s.AcquireWriter(t.Context(), key, "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PublishCAS(t.Context(), key, "", candidate, lease, fence); err != nil {
		t.Fatal(err)
	}
}
