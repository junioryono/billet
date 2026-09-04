package wiring

import (
	"github.com/junioryono/godi/v5"

	"github.com/junioryono/billet/internal/awscreds"
)

// AWSModule registers billet's one AWS credential chain.
//
// ONE CHAIN, RESOLVED ONCE PER PROCESS. The chain (environment variables, then
// this instance's IAM role over IMDSv2, with v1 refused rather than used as a
// fallback) carries a redaction table that took several rounds to get right,
// and every consumer takes it as awscreds.Source rather than building a second.
// awscreds.Default() constructs the chain and touches nothing (measured), so
// every role set may include this whether or not the deployment is on AWS.
func AWSModule() godi.ModuleOption {
	return godi.NewModule("aws",
		godi.AddSingleton(awscreds.Default),
	)
}
