package awspolicy

import (
	"strings"
	"testing"
)

// THE CONTROLLER'S SWEEP GRANT NAMES THE PATH AND ITS CHILDREN, AND NOTHING ELSE.
//
// GetParametersByPath is authorised against the hierarchy it is asked about and
// DeleteParameter against each parameter, so a rendering that named only the
// children would look scoped and list nothing, and one that named "*" would let
// the controller delete every parameter in the account.
func TestTheControllerSweepGrantIsScopedToThePathAndItsChildren(t *testing.T) {
	policy, err := Inputs{
		Region: "us-west-2", NoCompute: true,
		CodeBuildSweep: &CodeBuildSweep{ParameterPath: "/billet/linux/jit", Account: "123456789012"},
	}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if len(policy.Statement) != 1 {
		t.Fatalf("a sweep-only controller policy has %d statements, want exactly one", len(policy.Statement))
	}

	st := policy.Statement[0]

	wantResources := []string{
		"arn:aws:ssm:us-west-2:123456789012:parameter/billet/linux/jit",
		"arn:aws:ssm:us-west-2:123456789012:parameter/billet/linux/jit/*",
	}

	if len(st.Resource) != len(wantResources) {
		t.Fatalf("resources = %v, want %v", st.Resource, wantResources)
	}

	for i, want := range wantResources {
		if st.Resource[i] != want {
			t.Errorf("resource %d = %q, want %q", i, st.Resource[i], want)
		}
	}

	wantActions := map[string]bool{"ssm:GetParametersByPath": true, "ssm:DeleteParameter": true}
	for _, a := range st.Action {
		if !wantActions[a] {
			t.Errorf("the sweep grant carries %s, which listing and deleting under one path does not need", a)
		}

		delete(wantActions, a)
	}

	for missing := range wantActions {
		t.Errorf("the sweep grant is missing %s", missing)
	}
}

// AND IT MAY NEVER CARRY A READ, A WRITE, A KEY, A BUILD OR AN INSTANCE.
//
// The drift test pins the bytes; this says what the bytes must never contain, so a
// regeneration nobody read cannot widen the one principal that holds the ledger.
func TestTheControllerSweepGrantCanOnlyListAndDelete(t *testing.T) {
	policy, err := Inputs{
		Region: "us-west-2", NoCompute: true,
		CodeBuildSweep: &CodeBuildSweep{ParameterPath: "/billet/jit", Account: "123456789012"},
	}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if len(policy.Statement) == 0 {
		t.Fatal("the policy is empty, so the checks below prove nothing")
	}

	for _, st := range policy.Statement {
		for _, action := range st.Action {
			lower := strings.ToLower(action)

			switch {
			case lower == "ssm:getparameter", lower == "ssm:getparameters":
				t.Errorf("%s grants %s; the sweep never reads a registration", st.Sid, action)
			case lower == "ssm:putparameter":
				t.Errorf("%s grants %s; the sweep stages nothing", st.Sid, action)
			case strings.HasPrefix(lower, "kms:"):
				t.Errorf("%s grants %s; a listing without decryption calls no key", st.Sid, action)
			case strings.HasPrefix(lower, "codebuild:"), strings.HasPrefix(lower, "ec2:"):
				t.Errorf("%s grants %s to the control plane", st.Sid, action)
			}
		}

		for _, r := range st.Resource {
			if r == "*" {
				t.Errorf("%s is scoped to \"*\"", st.Sid)
			}
		}
	}
}

// A WIDENING INPUT IS REFUSED, not rendered: a wildcard in the path or a missing
// account is a grant that looks scoped and is not.
func TestTheControllerSweepGrantRefusesAWideningInput(t *testing.T) {
	cases := map[string]CodeBuildSweep{
		"wildcard path":   {ParameterPath: "/billet/*", Account: "123456789012"},
		"empty path":      {ParameterPath: "", Account: "123456789012"},
		"empty account":   {ParameterPath: "/billet/jit", Account: ""},
		"wildcard acct":   {ParameterPath: "/billet/jit", Account: "*"},
		"policy variable": {ParameterPath: "/billet/${aws:username}", Account: "123456789012"},
	}

	for name, sweep := range cases {
		t.Run(name, func(t *testing.T) {
			sweep := sweep

			if _, err := (Inputs{Region: "us-west-2", NoCompute: true, CodeBuildSweep: &sweep}).Build(); err == nil {
				t.Fatalf("%+v was rendered", sweep)
			}
		})
	}

	if _, err := (Inputs{NoCompute: true, CodeBuildSweep: &CodeBuildSweep{
		ParameterPath: "/billet/jit", Account: "123456789012",
	}}).Build(); err == nil {
		t.Fatal("a sweep grant with no region was rendered; a parameter ARN names one")
	}
}
