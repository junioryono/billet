package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	// target, when set, is the answer to every UsageTarget.
	target *provider.UsageTarget
}

func (p *measuredProvider) cgroupFor(id string) string { return "/cg/" + id }

func (p *measuredProvider) UsageTarget(_ context.Context, id string) (provider.UsageTarget, error) {
	if p.targetErr != nil {
		return provider.UsageTarget{}, p.targetErr
	}
	if p.target != nil {
		return *p.target, nil
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
	monitor := runningMonitor(t, root)
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

// startedMonitor records the target each job is started with.
type startedMonitor struct{ targets []usage.Target }

func (m *startedMonitor) Start(_ string, target usage.Target, _ int) {
	m.targets = append(m.targets, target)
}
func (m *startedMonitor) Final(string) (usage.Summary, bool) { return usage.Summary{}, false }
func (m *startedMonitor) Forget(string)                      {}

// EVERY FIELD A BACKEND SAYS REACHES THE SAMPLER. The provider's target and the
// sampler's are two structs with the same fields, and one the conversion drops
// is a group silently never measured (a container's network namespace, say).
// Every field is set to a value distinct from its zero and its neighbours',
// and each must arrive under the same name with the same value; the boolean
// fields are set one at a time, so one copied from another is seen too.
func TestEveryFieldOfAUsageTargetReachesTheSampler(t *testing.T) {
	typ := reflect.TypeFor[provider.UsageTarget]()

	var bools []int
	for i := range typ.NumField() {
		if typ.Field(i).Type.Kind() == reflect.Bool {
			bools = append(bools, i)
		}
	}
	if len(bools) == 0 {
		bools = []int{-1}
	}

	for _, only := range bools {
		var want provider.UsageTarget
		from := reflect.ValueOf(&want).Elem()
		for i := range from.NumField() {
			switch f := from.Field(i); f.Kind() {
			case reflect.String:
				f.SetString("value-" + typ.Field(i).Name)
			case reflect.Int:
				f.SetInt(int64(1000 + i))
			case reflect.Uint64:
				f.SetUint(uint64(2000 + i))
			case reflect.Bool:
				f.SetBool(i == only)
			default:
				t.Fatalf("provider.UsageTarget.%s is a %s this test cannot fill", typ.Field(i).Name, f.Kind())
			}
		}

		p := &measuredProvider{fakeProvider: &fakeProvider{kind: config.ProviderDocker}, target: &want}
		a, host := newAllocatorWithHost(t)
		monitor := &startedMonitor{}
		r := New(a, host, &fakeJIT{setID: 7}, p, nil, WithMonitor(monitor))

		lease := assignedLease(t, a)
		if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: lease.RequestID, Event: "push"}); err != nil {
			t.Fatalf("Launch: %v", err)
		}
		if len(monitor.targets) != 1 {
			t.Fatalf("the monitor was started %d times, want once", len(monitor.targets))
		}

		got := reflect.ValueOf(monitor.targets[0])
		if got.NumField() != from.NumField() {
			t.Errorf("usage.Target has %d fields and provider.UsageTarget %d", got.NumField(), from.NumField())
		}
		for i := range from.NumField() {
			name := typ.Field(i).Name
			field := got.FieldByName(name)
			if !field.IsValid() {
				t.Errorf("usage.Target has no %s", name)

				continue
			}
			if !reflect.DeepEqual(field.Interface(), from.Field(i).Interface()) {
				t.Errorf("%s reached the sampler as %v, want %v", name, field.Interface(), from.Field(i).Interface())
			}
		}
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
		WithMonitor(runningMonitor(t, root)))

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

// runningMonitor is a sampler serving requests, as cmd/billet runs one, stopped
// when the test ends.
func runningMonitor(t *testing.T, root string) *usage.Monitor {
	t.Helper()

	m := usage.NewMonitor(root, usage.Options{Interval: time.Hour})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	return m
}
