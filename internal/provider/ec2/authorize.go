package ec2

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// cleanupTimeout bounds the emergency teardown of an instance a mis-honored DryRun
// accidentally started. It runs on a context DETACHED from the diagnostic's, so a
// Ctrl-C cannot cut short the containment; the bound stops it hanging forever.
const cleanupTimeout = 30 * time.Second

// DryRunOutcome classifies an EC2 DryRun: whether the request would have been
// authorized, refused for permission, or refused for another reason.
type DryRunOutcome int

const (
	// DryRunInconclusive is any code that is not a permission verdict — a bad AMI, a
	// shape not offered in the zone, an invalid parameter. It is the ZERO VALUE on
	// purpose: in a classifier the dangerous mistake is a false "authorized", so an
	// unset or unexpected result must read as "proved nothing", never as a pass.
	DryRunInconclusive DryRunOutcome = iota
	// DryRunAuthorized is DryRunOperation: the request is well-formed and this
	// identity may make it — the launch would proceed.
	DryRunAuthorized
	// DryRunUnauthorized is UnauthorizedOperation: the role lacks the permission.
	// This is the gap the describe-only preflight cannot see.
	DryRunUnauthorized
)

// DryRunResult is one dry-run's classification and the AWS code behind it.
type DryRunResult struct {
	Outcome DryRunOutcome
	Code    string
}

// classifyDryRun reads a DryRun API error into a verdict. It is only ever handed an
// error that carries an AWS code: a nil error (a real launch) and a transport error
// are handled by DryRunLaunch before this, because neither is a permission verdict.
func classifyDryRun(err error) DryRunResult {
	code, ok := codeOf(err)
	if !ok {
		return DryRunResult{Outcome: DryRunInconclusive}
	}

	switch code {
	case "DryRunOperation":
		return DryRunResult{Outcome: DryRunAuthorized, Code: code}
	case "UnauthorizedOperation":
		return DryRunResult{Outcome: DryRunUnauthorized, Code: code}
	default:
		return DryRunResult{Outcome: DryRunInconclusive, Code: code}
	}
}

// authorizeName is the probe instance's Name tag, unique per (image, trust, disk).
// The ClientToken is derived from (name, shape), so a unique name per combination
// keeps two genuinely different launch requests — a trusted and an untrusted probe
// on one shape, say — from sharing a token and answering IdempotentParameterMismatch.
// It keeps the billet- prefix so a mis-launched instance still parses as an orphan.
func authorizeName(image string, trust provider.TrustClass, disk config.ByteSize) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d", image, int(trust), int64(disk))))

	// 16 hex (64 bits) is ample against collision AND stays inside the ClientToken
	// ceiling with margin: the name is 16+11+16 = 43 chars, and clientTokenFor adds
	// "-"+12hex = 13, for a 56-char token against AWS's 64-char limit. Do NOT widen
	// the digest or the prefix past a 51-char name, or the token overflows and a
	// probe answers InvalidParameterValue — an inconclusive, not the loud failure.
	return preflightOwner + "-authorize-" + hex.EncodeToString(h[:])[:16]
}

// DryRunLaunch asks EC2 whether the RunInstances the launch WOULD make — same
// image, shape, network for the trust class, instance profile, spot, disk and
// OWNER TAG — is authorized, without launching anything.
//
// DryRun IS SAFE FROM A DIAGNOSTIC, which is why `billet check` gates this behind
// --authorize rather than running it by default: DryRun=true has no side effect —
// AWS validates the request and checks IAM, then refuses with DryRunOperation
// (would have worked) or UnauthorizedOperation (may not) and starts nothing. It is
// AWS's own authorization test, which is exactly the permission the read-only
// describes cannot confirm.
//
// THE OWNER TAG MUST BE THE DEPLOYMENT'S, or a per-deployment IAM policy — which
// conditions ec2:CreateTags on the exact sh.billet.owner value — refuses the
// launch's TagSpecification and the whole RunInstances fails as UnauthorizedOperation.
// The real launch tags with the deployment id; so must this, or the diagnostic asks
// a different question than the launch. The caller constructs the provider with the
// deployment owner for exactly this reason.
func (p *Provider) DryRunLaunch(
	ctx context.Context, image string, trust provider.TrustClass, instanceType config.EC2InstanceType,
	disk config.ByteSize,
) (DryRunResult, error) {
	spec := provider.Spec{
		Name:         authorizeName(image, trust, disk),
		Image:        image,
		Trust:        trust,
		InstanceType: instanceType.Type,
		Disk:         disk,
		// A placeholder registration: DryRun never boots the instance, so no real
		// JIT config is needed, only a well-formed request.
		JITConfig: "billet-authorize-dry-run",
	}

	params, err := p.runInstancesParams(ctx, spec, instanceType)
	if err != nil {
		return DryRunResult{}, err
	}

	params.Set("DryRun", "true")

	var out runInstancesResponse
	err = p.api.call(ctx, params, &out)
	switch err {
	case nil:
		// A SUCCESS IS THE DANGEROUS CASE, not the happy one: with DryRun=true AWS
		// never returns 200, so a 200 means an intermediary stripped the parameter
		// and a REAL instance launched. A diagnostic must not leave that running.
		return DryRunResult{}, p.containMishonoredLaunch(ctx, spec.Name, out)

	default:
		if _, ok := codeOf(err); !ok {
			// NO AWS CODE means AWS never answered — a transport failure or a
			// cancellation, not a permission verdict. Worse, the request may have
			// COMMITTED (a proxy that ignores DryRun and then drops the response),
			// so name the tags an operator can search for rather than passing it off
			// as an inconclusive code.
			return DryRunResult{}, fmt.Errorf("ec2: dry-run launch could not reach an "+
				"authorization verdict: %w; if this endpoint committed a real launch it is tagged "+
				"%s=%s, %s=%s — search for and terminate any such instance", err,
				ownerTag, p.owner, nameTag, spec.Name)
		}

		return classifyDryRun(err), nil
	}
}

// containMishonoredLaunch tears down the instance a DryRun that returned 200
// accidentally started, and returns an error that never claims a teardown it did
// not achieve. It runs on a DETACHED, bounded context so a cancelled diagnostic
// cannot abandon a running instance.
func (p *Provider) containMishonoredLaunch(
	ctx context.Context, name string, out runInstancesResponse,
) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	ids := make([]string, 0, len(out.Instances))
	for _, inst := range out.Instances {
		if inst.InstanceID != "" {
			ids = append(ids, inst.InstanceID)
		}
	}

	// A 200 that named no instance still means a launch happened — recover the id
	// by the Name tag before giving up, because "no id in the body" is NOT "nothing
	// to terminate" on this branch. DescribeInstances is EVENTUALLY CONSISTENT: a
	// just-launched instance may be absent from the first answer, so retry until the
	// cleanup deadline rather than give up on one empty read.
	if len(ids) == 0 {
		ids = p.recoverMishonoredLaunch(cctx, name)
	}

	if len(ids) == 0 {
		return fmt.Errorf("ec2: a DryRun launch unexpectedly SUCCEEDED (the DryRun parameter was "+
			"not honored, likely a proxy or a non-AWS endpoint) and billet could not identify the "+
			"instance it started; an instance tagged %s=%s, %s=%s may be RUNNING and billable — "+
			"find and terminate it by hand, and do NOT trust --authorize through this endpoint",
			ownerTag, p.owner, nameTag, name)
	}

	var undead []string
	for _, id := range ids {
		if _, err := p.Destroy(cctx, id); err != nil {
			undead = append(undead, id)
		}
	}

	if len(undead) > 0 {
		// An orphan that could not be killed matters MORE, not less: name it so the
		// operator can terminate it by hand.
		return fmt.Errorf("ec2: a DryRun launch unexpectedly SUCCEEDED (DryRun not honored) and "+
			"terminating it FAILED; instance(s) %v are STILL RUNNING and billable — terminate them "+
			"by hand, and do NOT trust --authorize through this endpoint", undead)
	}

	// REQUESTED, not confirmed: TerminateInstances is asynchronous, so this reports
	// what billet asked for, not proof the instance is gone — the operator verifies
	// with `billet status`/the console.
	return fmt.Errorf("ec2: a DryRun launch unexpectedly SUCCEEDED — the DryRun parameter was not "+
		"honored (likely a proxy or a non-AWS endpoint), so a real instance started; billet "+
		"requested termination of %v, but do NOT trust --authorize through this endpoint and "+
		"verify the instance is gone", ids)
}

// launchRecoveryGap paces the retries that look for a mis-honored launch's
// instance while DescribeInstances becomes consistent. It is a package var so a
// test can drop it to zero; production waits between attempts.
var launchRecoveryGap = 2 * time.Second

// launchRecoveryAttempts bounds those retries independently of the cleanup
// deadline, so the give-up path does not hang for the full timeout.
const launchRecoveryAttempts = 5

// recoveryWait paces one retry, returning false if the (detached) context ends
// first. It is a package var so a test can count how often it is called and prove
// the loop skips the wait after its final attempt.
var recoveryWait = func(ctx context.Context, gap time.Duration) bool {
	timer := time.NewTimer(gap)
	select {
	case <-ctx.Done():
		timer.Stop()

		return false
	case <-timer.C:
		return true
	}
}

// recoverMishonoredLaunch finds the instance a DryRun-stripped 200 started but did
// not name, by its Name tag, retrying against EC2's eventual consistency until it
// finds one, exhausts its attempts, or the (detached) cleanup context ends.
func (p *Provider) recoverMishonoredLaunch(ctx context.Context, name string) []string {
	for attempt := range launchRecoveryAttempts {
		found, err := p.describe(ctx, []string{"pending", "running"},
			filter{name: "tag:" + nameTag, values: []string{name}})
		if err == nil && len(found) > 0 {
			ids := make([]string, 0, len(found))
			for _, inst := range found {
				ids = append(ids, inst.ID)
			}

			return ids
		}

		// No wait after the final attempt — there is no describe left to pace, so
		// the give-up path returns at once rather than sleeping one last gap.
		if attempt == launchRecoveryAttempts-1 {
			break
		}

		if !recoveryWait(ctx, launchRecoveryGap) {
			return nil
		}
	}

	return nil
}
