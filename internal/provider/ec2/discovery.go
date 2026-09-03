package ec2

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/config"
)

// InstanceTypeInfo is what one EC2 shape holds, as DescribeInstanceTypes reports
// it. `billet init --provider ec2` reads it so an operator can name a shape and
// have billet write the vcpu and memory an instance_types entry must DECLARE,
// rather than looking those up by hand and copying a number that overcommits the
// host the allocator escrowed against if it is wrong.
type InstanceTypeInfo struct {
	Type      string
	VCPU      int
	MemoryMiB int64
}

// discoveryClient builds a signed-API client for the read-only describes that
// `billet init` (and, later, the cloud preflight) make, WITHOUT going through
// ec2.New — discovery names no deployment owner and no security groups, which New
// requires. It mirrors New's redirect refusal: AWS does not redirect, so a
// redirect from this endpoint is not the API answering, and following it would
// send signed AWS credentials to whatever chose the target. creds nil defaults to
// the env/IMDS chain; endpoint empty derives from the region.
func discoveryClient(region, endpoint string, creds awscreds.Source) *client {
	if creds == nil {
		creds = awscreds.Default()
	}

	if endpoint == "" {
		endpoint = defaultEndpointFor(region)
	}

	httpClient := &http.Client{Timeout: apiTimeout}
	httpClient.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		return fmt.Errorf("%w to host %q", errRedirected, req.URL.Hostname())
	}

	return &client{http: httpClient, endpoint: endpoint, region: region, creds: creds}
}

// checkDiscoveryRegion validates a region that will be interpolated into the
// endpoint host. It is applied HERE and not only by the config path — ec2.New
// re-applies the same rule for the same reason. An unchecked region such as
// `x@attacker.example/?` yields a host of attacker.example, and a request signed
// with the operator's credentials would be sent there before anything else runs.
// A caller that supplies its own endpoint has chosen the host explicitly, so the
// region is then only a signing input and is left to the signer.
func checkDiscoveryRegion(region, endpoint string) error {
	if endpoint != "" {
		return nil
	}
	if err := config.CheckEC2Region(region); err != nil {
		return fmt.Errorf("ec2: %w", err)
	}

	return nil
}

// describeInstanceTypesResponse is the slice of the DescribeInstanceTypes reply
// billet reads: each shape's name and what it holds. Declared with xml tags and
// decoded by client.call, like every other response in this package.
type describeInstanceTypesResponse struct {
	InstanceTypes []struct {
		InstanceType string `xml:"instanceType"`
		VCPUInfo     struct {
			DefaultVCPUs int `xml:"defaultVCpus"`
		} `xml:"vCpuInfo"`
		MemoryInfo struct {
			SizeInMiB int64 `xml:"sizeInMiB"`
		} `xml:"memoryInfo"`
	} `xml:"instanceTypeSet>item"`
	NextToken string `xml:"nextToken"`
}

// DescribeInstanceTypes reports what each named shape holds, following the reply's
// pagination to the end. It is NOT part of the node's runtime IAM set: it runs at
// `billet init` time under the operator's own credentials, not under the launched
// node's least-privilege role, so adding ec2:DescribeInstanceTypes to a node's
// policy would widen a grant nothing at runtime exercises.
//
// A shape AWS does not offer in the region comes back absent rather than as an
// error, so the caller checks that every type it asked for is present: billet
// must not silently write a config missing a shape the operator named, because a
// tier derived from it would then have nothing to buy.
func DescribeInstanceTypes(
	ctx context.Context, region, endpoint string, creds awscreds.Source, types []string,
) ([]InstanceTypeInfo, error) {
	if err := checkDiscoveryRegion(region, endpoint); err != nil {
		return nil, err
	}

	c := discoveryClient(region, endpoint, creds)

	var infos []InstanceTypeInfo

	seen := make(map[string]bool)
	token := ""
	for {
		params := url.Values{}
		params.Set("Action", "DescribeInstanceTypes")
		for i, t := range types {
			params.Set("InstanceType."+strconv.Itoa(i+1), t)
		}

		if token != "" {
			params.Set("NextToken", token)
		}

		var out describeInstanceTypesResponse
		if err := c.call(ctx, params, &out); err != nil {
			return nil, err
		}

		for _, it := range out.InstanceTypes {
			infos = append(infos, InstanceTypeInfo{
				Type:      it.InstanceType,
				VCPU:      it.VCPUInfo.DefaultVCPUs,
				MemoryMiB: it.MemoryInfo.SizeInMiB,
			})
		}

		if out.NextToken == "" {
			return infos, nil
		}
		if seen[out.NextToken] {
			return nil, fmt.Errorf("ec2: DescribeInstanceTypes cycled its pagination token %q",
				out.NextToken)
		}

		seen[out.NextToken] = true
		token = out.NextToken
	}
}
