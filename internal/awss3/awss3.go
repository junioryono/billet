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
// already believes. WHAT WAS OBSERVED, exactly, is two unauthenticated GETs,
// both answering HTTP 404 with a body ending at </Error> and no trailing byte:
//
//	one key that was not there, in a public bucket that was:
//	  <Error><Code>NoSuchKey</Code><Message>The specified key does not
//	  exist.</Message><Key>…</Key><RequestId>…</RequestId><HostId>…</HostId></Error>
//
//	one key in a randomly named bucket that could not exist:
//	  <Error><Code>NoSuchBucket</Code><Message>The specified bucket does not
//	  exist</Message><BucketName>…</BucketName><RequestId>…</RequestId>…</Error>
//
// TWO INFERENCES, LABELLED AS THAT. That the first answered 404 rather than 403
// is consistent with that bucket granting anonymous s3:ListBucket, which is the
// condition internal/store/ebss3's CheckAccess already names — no policy was
// read. And that the second answered without a credential suggests the name is
// resolved before the request is authorized, which is a reading of one response
// rather than a measurement of S3's ordering. Neither inference is what the code
// rests on: it rests on the two codes.
//
// reals3_test.go is the same pair SIGNED, against a bucket an operator points it
// at, which is the run that would contradict any of this.
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

	// THE CODES ARE COLLECTED RATHER THAN ASSIGNED. Into a string field,
	// encoding/xml overwrites on each match, so `<Error><Code>a</Code>
	// <Code>b</Code></Error>` would quietly answer `b`. A document naming two
	// codes is one billet cannot read a single verdict out of.
	//
	// AND EACH ONE IS READ TWICE, AS TEXT AND AS RAW XML. Unmarshalling into a
	// string SKIPS nested elements and concatenates the character data around
	// them, so `<Code>NoSuch<x>Bucket</x>Key</Code>` answers `NoSuchKey` — a code
	// billet assembled out of a document it did not understand. Requiring the two
	// readings to be equal is what says the element is text and nothing else. It
	// refuses CDATA and entity references too, which S3 does not send and which
	// would each be a second spelling of a verdict.
	var document struct {
		Codes []struct {
			XMLName xml.Name
			Text    string `xml:",chardata"`
			Inner   string `xml:",innerxml"`
		} `xml:"Code"`
	}

	// STRIPPED BEFORE THE DECODER SEES IT. Go hands a leading byte-order mark
	// back as character data, which is not whitespace, and rootOf would then
	// refuse a document that is otherwise exactly right — turning a healthy miss
	// into a failure on any S3-compatible endpoint that emits one.
	decoder := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(body, utf8BOM)))

	// THE ELEMENT MUST BE S3's OWN <Error>, IN NO NAMESPACE. An encoding/xml tag
	// without a namespace matches ANY namespace, so `<x:Error xmlns:x="urn:not-s3">`
	// would otherwise be read as S3's verdict on this request; and without the
	// name pinned at all, a <Code> inside some other document — a listing, a
	// proxy's own XML — would be. Both measured bodies carry no namespace.
	start, ok := rootOf(decoder)
	if !ok || start.Name != (xml.Name{Local: "Error"}) {
		return refusal
	}

	if err := decoder.DecodeElement(&document, &start); err != nil {
		return refusal
	}

	if !endsAfter(decoder) || len(document.Codes) != 1 {
		return refusal
	}

	only := document.Codes[0]
	if only.XMLName != (xml.Name{Local: "Code"}) || only.Inner != only.Text {
		return refusal
	}

	code := strings.TrimSpace(only.Text)
	if !codeShape.MatchString(code) {
		return refusal
	}

	refusal.Code = code

	return refusal
}

// utf8BOM is the byte-order mark some XML writers put in front of a document.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// rootOf returns the body's one element, provided nothing but a prolog came
// before it.
//
// Decode SKIPS WHATEVER PRECEDES THE FIRST ELEMENT, character data included, so
// `garbage<Error><Code>NoSuchKey</Code></Error>` decodes cleanly — and a body
// billet did not understand whole would then be allowed to say that an object is
// absent.
func rootOf(decoder *xml.Decoder) (xml.StartElement, bool) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return xml.StartElement{}, false
		}

		if start, ok := token.(xml.StartElement); ok {
			return start, true
		}

		if !carriesNoVerdict(token) {
			return xml.StartElement{}, false
		}
	}
}

// endsAfter reports whether the element was the last of the body.
//
// DecodeElement STOPS AT THE END OF THAT ELEMENT and never looks further, so
// `<Error><Code>NoSuchKey</Code></Error><anything/>` decodes cleanly, and the
// same sentence applies at this end as at the other one.
func endsAfter(decoder *xml.Decoder) bool {
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return true
		}

		if err != nil || !carriesNoVerdict(token) {
			return false
		}
	}
}

// carriesNoVerdict reports whether a token is the matter XML allows AROUND a
// document's one element: a declaration, a comment, a directive, or whitespace.
//
// TOLERATED RATHER THAN REQUIRED, and tolerated in both directions because a
// document is one element with that matter either side of it. None of it says
// anything about an object, and refusing a body over a newline something added
// in front of S3 would turn a healthy miss into a failure — which is the
// opposite mistake and the more expensive one.
func carriesNoVerdict(token xml.Token) bool {
	switch t := token.(type) {
	case xml.ProcInst, xml.Comment, xml.Directive:
		return true
	case xml.CharData:
		return strings.TrimSpace(string(t)) == ""
	default:
		return false
	}
}

// hintShape is what billet will repeat out of a header.
//
// THE HINT IS REMOTE BYTES TOO. X-Amz-Bucket-Region lands in an operator's
// terminal beside the code, and the code is shape-checked for exactly that
// reason; a header is no more trustworthy than a body, and archivestore takes a
// configured endpoint, so the far side is not always AWS.
//
// IT IS NOT AN AWS REGION SHAPE, AND DELIBERATELY NOT. What has to be true is
// that the bytes are bounded and safe to print — printable ASCII, no spaces, no
// control characters — not that they name a region billet has heard of. A Ceph
// RGW or MinIO deployment signs with whatever region name its operator chose,
// and judging the name would throw away the only wrong-region diagnostic those
// deployments get. It is quoted where it is rendered.
var hintShape = regexp.MustCompile(`^[!-~]{1,64}$`)

// RegionHint reports the bucket's real region when S3 named one.
//
// S3 answers a wrong-region request with a redirect carrying this header, and an
// operator staring at a bare 301 has no other way to see that their region is
// wrong. It is empty when the header is absent or is not something billet will
// repeat.
func RegionHint(response *http.Response) string {
	if response == nil {
		return ""
	}

	hint := strings.TrimSpace(response.Header.Get("X-Amz-Bucket-Region"))
	if !hintShape.MatchString(hint) {
		return ""
	}

	return hint
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
