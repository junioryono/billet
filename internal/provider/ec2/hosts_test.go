package ec2

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func poolHostXML(id, owner, order, state string) string {
	return fmt.Sprintf(`<item><instanceId>%s</instanceId><instanceType>m7gd.metal</instanceType>`+
		`<privateIpAddress>10.77.1.9</privateIpAddress><instanceState><name>%s</name></instanceState>`+
		`<tagSet><item><key>Name</key><value>billet-pool-%s</value></item>`+
		`<item><key>sh.billet.pool-owner</key><value>%s</value></item>`+
		`<item><key>sh.billet.pool-order</key><value>%s</value></item></tagSet></item>`,
		id, state, order, owner, order)
}

func hostLaunch() HostLaunch {
	return HostLaunch{Order: "o-1", InstanceType: "m7gd.metal", Image: "ami-0host", UserData: []byte("join token-9QX")}
}

// A POOL HOST IS LAUNCHED UNDER ITS ORDER'S CLIENT TOKEN, so a launch sent again after
// a lost answer starts no second host, with the metadata service closed to its guests
// and tags of its own rather than a job instance's.
func TestAPoolHostIsLaunchedUnderItsOrder(t *testing.T) {
	t.Parallel()
	f := newFakeEC2(t)
	f.respond = func(action string, _ url.Values) (int, string) {
		if action == "RunInstances" {
			return http.StatusOK, `<RunInstancesResponse><instancesSet>` +
				poolHostXML("i-0host", "dep-1", "o-1", "pending") + `</instancesSet></RunInstancesResponse>`
		}

		return http.StatusOK, defaultReply(action)
	}
	rec, err := newTestProvider(t, f, nil).Hosts().Launch(t.Context(), hostLaunch())
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if rec.ID != "i-0host" || rec.Order != "o-1" || rec.State != "pending" {
		t.Fatalf("record = %+v", rec)
	}
	params := f.paramsFor(t, "RunInstances")
	for key, want := range map[string]string{
		"ClientToken":                "pool-o-1",
		"InstanceType":               "m7gd.metal",
		"ImageId":                    "ami-0host",
		"MetadataOptions.HttpTokens": "required",
		"MetadataOptions.HttpPutResponseHopLimit": "1",
		"SecurityGroupId.1":                       "sg-trusted",
		"TagSpecification.1.Tag.2.Key":            poolOwnerTag,
		"TagSpecification.1.Tag.2.Value":          "dep-1",
		"TagSpecification.1.Tag.3.Key":            poolOrderTag,
		"TagSpecification.1.Tag.3.Value":          "o-1",
		"TagSpecification.1.Tag.1.Value":          "billet-pool-o-1",
	} {
		if got := params.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if ud, err := base64.StdEncoding.DecodeString(params.Get("UserData")); err != nil || string(ud) != "join token-9QX" {
		t.Errorf("user data = %q (%v)", ud, err)
	}
	// NEVER THE JOB INSTANCES' OWNER TAG: List fails a node's inventory on an
	// owner-tagged instance whose name names no lease.
	for key, values := range params {
		for _, v := range values {
			if v == ownerTag {
				t.Errorf("%s carries the job instances' owner tag", key)
			}
		}
	}
}

// THE REQUEST CARRIES THE JOIN TOKEN, so a refusal keeps EC2's code and drops its
// message, which an echoing endpoint could fill with the request.
func TestALaunchRefusalKeepsTheCodeAndDropsTheMessage(t *testing.T) {
	t.Parallel()
	f := newFakeEC2(t)
	f.respond = func(action string, _ url.Values) (int, string) {
		if action == "RunInstances" {
			return http.StatusBadRequest, `<Response><Errors><Error><Code>VcpuLimitExceeded</Code>` +
				`<Message>echo: join token-9QX</Message></Error></Errors></Response>`
		}

		return http.StatusOK, defaultReply(action)
	}
	_, err := newTestProvider(t, f, nil).Hosts().Launch(t.Context(), hostLaunch())
	if err == nil {
		t.Fatal("a refused launch succeeded")
	}
	if code, ok := codeOf(err); !ok || code != "VcpuLimitExceeded" {
		t.Errorf("the code is lost: %v", err)
	}
	if strings.Contains(err.Error(), "token-9QX") {
		t.Errorf("the refusal quoted the request: %v", err)
	}
}

func TestAnOrderThatCannotBeATokenOrAFilterIsRefused(t *testing.T) {
	t.Parallel()
	f := newFakeEC2(t)
	h := newTestProvider(t, f, nil).Hosts()
	for _, order := range []string{"", "o-*", "O-1", "o 1", strings.Repeat("a", 49)} {
		l := hostLaunch()
		l.Order = order
		if _, err := h.Launch(t.Context(), l); err == nil {
			t.Errorf("order %q was launched", order)
		}
		if _, _, err := h.Find(t.Context(), order); err == nil {
			t.Errorf("order %q was looked up", order)
		}
	}
	if n := f.countOf("RunInstances") + f.countOf("DescribeInstances"); n != 0 {
		t.Fatalf("%d calls reached EC2 for orders it refuses", n)
	}
}

// A LOST ANSWER IS READ BACK BY THE ORDER'S TAG: one live host is the answer, two is
// an error, and a terminated one is reported as such rather than as none.
func TestFindReadsAnOrderBack(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		items  []string
		wantOK bool
		wantID string
		err    bool
	}{
		"none":                 {nil, false, "", false},
		"one live":             {[]string{poolHostXML("i-1", "dep-1", "o-1", "running")}, true, "i-1", false},
		"another order's host": {[]string{poolHostXML("i-9", "dep-1", "o-10", "running")}, false, "", false},
		"live beside an ended": {[]string{poolHostXML("i-0", "dep-1", "o-1", "terminated"), poolHostXML("i-1", "dep-1", "o-1", "pending")}, true, "i-1", false},
		"only ended":           {[]string{poolHostXML("i-0", "dep-1", "o-1", "terminated")}, true, "i-0", false},
		"two live":             {[]string{poolHostXML("i-1", "dep-1", "o-1", "running"), poolHostXML("i-2", "dep-1", "o-1", "running")}, false, "", true},
	} {
		f := newFakeEC2(t)
		f.respond = func(action string, _ url.Values) (int, string) {
			return http.StatusOK, describeReply("", c.items...)
		}
		rec, ok, err := newTestProvider(t, f, nil).Hosts().Find(t.Context(), "o-1")
		if (err != nil) != c.err || ok != c.wantOK || rec.ID != c.wantID {
			t.Errorf("%s: find = %+v, %t, %v", name, rec, ok, err)
		}
		params := f.paramsFor(t, "DescribeInstances")
		if params.Get("Filter.1.Name") != "tag:"+poolOwnerTag || params.Get("Filter.1.Value.1") != "dep-1" ||
			params.Get("Filter.2.Name") != "tag:"+poolOrderTag || params.Get("Filter.2.Value.1") != "o-1" {
			t.Errorf("%s: filters = %v", name, params)
		}
	}
}

// AN INSTANCE THAT IS NOT THIS DEPLOYMENT'S POOL HOST is neither described as one nor
// terminated, whatever id the caller names.
func TestOnlyThisDeploymentsPoolHostsAreTerminated(t *testing.T) {
	t.Parallel()
	for name, item := range map[string]string{
		"a job's instance":          instanceXML("i-1", "billet-lease"),
		"another deployment's host": poolHostXML("i-1", "dep-2", "o-1", "running"),
	} {
		f := newFakeEC2(t)
		f.respond = func(action string, _ url.Values) (int, string) {
			if action == "DescribeInstances" {
				return http.StatusOK, describeReply("", item)
			}

			return http.StatusOK, defaultReply(action)
		}
		err := newTestProvider(t, f, nil).Hosts().Terminate(t.Context(), "i-1")
		if !errors.Is(err, ErrNotAPoolHost) {
			t.Errorf("%s: terminate = %v", name, err)
		}
		if n := f.countOf("TerminateInstances"); n != 0 {
			t.Errorf("%s: %d terminations sent", name, n)
		}
	}
	f := newFakeEC2(t)
	f.respond = func(action string, _ url.Values) (int, string) {
		if action == "DescribeInstances" {
			return http.StatusOK, describeReply("", poolHostXML("i-1", "dep-1", "o-1", "running"))
		}

		return http.StatusOK, defaultReply(action)
	}
	if err := newTestProvider(t, f, nil).Hosts().Terminate(t.Context(), "i-1"); err != nil {
		t.Fatalf("terminate this deployment's host: %v", err)
	}
	if got := f.paramsFor(t, "TerminateInstances").Get("InstanceId.1"); got != "i-1" {
		t.Fatalf("terminated %q", got)
	}
}

// NOT FOUND IS ITS OWN ANSWER, never a record of a terminated host.
func TestDescribeSaysNotFoundAsItsOwnAnswer(t *testing.T) {
	t.Parallel()
	f := newFakeEC2(t)
	f.respond = func(string, url.Values) (int, string) {
		return http.StatusBadRequest, apiFailure("InvalidInstanceID.NotFound")
	}
	if _, err := newTestProvider(t, f, nil).Hosts().Describe(t.Context(), "i-1"); !errors.Is(err, ErrHostNotFound) {
		t.Fatalf("describe = %v", err)
	}
}

func jobAndPoolTaggedXML(id string) string {
	return `<item><instanceId>` + id + `</instanceId><instanceState><name>running</name></instanceState>` +
		`<tagSet><item><key>Name</key><value>billet-lease</value></item>` +
		`<item><key>sh.billet.owner</key><value>dep-1</value></item>` +
		`<item><key>sh.billet.pool-owner</key><value>dep-1</value></item>` +
		`<item><key>sh.billet.pool-order</key><value>o-1</value></item></tagSet></item>`
}

func poolOwnerOnlyXML(id string) string {
	return `<item><instanceId>` + id + `</instanceId><instanceState><name>running</name></instanceState>` +
		`<tagSet><item><key>sh.billet.pool-owner</key><value>dep-1</value></item></tagSet></item>`
}

// AN INSTANCE THAT CARRIES THE JOB INSTANCES' OWNER TAG, or the pool tag without an
// order, is not a pool host for any of the three operations, and an order lookup that
// meets one is an error rather than "none".
func TestAnInstanceWithConflictingOrMissingPoolTagsIsNotAPoolHost(t *testing.T) {
	t.Parallel()
	for name, item := range map[string]string{
		"job and pool tags":  jobAndPoolTaggedXML("i-1"),
		"pool tag, no order": poolOwnerOnlyXML("i-1"),
	} {
		f := newFakeEC2(t)
		f.respond = func(action string, _ url.Values) (int, string) {
			if action == "DescribeInstances" {
				return http.StatusOK, describeReply("", item)
			}

			return http.StatusOK, defaultReply(action)
		}
		h := newTestProvider(t, f, nil).Hosts()
		if _, err := h.Describe(t.Context(), "i-1"); !errors.Is(err, ErrNotAPoolHost) {
			t.Errorf("%s: describe = %v", name, err)
		}
		if err := h.Terminate(t.Context(), "i-1"); !errors.Is(err, ErrNotAPoolHost) {
			t.Errorf("%s: terminate = %v", name, err)
		}
		if _, err := h.Console(t.Context(), "i-1"); !errors.Is(err, ErrNotAPoolHost) {
			t.Errorf("%s: console = %v", name, err)
		}
		if n := f.countOf("TerminateInstances") + f.countOf("GetConsoleOutput"); n != 0 {
			t.Errorf("%s: %d calls reached a non-pool instance", name, n)
		}
	}
	f := newFakeEC2(t)
	f.respond = func(string, url.Values) (int, string) {
		return http.StatusOK, describeReply("", jobAndPoolTaggedXML("i-1"))
	}
	if _, ok, err := newTestProvider(t, f, nil).Hosts().Find(t.Context(), "o-1"); !errors.Is(err, ErrNotAPoolHost) || ok {
		t.Fatalf("find over a job instance carrying the order = %t, %v", ok, err)
	}
}

// AN ANSWER THAT IS NOT A LIST OF INSTANCES IS NOT AN EMPTY ONE: another root, or an
// instance without an id, cannot be read as none.
func TestAMalformedDescribeIsNotAnEmptyList(t *testing.T) {
	t.Parallel()
	for name, reply := range map[string]string{
		"another root": `<unexpected/>`,
		// Every pool tag present, so only the missing id can be what refuses it.
		"no id": describeReply("", strings.Replace(poolHostXML("i-1", "dep-1", "o-1", "running"),
			"<instanceId>i-1</instanceId>", "", 1)),
	} {
		f := newFakeEC2(t)
		f.respond = func(string, url.Values) (int, string) { return http.StatusOK, reply }
		if _, ok, err := newTestProvider(t, f, nil).Hosts().Find(t.Context(), "o-1"); err == nil || ok {
			t.Errorf("%s: find = %t, %v", name, ok, err)
		}
	}
	f := newFakeEC2(t)
	f.respond = func(string, url.Values) (int, string) { return http.StatusOK, describeReply("") }
	if _, ok, err := newTestProvider(t, f, nil).Hosts().Find(t.Context(), "o-1"); err != nil || ok {
		t.Fatalf("a genuinely empty list: %t, %v", ok, err)
	}
}

// AN ANSWER THAT WILL NOT PARSE CAN QUOTE THE REQUEST, so a launch's failure keeps no
// text but its classification.
func TestALaunchAnswerThatWillNotParseQuotesNothing(t *testing.T) {
	t.Parallel()
	f := newFakeEC2(t)
	f.respond = func(action string, _ url.Values) (int, string) {
		if action == "RunInstances" {
			return http.StatusOK, `<RunInstancesResponse><token-9QX></RunInstancesResponse>`
		}

		return http.StatusOK, defaultReply(action)
	}
	_, err := newTestProvider(t, f, nil).Hosts().Launch(t.Context(), hostLaunch())
	if err == nil || strings.Contains(err.Error(), "token-9QX") || !errors.Is(err, errWithheld) {
		t.Fatalf("launch = %v", err)
	}
}

// A LAUNCH CARRIES THE JOIN TOKEN, so it renders as its order, shape and image only.
func TestAHostLaunchIsRedactedOnEveryRenderingPath(t *testing.T) {
	t.Parallel()
	l := hostLaunch()
	safe := "ec2.HostLaunch{order=o-1 type=m7gd.metal image=ami-0host user data redacted}"
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%q"} {
		if got := fmt.Sprintf(verb, l); got != safe {
			t.Errorf("%s = %q", verb, got)
		}
	}
	b, err := json.Marshal(l)
	if err != nil || string(b) != `"`+safe+`"` {
		t.Errorf("json = %s (%v)", b, err)
	}
	var text, asJSON bytes.Buffer
	slog.New(slog.NewTextHandler(&text, nil)).Info("x", "launch", l)
	slog.New(slog.NewJSONHandler(&asJSON, nil)).Info("x", "launch", l)
	for _, out := range []string{text.String(), asJSON.String()} {
		if !strings.Contains(out, "user data redacted") || strings.Contains(out, "token-9QX") {
			t.Errorf("slog = %s", out)
		}
	}
}
