package ec2

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The request every vector below signs. Held as constants so the test and the
// command that regenerates the vectors cannot drift apart in the details that
// change a signature — and every one of them does.
const (
	vectorBody   = "Action=RunInstances&ImageId=ami-0abc&MaxCount=1&MinCount=1&Version=2016-11-15"
	vectorURL    = "https://ec2.us-west-2.amazonaws.com/"
	vectorRegion = "us-west-2"
	vectorKey    = "AKIDEXAMPLE"
	vectorSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	vectorToken  = "FQoGZXIvYXdzEExampleToken=="
)

func vectorTime() time.Time { return time.Date(2026, 8, 12, 12, 36, 0, 0, time.UTC) }

// THE VECTORS COME FROM AWS'S OWN SIGNER, NOT FROM READING THE SPECIFICATION.
//
// billet signs its own requests rather than taking the AWS SDK, because a program
// that does nothing but construct an EC2 client and call RunInstances once builds
// to 13.2MB against a whole billet of 21.8MB, and every node in a fleet would
// carry it — including the bare-metal hosts this backend exists to fall back
// FROM. That trade is only defensible if the output is known to be identical, and
// "I implemented the document correctly" is not knowing.
//
// So these two strings were produced by aws-sdk-go-v2 signing the exact request
// above. To regenerate them, in a scratch module with
// github.com/aws/aws-sdk-go-v2 required:
//
//	req, _ := http.NewRequest(http.MethodPost, vectorURL, strings.NewReader(vectorBody))
//	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
//	sum := sha256.Sum256([]byte(vectorBody))
//	v4.NewSigner().SignHTTP(ctx,
//		aws.Credentials{AccessKeyID: vectorKey, SecretAccessKey: vectorSecret, SessionToken: tok},
//		req, hex.EncodeToString(sum[:]), "ec2", vectorRegion, vectorTime())
//	fmt.Println(req.Header.Get("Authorization"))
//
// A failure here means billet and AWS disagree about what this request says. The
// service would answer 403 with nothing about which byte moved, which is the
// failure this test exists to turn into a diff.
func TestTheSignatureMatchesAWSsOwnSigner(t *testing.T) {
	for name, tc := range map[string]struct {
		token string
		want  string
	}{
		"no session token": {
			want: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260812/us-west-2/ec2/aws4_request, " +
				"SignedHeaders=content-length;content-type;host;x-amz-date, " +
				"Signature=9b0063ced912747efcb5d254101e12b955e6695f9d08a727db7bf16434e34dfa",
		},
		// A session token is signed as well as sent. Sending one that is not in
		// SignedHeaders is a 403 that reads like a credential problem.
		"with session token": {
			token: vectorToken,
			want: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260812/us-west-2/ec2/aws4_request, " +
				"SignedHeaders=content-length;content-type;host;x-amz-date;x-amz-security-token, " +
				"Signature=0897896168ad393928d15eb9dd1c3bfb9aad43d5a323e91482e02792e860c39a",
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := vectorRequest(t)

			creds := Credentials{
				AccessKeyID:     vectorKey,
				SecretAccessKey: vectorSecret,
				SessionToken:    tc.token,
			}

			if err := sign(req, []byte(vectorBody), creds, vectorRegion, vectorTime()); err != nil {
				t.Fatalf("sign: %v", err)
			}

			if got := req.Header.Get("Authorization"); got != tc.want {
				t.Errorf("billet and the aws sdk sign this request differently\n got: %s\nwant: %s",
					got, tc.want)
			}

			if got := req.Header.Get("X-Amz-Date"); got != "20260812T123600Z" {
				t.Errorf("X-Amz-Date = %q, want the signed timestamp", got)
			}

			if tc.token != "" && req.Header.Get("X-Amz-Security-Token") != tc.token {
				t.Error("the session token was signed but not sent")
			}
		})
	}
}

func vectorRequest(t *testing.T) *http.Request {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, vectorURL, strings.NewReader(vectorBody)) //nolint:noctx // signing does not issue it
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.ContentLength = int64(len(vectorBody))

	return req
}

// A RE-SIGN MUST NOT SIGN THE PREVIOUS SIGNATURE.
//
// NOT A PATH THE CLIENT TAKES TODAY, and saying otherwise would be worse than
// saying nothing: `call` builds a fresh request for every attempt, precisely
// because the signature covers a timestamp and credentials that can change
// between them. So a retry never presents a request that is already signed.
//
// This is kept because the property belongs to the SIGNER rather than to its
// current caller — signing is a general operation and the obvious future caller
// is one that re-signs in place — and because the failure it prevents is opaque:
// a request carrying its own previous Authorization header is refused with a bare
// 403, so a retry after a throttle would look like a credential problem and the
// real cause would disappear.
func TestSigningTwiceProducesTheSameSignature(t *testing.T) {
	creds := Credentials{AccessKeyID: vectorKey, SecretAccessKey: vectorSecret}

	req := vectorRequest(t)

	if err := sign(req, []byte(vectorBody), creds, vectorRegion, vectorTime()); err != nil {
		t.Fatalf("first sign: %v", err)
	}

	first := req.Header.Get("Authorization")

	if err := sign(req, []byte(vectorBody), creds, vectorRegion, vectorTime()); err != nil {
		t.Fatalf("second sign: %v", err)
	}

	if got := req.Header.Get("Authorization"); got != first {
		t.Errorf("re-signing changed the signature, so the first one was signed into the "+
			"second\nfirst:  %s\nsecond: %s", first, got)
	}
}

// A HEADER BILLET DOES NOT CONTROL MUST NOT BE SIGNED, because the value on the
// wire is not the value that was hashed. Go's transport sets User-Agent after
// this runs, so signing it produces a request that is invalid by the time it is
// sent — and the service says only 403.
func TestHeadersSomethingElseWritesAreNotSigned(t *testing.T) {
	creds := Credentials{AccessKeyID: vectorKey, SecretAccessKey: vectorSecret}

	req := vectorRequest(t)
	req.Header.Set("User-Agent", "something-go-adds/1.0")
	req.Header.Set("X-Amzn-Trace-Id", "Root=1-abc")

	if err := sign(req, []byte(vectorBody), creds, vectorRegion, vectorTime()); err != nil {
		t.Fatalf("sign: %v", err)
	}

	got := req.Header.Get("Authorization")

	for _, name := range []string{"user-agent", "x-amzn-trace-id"} {
		if strings.Contains(got, name) {
			t.Errorf("%s was signed, and billet does not control its value on the wire: %s",
				name, got)
		}
	}

	// And the signature is still the one AWS produces for the request without
	// them, which is what proves they were excluded rather than merely reordered.
	if !strings.HasSuffix(got,
		"Signature=9b0063ced912747efcb5d254101e12b955e6695f9d08a727db7bf16434e34dfa") {
		t.Errorf("adding unsigned headers changed the signature: %s", got)
	}
}

// A REQUEST WITH NO CREDENTIALS IS NOT SIGNED WITH AN EMPTY KEY. It is refused,
// so the failure names the missing credential rather than arriving as a 403.
func TestSigningWithoutCredentialsIsRefused(t *testing.T) {
	req := vectorRequest(t)

	err := sign(req, []byte(vectorBody), Credentials{}, vectorRegion, vectorTime())
	if err == nil {
		t.Fatal("a request with no credentials was signed")
	}

	if req.Header.Get("Authorization") != "" {
		t.Error("a refused signing still set an Authorization header")
	}
}

// The whitespace rule is the specification's and it changes the signature, so a
// value with padding must sign as the value without it.
func TestAHeaderValueIsTrimmedAndCollapsedBeforeSigning(t *testing.T) {
	creds := Credentials{AccessKeyID: vectorKey, SecretAccessKey: vectorSecret}

	padded := vectorRequest(t)
	padded.Header.Set("Content-Type", "  application/x-www-form-urlencoded;    charset=utf-8  ")

	if err := sign(padded, []byte(vectorBody), creds, vectorRegion, vectorTime()); err != nil {
		t.Fatalf("sign: %v", err)
	}

	if !strings.HasSuffix(padded.Header.Get("Authorization"),
		"Signature=9b0063ced912747efcb5d254101e12b955e6695f9d08a727db7bf16434e34dfa") {
		t.Errorf("a padded header value signed differently from the trimmed one: %s",
			padded.Header.Get("Authorization"))
	}
}

// SIGV4 CANONICALIZES A SPACE AS %20 AND Go's url.Values.Encode WRITES `+`.
//
// Latent for this client, which puts every parameter in the POST body where the
// payload hash covers the exact bytes — and worth pinning anyway, because the
// signer is a general thing and the previous comment claimed Go's normalization
// and the specification "happen to agree". They agree on sorting and on most
// escapes. On space they do not, and the failure is a 403 that names nothing.
func TestAQueryStringSpaceIsCanonicalizedTheWayAWSReadsIt(t *testing.T) {
	u, err := url.Parse("https://ec2.us-west-2.amazonaws.com/?Name=two+words&Other=a%20b")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	got, err := canonicalQuery(u)
	if err != nil {
		t.Fatalf("canonicalQuery: %v", err)
	}

	if strings.Contains(got, "+") {
		t.Errorf("canonical query %q encodes a space as +, which AWS canonicalizes as %%20 — "+
			"the signature would cover bytes the service never reconstructs", got)
	}

	if want := "Name=two%20words&Other=a%20b"; got != want {
		t.Errorf("canonical query = %q, want %q", got, want)
	}
}

// HOST AND CONTENT-LENGTH COME FROM THE REQUEST, NEVER FROM THE HEADER MAP.
//
// Go keeps both on the Request itself and writes them onto the wire from there,
// ignoring anything a caller put in Header. So signing the map's version produces
// a signature over a request nobody sent — a 403 naming nothing.
//
// The pinned AWS vectors cannot catch this: their Host and Content-Length exist
// only on the Request, so the branch that skips the map is never reached by them.
// This sets both in the map to something else and asserts the signature is
// unchanged from the vector.
func TestACallerSetHostOrContentLengthIsNotSignedInsteadOfWhatIsSent(t *testing.T) {
	creds := Credentials{AccessKeyID: vectorKey, SecretAccessKey: vectorSecret}

	req := vectorRequest(t)
	req.Header.Set("Host", "attacker.example")
	req.Header.Set("Content-Length", "99999")

	if err := sign(req, []byte(vectorBody), creds, vectorRegion, vectorTime()); err != nil {
		t.Fatalf("sign: %v", err)
	}

	const want = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260812/us-west-2/ec2/aws4_request, " +
		"SignedHeaders=content-length;content-type;host;x-amz-date, " +
		"Signature=9b0063ced912747efcb5d254101e12b955e6695f9d08a727db7bf16434e34dfa"

	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("a header the transport ignores was signed in place of the value it sends\n "+
			"got: %s\nwant: %s", got, want)
	}
}

// A QUERY BILLET CANNOT READ THE SAME WAY IT WILL BE SENT IS REFUSED, NOT SIGNED
// PARTIALLY.
//
// url.URL.Query discards ParseQuery's error, so a query Go dislikes is silently
// reduced rather than rejected — measured, `a=1;b=2` parses to an EMPTY map,
// because Go has refused the semicolon as a separator since 1.17, and a bad
// escape quietly drops its pair. Signing what is left covers a query the wire
// does not send, and the service answers 403 naming nothing.
//
// Latent for this client, which sends every parameter in the body. Fixed because
// the signer is general, which is the same reason the space encoding was.
func TestAQueryThatCannotBeReadFaithfullyIsRefused(t *testing.T) {
	creds := Credentials{AccessKeyID: vectorKey, SecretAccessKey: vectorSecret}

	for name, raw := range map[string]string{
		"a semicolon separator": "a=1;b=2",
		"a bad escape":          "a=1&b=%zz",
	} {
		t.Run(name, func(t *testing.T) {
			req := vectorRequest(t)
			req.URL.RawQuery = raw

			err := sign(req, []byte(vectorBody), creds, vectorRegion, vectorTime())
			if err == nil {
				t.Fatal("a query billet cannot reproduce was signed anyway; the signature " +
					"would cover something the wire does not send")
			}

			if req.Header.Get("Authorization") != "" {
				t.Error("a refused signing still set an Authorization header")
			}
		})
	}

	// And an ordinary query still signs, or this would be a refusal of everything.
	req := vectorRequest(t)
	req.URL.RawQuery = "b=2&a=1"

	if err := sign(req, []byte(vectorBody), creds, vectorRegion, vectorTime()); err != nil {
		t.Errorf("an ordinary query was refused: %v", err)
	}
}
