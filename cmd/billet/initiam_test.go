package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ec2NodeConfig writes a loadable ec2 node config with cache, spot and an
// instance profile, and returns its path. It mirrors the node-only shape the
// cloud-check tests use.
// testDeployID is a valid 32-hex deployment identity for the value-mode tests.
const testDeployID = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"

func ec2NodeConfig(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "billet.yaml")
	body := `
node:
  server_addr: 127.0.0.1:7717
  name: aws-1
  provider: ec2
  state_dir: ` + t.TempDir() + `
  max_vcpu: 64
  max_memory: 256GiB
  site: aws-us-west-2
  ec2:
    region: us-west-2
    subnet_id: subnet-0abc
    security_group_ids: [sg-0abc]
    untrusted_security_group_ids: [sg-0def]
    instance_profile: billet-node
    spot: true
    interruption_queue_url: https://sqs.us-west-2.amazonaws.com/123456789012/aws-1
    instance_types:
      - type: c7i.2xlarge
        vcpu: 8
        memory: 16GiB
        price_usd_per_hour: 0.34
  ebs_s3:
    region: us-west-2
    availability_zone: us-west-2a
    bucket: billet-cache-example
    prefix: production
    kms_key_id: arn:aws:kms:us-west-2:123456789012:key/abcd
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}

// parsePolicy decodes what init iam printed and returns its statement ids.
func parsePolicy(t *testing.T, out string) []string {
	t.Helper()

	var doc struct {
		Statement []struct {
			Sid string `json:"Sid"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("init iam did not print valid JSON: %v\n%s", err, out)
	}

	sids := make([]string, len(doc.Statement))
	for i, s := range doc.Statement {
		sids[i] = s.Sid
	}

	return sids
}

// `billet init iam` PRINTS THE POLICY THE CONFIG NEEDS. A config that declares
// cache, spot and an instance profile gets those statements; --builder adds the
// AMI-build permission. What the operator applies matches what the node exercises.
func TestInitIAMDerivesThePolicyFromTheConfig(t *testing.T) {
	path := ec2NodeConfig(t)

	var err error
	out := capture(t, func() {
		err = cmdInit(t.Context(), []string{
			"iam", "--config", path, "--builder", "--deployment", testDeployID,
			"--role-arn", "arn:aws:iam::123456789012:role/billet-node",
		})
	})
	if err != nil {
		t.Fatalf("init iam: %v", err)
	}

	got := parsePolicy(t, out)
	for _, want := range []string{
		"BilletRuntimeRead", "BilletRuntimeTerminate", "BilletCacheCreateVolume", "BilletCacheCloneSource",
		"BilletCacheSnapshotSource", "BilletCacheSnapshotCreate",
		"BilletCacheObjects",
		"BilletCacheKMSUse", "BilletCacheKMSGrant", "BilletSpotInterruptions", "BilletPassRole",
		"BilletAMIBuilderSource", "BilletAMIBuilderImage",
	} {
		found := false
		for _, s := range got {
			if s == want {
				found = true

				break
			}
		}
		if !found {
			t.Errorf("the derived policy is missing %q; has %v", want, got)
		}
	}

	// The spot queue ARN is derived from the URL, scoped to the one queue.
	if !strings.Contains(out, "arn:aws:sqs:us-west-2:123456789012:aws-1") {
		t.Errorf("the interruption statement is not scoped to the derived queue ARN:\n%s", out)
	}
}

// A COMPUTE-ONLY EC2 CONFIG GETS ONLY THE RUNTIME STATEMENT. Without a cache
// block, no S3 or EBS is granted; without spot, no queue; without --builder, no
// CreateImage.
func TestInitIAMComputeOnlyIsRuntimeOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")
	body := `
node:
  server_addr: 127.0.0.1:7717
  name: aws-1
  provider: ec2
  state_dir: ` + t.TempDir() + `
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
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var err error
	out := capture(t, func() { err = cmdInit(t.Context(), []string{"iam", "--config", path, "--deployment", testDeployID}) })
	if err != nil {
		t.Fatalf("init iam: %v", err)
	}

	got := parsePolicy(t, out)
	want := []string{
		"BilletRuntimeRead", "BilletRuntimeLaunch", "BilletRuntimeDenyForeignSnapshot",
		"BilletRuntimeTag", "BilletRuntimeTerminate",
	}
	if len(got) != len(want) {
		t.Errorf("a compute-only config produced %v, want the runtime statements %v", got, want)
	}
	for _, s := range got {
		if !strings.HasPrefix(s, "BilletRuntime") {
			t.Errorf("a compute-only config leaked a non-runtime statement %q: %v", s, got)
		}
	}
}

// IT REFUSES A HOST-BACKED CONFIG, because an IAM policy is meaningless for a docker
// or firecracker node — and the refusal NAMES THE BACKENDS IT IS ABOUT.
//
// The gate asks `RunsOnHost` rather than comparing against ec2, so there are now two
// providers it accepts. A refusal that named neither left an operator with what their
// config is not, and nothing about what would work.
func TestInitIAMRefusesAHostBackedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")
	body := `
node:
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: ` + t.TempDir() + `
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := cmdInit(t.Context(), []string{"iam", "--config", path})
	if err == nil {
		t.Fatal("init iam produced a policy for a docker node")
	}
	for _, want := range []string{"docker", "ec2", "codebuild"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal names neither this config's backend nor the ones that "+
				"would work: %q missing from %v", want, err)
		}
	}
}

// --kms-key-arn WITHOUT A CACHE IS REFUSED, mirroring --role-arn: passing it when
// the config has no ebs_s3 block would silently do nothing.
func TestInitIAMRefusesKMSKeyARNWithoutCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")
	body := `
node:
  server_addr: 127.0.0.1:7717
  name: aws-1
  provider: ec2
  state_dir: ` + t.TempDir() + `
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
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := cmdInit(t.Context(), []string{
		"iam", "--config", path, "--account-wide", "--kms-key-arn", "arn:aws:kms:us-west-2:1:key/k",
	})
	if err == nil {
		t.Fatal("init iam accepted --kms-key-arn on a config with no cache")
	}
	if !strings.Contains(err.Error(), "--kms-key-arn") {
		t.Errorf("the refusal does not name --kms-key-arn: %v", err)
	}
}

// THE QUEUE ARN IS DERIVED FROM THE ACCOUNT AND NAME IN THE URL, with the region
// taken from the config — so a China, legacy or port-bearing host that config.Load
// accepts still yields a scoped ARN, and a malformed account or name is a stop
// rather than a grant widened to every queue.
func TestSQSQueueARN(t *testing.T) {
	for name, tc := range map[string]struct {
		url, partition, region, want string
	}{
		"standard": {
			"https://sqs.eu-central-1.amazonaws.com/999988887777/billet-spot", "aws", "eu-central-1",
			"arn:aws:sqs:eu-central-1:999988887777:billet-spot",
		},
		"china host": {
			"https://sqs.cn-north-1.amazonaws.com.cn/123456789012/q", "aws-cn", "cn-north-1",
			"arn:aws-cn:sqs:cn-north-1:123456789012:q",
		},
		"legacy host": {
			"https://us-west-2.queue.amazonaws.com/123456789012/billet-spot", "aws", "us-west-2",
			"arn:aws:sqs:us-west-2:123456789012:billet-spot",
		},
		"explicit port": {
			"https://vpce-abc.sqs.us-west-2.vpce.amazonaws.com:443/123456789012/billet-spot", "aws", "us-west-2",
			"arn:aws:sqs:us-west-2:123456789012:billet-spot",
		},
		"fifo": {
			"https://sqs.us-west-2.amazonaws.com/123456789012/billet.fifo", "aws", "us-west-2",
			"arn:aws:sqs:us-west-2:123456789012:billet.fifo",
		},
		"fifo at the 80-char limit": {
			"https://sqs.us-west-2.amazonaws.com/123456789012/" + strings.Repeat("a", 75) + ".fifo", "aws", "us-west-2",
			"arn:aws:sqs:us-west-2:123456789012:" + strings.Repeat("a", 75) + ".fifo",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := sqsQueueARN(tc.url, tc.partition, tc.region)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got != tc.want {
				t.Errorf("sqsQueueARN = %q, want %q", got, tc.want)
			}
		})
	}

	for name, bad := range map[string]string{
		"only one path part":  "https://sqs.us-west-2.amazonaws.com/onlyonepart",
		"non-numeric account": "https://sqs.us-west-2.amazonaws.com/notanaccount/q",
		"short account":       "https://sqs.us-west-2.amazonaws.com/123/q",
		"wildcard name":       "https://sqs.us-west-2.amazonaws.com/123456789012/q*",
		"empty name":          "https://sqs.us-west-2.amazonaws.com/123456789012/",
		"fifo over 80 chars":  "https://sqs.us-west-2.amazonaws.com/123456789012/" + strings.Repeat("a", 76) + ".fifo",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := sqsQueueARN(bad, "aws", "us-west-2"); err == nil {
				t.Errorf("a malformed queue URL %q was accepted", bad)
			}
		})
	}
}

// PASSROLE NEEDS THE ROLE ARN, WHICH THE CONFIG DOES NOT HOLD. A config that
// names an instance profile but no --role-arn is refused, because the
// instance-profile name is not the role ARN.
func TestInitIAMRequiresRoleARNForAnInstanceProfile(t *testing.T) {
	path := ec2NodeConfig(t)

	err := cmdInit(t.Context(), []string{"iam", "--config", path, "--account-wide"})
	if err == nil {
		t.Fatal("init iam scoped PassRole to an instance-profile name")
	}
	if !strings.Contains(err.Error(), "--role-arn") {
		t.Errorf("the refusal does not name --role-arn: %v", err)
	}
}

// A NON-ARN KMS KEY ID NEEDS THE FULL ARN. The EBS config may carry a bare id or
// alias, but IAM scoping needs the ARN, so init iam refuses until --kms-key-arn.
func TestInitIAMRequiresKMSKeyARN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")
	body := `
node:
  server_addr: 127.0.0.1:7717
  name: aws-1
  provider: ec2
  state_dir: ` + t.TempDir() + `
  max_vcpu: 64
  max_memory: 256GiB
  site: aws-us-west-2
  ec2:
    region: us-west-2
    subnet_id: subnet-0abc
    security_group_ids: [sg-0abc]
    instance_types:
      - type: c7i.2xlarge
        vcpu: 8
        memory: 16GiB
        price_usd_per_hour: 0.34
  ebs_s3:
    region: us-west-2
    availability_zone: us-west-2a
    bucket: billet-cache-example
    prefix: production
    kms_key_id: alias/billet
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	err := cmdInit(t.Context(), []string{"iam", "--config", path, "--account-wide"})
	if err == nil {
		t.Fatal("init iam scoped KMS to a non-ARN key id")
	}
	if !strings.Contains(err.Error(), "--kms-key-arn") {
		t.Errorf("the refusal does not name --kms-key-arn: %v", err)
	}
}

// THE POLICY AN OPERATOR APPLIES REFUSES A LAUNCH FROM A SNAPSHOT THIS DEPLOYMENT
// DOES NOT OWN, in whichever mode they asked for.
//
// ec2:RunInstances authorizes every snapshot a block-device mapping names and this
// command grants it on "*", so without the deny the role an operator pastes into
// their account can launch an instance with the control plane's ledger snapshot
// attached. The generator's own tests prove the statement is built; this proves it
// reaches the bytes `billet init iam` prints, which is what anybody actually
// applies.
func TestInitIAMDeniesALaunchFromAnUnownedSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want func(t *testing.T, cond map[string]any)
	}{
		{
			name: "account-wide", args: []string{"--account-wide"},
			want: func(t *testing.T, cond map[string]any) {
				t.Helper()

				null, ok := cond["Null"].(map[string]any)
				if !ok || null["aws:ResourceTag/sh.billet.owner"] != "true" {
					t.Errorf("--account-wide does not deny an UNTAGGED snapshot: %v", cond)
				}
			},
		},
		{
			name: "per-deployment", args: []string{"--deployment", testDeployID},
			want: func(t *testing.T, cond map[string]any) {
				t.Helper()

				se, ok := cond["StringNotEquals"].(map[string]any)
				if !ok || se["aws:ResourceTag/sh.billet.owner"] != testDeployID {
					t.Errorf("--deployment does not deny every snapshot but this deployment's: %v", cond)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := ec2NodeConfig(t)

			var err error

			args := append([]string{"iam", "--config", path,
				"--role-arn", "arn:aws:iam::123456789012:role/billet-node"}, tc.args...)
			out := capture(t, func() { err = cmdInit(t.Context(), args) })
			if err != nil {
				t.Fatalf("init iam: %v", err)
			}

			// EFFECT AND RESOURCE ARE READ HERE rather than through stmtCondition:
			// the same statement rendered Allow, or scoped to something other than a
			// snapshot, would carry an identical Condition and bound nothing.
			var doc struct {
				Statement []struct {
					Sid       string         `json:"Sid"`
					Effect    string         `json:"Effect"`
					Action    []string       `json:"Action"`
					Resource  []string       `json:"Resource"`
					Condition map[string]any `json:"Condition"`
				} `json:"Statement"`
			}
			if err := json.Unmarshal([]byte(out), &doc); err != nil {
				t.Fatalf("policy JSON: %v\n%s", err, out)
			}

			var found bool

			for _, s := range doc.Statement {
				if s.Sid != "BilletRuntimeDenyForeignSnapshot" {
					continue
				}

				found = true

				if s.Effect != "Deny" {
					t.Errorf("the snapshot boundary is %q, not a Deny", s.Effect)
				}
				if !slices.Equal(s.Action, []string{"ec2:RunInstances"}) {
					t.Errorf("the deny names %v, want exactly the launch action", s.Action)
				}
				if !slices.Equal(s.Resource, []string{"arn:aws:ec2:*:*:snapshot/*"}) {
					t.Errorf("the deny acts on %v, want every snapshot ARN spelling", s.Resource)
				}

				tc.want(t, s.Condition)
			}

			if !found {
				t.Errorf("the printed policy carries no launch boundary: %s", out)
			}
		})
	}
}

// stmtCondition decodes init iam's printed policy and returns one statement's
// Condition as a generic map.
func stmtCondition(t *testing.T, out, sid string) map[string]any {
	t.Helper()

	var doc struct {
		Statement []struct {
			Sid       string         `json:"Sid"`
			Condition map[string]any `json:"Condition"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("policy JSON: %v\n%s", err, out)
	}
	for _, s := range doc.Statement {
		if s.Sid == sid {
			return s.Condition
		}
	}

	t.Fatalf("no statement %q in the policy", sid)

	return nil
}

// --deployment SCOPES THE POLICY TO ONE DEPLOYMENT'S VALUE, so it isolates this
// deployment from any other billet in the account.
func TestInitIAMScopesToTheDeploymentID(t *testing.T) {
	path := ec2NodeConfig(t)

	var err error
	out := capture(t, func() {
		err = cmdInit(t.Context(), []string{
			"iam", "--config", path, "--deployment", testDeployID,
			"--role-arn", "arn:aws:iam::123456789012:role/billet-node",
		})
	})
	if err != nil {
		t.Fatalf("init iam: %v", err)
	}

	cond := stmtCondition(t, out, "BilletRuntimeTerminate")
	se, ok := cond["StringEquals"].(map[string]any)
	if !ok || se["aws:ResourceTag/sh.billet.owner"] != testDeployID {
		t.Errorf("Terminate is not value-scoped to the deployment id: %v", cond)
	}
}

// THE DEPLOYMENT ID IS READ FROM THE STATE DIRECTORY when --deployment is absent,
// so an operator whose control plane already minted it need not repeat it.
func TestInitIAMResolvesDeploymentFromState(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "deployment-id"), []byte(testDeployID+"\n"), 0o600); err != nil {
		t.Fatalf("seed deployment id: %v", err)
	}

	path := filepath.Join(t.TempDir(), "billet.yaml")
	body := `
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
      - {type: c7i.2xlarge, vcpu: 8, memory: 16GiB, price_usd_per_hour: 0.34}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var err error
	out := capture(t, func() { err = cmdInit(t.Context(), []string{"iam", "--config", path}) })
	if err != nil {
		t.Fatalf("init iam: %v", err)
	}

	cond := stmtCondition(t, out, "BilletRuntimeTerminate")
	if se, ok := cond["StringEquals"].(map[string]any); !ok || se["aws:ResourceTag/sh.billet.owner"] != testDeployID {
		t.Errorf("Terminate is not scoped to the state-dir deployment id: %v", cond)
	}
}

// --account-wide FALLS BACK TO TAG PRESENCE, for a single-deployment account.
func TestInitIAMAccountWideUsesPresence(t *testing.T) {
	path := ec2NodeConfig(t)

	var err error
	out := capture(t, func() {
		err = cmdInit(t.Context(), []string{
			"iam", "--config", path, "--account-wide",
			"--role-arn", "arn:aws:iam::123456789012:role/billet-node",
		})
	})
	if err != nil {
		t.Fatalf("init iam: %v", err)
	}

	cond := stmtCondition(t, out, "BilletRuntimeTerminate")
	if _, ok := cond["StringEquals"]; ok {
		t.Errorf("--account-wide produced a value condition, not presence: %v", cond)
	}
	if null, ok := cond["Null"].(map[string]any); !ok || null["aws:ResourceTag/sh.billet.owner"] != "false" {
		t.Errorf("--account-wide is not tag-presence conditioned: %v", cond)
	}
}

// WITHOUT AN ID OR --account-wide IT REFUSES rather than silently generating a
// per-account policy that a second deployment could abuse.
func TestInitIAMRefusesWithoutADeploymentID(t *testing.T) {
	path := ec2NodeConfig(t) // its state dir is empty (no deployment-id file)

	err := cmdInit(t.Context(), []string{"iam", "--config", path})
	if err == nil {
		t.Fatal("init iam generated a policy with no deployment identity and no --account-wide")
	}
	if !strings.Contains(err.Error(), "--deployment") || !strings.Contains(err.Error(), "--account-wide") {
		t.Errorf("the refusal does not name the ways to proceed: %v", err)
	}
}

// --account-wide OVERRIDES A STATE-DIR ID, and conflicting explicit choices are
// refused — the flag must do what its help says even on an enrolled host.
func TestInitIAMAccountWidePrecedence(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "deployment-id"), []byte(testDeployID+"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	path := filepath.Join(t.TempDir(), "billet.yaml")
	body := `
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
      - {type: c7i.2xlarge, vcpu: 8, memory: 16GiB, price_usd_per_hour: 0.34}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// --account-wide beats the on-disk id: presence, not value.
	var err error
	out := capture(t, func() { err = cmdInit(t.Context(), []string{"iam", "--config", path, "--account-wide"}) })
	if err != nil {
		t.Fatalf("init iam: %v", err)
	}
	if _, ok := stmtCondition(t, out, "BilletRuntimeTerminate")["StringEquals"]; ok {
		t.Error("--account-wide did not override the state-dir id; got a value condition")
	}
	// The read statement carries no condition at all, whatever the mode.
	if cond := stmtCondition(t, out, "BilletRuntimeRead"); cond != nil {
		t.Errorf("the describe statement is conditioned: %v", cond)
	}

	// --deployment and --account-wide together are refused.
	err = cmdInit(t.Context(), []string{"iam", "--config", path, "--account-wide", "--deployment", testDeployID})
	if err == nil {
		t.Fatal("init iam accepted both --deployment and --account-wide")
	}
	if !strings.Contains(err.Error(), "contradictory") {
		t.Errorf("the refusal does not explain the conflict: %v", err)
	}
}

// --deployment BEATS A DIFFERENT STATE-DIR ID, so an operator can override.
func TestInitIAMDeploymentFlagBeatsState(t *testing.T) {
	stateDir := t.TempDir()
	const onDisk = "11112222333344445555666677778888"
	if err := os.WriteFile(filepath.Join(stateDir, "deployment-id"), []byte(onDisk+"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	path := filepath.Join(t.TempDir(), "billet.yaml")
	body := `
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
      - {type: c7i.2xlarge, vcpu: 8, memory: 16GiB, price_usd_per_hour: 0.34}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	var err error
	out := capture(t, func() { err = cmdInit(t.Context(), []string{"iam", "--config", path, "--deployment", testDeployID}) })
	if err != nil {
		t.Fatalf("init iam: %v", err)
	}
	if se, ok := stmtCondition(t, out, "BilletRuntimeTerminate")["StringEquals"].(map[string]any); !ok || se["aws:ResourceTag/sh.billet.owner"] != testDeployID {
		t.Errorf("--deployment did not override the state-dir id: got %v", se)
	}
}
