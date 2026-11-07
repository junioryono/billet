package awspolicy

import (
	"slices"
	"strings"
	"testing"
)

// A LAUNCH TAGS WHAT IT CREATES, AND STAYS ON ITS NETWORK (#779).
//
// The grant on "*" let a holder of the node's role launch into any subnet with any
// security group, and launch an instance with no owner tag, which no billet role can
// list or terminate. The instance and its volumes are now allowed only under the
// owner tag, and a named network is the only one. Each half is asserted on both
// statements it takes: the scoped grant, and the NotResource grant beside it, which
// would hand the same resource back unconditioned if it stopped excluding it.
func TestALaunchTagsWhatItCreatesAndStaysOnItsNetwork(t *testing.T) {
	const owner = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"

	network := &Network{
		SubnetIDs:        []string{"subnet-0123456789abcdef0"},
		SecurityGroupIDs: []string{"sg-0123456789abcdef0", "sg-fedcba9876543210f"},
	}

	for _, tc := range []struct {
		name string
		in   Inputs
		// operator and value the owned statement's condition must hold.
		op    string
		value any
	}{
		{name: "account-wide", in: Inputs{}, op: "Null", value: "false"},
		{name: "per-deployment", in: Inputs{Owner: owner}, op: "StringEquals", value: owner},
		{
			name: "per-deployment builder", in: Inputs{Owner: owner, Builder: true},
			op: "StringLike", value: []string{owner, "billet-ami-build-" + owner + "-*"},
		},
		{
			name: "per-deployment on a network", in: Inputs{Owner: owner, Network: network},
			op: "StringEquals", value: owner,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := tc.in.Build()
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			owned := stmt(t, p, "BilletRuntimeLaunchOwned")
			if owned.Effect != "Allow" || !slices.Equal(owned.Action, []string{"ec2:RunInstances"}) {
				t.Errorf("the owned launch grant is %s %v, want Allow ec2:RunInstances",
					owned.Effect, owned.Action)
			}

			created := []string{"arn:aws:ec2:*:*:instance/*", "arn:aws:ec2:*:*:volume/*"}
			if !slices.Equal(owned.Resource, created) {
				t.Errorf("the owned launch grant acts on %v, want %v", owned.Resource, created)
			}

			cond, ok := owned.Condition[tc.op].(map[string]any)
			if !ok || len(owned.Condition) != 1 {
				t.Fatalf("the owned launch grant's condition is %v, want only %s", owned.Condition, tc.op)
			}

			got := cond["aws:RequestTag/sh.billet.owner"]
			if want, isList := tc.value.([]string); isList {
				if list, _ := got.([]string); !slices.Equal(list, want) {
					t.Errorf("the owned launch grant admits owner %v, want %v", got, want)
				}
			} else if got != tc.value {
				t.Errorf("the owned launch grant admits owner %v, want %v", got, tc.value)
			}

			// THE EXCLUSIONS ARE EXACT, in both directions. One missing hands the owned
			// grant's resources, or the network, back without their scope; one extra (an
			// image, a network interface) is a resource type nothing then grants, and
			// every launch is refused.
			launch := stmt(t, p, "BilletRuntimeLaunch")
			if launch.Effect != "Allow" || !slices.Equal(launch.Action, []string{"ec2:RunInstances"}) ||
				launch.Condition != nil || len(launch.Resource) != 0 {
				t.Errorf("the unconditioned launch grant is %s %v with %v on %v; it is an Allow "+
					"of ec2:RunInstances by NotResource with no condition",
					launch.Effect, launch.Action, launch.Condition, launch.Resource)
			}

			excluded := created
			if tc.in.Network != nil {
				excluded = append(slices.Clone(created),
					"arn:aws:ec2:*:*:subnet/*", "arn:aws:ec2:*:*:security-group/*")
			}

			if !slices.Equal(launch.NotResource, excluded) {
				t.Errorf("the unconditioned launch grant excludes %v, want exactly %v",
					launch.NotResource, excluded)
			}

			if tc.in.Network == nil {
				for _, s := range p.Statement {
					if s.Sid == "BilletRuntimeLaunchNetwork" {
						t.Error("a policy that names no network carries a network grant")
					}
				}

				return
			}

			net := stmt(t, p, "BilletRuntimeLaunchNetwork")
			want := []string{
				"arn:aws:ec2:*:*:subnet/subnet-0123456789abcdef0",
				"arn:aws:ec2:*:*:security-group/sg-0123456789abcdef0",
				"arn:aws:ec2:*:*:security-group/sg-fedcba9876543210f",
			}
			if net.Effect != "Allow" || !slices.Equal(net.Action, []string{"ec2:RunInstances"}) ||
				net.Condition != nil || !slices.Equal(net.Resource, want) {
				t.Errorf("the network grant is %s %v with %v on %v, want an unconditioned Allow "+
					"of ec2:RunInstances on %v", net.Effect, net.Action, net.Condition, net.Resource, want)
			}
		})
	}
}

// A NETWORK THAT WOULD WIDEN THE GRANT OR GRANT NOTHING IS REFUSED, by the clause
// that names why. An id lands in a Resource ARN, so a `*` in one is every subnet it
// matches; an empty list is a policy under which every launch is refused.
func TestALaunchNetworkIsRefusedWhenItWidensOrGrantsNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Inputs
		want string
	}{
		{
			name: "a wildcard subnet",
			in: Inputs{Network: &Network{
				SubnetIDs: []string{"subnet-*"}, SecurityGroupIDs: []string{"sg-0123456789abcdef0"},
			}},
			want: "is not a subnet id",
		},
		{
			name: "a wildcard group",
			in: Inputs{Network: &Network{
				SubnetIDs: []string{"subnet-0123456789abcdef0"}, SecurityGroupIDs: []string{"sg-0123*"},
			}},
			want: "is not a security group id",
		},
		{
			name: "no subnet",
			in:   Inputs{Network: &Network{SecurityGroupIDs: []string{"sg-0123456789abcdef0"}}},
			want: "at least one subnet and one security group",
		},
		{
			name: "no group",
			in:   Inputs{Network: &Network{SubnetIDs: []string{"subnet-0123456789abcdef0"}}},
			want: "at least one subnet and one security group",
		},
		{
			name: "a principal that launches nothing",
			in: Inputs{NoCompute: true, Builder: true, Network: &Network{
				SubnetIDs:        []string{"subnet-0123456789abcdef0"},
				SecurityGroupIDs: []string{"sg-0123456789abcdef0"},
			}},
			want: "only meaningful for a principal that launches",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.in.Build()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Build answered %v, want a refusal saying %q", err, tc.want)
			}
		})
	}

	// A PARTITION THAT IS NOT ONE IS REFUSED, because it lands in every exclusion:
	// one carrying a separator excludes an ARN no instance has, and the
	// unconditioned grant then reaches the instance the owned grant scopes.
	for _, partition := range []string{
		"aws:ec2:*:*:instance/i-0123456789abcdef0", "aws*", "${aws:partition}", "aws/x",
	} {
		_, err := Inputs{Partition: partition}.Build()
		if err == nil || !strings.Contains(err.Error(), "is not a partition") {
			t.Errorf("partition %q: Build answered %v, want a refusal", partition, err)
		}
	}

	for _, partition := range []string{"aws", "aws-cn", "aws-us-gov", "TFPARTITION"} {
		if _, err := (Inputs{Partition: partition}).Build(); err != nil {
			t.Errorf("partition %q was refused: %v", partition, err)
		}
	}

	// AND THE EIGHT-DIGIT FORM, which older subnets and groups still carry, is an id.
	// The shape beyond refusing a widening character is EC2's to judge.
	if _, err := (Inputs{Network: &Network{
		SubnetIDs: []string{"subnet-0123abcd"}, SecurityGroupIDs: []string{"sg-0123abcd"},
	}}).Build(); err != nil {
		t.Errorf("an eight-digit subnet and group were refused: %v", err)
	}
}

// EVERY STATEMENT NAMES EXACTLY ONE OF Resource AND NotResource. IAM refuses a
// statement with both, and one with neither is refused too; both fields are
// omitempty now, so nothing in the encoding catches a statement that lost its scope.
func TestEveryStatementNamesExactlyOneResourceScope(t *testing.T) {
	p, err := Inputs{
		Owner:  "0f1e2d3c4b5a69788796a5b4c3d2e1f0",
		Region: "us-west-2",
		Network: &Network{
			SubnetIDs: []string{"subnet-0123abcd"}, SecurityGroupIDs: []string{"sg-0123abcd"},
		},
		Cache: &Cache{
			Bucket: "b", Prefix: "p", KMSKeyARN: "arn:aws:kms:us-west-2:1:key/k",
		},
		SpotQueueARN:           "arn:aws:sqs:us-west-2:1:q",
		InstanceProfileRoleARN: "arn:aws:iam::1:role/r",
		Builder:                true,
		Payload:                &Payload{Bucket: "payloads"},
	}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if len(p.Statement) == 0 {
		t.Fatal("the policy is empty, so the check below proves nothing")
	}

	var notResource []string

	for _, s := range p.Statement {
		if (len(s.Resource) == 0) == (len(s.NotResource) == 0) {
			t.Errorf("statement %q has Resource %v and NotResource %v; exactly one must be set",
				s.Sid, s.Resource, s.NotResource)
		}

		if len(s.NotResource) > 0 {
			notResource = append(notResource, s.Sid)
		}
	}

	if !slices.Equal(notResource, []string{"BilletRuntimeLaunch"}) {
		t.Errorf("NotResource appears on %v, want only the launch grant", notResource)
	}
}
