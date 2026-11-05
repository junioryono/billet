// Package awsmetal is the host source for AWS bare-metal instances (ADR-026), through
// the ec2 provider's pool-host surface, so there is one EC2 client.
//
// WHAT IS MEASURED. On 2026-11-05 four metal types reached their first script 21 to
// 47 s after launch, and a launch past the account's vCPU quota was refused
// synchronously with `VcpuLimitExceeded` and started nothing (the bare-metal hosts
// record). A capacity refusal is the codes the ec2 provider already treats as one. A
// terminated instance's record in a targeted DescribeInstances is the ec2 provider's
// causal proof that an instance ended; not-found is not.
package awsmetal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostsource"
	"github.com/junioryono/billet/internal/provider/ec2"
)

// Name is this source's kind.
const Name = "aws-metal"

// clientTokenWindow is how long after its first attempt a pool order may be sent again
// under its ClientToken. AWS keeps a token's record for a bounded time it does not
// state; the ec2 provider measured one still refusing a changed relaunch long after the
// fact, and ten minutes is billet's bound, far inside that. Past it, a lost answer waits
// for a person.
const clientTokenWindow = 10 * time.Minute

// hosts is the part of the ec2 provider's pool-host surface this source uses.
type hosts interface {
	Launch(ctx context.Context, l ec2.HostLaunch) (ec2.HostRecord, error)
	Find(ctx context.Context, order string) (ec2.HostRecord, bool, error)
	Describe(ctx context.Context, id string) (ec2.HostRecord, error)
	Terminate(ctx context.Context, id string) error
}

// Shape is one metal type a pool may order, with what it is.
type Shape struct {
	Type  string
	Arch  config.Arch
	Image string
}

// Source orders and releases AWS bare-metal instances in one subnet.
type Source struct {
	hosts    hosts
	location string
	shapes   map[string]Shape
	// code reads EC2's error code out of an error chain; ec2.Code outside a test.
	code func(error) (string, bool)
}

// New builds a source over a provider's pool-host surface. location is the subnet's
// availability zone, the one place this source orders in.
func New(h *ec2.Hosts, location string, shapes []Shape) (*Source, error) {
	return newSource(h, location, shapes)
}

func newSource(h hosts, location string, shapes []Shape) (*Source, error) {
	if location == "" {
		return nil, errors.New("awsmetal: a source needs the availability zone its subnet is in")
	}
	byType := make(map[string]Shape, len(shapes))
	for _, s := range shapes {
		if s.Type == "" || s.Image == "" || s.Arch == "" {
			return nil, fmt.Errorf("awsmetal: shape %+v needs a type, an architecture and an image", s)
		}
		if _, dup := byType[s.Type]; dup {
			return nil, fmt.Errorf("awsmetal: shape %s is declared twice", s.Type)
		}
		byType[s.Type] = s
	}
	if len(byType) == 0 {
		return nil, errors.New("awsmetal: a source needs at least one shape")
	}

	return &Source{hosts: h, location: location, shapes: byType, code: ec2.Code}, nil
}

var _ hostsource.Source = (*Source)(nil)

// Name is the source's kind.
func (s *Source) Name() string { return Name }

// Terms are on-demand EC2's: billed by the second, and a launch measured reaching its
// first script within a minute. The lead time to a registered node is not measured.
func (s *Source) Terms(shape string) (hostsource.Terms, error) {
	if _, ok := s.shapes[shape]; !ok {
		return hostsource.Terms{}, fmt.Errorf("awsmetal: %s is not a declared shape", shape)
	}

	return hostsource.Terms{LeadTime: time.Minute, BillingUnit: time.Second, IdempotentFor: clientTokenWindow}, nil
}

// Order launches one host of a declared shape in this source's zone.
func (s *Source) Order(ctx context.Context, req hostsource.Request) (hostsource.Host, error) {
	shape, ok := s.shapes[req.Shape]
	switch {
	case !ok:
		return hostsource.Host{}, fmt.Errorf("awsmetal: %s is not a declared shape", req.Shape)
	case req.Arch != "" && req.Arch != shape.Arch:
		return hostsource.Host{}, fmt.Errorf("awsmetal: %s is %s, not %s", req.Shape, shape.Arch, req.Arch)
	case req.Location != "" && req.Location != s.location:
		return hostsource.Host{}, fmt.Errorf("awsmetal: this source orders in %s, not %s", s.location, req.Location)
	}
	rec, err := s.hosts.Launch(ctx, ec2.HostLaunch{
		Order:        req.Order,
		InstanceType: shape.Type,
		Image:        shape.Image,
		UserData:     req.Boot.Reveal(),
	})
	if err != nil {
		return hostsource.Host{}, s.refusal(err)
	}

	return s.host(rec), nil
}

// Find reads back the host an order launched.
func (s *Source) Find(ctx context.Context, order string) (hostsource.Host, bool, error) {
	rec, ok, err := s.hosts.Find(ctx, order)
	if err != nil || !ok {
		return hostsource.Host{}, ok, err
	}

	return s.host(rec), true, nil
}

// Describe reads one host.
func (s *Source) Describe(ctx context.Context, id string) (hostsource.Host, error) {
	rec, err := s.hosts.Describe(ctx, id)
	if err != nil {
		return hostsource.Host{}, err
	}

	return s.host(rec), nil
}

// Gone is gone only on EC2's own `terminated` record for that instance. Not found is
// could-not-tell: DescribeInstances is eventually consistent.
func (s *Source) Gone(ctx context.Context, id string) (hostsource.Presence, error) {
	rec, err := s.hosts.Describe(ctx, id)
	switch {
	case errors.Is(err, ec2.ErrHostNotFound):
		return hostsource.PresenceUnknown, nil
	case err != nil:
		return hostsource.PresenceUnknown, err
	case rec.State == "terminated":
		return hostsource.PresenceGone, nil
	case rec.State == "":
		return hostsource.PresenceUnknown, nil
	}

	return hostsource.PresencePresent, nil
}

// Release terminates the instance the proof names.
func (s *Source) Release(ctx context.Context, proof hostsource.ReleaseProof) error {
	if err := proof.For(proof.Host()); err != nil {
		return err
	}

	return s.hosts.Terminate(ctx, proof.Host())
}

func (s *Source) host(rec ec2.HostRecord) hostsource.Host {
	return hostsource.Host{
		ID:       rec.ID,
		Order:    rec.Order,
		Shape:    rec.InstanceType,
		Location: s.location,
		State:    stateOf(rec.State),
	}
}

// stateOf maps EC2's instance states; anything else is could-not-tell.
func stateOf(state string) hostsource.State {
	switch state {
	case "pending":
		return hostsource.StateProvisioning
	case "running":
		return hostsource.StateRunning
	case "stopping", "stopped":
		// billet never stops a pool host, so one that stopped will not serve again.
		return hostsource.StateFailed
	case "shutting-down", "terminated":
		return hostsource.StateReleasing
	}

	return hostsource.StateUnknown
}

// refusal turns EC2's synchronous refusals into the contract's answers.
func (s *Source) refusal(err error) error {
	code, ok := s.code(err)
	if !ok {
		return err
	}
	switch {
	case ec2.CapacityRefusal(code):
		return fmt.Errorf("%w: %w", hostsource.ErrNoStock, err)
	case code == "VcpuLimitExceeded":
		// MEASURED 2026-11-05: a launch past the running-instance vCPU quota.
		return fmt.Errorf("%w: %w", hostsource.ErrQuota, err)
	case code == "UnauthorizedOperation":
		return fmt.Errorf("%w: %w", hostsource.ErrRefused, err)
	}

	return err
}
