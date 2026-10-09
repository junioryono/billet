package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// awsRegionRe matches the SHAPE of a region rather than a list of them.
//
// An allowlist is a rule about somebody else's product, and it goes stale the
// next time AWS opens a region — at which point billet refuses a config that is
// perfectly correct. The shape catches the mistake people actually make, which is
// dropping the hyphens, and still admits partitions billet has never run in:
// us-gov-west-1, cn-north-1, ap-southeast-4.
var awsRegionRe = regexp.MustCompile(`^[a-z]{2,}(-[a-z]+)+-\d+$`)

// AWSDNSSuffix is the DNS suffix of the partition a region belongs to.
//
// DECLARED HERE AND USED THERE, exactly like TapPrefix and for the same reason:
// config may not import awsjson, and a second copy of this rule in this file would
// be a constant that can drift from the thing it is validating against.
// awsjson.DNSSuffixFor is this function.
//
// MEASURED 2026-09-04 rather than read, because the documentation lists endpoints
// per service page and gets the legacy forms wrong. `sqs.cn-north-1.amazonaws.com`
// and `cn-north-1.queue.amazonaws.com` do not resolve; `sqs.cn-north-1.amazonaws.com.cn`
// does, and `cn-north-1.queue.amazonaws.com.cn` is a CNAME onto it. In the other
// direction `sqs.us-west-2.amazonaws.com.cn` does not resolve. GOVCLOUD IS NOT A
// SEPARATE CASE despite being a separate partition: `sqs.us-gov-west-1.amazonaws.com`
// resolves and the .cn form does not. The VPC-endpoint zone is delegated per
// partition too — `vpce.amazonaws.com` is served by ns-1714.awsdns-22.co.uk and
// `vpce.amazonaws.com.cn` by ns-960.awsdns-cn-60.com — which is corroboration
// rather than a probe, since the names below it are created with an endpoint.
//
// A PARTITION BILLET HAS NOT BEEN TAUGHT ABOUT answers "amazonaws.com" here, and
// the SQS host check therefore REFUSES its queue URL rather than admitting a host
// that is not one: the ISO partitions are not under amazonaws.com at all. That is
// the safe direction and it is unchanged.
func AWSDNSSuffix(region string) string {
	if strings.HasPrefix(region, "cn-") {
		return "amazonaws.com.cn"
	}

	return "amazonaws.com"
}

func checkSignedEndpoint(field, endpoint string) error {
	if strings.TrimSpace(endpoint) == "" {
		return nil
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("%s is not a url", field)
	}

	if u.Opaque != "" {
		return fmt.Errorf("%s is not a url billet can dial: it has no // and therefore no host, so "+
			"nothing says which machine to sign a request for", field)
	}

	if u.User != nil {
		return fmt.Errorf("%s must not carry a username or password: billet authenticates with a "+
			"request signature, so one would be a credential in a string that gets logged and "+
			"nothing else", field)
	}

	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s must not carry a query string or fragment: billet builds every request "+
			"itself, so anything there is either ignored or a secret in a value that gets "+
			"logged", field)
	}

	if u.Scheme != "https" && u.Scheme != "http" {
		return endpointNeedsHTTPS(field)
	}

	if u.Hostname() == "" {
		return fmt.Errorf("%s names no host", field)
	}

	// THE SERVICE LIVES AT THE ROOT, for both callers. A path here would be
	// signed and sent, so `https://vpce.example/v1` addresses every call
	// somewhere that is not the service — and no AWS regional, VPC-interface or
	// non-commercial-partition endpoint needs one, nor does an ordinary Ceph RGW
	// or MinIO. Absent and "/" are both the root.
	if path := u.EscapedPath(); path != "" && path != "/" {
		return fmt.Errorf("%s must name a host with no path: billet builds the request path "+
			"itself and signs whatever it is given", field)
	}

	if u.Scheme == "https" || isLoopbackHost(u.Hostname()) {
		return nil
	}

	return endpointNeedsHTTPS(field)
}

// endpointNeedsHTTPS names the rule without naming the value.
func endpointNeedsHTTPS(field string) error {
	return fmt.Errorf("%s must use https: billet signs each request and sends a session token "+
		"with it, so plaintext hands an on-path observer a replayable request. Only a loopback "+
		"address may use http, where the trust boundary is the machine itself", field)
}

// ssmParameterPathRe is a Parameter Store hierarchy prefix: a leading slash, then
// segments of letters, digits, underscore, dot and hyphen.
//
// NO WILDCARD AND NO TRAILING SLASH, and the wildcard is the one that matters: this
// prefix lands in an IAM Resource ARN, so a `*` or `?` in it widens the node's
// ssm:PutParameter grant to sibling paths — which on a shared account is another
// deployment's runner registrations. The same rule awspolicy states for a cache
// prefix, applied where the value is written.
// reservedSSMNamespace reports whether a parameter path lands in a namespace
// Parameter Store keeps for itself.
//
// CASE-INSENSITIVE, AND MEASURED. The path grammar admits uppercase, so a
// case-sensitive check let `/AWS/billet` through — and AWS refuses it. Asked in
// us-west-2 on 2026-08-31, PutParameter answered:
//
//	/AWS/billet/…  AccessDeniedException: No access to reserved parameter name
//	/Aws/billet/…  AccessDeniedException: No access to reserved parameter name
//	/SSM/billet/…  ValidationException: can't be prefixed with "ssm" (case-insensitive)
//	/aws/billet/…  AccessDeniedException: No access to reserved parameter name
//
// AWS's own message says case-insensitive. Refusing at load is the point: the
// alternative is a config that validates and then fails on every registration, which
// is a tier advertising capacity it cannot serve.
func reservedSSMNamespace(path string) bool {
	lower := strings.ToLower(path)

	return strings.HasPrefix(lower, "/aws") || strings.HasPrefix(lower, "/ssm")
}

var ssmParameterPathRe = regexp.MustCompile(`^(/[A-Za-z0-9_.-]+)+$`)

// CheckSSMParameterPath refuses a Parameter Store prefix that would widen an IAM
// grant or land in a namespace AWS keeps for itself.
//
// EXPORTED BECAUSE THE PATH HAS TWO READERS AND ONE RULE. A codebuild node writes
// registrations under it and its config is checked here at load; the node then
// REPORTS the same path at registration, and the control plane sweeps under it,
// so alloc.RegisterNode re-applies this to a value that arrived over the wire
// rather than through Load (the alloc.New rule). Two spellings of the rule would
// be two spellings that drift.
//
// The message starts with the quoted value so a caller can prefix the config key
// it is checking.
func CheckSSMParameterPath(path string) error {
	switch {
	case path == "":
		return errors.New("must name a Parameter Store path")

	case !ssmParameterPathRe.MatchString(path):
		// THE WILDCARD IS THE DANGEROUS ONE and the message says so, because a `*`
		// here reads as a harmless glob and is a widened IAM Resource: on a shared
		// account the sibling paths it admits are other deployments' runner
		// registrations.
		return fmt.Errorf("%q must be an absolute Parameter Store path with no trailing slash "+
			"and no wildcard (letters, digits, _ . - and / only); it lands in an IAM Resource "+
			"arn, so a * or ? widens the node's parameter grant to paths it should not reach",
			path)

	case reservedSSMNamespace(path):
		return fmt.Errorf("%q starts with a namespace AWS reserves (/aws, /ssm, in any case), "+
			"so PutParameter would refuse every registration", path)
	}

	return nil
}
