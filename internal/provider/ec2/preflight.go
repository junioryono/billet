package ec2

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/awscreds"
)

// The read-only describes `billet check --provider ec2` makes to prove a config
// can actually launch, beyond CheckReachable's one DescribeInstances: that the
// subnet exists in the region, that the security groups exist in the subnet's
// VPC, and that each tier's AMI is available. They are describes rather than a
// dry-run launch because a dry-run is a write-shaped call a diagnostic should not
// make by default; each runs under the operator's credentials at check time.

// SubnetInfo is what one DescribeSubnets item tells the preflight.
type SubnetInfo struct {
	SubnetID         string
	VPCID            string
	AvailabilityZone string
	State            string
}

// SecurityGroupInfo is what one DescribeSecurityGroups item tells the preflight.
type SecurityGroupInfo struct {
	GroupID string
	VPCID   string
}

// ImageInfo is one AMI's launch readiness. Found is false when the id resolves to
// nothing — a not-yet-built placeholder, a typo, or an AMI in another account or
// region — with State carrying the AWS code (e.g. InvalidAMIID.Malformed) when
// there was one.
type ImageInfo struct {
	ImageID string
	State   string
	Found   bool
	// Contract is the AMIContract the image was stamped with, and BuiltBy the
	// billet that stamped it. Contract is 0 for an image built before billet
	// tagged its output — which is not "contract zero" but "no answer", and is
	// reported as needing a rebuild for the same reason an old contract is.
	Contract int
	BuiltBy  string
}

// DescribeSubnet reports the subnet's VPC, availability zone and state. A subnet
// id that resolves to nothing is an error rather than an empty result: the check
// asked about a specific subnet and "there is no such subnet" is the answer it
// needs, not silence.
func DescribeSubnet(
	ctx context.Context, region, endpoint string, creds awscreds.Source, subnetID string,
) (SubnetInfo, error) {
	if err := checkDiscoveryRegion(region, endpoint); err != nil {
		return SubnetInfo{}, err
	}

	c := discoveryClient(region, endpoint, creds)

	params := url.Values{}
	params.Set("Action", "DescribeSubnets")
	params.Set("SubnetId.1", subnetID)

	var out struct {
		Subnets []struct {
			SubnetID         string `xml:"subnetId"`
			VPCID            string `xml:"vpcId"`
			AvailabilityZone string `xml:"availabilityZone"`
			State            string `xml:"state"`
		} `xml:"subnetSet>item"`
	}
	if err := c.call(ctx, params, &out); err != nil {
		return SubnetInfo{}, err
	}

	if len(out.Subnets) == 0 {
		return SubnetInfo{}, fmt.Errorf("ec2: subnet %s was not found in %s", subnetID, region)
	}

	s := out.Subnets[0]

	return SubnetInfo{
		SubnetID:         s.SubnetID,
		VPCID:            s.VPCID,
		AvailabilityZone: s.AvailabilityZone,
		State:            s.State,
	}, nil
}

// DescribeSecurityGroups reports each group's VPC. A group id that resolves to
// nothing is an error, for the same reason as a missing subnet.
func DescribeSecurityGroups(
	ctx context.Context, region, endpoint string, creds awscreds.Source, groupIDs []string,
) ([]SecurityGroupInfo, error) {
	if err := checkDiscoveryRegion(region, endpoint); err != nil {
		return nil, err
	}

	c := discoveryClient(region, endpoint, creds)

	// PAGINATED, and the missing-id check runs only AFTER every page. A group the
	// config names could otherwise sit on a later page and be reported as
	// nonexistent — a fatal false failure of an otherwise valid config.
	byID := make(map[string]SecurityGroupInfo, len(groupIDs))
	seen := make(map[string]bool)
	token := ""
	for {
		params := url.Values{}
		params.Set("Action", "DescribeSecurityGroups")
		for i, id := range groupIDs {
			params.Set("GroupId."+strconv.Itoa(i+1), id)
		}
		if token != "" {
			params.Set("NextToken", token)
		}

		var out struct {
			Groups []struct {
				GroupID string `xml:"groupId"`
				VPCID   string `xml:"vpcId"`
			} `xml:"securityGroupInfo>item"`
			NextToken string `xml:"nextToken"`
		}
		if err := c.call(ctx, params, &out); err != nil {
			return nil, err
		}

		for _, g := range out.Groups {
			byID[g.GroupID] = SecurityGroupInfo{GroupID: g.GroupID, VPCID: g.VPCID}
		}

		if out.NextToken == "" {
			break
		}
		if seen[out.NextToken] {
			return nil, fmt.Errorf("ec2: DescribeSecurityGroups cycled its pagination token %q",
				out.NextToken)
		}

		seen[out.NextToken] = true
		token = out.NextToken
	}

	// Returned in the ORDER ASKED, and a group AWS omitted is a hard error: the
	// config names it, so "it does not exist" is the finding, not a short list the
	// caller has to notice is short.
	groups := make([]SecurityGroupInfo, 0, len(groupIDs))
	for _, id := range groupIDs {
		g, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("ec2: security group %s was not found in %s", id, region)
		}
		groups = append(groups, g)
	}

	return groups, nil
}

// DescribeImageStates reports each AMI's launch readiness, one describe per id so
// a malformed or not-found placeholder does not fail the lookup of a sibling that
// resolves. An InvalidAMIID.* code becomes a not-found result rather than an
// error, because "the AMI is not built yet" is a finding the check reports, not a
// failure of the check itself; any other API error is returned.
func DescribeImageStates(
	ctx context.Context, region, endpoint string, creds awscreds.Source, imageIDs []string,
) ([]ImageInfo, error) {
	if err := checkDiscoveryRegion(region, endpoint); err != nil {
		return nil, err
	}

	c := discoveryClient(region, endpoint, creds)

	out := make([]ImageInfo, 0, len(imageIDs))
	for _, id := range imageIDs {
		params := url.Values{}
		params.Set("Action", "DescribeImages")
		params.Set("ImageId.1", id)

		var resp describeImagesResponse
		if err := c.call(ctx, params, &resp); err != nil {
			if code, ok := codeOf(err); ok && strings.HasPrefix(code, "InvalidAMIID") {
				out = append(out, ImageInfo{ImageID: id, State: code, Found: false})

				continue
			}

			return nil, err
		}

		if len(resp.Images) == 0 {
			out = append(out, ImageInfo{ImageID: id, Found: false})

			continue
		}

		info := ImageInfo{ImageID: id, State: resp.Images[0].State, Found: true}
		for _, tag := range resp.Images[0].Tags {
			switch tag.Key {
			case amiContractTag:
				// A MALFORMED VALUE IS NOT A CONTRACT. Anyone can set a tag, so an
				// unparseable one leaves Contract at 0 and is reported as needing a
				// rebuild rather than trusted as some other number.
				if n, err := strconv.Atoi(tag.Value); err == nil {
					info.Contract = n
				}
			case amiBuiltByTag:
				info.BuiltBy = tag.Value
			}
		}

		out = append(out, info)
	}

	return out, nil
}
