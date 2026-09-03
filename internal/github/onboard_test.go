package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeGitHub stands in for the API half of the flow. The browser half is driven
// by the OpenBrowser hook, which is what makes the whole handshake testable
// without a real organization.
type fakeGitHub struct {
	t *testing.T

	appID          int64
	installationID int64
	pem            string
	permissions    map[string]string

	// installed flips once the test "completes" the installation, so the poller
	// sees the same not-installed-then-installed transition an operator produces.
	installed atomic.Bool

	conversions atomic.Int32

	// rejectCode makes the conversion endpoint reject one code, and rejectStatus
	// says how. The default is 404, which is what GitHub returns for a code it
	// never issued. Set before serving.
	rejectCode   string
	rejectStatus int
	// rejectPrefix rejects every code with this prefix, for driving the
	// many-injected-codes case.
	rejectPrefix string

	// ambiguousOnce makes each NON-rejected code draw one ambiguous answer before
	// it redeems.
	//
	// This is the difference between a test that passes and a test that means
	// something. An honest code that succeeds on its first attempt never enters
	// the retry set, so it cannot be dropped BY the retry set — which is exactly
	// how a retention cap survived a test written to catch it.
	ambiguousOnce bool
	firstTryMu    sync.Mutex
	firstTry      map[string]bool

	// ambiguousFirst makes the conversion answer rejectStatus for the first N
	// attempts at ANY code, then behave normally. It exists to drive the
	// retry-the-same-code path, which is the only way to tell "billet kept the
	// code" apart from "billet skipped it".
	ambiguousFirst int32
	attempts       atomic.Int32
}

// shortenBackoff collapses the retry delays for the duration of one test.
func shortenBackoff(t *testing.T) {
	t.Helper()

	initial, maximum := initialExchangeBackoff, maxExchangeBackoff

	initialExchangeBackoff = time.Millisecond
	maxExchangeBackoff = 5 * time.Millisecond

	t.Cleanup(func() {
		initialExchangeBackoff, maxExchangeBackoff = initial, maximum
	})
}

// presentedCode is the code segment of a conversion request path.
func presentedCode(r *http.Request) string {
	return strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app-manifests/"), "/conversions")
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	encoded := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	return &fakeGitHub{
		t:              t,
		appID:          4242,
		installationID: 909090,
		pem:            string(encoded),
		permissions:    map[string]string{"metadata": "read", "organization_self_hosted_runners": "write"},
	}
}

func (g *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/app-manifests/", func(w http.ResponseWriter, r *http.Request) {
		g.conversions.Add(1)

		if !strings.HasSuffix(r.URL.Path, "/conversions") {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}

		if g.ambiguousOnce && !strings.HasPrefix(presentedCode(r), g.rejectPrefix) {
			g.firstTryMu.Lock()

			if g.firstTry == nil {
				g.firstTry = make(map[string]bool)
			}

			seenBefore := g.firstTry[presentedCode(r)]
			g.firstTry[presentedCode(r)] = true

			g.firstTryMu.Unlock()

			if !seenBefore {
				w.WriteHeader(http.StatusUnprocessableEntity)
				fmt.Fprint(w, `{"message":"try again"}`)

				return
			}
		}

		if g.attempts.Add(1) <= g.ambiguousFirst {
			w.WriteHeader(g.rejectStatus)
			fmt.Fprint(w, `{"message":"try again"}`)

			return
		}

		presented := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app-manifests/"), "/conversions")

		rejected := g.rejectCode != "" && presented == g.rejectCode
		if g.rejectPrefix != "" && strings.HasPrefix(presented, g.rejectPrefix) {
			rejected = true
		}

		if rejected {
			status := g.rejectStatus
			if status == 0 {
				status = http.StatusNotFound
			}

			w.WriteHeader(status)
			fmt.Fprint(w, `{"message":"Not Found"}`)

			return
		}

		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":%d,"slug":"billet-acme","name":"billet","html_url":"https://github.com/apps/billet-acme","pem":%q}`,
			g.appID, g.pem)
	})

	mux.HandleFunc("/orgs/", func(w http.ResponseWriter, _ *http.Request) {
		if !g.installed.Load() {
			// The ordinary "created but not installed yet" state.
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)

			return
		}

		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"id":%d,"account":{"login":"acme","type":"Organization"},"permissions":{`, g.installationID)

		first := true

		for k, v := range g.permissions {
			if !first {
				fmt.Fprint(w, ",")
			}

			first = false

			fmt.Fprintf(w, "%q:%q", k, v)
		}

		fmt.Fprint(w, "}}")
	})

	return mux
}

// browser plays the operator: it fetches the start page, extracts the manifest
// form's action, and drives the two callbacks the real GitHub would.
type browser struct {
	t      *testing.T
	fake   *fakeGitHub
	client *http.Client
	// visits records the URLs the flow asked the operator to open. Written and
	// read only on the goroutine that drives the flow.
	visits []string
	// origin is the loopback base the flow served, captured from the first visit
	// and never rewritten, so goroutines can read it without touching visits.
	origin string
	// skipSetupCallback simulates an operator who closes the tab, leaving the
	// authenticated poller as the only route to the installation.
	skipSetupCallback bool
	setupCallbacks    atomic.Int32
	// getFailures counts requests that failed for a reason OTHER than the connection.
	// A silently-failing request — a malformed URL, which fails while building or
	// resolving — is how the original version of this test passed without ever reaching
	// /installed.
	//
	// An unscoped counter would fire on the flow closing its listener as it finishes,
	// which refuses an in-flight request correctly. Gating on "has the flow returned"
	// instead discards a genuinely malformed URL that fails late.
	getFailures atomic.Int32

	// reached counts requests that got a RESPONSE, whatever became of the body.
	//
	// The attempt counter cannot tell "delivered" from "refused", and a refusal
	// against the flow's own origin is deliberately excused — so without this a
	// regression that closed the listener before /installed left an attempt
	// recorded, a refusal forgiven, and every other assertion passing while the
	// callback was never delivered. A truncated body still counts: the request
	// reached the handler, which is the fact being asserted.
	reached atomic.Int32
	// pending joins the fire-and-forget callback goroutines, so the assertions
	// cannot read the counter while a request is still deciding its fate.
	pending  sync.WaitGroup
	joinOnce sync.Once
}

// track registers a goroutine that must finish before its test does.
//
// The cleanup is registered HERE rather than at each of the eleven places a
// browser is built, because that is the version that cannot be forgotten — and
// forgetting it is not hypothetical: only one test called Wait explicitly, so
// every other one could return while a request was still in flight and have its
// t.Errorf fire against a finished test.
func (b *browser) track() {
	b.joinOnce.Do(func() { b.t.Cleanup(b.pending.Wait) })

	b.pending.Add(1)
}

func (b *browser) open(ctx context.Context, target string) error {
	b.visits = append(b.visits, target)

	// CAPTURED ONCE, on this goroutine, before any of the goroutines below exist.
	//
	// b.visits is appended here and was read from the callback and registration
	// goroutines — a concurrent read of a slice header while it is being
	// reallocated, which is a data race whatever the values happen to be. It had
	// not fired under -race, which is timing rather than proof: the second append
	// races only with a request already in flight from the first.
	if b.origin == "" {
		b.origin = target
	}

	// The first URL is the loopback start page; the second is GitHub's install
	// page, which the test answers by "completing" the install.
	if strings.Contains(target, "/installations/new") {
		if b.skipSetupCallback {
			// Exercise the poller alone: an operator who closed the tab, or
			// finished the install on another machine.
			b.fake.installed.Store(true)

			return nil
		}

		// Built by trimming the trailing slash and appending, NOT by replacing the
		// first "/" — that replaces the one in "http://" and yields a hostless
		// URL whose request silently fails, letting the poller carry the test.
		installedURL := strings.TrimSuffix(b.origin, "/") + "/installed" +
			"?installation_id=" + fmt.Sprint(b.fake.installationID) + "&setup_action=install"

		// Tracked so the assertions can WAIT for it. Fire-and-forget left the
		// counter being read while this request was still deciding whether it had
		// failed, which is a race no atomic fixes: the value is correct and simply
		// not there yet.
		b.track()

		// COUNTED ON ATTEMPT, not on delivery. Counting deliveries is the stronger
		// assertion and it cannot be made honestly here: the poller may legitimately
		// finish the flow first, close the listener, and leave the callback refused
		// for a correct reason — measured at 3 of 12 runs on a loaded machine, with
		// getFailures rightly staying zero. What makes the attempt count MEAN
		// something is the store below.
		b.setupCallbacks.Add(1)

		go func() {
			defer b.pending.Done()

			// THE INSTALLATION BECOMES VISIBLE ONLY BECAUSE THIS RUNS. Set when the install
			// page opens instead, the poller completes the whole flow whether or not a callback
			// is ever issued — so deleting the callback leaves every assertion green.
			//
			// AFTER the request, which is the only ordering that makes the callback necessary:
			// storing first lets the flow's immediate installation check succeed on its own.
			// Storing after costs nothing — the flow gets a 404 and keeps polling.
			page, ok := b.get(ctx, installedURL)

			// THE BODY IS CHECKED BECAUSE THE STATUS CANNOT BE. The loopback mux registers
			// root+"/" as a catch-all, so deleting the /installed route does not produce a 404
			// — the request falls through to the start page and answers 200.
			//
			// Skipped when the request did not arrive: a callback refused because the poller
			// already finished is legitimate, and get has accounted for it.
			if ok && !strings.Contains(page, "Installed") {
				b.getFailures.Add(1)

				b.t.Errorf("the setup callback reached something other than the installed "+
					"page, so the route it targets is gone or has moved: %.120s", page)
			}

			b.fake.installed.Store(true)
		}()

		return nil
	}

	// TRACKED TOO, and finding this took looking rather than assuming. Joining
	// only the /installed callback left this one — which issues its own requests
	// through the same counter — free to increment after the assertions had
	// already read it. A missed increment here is the same blind spot the join
	// was added to remove, one goroutine over.
	b.track()

	go func() {
		defer b.pending.Done()

		b.driveRegistration(ctx, target)
	}()

	return nil
}

// driveRegistration fetches the self-submitting form, VALIDATES the manifest it
// carries against GitHub's documented schema, then plays GitHub's redirect back
// to /callback with a code and the state it was given.
//
// Validating here is the point. The earlier version only scraped the state and
// jumped straight to the callback, so the manifest was never inspected by
// anything — which is how a manifest missing the required hook_attributes.url
// passed a green test suite and would have failed on first contact with GitHub.
func (b *browser) driveRegistration(ctx context.Context, startURL string) {
	body, _ := b.get(ctx, startURL)

	action := extractFormAction(b.t, body)
	if action == "" {
		b.t.Errorf("start page carried no form action:\n%s", body)
		return
	}

	b.validateManifest(extractManifest(b.t, body), strings.TrimSuffix(startURL, "/"))

	parsed, err := url.Parse(action)
	if err != nil {
		b.t.Errorf("parse form action: %v", err)
		return
	}

	state := parsed.Query().Get("state")
	if state == "" {
		b.t.Error("registration URL carried no state")
		return
	}

	base := strings.TrimSuffix(startURL, "/")
	_, _ = b.get(ctx, base+"/callback?code=testcode&state="+url.QueryEscape(state))
}

// validateManifest enforces billet's manifest invariants.
//
// Only `url` and `hook_attributes.url` are documented by GitHub as required.
// Everything else is billet's own: the callback URLs because onboarding depends on
// them, and the whole hook_attributes object because GitHub documents `active` as
// defaulting to TRUE, so omitting it leaves the webhook enabled.
//
// base is passed in so the callback URLs are asserted against what billet ACTUALLY
// serialized — tagging either field `json:"-"` otherwise leaves the suite green
// while GitHub has nowhere to redirect to.
func (b *browser) validateManifest(raw, base string) {
	b.t.Helper()

	if raw == "" {
		b.t.Error("the registration form carried no manifest")
		return
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		b.t.Errorf("manifest is not valid JSON: %v\n%s", err, raw)
		return
	}

	if s, ok := m["url"].(string); !ok || s == "" {
		b.t.Errorf("manifest.url is required by GitHub and is missing or not a string: %v", m["url"])
	}

	// Asserted against what billet serialized, not reconstructed from the same
	// base this test drives. Without these, tagging either field `json:"-"`
	// leaves the suite green while GitHub has nowhere to send the operator:
	// no redirect_url stalls Onboard until the manifest's one-hour deadline,
	// and no setup_url silently drops the fast installation path.
	for field, want := range map[string]string{
		"redirect_url": base + "/callback",
		"setup_url":    base + "/installed",
	} {
		if got, ok := m[field].(string); !ok || got != want {
			b.t.Errorf("manifest.%s = %v, want %q", field, m[field], want)
		}
	}

	// Registered for one organization's runners; installable by strangers is not
	// a thing billet should offer. `public` has no omitempty, so it must appear.
	if public, ok := m["public"].(bool); !ok || public {
		b.t.Errorf("manifest.public must be present and false, got %v", m["public"])
	}

	// Subscribing to events would contradict the no-webhook design.
	if events, present := m["default_events"]; present {
		if list, ok := events.([]any); !ok || len(list) > 0 {
			b.t.Errorf("manifest.default_events must be absent or empty, got %v", events)
		}
	}

	// request_oauth_on_install asks the installer to authorize the app as a
	// user, which billet has no OAuth flow for — and GitHub documents that
	// enabling it makes setup_url unavailable, which would quietly disable the
	// installation callback this same validator pins above. callback_urls
	// belongs to that flow too and must stay absent for the same reason.
	if v, present := m["request_oauth_on_install"]; present {
		if on, ok := v.(bool); !ok || on {
			b.t.Errorf("manifest.request_oauth_on_install must be absent or false, got %v", v)
		}
	}

	if urls, present := m["callback_urls"]; present {
		if list, ok := urls.([]any); !ok || len(list) > 0 {
			b.t.Errorf("manifest.callback_urls must be absent or empty; billet has no user OAuth flow, got %v", urls)
		}
	}

	// PRESENCE of the whole object is asserted, not just its contents. GitHub
	// documents hook_attributes.active as defaulting to TRUE, so a manifest that
	// omits the object entirely registers an ACTIVE webhook — and the previous
	// `if hook, ok := ...; ok` simply skipped the block, accepting exactly that.
	// billet's claim to need no inbound ingress rests on this.
	hook, ok := m["hook_attributes"].(map[string]any)
	if !ok {
		// Errorf and return, never Fatalf: this runs on the goroutine started by
		// driveRegistration, where FailNow would stop only that goroutine and let
		// the test go on to report some later, unrelated failure instead.
		b.t.Errorf("manifest.hook_attributes must be present and an object, got %v", m["hook_attributes"])

		return
	}

	// GitHub marks hook_attributes.url required whenever the object is present,
	// so an inactive hook still needs a URL.
	if s, ok := hook["url"].(string); !ok || s == "" {
		b.t.Error("manifest.hook_attributes is present but carries no url; GitHub rejects that")
	}

	// PRESENCE is asserted, not merely the value. `active, _ := ...(bool)` yields
	// false when the key is absent or the wrong type — which is the expected
	// value — so it passed without ever proving the manifest disables the
	// webhook.
	active, ok := hook["active"].(bool)

	switch {
	case !ok:
		b.t.Errorf("manifest.hook_attributes.active must be present and boolean, got %v", hook["active"])
	case active:
		b.t.Error("the webhook must be inactive: billet needs no inbound ingress")
	}

	perms, ok := m["default_permissions"].(map[string]any)
	if !ok {
		// Errorf, not Fatalf — see the hook_attributes note above: this runs on
		// driveRegistration's goroutine.
		b.t.Errorf("manifest carried no default_permissions: %s", raw)

		return
	}

	if len(perms) != len(permissions) {
		b.t.Errorf("manifest requests %d permissions, want %d: %v", len(perms), len(permissions), perms)
	}

	for name, want := range permissions {
		if got, ok := perms[name].(string); !ok || got != want {
			b.t.Errorf("manifest permission %q = %v, want %q", name, perms[name], want)
		}
	}

	// setup_on_update would point a future repository-access change at a loopback
	// port that stopped existing when onboarding finished. Absence is the
	// intended encoding; a present value must still be an explicit false rather
	// than any non-boolean that happens to read as one.
	if v, present := m["setup_on_update"]; present {
		off, ok := v.(bool)
		if !ok || off {
			b.t.Errorf("setup_on_update must be absent or explicitly false, got %v", v)
		}
	}
}

// get fetches a URL, reporting the body and whether it was DELIVERED — reached
// the intended server and came back 2xx.
func (b *browser) get(ctx context.Context, target string) (string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		b.t.Errorf("build request: %v", err)

		return "", false
	}

	resp, err := b.client.Do(req)

	// SCOPED TO THE CALLBACK, which is the request whose delivery cannot be
	// inferred from anything else. The browser makes several requests in a flow;
	// counting them all would assert a total that changes whenever the flow does.
	if err == nil && strings.Contains(target, "/installed") {
		b.reached.Add(1)
	}

	if err != nil {
		// COUNTED BY WHAT WENT WRONG, not by when. The defect this guards is a MALFORMED
		// URL, which fails while building the request and never reaches a socket;
		// swallowing it is how the original test reported success without reaching
		// /installed. A connection error is different in kind — the flow closes its listener
		// as it finishes, so a request in flight then is refused for a correct reason.
		if !b.benignFailure(target, err) {
			b.getFailures.Add(1)
		}

		return "", false
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// CLASSIFIED LIKE THE REQUEST ERROR, and previously not. The handler
		// signals the flow BEFORE writing its response, so the server can be
		// closed while the body is still going out — which surfaces here as
		// io.ErrUnexpectedEOF and was reported unconditionally, reopening the
		// same flake through a second door.
		if !b.benignFailure(target, err) {
			b.t.Errorf("read %s: %v", target, err)
		}

		return "", false
	}

	// A ROUTE THAT NO LONGER EXISTS ANSWERS 404, and the status was never
	// examined — so deleting the production /installed route left every
	// assertion green while the poller quietly carried the flow.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b.getFailures.Add(1)

		return string(body), false
	}

	return string(body), true
}

// benignFailure reports whether a request failure is the expected consequence of
// the flow shutting down, rather than billet aiming somewhere wrong.
//
// THE DESTINATION IS CHECKED, not just the error type. Treating every
// net.OpError as benign hid an entire class of regression: a syntactically valid
// callback URL pointing at the wrong host or port fails through the same
// net.OpError (a DNS or dial error), so a flow that started addressing the wrong
// place would have looked exactly like a listener closing on time. Only failures
// against the flow's OWN loopback origin can be excused.
func (b *browser) benignFailure(target string, err error) bool {
	if b.origin == "" {
		return false
	}

	want, wantErr := url.Parse(b.origin)
	got, gotErr := url.Parse(target)

	if wantErr != nil || gotErr != nil || want.Scheme != got.Scheme || want.Host != got.Host {
		return false
	}

	//nolint:errcheck // the discarded value is the typed error itself, not a failure; the bool is the answer. errcheck cannot exclude a generic function.
	_, ok := errors.AsType[*net.OpError](err)

	return ok ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, http.ErrServerClosed) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// THE CLASSIFIER IS THE GUARD, so it gets its own test.
//
// The end-to-end test above counts only failures benignFailure declines to
// excuse, which makes this function the entire difference between catching a
// regression and reporting a clean run. And it is the kind of function that
// looks obviously right and can be quietly wrong in the direction that hides
// bugs: excusing too much costs nothing visible, ever.
//
// The regression it exists to catch is a syntactically valid callback URL
// aimed at the wrong host or port. That fails with the same net.OpError as a
// listener closing on time, so "is it an OpError" — the obvious implementation —
// would excuse it.
func TestOnlyFailuresAgainstOurOwnOriginAreExcused(t *testing.T) {
	t.Parallel()

	b := &browser{origin: "http://127.0.0.1:41234"}

	refused := &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}

	if !b.benignFailure("http://127.0.0.1:41234/installed", refused) {
		t.Error("a connection refused by our own listener was counted as a failure; the flow " +
			"closes that listener as it finishes, so a request still in flight then is " +
			"refused for a correct reason and the test flakes")
	}

	for _, wrong := range []string{
		"http://127.0.0.1:41235/installed", // right host, wrong port
		"http://10.0.0.4:41234/installed",  // wrong host, right port
		"https://127.0.0.1:41234/installed",
	} {
		if b.benignFailure(wrong, refused) {
			t.Errorf("a request to %s was excused; it fails with the same error as a closing "+
				"listener, so excusing it means a flow aimed at the wrong place looks "+
				"identical to one that worked", wrong)
		}
	}

	// An error that has nothing to do with shutting down is never benign, even
	// against the right origin.
	if b.benignFailure("http://127.0.0.1:41234/installed", errors.New("no such host")) {
		t.Error("an unrelated error against our own origin was excused")
	}

	// NO ORIGIN MEANS NOTHING IS EXCUSED. Before the flow has published one there
	// is no address to compare against, and excusing by default would blind the
	// counter for the whole first half of the flow.
	empty := &browser{}
	if empty.benignFailure("http://127.0.0.1:41234/installed", refused) {
		t.Error("a failure was excused before the flow had an origin to compare against")
	}
}

func extractFormAction(t *testing.T, page string) string {
	t.Helper()
	return unescapeHTML(extractAttr(page, `action="`))
}

// extractManifest pulls the manifest out of the hidden form field, undoing the
// HTML escaping html/template applied on the way in.
func extractManifest(t *testing.T, page string) string {
	t.Helper()
	return unescapeHTML(extractAttr(page, `name="manifest" value="`))
}

func extractAttr(page, marker string) string {
	i := strings.Index(page, marker)
	if i < 0 {
		return ""
	}

	rest := page[i+len(marker):]

	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}

	return rest[:j]
}

func unescapeHTML(s string) string {
	r := strings.NewReplacer(
		"&amp;", "&",
		"&#34;", `"`,
		"&quot;", `"`,
		"&#39;", "'",
		"&lt;", "<",
		"&gt;", ">",
	)

	return r.Replace(s)
}

// The whole handshake, end to end: manifest form, code exchange, install, and
// the installation id coming back. This is the path that gates every later
// phase's testing loop, so it is worth exercising rather than trusting.
func TestOnboardEndToEnd(t *testing.T) {
	fake := newFakeGitHub(t)

	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	b := &browser{t: t, fake: fake, client: srv.Client()}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	var (
		persisted    []byte
		persistedAt  int
		installsSeen int
	)

	result, err := Onboard(ctx, OnboardOptions{
		Org:         "acme",
		Name:        "billet",
		OpenBrowser: b.open,
		Log:         func(string, ...any) {},
		Client:      srv.Client(),
		InstallPoll: 20 * time.Millisecond,
		apiBase:     srv.URL,
		OnAppCreated: func(app *App) error {
			persisted = []byte(app.PEM)
			// Record how many install pages had been opened when the key landed.
			// It must be zero: the key has to be durable BEFORE the installation
			// phase, since every failure there would otherwise orphan the app.
			persistedAt = installsSeen

			return nil
		},
	})
	// Joined before anything reads getFailures, so a callback still in flight has
	// finished deciding whether it failed.
	b.pending.Wait()

	if err != nil {
		t.Fatalf("Onboard: %v", err)
	}

	installsSeen = len(b.visits)

	if len(persisted) == 0 {
		t.Fatal("OnAppCreated never received the private key")
	}

	if persistedAt != 0 {
		t.Errorf("key was persisted after %d install prompts; it must be saved before installation", persistedAt)
	}

	if result.App.ID != fake.appID {
		t.Errorf("app id = %d, want %d", result.App.ID, fake.appID)
	}

	if result.Installation.ID != fake.installationID {
		t.Errorf("installation id = %d, want %d", result.Installation.ID, fake.installationID)
	}

	// The code is single-use; exchanging it twice would mean the flow retried
	// something it must not.
	if n := fake.conversions.Load(); n != 1 {
		t.Errorf("manifest conversions = %d, want exactly 1", n)
	}

	// The setup callback must have actually been reached. Asserting this is what
	// stops the fast path silently regressing into "the poller carried it".
	if n := b.setupCallbacks.Load(); n != 1 {
		t.Errorf("setup callback fired %d times, want 1", n)
	}

	// THE CALLBACK MUST HAVE BEEN ANSWERED, not merely attempted. The counter
	// above is incremented before the request is made, and a connection refused
	// against the flow's own origin is excused as an orderly shutdown — so a
	// regression that closed the listener BEFORE /installed produced exactly this
	// shape: an attempt recorded, a refusal forgiven, and every remaining
	// assertion passing while the callback was never delivered.
	if n := b.reached.Load(); n != 1 {
		t.Errorf("the setup callback got a response %d times, want 1: it was attempted but "+
			"never reached the flow's listener, and a refusal against our own origin is "+
			"excused as an orderly shutdown — so the callback can go undelivered with every "+
			"other assertion still passing", n)
	}

	if n := b.getFailures.Load(); n != 0 {
		// SAYS WHAT HAPPENED, not what probably caused it. The old wording blamed
		// a malformed URL, which is one of three things this counts — the others
		// are a request to the wrong host or port, and a route that answered a
		// non-2xx status — and naming the wrong one sends the reader looking in
		// the wrong place first.
		t.Errorf("%d browser requests failed for a reason other than the flow shutting down: "+
			"billet built a URL that could not be requested, aimed one somewhere other than "+
			"its own callback origin, or a route answered a non-2xx status", n)
	}

	if len(b.visits) != 2 {
		t.Fatalf("operator was asked to visit %d URLs, want 2 (create, then install): %v",
			len(b.visits), b.visits)
	}

	if !strings.Contains(b.visits[1], "/installations/new") {
		t.Errorf("second visit should be the install page, got %s", b.visits[1])
	}
}

// The poller alone must complete onboarding: an operator who closes the tab, or
// finishes the install on a different machine, still needs a working outcome.
func TestOnboardCompletesWithoutTheSetupCallback(t *testing.T) {
	fake := newFakeGitHub(t)

	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	b := &browser{t: t, fake: fake, client: srv.Client(), skipSetupCallback: true}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	result, err := Onboard(ctx, OnboardOptions{
		Org:          "acme",
		OpenBrowser:  b.open,
		Log:          func(string, ...any) {},
		Client:       srv.Client(),
		InstallPoll:  20 * time.Millisecond,
		apiBase:      srv.URL,
		OnAppCreated: func(*App) error { return nil },
	})
	if err != nil {
		t.Fatalf("Onboard: %v", err)
	}

	if result.Installation.ID != fake.installationID {
		t.Errorf("installation id = %d, want %d", result.Installation.ID, fake.installationID)
	}

	if n := b.setupCallbacks.Load(); n != 0 {
		t.Errorf("setup callback fired %d times; this test must exercise the poller alone", n)
	}
}

// A spoofed installation id must never reach the result. GitHub documents the
// setup-URL id as untrustworthy, and it ends up in billet.yaml deciding which
// installation runners register against.
func TestOnboardIgnoresSpoofedInstallationID(t *testing.T) {
	fake := newFakeGitHub(t)

	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	b := &browser{t: t, fake: fake, client: srv.Client()}
	// The "attacker" claims a different id than the API will report.
	b.fake.installationID = 4242

	spoof := func(ctx context.Context, target string) error {
		if strings.Contains(target, "/installations/new") {
			fake.installed.Store(true)

			// Tracked like the others, so this goroutine cannot outlive its test
			// and call t.Errorf after it has finished.
			b.track()

			go func() {
				defer b.pending.Done()

				_, _ = b.get(ctx, strings.TrimSuffix(b.origin, "/")+
					"/installed?installation_id=999999999")
			}()

			return nil
		}

		return b.open(ctx, target)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	result, err := Onboard(ctx, OnboardOptions{
		Org:          "acme",
		OpenBrowser:  spoof,
		Log:          func(string, ...any) {},
		Client:       srv.Client(),
		InstallPoll:  20 * time.Millisecond,
		apiBase:      srv.URL,
		OnAppCreated: func(*App) error { return nil },
	})
	if err != nil {
		t.Fatalf("Onboard: %v", err)
	}

	if result.Installation.ID == 999999999 {
		t.Fatal("the spoofed installation id from the setup callback was trusted")
	}

	if result.Installation.ID != fake.installationID {
		t.Errorf("installation id = %d, want the API's %d", result.Installation.ID, fake.installationID)
	}
}

// A permission the operator added between creating and installing the app must
// fail onboarding, not warn: billet would otherwise hold access it publicly
// claims not to have.
func TestOnboardFailsOnUnexpectedPermission(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.permissions["contents"] = "read"

	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	b := &browser{t: t, fake: fake, client: srv.Client()}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	saved := false

	_, err := Onboard(ctx, OnboardOptions{
		Org:          "acme",
		OpenBrowser:  b.open,
		Log:          func(string, ...any) {},
		Client:       srv.Client(),
		InstallPoll:  20 * time.Millisecond,
		apiBase:      srv.URL,
		OnAppCreated: func(*App) error { saved = true; return nil },
	})
	if err == nil {
		t.Fatal("onboarding succeeded despite an unrequested `contents` permission")
	}

	if !strings.Contains(err.Error(), "permission mismatch") {
		t.Errorf("expected a permission-mismatch error, got: %v", err)
	}

	// The recovery URL has to WORK. Nothing pinned it, and it had been built by
	// appending "/installations/<id>" to a base already ending in
	// /installations — handing a 404 to an operator who is by definition
	// already stuck.
	want := "https://github.com/organizations/acme/settings/installations/909090"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("recovery URL is wrong.\n got: %v\nwant it to contain: %s", err, want)
	}

	// The key must still have been saved, or the failure is unrecoverable.
	if !saved {
		t.Error("the app key was not persisted before the failure; the app is now orphaned")
	}
}

// If credentials cannot be persisted, the flow must stop rather than proceed to
// installation and leave an app whose key was discarded.
func TestOnboardAbortsWhenCredentialsCannotBeSaved(t *testing.T) {
	fake := newFakeGitHub(t)

	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	b := &browser{t: t, fake: fake, client: srv.Client()}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	_, err := Onboard(ctx, OnboardOptions{
		Org:          "acme",
		OpenBrowser:  b.open,
		Log:          func(string, ...any) {},
		Client:       srv.Client(),
		InstallPoll:  20 * time.Millisecond,
		apiBase:      srv.URL,
		OnAppCreated: func(*App) error { return errors.New("disk full") },
	})
	if err == nil {
		t.Fatal("onboarding continued despite failing to save credentials")
	}

	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("the underlying cause should be reported, got: %v", err)
	}

	// The operator must be told the app exists and needs deleting.
	if !strings.Contains(err.Error(), "delete it") {
		t.Errorf("the error should say the orphaned app must be deleted, got: %v", err)
	}

	// It must not have gone on to prompt for installation.
	if len(b.visits) != 1 {
		t.Errorf("flow prompted %d times; it should stop after the failed save", len(b.visits))
	}
}

// A callback carrying the wrong state must be refused: it is the CSRF guard on
// the one endpoint an attacker could otherwise drive.
func TestOnboardRejectsStateMismatch(t *testing.T) {
	fake := newFakeGitHub(t)

	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// A hostile local process races the browser with a wrong state, then the
	// real registration proceeds. It also probes the guessable paths first —
	// the start page carries the state in its form, so serving it from "/" let
	// a local process read the state and then present a VALID-looking callback
	// with a bogus code, which refusing invalid callbacks does not stop. The forged request must be refused WITHOUT
	// ending the flow: this listens on loopback, so any unprivileged process can
	// reach it, and killing onboarding after GitHub has created the App but
	// before billet exchanges the one-time code orphans the App and its private
	// key. A local denial of service would become credential loss.
	browser := &browser{t: t, fake: fake, client: srv.Client()}

	var forged atomic.Int32

	attacked := func(ctx context.Context, target string) error {
		// Only the FIRST call is the loopback start page; the second is
		// GitHub's install URL, where /callback does not exist.
		if forged.Add(1) == 1 {
			// The guessable paths must not exist. Derived from the real target's
			// origin, which is what a local process can trivially discover by
			// scanning loopback ports.
			origin, err := url.Parse(target)
			if err != nil {
				return err
			}

			for _, guess := range []string{"/", "/callback", "/installed"} {
				probe, err := http.NewRequestWithContext(ctx, http.MethodGet,
					"http://"+origin.Host+guess, http.NoBody)
				if err != nil {
					return err
				}

				resp, err := srv.Client().Do(probe)
				if err != nil {
					return err
				}

				body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
				resp.Body.Close()

				if err != nil {
					return err
				}

				if resp.StatusCode != http.StatusNotFound {
					t.Errorf("%s answered %d; every route must sit under the unguessable prefix",
						guess, resp.StatusCode)
				}

				// Belt and braces: even a non-404 must not hand over the state.
				if strings.Contains(string(body), "state=") {
					t.Errorf("%s leaked the registration state to an unauthenticated caller", guess)
				}
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodGet,
				strings.TrimSuffix(target, "/")+"/callback?code=evil&state=wrong", http.NoBody)
			if err != nil {
				return err
			}

			resp, err := srv.Client().Do(req)
			if err != nil {
				return err
			}

			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("forged state answered %d, want 400", resp.StatusCode)
			}
		}

		// The legitimate browser runs only after the forgery was refused.
		return browser.open(ctx, target)
	}

	app, err := Onboard(ctx, OnboardOptions{
		Org:          "acme",
		OpenBrowser:  attacked,
		Log:          func(string, ...any) {},
		Client:       srv.Client(),
		InstallPoll:  20 * time.Millisecond,
		apiBase:      srv.URL,
		OnAppCreated: func(*App) error { return nil },
	})

	if err != nil {
		t.Fatalf("a forged callback ended the flow: %v", err)
	}

	if app == nil || app.App == nil || app.App.ID == 0 {
		t.Fatal("onboarding produced no app despite the legitimate callback succeeding")
	}

	// Exactly one exchange: the forged code must never have been redeemed.
	if n := fake.conversions.Load(); n != 1 {
		t.Errorf("conversions = %d, want exactly 1 (the legitimate one)", n)
	}
}

// A callback whose STATE is correct but whose code is worthless must not end
// the flow either.
//
// The unguessable path stops a process that has to guess it — but billet hands
// that path to `open`/`xdg-open` as a command-line argument, and argv is
// readable by other processes on the machine. So the prefix and the state must
// both be assumed known, and the remaining question is what a caller can do
// with them. Injecting a bogus code was a kill switch: it was accepted, redeemed
// once, and its failure ended onboarding — orphaning an App whose one-time key
// GitHub had already issued and will not issue again.
//
// A code that does not redeem is now treated the same way a bad state is: it is
// discarded, and the flow keeps waiting for the redirect that does redeem.
func TestOnboardSurvivesAnInjectedCode(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.rejectCode = "injected-by-a-local-process"

	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	browser := &browser{t: t, fake: fake, client: srv.Client()}

	var calls atomic.Int32

	attacked := func(ctx context.Context, target string) error {
		// Only the first call is the loopback start page.
		if calls.Add(1) == 1 {
			// The attacker knows the secret path — it read the browser command
			// line — so it can also read the state out of the start page, exactly
			// as the legitimate browser does.
			page, _ := browser.get(ctx, target)
			state := extractAttr(page, "state=")
			if state == "" {
				t.Error("could not read the state from the start page")
			}

			forged := strings.TrimSuffix(target, "/") + "/callback?code=" + fake.rejectCode + "&state=" + state

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, forged, http.NoBody)
			if err != nil {
				return err
			}

			resp, err := srv.Client().Do(req)
			if err != nil {
				return err
			}

			resp.Body.Close()
		}

		return browser.open(ctx, target)
	}

	result, err := Onboard(ctx, OnboardOptions{
		Org:          "acme",
		OpenBrowser:  attacked,
		Log:          func(string, ...any) {},
		Client:       srv.Client(),
		InstallPoll:  20 * time.Millisecond,
		apiBase:      srv.URL,
		OnAppCreated: func(*App) error { return nil },
	})

	if err != nil {
		t.Fatalf("an injected code ended the flow and orphaned the App: %v", err)
	}

	if result == nil || result.App == nil || result.App.ID == 0 {
		t.Fatal("onboarding produced no app despite the legitimate redirect succeeding")
	}

	// Two exchanges: the rejected injection, then the real one. Fewer would mean
	// the injection never landed and this test proves nothing.
	if n := fake.conversions.Load(); n != 2 {
		t.Errorf("conversions = %d, want 2 (the rejected injection, then the real code)", n)
	}
}

// A registration callback billet cannot queue is REFUSED, never acknowledged.
//
// The queue silently discarded a code it had no room for and served the "App
// created" page anyway. GitHub's honest redirect could be the discarded one —
// its browser was told onboarding had worked while the code was thrown away,
// leaving the CLI waiting for a redirect that had already been and gone.
func TestFullCallbackQueueRefusesRatherThanDropping(t *testing.T) {
	flow := &onboardFlow{
		state:    "the-state",
		prefix:   "prefix",
		codeCh:   make(chan string, codeQueueDepth),
		installC: make(chan struct{}, 1),
		errCh:    make(chan error, 1),
	}

	srv := httptest.NewServer(flow.routes())
	defer srv.Close()

	callback := srv.URL + "/prefix/callback?state=" + flow.state + "&code="

	get := func(t *testing.T, code string) int {
		t.Helper()

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, callback+code, http.NoBody)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}

		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("callback: %v", err)
		}

		defer resp.Body.Close()

		return resp.StatusCode
	}

	// Fill it exactly, the way a local process racing the browser would.
	for i := range codeQueueDepth {
		if status := get(t, fmt.Sprintf("injected-%d", i)); status != http.StatusOK {
			t.Fatalf("callback %d answered %d, want 200 while the queue has room", i, status)
		}
	}

	if status := get(t, "the-honest-one"); status == http.StatusOK {
		t.Error("a callback that could not be queued was told the App was created")
	}

	// Every accepted code must still be there — refusing the overflow must not
	// have cost one that was already taken.
	if got := len(flow.codeCh); got != codeQueueDepth {
		t.Errorf("queue holds %d codes, want %d", got, codeQueueDepth)
	}
}

// A code that is ALWAYS ambiguous must not monopolize the exchange.
//
// Retrying one code inside a blocking loop reopened the kill switch in slow
// motion: a local process submits a malformed code that consistently draws 422,
// the loop sits on it, and the honest redirect waits in the queue until GitHub's
// window closes — App created, key unrecoverable. Round-robin is what makes the
// injected code cost time instead of the credential.
func TestAPersistentlyAmbiguousCodeDoesNotBlockTheHonestOne(t *testing.T) {
	shortenBackoff(t)

	fake := newFakeGitHub(t)
	fake.rejectCode = "injected-by-a-local-process"
	fake.rejectStatus = http.StatusUnprocessableEntity

	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	browser := &browser{t: t, fake: fake, client: srv.Client()}

	var calls atomic.Int32

	attacked := func(ctx context.Context, target string) error {
		if calls.Add(1) == 1 {
			page, _ := browser.get(ctx, target)
			state := extractAttr(page, "state=")
			if state == "" {
				return errors.New("could not read the state from the start page")
			}

			forged := strings.TrimSuffix(target, "/") + "/callback?code=" + fake.rejectCode + "&state=" + state

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, forged, http.NoBody)
			if err != nil {
				return err
			}

			resp, err := srv.Client().Do(req)
			if err != nil {
				return err
			}

			resp.Body.Close()
		}

		return browser.open(ctx, target)
	}

	result, err := Onboard(ctx, OnboardOptions{
		Org:          "acme",
		OpenBrowser:  attacked,
		Log:          func(string, ...any) {},
		Client:       srv.Client(),
		InstallPoll:  20 * time.Millisecond,
		apiBase:      srv.URL,
		OnAppCreated: func(*App) error { return nil },
	})
	if err != nil {
		t.Fatalf("an injected code that never resolves blocked the honest one: %v", err)
	}

	if result == nil || result.App == nil || result.App.ID == 0 {
		t.Fatal("onboarding produced no app")
	}
}

// A callback billet ACKNOWLEDGED must be exchanged at least once.
//
// Bounding admission rather than retries made this strictly worse than having no
// bound at all: injected codes that stay ambiguous fill the retry set
// permanently, and the honest redirect — already answered "App created" by the
// handler — was then dropped before it was ever tried. The unbounded version at
// least reached it eventually.
func TestTheHonestCodeIsTriedEvenBehindAFullRetrySet(t *testing.T) {
	shortenBackoff(t)

	fake := newFakeGitHub(t)
	fake.rejectStatus = http.StatusUnprocessableEntity

	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	browser := &browser{t: t, fake: fake, client: srv.Client()}

	var calls atomic.Int32

	attacked := func(ctx context.Context, target string) error {
		if calls.Add(1) == 1 {
			page, _ := browser.get(ctx, target)
			state := extractAttr(page, "state=")
			if state == "" {
				return errors.New("could not read the state from the start page")
			}

			// Comfortably more than one round of attempts, all permanently
			// ambiguous, all queued BEFORE the honest redirect.
			for i := range maxAttemptsPerRound * 2 {
				forged := fmt.Sprintf("%s/callback?code=injected-%d&state=%s",
					strings.TrimSuffix(target, "/"), i, state)

				req, err := http.NewRequestWithContext(ctx, http.MethodGet, forged, http.NoBody)
				if err != nil {
					return err
				}

				resp, err := srv.Client().Do(req)
				if err != nil {
					return err
				}

				resp.Body.Close()
			}
		}

		return browser.open(ctx, target)
	}

	// Every injected code is ambiguous forever; only the real one redeems.
	fake.rejectPrefix = "injected-"

	result, err := Onboard(ctx, OnboardOptions{
		Org:          "acme",
		OpenBrowser:  attacked,
		Log:          func(string, ...any) {},
		Client:       srv.Client(),
		InstallPoll:  20 * time.Millisecond,
		apiBase:      srv.URL,
		OnAppCreated: func(*App) error { return nil },
	})
	if err != nil {
		t.Fatalf("the honest code was dropped behind a full retry set: %v", err)
	}

	if result == nil || result.App == nil || result.App.ID == 0 {
		t.Fatal("onboarding produced no app")
	}
}

// THE invariant of the exchange loop, stated once and exercised against every
// shape that has broken it.
//
// **Every callback the handler acknowledged is attempted at least once.**
//
// That sentence is the whole safety property. The handler answers "App created"
// before billet knows whether the code is any good, so a code it acknowledged and
// then never presented to GitHub is a credential nobody can reach: the App
// exists, the one-time code dies with the process, and GitHub will not re-issue.
//
// Four separate bugs were all violations of this one line, and each was found by
// review rather than by a test — which is why it is written down as a property
// here rather than as four regression cases:
//
//   - an unbounded retry set let injected codes accumulate until the honest one
//     never came up inside the window;
//   - bounding ADMISSION dropped an acknowledged code outright;
//   - bounding RETENTION dropped it one ambiguous response later;
//   - a transport failure returned before draining codes that arrived while its
//     own request was in flight.
//
// The fake records every code it is asked to convert, so the assertion is against
// what GitHub actually saw, not against billet's account of itself.
func TestEveryAcknowledgedCodeIsAttempted(t *testing.T) {
	for name, sc := range map[string]struct {
		// injected is how many forged callbacks are queued before the honest one.
		injected int
		// injectedStatus is what the fake answers for those codes, forever.
		injectedStatus int
		// failInjectedTransport makes the FIRST injected code fail at the
		// transport layer rather than with a status.
		failInjectedTransport bool
		// honestAmbiguousOnce makes the honest code draw one ambiguous answer
		// before it redeems, so it passes THROUGH the retry set.
		honestAmbiguousOnce bool
	}{
		"one ambiguous injection": {
			injected: 1, injectedStatus: http.StatusUnprocessableEntity,
		},
		"more injections than one round of attempts": {
			injected: maxAttemptsPerRound * 2, injectedStatus: http.StatusUnprocessableEntity,
		},
		"rate-limited injections": {
			injected: maxAttemptsPerRound + 1, injectedStatus: http.StatusTooManyRequests,
		},
		"server-error injections": {
			injected: 3, injectedStatus: http.StatusInternalServerError,
		},
		"transport failure ahead of the honest code": {
			injected: 1, injectedStatus: http.StatusUnprocessableEntity, failInjectedTransport: true,
		},
		// The one that matters for a retention cap. An honest code that redeems on
		// its first attempt never enters the retry set and so can never be dropped
		// by it — which is why the earlier version of this test passed against the
		// bug it was written for.
		"honest code ambiguous once, behind a full round": {
			injected: maxAttemptsPerRound * 2, injectedStatus: http.StatusUnprocessableEntity,
			honestAmbiguousOnce: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			// The property is about ORDERING, not about waiting. Real backoff would
			// make this take minutes and prove nothing extra.
			shortenBackoff(t)

			fake := newFakeGitHub(t)
			fake.rejectPrefix = "injected-"
			fake.rejectStatus = sc.injectedStatus
			fake.ambiguousOnce = sc.honestAmbiguousOnce

			srv := httptest.NewServer(fake.handler())
			defer srv.Close()

			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()

			// Attempts are recorded at the TRANSPORT, not at the fake server.
			//
			// The fake can only see requests that reach it, so a code whose request
			// fails at the transport layer looks un-attempted from there — which is
			// the difference between "billet abandoned it" and "billet tried and
			// the network broke". The invariant is about what billet DID, so it is
			// measured on billet's side of the wire.
			var (
				attemptMu sync.Mutex
				attempted []string
			)

			base := srv.Client().Transport

			client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "/app-manifests/") {
					code := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app-manifests/"), "/conversions")

					attemptMu.Lock()
					attempted = append(attempted, code)
					attemptMu.Unlock()

					// A transport error for exactly one code — the shape that used
					// to return immediately and abandon everything queued behind it.
					if sc.failInjectedTransport && code == "injected-0" {
						return nil, errors.New("simulated transport failure")
					}
				}

				return base.RoundTrip(r)
			})}

			browser := &browser{t: t, fake: fake, client: srv.Client()}

			var calls atomic.Int32

			acknowledged := make([]string, 0, sc.injected)

			attacked := func(ctx context.Context, target string) error {
				if calls.Add(1) == 1 {
					page, _ := browser.get(ctx, target)
					state := extractAttr(page, "state=")
					if state == "" {
						return errors.New("could not read the state from the start page")
					}

					for i := range sc.injected {
						code := fmt.Sprintf("injected-%d", i)

						forged := fmt.Sprintf("%s/callback?code=%s&state=%s",
							strings.TrimSuffix(target, "/"), code, state)

						req, err := http.NewRequestWithContext(ctx, http.MethodGet, forged, http.NoBody)
						if err != nil {
							return err
						}

						resp, err := srv.Client().Do(req)
						if err != nil {
							return err
						}

						resp.Body.Close()

						// ACKNOWLEDGED is the precondition of the invariant. A
						// callback billet refused (503) carries no promise.
						if resp.StatusCode == http.StatusOK {
							acknowledged = append(acknowledged, code)
						}
					}
				}

				return browser.open(ctx, target)
			}

			_, err := Onboard(ctx, OnboardOptions{
				Org:          "acme",
				OpenBrowser:  attacked,
				Log:          func(string, ...any) {},
				Client:       client,
				InstallPoll:  20 * time.Millisecond,
				apiBase:      srv.URL,
				OnAppCreated: func(*App) error { return nil },
			})
			if err != nil {
				t.Fatalf("onboarding failed with an honest code available: %v", err)
			}

			// The honest code is acknowledged too — driveRegistration posts it —
			// and reaching here proves it was attempted, because it is the only
			// one that redeems.
			attemptMu.Lock()
			defer attemptMu.Unlock()

			for _, code := range acknowledged {
				if !slices.Contains(attempted, code) {
					t.Errorf("callback %q was acknowledged and never attempted; "+
						"attempted = %v", code, attempted)
				}
			}
		})
	}
}

func TestOnboardRequiresOrgAndLog(t *testing.T) {
	if _, err := Onboard(t.Context(), OnboardOptions{Log: func(string, ...any) {}}); err == nil {
		t.Error("an empty org should be rejected")
	}

	if _, err := Onboard(t.Context(), OnboardOptions{Org: "acme"}); err == nil {
		t.Error("a missing Log should be rejected")
	}
}
