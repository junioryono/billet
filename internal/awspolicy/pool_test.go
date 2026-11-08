package awspolicy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const poolOwner = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"

func poolInputs() Inputs {
	return Inputs{
		Owner: poolOwner, NoCompute: true,
		Pool: &Pool{
			Region: "us-west-2", SubnetID: "subnet-0abc", SecurityGroupIDs: []string{"sg-0abc", "sg-0def"},
			Images:                 []string{"ami-0a1b", "ami-0c2d"},
			InstanceProfileRoleARN: "arn:aws:iam::123456789012:role/billet-pool-host",
			KMSKeyARN:              "arn:aws:kms:us-west-2:123456789012:key/abcd",
		},
	}
}

// wantPoolPolicy is the whole grant, written out by hand from ec2.Hosts rather than
// from poolStatements: every statement's effect, actions, resources and condition
// operators. OWNER stands for the owner condition, which differs by mode.
const wantPoolPolicy = `{"Version":"2012-10-17","Statement":[
{"Sid":"BilletPoolRead","Effect":"Allow","Action":["ec2:DescribeInstances"],"Resource":["*"]},
{"Sid":"BilletPoolLaunchFrom","Effect":"Allow","Action":["ec2:RunInstances"],"Resource":[
 "arn:aws:ec2:us-west-2:*:subnet/subnet-0abc","arn:aws:ec2:us-west-2:*:network-interface/*",
 "arn:aws:ec2:us-west-2:*:security-group/sg-0abc","arn:aws:ec2:us-west-2:*:security-group/sg-0def",
 "arn:aws:ec2:us-west-2::image/ami-0a1b","arn:aws:ec2:us-west-2::image/ami-0c2d"]},
{"Sid":"BilletPoolLaunchHost","Effect":"Allow","Action":["ec2:RunInstances"],
 "Resource":["arn:aws:ec2:us-west-2:*:instance/*","arn:aws:ec2:us-west-2:*:volume/*"],
 "Condition":{REQUEST,
  "ForAllValues:StringEquals":{"aws:TagKeys":["Name","sh.billet.pool-owner","sh.billet.pool-order"]},
  "ForAnyValue:StringEquals":{"aws:TagKeys":["sh.billet.pool-owner"]}}},
{"Sid":"BilletPoolTag","Effect":"Allow","Action":["ec2:CreateTags"],
 "Resource":["arn:aws:ec2:us-west-2:*:instance/*","arn:aws:ec2:us-west-2:*:volume/*"],
 "Condition":{TAGGING,
  "ForAllValues:StringEquals":{"aws:TagKeys":["Name","sh.billet.pool-owner","sh.billet.pool-order"]},
  "ForAnyValue:StringEquals":{"aws:TagKeys":["sh.billet.pool-owner"]}}},
{"Sid":"BilletPoolTerminate","Effect":"Allow","Action":["ec2:TerminateInstances"],
 "Resource":["arn:aws:ec2:us-west-2:*:instance/*"],"Condition":{RESOURCE}},
{"Sid":"BilletPoolConsole","Effect":"Allow","Action":["ec2:GetConsoleOutput"],
 "Resource":["arn:aws:ec2:us-west-2:*:instance/*"],"Condition":{RESOURCE}},
{"Sid":"BilletPoolPassRole","Effect":"Allow","Action":["iam:PassRole"],
 "Resource":["arn:aws:iam::123456789012:role/billet-pool-host"],
 "Condition":{"StringEquals":{"iam:PassedToService":"ec2.amazonaws.com"}}},
{"Sid":"BilletPoolKMSUse","Effect":"Allow",
 "Action":["kms:DescribeKey","kms:GenerateDataKeyWithoutPlaintext","kms:ReEncrypt*","kms:Decrypt"],
 "Resource":["arn:aws:kms:us-west-2:123456789012:key/abcd"],
 "Condition":{"StringEquals":{"kms:ViaService":"ec2.us-west-2.amazonaws.com"}}},
{"Sid":"BilletPoolKMSGrant","Effect":"Allow","Action":["kms:CreateGrant"],
 "Resource":["arn:aws:kms:us-west-2:123456789012:key/abcd"],
 "Condition":{"Bool":{"kms:GrantIsForAWSResource":"true"}}}]}`

func samePolicy(t *testing.T, mode string, p Policy, want string) {
	t.Helper()
	got, err := p.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("the expected %s policy is not JSON: %v", mode, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("the %s pool grant is\n%s\nwant\n%s", mode, got, want)
	}
}

// THE POOL'S GRANT IS EXACTLY WHAT ec2.Hosts DOES, TO ONLY THE POOL'S OWN. Each action
// the pool-host surface performs is granted in one statement: the launch to the pool's
// subnet, groups and images and to a host and volumes tagged with this deployment's
// pool owner and no other key; the tagging to that launch; the terminate and the
// console read to instances carrying the pool owner; PassRole to EC2; the key through
// EC2 alone. A NoCompute principal gets nothing of a node's. Compared whole, so an
// action moved to another statement, a resource widened or an operator negated fails.
func TestThePoolGrantIsWhatThePoolHostSurfaceDoes(t *testing.T) {
	t.Parallel()
	p, err := poolInputs().Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	samePolicy(t, "per-deployment", p, strings.NewReplacer(
		"REQUEST", `"StringEquals":{"aws:RequestTag/sh.billet.pool-owner":"`+poolOwner+`"}`,
		"TAGGING", `"StringEquals":{"ec2:CreateAction":["RunInstances"],"aws:RequestTag/sh.billet.pool-owner":"`+poolOwner+`"}`,
		"RESOURCE", `"StringEquals":{"aws:ResourceTag/sh.billet.pool-owner":"`+poolOwner+`"}`,
	).Replace(wantPoolPolicy))

	// ACCOUNT-WIDE, the pool owner's presence stands for its value, as everywhere else.
	in := poolInputs()
	in.Owner = ""
	p, err = in.Build()
	if err != nil {
		t.Fatalf("Build account-wide: %v", err)
	}
	samePolicy(t, "account-wide", p, strings.NewReplacer(
		"REQUEST", `"Null":{"aws:RequestTag/sh.billet.pool-owner":"false"}`,
		"TAGGING", `"Null":{"aws:RequestTag/sh.billet.pool-owner":"false"},"StringEquals":{"ec2:CreateAction":["RunInstances"]}`,
		"RESOURCE", `"Null":{"aws:ResourceTag/sh.billet.pool-owner":"false"}`,
	).Replace(wantPoolPolicy))
}

// AN ID THAT LANDS IN A RESOURCE ARN IS CHECKED FIRST, because one carrying a `*`
// would widen the grant to everything it matches, and a pool naming nothing to launch
// into or from would grant a launch that can never succeed.
func TestThePoolGrantRefusesWhatWouldWidenIt(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*Pool){
		"a wildcard subnet":        func(p *Pool) { p.SubnetID = "subnet-*" },
		"a wildcard group":         func(p *Pool) { p.SecurityGroupIDs = []string{"sg-*"} },
		"a wildcard image":         func(p *Pool) { p.Images = []string{"ami-*"} },
		"a wildcard region":        func(p *Pool) { p.Region = "*" },
		"no group":                 func(p *Pool) { p.SecurityGroupIDs = nil },
		"no image":                 func(p *Pool) { p.Images = nil },
		"a role that is not":       func(p *Pool) { p.InstanceProfileRoleARN = "arn:aws:iam::123456789012:user/x" },
		"a key in another region":  func(p *Pool) { p.KMSKeyARN = "arn:aws:kms:us-east-1:123456789012:key/abcd" },
		"a key that is an alias":   func(p *Pool) { p.KMSKeyARN = "arn:aws:kms:us-west-2:123456789012:alias/x" },
		"a key with a wildcard":    func(p *Pool) { p.KMSKeyARN = "arn:aws:kms:us-west-2:123456789012:key/*" },
		"a key that is not an arn": func(p *Pool) { p.KMSKeyARN = "abcd" },
	} {
		in := poolInputs()
		pool := *in.Pool
		mutate(&pool)
		in.Pool = &pool
		if _, err := in.Build(); err == nil {
			t.Errorf("a pool with %s was granted", name)
		}
	}
}
