package phoenixnap

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostsource"
)

const (
	clientID     = "client-1"
	clientSecret = "secret-ZK41"
	bootPayload  = "#cloud-config\nruncmd: [[billet, node, join, token-9QX]]\n"

	// The shapes below are the real API's, trimmed, as it answered on 2026-11-05.
	quotasWithRoom = `[{"id":"bmc.servers.max_count","name":"Servers","description":"Number of servers per account","status":"WITHIN_LIMIT","limit":1,"unit":"COUNT","used":0,"quotaEditLimitRequestDetails":[]},{"id":"bmc.public_ips.max_count","name":"Public IPs","status":"WITHIN_LIMIT","limit":15,"unit":"COUNT","used":0}]`
	quotasFull     = `[{"id":"bmc.servers.max_count","name":"Servers","description":"Number of servers per account","status":"LIMIT_REACHED","limit":1,"unit":"COUNT","used":1,"quotaEditLimitRequestDetails":[]}]`
	stock          = `[{"productCode":"s3.c3.medium","productCategory":"SERVER","locationAvailabilityDetails":[]},{"productCode":"s2.c2.small","productCategory":"SERVER","locationAvailabilityDetails":[{"location":"PHX","minQuantityRequested":1,"minQuantityAvailable":true,"availableQuantity":1,"solutions":[]},{"location":"CHI","minQuantityRequested":1,"minQuantityAvailable":true,"availableQuantity":9,"solutions":[]}]}]`
	refused        = `{"message":"The request failed since this resource cannot be accessed by the provided credentials."}`
)

// fakeBMC serves the token grant and the API from scripted answers, and records what
// it was asked.
type fakeBMC struct {
	t *testing.T

	mu      sync.Mutex
	grants  int
	ordered []map[string]any
	quotas  string
	order   func(w http.ResponseWriter)
	servers string
	status  int
	// redirected counts requests that reached the redirect's target.
	redirected int
	deleted    []string
}

func newFake(t *testing.T) (*fakeBMC, *httptest.Server) {
	t.Helper()
	f := &fakeBMC{t: t, quotas: quotasWithRoom, servers: `[]`}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)

	return f, srv
}

func (f *fakeBMC) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/token" {
		id, secret, ok := r.BasicAuth()
		if !ok || id != clientID || secret != clientSecret || r.FormValue("grant_type") != "client_credentials" {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}
		f.grants++
		f.write(w, `{"access_token":"tok-1","expires_in":300,"token_type":"Bearer","scope":"bmc.read bmc"}`)

		return
	}
	if r.Header.Get("Authorization") != "Bearer tok-1" {
		w.WriteHeader(http.StatusUnauthorized)

		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/bmc/v1/quotas":
		f.write(w, f.quotas)
	case r.Method == http.MethodGet && r.URL.Path == "/billing/v1/product-availability":
		f.write(w, stock)
	case r.Method == http.MethodPost && r.URL.Path == "/bmc/v1/servers":
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("order body: %v", err)
		}
		f.ordered = append(f.ordered, body)
		f.order(w)
	case r.Method == http.MethodGet && r.URL.Path == "/bmc/v1/servers":
		f.write(w, f.servers)
	case r.URL.Path == "/elsewhere":
		f.redirected++
	case strings.HasPrefix(r.URL.Path, "/bmc/v1/servers/"):
		id := strings.TrimPrefix(r.URL.Path, "/bmc/v1/servers/")
		if r.Method == http.MethodDelete {
			f.deleted = append(f.deleted, id)
			f.write(w, `{"result":"OK","serverId":"`+id+`"}`)

			return
		}
		w.WriteHeader(f.status)
		if f.status == http.StatusOK {
			f.write(w, `{"id":"`+id+`","status":"powered-on","hostname":"billet-o-1","type":"s2.c2.small","location":"CHI","publicIpAddresses":["203.0.113.7"]}`)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeBMC) snapshot() (grants int, ordered []map[string]any, deleted []string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.grants, append([]map[string]any(nil), f.ordered...), append([]string(nil), f.deleted...)
}

func (f *fakeBMC) set(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

// write answers a request; a failed write fails the test rather than vanishing.
func (f *fakeBMC) write(w io.Writer, body string) {
	if _, err := io.WriteString(w, body); err != nil {
		f.t.Errorf("write the answer: %v", err)
	}
}

func source(t *testing.T, srv *httptest.Server, opts ...Option) *Source {
	t.Helper()
	opts = append([]Option{WithEndpoints(srv.URL+"/token", srv.URL), WithHTTPClient(srv.Client())}, opts...)
	s, err := New(Credentials{ClientID: clientID, Secret: hostsource.NewSecret([]byte(clientSecret))},
		[]Shape{{Type: "s2.c2.small", Arch: config.ArchX64}}, opts...)
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	return s
}

// field walks a decoded JSON document by its keys, nil where any key is missing.
func field(doc map[string]any, keys ...string) any {
	var cur any = doc
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}

	return cur
}

func request() hostsource.Request {
	return hostsource.Request{Order: "o-1", Shape: "s2.c2.small", Location: "CHI", Boot: hostsource.NewSecret([]byte(bootPayload))}
}

func TestAnOrderIsAnHourlyServerThatBootsThePayload(t *testing.T) {
	t.Parallel()
	f, srv := newFake(t)
	f.order = func(w http.ResponseWriter) {
		f.write(w, `{"id":"srv-1","status":"creating","hostname":"billet-o-1","type":"s2.c2.small","location":"CHI"}`)
	}
	host, err := source(t, srv).Order(t.Context(), request())
	if err != nil {
		t.Fatalf("order: %v", err)
	}
	if host.ID != "srv-1" || host.Order != "o-1" || host.State != hostsource.StateProvisioning {
		t.Fatalf("host = %+v", host)
	}
	_, ordered, _ := f.snapshot()
	if len(ordered) != 1 {
		t.Fatalf("ordered %d servers", len(ordered))
	}
	// THE WIRE'S OWN KEYS, read without the production struct, so a renamed JSON tag
	// fails here rather than changing the encoder and this reader together.
	got := ordered[0]
	userData, ok := field(got, "osConfiguration", "cloudInit", "userData").(string)
	if !ok {
		t.Fatalf("the order has no osConfiguration.cloudInit.userData: %v", got)
	}
	boot, err := base64.StdEncoding.DecodeString(userData)
	if err != nil || string(boot) != bootPayload {
		t.Fatalf("the cloud-init payload is %q (%v), want the boot payload", boot, err)
	}
	for key, want := range map[string]any{
		"hostname": "billet-o-1", "type": "s2.c2.small", "location": "CHI",
		"pricingModel": "HOURLY", "os": "ubuntu/noble", "installDefaultSshKeys": false,
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
}

// A FULL QUOTA AND AN EMPTY SHELF ARE ANSWERS, read before ordering, and nothing is
// ordered past either.
func TestAFullQuotaOrNoStockOrdersNothing(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		quotas   string
		location string
		want     error
	}{
		"quota reached":         {quotasFull, "CHI", hostsource.ErrQuota},
		"none in that location": {quotasWithRoom, "SEA", hostsource.ErrNoStock},
	} {
		f, srv := newFake(t)
		f.quotas = c.quotas
		f.order = func(http.ResponseWriter) { t.Errorf("%s: an order was placed", name) }
		req := request()
		req.Location = c.location
		if _, err := source(t, srv).Order(t.Context(), req); !errors.Is(err, c.want) {
			t.Errorf("%s: order = %v, want %v", name, err, c.want)
		}
	}
}

// A QUOTA LIST THAT DOES NOT NAME THE SERVERS QUOTA cannot say whether another server
// fits: could-not-tell, which neither orders nor reads as a full quota.
func TestAQuotaListWithoutTheServersQuotaOrdersNothing(t *testing.T) {
	t.Parallel()
	f, srv := newFake(t)
	f.quotas = `[]`
	f.order = func(http.ResponseWriter) { t.Error("an order was placed") }
	_, err := source(t, srv).Order(t.Context(), request())
	if err == nil || errors.Is(err, hostsource.ErrQuota) || errors.Is(err, hostsource.ErrNoStock) {
		t.Fatalf("order = %v, want could-not-tell", err)
	}
}

// MEASURED 2026-11-05: the real API refused an order with this 403. It is an answer an
// operator acts on, not could-not-tell.
func TestTheMeasuredRefusalIsARefusal(t *testing.T) {
	t.Parallel()
	f, srv := newFake(t)
	f.order = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusForbidden)
		f.write(w, refused)
	}
	_, err := source(t, srv).Order(t.Context(), request())
	if !errors.Is(err, hostsource.ErrRefused) || !strings.Contains(err.Error(), "cannot be accessed by the provided credentials") {
		t.Fatalf("order = %v, want a refusal quoting the API", err)
	}
}

// NOT FOUND IS COULD-NOT-TELL, HOWEVER LONG IT LASTS: no delete has been measured, so
// nothing on this source proves a server gone, and a failed read is could-not-tell too.
func TestNotFoundNeverProvesAServerGone(t *testing.T) {
	t.Parallel()
	f, srv := newFake(t)
	s := source(t, srv)
	ctx := t.Context()
	f.set(http.StatusOK)
	if p, err := s.Gone(ctx, "srv-1"); err != nil || p != hostsource.PresencePresent {
		t.Fatalf("a running server: %v, %v", p, err)
	}
	f.set(http.StatusNotFound)
	for range 3 {
		if p, err := s.Gone(ctx, "srv-1"); err != nil || p != hostsource.PresenceUnknown {
			t.Fatalf("not found: %v, %v", p, err)
		}
	}
	f.set(http.StatusInternalServerError)
	if p, err := s.Gone(ctx, "srv-1"); err == nil || p != hostsource.PresenceUnknown {
		t.Fatalf("a failed read: %v, %v", p, err)
	}
}

func TestReleaseDeletesOnlyOnProof(t *testing.T) {
	t.Parallel()
	f, srv := newFake(t)
	s := source(t, srv)
	if err := s.Release(t.Context(), hostsource.ReleaseProof{}); !errors.Is(err, hostsource.ErrNoProof) {
		t.Fatalf("release without proof = %v", err)
	}
	if _, _, deleted := f.snapshot(); len(deleted) != 0 {
		t.Fatal("a server was deleted without proof")
	}
	proof, err := hostsource.NewDrainedProof(hostsource.Holdings{Host: "srv-1", Sealed: time.Unix(1, 0)})
	if err != nil {
		t.Fatalf("proof: %v", err)
	}
	if err := s.Release(t.Context(), proof); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, _, deleted := f.snapshot(); len(deleted) != 1 || deleted[0] != "srv-1" {
		t.Fatalf("deleted %v, want srv-1", deleted)
	}
}

// AN ORDER WHOSE ANSWER WAS LOST IS FOUND BY ITS HOSTNAME, so it is not placed twice;
// two servers claiming one order is an error, never a choice.
func TestFindReadsAnOrderBackByItsHostname(t *testing.T) {
	t.Parallel()
	f, srv := newFake(t)
	s := source(t, srv)
	ctx := t.Context()
	f.mu.Lock()
	f.servers = `[{"id":"srv-9","hostname":"someone-else","status":"powered-on"},{"id":"srv-1","hostname":"billet-o-1","status":"powered-on","type":"s2.c2.small","location":"CHI"}]`
	f.mu.Unlock()
	host, ok, err := s.Find(ctx, "o-1")
	if err != nil || !ok || host.ID != "srv-1" || host.State != hostsource.StateRunning {
		t.Fatalf("find = %+v, %t, %v", host, ok, err)
	}
	if _, ok, err := s.Find(ctx, "o-2"); err != nil || ok {
		t.Fatalf("an order nobody placed: %t, %v", ok, err)
	}
	f.mu.Lock()
	f.servers = `[{"id":"srv-1","hostname":"billet-o-1"},{"id":"srv-2","hostname":"billet-o-1"}]`
	f.mu.Unlock()
	if _, ok, err := s.Find(ctx, "o-1"); err == nil || ok {
		t.Fatalf("two servers for one order: %t, %v", ok, err)
	}
}

func TestTheTokenIsReusedUntilItExpires(t *testing.T) {
	t.Parallel()
	f, srv := newFake(t)
	now := time.Unix(1000, 0)
	var clock sync.Mutex
	s := source(t, srv, WithClock(func() time.Time {
		clock.Lock()
		defer clock.Unlock()

		return now
	}))
	ctx := t.Context()
	for range 3 {
		if _, _, err := s.Find(ctx, "o-1"); err != nil {
			t.Fatalf("find: %v", err)
		}
	}
	if grants, _, _ := f.snapshot(); grants != 1 {
		t.Fatalf("%d grants for three calls inside a token's life", grants)
	}
	clock.Lock()
	now = now.Add(5 * time.Minute)
	clock.Unlock()
	if _, _, err := s.Find(ctx, "o-1"); err != nil {
		t.Fatalf("find: %v", err)
	}
	if grants, _, _ := f.snapshot(); grants != 2 {
		t.Fatalf("%d grants after the token expired, want 2", grants)
	}
}

// THE SOURCE AND ITS CREDENTIALS REDACT THEMSELVES on every rendering path, each to
// exactly its safe text, so a rendering that panicked and was recovered by fmt does not
// pass for one that redacted.
func TestTheCredentialsAreRedactedOnEveryRenderingPath(t *testing.T) {
	t.Parallel()
	_, srv := newFake(t)
	s := source(t, srv)
	if _, _, err := s.Find(t.Context(), "o-1"); err != nil { // so a live token is held
		t.Fatalf("find: %v", err)
	}
	creds := Credentials{ClientID: clientID, Secret: hostsource.NewSecret([]byte(clientSecret))}
	for _, c := range []struct {
		v    any
		safe string
	}{
		{s, "phoenixnap.Source{client=client-1}"},
		{creds, "phoenixnap.Credentials{client=client-1}"},
	} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%q"} {
			if got := fmt.Sprintf(verb, c.v); got != c.safe {
				t.Errorf("%T %s = %q, want %q", c.v, verb, got, c.safe)
			}
		}
		b, err := json.Marshal(c.v)
		if err != nil || string(b) != `"`+c.safe+`"` {
			t.Errorf("%T json = %s (%v)", c.v, b, err)
		}
		var text, asJSON bytes.Buffer
		slog.New(slog.NewTextHandler(&text, nil)).Info("x", "v", c.v)
		slog.New(slog.NewJSONHandler(&asJSON, nil)).Info("x", "v", c.v)
		for name, out := range map[string]string{"slog text": text.String(), "slog json": asJSON.String()} {
			if !strings.Contains(out, c.safe) || strings.Contains(out, clientSecret) || strings.Contains(out, "tok-1") {
				t.Errorf("%T %s = %s", c.v, name, out)
			}
		}
	}
}

// AN ANSWER THAT SAYS NOTHING IS NOT AN ANSWER: an empty or null list does not prove an
// order was never placed, an order answered without an id did not prove one was, and a
// quota or stock entry missing its numbers cannot say whether another server fits.
func TestAnswersMissingWhatTheyMustSayAreCouldNotTell(t *testing.T) {
	t.Parallel()
	for _, body := range []string{``, `null`, ` `} {
		f, srv := newFake(t)
		f.mu.Lock()
		f.servers = body
		f.mu.Unlock()
		if _, ok, err := source(t, srv).Find(t.Context(), "o-1"); err == nil || ok {
			t.Errorf("a server list of %q: %t, %v", body, ok, err)
		}
	}
	f, srv := newFake(t)
	f.order = func(w http.ResponseWriter) { f.write(w, `{"status":"creating"}`) }
	if _, err := source(t, srv).Order(t.Context(), request()); err == nil {
		t.Error("an order answered without a server id succeeded")
	}
	for name, quotas := range map[string]string{
		"no used":  `[{"id":"bmc.servers.max_count","limit":1}]`,
		"no limit": `[{"id":"bmc.servers.max_count","used":0}]`,
	} {
		f, srv := newFake(t)
		f.quotas = quotas
		f.order = func(http.ResponseWriter) { t.Errorf("%s: an order was placed", name) }
		_, err := source(t, srv).Order(t.Context(), request())
		if err == nil || errors.Is(err, hostsource.ErrQuota) {
			t.Errorf("%s: %v, want could-not-tell", name, err)
		}
	}
}

// THE API'S OWN TEXT IS NOT QUOTED unless it is the measured refusal: a validation
// answer could echo the order's body, which carries the join token.
func TestARefusalNeverQuotesWhatTheAPISentBack(t *testing.T) {
	t.Parallel()
	encoded := base64.StdEncoding.EncodeToString([]byte(bootPayload))
	f, srv := newFake(t)
	f.order = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		f.write(w, `{"message":"invalid userData `+encoded+` token-9QX","validationErrors":["`+encoded+`"]}`)
	}
	_, err := source(t, srv).Order(t.Context(), request())
	if err == nil || strings.Contains(err.Error(), "token-9QX") || strings.Contains(err.Error(), encoded) {
		t.Fatalf("order = %v", err)
	}
}

// NO REDIRECT IS FOLLOWED: an order's body would be sent again to wherever it pointed.
func TestARedirectIsAnsweredNotFollowed(t *testing.T) {
	t.Parallel()
	f, srv := newFake(t)
	f.order = func(w http.ResponseWriter) {
		w.Header().Set("Location", srv.URL+"/elsewhere")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}
	if _, err := source(t, srv).Order(t.Context(), request()); err == nil {
		t.Fatal("a redirected order succeeded")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.redirected != 0 {
		t.Fatalf("the order was sent on %d times", f.redirected)
	}
}

// AN ORDER THIS SOURCE CANNOT HONOUR AS ASKED is refused before anything is called.
func TestAnOrderOutsideTheDeclaredShapesOrdersNothing(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*hostsource.Request){
		"undeclared shape":   func(r *hostsource.Request) { r.Shape = "a1.c5.large" },
		"wrong architecture": func(r *hostsource.Request) { r.Arch = config.ArchARM64 },
		"no location":        func(r *hostsource.Request) { r.Location = "" },
		"order not a name":   func(r *hostsource.Request) { r.Order = "O 1" },
	} {
		f, srv := newFake(t)
		f.order = func(http.ResponseWriter) { t.Errorf("%s: an order was placed", name) }
		req := request()
		mutate(&req)
		if _, err := source(t, srv).Order(t.Context(), req); err == nil {
			t.Errorf("%s: ordered", name)
		}
	}
}

// NO IDEMPOTENCY KEY, SO NOT IDEMPOTENT: an order whose answer was lost waits for a
// person rather than being placed again on an absent hostname.
func TestAnOrderHereIsNotIdempotent(t *testing.T) {
	t.Parallel()
	_, srv := newFake(t)
	terms, err := source(t, srv).Terms("s2.c2.small")
	if err != nil || terms.Idempotent || terms.BillingUnit != time.Hour {
		t.Fatalf("terms = %+v, %v", terms, err)
	}
}
