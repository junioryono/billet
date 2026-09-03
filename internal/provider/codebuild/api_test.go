package codebuild

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/awssig"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// A RETRIED CALL DOES NOT ACCUMULATE ROWS.
//
// The ec2 client had to zero its decode target before every attempt, because
// encoding/xml APPENDS to a slice — so a truncated body followed by a retry appended
// a full set of rows to the partial ones and DescribeInstances reported two instances
// as four, into a list that feeds a loop that destroys. encoding/json REPLACES a
// slice instead, so this client needs no such reset.
//
// THAT IS ASSERTED RATHER THAN ASSUMED, because it is a property of somebody else's
// decoder and the failure it prevents is silent duplication in an inventory. The fake
// answers the first attempt with a truncated body — retryable, since a transfer that
// failed may not have arrived — and the second with the real one.
func TestARetriedCallDoesNotAccumulateRows(t *testing.T) {
	attempts := 0
	whole := `{"builds":[{"id":"p:one","buildStatus":"IN_PROGRESS"},` +
		`{"id":"p:two","buildStatus":"IN_PROGRESS"}]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++

		w.Header().Set("Content-Type", contentType)

		body := whole
		if attempts == 1 {
			// A body that decodes partially and then fails: two rows, then a cut.
			body = whole[:len(whole)-12]
		}

		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write the scripted body: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	c := newClient("us-west-2", srv.URL+"/", staticCreds{})
	c.api.Sleep = func(_ context.Context, _ time.Duration) error { return nil }

	var out batchGetBuildsResponse
	if err := c.call(t.Context(), "BatchGetBuilds",
		map[string]any{"ids": []string{"p:one", "p:two"}}, &out); err != nil {
		t.Fatalf("call: %v", err)
	}

	if attempts != 2 {
		t.Fatalf("the call made %d attempts, want 2 (a truncated body then the real one)", attempts)
	}

	if len(out.Builds) != 2 {
		t.Errorf("the retry produced %d builds, want 2: a partial decode followed by a retry "+
			"must not accumulate rows, or an inventory double-counts and reconciliation acts "+
			"on it", len(out.Builds))
	}
}

// A REFUSED REDIRECT NEVER RENDERS THE TARGET.
//
// net/http wraps whatever CheckRedirect returns in a *url.Error, and THAT type
// renders the whole URL it was working on — which for a redirect is the target,
// chosen by whatever answered, with its query string intact. billet says the HOST and
// nothing else, and the call boundary replaces the wrapper rather than wrapping it.
func TestARedirectIsRefusedWithoutRenderingItsTarget(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{})
	}))
	t.Cleanup(target.Close)

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/?token=SECRET-IN-THE-QUERY",
			http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)

	cfg := testConfig()
	cfg.Endpoint = redirector.URL + "/"

	p, err := New(testOwner, cfg, WithCredentials(staticCreds{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = p.List(t.Context())
	if err == nil {
		t.Fatal("a redirect from the api endpoint was followed")
	}

	if strings.Contains(err.Error(), "token=SECRET-IN-THE-QUERY") {
		t.Errorf("the refusal rendered the redirect target's query: %v", err)
	}

	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("the refusal does not say what happened: %v", err)
	}
}

// AND IT IS NOT RETRIED, because it is billet's own verdict rather than a blip: an
// endpoint that redirects will redirect again, and every attempt is another signed
// request handed to whatever is answering. Measured on the ec2 side as three signed
// requests reaching the redirect target before this existed.
func TestARefusedRedirectIsNotRetried(t *testing.T) {
	hits := 0

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "https://example.invalid/", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)

	cfg := testConfig()
	cfg.Endpoint = redirector.URL + "/"

	p, err := New(testOwner, cfg, WithCredentials(staticCreds{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := p.List(t.Context()); err == nil {
		t.Fatal("a redirect was followed")
	}

	if hits != 1 {
		t.Errorf("the endpoint was asked %d times, want 1: a redirect is a verdict, and each "+
			"retry is another signed request", hits)
	}
}

// AN ERROR TYPE IS READ IN BOTH THE FORMS AWS SENDS IT.
//
// `__type` arrives bare or fully qualified, and both are the same verdict. Matching
// the raw string would make every branch depend on which form a given endpoint
// happens to send — and the branches decide whether a teardown is retried and
// whether a launch may try another shape.
func TestAnErrorTypeIsReadQualifiedOrBare(t *testing.T) {
	for raw, want := range map[string]string{
		"ResourceNotFoundException":                         "ResourceNotFoundException",
		"com.amazonaws.codebuild#ResourceNotFoundException": "ResourceNotFoundException",
		"aws.codebuild:ResourceNotFoundException":           "ResourceNotFoundException",
		"": "",
	} {
		if got := unqualified(raw); got != want {
			t.Errorf("unqualified(%q) = %q, want %q", raw, got, want)
		}
	}

	// And through the parser, so the two are not two readings of the same rule.
	err := parseAPIError([]byte(
		`{"__type":"com.amazonaws.codebuild#AccountLimitExceededException","message":"full"}`), 400)

	code, ok := codeOf(err)
	if !ok || code != "AccountLimitExceededException" {
		t.Errorf("parseAPIError read code %q (present=%v)", code, ok)
	}
}

// A BODY THAT IS NOT AWS'S SHAPE STILL YIELDS A USABLE VERDICT.
//
// A gateway, a proxy or a load balancer can answer instead of the API, and its body
// is not this shape. The status is all there is, and it is enough to decide whether
// to retry — which is what keeps a 502 from a corporate proxy being read as a
// permanent refusal.
func TestANonAWSBodyIsClassifiedByStatusAlone(t *testing.T) {
	err := parseAPIError([]byte("<html>502 Bad Gateway</html>"), http.StatusBadGateway)

	if _, ok := codeOf(err); !ok {
		t.Fatal("a non-AWS body produced no apiError, so nothing could classify it")
	}

	if !retryable(err) {
		t.Error("a 502 from a proxy is not retryable, so a transient outage would be read as " +
			"a permanent refusal")
	}

	// AND A QUOTA REFUSAL IS NOT RETRYABLE, because retrying milliseconds later
	// cannot change an account's concurrent-build limit — the fallback to another
	// declared compute type is the recovery.
	quota := parseAPIError([]byte(`{"__type":"AccountLimitExceededException"}`), 400)
	if retryable(quota) {
		t.Error("a concurrency-quota refusal is retried, which spends the launch's deadline " +
			"arriving at the same answer instead of trying the operator's next shape")
	}

	if !capacityRefusal("AccountLimitExceededException") {
		t.Error("a concurrency-quota refusal is not treated as a synchronous capacity verdict, " +
			"so the declared-shape fallback would never run")
	}

	// AND A TOKEN COLLISION IS NOT ONE. The idempotency token matching means a build
	// for this exact (lease, shape) already started — trying another shape there
	// would launch a SECOND build for one job.
	if capacityRefusal("ResourceAlreadyExistsException") {
		t.Error("a token collision is treated as a capacity refusal, so a fallback would " +
			"launch a second build for one job")
	}
}

// THE PARAMETER STORE ENDPOINT IS DERIVED AND NEVER THE CONFIGURED OVERRIDE.
//
// node.codebuild.endpoint exists for a VPC interface endpoint or a non-commercial
// partition. Pointing Parameter Store at it would send the single-use runner
// registration to whatever answers there, which is the one call in this backend whose
// destination must not be operator-controlled.
func TestTheParameterStoreEndpointIsNeverTheConfiguredOne(t *testing.T) {
	cfg := testConfig()
	cfg.Endpoint = "https://codebuild.vpce-0abc.us-west-2.vpce.amazonaws.com/"

	p, err := New(testOwner, cfg, WithCredentials(staticCreds{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if p.api.ssm == p.api.endpoint {
		t.Fatal("Parameter Store shares the configured CodeBuild endpoint, so a runner " +
			"registration would be sent to an operator-chosen host")
	}

	if want := "https://ssm.us-west-2.amazonaws.com/"; p.api.ssm != want {
		t.Errorf("the parameter store endpoint is %q, want the derived %q", p.api.ssm, want)
	}

	// AND THE PARTITION IS FOLLOWED, so a China deployment does not sign requests
	// for a host that does not exist.
	cn := testConfig()
	cn.Region = "cn-north-1"

	cnP, err := New(testOwner, cn, WithCredentials(staticCreds{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for name, got := range map[string]string{
		"codebuild": cnP.api.endpoint,
		"ssm":       cnP.api.ssm,
	} {
		if !strings.HasSuffix(got, ".amazonaws.com.cn/") {
			t.Errorf("the %s endpoint for cn-north-1 is %q, which is not in that partition",
				name, got)
		}
	}
}

// THE CONSTRUCTOR REFUSES EVERY OPTION THAT WOULD PANIC LATER.
//
// Each of these fails further from its cause than it looks: a nil HTTP client
// dereferences in the constructor, a nil logger at the first line Launch logs, and a
// nil credential source at the first signed call — on a path holding leases. billet
// bans panic outright, because a control plane that panics drops every one of them.
func TestTheConstructorRefusesOptionsThatWouldPanicLater(t *testing.T) {
	cfg := testConfig()

	for name, opts := range map[string][]Option{
		"no credentials at all": nil,
		"nil credentials":       {WithCredentials(nil)},
		// A TYPED NIL SATISFIES AN INTERFACE and passes a plain `== nil`, then
		// dereferences at the first signed call.
		"typed nil credentials": {WithCredentials((*typedNilCreds)(nil))},
		"nil logger":            {WithCredentials(staticCreds{}), WithLogger(nil)},
		"nil http client":       {WithCredentials(staticCreds{}), WithHTTPClient(nil)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(testOwner, cfg, opts...); err == nil {
				t.Error("the constructor accepted an option that would panic later")
			}
		})
	}

	// AND AN EMPTY DEPLOYMENT IDENTITY, because List feeds a loop that stops builds:
	// a provider that cannot tell its own compute from another billet's is the thing
	// that stops somebody else's work.
	if _, err := New("  ", cfg, WithCredentials(staticCreds{})); err == nil {
		t.Error("the constructor accepted an empty deployment identity")
	}
}

type typedNilCreds struct{}

func (*typedNilCreds) Credentials(context.Context) (awssig.Credentials, error) {
	return awssig.Credentials{}, nil
}

// THE CONSTRUCTOR RE-APPLIES CONFIG VALIDATION, because it is exported and cannot
// assume its configuration came through config.Load — the alloc.New rule. The region
// is the least obvious and most important of them: it is interpolated into the
// default endpoint, so an unvalidated one chooses the HOST a signed request reaches.
func TestTheConstructorReAppliesConfigValidation(t *testing.T) {
	for name, mutate := range map[string]func(*config.CodeBuildConfig){
		"hostile region": func(c *config.CodeBuildConfig) { c.Region = "x@attacker.example/?" },
		"no project":     func(c *config.CodeBuildConfig) { c.Project = "" },
		"plaintext endpoint": func(c *config.CodeBuildConfig) {
			c.Endpoint = "http://codebuild.internal/"
		},
		"wildcard jit path": func(c *config.CodeBuildConfig) { c.JITParameterPath = "/billet/*" },
		"no compute types": func(c *config.CodeBuildConfig) {
			c.ComputeTypes = nil
		},
		"macos without a fleet": func(c *config.CodeBuildConfig) {
			c.EnvironmentType = config.CodeBuildMacARM
			c.PrivilegedMode = false
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			mutate(&cfg)

			if _, err := New(testOwner, cfg, WithCredentials(staticCreds{})); err == nil {
				t.Error("the constructor accepted a configuration config.Load refuses")
			}
		})
	}

	// THE CEILING ACKNOWLEDGEMENT IS DELIBERATELY NOT AMONG THEM. It gates a node
	// that will SERVE work, not a provider a diagnostic can build — and `billet
	// check --provider codebuild` constructs one precisely in order to report what
	// those ceilings are.
	cfg := testConfig()
	cfg.AcceptExternalBuildCeiling = false

	if _, err := New(testOwner, cfg, WithCredentials(staticCreds{})); err != nil {
		t.Errorf("the constructor refused an unacknowledged ceiling, which would stop `billet "+
			"check` from reporting the very limit an operator has not acknowledged: %v", err)
	}
}

// THE CALLER KEEPS THE CONFIG, SO THE SHAPES ARE CLONED.
//
// A caller that can widen a shape list after construction can buy compute the ledger
// never authorised — the same reason ec2.New clones its security groups, where the
// consequence was moving a fork's job onto a privileged network after the validation
// that was supposed to prevent it.
func TestTheConstructorClonesTheShapeList(t *testing.T) {
	cfg := testConfig()

	p, err := New(testOwner, cfg, WithCredentials(staticCreds{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cfg.ComputeTypes[0].Type = "SOMETHING-ELSE"

	if p.cfg.ComputeTypes[0].Type == "SOMETHING-ELSE" {
		t.Error("the provider shares its shape list with the caller's config, so a shape can " +
			"be changed after validation")
	}
}

// THE CONSTRUCTOR PREPARES ITS CONFIG THE WAY config.Load DOES, all of it.
//
// New is exported, so it cannot assume its configuration came through Load — the
// alloc.New rule — and the first version reproduced only part of Load by hand. Two
// halves were missing and each one is a request AWS refuses while every check billet
// makes passes: a padded compute type is VALIDATED trimmed and SENT raw, and an
// omitted timeout is valid (zero means "not stated") and sent as a zero override.
//
// ASSERTED THROUGH WHAT THE PROVIDER WOULD SEND, not through the config struct, because
// what config.Prepare wrote is only interesting if the launch path reads it.
func TestTheConstructorPreparesItsConfigTheWayLoadDoes(t *testing.T) {
	cfg := testConfig()

	// A padded shape name, and no timeouts at all.
	cfg.ComputeTypes[0].Type = "  " + cfg.ComputeTypes[0].Type + "\t"
	cfg.BuildTimeoutMinutes = 0
	cfg.QueuedTimeoutMinutes = 0

	want := strings.TrimSpace(cfg.ComputeTypes[0].Type)

	p, err := New(testOwner, cfg, WithCredentials(staticCreds{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := p.cfg.ComputeTypes[0].Type; got != want {
		t.Errorf("the provider would send compute type %q; AWS refuses that as an unknown "+
			"compute type and names nothing, while every check billet makes passed because "+
			"validation examines a trimmed copy", got)
	}

	if got := p.cfg.BuildTimeoutMinutes; got != config.CodeBuildBuildCeilingMinutes {
		t.Errorf("build timeout = %d, want the service ceiling %d: a zero override is refused "+
			"by StartBuild, so a config that named no ceiling could not launch at all",
			got, config.CodeBuildBuildCeilingMinutes)
	}

	if got := p.cfg.QueuedTimeoutMinutes; got != config.CodeBuildQueuedCeilingMinutes {
		t.Errorf("queued timeout = %d, want the service ceiling %d", got,
			config.CodeBuildQueuedCeilingMinutes)
	}

	// AND THE CALLER'S OWN CONFIG IS UNTOUCHED, because Prepare writes into the
	// compute-type slice: preparing before the clone would trim shapes in a config the
	// caller still holds.
	if cfg.ComputeTypes[0].Type == want {
		t.Error("the constructor trimmed the caller's own shape list in place")
	}
}

// THE INVENTORY WINDOW COMES FROM THE PREPARED CEILINGS, which is what makes the
// defaulting above load-bearing rather than cosmetic.
//
// A zero build ceiling produces a window of nothing but the slack, and `List` frees the
// capacity of every lease absent from its answer — so a walk that stops an hour back
// reads a running build started before that as gone.
func TestTheInventoryWindowIsDerivedFromThePreparedCeilings(t *testing.T) {
	cfg := testConfig()
	cfg.BuildTimeoutMinutes = 0
	cfg.QueuedTimeoutMinutes = 0

	p, err := New(testOwner, cfg, WithCredentials(staticCreds{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	floor := config.CodeBuildBuildCeilingMinutes + config.CodeBuildQueuedCeilingMinutes
	if got := p.cfg.InventoryWindowMinutes(); got < floor {
		t.Errorf("InventoryWindowMinutes() = %d, want at least %d — a window shorter than the "+
			"ceilings billet sends reads a running build as absent, and List frees the "+
			"capacity of everything absent from its answer", got, floor)
	}
}

// AND provider.InstanceName ROUND-TRIPS, because the name is the only durable link
// between a running build and the lease that authorised it.
func TestTheLeaseNameRoundTrips(t *testing.T) {
	const lease = "0123456789abcdef0123456789abcdef"

	name := provider.InstanceName(lease)

	got, ok := provider.LeaseOf(name)
	if !ok || got != lease {
		t.Errorf("LeaseOf(%q) = (%q, %v), want (%q, true)", name, got, ok, lease)
	}
}
