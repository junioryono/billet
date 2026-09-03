package main

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// reportQuotas puts the account's own ceilings in front of an operator before
// work is admitted.
//
// THE CLOUD PREFLIGHT'S LAST GAP, AND THE CODEBUILD BACKEND HANDED IT HERE BY
// NAME: reading Service Quotas is the general cloud preflight's job, and that
// backend consumes it rather than growing a CodeBuild-specific probe beside it.
// So this is one function over provider.QuotaReporter rather than a probe per
// backend, and a third remote backend gets it by implementing the interface.
//
// WHY IT MATTERS MORE THAN IT SOUNDS: CodeBuild's concurrently-running-builds
// quota defaults to ONE per compute type, so a fresh account cannot run two
// concurrent builds of one shape however much capacity billet has escrowed. The
// second queues, CodeBuild FAILS it after the queued ceiling, and GitHub requeues
// the job at most three times — so overflow becomes red builds rather than slow
// ones. An operator meeting that for the first time has no reason to suspect an
// account limit.
//
// ADVISORY, AND IT NEVER GATES. A quota is raised by a support request rather
// than a config change; a read can fail for reasons billet did not cause; and the
// number can move without billet hearing. Refusing a working deployment over a
// stale or unreadable answer is the failure ADR-005 names, after which the next
// thing anybody does is delete the check.
func reportQuotas(ctx context.Context, cfg *config.Config, p provider.Provider) {
	reporter, ok := p.(provider.QuotaReporter)
	if !ok {
		return
	}

	quotas, err := reporter.Quotas(ctx)

	// BOTH, AND IN THIS ORDER. Several lookups make several calls, and one that
	// failed must not discard the ones that answered — so the findings print
	// first and the failures follow, rather than an error replacing a report.
	for i := range quotas {
		printQuota(cfg, quotas[i])
	}

	if err != nil {
		fmt.Printf("  quota    NOT READ: %v\n", err)
		fmt.Printf("           (this says billet could not ask, which is not the same as the " +
			"account having no limit)\n")

		// THE NODE'S OWN ROLE DELIBERATELY LACKS THIS PERMISSION, so an access
		// denial here is the expected answer on a correctly-scoped host rather
		// than a misconfiguration. `billet init iam` does not grant
		// servicequotas:* because the node never reads a quota at RUNTIME —
		// nothing in a launch, a teardown or a sweep consults one — and a
		// permission granted for a diagnostic is a permission the machine holding
		// the GitHub App key carries forever.
		if strings.Contains(err.Error(), "AccessDenied") {
			fmt.Printf("           A node's own role does not grant servicequotas on purpose: " +
				"billet reads a quota only in this diagnostic, never at runtime. Run " +
				"`billet check` under credentials that have servicequotas:GetServiceQuota " +
				"and servicequotas:ListServiceQuotas, or read the limits in the console\n")
		}
	}
}

// printQuota renders one ceiling, and compares it to the configured number where
// the two are about the same thing.
func printQuota(cfg *config.Config, q provider.Quota) {
	fmt.Printf("  quota    %s: %s (%s)\n", q.Scope, limitText(q.Limit, q.Unit), q.Code)

	want, matched := configuredAgainst(cfg, q)
	if !matched {
		return
	}

	if float64(want) <= q.Limit {
		fmt.Printf("           this deployment is configured for at most %d, which fits\n", want)

		return
	}

	// SAID PROMINENTLY AND STILL NOT FATAL. What an operator does about it is
	// raise the quota or lower the budget, and billet cannot tell which they
	// meant — but meeting it as a queued build that failed hours later is the
	// outcome this exists to prevent.
	fmt.Printf("           OVER: this deployment is configured for up to %d, which the "+
		"account will not run\n", want)
	fmt.Printf("           Raise it in Service Quotas, or lower node.max_vcpu / " +
		"node.max_memory. Work past the limit does not queue politely: it is REFUSED, and " +
		"GitHub requeues a job at most three times\n")
}

// configuredAgainst is how many of this limit's unit billet is configured to
// want, and whether the two are comparable at all.
//
// THE COMPARISON LIVES HERE RATHER THAN IN THE BACKEND, and that is the whole
// reason provider.Quota carries a Shape rather than a number. A backend knows
// the vendor's codes; it does not know the deployment's budget — node.max_vcpu
// and node.max_memory are NodeConfig's, not the backend block's — so this
// arithmetic belongs where the whole config is visible.
//
// AND SOME LIMITS HAVE NO COUNTERPART AT ALL. CodeBuild's account-wide queue
// depth bounds a burst across every project in the account rather than this
// node's budget, so rendering "30 against 16" would invite a reader to compare
// two numbers that are not about the same thing. Those carry no Shape and are
// reported alone.
func configuredAgainst(cfg *config.Config, q provider.Quota) (int, bool) {
	if cfg.Node == nil {
		return 0, false
	}

	// A vCPU LIMIT LINES UP WITH THE BUDGET DIRECTLY, with no arithmetic about
	// shapes: it counts the same unit node.max_vcpu does.
	if q.Unit == "vCPU" && q.Shape == "" {
		return cfg.Node.MaxVCPU, true
	}

	if q.Shape == "" {
		return 0, false
	}

	shape, found := declaredShape(cfg, q.Shape)
	if !found {
		return 0, false
	}

	return concurrentOf(cfg.Node, shape), true
}

// declaredShape finds a shape in whichever remote catalogue this node declares.
func declaredShape(cfg *config.Config, name string) (config.RemoteShape, bool) {
	var shapes []config.RemoteShape

	switch {
	case cfg.Node.CodeBuild != nil:
		shapes = cfg.Node.CodeBuild.ComputeTypes
	case cfg.Node.EC2 != nil:
		shapes = cfg.Node.EC2.InstanceTypes
	}

	for i := range shapes {
		if shapes[i].Type == name {
			return shapes[i], true
		}
	}

	return config.RemoteShape{}, false
}

// concurrentOf is how many instances of one shape this node's budget could hold
// at once.
//
// THE TIGHTER OF THE TWO CEILINGS, because both are hard: placement charges the
// SHAPE rather than the tier request, and a launch is authorised against the node
// budget before the request reaches the API. A shape declaring zero of either
// answers zero rather than dividing by it.
func concurrentOf(node *config.NodeConfig, shape config.RemoteShape) int {
	if shape.VCPU <= 0 || shape.Memory <= 0 {
		return 0
	}

	byVCPU := node.MaxVCPU / shape.VCPU
	byMemory := int(node.MaxMemory / shape.Memory)

	return min(byVCPU, byMemory)
}

// limitText renders a ceiling the way an operator reads one.
//
// AWS REPORTS A DOUBLE and most limits are whole numbers, so a plain %v prints
// "1" for some and "1.5" for others depending on the quota — which reads as
// billet being inconsistent rather than as the limit being fractional.
func limitText(limit float64, unit string) string {
	whole := math.Trunc(limit) == limit

	suffix := ""
	if unit != "" && unit != "None" {
		suffix = " " + unit
	}

	if whole {
		return fmt.Sprintf("%d%s", int64(limit), suffix)
	}

	return fmt.Sprintf("%g%s", limit, suffix)
}
