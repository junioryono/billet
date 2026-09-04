package main

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/awspolicy"
)

// `billet ami` DISPATCHES TO THE SUBCOMMAND THAT WAS TYPED.
//
// A one-subcommand command grew a second one, and the shape it grew from — an
// `args[0] != "build"` guard — accepts nothing else. What this asserts is that
// each name reaches its own command and an unknown one is refused, because the
// alternative failure is silent in the worst direction: `billet ami verify` that
// falls through to the builder would launch a paid machine and try to build an
// image named after nothing.
//
// EACH CASE STOPS AT ITS OWN FIRST REFUSAL, which is what proves where it landed
// without any of these touching AWS: build refuses without --base-image, and
// verify refuses without an image id.
func TestAMIDispatchesToTheSubcommandThatWasTyped(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "no subcommand",
			args: nil,
			want: "usage: billet ami <build|verify>",
		},
		{
			name: "an unknown subcommand",
			args: []string{"publish"},
			want: "usage: billet ami <build|verify>",
		},
		{
			name: "build, which needs a base image",
			args: []string{"build"},
			want: "--base-image is required",
		},
		{
			name: "verify, which needs an image id",
			args: []string{"verify"},
			want: "usage: billet ami verify <ami-id>",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := cmdAMI(t.Context(), tc.args)
			if err == nil {
				t.Fatalf("billet ami %v returned success; it should have refused before "+
					"launching anything", tc.args)
			}

			// THE SPECIFIC REFUSAL, not merely an error. Every one of these cases
			// errors under a dispatch that ignores the subcommand entirely — build's
			// missing --base-image would answer for all four — so asserting "an error
			// came back" would agree with exactly the bug this is about.
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("billet ami %v said %q, want it to contain %q", tc.args, err, tc.want)
			}
		})
	}
}

// THE BUILDER'S OWNER TAG CARRIES THE DEPLOYMENT, AND THE POLICY IS WHY.
//
// `billet init iam --deployment <id> --builder` renders statements that admit
// `billet-ami-build-<id>-*` and nothing else, so the value this stamps decides
// whether the build is authorized at all. Before issue #56 it stamped
// `billet-ami-build-<name>`, which carried no id: a value-scoped policy admitted
// EVERY deployment's builders, so two deployments in one account could image,
// terminate, read the console of and stamp each other's builds while their job
// instances stayed isolated.
//
// The pattern the policy matches and the value stamped here are one function
// apart (ec2.BuilderOwner and ec2.BuilderOwnerPattern), which is what stops them
// drifting again.
func TestTheBuilderOwnerCarriesTheDeployment(t *testing.T) {
	const id = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"

	got, err := builderOwner(filepath.Join(t.TempDir(), "none.yaml"), id, "billet-runner-1")
	if err != nil {
		t.Fatalf("builderOwner: %v", err)
	}

	if want := "billet-ami-build-" + id + "-billet-runner-1"; got != want {
		t.Errorf("the builder is tagged %q, want %q", got, want)
	}

	// AND THE POLICY THAT SCOPES IT ADMITS IT. Asserting the two AGREE, rather
	// than asserting each against a literal, is what makes a change to either
	// fail here instead of at a build in somebody's account. The policy is the
	// one `billet init iam --deployment <id> --builder` prints, read back through
	// the same rendering an operator would paste.
	policy, err := awspolicy.Inputs{Owner: id, NoCompute: true, Builder: true}.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	body, err := policy.JSON()
	if err != nil {
		t.Fatalf("render the policy: %v", err)
	}

	like, ok := stmtCondition(t, string(body), "BilletAMIBuilderSource")["StringLike"].(map[string]any)
	if !ok {
		t.Fatal("the builder source statement carries no owner condition")
	}

	admitted, ok := like["aws:ResourceTag/sh.billet.owner"].([]any)
	if !ok || len(admitted) != 1 {
		t.Fatalf("the builder source statement admits %v", like)
	}

	pattern, ok := admitted[0].(string)
	if !ok {
		t.Fatalf("the admitted value is %T, not a string", admitted[0])
	}

	// IAM's StringLike wildcard is the same one path.Match implements for a
	// single segment, and this value has no separators in it.
	matched, err := path.Match(pattern, got)
	if err != nil {
		t.Fatalf("match %q: %v", pattern, err)
	}

	if !matched {
		t.Errorf("the policy admits %q, which does not match the tag %q this stamps",
			pattern, got)
	}

	// AND IT MUST NOT ADMIT ANOTHER DEPLOYMENT'S BUILD, which is the whole of
	// issue #56 and the half a one-sided "does the policy match my tag" check
	// cannot see: the old bare prefix `billet-ami-build-*` matches this
	// deployment's folded tag perfectly well, so asserting only that the policy
	// admits us passes with the defect fully restored. Measured by removing the
	// deployment from the pattern and watching this fail.
	other := builderOwnerFor(t, "b1c2d3e4f5a69788796a5b4c3d2e1f00", "billet-runner-1")

	foreign, err := path.Match(pattern, other)
	if err != nil {
		t.Fatalf("match %q: %v", pattern, err)
	}

	if foreign {
		t.Errorf("the policy admits %q, which is another deployment's build: a "+
			"value-scoped grant that reaches it can image, terminate, read the console "+
			"of and stamp that deployment's builds", other)
	}
}

// builderOwnerFor is the owner a build of another deployment would stamp, taken
// from the same function rather than written out, so the two cannot drift.
func builderOwnerFor(t *testing.T, deployment, name string) string {
	t.Helper()

	owner, err := builderOwner(filepath.Join(t.TempDir(), "none.yaml"), deployment, name)
	if err != nil {
		t.Fatalf("builderOwner: %v", err)
	}

	return owner
}

// AN ID FROM THE CONFIG'S STATE DIRECTORY IS THE DEFAULT, so the policy an
// operator generated from that same config and the tag this stamps agree without
// two flags to keep in step.
func TestTheBuilderOwnerReadsTheDeploymentOnDisk(t *testing.T) {
	const id = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"

	dir := t.TempDir()
	stateDir := filepath.Join(dir, "server")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "deployment-id"), []byte(id), 0o600); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "billet.yaml")
	if err := os.WriteFile(cfgPath, []byte(amiNodeConfig(stateDir)), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := builderOwner(cfgPath, "", "billet-runner-1")
	if err != nil {
		t.Fatalf("builderOwner: %v", err)
	}

	if want := "billet-ami-build-" + id + "-billet-runner-1"; got != want {
		t.Errorf("the builder is tagged %q, want %q", got, want)
	}
}

// WITH NO ID ANYWHERE THE ACCOUNT-WIDE FORM IS STAMPED, AND SAID.
//
// That value is admitted only by an --account-wide policy, so a build run from a
// laptop with no config against a value-scoped grant is denied at RunInstances
// with nothing naming the reason. The note is the naming.
func TestTheBuilderOwnerSaysWhenItHasNoDeployment(t *testing.T) {
	var owner string

	printed := captureStderr(t, func() {
		var err error
		owner, err = builderOwner(filepath.Join(t.TempDir(), "none.yaml"), "", "billet-runner-1")
		if err != nil {
			t.Fatalf("builderOwner: %v", err)
		}
	})

	if owner != "billet-ami-build-billet-runner-1" {
		t.Errorf("with no deployment the owner is %q, want the account-wide form", owner)
	}

	for _, want := range []string{"no deployment identity", "--deployment", "will NOT admit"} {
		if !strings.Contains(printed, want) {
			t.Errorf("the note must carry %q:\n%s", want, printed)
		}
	}
}

// A MALFORMED --deployment IS REFUSED BY NAME rather than folded in, because a
// value that is not the deployment's id produces a tag no policy admits and a
// build that fails at its first call.
func TestTheBuilderOwnerRefusesAMalformedDeployment(t *testing.T) {
	_, err := builderOwner(filepath.Join(t.TempDir(), "none.yaml"), "NOT-AN-ID", "billet-runner-1")
	if err == nil || !strings.Contains(err.Error(), "--deployment") {
		t.Fatalf("a malformed deployment must be refused by name, got %v", err)
	}
}

// amiNodeConfig is an ec2 node config naming the given state directory, which is
// the only thing builderOwner reads out of one. A node is the shape a build most
// often runs beside: `billet init hybrid --builder` puts the grant on the
// controller, and the controller carries the node block.
func amiNodeConfig(stateDir string) string {
	return `
node:
  server_addr: 127.0.0.1:7717
  name: aws-1
  provider: ec2
  state_dir: ` + stateDir + `
  max_vcpu: 64
  max_memory: 256GiB
  ec2:
    region: us-west-2
    subnet_id: subnet-0abc
    security_group_ids: [sg-0abc]
    instance_types:
      - type: c7i.2xlarge
        vcpu: 8
        memory: 16GiB
        price_usd_per_hour: 0.34
`
}
