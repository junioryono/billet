package config

import (
	"strconv"
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
    bridge: br0
  ceph:
    image_pool: billet-images
    cache_pool: billet-cache
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
        price_usd_per_hour: 0.34
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
			old: "    instance_types:\n      - type: c7i.2xlarge\n        vcpu: 8\n" +
				"        memory: 16GiB\n        price_usd_per_hour: 0.34\n",
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
			old: "      - type: c7i.2xlarge\n        vcpu: 8\n        memory: 16GiB\n" +
				"        price_usd_per_hour: 0.34\n",
			new: "      - type: c7i.2xlarge\n        vcpu: 8\n        memory: 16GiB\n" +
				"        price_usd_per_hour: 0.34\n" +
				"      - type: c7i.2xlarge\n        vcpu: 8\n        memory: 16GiB\n" +
				"        price_usd_per_hour: 0.34\n",
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
		got.VCPU != 8 || got.Memory != 16*GiB || got.PriceUSDPerHour != 340_000 {
		t.Errorf("instance type = %+v, want c7i.2xlarge/8/16GiB/$0.34", got)
	}
}

func TestACloudNodeMayConfigureItsSitesEBSAndS3Store(t *testing.T) {
	t.Parallel()

	body := cloudConfig(t, "  provider: ec2\n", `  provider: ec2
  site: aws-us-west-2
  ebs_s3:
    region: us-west-2
    availability_zone: us-west-2a
    bucket: billet-cache-example
    prefix: deployments/example/aws-us-west-2
`)
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Node.EBSS3 == nil || cfg.Node.EBSS3.AvailabilityZone != "us-west-2a" ||
		cfg.Node.EBSS3.Bucket != "billet-cache-example" {
		t.Fatalf("node ebs-s3 config = %+v", cfg.Node.EBSS3)
	}
}

func TestACloudNodeExposesItsCacheOnlyThroughTLS(t *testing.T) {
	t.Parallel()

	cache := `  site: aws-us-west-2
  ebs_s3:
    region: us-west-2
    availability_zone: us-west-2a
    bucket: billet-cache-example
  cache:
    listen: 10.0.2.10:7718
    guest_endpoint: https://cache.aws.example:7718
    tls_cert: /etc/billet/cache.crt
    tls_key: /etc/billet/cache.key
`
	body := cloudConfig(t, "  provider: ec2\n", "  provider: ec2\n"+cache)
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Node.Cache == nil || cfg.Node.Cache.TLSCert != "/etc/billet/cache.crt" {
		t.Fatalf("cloud cache config = %+v", cfg.Node.Cache)
	}

	for name, replacement := range map[string]string{
		"plaintext":      strings.Replace(cache, "https://cache.aws.example", "http://cache.aws.example", 1),
		"no certificate": strings.Replace(cache, "    tls_cert: /etc/billet/cache.crt\n", "", 1),
		"different port": strings.Replace(cache, "cache.aws.example:7718", "cache.aws.example:8443", 1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bad := cloudConfig(t, "  provider: ec2\n", "  provider: ec2\n"+replacement)
			if _, err := Load(writeConfig(t, bad)); err == nil {
				t.Fatal("Load accepted a cloud cache endpoint that could expose its bearer token")
			}
		})
	}
}

func TestAnEBSAndS3StoreMustBeUsableByThisCloudNode(t *testing.T) {
	t.Parallel()

	valid := `  site: aws-us-west-2
  ebs_s3:
    region: us-west-2
    availability_zone: us-west-2a
    bucket: billet-cache-example
    prefix: deployments/example/aws-us-west-2
`
	for name, replacement := range map[string]string{
		"no site":             strings.Replace(valid, "  site: aws-us-west-2\n", "", 1),
		"other region":        strings.Replace(valid, "    region: us-west-2\n", "    region: us-east-1\n", 1),
		"other region's zone": strings.Replace(valid, "    availability_zone: us-west-2a\n", "    availability_zone: us-east-1a\n", 1),
		"non-DNS bucket":      strings.Replace(valid, "    bucket: billet-cache-example\n", "    bucket: Billet_Cache\n", 1),
		"dotted TLS bucket":   strings.Replace(valid, "    bucket: billet-cache-example\n", "    bucket: billet.cache.example\n", 1),
		"absolute prefix":     strings.Replace(valid, "    prefix: deployments/example/aws-us-west-2\n", "    prefix: /absolute\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			body := cloudConfig(t, "  provider: ec2\n", "  provider: ec2\n"+replacement)
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Fatal("Load accepted an EBS/S3 store the node cannot address safely")
			}
		})
	}

	body := strings.Replace(validConfig, "  provider: firecracker\n",
		"  provider: firecracker\n"+valid, 1)
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("Load accepted EBS/S3 storage on a host-backed provider")
	}
}

func TestSpotNeedsAnInterruptionQueue(t *testing.T) {
	got := loadErr(t, cloudConfig(t, "    region: us-west-2\n",
		"    region: us-west-2\n    spot: true\n"))
	if !strings.Contains(got, "interruption_queue_url") {
		t.Errorf("the error does not name the missing warning channel: %s", got)
	}
}

func TestAValidSpotInterruptionQueueLoads(t *testing.T) {
	body := cloudConfig(t, "    region: us-west-2\n", "    region: us-west-2\n"+
		"    spot: true\n"+
		"    interruption_queue_url: https://sqs.us-west-2.amazonaws.com/123456789012/aws-1\n")

	if _, err := Load(writeConfig(t, body)); err != nil {
		t.Fatalf("a valid interruption queue was refused: %v", err)
	}
}

func TestASpotQueueIsDedicatedToTheConfiguredNode(t *testing.T) {
	body := cloudConfig(t, "    region: us-west-2\n", "    region: us-west-2\n"+
		"    spot: true\n"+
		"    interruption_queue_url: https://sqs.us-west-2.amazonaws.com/123456789012/shared\n")

	got := loadErr(t, body)
	if !strings.Contains(got, "exactly the effective node name") {
		t.Fatalf("shared queue error does not explain the per-node name: %s", got)
	}
}

func TestASpotQueueDefersItsNameCheckToTheCertificateIdentity(t *testing.T) {
	body := cloudConfig(t, "    region: us-west-2\n", "    region: us-west-2\n"+
		"    spot: true\n"+
		"    interruption_queue_url: https://sqs.us-west-2.amazonaws.com/123456789012/cert-node\n")
	body = strings.Replace(body, "  name: aws-1\n", "", 1)
	body = strings.Replace(body, "  server_addr: 127.0.0.1:7717\n",
		"  server_addr: 10.0.0.4:7717\n", 1)
	body = strings.Replace(body, "  listen: 127.0.0.1:7717\n",
		"  listen: 0.0.0.0:7717\n", 1)
	body = strings.Replace(body, "  state_dir: /var/lib/billet/node\n",
		"  state_dir: /var/lib/billet/node\n"+
			"  tls:\n"+
			"    cert: /etc/billet/tls/node.crt\n"+
			"    key: /etc/billet/tls/node.key\n"+
			"    ca: /etc/billet/tls/ca.crt\n", 1)

	if _, err := Load(writeConfig(t, body)); err != nil {
		t.Fatalf("a Spot queue whose name comes from the certificate was refused early: %v", err)
	}
}

func TestAnInterruptionQueueCannotReceiveCredentialsOverPlaintext(t *testing.T) {
	body := cloudConfig(t, "    region: us-west-2\n", "    region: us-west-2\n"+
		"    spot: true\n"+
		"    interruption_queue_url: http://sqs.us-west-2.amazonaws.com/123456789012/billet\n")

	got := loadErr(t, body)
	if !strings.Contains(got, "https") {
		t.Errorf("the error does not explain that the signed queue request needs https: %s", got)
	}
}

func TestAnInterruptionQueueMustBeInTheEC2Region(t *testing.T) {
	body := cloudConfig(t, "    region: us-west-2\n", "    region: us-west-2\n"+
		"    spot: true\n"+
		"    interruption_queue_url: https://sqs.us-east-1.amazonaws.com/123456789012/billet\n")

	got := loadErr(t, body)
	if !strings.Contains(got, "us-west-2") {
		t.Errorf("the error does not name the region used to sign the queue request: %s", got)
	}
}

// A CREDENTIAL-BEARING REQUEST MUST NOT GO OUT IN PLAINTEXT.
//
// `endpoint` exists for a VPC interface endpoint or a non-commercial partition,
// and it was accepted as anything url.Parse tolerates — including `http://`. The
// secret access key never crosses the wire, but a SESSION TOKEN and a replayable
// signed RunInstances do, so an on-path observer gets both.
//
// LOOPBACK IS THE EXCEPTION, and it is billet's existing rule rather than a new
// one: a loopback wire has no certificates at all, because the trust boundary is
// the machine. It is also what lets a test point this at an httptest server.
func TestACloudEndpointMustBeEncryptedUnlessItIsLoopback(t *testing.T) {
	for name, tc := range map[string]struct{ endpoint, want string }{
		"plaintext to a host":   {endpoint: "http://ec2.us-west-2.amazonaws.com/", want: "https"},
		"plaintext to a domain": {endpoint: "http://vpce-abc.ec2.us-west-2.vpce.amazonaws.com/", want: "https"},
		"no scheme":             {endpoint: "ec2.us-west-2.amazonaws.com", want: "https"},
		"no host":               {endpoint: "https:///v1", want: "host"},
	} {
		t.Run(name, func(t *testing.T) {
			got := loadErr(t, cloudConfig(t, "    region: us-west-2\n",
				"    region: us-west-2\n    endpoint: "+tc.endpoint+"\n"))

			if !strings.Contains(got, tc.want) {
				t.Errorf("the error does not mention %q: %s", tc.want, got)
			}
		})
	}

	for name, endpoint := range map[string]string{
		"https":               "https://ec2.us-west-2.amazonaws.com/",
		"a vpc endpoint":      "https://vpce-abc.ec2.us-west-2.vpce.amazonaws.com/",
		"loopback by address": "http://127.0.0.1:44301/",
		"loopback by name":    "http://localhost:44301/",
		"loopback over ipv6":  "http://[::1]:44301/",
	} {
		t.Run(name, func(t *testing.T) {
			body := cloudConfig(t, "    region: us-west-2\n",
				"    region: us-west-2\n    endpoint: "+endpoint+"\n")

			if _, err := Load(writeConfig(t, body)); err != nil {
				t.Errorf("endpoint %q was rejected: %v", endpoint, err)
			}
		})
	}
}

// AN EMPTY STRING IS NOT A SECURITY GROUP, and on the untrusted list it is worse
// than a missing key: `Accepts` admits fork pull-request work as soon as the list
// is non-empty, so a list holding one empty string opens the backend to untrusted
// code with no network actually described for it.
func TestACloudNodeRefusesAnEmptySecurityGroup(t *testing.T) {
	for name, tc := range map[string]struct{ old, replacement string }{
		"trusted": {
			old:         "    security_group_ids: [sg-0abc]\n",
			replacement: "    security_group_ids: [\"\"]\n",
		},
		"untrusted": {
			old:         "    security_group_ids: [sg-0abc]\n",
			replacement: "    security_group_ids: [sg-0abc]\n    untrusted_security_group_ids: [\"  \"]\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := loadErr(t, cloudConfig(t, tc.old, tc.replacement))

			if !strings.Contains(got, "security_group_ids") {
				t.Errorf("the error does not name the field: %s", got)
			}
		})
	}
}

// NORMALIZED BEFORE ANYTHING USES THEM, the way node names already are.
//
// Validation trimmed these values to CHECK them and the rest of billet then used
// the raw ones — so `region: "  us-west-2  "` passed the shape check and was
// signed with the padding, which is a 403 naming nothing. YAML strips whitespace
// from a plain scalar but keeps it inside quotes, so this is reachable from a
// perfectly ordinary-looking file.
func TestCloudValuesAreNormalizedBeforeTheyAreUsed(t *testing.T) {
	body := cloudConfig(t, "    region: us-west-2\n", "    region: \"  us-west-2  \"\n")
	body = strings.Replace(body, "    subnet_id: subnet-0abc\n",
		"    subnet_id: \"  subnet-0abc \"\n", 1)
	body = strings.Replace(body, "      - type: c7i.2xlarge\n", "      - type: \" c7i.2xlarge \"\n", 1)

	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := cfg.Node.EC2.Region; got != "us-west-2" {
		t.Errorf("region = %q, want it trimmed before it reaches the signer", got)
	}

	if got := cfg.Node.EC2.SubnetID; got != "subnet-0abc" {
		t.Errorf("subnet_id = %q, want it trimmed", got)
	}

	if got := cfg.Node.EC2.InstanceTypes[0].Type; got != "c7i.2xlarge" {
		t.Errorf("instance type = %q, want it trimmed", got)
	}
}

// A TIER THAT NO DECLARED SHAPE CAN HOLD QUEUES FOREVER WITH NOTHING SAYING WHY.
//
// The allocator will escrow it happily — the node's budget covers it — and the
// failure appears only after GitHub has assigned the job, as a launch error on
// one host. billet already refuses a tier pinned to a host that cannot run its
// guest OS at load time for exactly this reason.
func TestATierNoDeclaredShapeCanHoldIsRefusedAtLoad(t *testing.T) {
	body := cloudConfig(t, "", "")
	// The tiers in validConfig pin firecracker; make one reachable on this node
	// and larger than every shape it declares.
	body = strings.Replace(body,
		"  - label: billet-8vcpu-ubuntu-2404\n    provider: firecracker\n    vcpu: 8\n",
		"  - label: billet-8vcpu-ubuntu-2404\n    provider: ec2\n    vcpu: 32\n", 1)

	got := loadErr(t, body)

	for _, want := range []string{"billet-8vcpu-ubuntu-2404", "instance_types"} {
		if !strings.Contains(got, want) {
			t.Errorf("the error does not mention %q: %s", want, got)
		}
	}
}

// A URL is a thing that gets logged, so a credential must not live in one.
// billet authenticates with a request signature, so userinfo authenticates
// nothing and is pure exposure.
func TestACloudEndpointMustNotCarryACredential(t *testing.T) {
	got := loadErr(t, cloudConfig(t, "    region: us-west-2\n",
		"    region: us-west-2\n    endpoint: https://user:pass@ec2.us-west-2.amazonaws.com/\n"))

	if !strings.Contains(got, "password") {
		t.Errorf("the error does not explain what is wrong: %s", got)
	}

	// AND THE PASSWORD IS NOT IN THE ERROR, which would put it in the log that
	// reports the config being invalid.
	if strings.Contains(got, "pass@") || strings.Contains(got, "user:pass") {
		t.Errorf("the error rendered the credential it is refusing: %s", got)
	}
}

// A DNS NAME THAT HAPPENS TO RESOLVE TO LOOPBACK IS NOT LOOPBACK. billet does not
// resolve the host, so only a literal loopback address or "localhost" takes the
// exception — the safe direction, since resolution is attacker-influenceable and
// can change between the check and the request.
func TestTheLoopbackExceptionIsNotResolved(t *testing.T) {
	got := loadErr(t, cloudConfig(t, "    region: us-west-2\n",
		"    region: us-west-2\n    endpoint: http://localtest.me/\n"))

	if !strings.Contains(got, "https") {
		t.Errorf("a plaintext endpoint to a resolvable name was not refused: %s", got)
	}
}

// A REFUSAL MUST NOT PRINT THE CREDENTIAL IT IS REFUSING, whatever the reason for
// refusing. Checking the scheme before the userinfo meant a non-http scheme was
// rejected by a message that rendered the password — a validation rule creating
// the exposure it exists to prevent.
func TestNoEndpointRefusalRendersAPassword(t *testing.T) {
	const leaked = "hunter2hunter2"

	for name, endpoint := range map[string]string{
		"an odd scheme": "ftp://alice:" + leaked + "@example.com/",
		"plaintext":     "http://alice:" + leaked + "@example.com/",
		"unparseable":   "https://alice:" + leaked + "@exa mple.com/",
		"no host":       "https://alice:" + leaked + "@/v1",
	} {
		t.Run(name, func(t *testing.T) {
			got := loadErr(t, cloudConfig(t, "    region: us-west-2\n",
				"    region: us-west-2\n    endpoint: \""+endpoint+"\"\n"))

			if strings.Contains(got, leaked) {
				t.Errorf("the refusal rendered the password: %s", got)
			}
		})
	}
}

// A FLEET'S TIERS ARE READ BY EVERY NODE, so a small cloud node must not refuse
// the config because it can see a tier meant for a large one.
//
// The allocator never places work on a node beyond its own contribution, so a
// tier larger than this node's budget is not a contradiction — it is another
// machine's job. Refusing it would make one deployment's config unloadable on
// half its machines.
func TestATierThisNodeCouldNeverBeGivenIsNotItsProblem(t *testing.T) {
	body := cloudConfig(t, "", "")
	// Larger than the node's 64 vCPU budget, so it can never be placed here.
	// Bigger than the node's 64 vCPU budget and still inside server.max_vcpu, so
	// the only thing that could refuse it is the shape check under test.
	body = strings.Replace(body,
		"  - label: billet-8vcpu-ubuntu-2404\n    provider: firecracker\n    vcpu: 8\n",
		"  - label: billet-8vcpu-ubuntu-2404\n    provider: ec2\n    vcpu: 96\n", 1)
	body = strings.Replace(body, "    memory: 32GiB\n", "    memory: 200GiB\n", 1)

	if _, err := Load(writeConfig(t, body)); err != nil {
		t.Errorf("a tier too large for this node to be given was refused: %v", err)
	}
}

// AN ENDPOINT REFUSAL NEVER RENDERS THE ENDPOINT, whatever shape it is.
//
// Three attempts to render it safely were each wrong in a new way: interpolating
// it printed a password; wrapping url.Parse's error printed one too, because
// *url.Error embeds the whole URL; and url.Redacted masks only a HIERARCHICAL
// url's password, leaving an opaque one and any query string intact. Both of those
// last two were measured rather than reasoned about. So the rule is that the value
// is never rendered at all, and these are the shapes that defeated the previous
// rules.
func TestAnEndpointRefusalNeverRendersTheEndpoint(t *testing.T) {
	// A TOKEN THAT CANNOT APPEAR IN PROSE. The first version searched for the word
	// "secret", which the refusal messages themselves contain — so it failed on
	// their own explanation rather than on a leak.
	//
	// TWO MARKERS, in the secret AND in the host, because asserting only the first
	// would pass against an implementation that rendered scheme, host and path and
	// stripped nothing but the password — which is not what "never renders the
	// endpoint" says.
	const (
		leaked = "hunter2hunter2"
		host   = "markerhost9zz"
	)

	for name, endpoint := range map[string]string{
		"opaque, with a password": "http:alice:" + leaked + "@" + host + ".example",
		"a secret in the query":   "https://" + host + ".example/?token=" + leaked,
		"a secret in a fragment":  "https://" + host + ".example/#" + leaked,
		"userinfo":                "https://alice:" + leaked + "@" + host + ".example/",
		"unparseable":             "https://alice:" + leaked + "@exa mple." + host,
		"a path":                  "https://" + host + ".example/" + leaked,
	} {
		t.Run(name, func(t *testing.T) {
			got := loadErr(t, cloudConfig(t, "    region: us-west-2\n",
				"    region: us-west-2\n    endpoint: \""+endpoint+"\"\n"))

			for _, marker := range []string{leaked, host} {
				if strings.Contains(got, marker) {
					t.Errorf("the refusal rendered %q from the value it is refusing: %s",
						marker, got)
				}
			}
		})
	}
}

// A TIER PINNED TO THIS NODE AND TOO BIG FOR IT CAN NEVER RUN ANYWHERE.
//
// The skip that lets a fleet's oversized tiers load is for UNPINNED ones, which
// belong to another machine. A pinned tier means this node or nowhere, so the same
// skip applied to it would accept a configuration that queues forever.
func TestATierPinnedHereAndTooBigForHereIsRefused(t *testing.T) {
	body := cloudConfig(t, "", "")
	body = strings.Replace(body,
		"  - label: billet-8vcpu-ubuntu-2404\n    provider: firecracker\n    vcpu: 8\n",
		"  - label: billet-8vcpu-ubuntu-2404\n    provider: ec2\n    node: aws-1\n    vcpu: 96\n", 1)

	got := loadErr(t, body)

	if !strings.Contains(got, "billet-8vcpu-ubuntu-2404") {
		t.Errorf("the error does not name the tier: %s", got)
	}
}

// EVERY BLANK IS REPORTED, not the first. An operator fixing one and re-running to
// find the next is the failure mode Validate exists to avoid.
func TestEveryBlankSecurityGroupIsReported(t *testing.T) {
	got := loadErr(t, cloudConfig(t, "    security_group_ids: [sg-0abc]\n",
		"    security_group_ids: [\"\", \"\"]\n"))

	if strings.Count(got, "security_group_ids[") < 2 {
		t.Errorf("only some of the blank entries were reported: %s", got)
	}
}

// THE QUERY API LIVES AT THE ROOT, so a path would be signed and posted to
// somewhere that is not the service. No AWS regional, VPC-interface or
// non-commercial-partition endpoint needs one.
func TestACloudEndpointMustNameAHostWithNoPath(t *testing.T) {
	for name, endpoint := range map[string]string{
		"a versioned path": "https://vpce-abc.ec2.us-west-2.vpce.amazonaws.com/v1",
		"a trailing name":  "https://ec2.us-west-2.amazonaws.com/ec2",
	} {
		t.Run(name, func(t *testing.T) {
			got := loadErr(t, cloudConfig(t, "    region: us-west-2\n",
				"    region: us-west-2\n    endpoint: "+endpoint+"\n"))

			if !strings.Contains(got, "path") {
				t.Errorf("the error does not name the problem: %s", got)
			}
		})
	}

	// Absent and "/" are both the root, and both are ordinary.
	for name, endpoint := range map[string]string{
		"no path":      "https://ec2.us-west-2.amazonaws.com",
		"a root slash": "https://ec2.us-west-2.amazonaws.com/",
	} {
		t.Run(name, func(t *testing.T) {
			body := cloudConfig(t, "    region: us-west-2\n",
				"    region: us-west-2\n    endpoint: "+endpoint+"\n")

			if _, err := Load(writeConfig(t, body)); err != nil {
				t.Errorf("endpoint %q was rejected: %v", endpoint, err)
			}
		})
	}
}

// spotConfig is the cloud fixture with a chosen region and interruption queue.
//
// The node in ec2Node is named aws-1, so every queue basename here has to be aws-1
// or CheckSQSQueueNode refuses it for a different reason than the one under test.
func spotConfig(t *testing.T, region, queueURL string) string {
	t.Helper()

	return cloudConfig(t, "    region: us-west-2\n", "    region: "+region+"\n"+
		"    spot: true\n"+
		"    interruption_queue_url: "+queueURL+"\n")
}

// A QUEUE HOST BELONGS TO ITS REGION'S PARTITION, AND THE REGION PICKS IT.
//
// The validator used to admit EITHER DNS suffix for EVERY region, so a cn-north-1
// node could name sqs.cn-north-1.amazonaws.com — which is not a host — while
// `billet init iam` derived arn:aws-cn:sqs:... from the same region. The policy is
// then right, the config loads, and the node signs a ReceiveMessage for a name that
// does not resolve: the two-minute spot warning simply never arrives.
//
// MEASURED WITH dig ON 2026-09-04, because the endpoint tables read the other way
// for the legacy and VPC-endpoint forms. Every standard and legacy host below was
// resolved: the accepted ones answer and the refused ones are NXDOMAIN. GovCloud is
// a separate partition that takes the COMMERCIAL suffix, which is why it is here — a
// rule keyed on "is this the commercial partition" rather than on the region's own
// prefix gets exactly that case wrong.
func TestASpotQueueHostBelongsToItsRegionsPartition(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ region, queue string }{
		// sqs.cn-north-1.amazonaws.com.cn -> 140.179.15.58
		"china standard": {"cn-north-1",
			"https://sqs.cn-north-1.amazonaws.com.cn/123456789012/aws-1"},
		// cn-north-1.queue.amazonaws.com.cn is a CNAME onto the host above. This one
		// was REFUSED before the fix: the legacy form was admitted only with the
		// commercial suffix, so China's real legacy host was not a legal value.
		"china legacy": {"cn-north-1",
			"https://cn-north-1.queue.amazonaws.com.cn/123456789012/aws-1"},
		"china vpc endpoint": {"cn-north-1",
			"https://vpce-0a1b.sqs.cn-north-1.vpce.amazonaws.com.cn/123456789012/aws-1"},
		// sqs.us-gov-west-1.amazonaws.com -> 56.136.121.58, and the .cn form is
		// NXDOMAIN: GovCloud is its own partition on the commercial suffix.
		"govcloud standard": {"us-gov-west-1",
			"https://sqs.us-gov-west-1.amazonaws.com/123456789012/aws-1"},
		"commercial standard": {"us-west-2",
			"https://sqs.us-west-2.amazonaws.com/123456789012/aws-1"},
		// us-west-2.queue.amazonaws.com -> 44.242.184.144.
		"commercial legacy": {"us-west-2",
			"https://us-west-2.queue.amazonaws.com/123456789012/aws-1"},
		"commercial vpc endpoint": {"us-west-2",
			"https://vpce-0a1b.sqs.us-west-2.vpce.amazonaws.com/123456789012/aws-1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := Load(writeConfig(t, spotConfig(t, tc.region, tc.queue))); err != nil {
				t.Fatalf("a queue host in %s's own partition was refused: %v", tc.region, err)
			}
		})
	}
}

// THE OTHER PARTITION'S SUFFIX NAMES NO HOST, so accepting it can only produce a
// node that never hears a reclaim. Every case here loaded before the fix.
//
// The DIAGNOSTIC is asserted rather than the fact of an error: an operator who
// copied the wrong suffix has to be told which hosts their region's partition
// actually serves, and "an error came back" is also satisfied by any of the four
// unrelated refusals earlier in the same validator.
func TestASpotQueueHostFromTheOtherPartitionIsRefused(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ region, queue, wantHost string }{
		"commercial suffix in china": {"cn-north-1",
			"https://sqs.cn-north-1.amazonaws.com/123456789012/aws-1",
			"sqs.cn-north-1.amazonaws.com.cn,"},
		"commercial legacy in china": {"cn-north-1",
			"https://cn-north-1.queue.amazonaws.com/123456789012/aws-1",
			"sqs.cn-north-1.amazonaws.com.cn,"},
		"commercial vpc endpoint in china": {"cn-north-1",
			"https://vpce-0a1b.sqs.cn-north-1.vpce.amazonaws.com/123456789012/aws-1",
			"sqs.cn-north-1.amazonaws.com.cn,"},
		"china suffix in a commercial region": {"us-west-2",
			"https://sqs.us-west-2.amazonaws.com.cn/123456789012/aws-1",
			"sqs.us-west-2.amazonaws.com,"},
		"china legacy in a commercial region": {"us-west-2",
			"https://us-west-2.queue.amazonaws.com.cn/123456789012/aws-1",
			"sqs.us-west-2.amazonaws.com,"},
		"china vpc endpoint in a commercial region": {"us-west-2",
			"https://vpce-0a1b.sqs.us-west-2.vpce.amazonaws.com.cn/123456789012/aws-1",
			"sqs.us-west-2.amazonaws.com,"},
		"china suffix in govcloud": {"us-gov-west-1",
			"https://sqs.us-gov-west-1.amazonaws.com.cn/123456789012/aws-1",
			"sqs.us-gov-west-1.amazonaws.com,"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := loadErr(t, spotConfig(t, tc.region, tc.queue))

			if !strings.Contains(got, strconv.Quote(tc.region)) {
				t.Errorf("the error does not name the region it signs for: %s", got)
			}
			if !strings.Contains(got, tc.wantHost) {
				t.Errorf("the error does not name %s, the host %s's partition serves: %s",
					tc.wantHost, tc.region, got)
			}
		})
	}
}

// GOVCLOUD IS THE CASE A PARTITION-SHAPED RULE GETS WRONG. It is a partition of its
// own, so a suffix chosen by asking "is this the commercial partition" would send it
// to amazonaws.com.cn, where nothing answers.
func TestTheDNSSuffixIsThePartitionsOwn(t *testing.T) {
	t.Parallel()

	for region, want := range map[string]string{
		"cn-north-1":     "amazonaws.com.cn",
		"cn-northwest-1": "amazonaws.com.cn",
		"us-west-2":      "amazonaws.com",
		"us-east-1":      "amazonaws.com",
		"us-gov-west-1":  "amazonaws.com",
		"us-gov-east-1":  "amazonaws.com",
	} {
		if got := AWSDNSSuffix(region); got != want {
			t.Errorf("AWSDNSSuffix(%q) = %q, want %q", region, got, want)
		}
	}
}

// vpceTail is the suffix a us-west-2 VPC endpoint's own labels sit in front of.
const vpceTail = ".sqs.us-west-2.vpce.amazonaws.com"

// vpceQueue is a queue URL whose endpoint labels are prefix.
func vpceQueue(prefix string) string {
	return "https://" + prefix + vpceTail + "/123456789012/aws-1"
}

// vpcePrefixFilling builds endpoint labels that make the whole HOST exactly n
// characters, in valid 63-character labels, so the length boundary is exercised by a
// name that is otherwise correct. It fails rather than returns a wrong length,
// because a fixture that quietly built a 251-character host would leave the test
// green while asserting nothing about the bound.
func vpcePrefixFilling(t *testing.T, n int) string {
	t.Helper()

	want := n - len(vpceTail)
	if want < 1 {
		t.Fatalf("a %d-character host leaves no room for an endpoint label", n)
	}

	var b strings.Builder
	for b.Len() < want {
		if b.Len() > 0 {
			b.WriteByte('.')
		}

		b.WriteString(strings.Repeat("a", min(want-b.Len(), 63)))
	}

	if got := b.Len() + len(vpceTail); got != n {
		t.Fatalf("the fixture built a %d-character host, not %d", got, n)
	}

	return b.String()
}

// A VPC ENDPOINT'S HOST HAS TO BE A HOSTNAME, LABEL BY LABEL.
//
// The private form is matched by SUFFIX, because the labels in front are the
// operator's endpoint id and billet cannot know them. A bare suffix match asks for
// nothing at all in front of it, and a whole-prefix shape — the first version of this
// check — pins only the first and last character. So `.sqs...` with no label,
// `a_b!.sqs...`, `a..b` with an empty inner label, `a.-b`, and a 64-character label
// all loaded, and not one of them is a name DNS can answer.
//
// THAT IS THE SAME FAILURE AS THE WRONG PARTITION, which is why it is here rather
// than filed as tidiness: a queue URL billet accepts but a node cannot address costs
// the two-minute interruption warning, and it costs it silently — no error anywhere,
// the lease charged until it expires.
//
// WHAT IS NOT ASSERTED IS AWS'S `vpce-` SPELLING. The DNS suffixes were measured; the
// endpoint's own label cannot be, without owning an endpoint. So a rule about it would
// be a reading of the documentation, and several labels stay accepted: they are a
// well-formed name inside AWS's own zone.
func TestASpotQueueVPCEndpointNeedsItsOwnLabel(t *testing.T) {
	t.Parallel()

	refused := map[string]string{
		"no label at all":           "",
		"an empty leading label":    ".",
		"an empty inner label":      "a..b",
		"an illegal character":      "a_b!",
		"a trailing dot":            "vpce-0a1b.",
		"a leading hyphen":          "-vpce",
		"a trailing hyphen":         "vpce-",
		"an inner leading hyphen":   "a.-b",
		"an inner trailing hyphen":  "a-.b",
		"an all-hyphen inner label": "a.---.b",
		"a label of sixty-four":     strings.Repeat("a", 64),
	}

	for name, prefix := range refused {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := loadErr(t, spotConfig(t, "us-west-2", vpceQueue(prefix)))

			if !strings.Contains(got, "hostname label") {
				t.Errorf("the error does not say the endpoint's own label is the problem: %s", got)
			}
		})
	}

	accepted := map[string]string{
		"one character":          "a",
		"all digits":             "123",
		"what aws hands out":     "vpce-0a1b2c3d4e5f6g7h8-abcdefgh",
		"a zonal name":           "vpce-0a1b2c3d4e5f6g7h8-abcdefgh-us-west-2a",
		"a punycode label":       "xn--k3h",
		"a label of sixty-three": strings.Repeat("a", 63),
	}

	for name, prefix := range accepted {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := Load(writeConfig(t, spotConfig(t, "us-west-2", vpceQueue(prefix)))); err != nil {
				t.Fatalf("a well-formed VPC endpoint host was refused: %v", err)
			}
		})
	}
}

// AND THE WHOLE HOST HAS A LENGTH, which the per-label rule does not imply: sixty
// valid labels are sixty valid labels and still not a name anything can look up. Both
// sides of the boundary, because a bound asserted only from the refusing side is
// satisfied by a check that refuses everything.
func TestASpotQueueVPCEndpointHostIsBoundedInLength(t *testing.T) {
	t.Parallel()

	t.Run("exactly 253", func(t *testing.T) {
		t.Parallel()

		body := spotConfig(t, "us-west-2", vpceQueue(vpcePrefixFilling(t, 253)))
		if _, err := Load(writeConfig(t, body)); err != nil {
			t.Fatalf("a 253-character host is the longest DNS name there is, and it was refused: %v", err)
		}
	})

	t.Run("one over", func(t *testing.T) {
		t.Parallel()

		got := loadErr(t, spotConfig(t, "us-west-2", vpceQueue(vpcePrefixFilling(t, 254))))

		if !strings.Contains(got, "hostname label") {
			t.Errorf("the error does not name the endpoint host as the problem: %s", got)
		}
	})
}
