package awsjson

import "testing"

// THE ENDPOINT TAKES THE PARTITION'S OWN SUFFIX, AND THE RULE IS ASKED FOR RATHER
// THAN REPEATED.
//
// EndpointFor is what awsssm and the codebuild client address every request with,
// so a commercial suffix in a cn- region is a host that does not exist and a client
// that reaches nothing. The rule itself lives in internal/config, because config's
// spot-queue validator has to select the same suffix and may not import this
// package; this exercises the call rather than the constant, so a delegation that
// stopped delegating — or answered a fixed string — fails the China cases here.
//
// GOVCLOUD IS THE INTERESTING ROW. It is a partition of its own and takes the
// COMMERCIAL suffix, so a rule shaped as "commercial or not" gets it wrong in the
// direction nothing else in the suite would catch.
func TestTheEndpointTakesThePartitionsSuffix(t *testing.T) {
	t.Parallel()

	for region, want := range map[string]string{
		"cn-north-1":     "amazonaws.com.cn",
		"cn-northwest-1": "amazonaws.com.cn",
		"us-west-2":      "amazonaws.com",
		"us-gov-west-1":  "amazonaws.com",
	} {
		if got := DNSSuffixFor(region); got != want {
			t.Errorf("DNSSuffixFor(%q) = %q, want %q", region, got, want)
		}

		wantEndpoint := "https://sqs." + region + "." + want + "/"
		if got := EndpointFor("sqs", region); got != wantEndpoint {
			t.Errorf("EndpointFor(sqs, %q) = %q, want %q", region, got, wantEndpoint)
		}
	}
}
