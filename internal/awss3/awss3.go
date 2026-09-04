// Package awss3 reads what S3 said in the body of a refusal.
//
// IT IS NOT AN S3 CLIENT. billet has two of those — internal/store/ebss3 for the
// site cache and internal/archivestore for the off-box deployment archive — and
// this is the one thing they have to agree about. It lives beside internal/awssig
// for the reason internal/awsquota and internal/awssts do: a store and a
// transport are siblings that may not import each other, and both need this
// answer.
//
// WHAT IT EXISTS FOR is that a 404 from S3 is TWO DIFFERENT FACTS. `NoSuchKey`
// says the object is not there, which is an ordinary answer that a cold cache and
// a bucket nobody has written to yet both produce. `NoSuchBucket` says billet is
// addressing a bucket that does not exist, which is a misconfiguration no amount
// of retrying fixes. Both readers used to treat every 404 as the first, so a cache
// pointed at a bucket S3 had never heard of was indistinguishable from a cold one
// and nothing reported a fault.
//
// SO ONLY `NoSuchKey` MEANS ABSENCE, and a 404 carrying anything else — another
// code, no code, a body billet could not read — stays an error. That is the
// direction the house rule points: could-not-tell never collapses into no.
//
// MEASURED AGAINST REAL S3 IN us-east-1 ON 2026-09-04, because a rule about
// somebody else's API written from its documentation agrees with whatever billet
// already believes. Two GETs, both answering HTTP 404:
//
//	a key that is not there, in a bucket that is:
//	  <Error><Code>NoSuchKey</Code><Message>The specified key does not
//	  exist.</Message><Key>…</Key><RequestId>…</RequestId>…</Error>
//
//	any key, in a bucket that does not exist:
//	  <Error><Code>NoSuchBucket</Code><Message>The specified bucket does not
//	  exist</Message><BucketName>…</BucketName><RequestId>…</RequestId>…</Error>
//
// The first went to a public open-data bucket, whose policy grants anonymous
// s3:ListBucket — the same grant internal/store/ebss3's CheckAccess names as the
// condition for getting a 404 rather than a 403 on a miss. The second needed no
// identity at all: a bucket is resolved before a request is authorized, so the
// name being wrong is answered before anything looks at who is asking. Both were
// unauthenticated; reals3_test.go is the same pair signed, against a bucket an
// operator points it at.
package awss3

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// The two codes billet acts on by name. Every other code travels as itself.
const (
	// CodeNoSuchKey is the ONLY 404 that means an object is absent.
	CodeNoSuchKey = "NoSuchKey"
	// CodeNoSuchBucket is a bucket that does not exist at the address billet
	// built out of the configured bucket name and region.
	CodeNoSuchBucket = "NoSuchBucket"
)

// bodyLimit bounds what is read out of a refusal.
//
// An S3 error document is a few hundred bytes. This is generous rather than
// tight, and it is here because the body of a refusal is remote input on a path
// that has already gone wrong: without a bound, whatever is on the far side
// decides how much memory billet spends failing.
const bodyLimit = 64 << 10

// codeShape is what a code has to look like to be repeated to an operator.
//
// The code is rendered into an error message a person reads in a terminal and it
// arrives from the network. Every code AWS documents is a bare CamelCase
// identifier, so anything else — control characters, a whole HTML page inside a
// <Code> element, a kilobyte of anything — is discarded rather than echoed. A
// discarded code reads as "no code", which is an error rather than absence.
var codeShape = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Refusal is one non-2xx answer from S3, as an error.
//
// AN ERROR RATHER THAN A VALUE so a caller can wrap it with %w and anything above
// can ask what S3 actually said without matching on prose. `billet check`'s cache
// probe classifies a 403 as inconclusive, and it used to do that by looking for
// the substring "HTTP 403" in a rendered message — which makes a reworded
// diagnostic silently change the verdict.
type Refusal struct {
	// Status is the HTTP status S3 answered with.
	Status int
	// Code is the <Code> element of S3's error document, empty when S3 named
	// none or billet could not read one. Empty is could-not-tell.
	Code string
}

// Error renders the code and the status, or the status alone.
func (r *Refusal) Error() string {
	if r == nil {
		return "s3: no refusal"
	}

	if r.Code == "" {
		return fmt.Sprintf("HTTP %d", r.Status)
	}

	return fmt.Sprintf("%s (HTTP %d)", r.Code, r.Status)
}

// Absent reports the one 404 that is a fact rather than a failure.
//
// THE STATUS IS CHECKED AS WELL AS THE CODE. A code alone would let a body that
// arrived with some other status decide that an object is missing, and the two
// halves of the answer come from different places in the response.
func (r *Refusal) Absent() bool {
	return r != nil && r.Status == http.StatusNotFound && r.Code == CodeNoSuchKey
}

// ReadRefusal consumes a bounded prefix of a non-2xx response and reports what
// S3 said. The caller still closes the body.
func ReadRefusal(response *http.Response) *Refusal {
	if response == nil {
		return &Refusal{}
	}

	if response.Body == nil {
		return &Refusal{Status: response.StatusCode}
	}

	// A READ FAILURE IS NOT AN ABSENT OBJECT. Whatever was in the body, billet
	// did not see it, and the empty code that comes back from here says exactly
	// that — which every caller treats as an error.
	body, err := io.ReadAll(io.LimitReader(response.Body, bodyLimit+1))
	if err != nil {
		return &Refusal{Status: response.StatusCode}
	}

	return ParseRefusal(response.StatusCode, body)
}

// ParseRefusal is ReadRefusal for a caller that already holds the body — the
// listing paths read theirs before they look at the status.
func ParseRefusal(status int, body []byte) *Refusal {
	refusal := &Refusal{Status: status}

	if len(body) > bodyLimit {
		return refusal
	}

	// THE ROOT ELEMENT MUST BE <Error>. Without pinning it, a <Code> element
	// nested in some other document — a listing, a proxy's own XML — would be
	// read as S3's verdict on this request.
	//
	// AND THE CODES ARE COLLECTED RATHER THAN ASSIGNED. Into a string field,
	// encoding/xml overwrites on each match, so `<Error><Code>a</Code>
	// <Code>b</Code></Error>` would quietly answer `b`. A document naming two
	// codes is one billet cannot read a single verdict out of.
	var document struct {
		XMLName xml.Name `xml:"Error"`
		Codes   []string `xml:"Code"`
	}

	decoder := xml.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&document); err != nil {
		return refusal
	}

	if !endsAfter(decoder) || len(document.Codes) != 1 {
		return refusal
	}

	code := strings.TrimSpace(document.Codes[0])
	if !codeShape.MatchString(code) {
		return refusal
	}

	refusal.Code = code

	return refusal
}

// endsAfter reports whether the document was the whole body.
//
// Decode STOPS AT THE END OF THE FIRST ELEMENT and never looks further, so
// `<Error><Code>NoSuchKey</Code></Error><anything/>` decodes cleanly — and a body
// billet did not understand whole would then be allowed to say that an object is
// absent. Trailing WHITESPACE is tolerated because it carries no verdict and
// something between billet and S3 is free to add a newline.
func endsAfter(decoder *xml.Decoder) bool {
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return true
		}

		if err != nil {
			return false
		}

		text, ok := token.(xml.CharData)
		if !ok || strings.TrimSpace(string(text)) != "" {
			return false
		}
	}
}

// StatusOf reports the status of the S3 refusal in err's chain, or zero.
//
// ZERO IS "NOT AN S3 REFUSAL", which is what a transport failure, a credential
// that would not resolve and a nil error all are. A caller branching on a
// particular status therefore falls through on all three rather than treating
// them as that status.
func StatusOf(err error) int {
	if refusal, ok := errors.AsType[*Refusal](err); ok && refusal != nil {
		return refusal.Status
	}

	return 0
}
