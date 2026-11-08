package setup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// poolServerConfig writes a control plane's config whose pool orders from AWS metal,
// with no node block, and returns its path. profile is the aws_metal instance_profile
// line, or "".
func poolServerConfig(t *testing.T, profile string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "billet.yaml")
	body := `github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: /tmp/key.pem
server:
  state_dir: ` + t.TempDir() + `
  max_vcpu: 4
  max_memory: 8GiB
  listen: 10.77.0.10:7717
  bootstrap_listen: 10.77.0.10:7718
  pool:
    join_server: 10.77.0.10:7717
    boot_prelude: /etc/billet/pool-prelude.sh
    architectures:
      x64:
        floor: 0
        max: 2
        order:
          - {source: aws-metal, shape: z1d.metal}
      arm64:
        floor: 0
        max: 2
        order:
          - {source: aws-metal, shape: m7gd.metal}
    aws_metal:
      region: us-west-2
      subnet_id: subnet-0abc
      availability_zone: us-west-2a
      security_group_ids: [sg-0abc]
` + profile + `      images: {x64: ami-0a1b, arm64: ami-0c2d}
      shapes:
        - {type: z1d.metal, arch: x64}
        - {type: m7gd.metal, arch: arm64}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// `billet init iam --pool` PRINTS THE CONTROL PLANE'S POOL GRANT FROM THE CONFIG, with
// no node block: the pool statements, scoped to the deployment, the pool's network and
// both architectures' images, and nothing of a node's runtime.
func TestInitIAMPoolPrintsTheControlPlanesPoolGrant(t *testing.T) {
	t.Parallel()
	path := poolServerConfig(t, "      instance_profile: billet-pool-host\n")

	var err error
	out := capture(t, func() {
		err = Init(t.Context(), processEnv(), []string{
			"iam", "--config", path, "--pool", "--deployment", testDeployID,
			"--role-arn", "arn:aws:iam::123456789012:role/billet-pool-host",
		})
	})
	if err != nil {
		t.Fatalf("init iam --pool: %v", err)
	}
	got := parsePolicy(t, out)
	want := []string{"BilletPoolRead", "BilletPoolLaunchFrom", "BilletPoolLaunchHost", "BilletPoolTag",
		"BilletPoolTerminate", "BilletPoolConsole", "BilletPoolPassRole"}
	if !slices.Equal(got, want) {
		t.Fatalf("init iam --pool printed %v, want %v", got, want)
	}
	for _, need := range []string{
		"arn:aws:ec2:us-west-2:*:subnet/subnet-0abc", "arn:aws:ec2:us-west-2:*:security-group/sg-0abc",
		"arn:aws:ec2:us-west-2::image/ami-0a1b", "arn:aws:ec2:us-west-2::image/ami-0c2d", testDeployID,
	} {
		if !strings.Contains(out, need) {
			t.Errorf("the pool grant does not name %s:\n%s", need, out)
		}
	}

	if cond := stmtCondition(t, out, "BilletPoolTerminate"); cond == nil {
		t.Error("the pool's terminate carries no condition")
	}
}

// --pool REFUSES WHAT IT CANNOT SCOPE: a config with no AWS pool, an instance profile
// without the role it contains (or a role with no profile), and a node's flags.
func TestInitIAMPoolRefusesWhatItCannotScope(t *testing.T) {
	t.Parallel()
	noPool := ec2NodeConfig(t)
	withProfile := poolServerConfig(t, "      instance_profile: billet-pool-host\n")
	plain := poolServerConfig(t, "")
	for name, args := range map[string][]string{
		"a config with no aws_metal":        {"--config", noPool, "--pool", "--deployment", testDeployID},
		"a profile without its role":        {"--config", withProfile, "--pool", "--deployment", testDeployID},
		"a role with no profile":            {"--config", plain, "--pool", "--deployment", testDeployID, "--role-arn", "arn:aws:iam::123456789012:role/x"},
		"a node's flag":                     {"--config", plain, "--pool", "--deployment", testDeployID, "--builder"},
		"no deployment and no account-wide": {"--config", plain, "--pool"},
	} {
		var err error
		capture(t, func() { err = Init(t.Context(), processEnv(), append([]string{"iam"}, args...)) })
		if err == nil {
			t.Errorf("init iam --pool with %s printed a policy", name)
		}
	}
}
