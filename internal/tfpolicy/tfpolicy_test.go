package tfpolicy

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/awspolicy"
)

// The module commits TWO renderings and picks by whether the cache uses a
// customer-managed KMS key: with one, the policy carries a KMS statement scoped to
// the key; with the default EBS key, it carries none (the aws/ebs key's own policy
// authorizes the account). Both are the ec2 runtime + EBS/S3 cache + spot queue in
// PRESENCE mode (the deployment id is minted on the server's first run, unknown at
// apply), with no builder and no PassRole (those are added separately).
const policyDir = "../../terraform/modules/billet/modules/fleet-ec2/policy/"

func tfPolicyCases() map[string]awspolicy.Inputs {
	// UPPERCASE SENTINELS the terraform module string-substitutes. They are chosen
	// so no real value can ever contain one: a region, bucket, prefix and the
	// module's name are all validated lowercase, so an uppercase "TF..." token
	// cannot appear in any of them and no substitution can corrupt an operator's
	// input. The single prefix sentinel covers both the object resource
	// ("/TFPREFIX/") and the ListBucket condition ("TFPREFIX/*") in one replace.
	//
	// THREE RENDERINGS gated by the cache feature, so a compute-only or cacheless
	// node's role does not carry cache grants it never exercises. Spot is NOT in any
	// of these — the module adds the queue grant separately, scoped to the created
	// queue, only when spot is enabled.
	cache := &awspolicy.Cache{Bucket: "TFBUCKET", Prefix: "TFPREFIX"}
	cacheKMS := &awspolicy.Cache{
		Bucket: "TFBUCKET", Prefix: "TFPREFIX",
		KMSKeyARN: "arn:TFPARTITION:kms:TFREGION:000000000000:key/TFKMSKEY",
	}

	// Partition and DNSSuffix are ALSO sentinels the module substitutes, so the
	// committed rendering carries `arn:TFPARTITION:` and `ec2.TFREGION.TFDNSSUFFIX`
	// rather than the commercial partition and amazonaws.com — the module rewrites
	// them to `data.aws_partition.this.partition`/`.dns_suffix`, so one rendering
	// serves GovCloud and China too. Compute-only needs the PARTITION sentinel and
	// not the suffix one: it carries no kms:ViaService or PassRole service name, but
	// it does carry the snapshot ARN the launch boundary denies, and a rendering that
	// hard-coded `arn:aws:` there would deny nothing in GovCloud or China.
	return map[string]awspolicy.Inputs{
		"node-policy-compute.json": {Partition: "TFPARTITION"},
		"node-policy-cache.json": {
			Partition: "TFPARTITION", DNSSuffix: "TFDNSSUFFIX", Region: "TFREGION", Cache: cache,
		},
		"node-policy-cache-kms.json": {
			Partition: "TFPARTITION", DNSSuffix: "TFDNSSUFFIX", Region: "TFREGION", Cache: cacheKMS,
		},
	}
}

// THE CONTROL PLANE IS A DIFFERENT PRINCIPAL FROM THE NODE, so its renderings
// live in the control-plane child and carry NoCompute: a controller launches
// nothing, and the first version of this policy handed it ec2:RunInstances on
// the one host in a deployment that holds the GitHub App private key.
// TWO CHILDREN COMMIT THE SAME RENDERINGS, and that is a duplicate of generated
// bytes rather than a second source of truth: control-plane-ec2-sqlite and
// control-plane-postgres run the identical principal — one control plane, no
// compute — and differ only in where its ledger lives, which no IAM statement
// mentions. Both are pinned to the one generator here, so a change to
// internal/awspolicy that reaches one and not the other fails.
//
// A terraform module cannot read a file out of a sibling module, and reaching
// across with `${path.module}/../` would couple two children the README calls
// independently composable. So the bytes are copied and this test is what keeps
// them equal.
var controlPlanePolicyDirs = []string{
	"../../terraform/modules/billet/modules/control-plane-ec2-sqlite/policy/",
	"../../terraform/modules/billet/modules/control-plane-postgres/policy/",
}

func controlPlanePolicyCases() map[string]awspolicy.Inputs {
	backup := &awspolicy.Backup{Bucket: "TFBUCKET", Prefix: "TFPREFIX"}
	backupKMS := &awspolicy.Backup{
		Bucket: "TFBUCKET", Prefix: "TFPREFIX",
		KMSKeyARN: "arn:TFPARTITION:kms:TFREGION:000000000000:key/TFKMSKEY",
	}

	return map[string]awspolicy.Inputs{
		"backup-policy.json": {
			Partition: "TFPARTITION", DNSSuffix: "TFDNSSUFFIX", Region: "TFREGION",
			NoCompute: true, Backup: backup,
		},
		"backup-policy-kms.json": {
			Partition: "TFPARTITION", DNSSuffix: "TFDNSSUFFIX", Region: "TFREGION",
			NoCompute: true, Backup: backupKMS,
		},
	}
}

// TestTerraformControlPlanePolicyMatchesGenerator is the node test's sibling.
//
// ONE GENERATOR, and the module consumes its output rather than restating it in
// HCL: what billet's code performs and what its policy grants cannot disagree if
// only one place decides. The first version of the backup grant was hand-written
// jsonencode in the module, which is exactly the second source of truth this
// drift test exists to prevent.
func TestTerraformControlPlanePolicyMatchesGenerator(t *testing.T) {
	for name, in := range controlPlanePolicyCases() {
		for _, dir := range controlPlanePolicyDirs {
			t.Run(dir+name, func(t *testing.T) {
				policy, err := in.Build()
				if err != nil {
					t.Fatalf("Build: %v", err)
				}

				got, err := policy.JSON()
				if err != nil {
					t.Fatalf("JSON: %v", err)
				}

				got = append(got, '\n')

				path := dir + name
				if os.Getenv("UPDATE_TF_POLICY") == "1" {
					if err := os.WriteFile(path, got, 0o644); err != nil {
						t.Fatalf("write: %v", err)
					}

					return
				}

				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read %s: %v (regenerate with UPDATE_TF_POLICY=1)", path, err)
				}

				if !bytes.Equal(got, want) {
					t.Errorf("terraform-aws-billet's %s no longer matches internal/awspolicy. If "+
						"intended, regenerate with UPDATE_TF_POLICY=1 and review the diff.\nGot:\n%s",
						path, got)
				}
			})
		}
	}
}

// AND NEITHER RENDERING MAY EVER CARRY A DELETE OR A COMPUTE PERMISSION.
//
// The drift test above pins the bytes, which catches a change; this says what
// the bytes must never contain, which catches a change somebody regenerated
// without reading. A backup credential that can destroy its own history is not
// an off-site copy, and a control plane launches nothing.
func TestTheCommittedBackupPolicyGrantsNoDeleteAndNoCompute(t *testing.T) {
	for name := range controlPlanePolicyCases() {
		for _, dir := range controlPlanePolicyDirs {
			t.Run(dir+name, func(t *testing.T) {
				body, err := os.ReadFile(dir + name)
				if err != nil {
					t.Fatalf("read: %v (regenerate with UPDATE_TF_POLICY=1)", err)
				}

				var doc struct {
					Statement []struct {
						Sid    string
						Action []string
					}
				}

				if err := json.Unmarshal(body, &doc); err != nil {
					t.Fatalf("parse %s: %v", name, err)
				}

				if len(doc.Statement) == 0 {
					t.Fatal("the committed policy is empty, so the checks below prove nothing")
				}

				for _, s := range doc.Statement {
					for _, action := range s.Action {
						if strings.Contains(strings.ToLower(action), "delete") {
							t.Errorf("%s grants %s", s.Sid, action)
						}

						if strings.HasPrefix(action, "ec2:") {
							t.Errorf("%s grants %s to a principal that runs no compute", s.Sid, action)
						}
					}
				}
			})
		}
	}
}

// THE CODEBUILD FLEET IS A THIRD PRINCIPAL SET, and it is two renderings rather
// than one because a codebuild deployment has two roles that must not be confused.
//
// The NODE starts and stops builds and stages the single-use runner registration.
// The BUILD's service role runs INSIDE the compute that executes somebody's
// workflow, so every permission it holds is a permission that workflow holds — it
// reads one parameter and writes logs, and nothing else. Rendering them separately
// is what makes the second one readable at a glance, and the assertion below is what
// keeps a capability from creeping into it.
//
// NoCompute is set on both: a codebuild node launches no EC2 instances, and the
// first version of the backup policy is the record of what happens when a principal
// gets the ec2 runtime statements because nothing said not to.
const codeBuildPolicyDir = "" +
	"../../terraform/modules/billet/modules/fleet-codebuild/policy/"

func codeBuildPolicyCases() map[string]awspolicy.Inputs {
	// The same UPPERCASE sentinels the fleet-ec2 renderings use, and for the same
	// reason: a region, a project name and a parameter path are all validated
	// lowercase, so an uppercase "TF..." token cannot appear in an operator's input
	// and no substitution can corrupt it.
	const (
		project = "arn:TFPARTITION:codebuild:TFREGION:TFACCOUNT:project/TFPROJECT"
		fleet   = "arn:TFPARTITION:codebuild:TFREGION:TFACCOUNT:fleet/TFFLEET"
		key     = "arn:TFPARTITION:kms:TFREGION:TFACCOUNT:key/TFKMSKEY"
		path    = "/TFPARAMPATH"
		group   = "arn:TFPARTITION:logs:TFREGION:TFACCOUNT:log-group:TFLOGGROUP"
	)

	// FOUR NODE RENDERINGS, BECAUSE THE FLEET AND THE KEY ARE INDEPENDENT and the
	// first version committed only two. A deployment with a fleet and no
	// customer-managed key then fell through to the fleetless rendering and got NO
	// fleet grant — so `billet check` could not describe its own fleet, which is
	// where the capacity a macOS tier's macos_vm_limit should be set to comes from.
	// The terraform plan test is what caught it, which is the argument for asserting
	// on the rendered policy rather than on the selection logic.
	return map[string]awspolicy.Inputs{
		// On-demand Linux, account key: the inexpensive default the root example
		// leads with.
		"node-policy.json": {
			Partition: "TFPARTITION", DNSSuffix: "TFDNSSUFFIX", Region: "TFREGION",
			NoCompute: true,
			CodeBuild: &awspolicy.CodeBuild{ProjectARN: project, ParameterPath: path},
		},
		"node-policy-kms.json": {
			Partition: "TFPARTITION", DNSSuffix: "TFDNSSUFFIX", Region: "TFREGION",
			NoCompute: true,
			CodeBuild: &awspolicy.CodeBuild{
				ProjectARN: project, ParameterPath: path, KMSKeyARN: key,
			},
		},
		"node-policy-fleet.json": {
			Partition: "TFPARTITION", DNSSuffix: "TFDNSSUFFIX", Region: "TFREGION",
			NoCompute: true,
			CodeBuild: &awspolicy.CodeBuild{
				ProjectARN: project, FleetARN: fleet, ParameterPath: path,
			},
		},
		// A reserved fleet and a per-deployment key, which is the macOS shape.
		"node-policy-fleet-kms.json": {
			Partition: "TFPARTITION", DNSSuffix: "TFDNSSUFFIX", Region: "TFREGION",
			NoCompute: true,
			CodeBuild: &awspolicy.CodeBuild{
				ProjectARN: project, FleetARN: fleet, ParameterPath: path, KMSKeyARN: key,
			},
		},
		"build-role-policy.json": {
			Partition: "TFPARTITION", DNSSuffix: "TFDNSSUFFIX", Region: "TFREGION",
			NoCompute:     true,
			CodeBuildRole: &awspolicy.CodeBuildRole{ParameterPath: path, LogGroupARN: group},
		},
		"build-role-policy-kms.json": {
			Partition: "TFPARTITION", DNSSuffix: "TFDNSSUFFIX", Region: "TFREGION",
			NoCompute: true,
			CodeBuildRole: &awspolicy.CodeBuildRole{
				ParameterPath: path, KMSKeyARN: key, LogGroupARN: group,
			},
		},
		// THE CONTROL PLANE'S GRANT OVER THE PATH: list and delete the registrations
		// a dead node left behind, on the ledger's authority. A third principal, and
		// the narrowest — no key, because it lists without decrypting; no build
		// action at all.
		"controller-sweep-policy.json": {
			Partition: "TFPARTITION", DNSSuffix: "TFDNSSUFFIX", Region: "TFREGION",
			NoCompute:      true,
			CodeBuildSweep: &awspolicy.CodeBuildSweep{ParameterPath: path, Account: "TFACCOUNT"},
		},
	}
}

// AND THE CONTROLLER'S SWEEP MAY NEVER READ A REGISTRATION, STAGE ONE, TOUCH A KEY,
// OR REACH CODEBUILD OR EC2.
//
// It is the one grant that lands on the principal holding the ledger and the App
// key, so what it must never contain is asserted on the committed bytes, the way
// the build role's is.
func TestTheControllerSweepPolicyCanOnlyListAndDelete(t *testing.T) {
	body, err := os.ReadFile(codeBuildPolicyDir + "controller-sweep-policy.json")
	if err != nil {
		t.Fatalf("read: %v (regenerate with UPDATE_TF_POLICY=1)", err)
	}

	var doc struct {
		Statement []struct {
			Sid      string
			Action   []string
			Resource []string
		}
	}

	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(doc.Statement) == 0 {
		t.Fatal("the committed policy is empty, so the checks below prove nothing")
	}

	for _, st := range doc.Statement {
		for _, action := range st.Action {
			lower := strings.ToLower(action)

			switch {
			case lower == "ssm:getparameter", lower == "ssm:getparameters", lower == "ssm:putparameter":
				t.Errorf("%s grants %s; the sweep lists names and deletes, and never reads or "+
					"stages a registration", st.Sid, action)
			case strings.HasPrefix(lower, "kms:"):
				t.Errorf("%s grants %s; a listing without decryption calls no key", st.Sid, action)
			case strings.HasPrefix(lower, "codebuild:"), strings.HasPrefix(lower, "ec2:"):
				t.Errorf("%s grants %s to the control plane", st.Sid, action)
			}
		}

		for _, r := range st.Resource {
			if r == "*" {
				t.Errorf("%s is scoped to \"*\" in a grant that lands on the controller", st.Sid)
			}
		}
	}
}

func TestTerraformCodeBuildPolicyMatchesGenerator(t *testing.T) {
	for name, in := range codeBuildPolicyCases() {
		t.Run(name, func(t *testing.T) {
			policy, err := in.Build()
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			got, err := policy.JSON()
			if err != nil {
				t.Fatalf("JSON: %v", err)
			}

			got = append(got, '\n')

			path := codeBuildPolicyDir + name
			if os.Getenv("UPDATE_TF_POLICY") == "1" {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}

				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v (regenerate with UPDATE_TF_POLICY=1)", path, err)
			}

			if !bytes.Equal(got, want) {
				t.Errorf("the fleet-codebuild module's %s no longer matches "+
					"internal/awspolicy. If intended, regenerate with UPDATE_TF_POLICY=1 "+
					"and review the diff.\nGot:\n%s", name, got)
			}
		})
	}
}

// AND THE BUILD'S OWN ROLE MAY NEVER START A BUILD, DELETE A PARAMETER, OR TOUCH EC2.
//
// The drift test above pins the bytes, which catches a change; this says what the
// bytes must never contain, which catches a change somebody regenerated without
// reading. This role runs inside the compute that executes a workflow, so every
// permission it holds is a permission arbitrary job code holds: one that could
// StartBuild would let a job launch runners billet never escrowed capacity for, and
// one that could DeleteParameter would let a job destroy another build's staged
// registration.
func TestTheBuildRoleCanNeitherStartABuildNorDeleteAParameter(t *testing.T) {
	for name := range codeBuildPolicyCases() {
		if !strings.HasPrefix(name, "build-role-") {
			continue
		}

		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(codeBuildPolicyDir + name)
			if err != nil {
				t.Fatalf("read: %v (regenerate with UPDATE_TF_POLICY=1)", err)
			}

			var doc struct {
				Statement []struct {
					Sid    string
					Action []string
				}
			}

			if err := json.Unmarshal(body, &doc); err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}

			if len(doc.Statement) == 0 {
				t.Fatal("the committed policy is empty, so the checks below prove nothing")
			}

			for _, st := range doc.Statement {
				for _, action := range st.Action {
					lower := strings.ToLower(action)

					switch {
					case strings.HasPrefix(lower, "codebuild:"):
						t.Errorf("%s grants %s to a role that runs inside a build", st.Sid, action)

					case strings.Contains(lower, "delete"):
						t.Errorf("%s grants %s; cleanup is the node's job, and a build that "+
							"could delete a parameter could destroy another build's "+
							"registration", st.Sid, action)

					case strings.HasPrefix(lower, "ec2:"):
						t.Errorf("%s grants %s to a principal that launches nothing",
							st.Sid, action)

					case lower == "ssm:putparameter":
						t.Errorf("%s grants %s; a build writes no registration, it reads the "+
							"one staged for it", st.Sid, action)
					}
				}
			}
		})
	}
}

// AND NOTHING IN THE BUILD ROLE IS SCOPED TO "*".
//
// This role RUNS THE WORKFLOW, so a wildcard resource in it is a capability handed to
// arbitrary job code. It was reachable and shipped: the logs statement fell back to
// "*" whenever node.codebuild named no log group, which is logs:CreateLogGroup on
// every group in the account. awspolicy now refuses an unnamed group outright and cmd
// derives CodeBuild's own default (/aws/codebuild/<project>), so there is always a
// group to name — this is what keeps the fallback from coming back.
//
// IT IS A SEPARATE TEST rather than another case in the one above, because that one's
// question is about ACTIONS and this one's is about RESOURCES; folding them together
// is how a wildcard resource stayed invisible beside a correct action list.
func TestNothingInTheBuildRoleIsScopedToEverything(t *testing.T) {
	cases := 0

	for name := range codeBuildPolicyCases() {
		if !strings.HasPrefix(name, "build-role-") {
			continue
		}

		cases++

		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(codeBuildPolicyDir + name)
			if err != nil {
				t.Fatalf("read: %v (regenerate with UPDATE_TF_POLICY=1)", err)
			}

			var doc struct {
				Statement []struct {
					Sid      string
					Action   []string
					Resource []string
				}
			}

			if err := json.Unmarshal(body, &doc); err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}

			if len(doc.Statement) == 0 {
				t.Fatal("the committed policy is empty, so the check below proves nothing")
			}

			for _, st := range doc.Statement {
				if len(st.Resource) == 0 {
					t.Errorf("%s names no resource at all, which IAM reads as no scope for a "+
						"role that runs inside a build", st.Sid)
				}

				for _, resource := range st.Resource {
					if resource == "*" {
						t.Errorf("%s is scoped to \"*\" in a role that runs inside a build, so "+
							"every action in it is one arbitrary job code holds account-wide",
							st.Sid)
					}
				}
			}
		})
	}

	// AND THE LOOP RAN, or an empty case set would make every assertion above vacuous
	// — the shape this repository has been caught by before.
	if cases == 0 {
		t.Fatal("no build-role rendering was examined, so this test proves nothing")
	}
}

// AND THE NODE'S ROLE MAY NEVER TOUCH EC2 OR CREATE A FLEET.
//
// A codebuild node launches no instances — NoCompute exists for exactly this — and
// nothing in billet creates, resizes or deletes a fleet, so granting either would put
// standing cost under the control of a node process.
func TestTheCodeBuildNodeRoleLaunchesNoInstancesAndOwnsNoFleet(t *testing.T) {
	for name := range codeBuildPolicyCases() {
		if !strings.HasPrefix(name, "node-policy") {
			continue
		}

		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(codeBuildPolicyDir + name)
			if err != nil {
				t.Fatalf("read: %v (regenerate with UPDATE_TF_POLICY=1)", err)
			}

			var doc struct {
				Statement []struct {
					Sid    string
					Action []string
				}
			}

			if err := json.Unmarshal(body, &doc); err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}

			if len(doc.Statement) == 0 {
				t.Fatal("the committed policy is empty, so the checks below prove nothing")
			}

			for _, st := range doc.Statement {
				for _, action := range st.Action {
					lower := strings.ToLower(action)

					switch {
					case strings.HasPrefix(lower, "ec2:"):
						t.Errorf("%s grants %s to a principal that launches no instances",
							st.Sid, action)

					case lower == "codebuild:createfleet" || lower == "codebuild:updatefleet" ||
						lower == "codebuild:deletefleet":
						t.Errorf("%s grants %s; a fleet is standing cost and billet never "+
							"creates or changes one", st.Sid, action)

					case lower == "codebuild:createproject" ||
						lower == "codebuild:updateproject" ||
						lower == "codebuild:deleteproject":
						t.Errorf("%s grants %s; terraform owns the project and billet owns the "+
							"builds", st.Sid, action)
					}
				}
			}
		})
	}
}

// AND EVERY NODE RENDERING REFUSES A LAUNCH FROM A SNAPSHOT NOBODY OWNS.
//
// The drift test below pins the bytes, which catches a change; this says what the
// bytes must never lose, which catches a change somebody regenerated without
// reading. ec2:RunInstances authorizes every snapshot a block-device mapping
// names, and the module grants it on "*" — so without this statement the fleet
// role can launch an instance with the control plane's ledger snapshot attached
// and read the deployment identity and the node-wire CA key off it. The renderings
// are in PRESENCE mode (the deployment id is minted on the server's first run,
// unknown at apply), so the condition here is the tag being absent.
//
// THE PARTITION SENTINEL IS PART OF THE ASSERTION: the module rewrites it, and a
// rendering that hard-coded `arn:aws:` would deny nothing at all in GovCloud or
// China while every byte comparison stayed green.
func TestTheCommittedNodePolicyDeniesAnUnownedSnapshotLaunch(t *testing.T) {
	cases := 0

	for name := range tfPolicyCases() {
		cases++

		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(policyDir + name)
			if err != nil {
				t.Fatalf("read: %v (regenerate with UPDATE_TF_POLICY=1)", err)
			}

			var doc struct {
				Statement []struct {
					Sid       string
					Effect    string
					Action    []string
					Resource  []string
					Condition map[string]map[string]any
				}
			}

			if err := json.Unmarshal(body, &doc); err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}

			var found bool

			for _, st := range doc.Statement {
				if st.Sid != "BilletRuntimeDenyForeignSnapshot" {
					continue
				}

				found = true

				if st.Effect != "Deny" {
					t.Errorf("%s has Effect %q; an Allow bounds nothing", st.Sid, st.Effect)
				}

				if !slices.Equal(st.Action, []string{"ec2:RunInstances"}) {
					t.Errorf("%s denies %v, want exactly the launch action", st.Sid, st.Action)
				}

				if !slices.Equal(st.Resource, []string{"arn:TFPARTITION:ec2:*:*:snapshot/*"}) {
					t.Errorf("%s acts on %v, want the partition-sentinel snapshot ARN with a "+
						"wildcard account", st.Sid, st.Resource)
				}

				if st.Condition["Null"]["aws:ResourceTag/sh.billet.owner"] != "true" {
					t.Errorf("%s does not require the owner tag to be ABSENT: %v",
						st.Sid, st.Condition)
				}
			}

			if !found {
				t.Errorf("%s carries no launch boundary, so the role it renders may launch an "+
					"instance with any snapshot in the account attached", name)
			}
		})
	}

	// AND THE LOOP RAN, or an empty case set makes every assertion above vacuous.
	if cases == 0 {
		t.Fatal("no node rendering was examined, so this test proves nothing")
	}
}

func TestTerraformPolicyMatchesGenerator(t *testing.T) {
	for name, in := range tfPolicyCases() {
		t.Run(name, func(t *testing.T) {
			policy, err := in.Build()
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			got, err := policy.JSON()
			if err != nil {
				t.Fatalf("JSON: %v", err)
			}
			got = append(got, '\n')

			path := policyDir + name
			if os.Getenv("UPDATE_TF_POLICY") == "1" {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}

				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v (regenerate with UPDATE_TF_POLICY=1)", path, err)
			}

			if !bytes.Equal(got, want) {
				t.Errorf("terraform-aws-billet's %s no longer matches internal/awspolicy. If "+
					"intended, regenerate with UPDATE_TF_POLICY=1 and review the diff.\nGot:\n%s",
					name, got)
			}
		})
	}
}
