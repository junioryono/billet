package setup

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// networkGrant decodes what init iam printed and returns the launch network
// statement's resources, failing when there is none.
func networkGrant(t *testing.T, out string) []string {
	t.Helper()

	var doc struct {
		Statement []struct {
			Sid      string   `json:"Sid"`
			Resource []string `json:"Resource"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("init iam did not print valid JSON: %v\n%s", err, out)
	}

	for _, s := range doc.Statement {
		if s.Sid == "BilletRuntimeLaunchNetwork" {
			return s.Resource
		}
	}

	t.Fatalf("init iam printed no launch network grant:\n%s", out)

	return nil
}

// THE LAUNCH GRANT NAMES THE CONFIG'S OWN NETWORK, and nothing else in the
// account: the subnet, the trusted group and the untrusted one (#779). A launch
// for fork work attaches the untrusted group, so a grant that left it out would
// refuse every untrusted job.
func TestInitIAMScopesTheLaunchToTheConfigsNetwork(t *testing.T) {
	path := ec2NodeConfig(t)

	out := capture(t, func() {
		if err := Init(t.Context(), processEnv(), []string{
			"iam", "--config", path, "--deployment", testDeployID, "--role-arn",
			"arn:aws:iam::123456789012:role/billet-node",
		}); err != nil {
			t.Fatalf("init iam: %v", err)
		}
	})

	want := []string{
		"arn:aws:ec2:*:*:subnet/subnet-0abc",
		"arn:aws:ec2:*:*:security-group/sg-0abc",
		"arn:aws:ec2:*:*:security-group/sg-0def",
	}
	if got := networkGrant(t, out); !slices.Equal(got, want) {
		t.Errorf("the launch network grant names %v, want %v", got, want)
	}
}

// A BUILDER THAT LAUNCHES ELSEWHERE SAYS WHERE, and the grant names it beside the
// node's own; one that launches where the node does adds nothing twice.
func TestInitIAMAddsTheBuildersOwnNetwork(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		want  []string
	}{
		{
			name:  "its own subnet and group",
			flags: []string{"--builder-subnet", "subnet-0b1d", "--builder-security-group", "sg-0b1d"},
			want: []string{
				"arn:aws:ec2:*:*:subnet/subnet-0abc",
				"arn:aws:ec2:*:*:subnet/subnet-0b1d",
				"arn:aws:ec2:*:*:security-group/sg-0abc",
				"arn:aws:ec2:*:*:security-group/sg-0b1d",
			},
		},
		{
			name:  "the node's own again",
			flags: []string{"--builder-subnet", "subnet-0abc", "--builder-security-group", "sg-0abc"},
			want: []string{
				"arn:aws:ec2:*:*:subnet/subnet-0abc",
				"arn:aws:ec2:*:*:security-group/sg-0abc",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := plainEC2NodeConfig(t)

			out := capture(t, func() {
				args := append([]string{
					"iam", "--config", path, "--deployment", testDeployID, "--builder",
				}, tc.flags...)
				if err := Init(t.Context(), processEnv(), args); err != nil {
					t.Fatalf("init iam: %v", err)
				}
			})

			if got := networkGrant(t, out); !slices.Equal(got, tc.want) {
				t.Errorf("the launch network grant names %v, want %v", got, tc.want)
			}
		})
	}
}

// WITHOUT --builder THE BUILDER'S NETWORK IS REFUSED, by the flags it names, for
// the payload bucket's reason: an operator who passed it meant to grant something.
func TestInitIAMRefusesABuilderNetworkWithoutTheBuilder(t *testing.T) {
	for _, flag := range []string{"--builder-subnet", "--builder-security-group"} {
		t.Run(flag, func(t *testing.T) {
			err := Init(t.Context(), processEnv(), []string{
				"iam", "--config", plainEC2NodeConfig(t), "--deployment", testDeployID,
				flag, "x-0abc",
			})
			if err == nil || !strings.Contains(err.Error(), "only meaningful with --builder") {
				t.Fatalf("init iam answered %v, want a refusal naming --builder", err)
			}
		})
	}
}
