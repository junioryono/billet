package main

import (
	"net/http"

	"github.com/junioryono/billet/internal/awss3"
)

// cacheProbeVerdict is what `billet check` should say about the ebs-s3 bucket
// probe, and it is three-valued because the probe's answer is.
type cacheProbeVerdict int

const (
	// cacheProbeFailed is the zero value on purpose: an answer nothing has
	// classified is a fault, never a pass and never an advisory line.
	cacheProbeFailed cacheProbeVerdict = iota
	// cacheProbeAnswered is the bucket reading back under this deployment's own
	// prefix.
	cacheProbeAnswered
	// cacheProbeInconclusive is a refusal that says nothing about the bucket.
	cacheProbeInconclusive
)

// judgeCacheProbe classifies what the cache bucket answered.
//
// A 403 IS NOT PROOF OF A BROKEN BUCKET. S3 answers 404 for a missing key only
// when the caller holds s3:ListBucket, and billet's minimal grant conditions that
// on s3:prefix — a context key a GetObject request does not carry — so under
// exactly the policy billet generates, a healthy miss can answer 403.
//
// IT ASKS THE ANSWER, NOT THE WORDS. This was `strings.Contains(err.Error(),
// "HTTP 403")` in the middle of a print statement, which made every diagnostic on
// the path load-bearing: reword one and a refused identity becomes a hard
// failure, or a real fault becomes an advisory line. awss3.StatusOf reads the
// refusal S3 actually sent, and reports zero for a transport failure or a
// credential that would not resolve — neither of which is a 403.
//
// A FUNCTION RATHER THAN A SWITCH IN main.go, because nothing carrying real
// semantics may live in that file: it is the one file coverage ignores, and a
// verdict there is a verdict no test can reach.
func judgeCacheProbe(err error) cacheProbeVerdict {
	switch {
	case err == nil:
		return cacheProbeAnswered
	case awss3.StatusOf(err) == http.StatusForbidden:
		return cacheProbeInconclusive
	default:
		return cacheProbeFailed
	}
}
