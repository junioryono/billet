package config

import (
	"strings"
	"testing"
	"time"
)

const poolBlock = `  pool:
    join_server: 10.77.0.10:7717
    boot_prelude: /etc/billet/pool-prelude.sh
    architectures:
      x64:
        floor: 1
        max: 3
        order:
          - {source: aws-metal, shape: z1d.metal}
          - {source: phoenixnap, shape: s2.c2.small, location: PHX}
      arm64:
        floor: 0
        max: 2
        order:
          - {source: aws-metal, shape: m7gd.metal, location: us-west-2a}
    aws_metal:
      region: us-west-2
      subnet_id: subnet-0abc
      availability_zone: us-west-2a
      security_group_ids: [sg-0abc]
      images: {x64: ami-x64, arm64: ami-arm}
      shapes:
        - {type: z1d.metal, arch: x64}
        - {type: m7gd.metal, arch: arm64}
    phoenixnap:
      client_id_env: PNAP_CLIENT_ID
      client_secret_env: PNAP_CLIENT_SECRET
      shapes:
        - {type: s2.c2.small, arch: x64}
`

func poolConfig(block string) string {
	return "github:\n  org: acme\n  app_id: 1\n  installation_id: 2\n  private_key_path: /tmp/key.pem\nserver:\n" +
		"  state_dir: /srv/billet\n  max_vcpu: 4\n  max_memory: 8GiB\n" + block
}

func TestAPoolBlockLoads(t *testing.T) {
	t.Parallel()
	cfg, err := loadServer(t, poolConfig(poolBlock))
	if err != nil {
		t.Fatalf("a valid pool: %v", err)
	}
	p := cfg.Server.Pool
	if p == nil || p.Architectures[ArchX64].Max != 3 || len(p.Architectures[ArchX64].Order) != 2 ||
		p.AWSMetal.Images[ArchARM64] != "ami-arm" || p.PhoenixNAP.ClientSecretEnv != "PNAP_CLIENT_SECRET" {
		t.Fatalf("the block did not reach the config: %+v", p)
	}
	if p.PoolInterval() != DefaultPoolInterval || p.PoolRefusalBackoff() != DefaultPoolRefusalBackoff {
		t.Fatalf("defaults = %v, %v", p.PoolInterval(), p.PoolRefusalBackoff())
	}
	if a, ok := p.ShapeArch(PoolSourcePhoenixNAP, "s2.c2.small"); !ok || a != ArchX64 {
		t.Fatalf("shape arch = %v, %t", a, ok)
	}
	withTimes := strings.Replace(poolBlock, "    boot_prelude:", "    interval: 1m\n    refusal_backoff: 1h\n    boot_prelude:", 1)
	cfg, err = loadServer(t, poolConfig(withTimes))
	if err != nil || cfg.Server.Pool.PoolInterval() != time.Minute || cfg.Server.Pool.PoolRefusalBackoff() != time.Hour {
		t.Fatalf("set durations: %v", err)
	}
}

// EACH REFUSAL IS ONE CHANGE TO A VALID BLOCK, so a refusal that fires for another
// reason does not pass for this one.
func TestAPoolBlockThatCannotWorkIsRefused(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ from, to, want string }{
		"no join server":          {"join_server: 10.77.0.10:7717", "join_server: ''", "join_server"},
		"a wildcard join":         {"join_server: 10.77.0.10:7717", "join_server: 0.0.0.0:7717", "join_server"},
		"a relative prelude":      {"boot_prelude: /etc/billet/pool-prelude.sh", "boot_prelude: prelude.sh", "boot_prelude"},
		"a bad interval":          {"    boot_prelude:", "    interval: soon\n    boot_prelude:", "interval"},
		"a negative floor":        {"floor: 1", "floor: -1", "must not be negative"},
		"max below the floor":     {"floor: 1\n        max: 3", "floor: 4\n        max: 3", "below its floor"},
		"no max":                  {"max: 2", "max: 0", "at least 1"},
		"an unknown source":       {"{source: phoenixnap, shape: s2.c2.small, location: PHX}", "{source: hetzner, shape: x}", "must be aws-metal or phoenixnap"},
		"an undeclared shape":     {"{source: aws-metal, shape: z1d.metal}", "{source: aws-metal, shape: c7i.metal-24xl}", "not one of aws-metal's declared shapes"},
		"a shape of another arch": {"{source: aws-metal, shape: m7gd.metal, location: us-west-2a}", "{source: aws-metal, shape: z1d.metal}", "is x64, not arm64"},
		"another zone":            {"location: us-west-2a}", "location: us-west-2b}", "subnet's zone"},
		"no phoenixnap location":  {"{source: phoenixnap, shape: s2.c2.small, location: PHX}", "{source: phoenixnap, shape: s2.c2.small}", "location is required for phoenixnap"},
		"no image for the arch":   {"images: {x64: ami-x64, arm64: ami-arm}", "images: {x64: ami-x64}", "names no arm64 image"},
		"no zone":                 {"availability_zone: us-west-2a", "availability_zone: ''", "availability_zone is required"},
		"no security group":       {"security_group_ids: [sg-0abc]", "security_group_ids: []", "security_group_ids"},
		"a pasted secret":         {"client_secret_env: PNAP_CLIENT_SECRET", "client_secret_env: 'abc-123=xyz'", "must name an environment variable"},
		"one variable for both":   {"client_secret_env: PNAP_CLIENT_SECRET", "client_secret_env: PNAP_CLIENT_ID", "one variable for both"},
		"a shape declared twice":  {"        - {type: m7gd.metal, arch: arm64}", "        - {type: m7gd.metal, arch: arm64}\n        - {type: m7gd.metal, arch: arm64}", "declared twice"},
		"an unknown arch shape":   {"{type: s2.c2.small, arch: x64}", "{type: s2.c2.small, arch: sparc}", "not an architecture billet knows"},
		"no source configured":    {"    aws_metal:", "    unused_aws_metal:", "field unused_aws_metal not found"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(poolBlock, c.from) {
				t.Fatalf("the fixture has no %q", c.from)
			}
			_, err := loadServer(t, poolConfig(strings.Replace(poolBlock, c.from, c.to, 1)))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want it to say %q", err, c.want)
			}
		})
	}
}

func TestAPoolWithNoSourceOrNoArchitectureIsRefused(t *testing.T) {
	t.Parallel()
	block := "  pool:\n    join_server: 10.0.0.1:7717\n    boot_prelude: /etc/p.sh\n"
	_, err := loadServer(t, poolConfig(block))
	if err == nil || !strings.Contains(err.Error(), "names no source") || !strings.Contains(err.Error(), "architectures is empty") {
		t.Fatalf("got %v", err)
	}
}
