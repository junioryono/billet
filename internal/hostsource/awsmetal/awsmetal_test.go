package awsmetal

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostsource"
	"github.com/junioryono/billet/internal/provider/ec2"
)

// codedError stands in for the provider's API error; the provider's own tests prove
// its errors keep their code through ec2.Code.
type codedError struct{ code string }

func (e codedError) Error() string { return "ec2: " + e.code }

func codeOfStandIn(err error) (string, bool) {
	if c, ok := errors.AsType[codedError](err); ok {
		return c.code, true
	}

	return "", false
}

type fakeHosts struct {
	mu         sync.Mutex
	launched   []ec2.HostLaunch
	launchErr  error
	record     ec2.HostRecord
	describe   error
	terminated []string
}

func (f *fakeHosts) Launch(_ context.Context, l ec2.HostLaunch) (ec2.HostRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launched = append(f.launched, l)
	if f.launchErr != nil {
		return ec2.HostRecord{}, f.launchErr
	}

	return ec2.HostRecord{ID: "i-1", State: "pending", InstanceType: l.InstanceType, Order: l.Order}, nil
}

func (f *fakeHosts) Find(context.Context, string) (ec2.HostRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.record, f.record.ID != "", nil
}

func (f *fakeHosts) Describe(context.Context, string) (ec2.HostRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.record, f.describe
}

func (f *fakeHosts) Terminate(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated = append(f.terminated, id)

	return nil
}

func newTestSource(t *testing.T, f *fakeHosts) *Source {
	t.Helper()
	s, err := newSource(f, "us-west-2a", []Shape{
		{Type: "m7gd.metal", Arch: config.ArchARM64, Image: "ami-arm"},
		{Type: "z1d.metal", Arch: config.ArchX64, Image: "ami-x64"},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	s.code = codeOfStandIn

	return s
}

func TestAnOrderLaunchesItsShapesImageWithTheBootPayload(t *testing.T) {
	t.Parallel()
	f := &fakeHosts{}
	host, err := newTestSource(t, f).Order(t.Context(), hostsource.Request{
		Order: "o-1", Shape: "z1d.metal", Arch: config.ArchX64, Boot: hostsource.NewSecret([]byte("join")),
	})
	if err != nil {
		t.Fatalf("order: %v", err)
	}
	if host.ID != "i-1" || host.Order != "o-1" || host.State != hostsource.StateProvisioning || host.Location != "us-west-2a" {
		t.Fatalf("host = %+v", host)
	}
	if len(f.launched) != 1 || f.launched[0].Image != "ami-x64" || f.launched[0].InstanceType != "z1d.metal" ||
		string(f.launched[0].UserData) != "join" || f.launched[0].Order != "o-1" {
		t.Fatalf("launched %+v", f.launched)
	}
}

// AN ORDER THIS SOURCE CANNOT HONOUR AS ASKED is refused before anything is launched.
func TestAnOrderOutsideTheDeclaredShapesIsRefused(t *testing.T) {
	t.Parallel()
	for name, req := range map[string]hostsource.Request{
		"undeclared shape":   {Order: "o-1", Shape: "c7i.metal-24xl"},
		"wrong architecture": {Order: "o-1", Shape: "m7gd.metal", Arch: config.ArchX64},
		"another zone":       {Order: "o-1", Shape: "m7gd.metal", Location: "us-west-2b"},
	} {
		f := &fakeHosts{}
		if _, err := newTestSource(t, f).Order(t.Context(), req); err == nil {
			t.Errorf("%s: ordered", name)
		}
		if len(f.launched) != 0 {
			t.Errorf("%s: launched %d", name, len(f.launched))
		}
	}
}

// EC2'S SYNCHRONOUS REFUSALS ARE THE CONTRACT'S ANSWERS; anything else stays an error,
// which the pool reads as could-not-tell.
func TestEC2RefusalsAreTheContractsAnswers(t *testing.T) {
	t.Parallel()
	for code, want := range map[string]error{
		"InsufficientInstanceCapacity": hostsource.ErrNoStock,
		"VcpuLimitExceeded":            hostsource.ErrQuota,
		"UnauthorizedOperation":        hostsource.ErrRefused,
	} {
		f := &fakeHosts{launchErr: codedError{code}}
		_, err := newTestSource(t, f).Order(t.Context(), hostsource.Request{Order: "o-1", Shape: "m7gd.metal"})
		if !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", code, err, want)
		}
	}
	f := &fakeHosts{launchErr: codedError{"InternalError"}}
	_, err := newTestSource(t, f).Order(t.Context(), hostsource.Request{Order: "o-1", Shape: "m7gd.metal"})
	for _, answer := range []error{hostsource.ErrNoStock, hostsource.ErrQuota, hostsource.ErrRefused} {
		if errors.Is(err, answer) {
			t.Errorf("an ambiguous failure read as %v", answer)
		}
	}
	f = &fakeHosts{launchErr: fmt.Errorf("connection reset")}
	if _, err := newTestSource(t, f).Order(t.Context(), hostsource.Request{Order: "o-1", Shape: "m7gd.metal"}); err == nil ||
		errors.Is(err, hostsource.ErrNoStock) || errors.Is(err, hostsource.ErrQuota) {
		t.Errorf("a failed call read as an answer: %v", err)
	}
}

// GONE ONLY ON EC2'S TERMINATED RECORD. Not found and a failed read are could-not-tell.
func TestGoneOnlyOnATerminatedRecord(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		record ec2.HostRecord
		err    error
		want   hostsource.Presence
	}{
		"terminated":    {ec2.HostRecord{ID: "i-1", State: "terminated"}, nil, hostsource.PresenceGone},
		"shutting down": {ec2.HostRecord{ID: "i-1", State: "shutting-down"}, nil, hostsource.PresencePresent},
		"running":       {ec2.HostRecord{ID: "i-1", State: "running"}, nil, hostsource.PresencePresent},
		"no state":      {ec2.HostRecord{ID: "i-1"}, nil, hostsource.PresenceUnknown},
		"not found":     {ec2.HostRecord{}, fmt.Errorf("%w: i-1", ec2.ErrHostNotFound), hostsource.PresenceUnknown},
		"failed read":   {ec2.HostRecord{}, errors.New("timeout"), hostsource.PresenceUnknown},
	} {
		f := &fakeHosts{record: c.record, describe: c.err}
		got, err := newTestSource(t, f).Gone(t.Context(), "i-1")
		if got != c.want {
			t.Errorf("%s: gone = %v, want %v", name, got, c.want)
		}
		// Not found is an answer (could-not-tell); a failed read is an error too.
		if wantErr := name == "failed read"; (err != nil) != wantErr {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestReleaseTerminatesOnlyOnProof(t *testing.T) {
	t.Parallel()
	f := &fakeHosts{}
	s := newTestSource(t, f)
	if err := s.Release(t.Context(), hostsource.ReleaseProof{}); !errors.Is(err, hostsource.ErrNoProof) {
		t.Fatalf("release without proof = %v", err)
	}
	if len(f.terminated) != 0 {
		t.Fatal("terminated without proof")
	}
	proof, err := hostsource.NewDrainedProof(hostsource.Holdings{Host: "i-1", Sealed: time.Unix(1, 0)})
	if err != nil {
		t.Fatalf("proof: %v", err)
	}
	if err := s.Release(t.Context(), proof); err != nil {
		t.Fatalf("release: %v", err)
	}
	if len(f.terminated) != 1 || f.terminated[0] != "i-1" {
		t.Fatalf("terminated %v", f.terminated)
	}
}

func TestAStoppedHostIsFailed(t *testing.T) {
	t.Parallel()
	for state, want := range map[string]hostsource.State{
		"pending": hostsource.StateProvisioning, "running": hostsource.StateRunning,
		"stopping": hostsource.StateFailed, "stopped": hostsource.StateFailed,
		"shutting-down": hostsource.StateReleasing, "terminated": hostsource.StateReleasing,
		"": hostsource.StateUnknown, "mystery": hostsource.StateUnknown,
	} {
		if got := stateOf(state); got != want {
			t.Errorf("%q = %v, want %v", state, got, want)
		}
	}
}
