package codebuild

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/awsquota"
	"github.com/junioryono/billet/internal/provider"
)

// codeBuildServiceCode is what Service Quotas calls this service.
const codeBuildServiceCode = "codebuild"

// AccountQueueCeiling is how many builds the WHOLE ACCOUNT may have queued.
//
// MEASURED RATHER THAN LOOKED UP, on 2026-09-02 and recorded in
// docs/deploying/aws-codebuild.md: past thirty queued builds StartBuild is refused with
// `AccountLimitExceededException: Cannot have more than 30 builds in queue for
// the account`. Service Quotas does not list it, so it cannot be read — which is
// exactly why it is a constant here with the measurement attached rather than a
// number in a comment somewhere.
//
// It is account-wide, so every CodeBuild project shares it.
const AccountQueueCeiling = 30

// Quotas reports the account ceilings this node's configuration runs against.
//
// THIS IS THE SENTENCE docs/deploying/aws-codebuild.md ALREADY PROMISED AND NOTHING
// SUPPLIED.
// checks.go's own doc comment said an operator "has to see the 36-hour cap, the
// 8-hour queued cap, and the concurrency quota before work is admitted", and
// Ceilings carried the first two and no quota at all — a claim in a comment the
// code did not support. That document's own measurement makes the case sharper
// than the comment did: the default is ONE per compute type, the account-wide
// queue is capped at thirty, and past either the overflow becomes FAILED jobs
// rather than slow ones, because GitHub requeues a job at most three times.
//
// IT LISTS RATHER THAN NAMING CODES. CodeBuild has one concurrency limit per
// environment and compute type and their identifiers are not derivable from
// anything billet knows, so billet asks AWS what they are and matches the names
// AWS returns. Shipping a table of quota codes would be billet inventing
// identifiers for somebody else's API — and a wrong one reads as "no limit",
// which is the direction that costs somebody's build.
//
// A PARTIAL ANSWER AND AN ERROR TOGETHER, which is the QuotaReporter contract:
// one shape billet could not find a limit for must not discard the ones it did.
func (p *Provider) Quotas(ctx context.Context) ([]provider.Quota, error) {
	// THE CREDENTIALS AND THE ENDPOINT COME FROM THE API CLIENT THIS PROVIDER
	// ALREADY HOLDS. Service Quotas is a different service from CodeBuild, so it
	// gets its own signed client — but it must sign as the same principal, and a
	// test that pointed the CodeBuild calls at a fake would otherwise have this
	// one reach the real AWS.
	client := awsquota.New(p.cfg.Region, p.api.quotas, p.api.creds())

	published, listErr := client.List(ctx, codeBuildServiceCode)

	var (
		out  []provider.Quota
		errs []error
	)

	if listErr != nil {
		errs = append(errs, listErr)
	}

	for i := range p.cfg.ComputeTypes {
		shape := p.cfg.ComputeTypes[i].Type

		q, found := concurrencyQuotaFor(published, shape)
		if !found {
			// NAMED RATHER THAN SKIPPED. "billet could not find a limit for this
			// compute type" and "the account has no limit on it" are different
			// facts, and only the second would be safe to say nothing about.
			errs = append(errs, fmt.Errorf("%w: no concurrency limit was found for compute "+
				"type %s; check it in Service Quotas", awsquota.ErrUnavailable, shape))

			continue
		}

		out = append(out, provider.Quota{
			Name:  q.Name,
			Code:  q.Code,
			Limit: q.Value,
			Unit:  q.Unit,
			Scope: "concurrent builds of " + shape,
			Shape: shape,
		})
	}

	// THE ACCOUNT-WIDE QUEUE, REPORTED WITH NO SHAPE. It bounds a BURST rather
	// than a budget — thirty builds across every project in the account — so it
	// carries no Shape and nothing compares it to this node's concurrency, which
	// would be two numbers that are not about the same thing.
	out = append(out, provider.Quota{
		Name:  "Builds queued for the account",
		Limit: AccountQueueCeiling,
		Unit:  "None",
		Scope: "every CodeBuild project in this account, together (measured; " +
			"Service Quotas does not list it)",
	})

	return out, errors.Join(errs...)
}

// concurrencyQuotaFor finds the published limit on running builds of one compute
// type.
//
// MATCHED ON THE COMPUTE TYPE APPEARING IN AWS'S OWN NAME, which is what those
// names carry ("Concurrently running builds for BUILD_GENERAL1_MEDIUM"). Two
// details make that safe rather than hopeful: the match is case-insensitive on
// the TYPE, which is an uppercase identifier that cannot appear by accident in
// prose; and the name must also be about running builds, so a future "Queued
// builds for BUILD_GENERAL1_MEDIUM" is not mistaken for a concurrency limit.
//
// A RENAME MAKES THIS FIND NOTHING, which is reported as a limit billet could
// not find — the safe direction. The alternative, a table of quota codes, fails
// the other way: a stale code reads as "no limit".
func concurrencyQuotaFor(published []awsquota.Quota, shape string) (awsquota.Quota, bool) {
	for i := range published {
		name := strings.ToUpper(published[i].Name)

		if strings.Contains(name, strings.ToUpper(shape)) &&
			strings.Contains(name, "CONCURRENTLY RUNNING BUILDS") {
			return published[i], true
		}
	}

	return awsquota.Quota{}, false
}
