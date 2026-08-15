package firecracker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// A REAL MICROVM, LAUNCHED AND DESTROYED THROUGH THE REAL JAILER.
//
// The fake-VMM tests assert what billet SAYS; this asserts that what it says works.
// It is the same shape as the docker backend's realdocker_test.go and it exists for
// the same reason: every defect this backend's design turned on — the chroot name,
// the exit code that means nothing, the cgroup that must always be asked for — was
// found by running the thing, and none of them is reachable from a fake.
//
// It skips unless the machine can actually do it, which is the reference host and
// nowhere else. Everything it needs beyond firecracker itself is stated in the skip
// messages, so a host that is nearly ready says which part is missing.
func TestRealFirecrackerLaunchAndDestroy(t *testing.T) {
	env := requireRealHost(t)

	p, err := New("billet-selftest", env.cfg, env.disk,
		WithBootWait(20*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// A UNIQUE LEASE PER RUN, because the jailer refuses an id whose chroot
	// survives — which is exactly what a previous failed run leaves behind.
	name := provider.InstanceName(env.lease)

	t.Cleanup(func() {
		// WithoutCancel: the test context is done by the time cleanup runs, and a
		// cleanup that cannot run leaves a microVM, a jail, a tap and a mapped disk.
		if err := p.Destroy(context.WithoutCancel(t.Context()), name); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	inst, err := p.Launch(t.Context(), provider.Spec{
		Name:      name,
		Image:     env.image,
		VCPU:      2,
		Memory:    1 * config.GiB,
		Command:   []string{"./run.sh"},
		Trust:     provider.TrustTrusted,
		JITConfig: "not-a-real-registration",
	})
	if err != nil {
		t.Fatalf("Launch against real firecracker: %v", err)
	}

	// THE JAIL IS WHERE THE JAILER PUT IT, not where the configured path suggests.
	// This is the assertion the whole design turned on, and it can only be made
	// against the real jailer: a fake would agree with whatever billet computed.
	j := p.jailFor(name)

	if _, err := os.Stat(j.socket()); err != nil {
		t.Errorf("no api socket at the path billet derives, so the jailer used another: %v", err)
	}

	// AND THE VMM IS ACTUALLY RUNNING, which is the claim Launch makes.
	if !inst.Running {
		t.Error("Launch reported an instance that is not running")
	}

	found, ok, err := p.Find(t.Context(), name)
	if err != nil || !ok {
		t.Fatalf("Find: found=%v err=%v", ok, err)
	}

	if !found.Running {
		t.Error("Find reported a live microVM as not running")
	}

	listed, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var seen bool

	for _, i := range listed {
		if i.Name == name {
			seen = true
		}
	}

	if !seen {
		t.Errorf("List did not report the microVM it just launched: %+v", listed)
	}

	// THE CREDENTIAL IS NOT IN ARGV. Asserted against the real process table
	// rather than against what billet recorded, because /proc is where a local
	// process would actually read it.
	if out, err := exec.CommandContext(t.Context(), "ps", "-eo", "args").Output(); err == nil {
		if strings.Contains(string(out), "not-a-real-registration") {
			t.Error("the runner registration is visible in the process table")
		}
	}
}

// AND A DESTROY LEAVES NOTHING, which is the half the fake cannot check: a jail
// that survives blocks every relaunch of that lease, and a mapped device holds pool
// space no sweep will find.
func TestRealFirecrackerDestroyLeavesNothing(t *testing.T) {
	env := requireRealHost(t)

	p, err := New("billet-selftest", env.cfg, env.disk, WithBootWait(20*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	name := provider.InstanceName(env.lease)

	if _, err := p.Launch(t.Context(), provider.Spec{
		Name: name, Image: env.image, VCPU: 1, Memory: 512 * config.MiB,
		Command: []string{"./run.sh"}, Trust: provider.TrustTrusted,
		JITConfig: "not-a-real-registration",
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := p.Destroy(t.Context(), name); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	j := p.jailFor(name)

	if _, err := os.Stat(j.dir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the jail survived: %v", err)
	}

	// THE TAP TOO. Nothing enumerates network devices looking for orphans, so one
	// left attached is invisible — and its NAME is allocated rather than derived, so
	// the claim directory is what says whether it came back.
	if left, err := p.claimedBy(name); err != nil {
		t.Errorf("read what %s still holds: %v", name, err)
	} else if left != (resources{}) {
		t.Errorf("%s still holds %+v after its destroy", name, left)
	}

	// AND DESTROYING AGAIN IS STILL SUCCESS.
	if err := p.Destroy(t.Context(), name); err != nil {
		t.Errorf("the second Destroy: %v", err)
	}
}

// realHost is what a live launch needs, and where it came from.
type realHost struct {
	cfg   config.FirecrackerConfig
	disk  RootDisk
	image string
	lease string
}

// requireRealHost skips unless this machine can launch a microVM for real, saying
// which part is missing when it cannot.
func requireRealHost(t *testing.T) realHost {
	t.Helper()

	// NOT PARALLEL, and deliberately: these share the host's jailer, its cgroup
	// tree, its bridge and its ceph pools, which is precisely the process-global
	// state the package's testing rules say must run alone.
	if os.Geteuid() != 0 {
		t.Skip("not root: the jailer chroots, mknods a device node and attaches a tap")
	}

	if err := checkKVM(); err != nil {
		t.Skipf("no usable /dev/kvm: %v", err)
	}

	image := os.Getenv("BILLET_TEST_GOLDEN_IMAGE")
	if image == "" {
		t.Skip("set BILLET_TEST_GOLDEN_IMAGE to a golden image in the ceph image pool, " +
			"written image@snapshot, to run this")
	}

	bridge := os.Getenv("BILLET_TEST_BRIDGE")
	if bridge == "" {
		t.Skip("set BILLET_TEST_BRIDGE to a bridge on this host to run this")
	}

	cfg := config.FirecrackerConfig{
		KernelImage: os.Getenv("BILLET_TEST_KERNEL"),
		Bridge:      bridge,
	}
	cfg.Normalize()

	if cfg.KernelImage == "" {
		t.Skip("set BILLET_TEST_KERNEL to an uncompressed guest kernel to run this")
	}

	for _, bin := range []string{cfg.BinaryPath, cfg.JailerPath} {
		if _, err := os.Stat(bin); err != nil {
			t.Skipf("%s is not installed: %v", bin, err)
		}
	}

	if errs := config.CheckFirecracker(cfg); len(errs) > 0 {
		t.Skipf("the environment does not describe a usable host: %v", errs)
	}

	return realHost{
		cfg:   cfg,
		disk:  realDisk(t),
		image: image,
		lease: freshLease(t),
	}
}

// realDisk drives the actual rbd command, through the same shape the wiring uses.
//
// A LOCAL TYPE RATHER THAN internal/store/ceph, because a provider may not import
// the store — the layering depguard enforces. It runs the same two commands the
// ceph client does, which is what makes this a test of the launch path rather than
// of the storage.
func realDisk(t *testing.T) RootDisk {
	t.Helper()

	pool := os.Getenv("BILLET_TEST_CACHE_POOL")
	if pool == "" {
		t.Skip("set BILLET_TEST_CACHE_POOL to the ceph pool per-job clones live in")
	}

	images := os.Getenv("BILLET_TEST_IMAGE_POOL")
	if images == "" {
		t.Skip("set BILLET_TEST_IMAGE_POOL to the ceph pool golden images live in")
	}

	return rbdDisk{images: images, cache: pool, id: os.Getenv("BILLET_TEST_CEPH_USER")}
}

// rbdDisk is the smallest thing that satisfies RootDisk against a live cluster.
type rbdDisk struct {
	images, cache, id string
}

func (d rbdDisk) args(rest ...string) []string {
	if d.id == "" {
		return rest
	}

	return append([]string{"--id", d.id}, rest...)
}

// The real host test always names an explicit generation, which is what a resolver
// returns unchanged — so this stands in for the store without needing one.
func (d rbdDisk) ResolveGeneration(_ context.Context, image string) (string, error) {
	return image, nil
}

func (d rbdDisk) CloneRoot(ctx context.Context, image, name string) (string, error) {
	if err := exec.CommandContext(ctx, "rbd", d.args("clone",
		d.images+"/"+image, d.cache+"/"+name)...).Run(); err != nil {
		return "", err
	}

	out, err := exec.CommandContext(ctx, "rbd", d.args("device", "map",
		d.cache+"/"+name)...).Output()
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(out)), nil
}

func (d rbdDisk) DiscardRoot(ctx context.Context, name string) error {
	//nolint:errcheck // both are idempotent teardown; the caller cannot act on either
	_ = exec.CommandContext(ctx, "rbd", d.args("device", "unmap", d.cache+"/"+name)...).Run()

	//nolint:errcheck // as above
	_ = exec.CommandContext(ctx, "rbd", d.args("rm", d.cache+"/"+name)...).Run()

	return nil
}

// KernelFor answers "nothing recorded". This harness boots against a real cluster
// with the kernel the test configures, which is exactly the unpaired case -- and
// pretending otherwise would have the launch look for a kernel this test never
// installed.
func (d rbdDisk) KernelFor(_ context.Context, _, _ string) (string, bool, error) {
	return "", false, nil
}

// freshLease is a lease id of the shape alloc mints, unique per run so a previous
// failed run's leftovers cannot be mistaken for this one's.
func freshLease(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile("/proc/sys/kernel/random/uuid")
	if err != nil {
		t.Skipf("cannot mint a lease id on this machine: %v", err)
	}

	id := strings.ReplaceAll(strings.TrimSpace(string(raw)), "-", "")
	if len(id) != leaseIDLength {
		t.Skipf("the uuid source produced %d characters, not %d", len(id), leaseIDLength)
	}

	return id
}
