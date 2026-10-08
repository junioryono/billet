package fleetops

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
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

	"github.com/junioryono/billet/internal/provider/ec2"
)

// Helpers this package's tests share with cmd/billet's, copied rather than
// imported: a test helper is not part of any package's API.

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

// writeTargetConfig writes a control-plane config whose github block is the
// given scope line, with an untrusted docker tier, and a throwaway App key.
func writeTargetConfig(t *testing.T, scope string, extra string) string {
	t.Helper()

	dir := t.TempDir()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}

	keyPath := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600); err != nil {
		t.Fatalf("write the key: %v", err)
	}

	extraKey := filepath.Join(dir, "app-personal.pem")
	if err := os.WriteFile(extraKey, pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600); err != nil {
		t.Fatalf("write the second key: %v", err)
	}

	cfgPath := filepath.Join(dir, "billet.yaml")
	body := fmt.Sprintf(`server:
  listen: 127.0.0.1:7717
  state_dir: %s
  max_vcpu: 8
  max_memory: 32GiB
github:
%s
  app_id: 7
  installation_id: 42
  private_key_path: %s
tiers:
  - label: billet-4vcpu
    provider: docker
    vcpu: 4
    memory: 16GiB
    image: ghcr.io/actions/actions-runner:latest
    trust: untrusted
%s`, filepath.Join(dir, "server"), scope, keyPath, strings.ReplaceAll(extra, "EXTRA_KEY", extraKey))
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	return cfgPath
}
