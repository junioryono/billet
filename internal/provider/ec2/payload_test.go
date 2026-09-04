package ec2

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/config"
)

// STAGING RUNS THROUGH A PROVIDER, NOT THROUGH A STRUCT LITERAL.
//
// payloadStager's own test builds it field by field, which means every field is
// populated by definition -- and the one thing that mattered was a field the
// PROVIDER does not populate. `now` is nil unless a test pinned it, and reading it
// straight into a struct that calls it panicked on the first real build, inside
// the signer, with a stack that said nothing about a missing default.
//
// So this exercises the path production takes: a Provider, its options, and
// stagePayload.
func TestStagingWorksThroughAProviderRatherThanALiteral(t *testing.T) {
	t.Parallel()

	// GUARDED FOR THE SAME REASON AS THE ONE BELOW: written on the server's
	// goroutine, read on the test's.
	var (
		mu              sync.Mutex
		gotPut, gotAuth string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			mu.Lock()
			gotPut = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			mu.Unlock()
		}

		w.WriteHeader(http.StatusOK)
	}))

	t.Cleanup(srv.Close)

	p, err := New("deployment", config.EC2Config{
		Region:           "us-west-2",
		Endpoint:         srv.URL,
		SubnetID:         "subnet-1",
		SecurityGroupIDs: []string{"sg-1"},
		InstanceTypes: []config.EC2InstanceType{
			{Type: "c7i.large", VCPU: 2, Memory: 4 * config.GiB},
		},
	}, WithCredentials(awscreds.Static{
		AccessKeyID: "AKID", SecretAccessKey: "s",
	}), WithHTTPClient(&http.Client{
		// THE S3 ENDPOINT IS REDIRECTED, NOT THE CODE. What is under test is that a
		// Provider can stage at all, so the request reaches a local server while
		// everything about how it is built stays production's -- including the
		// clock, which is the field that was nil.
		Transport: redirectTo(srv.URL, srv.Client().Transport),
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	spec := BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, PayloadBucket: "billet-payload-test", Arch: "x64"}

	key, cleanup, err := p.stagePayload(t.Context(), &spec)
	if err != nil {
		t.Fatalf("stagePayload through a Provider: %v", err)
	}

	// REPORTED, NOT DISCARDED. The cleanup is what removes a staged object, and a
	// test that ignores its failure is the same silence that leaves one billed.
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("the staged object could not be removed: %v", err)
		}
	})

	mu.Lock()
	sawPut, sawAuth := gotPut, gotAuth
	mu.Unlock()

	if sawPut == "" {
		t.Fatal("nothing was uploaded")
	}

	if sawAuth == "" || !strings.Contains(sawAuth, "AWS4-HMAC-SHA256") {
		t.Errorf("the upload was not signed: %q", sawAuth)
	}

	// THE SPEC CARRIES WHAT THE BOOTSTRAP NEEDS. A staging call that uploaded and
	// left these empty would emit a script that fetches nothing and verifies it.
	if spec.payloadURL == "" || spec.payloadSHA256 == "" {
		t.Errorf("staging left the spec without a url (%q) or digest (%q)",
			spec.payloadURL, spec.payloadSHA256)
	}

	if !strings.Contains(spec.payloadURL, "X-Amz-Signature=") {
		t.Errorf("the url is not presigned: %q", spec.payloadURL)
	}

	if !strings.Contains(key, spec.payloadSHA256) {
		t.Errorf("the key %q does not carry the digest %q, so two builds with different "+
			"payloads could share an object", key, spec.payloadSHA256)
	}
}

// TWO BUILDS STAGING THE SAME BYTES MUST NOT SHARE AN OBJECT.
//
// A key built from the digest alone is content-addressed, which reads as a virtue
// and is a bug: every build deletes its key when it finishes, so a build that ends
// while a second builder is still fetching deletes the archive out from under it.
// The second build then fails on something it did nothing to cause, intermittently,
// only under concurrency — the shape that does not reproduce.
func TestTwoBuildsDoNotShareOnePayloadObject(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	t.Cleanup(srv.Close)

	p := stagingProvider(t, srv)

	// THE SAME PAYLOAD, TWICE. payloadArchive is deterministic, so both stagings
	// produce one digest and the digest cannot be what separates them.
	first := BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, PayloadBucket: "billet-payload-test", Arch: "x64"}
	second := BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, PayloadBucket: "billet-payload-test", Arch: "x64"}

	keyA, cleanupA, err := p.stagePayload(t.Context(), &first)
	if err != nil {
		t.Fatalf("stage the first payload: %v", err)
	}

	t.Cleanup(func() {
		if err := cleanupA(); err != nil {
			t.Errorf("the first staged object could not be removed: %v", err)
		}
	})

	keyB, cleanupB, err := p.stagePayload(t.Context(), &second)
	if err != nil {
		t.Fatalf("stage the second payload: %v", err)
	}

	t.Cleanup(func() {
		if err := cleanupB(); err != nil {
			t.Errorf("the second staged object could not be removed: %v", err)
		}
	})

	// THE PREMISE, ASSERTED. If the two payloads differed, distinct keys would
	// prove nothing about the nonce and this test would be about the digest.
	if first.payloadSHA256 != second.payloadSHA256 {
		t.Fatalf("the two stagings produced different payloads (%q, %q), so identical "+
			"bytes were never actually tested", first.payloadSHA256, second.payloadSHA256)
	}

	if keyA == keyB {
		t.Errorf("both builds staged to %q, so whichever finishes first deletes the "+
			"object the other is still fetching", keyA)
	}
}

// AND A PUT WHOSE RESPONSE SAYS FAILURE MAY STILL HAVE COMMITTED.
//
// S3 can write the object and then the response can be lost, or arrive as a 500.
// The caller is handed a cleanup only on success, so an error return that never
// names the key leaves an object nobody can attribute and nobody will delete —
// billed, indefinitely, for a build that reported failure.
func TestAnAmbiguousUploadIsStillCleanedUp(t *testing.T) {
	t.Parallel()

	// GUARDED, BECAUSE TWO GOROUTINES REACH IT. The handler runs on the server's
	// goroutine and the assertion on the test's; whether net/http happens to
	// synchronise them through the response is not a guarantee to build on, and the
	// sibling test in this file shipped exactly this shape and raced.
	var (
		mu      sync.Mutex
		deleted []string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			mu.Lock()
			deleted = append(deleted, r.URL.Path)
			mu.Unlock()

			w.WriteHeader(http.StatusNoContent)
		case http.MethodPut:
			// COMMITTED, THEN REPORTED AS FAILED. This is the case the code cannot
			// distinguish from a PUT that never landed, which is why it must try.
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))

	t.Cleanup(srv.Close)

	p := stagingProvider(t, srv)
	spec := BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, PayloadBucket: "billet-payload-test", Arch: "x64"}

	if _, _, err := p.stagePayload(t.Context(), &spec); err == nil {
		t.Fatal("a 500 from the upload was reported as a successful staging")
	}

	mu.Lock()
	removals := len(deleted)
	mu.Unlock()

	if removals == 0 {
		t.Error("the upload failed ambiguously and nothing tried to remove the object; " +
			"an object S3 may have written is then billed forever with no way to " +
			"attribute it")
	}
}

// stagingProvider builds the Provider these tests stage through, with every
// request redirected to srv.
func stagingProvider(t *testing.T, srv *httptest.Server) *Provider {
	t.Helper()

	p, err := New("deployment", config.EC2Config{
		Region:           "us-west-2",
		Endpoint:         srv.URL,
		SubnetID:         "subnet-1",
		SecurityGroupIDs: []string{"sg-1"},
		InstanceTypes: []config.EC2InstanceType{
			{Type: "c7i.large", VCPU: 2, Memory: 4 * config.GiB},
		},
	}, WithCredentials(awscreds.Static{
		AccessKeyID: "AKID", SecretAccessKey: "s",
	}), WithHTTPClient(&http.Client{
		Transport: redirectTo(srv.URL, srv.Client().Transport),
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return p
}

// A BUCKET BILLET CANNOT ADDRESS IS REFUSED BEFORE IT IS SIGNED.
//
// The endpoint is virtual-hosted, so the bucket name becomes the leftmost labels of
// the host. S3 permits dots in a name; a dotted name produces a multi-label host
// that S3's wildcard certificate does not cover, because a wildcard matches exactly
// one label — so the build fails TLS verification against a bucket that exists and
// is spelled the way the operator typed it. That is a bad half-hour, and it is
// avoidable by saying so at the point the name arrives.
func TestABucketBilletCannotAddressIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		bucket string
		ok     bool
	}{
		{name: "ordinary", bucket: "billet-payload-123", ok: true},
		{name: "dotted", bucket: "billet.payloads"},
		{name: "consecutive dots", bucket: "billet..payloads"},
		{name: "uppercase", bucket: "Billet-Payloads"},
		{name: "leading hyphen", bucket: "-billet"},
		{name: "too short", bucket: "ab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// CONSTRUCTED HERE, NOT HOISTED. One stager declared outside the loop
			// and assigned inside it is shared mutable state across parallel
			// subtests -- a real data race, and the detector found it on the second
			// whole-suite run rather than the first. A clean -race run is evidence,
			// not proof: it only sees the interleavings that happened.
			stager := payloadStager{region: "us-west-2", bucket: tc.bucket}

			u, err := stager.endpoint("billet-payload-x.tar.gz")

			switch {
			case tc.ok && err != nil:
				t.Fatalf("endpoint(%q): %v", tc.bucket, err)
			case tc.ok:
				// THE HOST HAS EXACTLY ONE LABEL BEFORE `s3`, which is the property
				// the wildcard certificate requires and the reason for the rule.
				if got := u.Host; got != tc.bucket+".s3.us-west-2.amazonaws.com" {
					t.Errorf("endpoint host is %q", got)
				}
			case err == nil:
				t.Errorf("%q was accepted and signed as %q; a request billet cannot "+
					"complete is better refused with the reason", tc.bucket, u)
			}
		})
	}
}

// redirectTo sends every request to one host, keeping the rest of the URL.
type redirectRoundTripper struct {
	host string
	next http.RoundTripper
}

func redirectTo(base string, next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}

	return redirectRoundTripper{host: strings.TrimPrefix(base, "http://"), next: next}
}

func (r redirectRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = r.host

	return r.next.RoundTrip(clone)
}

// THE PAYLOAD IS SIGNED FOR THE PARTITION'S OWN HOST.
//
// A build's first act is staging its own payload, and this endpoint used to be
// built by hand with the commercial suffix. In cn-north-1 that names a host
// which does not exist, so the build dies before it launches anything — while
// `billet init iam` and the terraform module both render a correct aws-cn grant
// for the same region, which makes it read as a permissions problem.
//
// The URL is what the signature covers, so a wrong host is not a redirect: it
// is a request signed for one name and sent to another.
//
// Mutation: restoring ".amazonaws.com" fails the two China cases.
func TestThePayloadEndpointFollowsThePartition(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		region, want string
	}{
		{"us-west-2", "billet-payloads.s3.us-west-2.amazonaws.com"},
		{"eu-central-1", "billet-payloads.s3.eu-central-1.amazonaws.com"},
		// GovCloud keeps the commercial suffix; only China differs.
		{"us-gov-west-1", "billet-payloads.s3.us-gov-west-1.amazonaws.com"},
		{"cn-north-1", "billet-payloads.s3.cn-north-1.amazonaws.com.cn"},
		{"cn-northwest-1", "billet-payloads.s3.cn-northwest-1.amazonaws.com.cn"},
	} {
		stager := payloadStager{bucket: "billet-payloads", region: c.region}

		u, err := stager.endpoint("billet-payload-abc")
		if err != nil {
			t.Fatalf("%s: %v", c.region, err)
		}

		if u.Host != c.want {
			t.Errorf("%s stages at %q, want %q", c.region, u.Host, c.want)
		}
	}
}
