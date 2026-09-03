package ec2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// authorizeProvider builds a provider whose EC2 API is a fake that answers a
// RunInstances DryRun with `code`. It fails the test on any TerminateInstances
// request: DryRunLaunch must never dry-run a teardown (it cannot be proved without
// a real instance), so an accidental terminate probe is a regression.
func authorizeProvider(t *testing.T, code string) *Provider {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)

		// Building the RunInstances request reads the AMI's block devices first;
		// answer that validly so the dry run is reached.
		if strings.Contains(body, "Action=DescribeImages") {
			fmt.Fprint(w, `<DescribeImagesResponse><imagesSet><item>`+
				`<imageId>ami-1</imageId><rootDeviceName>/dev/xvda</rootDeviceName>`+
				`<rootDeviceType>ebs</rootDeviceType><imageState>available</imageState>`+
				`<blockDeviceMapping><item><deviceName>/dev/xvda</deviceName>`+
				`<ebs><deleteOnTermination>true</deleteOnTermination></ebs></item></blockDeviceMapping>`+
				`</item></imagesSet></DescribeImagesResponse>`)

			return
		}

		if strings.Contains(body, "Action=TerminateInstances") {
			t.Errorf("DryRunLaunch must not dry-run a teardown: %s", body)
		}

		// The dry run itself must carry DryRun=true and be a launch.
		if !strings.Contains(body, "DryRun=true") {
			t.Errorf("authorization request is not a dry run: %s", body)
		}
		if !strings.Contains(body, "Action=RunInstances") {
			t.Errorf("unexpected authorization action: %s", body)
		}

		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `<Response><Errors><Error><Code>%s</Code><Message>x</Message>`+
			`</Error></Errors></Response>`, code)
	}))
	t.Cleanup(srv.Close)

	p, err := New("deployment", config.EC2Config{
		Region:                    "us-west-2",
		Endpoint:                  srv.URL,
		SubnetID:                  "subnet-1",
		SecurityGroupIDs:          []string{"sg-trusted"},
		UntrustedSecurityGroupIDs: []string{"sg-untrusted"},
		InstanceTypes:             []config.EC2InstanceType{{Type: "c7i.large", VCPU: 2, Memory: 4 * config.GiB}},
	}, WithHTTPClient(srv.Client()), WithCredentials(awscreds.Static{
		AccessKeyID: "AKID", SecretAccessKey: "s",
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return p
}

func TestDryRunLaunchClassifies(t *testing.T) {
	shape := config.EC2InstanceType{Type: "c7i.large", VCPU: 2, Memory: 4 * config.GiB}

	cases := map[string]DryRunOutcome{
		"DryRunOperation":       DryRunAuthorized,
		"UnauthorizedOperation": DryRunUnauthorized,
		"InvalidAMIID.NotFound": DryRunInconclusive,
		"InvalidParameterValue": DryRunInconclusive,
	}

	for code, want := range cases {
		p := authorizeProvider(t, code)
		res, err := p.DryRunLaunch(t.Context(), "ami-1", provider.TrustTrusted, shape, 20*config.GiB)
		if err != nil {
			t.Fatalf("DryRunLaunch(%s): %v", code, err)
		}
		if res.Outcome != want {
			t.Errorf("code %s -> outcome %d, want %d", code, res.Outcome, want)
		}
	}
}

// AN UNTRUSTED DRY RUN USES THE UNTRUSTED SECURITY GROUP, so a role authorized for
// the trusted network but not the untrusted one is caught.
func TestDryRunLaunchUntrustedUsesUntrustedNetwork(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := readBody(t, r)
		if strings.Contains(b, "Action=DescribeImages") {
			fmt.Fprint(w, `<DescribeImagesResponse><imagesSet><item>`+
				`<imageId>ami-1</imageId><rootDeviceName>/dev/xvda</rootDeviceName>`+
				`<rootDeviceType>ebs</rootDeviceType><imageState>available</imageState></item>`+
				`</imagesSet></DescribeImagesResponse>`)

			return
		}
		body = b
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<Response><Errors><Error><Code>DryRunOperation</Code><Message>x</Message></Error></Errors></Response>`)
	}))
	t.Cleanup(srv.Close)

	p, err := New("deployment", config.EC2Config{
		Region: "us-west-2", Endpoint: srv.URL, SubnetID: "subnet-1",
		SecurityGroupIDs:          []string{"sg-trusted"},
		UntrustedSecurityGroupIDs: []string{"sg-untrusted"},
		InstanceTypes:             []config.EC2InstanceType{{Type: "c7i.large", VCPU: 2, Memory: 4 * config.GiB}},
	}, WithHTTPClient(srv.Client()), WithCredentials(awscreds.Static{AccessKeyID: "AKID", SecretAccessKey: "s"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	shape := config.EC2InstanceType{Type: "c7i.large", VCPU: 2, Memory: 4 * config.GiB}
	if _, err := p.DryRunLaunch(t.Context(), "ami-1", provider.TrustUntrusted, shape, 0); err != nil {
		t.Fatalf("DryRunLaunch: %v", err)
	}
	if !strings.Contains(body, "sg-untrusted") || strings.Contains(body, "sg-trusted") {
		t.Errorf("untrusted dry run did not use the untrusted group: %s", body)
	}
}

// A DRYRUN THAT SUCCEEDS MEANS A REAL INSTANCE LAUNCHED — the DryRun parameter was
// not honored (a proxy stripped it) — so DryRunLaunch must NOT report authorized;
// it must terminate the instance it accidentally started and fail loudly.
func TestDryRunLaunchSuccessTerminatesAndFails(t *testing.T) {
	var terminated string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		switch {
		case strings.Contains(body, "Action=DescribeImages"):
			fmt.Fprint(w, `<DescribeImagesResponse><imagesSet><item>`+
				`<imageId>ami-1</imageId><rootDeviceName>/dev/xvda</rootDeviceName>`+
				`<rootDeviceType>ebs</rootDeviceType><imageState>available</imageState></item>`+
				`</imagesSet></DescribeImagesResponse>`)
		case strings.Contains(body, "Action=RunInstances"):
			// A 200 despite DryRun=true: the parameter was not honored and an
			// instance really started.
			fmt.Fprint(w, `<RunInstancesResponse><instancesSet><item>`+
				`<instanceId>i-deadbeef</instanceId></item></instancesSet></RunInstancesResponse>`)
		case strings.Contains(body, "Action=TerminateInstances"):
			// PRODUCTION TERMINATES FOR REAL — a DryRun terminate would leave the
			// accidentally-launched instance alive, so a DryRun here is a regression.
			if strings.Contains(body, "DryRun=true") {
				t.Errorf("containment terminate must NOT be a dry run: %s", body)
			}
			terminated = body
			fmt.Fprint(w, `<TerminateInstancesResponse/>`)
		default:
			t.Errorf("unexpected action: %s", body)
		}
	}))
	t.Cleanup(srv.Close)

	p, err := New("deployment", config.EC2Config{
		Region: "us-west-2", Endpoint: srv.URL, SubnetID: "subnet-1",
		SecurityGroupIDs: []string{"sg-trusted"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.large", VCPU: 2, Memory: 4 * config.GiB}},
	}, WithHTTPClient(srv.Client()), WithCredentials(awscreds.Static{AccessKeyID: "AKID", SecretAccessKey: "s"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	shape := config.EC2InstanceType{Type: "c7i.large", VCPU: 2, Memory: 4 * config.GiB}
	res, err := p.DryRunLaunch(t.Context(), "ami-1", provider.TrustTrusted, shape, 0)
	if err == nil {
		t.Fatalf("a mis-honored DryRun must be an error, got outcome %d", res.Outcome)
	}
	if !strings.Contains(terminated, "i-deadbeef") {
		t.Errorf("the accidentally-launched instance was not terminated; terminate body: %q", terminated)
	}
	// TerminateInstances is asynchronous, so the message must say "requested", not
	// claim the instance is already gone.
	if !strings.Contains(err.Error(), "requested termination") {
		t.Errorf("the error overclaims a completed teardown: %v", err)
	}
}

// The trust class picks the network AND the instance profile: a trusted dry-run
// carries iam:PassRole (IamInstanceProfile), an untrusted one must not.
func TestDryRunLaunchProfileFollowsTrust(t *testing.T) {
	capture := func(t *testing.T, trust provider.TrustClass) string {
		t.Helper()

		var body string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b := readBody(t, r)
			if strings.Contains(b, "Action=DescribeImages") {
				fmt.Fprint(w, `<DescribeImagesResponse><imagesSet><item>`+
					`<imageId>ami-1</imageId><rootDeviceName>/dev/xvda</rootDeviceName>`+
					`<rootDeviceType>ebs</rootDeviceType><imageState>available</imageState></item>`+
					`</imagesSet></DescribeImagesResponse>`)

				return
			}
			body = b
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `<Response><Errors><Error><Code>DryRunOperation</Code>`+
				`<Message>x</Message></Error></Errors></Response>`)
		}))
		t.Cleanup(srv.Close)

		p, err := New("deployment", config.EC2Config{
			Region: "us-west-2", Endpoint: srv.URL, SubnetID: "subnet-1",
			SecurityGroupIDs:          []string{"sg-trusted"},
			UntrustedSecurityGroupIDs: []string{"sg-untrusted"},
			InstanceProfile:           "billet-runner",
			InstanceTypes:             []config.EC2InstanceType{{Type: "c7i.large", VCPU: 2, Memory: 4 * config.GiB}},
		}, WithHTTPClient(srv.Client()), WithCredentials(awscreds.Static{AccessKeyID: "AKID", SecretAccessKey: "s"}))
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		shape := config.EC2InstanceType{Type: "c7i.large", VCPU: 2, Memory: 4 * config.GiB}
		if _, err := p.DryRunLaunch(t.Context(), "ami-1", trust, shape, 0); err != nil {
			t.Fatalf("DryRunLaunch: %v", err)
		}

		return body
	}

	if b := capture(t, provider.TrustTrusted); !strings.Contains(b, "IamInstanceProfile") {
		t.Errorf("trusted dry-run must carry the instance profile: %s", b)
	}
	if b := capture(t, provider.TrustUntrusted); strings.Contains(b, "IamInstanceProfile") {
		t.Errorf("untrusted dry-run must NOT carry the instance profile: %s", b)
	}
}

// A 200 THAT NAMES NO INSTANCE still means a launch happened; when the Name-tag
// recovery also finds nothing, DryRunLaunch must NOT claim it terminated anything —
// it must report an unidentified live orphan and name the tags to search for.
func TestDryRunLaunchSuccessEmptyIDsReportsUnidentifiedOrphan(t *testing.T) {
	waits := countingRecovery(t)

	wantName := authorizeName("ami-1", provider.TrustTrusted, 0)
	describes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		switch {
		case strings.Contains(body, "Action=DescribeImages"):
			fmt.Fprint(w, `<DescribeImagesResponse><imagesSet><item>`+
				`<imageId>ami-1</imageId><rootDeviceName>/dev/xvda</rootDeviceName>`+
				`<rootDeviceType>ebs</rootDeviceType><imageState>available</imageState></item>`+
				`</imagesSet></DescribeImagesResponse>`)
		case strings.Contains(body, "Action=RunInstances"):
			// A structurally valid 200 carrying NO instance id.
			fmt.Fprint(w, `<RunInstancesResponse><instancesSet/></RunInstancesResponse>`)
		case strings.Contains(body, "Action=DescribeInstances"):
			describes++
			// The recovery must search for the EXACT probe Name (production would
			// find nothing under any other) — assert it, then answer empty.
			if got := requestedNameFilter(t, body); got != wantName {
				t.Errorf("recovery searched Name %q, want %q", got, wantName)
			}
			fmt.Fprint(w, `<DescribeInstancesResponse><reservationSet/></DescribeInstancesResponse>`)
		case strings.Contains(body, "Action=TerminateInstances"):
			t.Errorf("nothing was identified, so nothing should be terminated: %s", body)
		default:
			t.Errorf("unexpected action: %s", body)
		}
	}))
	t.Cleanup(srv.Close)

	p, err := New("deployment", config.EC2Config{
		Region: "us-west-2", Endpoint: srv.URL, SubnetID: "subnet-1",
		SecurityGroupIDs: []string{"sg-trusted"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.large", VCPU: 2, Memory: 4 * config.GiB}},
	}, WithHTTPClient(srv.Client()), WithCredentials(awscreds.Static{AccessKeyID: "AKID", SecretAccessKey: "s"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	shape := config.EC2InstanceType{Type: "c7i.large", VCPU: 2, Memory: 4 * config.GiB}
	_, err = p.DryRunLaunch(t.Context(), "ami-1", provider.TrustTrusted, shape, 0)
	if err == nil {
		t.Fatal("a mis-honored DryRun with no decoded id must be an error")
	}
	if !strings.Contains(err.Error(), "could not identify") || !strings.Contains(err.Error(), "may be RUNNING") {
		t.Errorf("did not report an unidentified live orphan: %v", err)
	}
	// The give-up path must make the full number of attempts before quitting, so
	// EC2 has time to become consistent. Pinned to the LITERAL policy value (5), not
	// the constant — comparing against launchRecoveryAttempts would move with a
	// mutation and never catch a shortened retry budget.
	const wantAttempts = 5
	if launchRecoveryAttempts != wantAttempts {
		t.Fatalf("the recovery policy changed to %d attempts; update this test's expectation",
			launchRecoveryAttempts)
	}
	if describes != wantAttempts {
		t.Errorf("recovery made %d describes, want the full %d attempts", describes, wantAttempts)
	}
	// Exactly one fewer wait than attempts: the loop must NOT sleep after its final
	// describe (removing that break would make waits == attempts here).
	if *waits != wantAttempts-1 {
		t.Errorf("recovery waited %d times, want %d (no wait after the final attempt)",
			*waits, wantAttempts-1)
	}
}

// requestedNameFilter extracts the tag:Name filter value from a DescribeInstances
// query body, so a test can assert the recovery searches for the exact probe Name.
func requestedNameFilter(t *testing.T, body string) string {
	t.Helper()

	v, err := url.ParseQuery(body)
	if err != nil {
		t.Fatalf("parse describe body: %v", err)
	}
	for i := 1; ; i++ {
		key := "Filter." + strconv.Itoa(i) + ".Name"
		name := v.Get(key)
		if name == "" {
			return ""
		}
		if name == "tag:"+nameTag {
			return v.Get("Filter." + strconv.Itoa(i) + ".Value.1")
		}
	}
}

// CONTAINMENT OUTLIVES A CANCELLED DIAGNOSTIC: a Ctrl-C after the launch commits
// must not abandon the instance, so the teardown runs on a detached context.
func TestContainMishonoredLaunchSurvivesCancel(t *testing.T) {
	var terminated string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		if strings.Contains(body, "Action=TerminateInstances") {
			terminated = body
			fmt.Fprint(w, `<TerminateInstancesResponse/>`)

			return
		}
		t.Errorf("unexpected action: %s", body)
	}))
	t.Cleanup(srv.Close)

	p, err := New("deployment", config.EC2Config{
		Region: "us-west-2", Endpoint: srv.URL, SubnetID: "subnet-1",
		SecurityGroupIDs: []string{"sg-trusted"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.large", VCPU: 2, Memory: 4 * config.GiB}},
	}, WithHTTPClient(srv.Client()), WithCredentials(awscreds.Static{AccessKeyID: "AKID", SecretAccessKey: "s"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// A context already cancelled, as it would be after the operator aborts.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	out := runInstancesResponse{Instances: []instanceItem{{InstanceID: "i-cafef00d"}}}
	if err := p.containMishonoredLaunch(ctx, "billet-preflight-authorize-x", out); err == nil {
		t.Fatal("containment must still return the loud error")
	}
	if !strings.Contains(terminated, "i-cafef00d") {
		t.Errorf("the instance was not terminated despite a cancelled parent ctx: %q", terminated)
	}
}

// A 200 WITH NO ID IN THE BODY still means a launch happened; when the Name-tag
// recovery FINDS the instance, containment must terminate it — "no id in the body"
// is not "nothing to terminate".
func TestDryRunLaunchSuccessEmptyIDsRecoversAndTerminates(t *testing.T) {
	fastRecovery(t)

	wantName := authorizeName("ami-1", provider.TrustTrusted, 0)
	var terminated string
	var describes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		switch {
		case strings.Contains(body, "Action=DescribeImages"):
			fmt.Fprint(w, `<DescribeImagesResponse><imagesSet><item>`+
				`<imageId>ami-1</imageId><rootDeviceName>/dev/xvda</rootDeviceName>`+
				`<rootDeviceType>ebs</rootDeviceType><imageState>available</imageState></item>`+
				`</imagesSet></DescribeImagesResponse>`)
		case strings.Contains(body, "Action=RunInstances"):
			fmt.Fprint(w, `<RunInstancesResponse><instancesSet/></RunInstancesResponse>`)
		case strings.Contains(body, "Action=DescribeInstances"):
			describes++
			// The recovery must search for the EXACT probe Name.
			if got := requestedNameFilter(t, body); got != wantName {
				t.Errorf("recovery searched Name %q, want %q", got, wantName)
			}
			// EVENTUAL CONSISTENCY: the first lookup is empty, the retry finds it.
			if describes == 1 {
				fmt.Fprint(w, `<DescribeInstancesResponse><reservationSet/></DescribeInstancesResponse>`)

				return
			}
			fmt.Fprint(w, `<DescribeInstancesResponse><reservationSet><item><instancesSet><item>`+
				`<instanceId>i-recovered</instanceId>`+
				`<instanceState><name>running</name></instanceState>`+
				`<tagSet><item><key>Name</key><value>`+wantName+`</value></item></tagSet>`+
				`</item></instancesSet></item></reservationSet></DescribeInstancesResponse>`)
		case strings.Contains(body, "Action=TerminateInstances"):
			terminated = body
			fmt.Fprint(w, `<TerminateInstancesResponse/>`)
		default:
			t.Errorf("unexpected action: %s", body)
		}
	}))
	t.Cleanup(srv.Close)

	p, err := New("deployment", config.EC2Config{
		Region: "us-west-2", Endpoint: srv.URL, SubnetID: "subnet-1",
		SecurityGroupIDs: []string{"sg-trusted"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.large", VCPU: 2, Memory: 4 * config.GiB}},
	}, WithHTTPClient(srv.Client()), WithCredentials(awscreds.Static{AccessKeyID: "AKID", SecretAccessKey: "s"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	shape := config.EC2InstanceType{Type: "c7i.large", VCPU: 2, Memory: 4 * config.GiB}
	if _, err := p.DryRunLaunch(t.Context(), "ami-1", provider.TrustTrusted, shape, 0); err == nil {
		t.Fatal("a mis-honored DryRun must be an error")
	}
	if !strings.Contains(terminated, "i-recovered") {
		t.Errorf("the recovered instance was not terminated: %q", terminated)
	}
	if describes < 2 {
		t.Errorf("recovery did not retry past the first empty describe: %d describes", describes)
	}
}

// fastRecovery drops the mis-honored-launch recovery backoff to zero for the test
// and restores it after, so the retry path runs without real sleeps.
func fastRecovery(t *testing.T) {
	t.Helper()

	saved := launchRecoveryGap
	launchRecoveryGap = 0
	t.Cleanup(func() { launchRecoveryGap = saved })
}

// countingRecovery makes the recovery run without real sleeps AND counts the
// waits, so a test can prove the loop skips the wait after its final attempt.
func countingRecovery(t *testing.T) *int {
	t.Helper()

	savedGap, savedWait := launchRecoveryGap, recoveryWait
	launchRecoveryGap = 0
	count := 0
	recoveryWait = func(_ context.Context, _ time.Duration) bool {
		count++

		return true
	}
	t.Cleanup(func() { launchRecoveryGap, recoveryWait = savedGap, savedWait })

	return &count
}

// A TRANSPORT FAILURE (no AWS code) is NOT a permission verdict: the request may
// have committed before its response was lost, so DryRunLaunch must return an error
// that names the tags to search for — never a verdict, and never a credential.
func TestDryRunLaunchTransportErrorIsNotAVerdict(t *testing.T) {
	// A round-tripper that answers DescribeImages (so the params build) then fails
	// the RunInstances transport, as a broken proxy would.
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if strings.Contains(string(body), "Action=DescribeImages") {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(strings.NewReader(`<DescribeImagesResponse><imagesSet><item>` +
					`<imageId>ami-1</imageId><rootDeviceName>/dev/xvda</rootDeviceName>` +
					`<rootDeviceType>ebs</rootDeviceType><imageState>available</imageState></item>` +
					`</imagesSet></DescribeImagesResponse>`)),
				Header: make(http.Header),
			}, nil
		}

		return nil, errors.New("connection reset by peer")
	})

	p, err := New("deployment", config.EC2Config{
		Region: "us-west-2", Endpoint: "https://ec2.example.invalid", SubnetID: "subnet-1",
		SecurityGroupIDs: []string{"sg-trusted"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.large", VCPU: 2, Memory: 4 * config.GiB}},
	}, WithHTTPClient(&http.Client{Transport: rt}),
		WithCredentials(awscreds.Static{AccessKeyID: "AKID", SecretAccessKey: "supersecret"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	shape := config.EC2InstanceType{Type: "c7i.large", VCPU: 2, Memory: 4 * config.GiB}
	res, err := p.DryRunLaunch(t.Context(), "ami-1", provider.TrustTrusted, shape, 0)
	if err == nil {
		t.Fatalf("a transport failure must be an error, not the verdict %d", res.Outcome)
	}
	// It must point the operator at the tags to search for...
	if !strings.Contains(err.Error(), "sh.billet.owner") || !strings.Contains(err.Error(), nameTagFor(t)) {
		t.Errorf("the error does not name the tags to search for: %v", err)
	}
	// ...and never leak the registration credential material.
	if strings.Contains(err.Error(), "supersecret") {
		t.Errorf("the error leaked a credential: %v", err)
	}
}

// nameTagFor is the probe Name value a trusted disk-0 launch of ami-1 carries, so
// the transport-error test can assert the error names the right tag.
func nameTagFor(t *testing.T) string {
	t.Helper()

	return authorizeName("ami-1", provider.TrustTrusted, 0)
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// THE PROBE'S CLIENTTOKEN MUST STAY UNDER AWS'S 64-CHAR CEILING. A name (and thus
// token) that overflows makes RunInstances answer InvalidParameterValue, which
// classifies as inconclusive — the launch check quietly proving nothing. This pins
// the boundary so a future widening of the name fails here, not in production.
func TestAuthorizeNameClientTokenStaysUnder64(t *testing.T) {
	name := authorizeName("ami-01234567890abcdef", provider.TrustUntrusted, 300*config.GiB)
	token := clientTokenFor(name, "c7i.metal-48xl")
	// PIN THE EXACT LENGTH (16 + 11 + 16-hex name = 43; token adds "-"+12hex = 13 =>
	// 56), not merely "<= 64". The token is input-independent because image/trust/
	// disk and the shape all feed fixed-width digests, so any change to this 56 eats
	// the margin to AWS's 64 ceiling — which is exactly what a 24-hex digest (token
	// 64, zero margin) did.
	const wantLen = 56
	if len(token) != wantLen {
		t.Errorf("authorize ClientToken is %d chars, want %d (margin to AWS's 64 ceiling): %q",
			len(token), wantLen, token)
	}
	// It must also still parse as an orphan (billet- prefix) if mis-launched.
	if _, ours := provider.LeaseOf(name); !ours {
		t.Errorf("authorize name %q is not orphan-parseable", name)
	}
}

// recoveryWait must honor a cancelled context: containment runs it on a detached,
// bounded context, and if it ignored cancellation the retry loop could outrun that
// bound. Pinned directly because the counting stub replaces the real wait.
func TestRecoveryWaitHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// A positive gap so a passing result can only come from the ctx.Done branch,
	// not the timer firing first.
	if recoveryWait(ctx, time.Hour) {
		t.Error("recoveryWait ignored a cancelled context")
	}
}
