package awspolicy

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/provider/ec2"
)

const poolOwner = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"

func poolInputs() Inputs {
	return Inputs{
		Owner: poolOwner, NoCompute: true,
		Pool: &Pool{
			Region: "us-west-2", SubnetID: "subnet-0abc", SecurityGroupIDs: []string{"sg-0abc", "sg-0def"},
			Images:                 []string{"ami-0a1b", "ami-0c2d"},
			InstanceProfileRoleARN: "arn:aws:iam::123456789012:role/billet-pool-host",
		},
	}
}

func condJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return b
}

func poolStatement(t *testing.T, p Policy, sid string) Statement {
	t.Helper()
	for _, s := range p.Statement {
		if s.Sid == sid {
			return s
		}
	}
	t.Fatalf("the policy has no %s statement: %+v", sid, p.Statement)

	return Statement{}
}

// THE POOL'S GRANT IS EXACTLY WHAT ec2.Hosts DOES, TO ONLY THE POOL'S OWN. Each action
// the pool-host surface performs is granted once, scoped: the launch to the pool's
// subnet, groups and images and to a host and volumes tagged with this deployment's
// pool owner and no other key; the terminate and the console read to instances
// carrying it. A NoCompute principal gets nothing of a node's.
func TestThePoolGrantIsWhatThePoolHostSurfaceDoes(t *testing.T) {
	t.Parallel()
	p, err := poolInputs().Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var sids, actions []string
	for _, s := range p.Statement {
		sids = append(sids, s.Sid)
		if s.NotResource != nil {
			t.Errorf("%s grants by NotResource; every pool resource is named", s.Sid)
		}
		if s.Effect == "Allow" {
			actions = append(actions, s.Action...)
		}
	}
	want := []string{"BilletPoolRead", "BilletPoolLaunchFrom", "BilletPoolLaunchHost", "BilletPoolTag",
		"BilletPoolTerminate", "BilletPoolConsole", "BilletPoolPassRole"}
	if !slices.Equal(sids, want) {
		t.Fatalf("statements = %v, want %v", sids, want)
	}
	for _, a := range []string{ec2.IAMRunInstances, ec2.IAMDescribeInstances, ec2.IAMTerminateInstances,
		ec2.IAMGetConsoleOutput, ec2.IAMCreateTags, "iam:PassRole"} {
		if !slices.Contains(actions, a) {
			t.Errorf("the pool grant lacks %s, which the pool-host surface performs", a)
		}
	}

	from := poolStatement(t, p, "BilletPoolLaunchFrom").Resource
	wantFrom := []string{
		"arn:aws:ec2:us-west-2:*:subnet/subnet-0abc", "arn:aws:ec2:us-west-2:*:network-interface/*",
		"arn:aws:ec2:us-west-2:*:security-group/sg-0abc", "arn:aws:ec2:us-west-2:*:security-group/sg-0def",
		"arn:aws:ec2:us-west-2::image/ami-0a1b", "arn:aws:ec2:us-west-2::image/ami-0c2d",
	}
	if !slices.Equal(from, wantFrom) {
		t.Errorf("the launch reaches %v, want exactly %v", from, wantFrom)
	}

	launch := poolStatement(t, p, "BilletPoolLaunchHost")
	if !slices.Equal(launch.Resource, []string{"arn:aws:ec2:us-west-2:*:instance/*", "arn:aws:ec2:us-west-2:*:volume/*"}) {
		t.Errorf("the host launch covers %v", launch.Resource)
	}
	for _, s := range []Statement{launch, poolStatement(t, p, "BilletPoolTag")} {
		cond := condJSON(t, s.Condition)
		for _, need := range []string{
			`"aws:RequestTag/` + ec2.PoolOwnerTagKey + `":"` + poolOwner + `"`,
			`"ForAllValues:StringEquals":{"aws:TagKeys":["Name","` + ec2.PoolOwnerTagKey + `","` + ec2.PoolOrderTagKey + `"]}`,
			`"ForAnyValue:StringEquals":{"aws:TagKeys":["` + ec2.PoolOwnerTagKey + `"]}`,
		} {
			if !strings.Contains(string(cond), need) {
				t.Errorf("%s's condition %s lacks %s", s.Sid, cond, need)
			}
		}
		if strings.Contains(string(cond), `"aws:RequestTag/`+ec2.OwnerTagKey+`"`) {
			t.Errorf("%s admits the job instances' owner tag: %s", s.Sid, cond)
		}
	}
	tag := condJSON(t, poolStatement(t, p, "BilletPoolTag").Condition)
	if !strings.Contains(string(tag), `"ec2:CreateAction":["RunInstances"]`) {
		t.Errorf("the pool's tagging is not limited to its launch: %s", tag)
	}
	for _, sid := range []string{"BilletPoolTerminate", "BilletPoolConsole"} {
		s := poolStatement(t, p, sid)
		cond := condJSON(t, s.Condition)
		if string(cond) != `{"StringEquals":{"aws:ResourceTag/`+ec2.PoolOwnerTagKey+`":"`+poolOwner+`"}}` ||
			!slices.Equal(s.Resource, []string{"arn:aws:ec2:us-west-2:*:instance/*"}) {
			t.Errorf("%s reaches %v when %s; want only instances carrying this deployment's pool owner",
				sid, s.Resource, cond)
		}
	}
	pass := poolStatement(t, p, "BilletPoolPassRole")
	if !slices.Equal(pass.Resource, []string{"arn:aws:iam::123456789012:role/billet-pool-host"}) {
		t.Errorf("PassRole reaches %v", pass.Resource)
	}

	// ACCOUNT-WIDE, the pool owner's presence stands for its value, as everywhere else.
	in := poolInputs()
	in.Owner = ""
	p, err = in.Build()
	if err != nil {
		t.Fatalf("Build account-wide: %v", err)
	}
	cond := condJSON(t, poolStatement(t, p, "BilletPoolTerminate").Condition)
	if string(cond) != `{"Null":{"aws:ResourceTag/`+ec2.PoolOwnerTagKey+`":"false"}}` {
		t.Errorf("account-wide terminate condition = %s", cond)
	}
	tag = condJSON(t, poolStatement(t, p, "BilletPoolTag").Condition)
	if !strings.Contains(string(tag), `"StringEquals":{"ec2:CreateAction":["RunInstances"]}`) {
		t.Errorf("account-wide tagging is not limited to the launch: %s", tag)
	}
}

// AN ID THAT LANDS IN A RESOURCE ARN IS CHECKED FIRST, because one carrying a `*`
// would widen the grant to everything it matches, and a pool naming nothing to launch
// into or from would grant a launch that can never succeed.
func TestThePoolGrantRefusesWhatWouldWidenIt(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*Pool){
		"a wildcard subnet":  func(p *Pool) { p.SubnetID = "subnet-*" },
		"a wildcard group":   func(p *Pool) { p.SecurityGroupIDs = []string{"sg-*"} },
		"a wildcard image":   func(p *Pool) { p.Images = []string{"ami-*"} },
		"a wildcard region":  func(p *Pool) { p.Region = "*" },
		"no group":           func(p *Pool) { p.SecurityGroupIDs = nil },
		"no image":           func(p *Pool) { p.Images = nil },
		"a role that is not": func(p *Pool) { p.InstanceProfileRoleARN = "arn:aws:iam::123456789012:user/x" },
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
