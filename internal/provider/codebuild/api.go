package codebuild

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	stdhttp "net/http"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awsjson"
)

// targetPrefix is the API version this client speaks, carried in X-Amz-Target.
// Pinned, because the request and response shapes below are that version's.
const targetPrefix = "CodeBuild_20161006."

// service is the SigV4 service name. It is part of the credential scope, so a typo
// here is indistinguishable from a bad secret key.
const service = "codebuild"

// ssmService and ssmTargetPrefix address Parameter Store, which carries the
// single-use runner registration. A SECOND SERVICE ON ONE CLIENT, deliberately:
// the signer takes the service as a parameter, and giving the JIT channel its own
// HTTP client and its own redaction table would be a second security boundary for
// no benefit.
const (
	ssmService      = "ssm"
	ssmTargetPrefix = "AmazonSSM."
)

// THE TRANSPORT, THE RETRY LADDER AND THE ERROR CLASSIFICATION LIVE IN
// internal/awsjson SINCE SHARED IDENTITY LANDED, and the aliases below are what
// keeps this package's call sites and its tests talking about codebuild rather
// than about a shared package.
//
// EXTRACTED RATHER THAN COPIED, for the reason internal/awscreds was: the control
// plane now reaches Parameter Store for the deployment's identity material, and a
// compute backend cannot be a library for the rest of billet. What moved is
// everything that is the same for any AWS JSON 1.1 service; what stayed is every
// judgement that is about CodeBuild.
type apiError = awsjson.APIError

// errRedirected is billet's refusal of a redirect from a signed endpoint. It is
// the SHARED sentinel, because errors.Is has to match across the package
// boundary: a value of this package's own would make awsjson's checks false.
var errRedirected = awsjson.ErrRedirected

// contentType is what AWS JSON 1.1 requires.
const contentType = awsjson.ContentType

// apiTimeout bounds one API call.
const apiTimeout = awsjson.APITimeout

// CredentialSource resolves AWS credentials.
//
// AN ALIAS FOR awscreds.Source SINCE THE CHAIN MOVED, and it used to be a
// declaration. The chain lived in internal/provider/ec2, so importing it would
// have made one compute backend a library for a sibling — this package therefore
// declared its own interface over the same method and cmd/billet adapted ec2's
// chain into it. The chain is a shared package now, and the second interface has
// nothing left to be: what it named is what awscreds.Source names.
//
// KEPT AS A NAME rather than deleted, because a caller reading this package's
// constructor should see what kind of thing it wants without following an import.
//
// THERE IS NO DEFAULT. New refuses a nil source rather than reaching for one, so a
// caller cannot end up signing with credentials it did not choose.
type CredentialSource = awscreds.Source

// client talks the CodeBuild and Parameter Store JSON APIs over signed HTTPS.
//
//nolint:recvcheck // The redaction methods MUST take a value receiver: a pointer-receiver String is not consulted when a VALUE is formatted, so %+v on a dereferenced client would print the secret out of its unexported api field. Every other method needs the pointer. The ec2 client carries the identical exception for the identical reason.
type client struct {
	api      *awsjson.Client
	endpoint string
	ssm      string
	// quotas is the Service Quotas endpoint, DERIVED like ssm rather than
	// configured: it is a different service from CodeBuild, so it cannot share
	// node.codebuild.endpoint, and an operator has no reason to aim a read-only
	// diagnostic at a host of their choosing. A test sets it directly.
	quotas string
}

// REDACTED, AND MOVING THE CREDENTIAL SOURCE ONE LEVEL DOWN DID NOT REMOVE THE
// NEED — it is exactly the trap `awscreds.IMDS` already records.
//
// The source now lives inside awsjson.Client, which redacts itself on every path.
// That is irrelevant here: `api` is an UNEXPORTED field, and reflect refuses to
// call methods on one, so nothing awsjson.Client implements is ever consulted
// when the struct around it is printed structurally. `%+v` would walk straight
// into its fields and print the secret access key in full, past a layer of
// redaction that works perfectly in isolation.
//
// ON A VALUE RECEIVER, which is the rule and which the ec2 client broke on its
// first attempt: a pointer receiver is not consulted when a VALUE is formatted.
func (c client) String() string { return "codebuild.client{endpoint=" + c.endpoint + "}" }

// GoString covers %#v.
func (c client) GoString() string { return c.String() }

// Format catches every verb. Implementing it means fmt never consults String or
// GoString, which is why they are also called directly by the redaction test.
func (c client) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, c.String()) //nolint:errcheck // fmt.State swallows write errors by design
}

// MarshalJSON keeps a client out of anything that serializes it structurally.
func (c client) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }

// LogValue is what slog consults; its JSON handler ignores fmt entirely.
func (c client) LogValue() slog.Value { return slog.StringValue(c.String()) }

// The four accessors the options and the constructor need, so nothing outside
// this file reaches through to the shared client.
func (c *client) creds() CredentialSource         { return c.api.CredentialSource() }
func (c *client) setCreds(src CredentialSource)   { c.api.SetCredentials(src) }
func (c *client) httpClient() *stdhttp.Client     { return c.api.HTTPClient() }
func (c *client) setHTTPClient(h *stdhttp.Client) { c.api.SetHTTPClient(h) }

// codeOf reports the API error code in a chain, and whether there was one.
func codeOf(err error) (string, bool) { return awsjson.CodeOf(err) }

// unqualified strips the namespace AWS prefixes onto a JSON error type.
func unqualified(t string) string { return awsjson.Unqualified(t) }

// parseAPIError turns a non-200 into an apiError.
func parseAPIError(payload []byte, status int) error {
	return awsjson.ParseAPIError(service, payload, status)
}

// retryable reports whether an attempt is worth repeating.
//
// AND RETRYING IS NOT WHAT MAKES A LAUNCH SAFE HERE, unlike the ec2 backend.
// StartBuild's idempotencyToken is valid for FIVE MINUTES — AWS documents the
// window — where ec2's ClientToken was measured still refusing a changed relaunch
// of the same lease long afterwards. So a retry inside one call is covered by the
// token, and nothing above this may retry a launch on its own; see Launch.
func retryable(err error) bool { return awsjson.Retryable(err) }

// retryableRefusal is `retryable` narrowed to outcomes AWS ANSWERED WITH.
//
// The distinction is whether billet knows the request was not acted on. A throttle
// says so; a dropped connection, a body that would not parse and a 5xx do not — and
// for an action that creates compute, "may have been processed" and "was processed"
// have to be treated the same way. See callCreating.
func retryableRefusal(err error) bool { return awsjson.RetryableRefusal(err) }

// newClient builds the signed client for one region, resolving both endpoints.
func newClient(region, endpoint string, creds CredentialSource) *client {
	if endpoint == "" {
		endpoint = defaultEndpointFor(region)
	}

	return &client{
		api:      awsjson.New(service, region, creds),
		endpoint: endpoint,
		ssm:      defaultSSMEndpointFor(region),
	}
}

// call issues one CodeBuild action and unmarshals the response into out.
func (c *client) call(ctx context.Context, action string, in, out any) error {
	return c.api.Invoke(ctx, c.endpoint, service, targetPrefix+action, action, in, out, retryable)
}

// callCreating issues an action that CREATES something, and it retries only what
// AWS itself has refused to act on.
//
// AN AMBIGUOUS OUTCOME IS NEVER RETRIED, which is a stronger rule than `retryable`
// and the reason this exists. A dropped connection, an unparseable body and a 5xx
// are all outcomes where the request MAY have been processed — for a read that is a
// reason to ask again, and for StartBuild it means a build may already be running.
//
// THE IDEMPOTENCY TOKEN IS NOT THE ANSWER, and leaning on it was the defect. It is
// valid for FIVE MINUTES (AWS documents it; measured against real CodeBuild, an
// identical retry inside that window returns the SAME build id and a changed
// parameter is refused with `InvalidInputException: Request parameter mismatch for
// idempotency token`). Five minutes is a wall clock, the provider accepts a
// caller-supplied *http.Client with no bound on its timeout, and correctness that
// depends on a retry landing inside a window billet does not control is not
// correctness. Refusing the retry makes the window irrelevant.
//
// WHAT IS STILL RETRIED is a throttle: AWS answered, and its answer is that it did
// not act. That is a refusal billet received, not an outcome it cannot see.
func (c *client) callCreating(ctx context.Context, action string, in, out any) error {
	return c.api.Invoke(ctx, c.endpoint, service, targetPrefix+action, action, in, out,
		retryableRefusal)
}

// callSSM issues one Parameter Store action.
//
// A SEPARATE ENDPOINT AND A SEPARATE SIGNING SERVICE, never the configured
// CodeBuild override: node.codebuild.endpoint exists for a VPC interface endpoint
// or a non-commercial partition, and pointing Parameter Store at it would send the
// runner registration to whatever answers there.
func (c *client) callSSM(ctx context.Context, action string, in, out any) error {
	return c.api.Invoke(ctx, c.ssm, ssmService, ssmTargetPrefix+action, action, in, out, retryable)
}

// defaultEndpointFor derives the regional CodeBuild endpoint.
//
// THE SUFFIX IS NOT THE SAME IN EVERY PARTITION, the rule the ec2 client already
// states: the region check deliberately admits partitions billet has never run in,
// so the commercial suffix would derive a host that does not exist for `cn-north-1`.
// AWS China is reached at amazonaws.com.cn; GovCloud uses the commercial suffix.
func defaultEndpointFor(region string) string {
	return awsjson.EndpointFor(service, region)
}

// defaultSSMEndpointFor derives the regional Parameter Store endpoint.
//
// DERIVED RATHER THAN CONFIGURABLE, and that is the safety property: the runner
// registration is written here, so an operator override would be a way to send a
// single-use credential to a host of somebody's choosing. A deployment in a
// partition billet has not been taught about needs a code change rather than a
// config field.
func defaultSSMEndpointFor(region string) string {
	return awsjson.EndpointFor(ssmService, region)
}

// build is what BatchGetBuilds and StartBuild say about one build.
//
// ONLY THE FIELDS BILLET ACTS ON. The response carries dozens more, and decoding
// them would make this struct a second, drifting description of somebody else's
// API — the reason internal/scaleset exists rather than importing a preview type.
type build struct {
	ID string `json:"id"`
	// BuildStatus is IN_PROGRESS, SUCCEEDED, FAILED, FAULT, TIMED_OUT or STOPPED.
	BuildStatus string `json:"buildStatus"`
	// BuildComplete is CodeBuild's own answer to "is this over", carried beside
	// the status because the two are recorded at different moments and a status
	// this build has never heard of must not be read as finished.
	BuildComplete bool `json:"buildComplete"`
	// CurrentPhase says how far a running build got, which is what establishes
	// that the runner registration was consumed.
	CurrentPhase string `json:"currentPhase"`
	// StartTime is unix seconds (possibly fractional), and it is what bounds the
	// inventory walk: a build older than the declared ceilings cannot be running.
	StartTime float64 `json:"startTime"`
	// Environment carries the environment-variable overrides back, which is where
	// billet's owner and lease markers live — a CodeBuild build cannot be tagged.
	Environment struct {
		EnvironmentVariables []envVar `json:"environmentVariables"`
	} `json:"environment"`
	// Phases is where a TIMEOUT actually appears, and it is decoded for that one
	// reason. See TimedOut: buildStatus does NOT say TIMED_OUT for a build
	// CodeBuild ended at its ceiling.
	Phases []buildPhase `json:"phases"`
}

// buildPhase is one entry of a build's phase list.
type buildPhase struct {
	PhaseType string `json:"phaseType"`
	// PhaseStatus is TIMED_OUT for the phase CodeBuild's build timeout ended,
	// while the build's own status is FAILED. MEASURED — see TimedOut.
	PhaseStatus string `json:"phaseStatus"`
	Contexts    []struct {
		StatusCode string `json:"statusCode"`
		Message    string `json:"message"`
	} `json:"contexts"`
}

// envVar is one environment-variable override as the API reports it.
//
// THE TYPE IS READ, NOT JUST THE NAME AND VALUE. A PARAMETER_STORE variable's
// `value` is the parameter's NAME rather than its contents, and asserting that is
// what proves the registration never reached the response — see
// TestTheRegistrationNeverAppearsInABuildDescription.
type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Type  string `json:"type"`
}

// value reports one variable's value, and whether it was present.
func envValue(vars []envVar, name string) (string, bool) {
	for _, v := range vars {
		if v.Name == name {
			return v.Value, true
		}
	}

	return "", false
}

// startBuildResponse is what a launch answers with.
type startBuildResponse struct {
	Build build `json:"build"`
}

// batchGetBuildsResponse is what a lookup answers with.
//
// BuildsNotFound is read as well as Builds: CodeBuild answers a batch with two
// lists, and treating an id that is simply absent as an empty Build would report a
// build billet cannot see as one whose status is the zero value — which
// runningState reads as running, forever.
type batchGetBuildsResponse struct {
	Builds         []build  `json:"builds"`
	BuildsNotFound []string `json:"buildsNotFound"`
}

// listBuildsForProjectResponse is one page of build ids, newest first.
//
// NextToken is why this is a page rather than the answer, and pagination is not
// optional: these ids feed reconciliation and teardown, so a truncated list reads
// as "that lease is not running here" — which frees capacity for a build that is
// still executing a job.
type listBuildsForProjectResponse struct {
	IDs       []string `json:"ids"`
	NextToken string   `json:"nextToken"`
}

// projectDescription is what BatchGetProjects says about the project billet uses.
type projectDescription struct {
	Name string `json:"name"`
	ARN  string `json:"arn"`
	Tags []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"tags"`
	Environment struct {
		Type        string `json:"type"`
		ComputeType string `json:"computeType"`
	} `json:"environment"`
	Source struct {
		Type string `json:"type"`
	} `json:"source"`
	// VpcConfig is the network the project's on-demand builds run on. It is what an
	// untrusted build's isolation actually rests on, because StartBuild has no VPC
	// override — so the launch path verifies this against the declared
	// node.codebuild.untrusted_* before it starts a fork's build. Absent (nil) on a
	// project with no VPC configuration, which for an untrusted tier is the failure.
	VpcConfig *struct {
		VpcID            string   `json:"vpcId"`
		Subnets          []string `json:"subnets"`
		SecurityGroupIDs []string `json:"securityGroupIds"`
	} `json:"vpcConfig"`
	// Webhook is the field that answers the one question billet must not get
	// wrong about a project it did not create: a WORKFLOW_JOB_QUEUED webhook
	// means CodeBuild is also acquiring jobs, and two schedulers on one job
	// produce duplicate runners.
	Webhook *struct {
		URL          string `json:"url"`
		FilterGroups [][]struct {
			Type    string `json:"type"`
			Pattern string `json:"pattern"`
		} `json:"filterGroups"`
	} `json:"webhook"`
}

type batchGetProjectsResponse struct {
	Projects         []projectDescription `json:"projects"`
	ProjectsNotFound []string             `json:"projectsNotFound"`
}

// fleetDescription is what BatchGetFleets says about a reserved-capacity fleet.
type fleetDescription struct {
	Name            string `json:"name"`
	ARN             string `json:"arn"`
	EnvironmentType string `json:"environmentType"`
	ComputeType     string `json:"computeType"`
	BaseCapacity    int    `json:"baseCapacity"`
	Status          struct {
		StatusCode string `json:"statusCode"`
		// Context is AWS's reason when the code alone does not say enough — an
		// ACTIVE fleet with no instance behind it reports INSUFFICIENT_CAPACITY here
		// and nothing in StatusCode (measured 2026-09-02).
		Context string `json:"context"`
		Message string `json:"message"`
	} `json:"status"`
	ScalingConfiguration *struct {
		MaxCapacity int `json:"maxCapacity"`
	} `json:"scalingConfiguration"`
}

type batchGetFleetsResponse struct {
	Fleets         []fleetDescription `json:"fleets"`
	FleetsNotFound []string           `json:"fleetsNotFound"`
}
