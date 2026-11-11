package app

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// THE ACCEPTANCE LANE'S LIMIT ON A RENDERED PRELUDE LEAVES ROOM FOR THE REST OF THE BOOT:
// a prelude of exactly the size scripts/acceptance/matrix-host.sh accepts, wrapped and
// followed by the join's line with every init_arg the lane's pool hands its hosts and a
// full-length order, token and fingerprint, is a boot PoolBoot builds within EC2's user
// data. The scripts tests render the collection's own prelude under that limit, so
// between them a prelude that would not fit fails a pull request, never a dispatch.
func TestTheLanesPreludeLimitLeavesRoomForTheJoin(t *testing.T) {
	t.Parallel()

	script, err := os.ReadFile("../../scripts/acceptance/matrix-host.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`if \[ "\$size" -gt ([0-9]+) \]`).FindSubmatch(script)
	if m == nil {
		t.Fatal("matrix-host.sh names no limit on the rendered prelude")
	}
	limit, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatal(err)
	}

	prelude := strings.Repeat("true\n", limit/5) + strings.Repeat("#", limit%5)
	if len(prelude) != limit {
		t.Fatalf("a prelude of %d bytes, want %d", len(prelude), limit)
	}
	cfg := &config.Config{GitHub: &config.GitHubConfig{Org: "billet-testbed"}, Server: &config.ServerConfig{
		BootstrapListen: "10.77.0.10:7718", Pool: &config.PoolConfig{JoinServer: "10.77.0.10:7717",
			InitArgs: []string{"--kernel-image=/var/lib/billet/kernels/vmlinux", "--ceph-user=billet",
				"--ceph-keyring=/etc/ceph/ceph.client.billet.keyring", "--site=aws-us-west-2a",
				"--ebs-s3-region=us-west-2", "--ebs-s3-availability-zone=us-west-2a",
				"--ebs-s3-bucket=acceptance-cache-123456789012", "--ebs-s3-prefix=billet-cache",
				"--ebs-s3-kms-key-id=arn:aws:kms:us-west-2:123456789012:key/00000000-0000-0000-0000-000000000000",
				"--cache-listen=10.240.0.1:7718", "--cache-guest-endpoint=http://10.240.0.1:7718"}}}}
	boot, err := PoolBoot(cfg, prelude, "SHA256:"+strings.Repeat("A", 43), "p0123456789ab", strings.Repeat("a", 32))
	if err != nil {
		t.Fatalf("a %d-byte prelude, which the lane accepts, makes a boot the control plane refuses: %v", limit, err)
	}
	t.Logf("a %d-byte prelude makes a %d-byte boot of EC2's %d", limit, len(boot), maxBootPayload)
}
