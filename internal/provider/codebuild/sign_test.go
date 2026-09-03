package codebuild

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/awssig"
)

// The request every vector below signs. Held as constants so the test and the
// snippet that regenerates the vectors cannot drift apart in the details that change
// a signature — and every one of them does.
const (
	vectorBody        = `{"idempotencyToken":"billet-abc","projectName":"billet-linux"}`
	vectorEndpoint    = "https://codebuild.us-west-2.amazonaws.com/"
	vectorSSMEndpoint = "https://ssm.us-west-2.amazonaws.com/"
	vectorTarget      = "CodeBuild_20161006.StartBuild"
	vectorSSMTarget   = "AmazonSSM.PutParameter"
	vectorRegion      = "us-west-2"
	vectorKey         = "AKIDEXAMPLE"
	vectorSecret      = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	vectorToken       = "FQoGZXIvYXdzEExampleToken=="
)

func vectorTime() time.Time { return time.Date(2026, 8, 12, 12, 36, 0, 0, time.UTC) }

// THE CODEBUILD VECTORS ARE THEIR OWN, AND THAT IS THE POINT.
//
// internal/provider/ec2/sign_test.go already pins the shared signer to AWS's own
// output — but it signs a form-encoded body for the EC2 query API, with no
// X-Amz-Target header. That proves nothing about this backend's requests, which are
// AWS JSON 1.1: a different content type, an extra SIGNED header, and a different
// service in the credential scope. Reusing the ec2 vectors here would be a test that
// passes while every CodeBuild request is rejected as a bare 403.
//
// THEY COME FROM A SECOND IMPLEMENTATION, NOT FROM BILLET'S OWN SIGNER. Deriving
// them by running the code under test would assert that it agrees with itself. These
// were produced by an independent Python implementation of AWS's published algorithm,
// which is the same discipline the ec2 vectors follow with aws-sdk-go-v2 — pin the
// output to somebody else's, never to a reading of the specification.
//
// To regenerate with AWS's own signer, in a scratch module requiring
// github.com/aws/aws-sdk-go-v2:
//
//	req, _ := http.NewRequest(http.MethodPost, vectorEndpoint, strings.NewReader(vectorBody))
//	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
//	req.Header.Set("X-Amz-Target", vectorTarget)
//	req.ContentLength = int64(len(vectorBody))
//	sum := sha256.Sum256([]byte(vectorBody))
//	v4.NewSigner().SignHTTP(ctx,
//		aws.Credentials{AccessKeyID: vectorKey, SecretAccessKey: vectorSecret, SessionToken: tok},
//		req, hex.EncodeToString(sum[:]), "codebuild", vectorRegion, vectorTime())
//	fmt.Println(req.Header.Get("Authorization"))
//
// A failure here means billet and AWS disagree about what this request says. The
// service answers 403 with nothing about which byte moved, which is the failure this
// test exists to turn into a diff.
func TestTheJSONSignatureMatchesAnIndependentSigner(t *testing.T) {
	for name, tc := range map[string]struct {
		endpoint string
		service  string
		target   string
		token    string
		want     string
	}{
		"codebuild, no session token": {
			endpoint: vectorEndpoint,
			service:  "codebuild",
			target:   vectorTarget,
			want: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260812/us-west-2/codebuild/" +
				"aws4_request, SignedHeaders=content-length;content-type;host;x-amz-date;" +
				"x-amz-target, " +
				"Signature=bc4ff44cf4c8d18a968f6c0a2a7a28b106e70228b1c0bd6d928b7001ceeecb8c",
		},
		// A SESSION TOKEN IS SIGNED AS WELL AS SENT, and it changes SignedHeaders.
		// Sending one that is not in SignedHeaders is a 403 that reads like a
		// credential problem — and every credential billet resolves from an instance
		// role carries one, so this is the ordinary case on the recommended topology
		// rather than the exotic one.
		"codebuild, session token": {
			endpoint: vectorEndpoint,
			service:  "codebuild",
			target:   vectorTarget,
			token:    vectorToken,
			want: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260812/us-west-2/codebuild/" +
				"aws4_request, SignedHeaders=content-length;content-type;host;x-amz-date;" +
				"x-amz-security-token;x-amz-target, " +
				"Signature=886a2c38dda7fcf8c1986d01fdecdd03b81b034103c4f661998c6f7cd172c800",
		},
		// PARAMETER STORE IS A DIFFERENT SERVICE IN THE SCOPE, which is the one thing
		// about the JIT channel a signature can get wrong silently: signing the
		// runner registration's write for `codebuild` produces a 403 on the one call
		// whose failure leaves a launch with no credential staged.
		"ssm, no session token": {
			endpoint: vectorSSMEndpoint,
			service:  "ssm",
			target:   vectorSSMTarget,
			want: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260812/us-west-2/ssm/" +
				"aws4_request, SignedHeaders=content-length;content-type;host;x-amz-date;" +
				"x-amz-target, " +
				"Signature=940ba105f701f7673b697092e5d8e42ab58456e15cca9b74077fefe7fc05ab71",
		},
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, tc.endpoint,
				strings.NewReader(vectorBody))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}

			req.Header.Set("Content-Type", contentType)
			req.Header.Set("X-Amz-Target", tc.target)
			req.ContentLength = int64(len(vectorBody))

			creds := awssig.Credentials{
				AccessKeyID:     vectorKey,
				SecretAccessKey: vectorSecret,
				SessionToken:    tc.token,
			}

			if err := awssig.Sign(req, []byte(vectorBody), creds,
				vectorRegion, tc.service, vectorTime()); err != nil {
				t.Fatalf("Sign: %v", err)
			}

			if got := req.Header.Get("Authorization"); got != tc.want {
				t.Errorf("Authorization mismatch.\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// THE HEADERS BILLET SETS ARE THE HEADERS IT SIGNS.
//
// The two are separate derivations — one sets a header on the request, the other
// builds a canonical string — and a mismatch is a 403 naming nothing. Content-Length
// is the one that has bitten: AWS's signer includes it, Go's transport derives it
// from the body, and a request that signed one value and sent another is rejected
// with no clue which.
func TestTheSignedHeadersCoverEverythingBilletSets(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	if _, err := p.Launch(t.Context(), launchSpec("billet-abc")); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// The fake refuses an unsigned request outright, so reaching here at all proves
	// something was signed. What this adds is WHICH headers — read out of the
	// Authorization header the fake received.
	if len(f.signedHeaders) == 0 {
		t.Fatal("no signed-header list was recorded")
	}

	for _, header := range f.signedHeaders {
		for _, want := range []string{
			"content-length", "content-type", "host", "x-amz-date", "x-amz-target",
		} {
			if !strings.Contains(header, want) {
				t.Errorf("a request signed %q, which omits %q", header, want)
			}
		}
	}
}
