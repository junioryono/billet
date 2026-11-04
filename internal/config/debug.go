package config

import (
	"fmt"
	"strings"
	"time"
)

// TierDebug is tiers[].debug: getting into a job's VM. Off unless a tier says, and refused
// on an untrusted tier whatever it says (#28): a person attaching to a machine an outside
// contributor's job controls would be typing into it.
type TierDebug struct {
	// Attach lets an operator open a terminal in this tier's jobs' VMs, through the node's
	// own outbound connection (`billet jobs attach`).
	Attach bool `yaml:"attach,omitempty"`
	// HoldOnFailure keeps a failed job's VM for this long or until a session's shell has
	// exited and the last session has closed, its capacity charged. A Go duration string, at most MaxHoldOnFailure; absent,
	// a failed job's VM is destroyed as any other's. Needs Attach.
	HoldOnFailure string `yaml:"hold_on_failure,omitempty"`
}

// MaxHoldOnFailure bounds a hold. A hold delays capacity coming back, so it is an operator's
// few minutes and never a way to park a machine.
const MaxHoldOnFailure = time.Hour

// HoldDuration is how long a failed job's VM is held, zero for no hold. Only what validation
// passed reaches a reader, so a value it cannot read is no hold rather than an error.
func (t Tier) HoldDuration() time.Duration {
	if t.Debug == nil {
		return 0
	}

	d, err := time.ParseDuration(strings.TrimSpace(t.Debug.HoldOnFailure))
	if err != nil {
		return 0
	}

	return d
}

// Attachable says an operator may open a terminal in this tier's jobs.
func (t Tier) Attachable() bool { return t.Debug != nil && t.Debug.Attach }

// validateDebug refuses what would let a person into a machine they should not reach, or
// what would quietly do nothing.
func (t Tier) validateDebug(where string) []error {
	if t.Debug == nil {
		return nil
	}

	var errs []error

	if t.Debug.Attach && t.Trust.Effective() != WorkloadTrusted {
		errs = append(errs, fmt.Errorf("%s: debug.attach needs a trusted tier; an untrusted tier's jobs are "+
			"outside work on a machine they control, and a person attaching would be typing into it", where))
	}

	if t.Debug.Attach && !t.servesGuestCaches() {
		errs = append(errs, fmt.Errorf("%s: debug.attach needs a Linux Firecracker tier and no other backend, whose "+
			"guest serves the terminal over vsock", where))
	}

	if raw := strings.TrimSpace(t.Debug.HoldOnFailure); raw != "" {
		d, err := time.ParseDuration(raw)

		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: debug.hold_on_failure: %q is not a duration such as \"5m\": %w", where,
				raw, err))
		case d <= 0 || d > MaxHoldOnFailure:
			errs = append(errs, fmt.Errorf("%s: debug.hold_on_failure: %s is not above zero and at most %s, which is "+
				"how long a failed job's capacity can be kept from other jobs", where, d, MaxHoldOnFailure))
		case !t.Debug.Attach:
			errs = append(errs, fmt.Errorf("%s: debug.hold_on_failure needs debug.attach: a VM held with no way into "+
				"it costs capacity and shows nobody anything", where))
		}
	}

	return errs
}
