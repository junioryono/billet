package ec2

import (
	"context"
	"fmt"

	"github.com/junioryono/billet/internal/awsquota"
	"github.com/junioryono/billet/internal/provider"
)

const (
	// ec2ServiceCode is what Service Quotas calls this service.
	ec2ServiceCode = "ec2"

	// standardOnDemandVCPUQuota bounds the vCPUs of running On-Demand instances
	// from the standard families — A, C, D, H, I, M, R, T and Z, which is every
	// shape billet's own generated catalogues use.
	//
	// NAMED BY CODE RATHER THAN LISTED, which is the opposite of what the
	// codebuild side does, and the difference is confidence. EC2 publishes
	// hundreds of quotas and this one identifier is stable, documented and the
	// same in every region — so listing them all to find it would be several
	// pages of traffic to rediscover a constant. CodeBuild's are per compute
	// type and billet would be inventing them.
	//
	// IT IS COUNTED IN vCPUs, NOT INSTANCES, which is why it lines up with
	// node.max_vcpu directly and needs no arithmetic about shapes.
	standardOnDemandVCPUQuota = "L-1216C47A"
)

// Quotas reports the account ceiling this node's budget runs against.
//
// ONE LIMIT, DELIBERATELY. An ec2 node may buy several shapes, but the ceiling
// that binds them is a single vCPU allowance across the standard families rather
// than a limit per shape — so the useful sentence is "this account will run N
// vCPUs and you have configured M", which is a comparison `billet check` can
// make against node.max_vcpu without knowing anything about the catalogue.
//
// WHAT IT DOES NOT COVER, and the report says so: a deployment declaring a
// shape outside the standard families — a GPU, a metal, a burstable-unlimited
// spot pool — runs against a different allowance this does not read. Reporting
// the standard one is still worth more than reporting nothing, and claiming it
// covers everything would be the overreach ADR-005 warns about.
func (p *Provider) Quotas(ctx context.Context) ([]provider.Quota, error) {
	// THE CREDENTIALS AND THE ENDPOINT COME FROM THE API CLIENT THIS PROVIDER
	// ALREADY HOLDS. Service Quotas is a different service from EC2, so it gets
	// its own signed client — but it must sign as the same principal, and a test
	// that pointed the EC2 calls at a fake would otherwise have this one reach
	// the real AWS.
	client := awsquota.New(p.cfg.Region, p.api.quotas, p.api.creds)

	q, err := client.Get(ctx, ec2ServiceCode, standardOnDemandVCPUQuota)
	if err != nil {
		return nil, fmt.Errorf("running on-demand instances: %w", err)
	}

	return []provider.Quota{{
		Name:  q.Name,
		Code:  q.Code,
		Limit: q.Value,
		Unit:  q.Unit,
		Scope: "vCPUs of running On-Demand instances from the standard families " +
			"(A, C, D, H, I, M, R, T, Z); a GPU or metal shape runs against a different one",
	}}, nil
}
