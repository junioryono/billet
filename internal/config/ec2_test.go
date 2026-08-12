package config

import (
	"strings"
	"testing"
)

// firecrackerNode is the node block validConfig carries, so the ec2 cases can
// swap the whole thing out rather than patching it line by line.
const firecrackerNode = `node:
  name: epyc-1
  server_addr: 127.0.0.1:7717
  provider: firecracker
  state_dir: /var/lib/billet/node
  firecracker:
    kernel_image: /var/lib/billet/vmlinux
    zfs_pool: tank
`

// ec2Node is a complete, valid cloud node.
//
// The tiers in validConfig pin themselves to firecracker, which is fine: a node
// declaring a backend no tier accepts is a deployment that has not finished being
// written, not an error — the fleet is assembled from several files.
const ec2Node = `node:
  name: aws-1
  server_addr: 127.0.0.1:7717
  provider: ec2
  state_dir: /var/lib/billet/node
  max_vcpu: 64
  max_memory: 256GiB
` + ec2Block

// ec2Block is the backend's own section, kept separate so a case can remove all
// of it at once.
const ec2Block = `  ec2:
    region: us-west-2
    subnet_id: subnet-0abc
    security_group_ids: [sg-0abc]
    instance_types:
      - type: c7i.2xlarge
        vcpu: 8
        memory: 16GiB
`

// cloudConfig is validConfig with its node replaced by a cloud one, optionally
// with a single substitution applied to that node.
func cloudConfig(t *testing.T, old, replacement string) string {
	t.Helper()

	body := strings.Replace(validConfig, firecrackerNode, ec2Node, 1)
	if !strings.Contains(body, "provider: ec2") {
		t.Fatal("the node block in validConfig has changed, so these cases patch nothing")
	}

	if old == "" {
		return body
	}

	patched := strings.Replace(body, old, replacement, 1)
	if patched == body {
		t.Fatalf("substituting %q changed nothing; the fixture has drifted", old)
	}

	return patched
}

// loadErr loads a config body and returns the error, which every case here
// expects to be non-nil.
func loadErr(t *testing.T, body string) string {
	t.Helper()

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("this config was accepted, and it should not have been")
	}

	return err.Error()
}

// A CLOUD NODE HAS NO MACHINE TO MEASURE, SO IT HAS TO SAY.
//
// Every other backend runs jobs on the box billet is running on, so leaving the
// contribution unset means "everything I can detect" and that is a good answer.
// An ec2 node is an orchestrator: it calls an API and the compute appears in a
// region. Detection would report the hardware of whatever small instance holds
// the process — plausibly two vCPU — and billet would advertise that to GitHub as
// the whole cloud's capacity, so the failover this backend exists for would place
// one job and then look full.
//
// It is REQUIRED rather than defaulted because the number is a spending limit,
// and billet has no standing to pick one on somebody's AWS account.
func TestACloudNodeMustSayWhatItWillBuy(t *testing.T) {
	for name, tc := range map[string]struct{ old, want string }{
		"no cores":  {old: "  max_vcpu: 64\n", want: "max_vcpu"},
		"no memory": {old: "  max_memory: 256GiB\n", want: "max_memory"},
	} {
		t.Run(name, func(t *testing.T) {
			got := loadErr(t, cloudConfig(t, tc.old, ""))

			if !strings.Contains(got, tc.want) {
				t.Errorf("the error does not name %s: %s", tc.want, got)
			}

			// The diagnostic has to say WHY this backend is different, or an
			// operator reads it as billet being obtuse about a field that is
			// optional three lines up in the documentation.
			if !strings.Contains(got, "detect") {
				t.Errorf("the error does not explain that there is nothing to detect: %s", got)
			}
		})
	}
}

// A backend that launches machines somewhere needs to be told where.
func TestACloudNodeNeedsSomewhereToLaunch(t *testing.T) {
	for name, tc := range map[string]struct{ old, want string }{
		// The WHOLE block, because dropping the `ec2:` key alone leaves its
		// children indented under nothing and yaml refuses the file before any of
		// billet's own validation runs — a red test that proves only that the
		// fixture was edited carelessly.
		"no ec2 block":       {old: ec2Block, want: "node.ec2 is required"},
		"no region":          {old: "    region: us-west-2\n", want: "node.ec2.region"},
		"no subnet":          {old: "    subnet_id: subnet-0abc\n", want: "node.ec2.subnet_id"},
		"no security groups": {old: "    security_group_ids: [sg-0abc]\n", want: "security_group_ids"},
	} {
		t.Run(name, func(t *testing.T) {
			got := loadErr(t, cloudConfig(t, tc.old, ""))

			if !strings.Contains(got, tc.want) {
				t.Errorf("the error does not name %s: %s", tc.want, got)
			}
		})
	}
}

// A REGION IS CHECKED FOR SHAPE, because it is not only an address.
//
// It goes into the SIGNING SCOPE, so a typo cannot be rescued by setting
// `endpoint` — the request would reach the right host and be refused with a 403
// that names nothing. Left to be discovered at runtime, that is the first launch
// of the day failing rather than `billet check` saying so.
//
// A SHAPE RATHER THAN A LIST. An allowlist of regions is a rule about somebody
// else's product that goes stale the next time AWS opens one, and being stale
// means refusing a config that is perfectly correct. The shape catches the
// mistake people actually make, which is dropping the hyphens.
func TestARegionIsCheckedForShape(t *testing.T) {
	for name, region := range map[string]string{
		"no hyphens": "uswest2",
		"no digit":   "us-west",
		"shouting":   "US-WEST-2",
		"a hostname": "ec2.us-west-2.amazonaws.com",
	} {
		t.Run(name, func(t *testing.T) {
			got := loadErr(t, cloudConfig(t, "    region: us-west-2\n",
				"    region: "+region+"\n"))

			if !strings.Contains(got, "region") {
				t.Errorf("the error does not name the region: %s", got)
			}
		})
	}

	// ACCEPTED, and every one of these is a real region. GovCloud and China are
	// the reason this is not an allowlist: partitions billet has never been run in
	// still have to load. The ordinary `us-west-2` is the fixture's own default,
	// so the happy-path test covers it rather than a no-op substitution here.
	for name, region := range map[string]string{
		"four parts":  "ap-southeast-4",
		"govcloud":    "us-gov-west-1",
		"china":       "cn-north-1",
		"a long name": "il-central-1",
	} {
		t.Run(name, func(t *testing.T) {
			body := cloudConfig(t, "    region: us-west-2\n", "    region: "+region+"\n")

			if _, err := Load(writeConfig(t, body)); err != nil {
				t.Errorf("region %q was rejected: %v", region, err)
			}
		})
	}
}

// EVERY SHAPE DECLARES WHAT IT HOLDS, because billet ships no table of EC2
// instance types and the allocator has already escrowed a size by the time one is
// chosen. A shape that lies about its cores over-commits a machine nobody can see,
// so a zero — the shape of a forgotten field — is refused rather than read as
// "unknown, use it anyway".
func TestACloudNodeMustDeclareTheShapesItMayBuy(t *testing.T) {
	for name, tc := range map[string]struct{ old, new, want string }{
		"no types at all": {
			old:  "    instance_types:\n      - type: c7i.2xlarge\n        vcpu: 8\n        memory: 16GiB\n",
			want: "instance_types",
		},
		"a shape with no name": {
			old: "      - type: c7i.2xlarge\n", new: "      - type: \"\"\n",
			want: "type is required",
		},
		"a shape with no cores": {
			old: "        vcpu: 8\n", new: "        vcpu: 0\n",
			want: "vcpu",
		},
		"a shape with no memory": {
			old: "        memory: 16GiB\n", new: "        memory: 0\n",
			want: "memory",
		},
		"the same shape twice": {
			old: "      - type: c7i.2xlarge\n        vcpu: 8\n        memory: 16GiB\n",
			new: "      - type: c7i.2xlarge\n        vcpu: 8\n        memory: 16GiB\n" +
				"      - type: c7i.2xlarge\n        vcpu: 8\n        memory: 16GiB\n",
			want: "listed twice",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := loadErr(t, cloudConfig(t, tc.old, tc.new))

			if !strings.Contains(got, tc.want) {
				t.Errorf("the error does not name %s: %s", tc.want, got)
			}
		})
	}
}

// The happy path, which also pins the defaults that decide what gets bought.
func TestAValidCloudNodeLoads(t *testing.T) {
	cfg, err := Load(writeConfig(t, cloudConfig(t, "", "")))
	if err != nil {
		t.Fatalf("a complete cloud node was rejected: %v", err)
	}

	if cfg.Node.EC2 == nil {
		t.Fatal("node.ec2 did not survive the load")
	}

	// SPOT DEFAULTS OFF, and this is the assertion that keeps it that way. This
	// backend exists so one `runs-on` label survives the bare-metal host going
	// away, and GitHub does not requeue a job whose runner vanished mid-execution
	// — so a reclaimed spot instance is a failed build, not a retry. Defaulting to
	// spot would make the failover path the unreliable one.
	if cfg.Node.EC2.Spot {
		t.Error("spot defaulted to on; a reclaim fails a build that GitHub will not requeue")
	}

	// Untrusted work is refused until its network is described separately, and
	// the absence of this list is what does the refusing.
	if len(cfg.Node.EC2.UntrustedSecurityGroupIDs) != 0 {
		t.Error("untrusted security groups defaulted to something; they must default to none")
	}

	if got := cfg.Node.EC2.InstanceTypes[0]; got.Type != "c7i.2xlarge" ||
		got.VCPU != 8 || got.Memory != 16*GiB {
		t.Errorf("instance type = %+v, want c7i.2xlarge/8/16GiB", got)
	}
}
