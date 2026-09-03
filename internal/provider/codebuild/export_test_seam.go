package codebuild

import (
	"context"
	"time"
)

// SetSSMEndpointForTest points a provider's Parameter Store client at a stand-in.
//
// A NAMED SEAM RATHER THAN AN OPTION, and the distinction is the security property it
// protects. The Parameter Store endpoint is DERIVED from the region and deliberately
// NOT configurable: the single-use runner registration is written there, so an
// operator override would be a way to send a credential to a host of their choosing.
// `node.codebuild.endpoint` exists for a VPC interface endpoint or a non-commercial
// partition and covers the CodeBuild API alone.
//
// A cross-package test still has to reach it — internal/e2e drives the real node
// runtime against a fake AWS — so this exists rather than widening the config. Its
// name says what it is for, it is documented as a test seam, and
// TestTheParameterStoreEndpointIsNeverTheConfiguredOne asserts the production
// derivation independently of it, so a mistake here cannot make that assertion
// vacuous.
//
// It is NOT in a _test.go file because internal/e2e is a different package and Go
// does not export test helpers across one. That is the cost of the seam being
// honest about itself; the alternative was a config field with no legitimate use.
func SetSSMEndpointForTest(p *Provider, endpoint string) {
	p.api.ssm = endpoint
}

// SetSleepForTest replaces the pacing wait between teardown polls.
//
// THE SAME KIND OF SEAM AS THE ONE ABOVE, for the same cross-package reason:
// internal/e2e proves what a node does with a teardown the backend never confirms,
// and confirmStopped paces its polls at teardownPollInterval — twenty seconds of
// wall clock per unconfirmed stop, proving nothing about waiting. A replacement
// must still honour the context, because that loop is bounded by cancellation as
// well as by a count and one that ignored it would spin.
func SetSleepForTest(p *Provider, sleep func(ctx context.Context, d time.Duration) error) {
	p.sleep = sleep
}

// SetSweeperSSMEndpointForTest is the same seam for the control plane's sweeper,
// which reaches Parameter Store through a client of its own.
func SetSweeperSSMEndpointForTest(s *RegistrationSweeper, endpoint string) {
	s.api.ssm = endpoint
}
