package codebuild

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/awssig"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// Provider must satisfy the interface the node runtime drives. A compile-time
// assertion rather than a test, because a missing method is a build failure and
// discovering it from a test is discovering it later.
var _ provider.Provider = (*Provider)(nil)

// staticCreds is a credential source for tests.
//
// It carries a recognisable secret so a redaction test can assert the secret is
// ABSENT rather than assert that some string is present — the difference between
// proving nothing leaked and proving a particular rendering happened to be short.
type staticCreds struct{}

func (staticCreds) Credentials(context.Context) (awssig.Credentials, error) {
	return awssig.Credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI-SECRET-DO-NOT-LEAK",
		SessionToken:    "SESSION-TOKEN-DO-NOT-LEAK",
	}, nil
}

// fakeAWS is a scripted CodeBuild and Parameter Store.
//
// IT RECORDS EVERY REQUEST BODY, which is what lets the security assertions be
// about bytes rather than about intent: "the registration never appears in a
// request billet did not intend it in" is checkable only if every request is kept.
type fakeAWS struct {
	t *testing.T

	mu sync.Mutex
	// requests is every (target, body) pair, in order.
	requests []recorded
	// builds is the fake's world, keyed by build id.
	builds map[string]*fakeBuild
	// params is Parameter Store, keyed by name; paramWritten is when each was last
	// written, which the listing reports as LastModifiedDate.
	params       map[string]string
	paramWritten map[string]time.Time
	// onParamPage, when set, runs under the lock after each GetParametersByPath
	// page is computed and before it is answered, so a test can change the store
	// BETWEEN two page requests — the positional cursor's one hazard.
	onParamPage func(page int)
	// nextID names each new build.
	nextID int
	// project is the project name the fake will answer about.
	project string
	// webhook makes DescribeProject report a competing runner webhook.
	webhook bool
	// projectOwner is the tag the fake's project carries.
	projectOwner string
	// projectVPC, when set, is the vpcConfig BatchGetProjects reports for the
	// project — what an untrusted launch verifies against the declared network.
	projectVPC *fakeVPC
	// projectErr, when set, is returned for the next BatchGetProjects.
	projectErr []apiFault
	// projectMissing makes BatchGetProjects answer with no project at all.
	projectMissing bool
	// fleetContext, when set, is the status context BatchGetFleets reports beside
	// an ACTIVE status code — what a fleet with no instance behind it says.
	fleetContext string
	// fleetMaxCapacity, when set, gives the fleet a scaling configuration with
	// that maximum.
	fleetMaxCapacity int
	// startErr, when set, is returned for the next StartBuild as an API error.
	startErr []apiFault
	// stopErr, when set, is returned for the next StopBuild.
	stopErr []apiFault
	// listParamsErr, when set, is returned for the next GetParametersByPath, and
	// deleteParamErr for the next DeleteParameter.
	listParamsErr  []apiFault
	deleteParamErr []apiFault
	// paramListCalls counts GetParametersByPath calls, so a pagination test can
	// prove how many pages were walked rather than only that every name was seen.
	paramListCalls int
	// paramCycle makes every GetParametersByPath page hand back the same token, the
	// listing that never ends.
	paramCycle bool
	// listPages overrides ListBuildsForProject with scripted pages.
	listPages []listPage
	// listCalls counts ListBuildsForProject calls, so a test can prove a walk
	// stopped rather than merely that it returned.
	listCalls int
	// reverseBatch makes BatchGetBuilds answer in the OPPOSITE order to the request.
	//
	// A SERVICE IS ALLOWED TO. AWS documents BatchGetBuilds as answering about the
	// ids it was given and says nothing about order — and measured against real
	// CodeBuild it happens to preserve it, which is why billet's dependence on that
	// was invisible. A fake that always preserved the order could not see it either.
	reverseBatch bool
	// signedHeaders is the SignedHeaders list off each Authorization header, so a
	// test can assert WHICH headers were covered rather than only that something
	// was signed.
	signedHeaders []string
}

type recorded struct {
	target string
	body   string
}

type apiFault struct {
	status int
	code   string
}

// fakeVPC is the vpcConfig the fake reports for its project.
type fakeVPC struct {
	vpcID            string
	subnets          []string
	securityGroupIDs []string
}

type listPage struct {
	ids       []string
	nextToken string
}

type fakeBuild struct {
	id      string
	status  string
	phase   string
	started time.Time
	env     []envVar
}

func newFakeAWS(t *testing.T) *fakeAWS {
	t.Helper()

	return &fakeAWS{
		t:            t,
		builds:       map[string]*fakeBuild{},
		params:       map[string]string{},
		paramWritten: map[string]time.Time{},
		project:      "billet-linux",
		projectOwner: testOwner,
		nextID:       1,
	}
}

const testOwner = "dep-0123456789abcdef"

// addOwnedBuild puts a running build billet owns into the fake's world.
//
// IN THE WINDOW AND CARRYING BOTH MARKERS, because the walk skips anything else —
// so a scripted page of ids the fake does not hold proves nothing about the walk's
// stopping rules. That is how the first version of the pagination-cycle test passed
// against a client that never reached the cycle.
func (f *fakeAWS) addOwnedBuild(id, lease string) {
	f.addOwnedBuildWithStatus(id, lease, "IN_PROGRESS")
}

// addOwnedBuildWithStatus is addOwnedBuild for a build that is already over.
//
// A LEASE'S TERMINAL BUILD IS NOT INTERCHANGEABLE WITH ITS LIVE ONE, which is why the
// status has to be settable: custody reads a terminal answer from Find as causal proof
// and settles the lease on it, so a test where every build carries the same status
// cannot see which one Find picked for the reason that matters.
func (f *fakeAWS) addOwnedBuildWithStatus(id, lease, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.builds[id] = &fakeBuild{
		id: id, status: status, phase: "BUILD", started: time.Now(),
		env: []envVar{
			{Name: ownerEnvVar, Value: testOwner, Type: "PLAINTEXT"},
			{Name: nameEnvVar, Value: lease, Type: "PLAINTEXT"},
		},
	}
}

// recordSignedHeaders keeps the SignedHeaders list off one Authorization header.
func (f *fakeAWS) recordSignedHeaders(auth string) {
	_, rest, ok := strings.Cut(auth, "SignedHeaders=")
	if !ok {
		return
	}

	list, _, _ := strings.Cut(rest, ",")

	f.mu.Lock()
	defer f.mu.Unlock()

	f.signedHeaders = append(f.signedHeaders, strings.TrimSpace(list))
}

func (f *fakeAWS) record(target, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.requests = append(f.requests, recorded{target: target, body: body})
}

// bodies returns every recorded request body, so an assertion can sweep all of them.
func (f *fakeAWS) bodies() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]recorded, len(f.requests))
	copy(out, f.requests)

	return out
}

func (f *fakeAWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target := r.Header.Get("X-Amz-Target")

	body := make([]byte, 0, 4096)
	buf := make([]byte, 4096)

	for {
		n, err := r.Body.Read(buf)
		body = append(body, buf[:n]...)

		if err != nil {
			break
		}
	}

	f.record(target, string(body))

	// EVERY REQUEST MUST BE SIGNED. A fake that answered an unsigned request would
	// let the signing path be deleted without a test noticing — which is exactly
	// the "proving the mechanism is not proving it is used" failure.
	auth := r.Header.Get("Authorization")
	if auth == "" {
		http.Error(w, `{"__type":"AccessDeniedException","message":"unsigned"}`,
			http.StatusForbidden)

		return
	}

	f.recordSignedHeaders(auth)

	var in map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			http.Error(w, `{"__type":"InvalidInputException","message":"bad json"}`,
				http.StatusBadRequest)

			return
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case strings.HasSuffix(target, ".StartBuild"):
		f.startBuild(w, in)
	case strings.HasSuffix(target, ".StopBuild"):
		f.stopBuild(w, in)
	case strings.HasSuffix(target, ".BatchGetBuilds"):
		f.batchGetBuilds(w, in)
	case strings.HasSuffix(target, ".ListBuildsForProject"):
		f.listBuilds(w, in)
	case strings.HasSuffix(target, ".BatchGetProjects"):
		f.batchGetProjects(w)
	case strings.HasSuffix(target, ".BatchGetFleets"):
		f.batchGetFleets(w, in)
	case strings.HasSuffix(target, ".PutParameter"):
		f.putParameter(w, in)
	case strings.HasSuffix(target, ".DeleteParameter"):
		f.deleteParameter(w, in)
	case strings.HasSuffix(target, ".GetParametersByPath"):
		f.getParametersByPath(w, in)
	default:
		http.Error(w, `{"__type":"InvalidAction","message":"unknown target"}`,
			http.StatusBadRequest)
	}
}

func (f *fakeAWS) fault(w http.ResponseWriter, queue *[]apiFault) bool {
	if len(*queue) == 0 {
		return false
	}

	next := (*queue)[0]
	*queue = (*queue)[1:]

	f.writeErr(w, next.status, next.code, "scripted")

	return true
}

func (f *fakeAWS) startBuild(w http.ResponseWriter, in map[string]any) {
	if f.fault(w, &f.startErr) {
		return
	}

	id := f.project + ":" + string(rune('a'+f.nextID-1)) + "0000000"
	f.nextID++

	b := &fakeBuild{id: id, status: "IN_PROGRESS", phase: "QUEUED", started: time.Now()}

	for _, raw := range asSlice(in["environmentVariablesOverride"]) {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		b.env = append(b.env, envVar{
			Name:  asString(m["name"]),
			Value: asString(m["value"]),
			Type:  asString(m["type"]),
		})
	}

	f.builds[id] = b

	writeJSON(w, map[string]any{"build": f.describe(b)})
}

func (f *fakeAWS) stopBuild(w http.ResponseWriter, in map[string]any) {
	if f.fault(w, &f.stopErr) {
		return
	}

	id := asString(in["id"])

	b, ok := f.builds[id]
	if !ok {
		f.writeErr(w, http.StatusBadRequest, "ResourceNotFoundException", "no such build")

		return
	}

	// A STOP IS A REQUEST. The fake models exactly that: the status moves to
	// STOPPING, which is not terminal, and only a later poll finds STOPPED. A fake
	// that went straight to STOPPED would let Destroy return TeardownStopped without
	// ever polling, and the test asserting it polls would pass with the poll deleted.
	b.status = "STOPPING"

	writeJSON(w, map[string]any{"build": f.describe(b)})
}

func (f *fakeAWS) batchGetBuilds(w http.ResponseWriter, in map[string]any) {
	var (
		found    []map[string]any
		notFound []string
	)

	for _, raw := range asSlice(in["ids"]) {
		id := asString(raw)

		b, ok := f.builds[id]
		if !ok {
			notFound = append(notFound, id)

			continue
		}

		// STOPPING SETTLES ON THE NEXT LOOK, which is what makes the poll
		// observable: the first BatchGetBuilds after a stop reports STOPPING and the
		// second reports STOPPED.
		if b.status == "STOPPING" {
			b.status = "STOPPED"
			found = append(found, f.describeWith(b, "STOPPING"))

			continue
		}

		found = append(found, f.describe(b))
	}

	if f.reverseBatch {
		for i, j := 0, len(found)-1; i < j; i, j = i+1, j-1 {
			found[i], found[j] = found[j], found[i]
		}
	}

	writeJSON(w, map[string]any{"builds": found, "buildsNotFound": notFound})
}

func (f *fakeAWS) listBuilds(w http.ResponseWriter, in map[string]any) {
	f.listCalls++

	// SORT ORDER MUST NEVER BE SENT. AWS documents ListBuildsForProject as erroring
	// when sortOrder is passed and the project has more than 100 builds, so a client
	// that sent it works in every test and breaks on the first busy project.
	if _, sent := in["sortOrder"]; sent {
		f.writeErr(w, http.StatusBadRequest, "InvalidInputException",
			"sortOrder is not supported above 100 builds")

		return
	}

	// SCRIPTED PAGES ARE SERVED IN ORDER, driven by the call count rather than by
	// the token the client sent — so a client that echoed the wrong token still
	// walks the script, and what the test observes is the walk's own stopping rule
	// rather than the fake's bookkeeping.
	if len(f.listPages) > 0 {
		page := f.listPages[min(f.listCalls-1, len(f.listPages)-1)]

		writeJSON(w, map[string]any{"ids": page.ids, "nextToken": page.nextToken})

		return
	}

	// NEWEST FIRST, which is the default order billet relies on.
	ids := make([]string, 0, len(f.builds))
	for id := range f.builds {
		ids = append(ids, id)
	}

	// Deterministic, and newest-first: ids are assigned in ascending order, so
	// reversing the sorted list is newest first.
	sortStrings(ids)
	reverseStrings(ids)

	writeJSON(w, map[string]any{"ids": ids})
}

func (f *fakeAWS) batchGetProjects(w http.ResponseWriter) {
	if f.fault(w, &f.projectErr) {
		return
	}

	if f.projectMissing {
		writeJSON(w, map[string]any{"projects": []any{}, "projectsNotFound": []string{f.project}})

		return
	}

	project := map[string]any{
		"name": f.project,
		"arn":  "arn:aws:codebuild:us-west-2:000000000000:project/" + f.project,
		"environment": map[string]any{
			"type": "LINUX_CONTAINER", "computeType": "BUILD_GENERAL1_MEDIUM",
		},
		"source": map[string]any{"type": "NO_SOURCE"},
	}

	if f.projectOwner != "" {
		project["tags"] = []map[string]any{{"key": "sh.billet.owner", "value": f.projectOwner}}
	}

	if f.projectVPC != nil {
		project["vpcConfig"] = map[string]any{
			"vpcId":            f.projectVPC.vpcID,
			"subnets":          f.projectVPC.subnets,
			"securityGroupIds": f.projectVPC.securityGroupIDs,
		}
	}

	if f.webhook {
		project["webhook"] = map[string]any{
			"url": "https://example.invalid/hook",
			"filterGroups": [][]map[string]any{{
				{"type": "EVENT", "pattern": "WORKFLOW_JOB_QUEUED"},
			}},
		}
	}

	writeJSON(w, map[string]any{"projects": []map[string]any{project}})
}

func (f *fakeAWS) batchGetFleets(w http.ResponseWriter, in map[string]any) {
	names := asSlice(in["names"])
	if len(names) == 0 {
		writeJSON(w, map[string]any{"fleets": []any{}})

		return
	}

	status := map[string]any{"statusCode": "ACTIVE"}
	if f.fleetContext != "" {
		status["context"] = f.fleetContext
		status["message"] = "We currently do not have sufficient capacity for the instance " +
			"type you requested. Please try again later."
	}

	fleet := map[string]any{
		"name":            "macs",
		"arn":             asString(names[0]),
		"environmentType": "MAC_ARM",
		"computeType":     "BUILD_GENERAL1_MEDIUM",
		"baseCapacity":    2,
		"status":          status,
	}
	if f.fleetMaxCapacity > 0 {
		fleet["scalingConfiguration"] = map[string]any{"maxCapacity": f.fleetMaxCapacity}
	}

	writeJSON(w, map[string]any{"fleets": []map[string]any{fleet}})
}

func (f *fakeAWS) putParameter(w http.ResponseWriter, in map[string]any) {
	name := asString(in["Name"])

	if _, exists := f.params[name]; exists {
		f.writeErr(w, http.StatusBadRequest, "ParameterAlreadyExists", "exists")

		return
	}

	if got := asString(in["Type"]); got != "SecureString" {
		f.t.Errorf("a runner registration was staged as %q rather than SecureString", got)
	}

	f.params[name] = asString(in["Value"])
	f.paramWritten[name] = time.Now()

	writeJSON(w, map[string]any{"Version": 1})
}

// fakeCiphertext is what the fake returns as every SecureString's Value when no
// decryption was asked for.
//
// DISTINCTIVE SO ITS ABSENCE IS CHECKABLE: the sweep must decode no value at all,
// and asserting that a log does not contain "AQICAHh" would be satisfied by a log
// that contained nothing. Asserting it does not contain THIS proves the value the
// service handed back did not travel.
const fakeCiphertext = "AQICAHh-CIPHERTEXT-MUST-NOT-APPEAR-ANYWHERE"

// getParametersByPath lists one level under a path, ten names a page, in name
// order, with the ciphertext AWS returns for an undecrypted SecureString.
//
// THE FAKE ASSERTS THE REQUEST'S SHAPE, because the shape IS the security
// property: a sweep that asked for decryption would receive registrations, and one
// that walked recursively would list a hierarchy somebody else keeps under a
// shared prefix.
func (f *fakeAWS) getParametersByPath(w http.ResponseWriter, in map[string]any) {
	f.paramListCalls++

	if f.fault(w, &f.listParamsErr) {
		return
	}

	if decrypt, ok := in["WithDecryption"].(bool); !ok || decrypt {
		f.t.Errorf("GetParametersByPath was asked with WithDecryption=%v; the sweep must state "+
			"false, because it must never hold a registration", in["WithDecryption"])
	}

	if recursive, ok := in["Recursive"].(bool); !ok || recursive {
		f.t.Errorf("GetParametersByPath was asked with Recursive=%v; the sweep lists one level "+
			"so a hierarchy under a shared prefix is not walked", in["Recursive"])
	}

	path := asString(in["Path"])
	prefix := strings.TrimSuffix(path, "/") + "/"

	pageSize := 10
	if v, ok := in["MaxResults"].(float64); ok && v > 0 {
		pageSize = int(v)
	}

	var names []string

	for name := range f.params {
		rel, ok := strings.CutPrefix(name, prefix)
		if !ok || rel == "" || strings.Contains(rel, "/") {
			continue
		}

		names = append(names, name)
	}

	slices.Sort(names)

	// A POSITIONAL CURSOR, BECAUSE THAT IS WHAT THE REAL ONE IS. MEASURED against
	// Parameter Store on 2026-09-02: three parameters at MaxResults 1, delete the
	// one page one returned, and page two fetched with the old token returns the
	// THIRD — the second is never listed. A fake that modelled a kinder, key-based
	// cursor would let a sweep that deleted while paging pass every test here and
	// skip entries in production; this one is what caught that sweep.
	start := 0
	if tok := asString(in["NextToken"]); tok != "" {
		offset, ok := strings.CutPrefix(tok, "offset:")
		if !ok {
			f.writeErr(w, http.StatusBadRequest, "InvalidNextToken", "bad token")

			return
		}

		n, err := strconv.Atoi(offset)
		if err != nil {
			f.writeErr(w, http.StatusBadRequest, "InvalidNextToken", "bad token")

			return
		}

		start = min(n, len(names))
	}

	end := min(start+pageSize, len(names))

	params := make([]map[string]any, 0, end-start)
	for _, name := range names[start:end] {
		entry := map[string]any{
			"Name": name, "Type": "SecureString", "Value": fakeCiphertext, "Version": 1,
		}

		// THE WRITE TIME TRAVELS AS AWS SENDS IT: unix seconds, fractional. A
		// parameter staged with no time reports none, which the sweep must read as
		// "cannot vouch for its age".
		if written, ok := f.paramWritten[name]; ok && !written.IsZero() {
			entry["LastModifiedDate"] = float64(written.UnixNano()) / 1e9
		}

		params = append(params, entry)
	}

	out := map[string]any{"Parameters": params}

	switch {
	case f.paramCycle:
		out["NextToken"] = "offset:0"
	case end < len(names):
		out["NextToken"] = "offset:" + strconv.Itoa(end)
	}

	if f.onParamPage != nil {
		f.onParamPage(f.paramListCalls)
	}

	writeJSON(w, out)
}

// paramNames is every parameter the fake holds, in name order.
func (f *fakeAWS) paramNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	names := make([]string, 0, len(f.params))
	for name := range f.params {
		names = append(names, name)
	}

	slices.Sort(names)

	return names
}

func (f *fakeAWS) deleteParameter(w http.ResponseWriter, in map[string]any) {
	if f.fault(w, &f.deleteParamErr) {
		return
	}

	name := asString(in["Name"])

	if _, exists := f.params[name]; !exists {
		f.writeErr(w, http.StatusBadRequest, "ParameterNotFound", "absent")

		return
	}

	delete(f.params, name)
	delete(f.paramWritten, name)

	writeJSON(w, map[string]any{})
}

// describe renders one build the way BatchGetBuilds does.
//
// THE PARAMETER_STORE VARIABLE IS ECHOED AS ITS NAME, not its value, which is what
// AWS documents and what the whole secret channel depends on. Modelling it the other
// way would make the leak assertion pass against a service that leaks.
func (f *fakeAWS) describe(b *fakeBuild) map[string]any {
	return f.describeWith(b, b.status)
}

func (f *fakeAWS) describeWith(b *fakeBuild, status string) map[string]any {
	env := make([]map[string]any, 0, len(b.env))
	for _, v := range b.env {
		env = append(env, map[string]any{"name": v.Name, "value": v.Value, "type": v.Type})
	}

	return map[string]any{
		"id":            b.id,
		"buildStatus":   status,
		"buildComplete": terminalStatus(status),
		"currentPhase":  b.phase,
		"startTime":     float64(b.started.Unix()),
		"environment":   map[string]any{"environmentVariables": env},
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", contentType)

	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"__type":"InternalServerError"}`, http.StatusInternalServerError)

		return
	}

	if _, err := w.Write(body); err != nil {
		panic("fakeAWS: write a response: " + err.Error())
	}
}

// writeErr answers with one of AWS's JSON error shapes.
//
// A WRITE FAILURE PANICS RATHER THAN BEING DISCARDED, because a fake that silently
// fails to answer makes the client's own timeout the observed behaviour, and a test
// asserting a refusal then passes for the wrong reason.
func (f *fakeAWS) writeErr(w http.ResponseWriter, status int, code, message string) {
	f.t.Helper()

	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)

	if _, err := w.Write([]byte(`{"__type":"` + code + `","message":"` + message + `"}`)); err != nil {
		f.t.Errorf("write a %s response: %v", code, err)
	}
}

// asString reads a request field the fake expects to be a string.
//
// THE MISSING CASE IS NAMED rather than left to comma-ok's zero value: an absent
// field and a field of the wrong type both mean the request did not carry what the
// fake is about to assert on, and an empty string reads as "billet sent nothing".
func asString(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}

	return s
}

func asSlice(v any) []any {
	s, ok := v.([]any)
	if !ok {
		return nil
	}

	return s
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func reverseStrings(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

// testConfig is a valid on-demand Linux node config.
func testConfig() config.CodeBuildConfig {
	cfg := config.CodeBuildConfig{
		Region:                     "us-west-2",
		Project:                    "billet-linux",
		EnvironmentType:            config.CodeBuildLinuxContainer,
		PrivilegedMode:             true,
		AcceptExternalBuildCeiling: true,
		JITParameterPath:           "/billet/jit",
		ComputeTypes: []config.RemoteShape{{
			Type: "BUILD_GENERAL1_MEDIUM", VCPU: 4, Memory: 7 * config.GiB,
			PriceUSDPerHour: 10000,
		}},
	}

	return cfg
}

// newTestProvider wires a Provider to a fake AWS.
//
// THE ENDPOINT COMES FROM httptest, WHICH CARRIES A SCHEME. That matters: the one
// code path that builds a base URL from configuration had no test at all on the
// node-client side, and `billet node` could not construct a single request. Here the
// config's Endpoint is what New validates and what the client dials, so a test that
// bypassed it would prove nothing about the configured path.
func newTestProvider(t *testing.T, f *fakeAWS, mutate func(*config.CodeBuildConfig)) *Provider {
	t.Helper()

	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	cfg := testConfig()
	cfg.Endpoint = srv.URL + "/"

	if mutate != nil {
		mutate(&cfg)
	}

	p, err := New(testOwner, cfg, WithCredentials(staticCreds{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Parameter Store is derived rather than configured — deliberately, so an
	// operator cannot aim the runner registration at a host of their choosing — so
	// a test has to reach past the config to point it at the fake.
	p.api.ssm = srv.URL + "/"

	// The teardown poll must not spend wall clock, and its replacement still
	// honours the context so a cancellation test is not made vacuous.
	p.sleep = func(ctx context.Context, _ time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		return nil
	}

	return p
}

// launchSpec is a complete, valid launch.
func launchSpec(name string) provider.Spec {
	return provider.Spec{
		Name:      name,
		Image:     "aws/codebuild/amazonlinux-x86_64-standard:5.0",
		VCPU:      4,
		Memory:    7 * config.GiB,
		Command:   []string{"./run.sh"},
		Trust:     provider.TrustTrusted,
		JITConfig: theRegistration,
	}
}

// theRegistration is a recognisable stand-in for a JIT config.
//
// IT IS A DISTINCTIVE STRING SO ITS ABSENCE IS CHECKABLE. Asserting that a request
// body does not contain "eyJ..." would be satisfied by a body that contained
// nothing at all; asserting it does not contain this proves the value billet was
// handed did not travel.
const theRegistration = "BILLET-JIT-REGISTRATION-MUST-NOT-APPEAR-ANYWHERE"
