package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/usage"
)

// measuredProvider is a backend whose instances have host counters: a cgroup
// under a fixture root that disappears when the instance is destroyed, as a
// real one does.
type measuredProvider struct {
	*fakeProvider
	root      string
	targetErr error
}

func (p *measuredProvider) cgroupFor(id string) string { return "/cg/" + id }

func (p *measuredProvider) UsageTarget(_ context.Context, id string) (provider.UsageTarget, error) {
	if p.targetErr != nil {
		return provider.UsageTarget{}, p.targetErr
	}

	return provider.UsageTarget{CgroupDir: p.cgroupFor(id)}, nil
}

func (p *measuredProvider) writeCPU(t *testing.T, id, usec string) {
	t.Helper()

	dir := filepath.Join(p.root, p.cgroupFor(id))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "usage_usec " + usec + "\nuser_usec " + usec + "\nsystem_usec 0\n"
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (p *measuredProvider) Destroy(ctx context.Context, id string) (provider.Teardown, error) {
	if err := os.RemoveAll(filepath.Join(p.root, p.cgroupFor(id))); err != nil {
		return provider.TeardownRequested, err
	}

	return p.fakeProvider.Destroy(ctx, id)
}

// A JOB IS MEASURED FROM LAUNCH TO DESTROY AND THE REPORT REACHES THE LEDGER
// FENCED ON ITS LEASE. The last sample is taken before the compute goes: the
// fake's destroy removes the cgroup, so a sample taken after it would carry
// only the reading from launch.
func TestAJobsUsageIsSampledBeforeItsComputeGoesAndReported(t *testing.T) {
	root := t.TempDir()
	p := &measuredProvider{fakeProvider: &fakeProvider{kind: config.ProviderDocker}, root: root}
	a, host := newAllocatorWithHost(t)
	monitor := usage.NewMonitor(root, usage.Options{Interval: time.Second})
	r := New(a, host, &fakeJIT{setID: 7}, p, nil, WithMonitor(monitor))

	lease := assignedLease(t, a)
	name := provider.InstanceName(lease.ID)
	id := "instance-" + name // the fake's instance id for that name
	p.writeCPU(t, id, "1000")

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: lease.RequestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	p.writeCPU(t, id, "9000000")

	if err := r.Destroy(t.Context(), lease.RequestID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	got, err := a.LeaseUsage(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("LeaseUsage: %v", err)
	}
	if got.CPUUserMicros != 9_000_000 {
		t.Errorf("cpu = %d µs, want the 9000000 read just before the destroy", got.CPUUserMicros)
	}
	if !got.Measured(alloc.UsageCPU) || got.Measured(alloc.UsageMemory) || got.Measured(alloc.UsageEnergy) {
		t.Errorf("unmeasured = %v, want cpu measured and memory and energy not", got.Unmeasured)
	}
	if _, err := a.LeaseUsageSeries(t.Context(), lease.ID); err != nil {
		t.Errorf("no series was kept: %v", err)
	}
	if _, ok := monitor.Final(name); ok {
		t.Error("the monitor still holds a job whose compute is gone")
	}
}

// MEASUREMENT IS OFF THE JOB'S PATH: a backend that cannot say where the
// counters are costs the report and nothing else.
func TestAJobThatCannotBeMeasuredStillRunsAndStops(t *testing.T) {
	root := t.TempDir()
	p := &measuredProvider{fakeProvider: &fakeProvider{kind: config.ProviderDocker}, root: root,
		targetErr: errors.New("no cgroup")}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil,
		WithMonitor(usage.NewMonitor(root, usage.Options{Interval: time.Second})))

	lease := assignedLease(t, a)
	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: lease.RequestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := r.Destroy(t.Context(), lease.RequestID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := a.LeaseUsage(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Errorf("LeaseUsage = %v, want nothing recorded", err)
	}
}

// The series codec the sampler writes is the one the ledger accepts.
func TestTheSeriesCodecsAgree(t *testing.T) {
	if usage.SeriesCodec != alloc.UsageSeriesCodec {
		t.Fatalf("usage writes codec %d and the ledger accepts %d", usage.SeriesCodec, alloc.UsageSeriesCodec)
	}
}
