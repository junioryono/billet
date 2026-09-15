package main

import (
	"context"
	"errors"

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
func openRetireIdentity(ctx context.Context, dir string, retiring bool, admit func() *retireRefusal) (*identityAccess, *retireRefusal) {
	if r := admit(); r != nil {
		return nil, r
	}
	noteRetireMutation("global-lock", dir)
	resolve := wirecert.ResolveExclusion
	if retiring {
		resolve = wirecert.ResolveRetiringExclusion
	}
	ex, err := resolve(ctx, dir, identityAccessWait)
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
	acc := &identityAccess{dir: dir, exclusion: ex, account: ex.Account}
	noteRetireMutation("identity-lock", dir)
	err = acc.lockInner(ctx, ex)
	noteRetireMutation("wait", "identity lock")
	if err != nil {
		return nil, retireUnknown(retireReasonIdentity, errors.Join(err, ex.Release()).Error(), "")
	}
	return acc.registered(), nil
}

// A refused hand-back still releases both exclusions, without ownership repair.
// These accesses never initialise an identity or acquire a lifecycle hold.
func releaseRetireIdentity(acc *identityAccess, admit func() *retireRefusal) *retireRefusal {
	var r *retireRefusal
	if !acc.dirMoved {
		r = admit()
		if r == nil {
			noteRetireMutation("identity-handback", acc.dir)
			if err := acc.handBack(); err != nil {
				r = retireUnknown(retireReasonIdentity, err.Error(), "")
			}
		}
	}
	lock := acc.lock
	acc.lock = nil
	heldAccesses.CompareAndDelete(accessKey(acc.dir), acc)
	if err := errors.Join(lock.Release(), acc.Release()); err != nil {
		if r == nil {
			r = retireUnknown(retireReasonIdentity, err.Error(), "")
		} else {
			r.Why += "; release identity exclusion: " + err.Error()
		}
	}
	return r
}
