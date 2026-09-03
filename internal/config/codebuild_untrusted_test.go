package config

import (
	"slices"
	"strings"
	"testing"
)

// AN UNTRUSTED NETWORK LOADS ON AN ON-DEMAND CONTAINER NODE, and the three fields
// survive to the config the provider reads. This is the shape a fork pull-request
// tier needs; whether a given tier is untrusted is the provider's decision, not
// this layer's, so config only proves the fields parse and stay together.
func TestAnUntrustedCodeBuildNetworkLoads(t *testing.T) {
	body := codeBuildConfig(t, "    jit_parameter_path: /billet/jit\n",
		"    jit_parameter_path: /billet/jit\n"+
			"    untrusted_vpc_id: vpc-0fork\n"+
			"    untrusted_subnets: [subnet-0fork]\n"+
			"    untrusted_security_group_ids: [sg-0fork]\n")

	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cb := cfg.Node.CodeBuild
	if cb == nil || !cb.HasUntrustedNetwork() {
		t.Fatalf("the untrusted network did not survive loading: %+v", cb)
	}

	if cb.UntrustedVPCID != "vpc-0fork" ||
		!slices.Equal(cb.UntrustedSubnetIDs, []string{"subnet-0fork"}) ||
		!slices.Equal(cb.UntrustedSecurityGroupIDs, []string{"sg-0fork"}) {
		t.Errorf("the untrusted network fields loaded wrong: %+v", cb)
	}
}

// A BLANK ENTRY IS REFUSED, because normalize trims it to nothing and a list
// holding one empty string is non-empty by length while naming no subnet at all
// — it passed the "all three set" rule and failed on the first fork job.
func TestABlankUntrustedNetworkEntryIsRefused(t *testing.T) {
	for name, tc := range map[string]struct{ add, want string }{
		"blank subnet": {
			add:  "    untrusted_subnets: [\" \"]\n    untrusted_security_group_ids: [sg-0fork]\n",
			want: "untrusted_subnets[0] is blank",
		},
		"blank group": {
			add:  "    untrusted_subnets: [subnet-0fork]\n    untrusted_security_group_ids: [\" \"]\n",
			want: "untrusted_security_group_ids[0] is blank",
		},
	} {
		t.Run(name, func(t *testing.T) {
			body := codeBuildConfig(t, "    jit_parameter_path: /billet/jit\n",
				"    jit_parameter_path: /billet/jit\n    untrusted_vpc_id: vpc-0fork\n"+tc.add)

			got := loadErr(t, body)
			if !strings.Contains(got, tc.want) {
				t.Errorf("a blank entry was not refused naming its index: %s", got)
			}
		})
	}
}

// A PARTIAL UNTRUSTED NETWORK IS REFUSED — the ec2 use_vpc trap. A vpc with no
// subnet, or subnets with no group, describes an isolation that cannot be built,
// so it is refused at load rather than discovered on the first fork job.
func TestAPartialUntrustedNetworkIsRefused(t *testing.T) {
	for name, add := range map[string]string{
		"vpc only":       "    untrusted_vpc_id: vpc-0fork\n",
		"vpc and subnet": "    untrusted_vpc_id: vpc-0fork\n    untrusted_subnets: [subnet-0fork]\n",
		"subnet only":    "    untrusted_subnets: [subnet-0fork]\n",
		"group only":     "    untrusted_security_group_ids: [sg-0fork]\n",
	} {
		t.Run(name, func(t *testing.T) {
			body := codeBuildConfig(t, "    jit_parameter_path: /billet/jit\n",
				"    jit_parameter_path: /billet/jit\n"+add)

			got := loadErr(t, body)
			if !strings.Contains(got, "must be set together") {
				t.Errorf("a partial untrusted network was not refused as incomplete: %s", got)
			}
		})
	}
}

// AN UNTRUSTED NETWORK BESIDE A FLEET IS REFUSED EVEN WHEN COMPLETE, because a
// reserved fleet shares a machine between builds and a fleetOverride discards the
// project's vpc, so the declared network isolates nothing — it is dead config.
func TestAnUntrustedNetworkBesideAFleetIsRefused(t *testing.T) {
	body := codeBuildConfig(t, "    jit_parameter_path: /billet/jit\n",
		"    jit_parameter_path: /billet/jit\n"+
			"    fleet_arn: arn:aws:codebuild:us-west-2:000000000000:fleet/linux\n"+
			"    untrusted_vpc_id: vpc-0fork\n"+
			"    untrusted_subnets: [subnet-0fork]\n"+
			"    untrusted_security_group_ids: [sg-0fork]\n")

	got := loadErr(t, body)
	if !strings.Contains(got, "fleet_arn") {
		t.Errorf("an untrusted network beside a fleet was not refused naming fleet_arn: %s", got)
	}
}
