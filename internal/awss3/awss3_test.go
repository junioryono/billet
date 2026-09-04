package awss3

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The documents S3 actually sends, in the shape it sends them.
const (
	noSuchKeyDocument = `<?xml version="1.0" encoding="UTF-8"?>` +
		`<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message>` +
		`<Key>billet-cache/owners/abc/state/def.json</Key>` +
		`<RequestId>QWERTY123</RequestId><HostId>aGVsbG8=</HostId></Error>`

	noSuchBucketDocument = `<?xml version="1.0" encoding="UTF-8"?>` +
		`<Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist</Message>` +
		`<BucketName>billet-cache-example</BucketName>` +
		`<RequestId>QWERTY123</RequestId><HostId>aGVsbG8=</HostId></Error>`

	accessDeniedDocument = `<?xml version="1.0" encoding="UTF-8"?>` +
		`<Error><Code>AccessDenied</Code><Message>Access Denied</Message>` +
		`<RequestId>QWERTY123</RequestId><HostId>aGVsbG8=</HostId></Error>`
)

// ONLY NoSuchKey AT 404 IS ABSENCE, AND EVERY OTHER 404 IS AN ERROR.
//
// This is the whole issue in one table. A bucket that does not exist answers the
// same status as an object that does not exist, so a reader that looks only at
// the status cannot tell a misconfigured deployment from a cold cache — and the
// cache path fails open, so nothing else in billet would ever say a word about
// it.
func TestOnlyNoSuchKeyAtFourOhFourIsAbsence(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		status int
		body   string
		code   string
		absent bool
	}{
		{
			name: "a missing object", status: http.StatusNotFound, body: noSuchKeyDocument,
			code: CodeNoSuchKey, absent: true,
		},
		{
			name: "a bucket that does not exist", status: http.StatusNotFound,
			body: noSuchBucketDocument, code: CodeNoSuchBucket,
		},
		{
			name: "a refused read", status: http.StatusForbidden, body: accessDeniedDocument,
			code: "AccessDenied",
		},
		{
			// A 404 WITH NOTHING IN IT is not something S3 sends, which is why it
			// must not be the case that means absence: whatever produced it — a
			// proxy, a load balancer, a captive network — is not S3 answering
			// about an object.
			name: "a 404 with an empty body", status: http.StatusNotFound, body: "",
		},
		{
			// THE ROOT ELEMENT DECIDES. A <Code> element inside some other
			// document is not S3's verdict on this request, and reading one as a
			// verdict is how a listing or a proxy's own XML gets to say that an
			// object is absent.
			name:   "NoSuchKey inside a document that is not an error",
			status: http.StatusNotFound,
			body: `<ListBucketResult><Contents><Code>NoSuchKey</Code></Contents>` +
				`</ListBucketResult>`,
		},
		{
			name: "an html error page", status: http.StatusNotFound,
			body: "<html><head><title>404 Not Found</title></head></html>",
		},
		{
			name: "a body that is not xml at all", status: http.StatusNotFound,
			body: "not found\n",
		},
		{
			name: "a code with a newline in it", status: http.StatusNotFound,
			body: "<Error><Code>NoSuchKey\nand more</Code></Error>",
		},
		{
			name: "a code longer than any AWS sends", status: http.StatusNotFound,
			body: "<Error><Code>" + strings.Repeat("A", 65) + "</Code></Error>",
		},
		{
			name: "an empty code element", status: http.StatusNotFound,
			body: "<Error><Code></Code><Message>something</Message></Error>",
		},
		{
			// THE STATUS IS HALF THE ANSWER. The code says what S3 thinks and the
			// status says how it answered; a code alone would let a body that
			// arrived with some other status decide that an object is missing.
			name: "NoSuchKey at a status that is not 404", status: http.StatusForbidden,
			body: noSuchKeyDocument, code: CodeNoSuchKey,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			refusal := ParseRefusal(tc.status, []byte(tc.body))

			if refusal.Status != tc.status {
				t.Errorf("status = %d, want %d", refusal.Status, tc.status)
			}

			if refusal.Code != tc.code {
				t.Errorf("code = %q, want %q", refusal.Code, tc.code)
			}

			if refusal.Absent() != tc.absent {
				t.Errorf("Absent() = %v, want %v — %s", refusal.Absent(), tc.absent, refusal)
			}
		})
	}
}

// A BODY BILLET DID NOT READ WHOLE SAYS NOTHING, and it says it as an error.
func TestAnOversizedBodyNamesNoCode(t *testing.T) {
	t.Parallel()

	padded := "<Error><Code>NoSuchKey</Code><Message>" +
		strings.Repeat("x", bodyLimit) + "</Message></Error>"

	refusal := ParseRefusal(http.StatusNotFound, []byte(padded))

	if refusal.Code != "" || refusal.Absent() {
		t.Fatalf("a body past the read bound answered %s and Absent()=%v; billet did not see "+
			"the whole document and must not conclude the object is gone",
			refusal, refusal.Absent())
	}
}

// errorBody fails on the first read, the way a connection dropped mid-answer does.
type errorBody struct{}

func (errorBody) Read([]byte) (int, error) {
	return 0, errors.New("connection reset")
}

func (errorBody) Close() error { return nil }

// A RESPONSE BILLET COULD NOT READ IS COULD-NOT-TELL, never absence.
func TestReadRefusalTreatsAnUnreadableBodyAsNoCode(t *testing.T) {
	t.Parallel()

	refusal := ReadRefusal(&http.Response{
		StatusCode: http.StatusNotFound,
		Body:       errorBody{},
	})

	if refusal.Status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", refusal.Status)
	}

	if refusal.Code != "" || refusal.Absent() {
		t.Fatalf("a body that could not be read answered %s and Absent()=%v", refusal,
			refusal.Absent())
	}
}

// ReadRefusal takes the code out of the body it is given.
func TestReadRefusalReadsTheDocument(t *testing.T) {
	t.Parallel()

	refusal := ReadRefusal(&http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader(noSuchBucketDocument)),
	})

	if refusal.Code != CodeNoSuchBucket {
		t.Fatalf("code = %q, want %q", refusal.Code, CodeNoSuchBucket)
	}

	if refusal.Absent() {
		t.Error("a bucket that does not exist read as an absent object")
	}
}

// A RESPONSE WITH NO BODY AT ALL is still a status, and still not absence.
func TestReadRefusalToleratesAMissingBody(t *testing.T) {
	t.Parallel()

	refusal := ReadRefusal(&http.Response{StatusCode: http.StatusNotFound})

	if refusal.Status != http.StatusNotFound || refusal.Code != "" || refusal.Absent() {
		t.Fatalf("a bodyless response answered %s and Absent()=%v", refusal, refusal.Absent())
	}

	if got := ReadRefusal(nil); got.Status != 0 || got.Absent() {
		t.Fatalf("a nil response answered %s", got)
	}
}

// THE RENDERING IS WHAT AN OPERATOR READS, so it names the code when there is
// one and never invents one when there is not.
func TestARefusalRendersTheCodeAndTheStatus(t *testing.T) {
	t.Parallel()

	named := &Refusal{Status: http.StatusNotFound, Code: CodeNoSuchBucket}
	if got := named.Error(); got != "NoSuchBucket (HTTP 404)" {
		t.Errorf("Error() = %q", got)
	}

	if got := (&Refusal{Status: 301}).Error(); got != "HTTP 301" {
		t.Errorf("Error() = %q", got)
	}
}

// THE STATUS TRAVELS THROUGH THE WRAPPING, which is what lets `billet check`
// classify a 403 without matching on the words of a message.
func TestStatusOfFindsARefusalThroughItsWrapping(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("ebs-s3: the cache bucket did not answer a probe read: %w",
		fmt.Errorf("ebs-s3: S3 GET returned %w", &Refusal{Status: http.StatusForbidden,
			Code: "AccessDenied"}))

	if got := StatusOf(wrapped); got != http.StatusForbidden {
		t.Errorf("StatusOf = %d, want 403", got)
	}

	// ZERO IS "NOT AN S3 REFUSAL". A transport failure and a credential that
	// would not resolve reach the same branch, and neither is a 403.
	if got := StatusOf(errors.New("dial tcp: connection refused")); got != 0 {
		t.Errorf("StatusOf(a transport failure) = %d, want 0", got)
	}

	if got := StatusOf(nil); got != 0 {
		t.Errorf("StatusOf(nil) = %d, want 0", got)
	}
}
