package main

import (
	"context"
	"errors"

	"github.com/junioryono/billet/internal/hostauthority"
	"github.com/junioryono/billet/internal/wirecert"
)

// retireMutationEvent observes command step boundaries and waits. Admission
// events originate in the full admission functions, independently of the steps.
var retireMutationEvent func(event, path string)

func noteRetireMutation(event, path string) {
	if retireMutationEvent != nil {
		retireMutationEvent(event, path)
	}
}

func retirePersistenceError(reason, doing string, err error) *retireRefusal {
	return retireUnknown(reason, doing+err.Error(), "")
}

// retireStepError carries a command refusal through a ledger-opener callback.
type retireStepError struct{ refusal *retireRefusal }

func (e *retireStepError) Error() string { return e.refusal.Why }

// Retirement composes the existing exclusions so each lock wait ends a step.
// No shared lock or ownership primitive takes a retirement-specific callback.
func openRetireIdentity(ctx context.Context, dir string, retiring bool, admit func() *retireRefusal) (*hostauthority.Access, *retireRefusal) {
	if r := admit(); r != nil {
		return nil, r
	}
	noteRetireMutation("global-lock", dir)
	resolve := wirecert.ResolveExclusion
	if retiring {
		resolve = wirecert.ResolveRetiringExclusion
	}
	ex, err := resolve(ctx, dir, hostauthority.Wait)
	noteRetireMutation("wait", "global lock")
	if err != nil {
		return nil, retireUnknown(retireReasonIdentity, err.Error(), "")
	}
	if r := admit(); r != nil {
		if err := ex.Release(); err != nil {
			r.Why += "; release global exclusion: " + err.Error()
		}
		return nil, r
	}
	noteRetireMutation("identity-lock", dir)
	acc, err := hostauthority.Adopt(ctx, dir, ex)
	noteRetireMutation("wait", "identity lock")
	if err != nil {
		return nil, retireUnknown(retireReasonIdentity, errors.Join(err, ex.Release()).Error(), "")
	}
	return acc, nil
}

// A refused hand-back still releases both exclusions, without ownership repair.
// These accesses never initialise an identity or acquire a lifecycle hold.
func releaseRetireIdentity(acc *hostauthority.Access, admit func() *retireRefusal) *retireRefusal {
	var r *retireRefusal
	if !acc.WasMoved() {
		r = admit()
		if r == nil {
			noteRetireMutation("identity-handback", acc.Dir())
			if err := acc.HandBack(); err != nil {
				r = retireUnknown(retireReasonIdentity, err.Error(), "")
			}
		}
	}
	if err := acc.ReleaseWithoutHandBack(); err != nil {
		if r == nil {
			r = retireUnknown(retireReasonIdentity, err.Error(), "")
		} else {
			r.Why += "; release identity exclusion: " + err.Error()
		}
	}
	return r
}
