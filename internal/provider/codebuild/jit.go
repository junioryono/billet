package codebuild

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// jitEnvVar is what the GitHub runner reads its single-use registration from.
//
// THE PARAMETER IS HANDED OVER UNDER THIS NAME DIRECTLY, which is the whole reason
// the channel is safe: CodeBuild resolves a PARAMETER_STORE variable into the build
// environment itself, so the value never appears in argv, never appears in a
// buildspec command, and no step billet generates ever touches it. The buildspec
// does not mention this name at all — see buildspec.go.
const jitEnvVar = "ACTIONS_RUNNER_INPUT_JITCONFIG"

// ownerEnvVar and nameEnvVar carry the ownership markers a build cannot carry as
// tags.
//
// A CODEBUILD BUILD IS NOT TAGGABLE — tags exist on projects and report groups, and
// StartBuild has no field that becomes one — so the per-instance `sh.billet.owner`
// tag the ec2 backend filters List on does not exist here. These replace it, and
// they are read back through BatchGetBuilds' environment.
//
// PLAINTEXT ON PURPOSE. Neither a deployment identity nor a lease name is a secret,
// and both have to be readable by billet without decrypting anything — the point of
// the SecureString beside them is that the REGISTRATION is not readable that way.
const (
	ownerEnvVar = "BILLET_OWNER"
	nameEnvVar  = "BILLET_INSTANCE_NAME"
)

// jitParameterName is where one build's registration is written.
//
// NAMED FOR THE LEASE, so it is unique by construction and so an operator finding a
// stray parameter can tell which lease it belonged to. The lease id is already the
// only durable link between running compute and the lease that authorised it; this
// keeps that true for the credential too.
func (p *Provider) jitParameterName(instanceName string) string {
	return strings.TrimSuffix(p.cfg.JITParameterPath, "/") + "/" + instanceName
}

// errNoParameterName refuses to address a parameter for a lease billet cannot name.
//
// A DELETION MUST BE AUTHORISED BY A NAME BILLET WROTE, not by one it derived from an
// empty string. With no guard, an empty instance name yields the PATH PREFIX itself
// with a trailing slash — a perfectly valid parameter name, and one billet never
// staged anything under. The teardown path reads that name out of a RESPONSE (the
// build's own environment markers), so an absent marker is exactly how an empty one
// arrives, and a build billet did not create is exactly the case where it is absent.
//
// It is the same rule the App-key paths follow: nothing is deleted by a pathname
// unless it is known what that pathname holds.
var errNoParameterName = errors.New("codebuild: refusing to address a staged runner " +
	"registration for a build billet cannot name: the parameter name is derived from the " +
	"lease, and an empty lease would address the path prefix itself")

// putJITConfig writes one build's registration as a SecureString.
//
// NO OVERWRITE, and that is a refusal rather than a convenience. A parameter already
// standing at this name means either a launch for this lease is in flight — in which
// case replacing its registration strands whatever consumed the first one — or a
// previous attempt's credential was never cleaned up, which an operator needs to
// know about rather than have silently replaced. Both are answered by failing the
// launch, because the lease is released and the job is reassigned; overwriting
// answers the first one by breaking it.
func (p *Provider) putJITConfig(ctx context.Context, instanceName, jitConfig string) error {
	if instanceName == "" {
		return errNoParameterName
	}

	in := map[string]any{
		"Name":  p.jitParameterName(instanceName),
		"Value": jitConfig,
		"Type":  "SecureString",
		// EXPLICITLY FALSE. PutParameter defaults to no-overwrite, and stating it
		// keeps the request's meaning from depending on a default AWS could change
		// under a credential this sensitive.
		"Overwrite": false,
		// INTELLIGENT-TIERING, AND WITHOUT IT THIS BACKEND CANNOT RUN A SINGLE JOB.
		//
		// A STANDARD SSM PARAMETER CAPS ITS VALUE AT 4096 CHARACTERS and a GitHub JIT
		// runner configuration is a base64 blob that EXCEEDS that. Measured against
		// real Parameter Store in us-west-2 on 2026-08-31: 4096 accepted, 4097
		// refused with `ValidationException: Standard tier parameters support a
		// maximum parameter value of 4096 characters` — and a real registration
		// minted for a real scale set was refused the same way, so every launch died
		// at staging before StartBuild was ever reached.
		//
		// NOTHING CAUGHT IT because a fake accepts any value: the unit suite, the e2e
		// stand-in and the live provider test all stage a SHORT placeholder rather
		// than a genuine registration, so the one thing that varies between them and
		// production is the exact thing that broke. That is the argument for the
		// end-to-end run in one line.
		//
		// INTELLIGENT-TIERING RATHER THAN ADVANCED, because it is the choice that
		// costs nothing when it does not have to: AWS keeps the parameter standard
		// while the value fits and promotes it to advanced only when it does not.
		// Advanced parameters are billed per parameter-month, prorated — for a
		// credential billet deletes within minutes of the build reading it that is
		// fractions of a cent, but "only when necessary" is still the right default
		// for something every launch creates.
		"Tier": "Intelligent-Tiering",
		// A DESCRIPTION AN OPERATOR CAN ACT ON, because the one time anybody reads
		// it is when they have found a leftover parameter and are deciding whether
		// deleting it is safe.
		"Description": "billet single-use GitHub Actions runner registration; safe to delete once " +
			"its build is no longer running",
	}

	if p.cfg.JITKMSKeyID != "" {
		in["KeyId"] = p.cfg.JITKMSKeyID
	}

	if err := p.api.callSSM(ctx, "PutParameter", in, nil); err != nil {
		// NOTHING FROM THIS CALL IS RENDERED BEYOND ITS CODE. The request body IS
		// the credential, so one shared error format string across the put and the
		// delete is how it reaches a log — the rule the ec2 backend states for its
		// one credential-carrying action.
		code, ok := codeOf(err)
		if ok && code == "ParameterAlreadyExists" {
			return fmt.Errorf("codebuild: a runner registration already stands at the parameter "+
				"for %s; either a launch for that lease is still in flight, or an earlier one "+
				"left its credential behind. billet will not replace it: refusing releases the "+
				"lease so the job is reassigned, where overwriting would strand whatever "+
				"consumed the first one. Delete the parameter by hand once no build for that "+
				"lease is running", instanceName)
		}

		if ok {
			return fmt.Errorf("codebuild: could not stage the runner registration for %s: %s",
				instanceName, code)
		}

		return fmt.Errorf("codebuild: could not stage the runner registration for %s", instanceName)
	}

	return nil
}

// deleteJITConfig removes one build's registration.
//
// IDEMPOTENT: a parameter that is already gone is success, because this runs on
// paths that have already failed once and an error there turns a recoverable state
// into a stuck one.
//
// THE RETURN IS ADVISORY EVERYWHERE IT IS CALLED, and that is deliberate. The
// registration is single-use and consumed by the runner, so a parameter that
// outlives its build is litter rather than a live credential — while a launch failed
// for being unable to tidy up is a job that did not run. It is REPORTED rather than
// swallowed, because an unmentioned leftover credential is what nobody finds until
// it matters.
func (p *Provider) deleteJITConfig(ctx context.Context, instanceName string) error {
	// REFUSED RATHER THAN ATTEMPTED. See errNoParameterName: an empty name addresses
	// the path prefix, which billet never staged anything under.
	if instanceName == "" {
		return errNoParameterName
	}

	if err := p.api.deleteParameter(ctx, p.jitParameterName(instanceName)); err != nil {
		return fmt.Errorf("codebuild: could not remove the staged runner registration for %s: %w",
			instanceName, err)
	}

	return nil
}

// deleteParameter removes one parameter by its full name, treating one that is
// already gone as removed.
//
// ONE DELETE FOR TWO CALLERS. The node deletes a registration it staged, by the
// name it derived from the lease; the control plane's sweep deletes one it LISTED,
// by the name the listing returned. Both must read ParameterNotFound as success —
// each runs where the other may already have acted — and both must render the
// failure by its code alone, so the mapping lives once.
//
// THE CODE, NEVER THE MESSAGE OR THE REQUEST. The name is billet's own and safe to
// say; the value is what this whole channel exists to keep out of a log.
func (c *client) deleteParameter(ctx context.Context, name string) error {
	if name == "" {
		return errNoParameterName
	}

	err := c.callSSM(ctx, "DeleteParameter", map[string]any{"Name": name}, nil)
	if err == nil {
		return nil
	}

	if code, ok := codeOf(err); ok && code == "ParameterNotFound" {
		return nil
	}

	if code, ok := codeOf(err); ok {
		return errors.New(code)
	}

	return errors.New("the api could not be reached")
}

// THERE IS NO PHASE INFERENCE HERE ANY MORE, and the absence is deliberate.
//
// A `consumed(build)` predicate used to live at this spot: CodeBuild resolves a
// PARAMETER_STORE variable while preparing the environment, so a build past
// DOWNLOAD_SOURCE had certainly read its registration and one before it had not. It was
// how the old tidy path decided whether deleting was safe.
//
// WHAT REPLACED IT IS A CALLER THAT ALREADY KNOWS. Every inference of that shape has to
// answer "has the build read it yet" from an inventory that is eventually consistent,
// and its wrong answers are asymmetric: keeping a parameter too long is litter, while
// deleting one too early is a runner that never registers and a job that queues until
// GitHub gives up. `ReapStagedCredential` is called only from custody settlement, which
// runs after something has PROVED the compute is gone — so there is nothing left to
// infer, and a predicate that can be wrong was removed rather than kept beside a caller
// that does not need it.
//
// THE ONE THAT ESCAPES ALL THREE — a node that dies between staging and settling — is
// removed by the CONTROL PLANE, on the ledger's authority rather than the provider's:
// see sweep.go, and the ClosureLookup it takes instead of asking CodeBuild anything.

// errNoJITPath is what a provider built without a parameter path answers, rather
// than writing a registration to a name it invented.
var errNoJITPath = errors.New("codebuild: no jit_parameter_path is configured, so there is " +
	"nowhere to stage a runner registration that keeps it out of the launch request")
