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
		"  state_dir: /srv/billet\n  max_vcpu: 4\n  max_memory: 8GiB\n  listen: 10.77.0.10:7717\n" +
		"  bootstrap_listen: 10.77.0.10:7718\n" + block
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
	// NO IDLE_AFTER, NO SHRINK: a person releases a host, as before the key existed.
	if d, ok := p.PoolIdleAfter(); ok {
		t.Fatalf("a pool with no idle_after shrinks itself after %v", d)
	}
	withTimes := strings.Replace(poolBlock, "    boot_prelude:",
		"    interval: 1m\n    refusal_backoff: 1h\n    idle_after: 20m\n    boot_prelude:", 1)
	cfg, err = loadServer(t, poolConfig(withTimes))
	if err != nil || cfg.Server.Pool.PoolInterval() != time.Minute || cfg.Server.Pool.PoolRefusalBackoff() != time.Hour {
		t.Fatalf("set durations: %v", err)
	}
	if d, ok := cfg.Server.Pool.PoolIdleAfter(); !ok || d != 20*time.Minute {
		t.Fatalf("idle_after = %v, %t; want 20m", d, ok)
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
		"a short idle_after":      {"    boot_prelude:", "    idle_after: 4m59s\n    boot_prelude:", "idle_after"},
		"a bad idle_after":        {"    boot_prelude:", "    idle_after: later\n    boot_prelude:", "idle_after"},
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
		"a non-metal aws shape":   {"- {type: z1d.metal, arch: x64}", "- {type: c7i.2xlarge, arch: x64}", "not a bare-metal type"},
		"a bad bootstrap addr":    {"    boot_prelude:", "    bootstrap_addr: nowhere\n    boot_prelude:", "bootstrap_addr"},
		"an ipv6 wildcard join":   {"join_server: 10.77.0.10:7717", "join_server: '[::]:7717'", "join_server"},
		"an empty shape type":     {"{type: s2.c2.small, arch: x64}", "{type: '', arch: x64}", "type is required"},
		"an empty order":          {"        order:\n          - {source: aws-metal, shape: m7gd.metal, location: us-west-2a}", "        order: []", "order is empty"},
		"an unconfigured source": {
			"    phoenixnap:\n      client_id_env: PNAP_CLIENT_ID\n      client_secret_env: PNAP_CLIENT_SECRET\n      shapes:\n        - {type: s2.c2.small, arch: x64}\n",
			"", "which server.pool.phoenixnap does not configure",
		},
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

// AN ORDERED HOST JOINS ONLY THROUGH AN ENROLLMENT LISTENER, so a pool on a control plane
// that enrolls nothing over the network, or whose listener names no host to dial and
// no bootstrap_addr says one, is refused; otherwise the address it dials is the one
// given, else the listener's own.
func TestAPoolNeedsAnEnrollmentListenerItsHostsCanDial(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ from, to, want string }{
		"no enrollment listener": {"  bootstrap_listen: 10.77.0.10:7718\n", "", "needs server.bootstrap_listen"},
		"a wildcard listener":    {"bootstrap_listen: 10.77.0.10:7718", "bootstrap_listen: 0.0.0.0:7718", "bootstrap_addr is required"},
		"a loopback listener":    {"bootstrap_listen: 10.77.0.10:7718", "bootstrap_listen: 127.0.0.1:7718", "bootstrap_addr is required"},
		"an ipv6 loopback":       {"bootstrap_listen: 10.77.0.10:7718", "bootstrap_listen: '[::1]:7718'", "bootstrap_addr is required"},
		"localhost":              {"bootstrap_listen: 10.77.0.10:7718", "bootstrap_listen: localhost:7718", "bootstrap_addr is required"},
		"a loopback override":    {"    boot_prelude:", "    bootstrap_addr: 127.0.0.1:7718\n    boot_prelude:", "no wildcard and no loopback"},
		"a wildcard override":    {"    boot_prelude:", "    bootstrap_addr: '[::]:7718'\n    boot_prelude:", "no wildcard and no loopback"},
		"a zoned override":       {"    boot_prelude:", "    bootstrap_addr: '[::1%lo]:7718'\n    boot_prelude:", "no wildcard and no loopback"},
		"a zoned listener":       {"bootstrap_listen: 10.77.0.10:7718", "bootstrap_listen: '[fe80::1%eth0]:7718'", "bootstrap_addr is required"},
		"a loopback join server": {"join_server: 10.77.0.10:7717", "join_server: localhost:7717", "join_server"},
	} {
		raw := poolConfig(poolBlock)
		if !strings.Contains(raw, c.from) {
			t.Fatalf("%s: the fixture has no %q", name, c.from)
		}
		if _, err := loadServer(t, strings.Replace(raw, c.from, c.to, 1)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: got %v, want it to say %q", name, err, c.want)
		}
	}

	cfg, err := loadServer(t, poolConfig(poolBlock))
	if err != nil || cfg.Server.PoolEnrollAddr() != "10.77.0.10:7718" {
		t.Fatalf("the listener's own address: %v, %v", cfg, err)
	}
	given := strings.Replace(poolConfig(poolBlock), "bootstrap_listen: 10.77.0.10:7718", "bootstrap_listen: 0.0.0.0:7718", 1)
	given = strings.Replace(given, "    boot_prelude:", "    bootstrap_addr: billet.internal:7718\n    boot_prelude:", 1)
	cfg, err = loadServer(t, given)
	if err != nil || cfg.Server.PoolEnrollAddr() != "billet.internal:7718" {
		t.Fatalf("the address given: %v", err)
	}
}

// AN ENROLLMENT ADDRESS INHERITED FROM THE ROUTER IS HELD TO THE SAME RULE: a rented host
// dials it, and loopback there is the host itself.
func TestAPoolRefusesAnInheritedEnrollmentAddressItsHostsCannotDial(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{"localhost:443", "billet.localhost:443", "127.0.0.1:443", "[::1%lo]:443", "billet.example:443"} {
		s := &ServerConfig{BootstrapListen: "127.0.0.1:7718", TLSRouter: &TLSRouterConfig{EnrollAddr: addr},
			Pool: &PoolConfig{}}
		want := ""
		if addr == "billet.example:443" {
			want = addr
		}
		if got := s.PoolEnrollAddr(); got != want {
			t.Errorf("router enroll_addr %s: the pool's hosts dial %q, want %q", addr, got, want)
		}
	}
}
