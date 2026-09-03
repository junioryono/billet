package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider/ec2"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// fakeEC2 answers the read-only describes the cloud preflight makes, so its live
// calls have somewhere to go that is not somebody's AWS account.
//
// IT CHECKS WHAT IT WAS ASKED, rather than answering a fixed status to anything.
// A fake that ignores the request leaves the test green when the call is
// unsigned or is made with some other identity — so `accept` is the credential
// every call must present, refused the way AWS would refuse it. The topology it
// reports (the subnet's vpc and zone, each group's vpc, each image's state) is
// what the preflight's policy checks are run against.
type fakeEC2Topology struct {
	accept     string
	subnetVPC  string
	subnetZone string
	groupVPC   string // defaults to subnetVPC
	imageState string // "available"; "" answers InvalidAMIID.NotFound (a not-built AMI)
	// untaggedImage drops the provenance tags, describing an image built before
	// billet stamped its output — the state every AMI in service was in when this
	// was written.
	untaggedImage bool
	authzCode     string          // the code a RunInstances DryRun answers ("DryRunOperation")
	unavailAMI    map[string]bool // AMIs that answer InvalidAMIID.NotFound; others are available
	instances     []fakeInstance  // running instances DescribeInstances reports
	runRec        *runRecorder
	profile       string
	queueDenied   bool
	authFailure   bool
}

// fakeInstance is one running instance the fake's DescribeInstances reports.
type fakeInstance struct {
	id, name string
}

// runRecorder captures the RunInstances dry-run bodies the fake receives, so a
// test can assert what the launch request carried (e.g. the owner tag).
type runRecorder struct {
	mu     sync.Mutex
	bodies []string
}

func (r *runRecorder) add(body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, body)
}

func (r *runRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.bodies)
}

func fakeEC2(t *testing.T, accept string) string {
	t.Helper()

	return fakeEC2With(t, fakeEC2Topology{accept: accept})
}

// writeXML writes a fake response body, checked because the project's errcheck
// checks blank writes too.
func writeXML(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()

	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func fakeEC2With(t *testing.T, topo fakeEC2Topology) string {
	t.Helper()

	if topo.subnetVPC == "" {
		topo.subnetVPC = "vpc-test"
	}
	if topo.subnetZone == "" {
		topo.subnetZone = "us-west-2a"
	}
	if topo.groupVPC == "" {
		topo.groupVPC = topo.subnetVPC
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The SQS probe posts JSON with an X-Amz-Target header rather than the
		// EC2 form protocol; answered first, before the form parsing below —
		// with its own signing-scope assertion, since it skips the form one.
		if r.Header.Get("X-Amz-Target") == "AmazonSQS.GetQueueAttributes" {
			if auth := r.Header.Get("Authorization"); !strings.Contains(auth,
				"Credential="+topo.accept+"/") || !strings.Contains(auth, "/us-west-2/sqs/aws4_request") {
				t.Errorf("the sqs probe is not signed with the expected scope: %q", auth)
			}
			if topo.queueDenied {
				w.WriteHeader(http.StatusBadRequest)
				writeXML(t, w, `{"__type":"com.amazon.coral.service#AccessDeniedException","message":"no"}`)

				return
			}
			w.Header().Set("Content-Type", "application/x-amz-json-1.0")
			writeXML(t, w, `{"Attributes":{"QueueArn":"arn:aws:sqs:us-west-2:123456789012:billet"}}`)

			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)

			return
		}

		params, err := url.ParseQuery(string(body))
		if err != nil {
			t.Errorf("parse body: %v", err)

			return
		}

		// THE CREDENTIAL IDENTITY, on every action. SigV4's credential scope carries
		// the access key; a call made with some other key than the one reported is
		// refused here rather than silently accepted. The signature itself is settled
		// in internal/provider/ec2 against AWS's own vectors, not re-tested here.
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") || !strings.Contains(auth, "Signature=") {
			t.Errorf("a preflight request is not sigv4-signed: %q", auth)
		}
		if topo.authFailure {
			// The MEASURED opted-out-region shape: AuthFailure with
			// credential prose, from a region gate rather than a bad key.
			w.WriteHeader(http.StatusUnauthorized)
			writeXML(t, w, `<Response><Errors><Error><Code>AuthFailure</Code>`+
				`<Message>AWS was not able to validate the provided access credentials</Message>`+
				`</Error></Errors></Response>`)

			return
		}
		if topo.accept == "" || !strings.Contains(auth, "Credential="+topo.accept+"/") {
			w.WriteHeader(http.StatusForbidden)
			writeXML(t, w, `<Response><Errors><Error>`+
				`<Code>UnauthorizedOperation</Code><Message>no</Message>`+
				`</Error></Errors></Response>`)

			return
		}

		switch params.Get("Action") {
		case "GetInstanceProfile":
			switch topo.profile {
			case "missing":
				w.WriteHeader(http.StatusNotFound)
				writeXML(t, w, `<ErrorResponse><Error><Code>NoSuchEntity</Code>`+
					`<Message>Instance Profile not found</Message></Error></ErrorResponse>`)
			case "denied":
				w.WriteHeader(http.StatusForbidden)
				writeXML(t, w, `<ErrorResponse><Error><Code>AccessDenied</Code>`+
					`<Message>not authorized</Message></Error></ErrorResponse>`)
			default:
				writeXML(t, w, `<GetInstanceProfileResponse><GetInstanceProfileResult>`+
					`</GetInstanceProfileResult></GetInstanceProfileResponse>`)
			}

		case "DescribeInstances":
			var items strings.Builder
			for _, inst := range topo.instances {
				items.WriteString(`<item><instancesSet><item>` +
					`<instanceId>` + inst.id + `</instanceId>` +
					`<instanceState><name>running</name></instanceState>` +
					`<tagSet><item><key>Name</key><value>` + inst.name + `</value></item></tagSet>` +
					`</item></instancesSet></item>`)
			}
			writeXML(t, w, `<DescribeInstancesResponse><reservationSet>`+
				items.String()+`</reservationSet></DescribeInstancesResponse>`)

		case "DescribeSubnets":
			writeXML(t, w, `<DescribeSubnetsResponse><subnetSet><item>`+
				`<subnetId>`+params.Get("SubnetId.1")+`</subnetId>`+
				`<vpcId>`+topo.subnetVPC+`</vpcId>`+
				`<availabilityZone>`+topo.subnetZone+`</availabilityZone>`+
				`<state>available</state></item></subnetSet></DescribeSubnetsResponse>`)

		case "DescribeSecurityGroups":
			var items strings.Builder
			for i := 1; ; i++ {
				id := params.Get("GroupId." + strconv.Itoa(i))
				if id == "" {
					break
				}
				items.WriteString(`<item><groupId>` + id + `</groupId><vpcId>` +
					topo.groupVPC + `</vpcId></item>`)
			}
			writeXML(t, w, `<DescribeSecurityGroupsResponse><securityGroupInfo>`+
				items.String()+`</securityGroupInfo></DescribeSecurityGroupsResponse>`)

		case "DescribeImages":
			state := topo.imageState
			if state == "" {
				state = "available"
			}
			if topo.imageState == "missing" || topo.unavailAMI[params.Get("ImageId.1")] {
				w.WriteHeader(http.StatusBadRequest)
				writeXML(t, w, `<Response><Errors><Error>`+
					`<Code>InvalidAMIID.NotFound</Code><Message>no</Message>`+
					`</Error></Errors></Response>`)

				return
			}
			tags := `<tagSet><item><key>sh.billet.ami-contract</key><value>` +
				strconv.Itoa(ec2.AMIContract) + `</value></item>` +
				`<item><key>sh.billet.built-by</key><value>v9.9.9-test</value></item></tagSet>`
			if topo.untaggedImage {
				tags = ""
			}

			// STAMPED AT THE CURRENT CONTRACT, so these fixtures describe an image
			// a current billet built. An untagged image is a real and separate
			// case — every AMI built before billet stamped its output is in it —
			// and TestAnImageBelowTheContractIsReported covers it deliberately
			// rather than every other test inheriting the warning by accident.
			writeXML(t, w, `<DescribeImagesResponse><imagesSet><item>`+
				`<imageId>`+params.Get("ImageId.1")+`</imageId>`+
				`<imageState>`+state+`</imageState>`+
				`<rootDeviceName>/dev/xvda</rootDeviceName><rootDeviceType>ebs</rootDeviceType>`+
				`<blockDeviceMapping><item><deviceName>/dev/xvda</deviceName>`+
				`<ebs><deleteOnTermination>true</deleteOnTermination></ebs></item></blockDeviceMapping>`+
				tags+
				`</item></imagesSet></DescribeImagesResponse>`)

		case "TerminateInstances":
			// A DryRun teardown is a preflight-authorize regression (it cannot be
			// proved without a real instance). A real one is decommission tearing
			// down a leftover instance — record it and succeed.
			if strings.Contains(string(body), "DryRun=true") {
				t.Errorf("a teardown must not be dry-run: %s", body)
			}
			if topo.runRec != nil {
				topo.runRec.add(string(body))
			}
			writeXML(t, w, `<TerminateInstancesResponse/>`)

		case "RunInstances":
			code := topo.authzCode
			if code == "" {
				code = "DryRunOperation"
			}
			if topo.runRec != nil {
				topo.runRec.add(string(body))
			}
			if !strings.Contains(string(body), "DryRun=true") {
				t.Errorf("authorization %s is not a dry run: %s", params.Get("Action"), body)
			}
			w.WriteHeader(http.StatusBadRequest)
			writeXML(t, w, `<Response><Errors><Error><Code>`+code+
				`</Code><Message>x</Message></Error></Errors></Response>`)

		default:
			t.Errorf("unexpected preflight action %q", params.Get("Action"))
		}
	}))

	t.Cleanup(srv.Close)

	return srv.URL
}

// wrapEC2 puts an EC2Config into a minimal single-machine Config so the preflight
// (which now reads tiers and the ebs_s3 zone) has a whole config to work from.
func wrapEC2(ec2cfg *config.EC2Config, tiers ...config.Tier) *config.Config {
	return &config.Config{
		Node: &config.NodeConfig{
			Name:     ec2cfg.NodeName,
			Provider: config.ProviderEC2,
			EC2:      ec2cfg,
		},
		Tiers: tiers,
	}
}

// capture redirects stdout for the duration of fn and returns what was written.
//
// `billet check` REPORTS to an operator, so what it prints is the whole product
// and asserting only its error return would leave the interesting half untested.
func capture(t *testing.T, fn func()) string {
	t.Helper()

	saved := os.Stdout

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	os.Stdout = w

	// RESTORED BY CLEANUP, not only by the happy path below. A t.Fatal inside fn
	// unwinds past the restore, leaving every later test in the package writing
	// into a pipe nobody reads — which surfaces as an unrelated test hanging or
	// losing its output, a long way from the test that actually failed.
	t.Cleanup(func() { os.Stdout = saved })

	done := make(chan string, 1)

	go func() {
		var b strings.Builder

		_, _ = io.Copy(&b, r) //nolint:errcheck // the write end is closed below, ending the copy

		done <- b.String()
	}()

	fn()

	os.Stdout = saved

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}

	return <-done
}

// THE PRE-FLIGHT REPORTS WHICH IDENTITY BILLET WILL USE, AND NEVER THE SECRET.
//
// The access key id is an identifier, and printing it is the difference between
// "billet is using the wrong role" and an operator staring at a config that looks
// right. The secret is a durable credential for a whole AWS account and must not
// reach a terminal, a scrollback buffer, or the paste of a support request.
func TestTheCloudPreflightNamesTheIdentityAndNotTheSecret(t *testing.T) {
	const secret = "wJalrXUtnFEMI-thisIsTheSecret"

	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", secret)

	cfg := &config.EC2Config{
		Region:           "us-west-2",
		Endpoint:         fakeEC2(t, "AKIDEXAMPLE"),
		SubnetID:         "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}

	var err error

	out := capture(t, func() { err = checkEC2Credentials(t.Context(), wrapEC2(cfg), nil, false, false) })
	if err != nil {
		t.Fatalf("a host with credentials in its environment failed the pre-flight: %v", err)
	}

	if strings.Contains(out, secret) {
		t.Fatalf("the pre-flight printed the secret access key:\n%s", out)
	}

	for _, want := range []string{"AKIDEXAMPLE", "us-west-2", "subnet-0abc", "on-demand"} {
		if !strings.Contains(out, want) {
			t.Errorf("the pre-flight does not report %q:\n%s", want, out)
		}
	}

	// SAID OUT LOUD RATHER THAN INFERRED FROM AN ABSENT KEY. A deployment that
	// expected to run fork pull requests on rented machines and finds them queuing
	// forever has no other way to see why.
	if !strings.Contains(out, "untrusted") {
		t.Errorf("the pre-flight does not say untrusted work will be refused:\n%s", out)
	}
}

// SPOT IS NAMED AS WHAT IT IS. An operator reading a pre-flight should not have
// to know that a reclaimed instance is a failed build rather than a retry.
func TestTheCloudPreflightSaysWhatSpotCosts(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	endpoint := fakeEC2(t, "AKIDEXAMPLE")
	cfg := &config.EC2Config{
		Region:                    "us-west-2",
		Endpoint:                  endpoint,
		SubnetID:                  "subnet-0abc",
		SecurityGroupIDs:          []string{"sg-0abc"},
		UntrustedSecurityGroupIDs: []string{"sg-fork"},
		InstanceTypes:             []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
		Spot:                      true,
		// The probe dials the queue URL's own host, so it points at the fake.
		InterruptionQueueURL: endpoint + "/123456789012/billet",
		NodeName:             "billet",
	}

	var err error

	out := capture(t, func() { err = checkEC2Credentials(t.Context(), wrapEC2(cfg), nil, false, false) })
	if err != nil {
		t.Fatalf("pre-flight: %v", err)
	}

	if !strings.Contains(out, "spot") || !strings.Contains(out, "requeue") {
		t.Errorf("the pre-flight does not say what buying spot costs:\n%s", out)
	}

	// And with a network described for it, the refusal notice is absent — a
	// warning that never goes away is one nobody reads.
	if strings.Contains(out, "untrusted work will be refused") {
		t.Errorf("untrusted work was reported as refused despite having its own group:\n%s", out)
	}
}

func TestCheckResolvesASpotNodesCertificateIdentity(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	serverCfg := writeCAConfig(t, t.TempDir())
	bundleDir := filepath.Join(t.TempDir(), "bundle")
	if err := cmdCAIssue(t.Context(), []string{
		"aws-1", "--config", serverCfg, "--out", bundleDir,
	}); err != nil {
		t.Fatalf("ca issue: %v", err)
	}

	configPath := filepath.Join(t.TempDir(), "billet.yaml")
	fakeEndpoint := fakeEC2(t, "AKIDEXAMPLE")
	body := `
node:
  server_addr: 10.0.0.4:7717
  provider: ec2
  state_dir: ` + t.TempDir() + `
  max_vcpu: 64
  max_memory: 256GiB
  tls:
    cert: ` + filepath.Join(bundleDir, "node.crt") + `
    key: ` + filepath.Join(bundleDir, "node.key") + `
    ca: ` + filepath.Join(bundleDir, "ca.crt") + `
  ec2:
    region: us-west-2
    endpoint: ` + fakeEndpoint + `
    subnet_id: subnet-0abc
    security_group_ids: [sg-0abc]
    spot: true
    interruption_queue_url: ` + fakeEndpoint + `/123456789012/aws-1
    instance_types:
      - type: c7i.2xlarge
        vcpu: 8
        memory: 16GiB
        price_usd_per_hour: 0.34
`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var err error
	stubGitHubUnverifiable(t)
	out := capture(t, func() { err = cmdCheck(t.Context(), []string{"--config", configPath}) })
	if err != nil {
		t.Fatalf("billet check: %v", err)
	}
	if !strings.Contains(out, "node     aws-1") {
		t.Fatalf("check did not report the certificate-derived node identity:\n%s", out)
	}
}

// RESOLVING A CREDENTIAL IS NOT THE SAME AS BEING ABLE TO USE ONE.
//
// An expired key, a key for the wrong account, or a role without ec2 permissions
// all resolve perfectly and then fail on the first job of the day, with a 403
// that names neither. The pre-flight uses the credentials it just reported, so
// what is proved is what was named.
func TestTheCloudPreflightFailsWhenTheCredentialsCannotBeUsed(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	cfg := &config.EC2Config{
		Region:           "us-west-2",
		Endpoint:         fakeEC2(t, "SOME-OTHER-KEY"),
		SubnetID:         "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}

	var err error

	capture(t, func() { err = checkEC2Credentials(t.Context(), wrapEC2(cfg), nil, false, false) })

	if err == nil {
		t.Fatal("credentials that the api refuses were reported as usable")
	}

	if !strings.Contains(err.Error(), "us-west-2") {
		t.Errorf("the error does not say where the call failed: %v", err)
	}
}

// A SECURITY GROUP IN ANOTHER VPC THAN THE SUBNET IS FATAL — a launch would be
// refused by EC2, and config validation cannot see it because it never asked AWS.
func TestPreflightRefusesASecurityGroupInAnotherVPC(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	endpoint := fakeEC2With(t, fakeEC2Topology{accept: "AKIDEXAMPLE", subnetVPC: "vpc-a", groupVPC: "vpc-b"})
	cfg := &config.EC2Config{
		Region:           "us-west-2",
		Endpoint:         endpoint,
		SubnetID:         "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}

	var err error
	capture(t, func() { err = checkEC2Credentials(t.Context(), wrapEC2(cfg), nil, false, false) })
	if err == nil || !strings.Contains(err.Error(), "vpc") {
		t.Fatalf("a group in another vpc was not refused: %v", err)
	}
}

// A CACHE ZONE THAT DOES NOT MATCH THE SUBNET'S IS FATAL — an EBS cache volume
// cannot attach across zones, and only the API knows the subnet's real zone.
func TestPreflightRefusesACacheZoneMismatch(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	endpoint := fakeEC2With(t, fakeEC2Topology{accept: "AKIDEXAMPLE", subnetZone: "us-west-2a"})
	cfg := &config.Config{
		Node: &config.NodeConfig{
			Provider: config.ProviderEC2,
			Site:     "aws",
			EC2: &config.EC2Config{
				Region:           "us-west-2",
				Endpoint:         endpoint,
				SubnetID:         "subnet-0abc",
				SecurityGroupIDs: []string{"sg-0abc"},
				InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
			},
			EBSS3: &config.EBSS3Config{
				Region:           "us-west-2",
				AvailabilityZone: "us-west-2b", // subnet is in 2a
				Bucket:           "billet-cache",
			},
		},
	}

	var err error
	capture(t, func() { err = checkEC2Credentials(t.Context(), cfg, nil, false, false) })
	if err == nil || !strings.Contains(err.Error(), "zone") {
		t.Fatalf("a cache-zone mismatch was not refused: %v", err)
	}
}

// A TIER'S AMI IS RESOLVED: an available one is reported ok, a not-yet-built one
// is a WARNING (not an error), since the staged flow writes a placeholder for
// `billet ami build` to replace.
func TestPreflightReportsTierImages(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	ec2cfg := &config.EC2Config{
		Region:           "us-west-2",
		SubnetID:         "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}
	tier := config.Tier{Label: "cloud", Provider: config.ProviderEC2, Trust: config.WorkloadTrusted, VCPU: 8, Memory: 16 * config.GiB, Image: "ami-good"}

	// Available image.
	ec2cfg.Endpoint = fakeEC2With(t, fakeEC2Topology{accept: "AKIDEXAMPLE", imageState: "available"})
	var err error
	out := capture(t, func() { err = checkEC2Credentials(t.Context(), wrapEC2(ec2cfg, tier), nil, false, false) })
	if err != nil {
		t.Fatalf("preflight with an available image failed: %v", err)
	}
	if !strings.Contains(out, "ami-good available (AMI contract ") {
		t.Errorf("an available AMI was not reported ok:\n%s", out)
	}

	// A not-built AMI is a warning, not an error.
	ec2cfg.Endpoint = fakeEC2With(t, fakeEC2Topology{accept: "AKIDEXAMPLE", imageState: "missing"})
	out = capture(t, func() { err = checkEC2Credentials(t.Context(), wrapEC2(ec2cfg, tier), nil, false, false) })
	if err != nil {
		t.Fatalf("a not-yet-built AMI was treated as fatal: %v", err)
	}
	if !strings.Contains(out, "billet ami build") {
		t.Errorf("a not-built AMI does not point at the build step:\n%s", out)
	}
}

// THE NETWORK PREFLIGHT IS SKIPPED DURING A HOST UPGRADE, so an AWS blip cannot
// roll one back — ONLY the pre-existing reachability call runs. Proven by a fake
// that records every action: the boundary is that the sequence is exactly one
// DescribeInstances, which also catches the gate moving above CheckReachable and
// silently dropping reachability.
func TestPreflightSkippedDuringMaintenance(t *testing.T) {
	// NO credentials in the environment, deliberately: during the upgrade
	// transaction's stopped-service window an AWS or IMDS failure must not
	// read as a broken host, so the probe must succeed with AWS entirely
	// unavailable — zero requests, zero credential resolution.
	var mu sync.Mutex
	var actions []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)

			return
		}
		params, err := url.ParseQuery(string(body))
		if err != nil {
			t.Errorf("parse body: %v", err)

			return
		}

		mu.Lock()
		actions = append(actions, params.Get("Action"))
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	ec2cfg := &config.EC2Config{
		Region:           "us-west-2",
		Endpoint:         srv.URL,
		SubnetID:         "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}

	var checkErr error
	out := capture(t, func() { checkErr = checkEC2Credentials(t.Context(), wrapEC2(ec2cfg), nil, false, true) })
	if checkErr != nil {
		t.Fatalf("the maintenance probe depended on AWS: %v", checkErr)
	}
	if !strings.Contains(out, "skipped during maintenance") {
		t.Errorf("maintenance skip not reported:\n%s", out)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(actions) != 0 {
		t.Errorf("AWS was called during maintenance: %v", actions)
	}
}

// distinctEC2TierAMIs skips non-ec2 tiers, reads each ec2 tier's OWN image
// (including a per-provider launch image), dedups, and keeps first-seen order.
func TestDistinctEC2TierAMIs(t *testing.T) {
	cfg := &config.Config{Tiers: []config.Tier{
		{Label: "docker", Provider: config.ProviderDocker, Image: "docker-img"},
		{Label: "ec2a", Provider: config.ProviderEC2, Image: "ami-1"},
		{Label: "ec2dup", Provider: config.ProviderEC2, Image: "ami-1"},
		{Label: "ec2b", Provider: config.ProviderEC2, Image: "ami-2"},
		{Label: "multi",
			Providers: []config.ProviderKind{config.ProviderEC2, config.ProviderDocker},
			Launch: map[config.ProviderKind]config.TierLaunch{
				config.ProviderEC2:    {Image: "ami-3"},
				config.ProviderDocker: {Image: "docker-img-2"},
			}},
	}}

	got := distinctEC2TierAMIs(cfg)
	want := []string{"ami-1", "ami-2", "ami-3"}
	if !slices.Equal(got, want) {
		t.Errorf("distinctEC2TierAMIs = %v, want %v (docker skipped, deduped, ec2 launch image used)",
			got, want)
	}
}

// A FLEET NODE FILE HAS NO TIERS, so there is no AMI to check; the preflight says
// so rather than passing as if it had checked.
func TestPreflightSaysWhenThereAreNoTiers(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	endpoint := fakeEC2(t, "AKIDEXAMPLE")
	ec2cfg := &config.EC2Config{
		Region:           "us-west-2",
		Endpoint:         endpoint,
		SubnetID:         "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}

	var err error
	out := capture(t, func() { err = checkEC2Credentials(t.Context(), wrapEC2(ec2cfg), nil, false, false) })
	if err != nil {
		t.Fatalf("preflight failed: %v", err)
	}
	if !strings.Contains(out, "no tiers in this file") {
		t.Errorf("a tier-less node file did not say so:\n%s", out)
	}
}

// authorizeConfig is wrapEC2 with a state directory carrying a minted deployment
// id, so ec2Authorize can tag the dry-run as this deployment (a per-deployment IAM
// policy conditions CreateTags on that value; without it the probe would falsely
// fail). Returns the config and the id it minted.
func authorizeConfig(t *testing.T, ec2cfg *config.EC2Config, tiers ...config.Tier) (*config.Config, string) {
	t.Helper()

	dir := t.TempDir()
	id, err := state.DeploymentID(dir)
	if err != nil {
		t.Fatalf("mint deployment id: %v", err)
	}

	cfg := wrapEC2(ec2cfg, tiers...)
	cfg.Node.StateDir = dir

	return cfg, id
}

// --AUTHORIZE DRY-RUNS THE LAUNCH. With every dry run authorized, the preflight
// reports it and passes.
func TestPreflightAuthorizeReportsAuthorized(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	endpoint := fakeEC2With(t, fakeEC2Topology{accept: "AKIDEXAMPLE", imageState: "available", authzCode: "DryRunOperation"})
	ec2cfg := &config.EC2Config{
		Region: "us-west-2", Endpoint: endpoint, SubnetID: "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}
	tier := config.Tier{Label: "cloud", Provider: config.ProviderEC2, Trust: config.WorkloadTrusted, VCPU: 8, Memory: 16 * config.GiB, Image: "ami-good"}
	cfg, _ := authorizeConfig(t, ec2cfg, tier)

	var err error
	out := capture(t, func() { err = checkEC2Credentials(t.Context(), cfg, nil, true, false) })
	if err != nil {
		t.Fatalf("authorize preflight failed unexpectedly: %v", err)
	}
	if !strings.Contains(out, "authz    launch cloud on c7i.2xlarge (trusted): authorized") {
		t.Errorf("did not report an authorized launch:\n%s", out)
	}
	if !strings.Contains(out, "ec2:TerminateInstances cannot be dry-run") {
		t.Errorf("did not note that terminate is not dry-run:\n%s", out)
	}
	// An authorized run REACHED a verdict, so it must NOT also say authority is
	// unproven (which would mean the authorized bit was dropped).
	if strings.Contains(out, "still unproven") {
		t.Errorf("an authorized run wrongly reported authority as unproven:\n%s", out)
	}
}

// THE DRY-RUN TAGS AS THE DEPLOYMENT, not an invented owner: a per-deployment IAM
// policy conditions ec2:CreateTags on that exact value, so tagging anything else
// would make --authorize falsely fail against the policy `billet init iam` writes.
func TestPreflightAuthorizeTagsAsTheDeployment(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	rec := &runRecorder{}
	endpoint := fakeEC2With(t, fakeEC2Topology{accept: "AKIDEXAMPLE", imageState: "available", authzCode: "DryRunOperation", runRec: rec})
	ec2cfg := &config.EC2Config{
		Region: "us-west-2", Endpoint: endpoint, SubnetID: "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}
	tier := config.Tier{Label: "cloud", Provider: config.ProviderEC2, Trust: config.WorkloadTrusted, VCPU: 8, Memory: 16 * config.GiB, Image: "ami-good"}
	cfg, id := authorizeConfig(t, ec2cfg, tier)

	if err := checkEC2Credentials(t.Context(), cfg, nil, true, false); err != nil {
		t.Fatalf("authorize preflight: %v", err)
	}

	bodies := rec.all()
	if len(bodies) == 0 {
		t.Fatal("no RunInstances dry-run was sent")
	}
	// The owner tag value is url-encoded in the request body; the minted id is
	// unique and appears nowhere else, so its presence proves the launch tagged as
	// this deployment. Its ABSENCE (an invented owner) is the P1 the probe avoids.
	for _, b := range bodies {
		if !strings.Contains(b, url.QueryEscape("sh.billet.owner")) {
			t.Fatalf("dry-run carried no owner tag: %s", b)
		}
		if !strings.Contains(b, url.QueryEscape(id)) {
			t.Errorf("dry-run did not tag as the deployment id %q: %s", id, b)
		}
	}
}

// WITHOUT A KNOWN IDENTITY the dry-run cannot tag correctly, so it is skipped with
// a message rather than probing under an invented owner that a per-deployment
// policy would reject — AND it must PEEK, never mint, so the empty state directory
// stays empty (a mutation to state.DeploymentID would mint here and be caught).
func TestPreflightAuthorizeSkipsWithoutIdentity(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	rec := &runRecorder{}
	endpoint := fakeEC2With(t, fakeEC2Topology{accept: "AKIDEXAMPLE", imageState: "available", authzCode: "DryRunOperation", runRec: rec})
	ec2cfg := &config.EC2Config{
		Region: "us-west-2", Endpoint: endpoint, SubnetID: "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}
	tier := config.Tier{Label: "cloud", Provider: config.ProviderEC2, Trust: config.WorkloadTrusted, VCPU: 8, Memory: 16 * config.GiB, Image: "ami-good"}

	// A CONFIGURED but UNINITIALIZED state directory: the resolution loop runs and
	// finds no id, so a mint would be visible as a newly created id file.
	dir := t.TempDir()
	cfg := wrapEC2(ec2cfg, tier)
	cfg.Node.StateDir = dir

	var err error
	out := capture(t, func() { err = checkEC2Credentials(t.Context(), cfg, nil, true, false) })
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if !strings.Contains(out, "identity is not known here yet") {
		t.Errorf("did not say the identity is not known:\n%s", out)
	}
	if strings.Contains(out, "authorized") {
		t.Errorf("a launch was probed without an identity:\n%s", out)
	}
	if len(rec.all()) != 0 {
		t.Errorf("a RunInstances dry-run was sent without an identity: %d", len(rec.all()))
	}
	// PEEK, NOT MINT: the directory must still have no id.
	if _, found, perr := state.PeekDeploymentID(dir); perr != nil || found {
		t.Errorf("the state directory was minted into (found=%v, err=%v)", found, perr)
	}
}

// AN UNAUTHORIZED LAUNCH IS FATAL — a job would be admitted and then fail.
func TestPreflightAuthorizeFailsOnUnauthorized(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	endpoint := fakeEC2With(t, fakeEC2Topology{accept: "AKIDEXAMPLE", imageState: "available", authzCode: "UnauthorizedOperation"})
	ec2cfg := &config.EC2Config{
		Region: "us-west-2", Endpoint: endpoint, SubnetID: "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}
	tier := config.Tier{Label: "cloud", Provider: config.ProviderEC2, Trust: config.WorkloadTrusted, VCPU: 8, Memory: 16 * config.GiB, Image: "ami-good"}
	cfg, _ := authorizeConfig(t, ec2cfg, tier)

	var err error
	out := capture(t, func() { err = checkEC2Credentials(t.Context(), cfg, nil, true, false) })
	if err == nil || !strings.Contains(err.Error(), "NOT authorized") {
		t.Fatalf("an unauthorized launch was not fatal: %v", err)
	}
	if !strings.Contains(out, "NOT AUTHORIZED") {
		t.Errorf("did not report the unauthorized launch:\n%s", out)
	}
}

// A NON-PERMISSION REFUSAL (e.g. a shape not offered in the zone) is INCONCLUSIVE,
// not fatal, and when every probe lands there the preflight says authority is still
// unproven rather than passing as if it had been checked.
func TestPreflightAuthorizeInconclusiveIsNotFatal(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	endpoint := fakeEC2With(t, fakeEC2Topology{accept: "AKIDEXAMPLE", imageState: "available", authzCode: "Unsupported"})
	ec2cfg := &config.EC2Config{
		Region: "us-west-2", Endpoint: endpoint, SubnetID: "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}
	tier := config.Tier{Label: "cloud", Provider: config.ProviderEC2, Trust: config.WorkloadTrusted, VCPU: 8, Memory: 16 * config.GiB, Image: "ami-good"}
	cfg, _ := authorizeConfig(t, ec2cfg, tier)

	var err error
	out := capture(t, func() { err = checkEC2Credentials(t.Context(), cfg, nil, true, false) })
	if err != nil {
		t.Fatalf("an inconclusive launch was fatal: %v", err)
	}
	if !strings.Contains(out, "inconclusive (Unsupported") {
		t.Errorf("did not report the inconclusive launch:\n%s", out)
	}
	if !strings.Contains(out, "launch authority still unproven") {
		t.Errorf("did not say authority is unproven when no verdict was reached:\n%s", out)
	}
}

// WITHOUT --authorize the launch permission is not checked, and it says so.
func TestPreflightWithoutAuthorizeSaysSo(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	endpoint := fakeEC2(t, "AKIDEXAMPLE")
	ec2cfg := &config.EC2Config{
		Region: "us-west-2", Endpoint: endpoint, SubnetID: "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}

	var err error
	out := capture(t, func() { err = checkEC2Credentials(t.Context(), wrapEC2(ec2cfg), nil, false, false) })
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if !strings.Contains(out, "pass --authorize") {
		t.Errorf("did not say launch authority is unchecked:\n%s", out)
	}
}

// reportAuthz's (hard, verdict) pair is what decides fatality and the "unproven"
// note; pin all three mappings directly so a flipped bit fails here.
func TestReportAuthzOutcomes(t *testing.T) {
	cases := []struct {
		name          string
		res           ec2.DryRunResult
		hard, verdict bool
	}{
		{"unauthorized", ec2.DryRunResult{Outcome: ec2.DryRunUnauthorized, Code: "UnauthorizedOperation"}, true, true},
		{"authorized", ec2.DryRunResult{Outcome: ec2.DryRunAuthorized, Code: "DryRunOperation"}, false, true},
		{"inconclusive", ec2.DryRunResult{Outcome: ec2.DryRunInconclusive, Code: "Unsupported"}, false, false},
		// THE ZERO VALUE MUST BE FAIL-SAFE: an uninitialized result must read as
		// "proved nothing" (inconclusive), never as authorized.
		{"zero-value", ec2.DryRunResult{}, false, false},
	}
	for _, c := range cases {
		hard, verdict := reportAuthz("launch x", c.res)
		if hard != c.hard || verdict != c.verdict {
			t.Errorf("%s: got (hard=%v, verdict=%v), want (%v, %v)", c.name, hard, verdict, c.hard, c.verdict)
		}
	}
}

// AN UNTRUSTED TIER WITH NO UNTRUSTED SECURITY GROUPS is refused by the node, so
// its launch must NOT be probed (a probe would land on the VPC default group and
// answer about a request billet never sends). It is skipped with a message, and no
// RunInstances is sent for it.
func TestPreflightAuthorizeSkipsUntrustedWithoutGroups(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	rec := &runRecorder{}
	endpoint := fakeEC2With(t, fakeEC2Topology{accept: "AKIDEXAMPLE", imageState: "available", authzCode: "DryRunOperation", runRec: rec})
	ec2cfg := &config.EC2Config{
		Region: "us-west-2", Endpoint: endpoint, SubnetID: "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"}, // no UntrustedSecurityGroupIDs
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}
	tier := config.Tier{Label: "risky", Provider: config.ProviderEC2, Trust: config.WorkloadUntrusted, VCPU: 8, Memory: 16 * config.GiB, Image: "ami-good"}
	cfg, _ := authorizeConfig(t, ec2cfg, tier)

	var err error
	out := capture(t, func() { err = checkEC2Credentials(t.Context(), cfg, nil, true, false) })
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if !strings.Contains(out, "risky runs untrusted work but node.ec2.untrusted_security_group_ids is empty") {
		t.Errorf("did not skip the unprobeable untrusted tier:\n%s", out)
	}
	if len(rec.all()) != 0 {
		t.Errorf("an untrusted tier with no untrusted groups was probed anyway: %d requests", len(rec.all()))
	}
	// The only tier was SKIPPED, not blocked by an AMI, so the summary must not
	// misdirect to `billet ami build`.
	if strings.Contains(out, "billet ami build") {
		t.Errorf("an all-skipped config was misdirected to build an AMI:\n%s", out)
	}
	if !strings.Contains(out, "every ec2 tier was skipped above") {
		t.Errorf("did not explain that every tier was skipped:\n%s", out)
	}
}

// runProbe is one parsed RunInstances dry-run: the launchable combination it asks
// about. trust is read from which security group the request carries.
type runProbe struct {
	image, shape, trust string
	diskGiB             string
}

func parseRunProbes(t *testing.T, bodies []string) []runProbe {
	t.Helper()

	var probes []runProbe
	for _, b := range bodies {
		v, err := url.ParseQuery(b)
		if err != nil {
			t.Fatalf("parse run body: %v", err)
		}
		// Trust is read from the EXACT security group, and an unrecognized (or
		// missing) group is a failure rather than a default to trusted — a probe
		// that lost its group would otherwise pass as a trusted launch.
		var trust string
		switch v.Get("SecurityGroupId.1") {
		case "sg-trusted":
			trust = "trusted"
		case "sg-untrusted":
			trust = "untrusted"
		default:
			t.Fatalf("run request carried an unexpected security group %q: %s",
				v.Get("SecurityGroupId.1"), b)
		}
		probes = append(probes, runProbe{
			image:   v.Get("ImageId"),
			shape:   v.Get("InstanceType"),
			trust:   trust,
			diskGiB: v.Get("BlockDeviceMapping.1.Ebs.VolumeSize"),
		})
	}

	return probes
}

// THE ENUMERATION IS THE CLAIM: every tier's AMI, on its trust's network, at its
// disk, for every fitting shape — deduped, and never an undersized shape. Drive the
// full matrix and compare the exact set of dry-runs sent.
func TestPreflightAuthorizeEnumeratesEveryLaunchableCombination(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	rec := &runRecorder{}
	endpoint := fakeEC2With(t, fakeEC2Topology{accept: "AKIDEXAMPLE", imageState: "available", authzCode: "DryRunOperation", runRec: rec})
	ec2cfg := &config.EC2Config{
		Region: "us-west-2", Endpoint: endpoint, SubnetID: "subnet-0abc",
		SecurityGroupIDs:          []string{"sg-trusted"},
		UntrustedSecurityGroupIDs: []string{"sg-untrusted"},
		InstanceTypes: []config.EC2InstanceType{
			{Type: "small", VCPU: 2, Memory: 4 * config.GiB},
			{Type: "big", VCPU: 8, Memory: 16 * config.GiB},
		},
	}
	tiers := []config.Tier{
		// A: trusted, ami-a, 20GiB, fits both shapes.
		{Label: "a", Provider: config.ProviderEC2, Trust: config.WorkloadTrusted, VCPU: 2, Memory: 4 * config.GiB, Disk: 20 * config.GiB, Image: "ami-a"},
		// A2: an exact duplicate of A — must be deduped, adding no probe.
		{Label: "a2", Provider: config.ProviderEC2, Trust: config.WorkloadTrusted, VCPU: 2, Memory: 4 * config.GiB, Disk: 20 * config.GiB, Image: "ami-a"},
		// B: trusted, ami-b, 40GiB, fits ONLY big (small is undersized).
		{Label: "b", Provider: config.ProviderEC2, Trust: config.WorkloadTrusted, VCPU: 8, Memory: 16 * config.GiB, Disk: 40 * config.GiB, Image: "ami-b"},
		// C: untrusted, ami-a, 20GiB, fits both — different trust key from A.
		{Label: "c", Provider: config.ProviderEC2, Trust: config.WorkloadUntrusted, VCPU: 2, Memory: 4 * config.GiB, Disk: 20 * config.GiB, Image: "ami-a"},
	}
	cfg, _ := authorizeConfig(t, ec2cfg, tiers...)

	if err := checkEC2Credentials(t.Context(), cfg, nil, true, false); err != nil {
		t.Fatalf("preflight: %v", err)
	}

	got := map[runProbe]int{}
	for _, p := range parseRunProbes(t, rec.all()) {
		got[p]++
	}

	want := []runProbe{
		{image: "ami-a", shape: "small", trust: "trusted", diskGiB: "20"},
		{image: "ami-a", shape: "big", trust: "trusted", diskGiB: "20"},
		{image: "ami-b", shape: "big", trust: "trusted", diskGiB: "40"},
		{image: "ami-a", shape: "small", trust: "untrusted", diskGiB: "20"},
		{image: "ami-a", shape: "big", trust: "untrusted", diskGiB: "20"},
	}

	if len(got) != len(want) {
		t.Fatalf("probed %d distinct combinations, want %d: %+v", len(got), len(want), got)
	}
	for _, w := range want {
		if got[w] != 1 {
			t.Errorf("combination %+v was probed %d times, want exactly 1", w, got[w])
		}
	}
	// The undersized small-on-b combination must never appear (ami-b + small).
	if got[runProbe{image: "ami-b", shape: "small", trust: "trusted", diskGiB: "40"}] != 0 {
		t.Error("an undersized shape (small on tier b) was probed")
	}
}

// THE PROBE OWNER COMES FROM THE CERTIFICATE, not the state directory: an enrolled
// node whose service has not started yet has its deployment id only in its TLS
// bundle (the state dir is empty), and that is exactly the host `--authorize` is
// run on before first start. The dry-run must tag as the certificate's deployment,
// or it falsely fails against a per-deployment policy.
func TestPreflightAuthorizeUsesCertificateIdentity(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	serverCfg := writeCAConfig(t, t.TempDir())
	bundleDir := filepath.Join(t.TempDir(), "bundle")
	if err := cmdCAIssue(t.Context(), []string{
		"aws-1", "--config", serverCfg, "--out", bundleDir,
	}); err != nil {
		t.Fatalf("ca issue: %v", err)
	}

	bundle, err := wirecert.LoadBundle(
		filepath.Join(bundleDir, "node.crt"),
		filepath.Join(bundleDir, "node.key"),
		filepath.Join(bundleDir, "ca.crt"))
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	wantOwner, err := bundle.Deployment()
	if err != nil {
		t.Fatalf("bundle deployment: %v", err)
	}

	rec := &runRecorder{}
	endpoint := fakeEC2With(t, fakeEC2Topology{accept: "AKIDEXAMPLE", imageState: "available", authzCode: "DryRunOperation", runRec: rec})

	// A DECOY id in the node state directory: the certificate must OUTRANK it, so a
	// regression that reads the state dir first would tag this value and be caught.
	stateDir := t.TempDir()
	decoyOwner, err := state.DeploymentID(stateDir)
	if err != nil {
		t.Fatalf("mint decoy id: %v", err)
	}
	if decoyOwner == wantOwner {
		t.Fatal("decoy id collided with the certificate id; rerun")
	}

	configPath := filepath.Join(t.TempDir(), "billet.yaml")
	body := `
node:
  server_addr: 10.0.0.4:7717
  provider: ec2
  state_dir: ` + stateDir + `
  max_vcpu: 64
  max_memory: 256GiB
  tls:
    cert: ` + filepath.Join(bundleDir, "node.crt") + `
    key: ` + filepath.Join(bundleDir, "node.key") + `
    ca: ` + filepath.Join(bundleDir, "ca.crt") + `
  ec2:
    region: us-west-2
    endpoint: ` + endpoint + `
    subnet_id: subnet-0abc
    security_group_ids: [sg-0abc]
    instance_types:
      - type: c7i.2xlarge
        vcpu: 8
        memory: 16GiB
        price_usd_per_hour: 0.34
tiers:
  - label: cloud
    trust: trusted
    providers: [ec2]
    vcpu: 8
    memory: 16GiB
    image: ami-good
    runner_group: billet-cloud
    workflows: [acme/repo/.github/workflows/ci.yml@refs/heads/main]
`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	stubGitHubUnverifiable(t)
	if err := cmdCheck(t.Context(), []string{"--config", configPath, "--authorize"}); err != nil {
		t.Fatalf("billet check --authorize: %v", err)
	}

	bodies := rec.all()
	if len(bodies) == 0 {
		t.Fatal("the certificate-identified node sent no dry-run — the cert owner was not resolved")
	}
	for _, b := range bodies {
		if !strings.Contains(b, url.QueryEscape(wantOwner)) {
			t.Errorf("dry-run did not tag as the certificate's deployment %q: %s", wantOwner, b)
		}
		if strings.Contains(b, url.QueryEscape(decoyOwner)) {
			t.Errorf("dry-run tagged the state-dir decoy %q instead of the certificate: %s", decoyOwner, b)
		}
	}
}

// A FLEET NODE FILE (no tiers) with --authorize must say launch authority is
// unproven because its tiers live on the control plane — NOT misdirect to
// `billet ami build`. The prior tierless test ran with authorize=false, so this
// branch had no executing coverage.
func TestPreflightAuthorizeFleetNodeSaysUnproven(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	rec := &runRecorder{}
	endpoint := fakeEC2With(t, fakeEC2Topology{accept: "AKIDEXAMPLE", imageState: "available", authzCode: "DryRunOperation", runRec: rec})
	ec2cfg := &config.EC2Config{
		Region: "us-west-2", Endpoint: endpoint, SubnetID: "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}
	cfg, _ := authorizeConfig(t, ec2cfg) // no tiers

	var err error
	out := capture(t, func() { err = checkEC2Credentials(t.Context(), cfg, nil, true, false) })
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if !strings.Contains(out, "this file declares no ec2 tiers") {
		t.Errorf("did not explain the fleet-node case:\n%s", out)
	}
	if strings.Contains(out, "billet ami build") {
		t.Errorf("a fleet node was misdirected to build an AMI:\n%s", out)
	}
	if len(rec.all()) != 0 {
		t.Errorf("a fleet node with no tiers probed a launch anyway: %d", len(rec.all()))
	}
}

// A MIXED CONFIG — one tier skipped for a missing untrusted network, another whose
// AMI is unresolvable — must name BOTH remedies, not falsely claim every tier was
// skipped (which would suppress the AMI-build guidance).
func TestPreflightAuthorizeMixedSkipAndUnresolvableAMI(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	rec := &runRecorder{}
	endpoint := fakeEC2With(t, fakeEC2Topology{
		accept: "AKIDEXAMPLE", authzCode: "DryRunOperation", runRec: rec,
		unavailAMI: map[string]bool{"ami-missing": true}, // ami-ok resolves, ami-missing does not
	})
	ec2cfg := &config.EC2Config{
		Region: "us-west-2", Endpoint: endpoint, SubnetID: "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"}, // no untrusted groups
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}
	tiers := []config.Tier{
		// Untrusted with a RESOLVABLE ami but no untrusted network -> skipped.
		{Label: "risky", Provider: config.ProviderEC2, Trust: config.WorkloadUntrusted, VCPU: 8, Memory: 16 * config.GiB, Image: "ami-ok"},
		// Trusted but its ami is unresolvable -> unresolvable.
		{Label: "cloud", Provider: config.ProviderEC2, Trust: config.WorkloadTrusted, VCPU: 8, Memory: 16 * config.GiB, Image: "ami-missing"},
	}
	cfg, _ := authorizeConfig(t, ec2cfg, tiers...)

	var err error
	out := capture(t, func() { err = checkEC2Credentials(t.Context(), cfg, nil, true, false) })
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if len(rec.all()) != 0 {
		t.Errorf("nothing was probeable, yet a launch was dry-run: %d", len(rec.all()))
	}
	// The summary must NOT claim every tier was skipped (the AMI tier was not).
	if strings.Contains(out, "every ec2 tier was skipped above") {
		t.Errorf("misreported a mixed config as all-skipped:\n%s", out)
	}
	// It must point at BOTH remedies.
	if !strings.Contains(out, "billet ami build") || !strings.Contains(out, "give untrusted tiers a network") {
		t.Errorf("did not name both remedies for the mixed config:\n%s", out)
	}
}

// A SINGLE TIER WITH BOTH BLOCKERS — an unresolvable AMI AND untrusted work with no
// untrusted network — must count as both, so the summary names both remedies rather
// than sending the operator to build an AMI only to hit the network gap on rerun.
func TestPreflightAuthorizeOneTierBothBlockers(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	rec := &runRecorder{}
	endpoint := fakeEC2With(t, fakeEC2Topology{
		accept: "AKIDEXAMPLE", authzCode: "DryRunOperation", runRec: rec,
		unavailAMI: map[string]bool{"ami-missing": true},
	})
	ec2cfg := &config.EC2Config{
		Region: "us-west-2", Endpoint: endpoint, SubnetID: "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"}, // no untrusted groups
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}
	// One tier: untrusted (no network) AND its AMI is unresolvable.
	tier := config.Tier{Label: "doomed", Provider: config.ProviderEC2, Trust: config.WorkloadUntrusted, VCPU: 8, Memory: 16 * config.GiB, Image: "ami-missing"}
	cfg, _ := authorizeConfig(t, ec2cfg, tier)

	var err error
	out := capture(t, func() { err = checkEC2Credentials(t.Context(), cfg, nil, true, false) })
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if len(rec.all()) != 0 {
		t.Errorf("a doomed tier was probed anyway: %d", len(rec.all()))
	}
	// The network remedy must NOT be suppressed by the AMI blocker.
	if strings.Contains(out, "every ec2 tier was skipped above") {
		t.Errorf("misreported a both-blockers tier as all-skipped:\n%s", out)
	}
	if !strings.Contains(out, "billet ami build") || !strings.Contains(out, "give untrusted tiers a network") {
		t.Errorf("did not name both remedies for a tier with both blockers:\n%s", out)
	}
	// The per-tier skip line must still identify the tier by name — a mutation that
	// drops it only when the AMI is also bad would otherwise survive on the summary.
	if !strings.Contains(out, "doomed runs untrusted work but node.ec2.untrusted_security_group_ids") {
		t.Errorf("did not name the specific tier that lacks an untrusted network:\n%s", out)
	}
}

// THE INSTANCE-PROFILE BANDS THROUGH THE REAL PREFLIGHT: found reports, denied
// stays advisory (the CHECKING identity's limitation, not the profile's), and
// missing is fatal — a trusted job's launch will fail on it.
func TestPreflightChecksTheInstanceProfile(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	build := func(profileState string) *config.EC2Config {
		endpoint := fakeEC2With(t, fakeEC2Topology{accept: "AKIDEXAMPLE", profile: profileState})
		iamEndpointOverride = endpoint
		t.Cleanup(func() { iamEndpointOverride = "" })

		return &config.EC2Config{
			Region:                    "us-west-2",
			Endpoint:                  endpoint,
			SubnetID:                  "subnet-0abc",
			SecurityGroupIDs:          []string{"sg-0abc"},
			UntrustedSecurityGroupIDs: []string{"sg-fork"},
			InstanceTypes:             []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
			InstanceProfile:           "billet-jobs",
			NodeName:                  "billet",
		}
	}

	var err error
	out := capture(t, func() { err = checkEC2Credentials(t.Context(), wrapEC2(build("")), nil, false, false) })
	if err != nil {
		t.Fatalf("a present profile failed the preflight: %v", err)
	}
	if !strings.Contains(out, "billet-jobs exists") {
		t.Errorf("a present profile is not reported:\n%s", out)
	}

	out = capture(t, func() { err = checkEC2Credentials(t.Context(), wrapEC2(build("denied")), nil, false, false) })
	if err != nil {
		t.Fatalf("an IAM-denied CHECK failed the preflight; it says nothing about the profile: %v", err)
	}
	if !strings.Contains(out, "could not be checked") {
		t.Errorf("the denied band is not reported as unknown:\n%s", out)
	}

	capture(t, func() { err = checkEC2Credentials(t.Context(), wrapEC2(build("missing")), nil, false, false) })
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a missing profile was not fatal by name: %v", err)
	}
}

// A DENIED QUEUE PROBE IS A FACT ABOUT THE CHECKER: a role provisioned before
// sqs:GetQueueAttributes joined the generated grant consumes warnings
// perfectly well, so the probe must stay advisory — a fatal here would fail
// billet check on every pre-upgrade spot node and block the Ansible converge
// that gates on it.
func TestPreflightQueueDenialStaysAdvisory(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	endpoint := fakeEC2With(t, fakeEC2Topology{accept: "AKIDEXAMPLE", queueDenied: true})
	cfg := &config.EC2Config{
		Region:                    "us-west-2",
		Endpoint:                  endpoint,
		SubnetID:                  "subnet-0abc",
		SecurityGroupIDs:          []string{"sg-0abc"},
		UntrustedSecurityGroupIDs: []string{"sg-fork"},
		InstanceTypes:             []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
		Spot:                      true,
		InterruptionQueueURL:      endpoint + "/123456789012/billet",
		NodeName:                  "billet",
	}

	var err error
	out := capture(t, func() { err = checkEC2Credentials(t.Context(), wrapEC2(cfg), nil, false, false) })
	if err != nil {
		t.Fatalf("an access-denied queue probe failed the preflight: %v", err)
	}
	for _, must := range []string{"INCONCLUSIVE", "sqs:GetQueueAttributes"} {
		if !strings.Contains(out, must) {
			t.Errorf("the advisory does not carry %q:\n%s", must, out)
		}
	}
}

// THE REGION TRAP IS NAMED IN THE FAILURE ITSELF: an opted-out region answers
// with credential prose, and an operator whose key works elsewhere must be
// pointed at the region before they rotate credentials that were never wrong.
func TestReachabilityFailureNamesTheRegionTrap(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	cfg := &config.EC2Config{
		Region:           "af-south-1",
		Endpoint:         fakeEC2With(t, fakeEC2Topology{accept: "AKIDEXAMPLE", authFailure: true}),
		SubnetID:         "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}

	var err error
	capture(t, func() { err = checkEC2Credentials(t.Context(), wrapEC2(cfg), nil, false, false) })
	if err == nil {
		t.Fatal("an unreachable region passed the preflight")
	}
	if !strings.Contains(err.Error(), "not enabled on this account") {
		t.Errorf("the failure does not name the region trap: %v", err)
	}
}

// TestAnImageBelowTheContractIsReported covers the state every AMI in service was
// in when this was written: built before billet stamped its output, so it answers
// nothing about what made it and may carry Docker's containerd image store, which
// makes the cache publish with no images in it.
//
// BOTH DIRECTIONS, because a one-sided test here is worthless. A check that warns
// about everything is as useless as one that warns about nothing, and the failure
// this guards against — an operator learning to ignore the line — needs the
// current-contract case to stay quiet.
func TestAnImageBelowTheContractIsReported(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	ec2cfg := &config.EC2Config{
		Region:           "us-west-2",
		SubnetID:         "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}

	tier := config.Tier{
		Label: "cloud", Provider: config.ProviderEC2, Trust: config.WorkloadTrusted,
		VCPU: 8, Memory: 16 * config.GiB, Image: "ami-good",
	}

	ec2cfg.Endpoint = fakeEC2With(t, fakeEC2Topology{
		accept: "AKIDEXAMPLE", imageState: "available", untaggedImage: true,
	})

	var err error
	out := capture(t, func() {
		err = checkEC2Credentials(t.Context(), wrapEC2(ec2cfg, tier), nil, false, false)
	})

	// NOT FATAL. A wrong image store loses the cache and runs jobs correctly, so
	// refusing here would strand a working fleet over a cold cache.
	if err != nil {
		t.Fatalf("an image below the contract made the preflight fail; it must warn "+
			"and let the deployment run: %v", err)
	}

	for _, want := range []string{
		"AMI contract",
		"billet ami build",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not mention %q, so an operator has no way to learn "+
				"their cache is silently empty:\n%s", want, out)
		}
	}

	// And a stamped image must NOT produce it.
	ec2cfg.Endpoint = fakeEC2With(t, fakeEC2Topology{
		accept: "AKIDEXAMPLE", imageState: "available",
	})

	current := capture(t, func() {
		err = checkEC2Credentials(t.Context(), wrapEC2(ec2cfg, tier), nil, false, false)
	})
	if err != nil {
		t.Fatalf("preflight with a current image failed: %v", err)
	}

	if strings.Contains(current, "billet ami build") {
		t.Errorf("an image at the current contract is still told to rebuild, so the "+
			"warning fires for everyone and stops meaning anything:\n%s", current)
	}
}
