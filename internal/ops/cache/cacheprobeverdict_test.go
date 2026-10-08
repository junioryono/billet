package cache

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/junioryono/billet/internal/awss3"
)

// THE VERDICT COMES FROM THE ANSWER S3 SENT, NOT FROM THE WORDS OF A MESSAGE.
//
// `billet check` reads a 403 from the ebs-s3 bucket probe as INCONCLUSIVE rather
// than as a broken bucket, because billet's minimal grant conditions
// s3:ListBucket on s3:prefix — a context key a GetObject request does not carry —
// so a healthy miss can answer 403 under exactly the policy billet generates.
//
// THE DECEPTIVE CASE IS THE ONE THAT MATTERS. This branch was selected by looking
// for the substring "HTTP 403" in the probe error's rendered message, so an error
// that merely CONTAINS those characters was read as a refused identity, and any
// reword of a diagnostic on the path changed the verdict. That error is in the
// table below and must be a failure.
func TestTheCacheProbeVerdictReadsTheRefusalRatherThanTheMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want ProbeVerdict
	}{
		{
			name: "a bucket that answered", err: nil, want: ProbeAnswered,
		},
		{
			name: "a refusal S3 sent",
			err: fmt.Errorf("ebs-s3: the cache bucket did not answer a probe read: %w",
				fmt.Errorf("ebs-s3: S3 GET returned %w",
					&awss3.Refusal{Status: http.StatusForbidden, Code: "AccessDenied"})),
			want: ProbeInconclusive,
		},
		{
			// PROSE THAT LOOKS LIKE A REFUSAL IS NOT ONE. Nothing here carries an
			// S3 answer, so it must not reach the advisory branch.
			name: "a message that merely says HTTP 403",
			err:  errors.New("ebs-s3: something else entirely went wrong: HTTP 403"),
			want: ProbeFailed,
		},
		{
			name: "a bucket that does not exist",
			err: fmt.Errorf("ebs-s3: S3 GET returned %w",
				&awss3.Refusal{Status: http.StatusNotFound, Code: awss3.CodeNoSuchBucket}),
			want: ProbeFailed,
		},
		{
			// A transport failure carries no S3 answer either, and reporting it
			// as inconclusive would hide an unreachable bucket behind an
			// advisory line.
			name: "a host that could not be dialled",
			err:  errors.New("ebs-s3: call S3: dial tcp: connection refused"),
			want: ProbeFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := JudgeProbe(tc.err); got != tc.want {
				t.Errorf("judgeCacheProbe = %d, want %d", got, tc.want)
			}
		})
	}
}
