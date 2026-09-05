package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The manifest code buys the App's private key, and it is still live when the
// POST fails. net/http reports transport failures as *url.Error, whose Error()
// embeds the full URL — so a proxy misconfiguration or a DNS failure would print
// the code to stderr, where anyone reading a terminal scrollback, a systemd
// journal or a CI log could redeem it first.
func TestConvertManifestDoesNotLeakTheCodeOnTransportFailure(t *testing.T) {
	const code = "super-secret-one-time-code"

	// Accepting then closing gives a transport error rather than an HTTP status.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	base, client := srv.URL, srv.Client()

	srv.Close()

	_, err := convertManifestAt(t.Context(), client, base, code)
	if err == nil {
		t.Fatal("expected a transport error against a closed server")
	}

	if strings.Contains(err.Error(), code) {
		t.Errorf("the one-time code appears in the error:\n%v", err)
	}

	// The underlying network error must survive, or an operator cannot tell a
	// proxy failure from a DNS one.
	//nolint:errcheck // the discarded value is the typed error itself, not a failure; the bool is the answer. errcheck cannot exclude a generic function.
	_, ok := errors.AsType[*url.Error](err)
	if !ok {
		t.Errorf("the transport error was discarded rather than redacted: %v", err)
	}
}

// Redacting only the OUTER *url.Error was not enough. http.Client.Do wraps
// whatever a RoundTripper returns, so a transport that itself produces a
// *url.Error leaves the inner one — carrying the live code — inside the
// retained Err.
func TestConvertManifestRedactsANestedURLError(t *testing.T) {
	const code = "super-secret-one-time-code"

	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		// Exactly the shape an instrumented transport produces.
		return nil, &url.Error{Op: "dial", URL: r.URL.String(), Err: errors.New("inner boom")}
	})}

	_, err := convertManifestAt(t.Context(), client, "https://example.invalid", code)
	if err == nil {
		t.Fatal("expected an error from the failing transport")
	}

	if strings.Contains(err.Error(), code) {
		t.Errorf("a nested url.Error leaked the one-time code:\n%v", err)
	}

	// The transport's own message must survive, or the operator loses the only
	// clue about what actually failed.
	if !strings.Contains(err.Error(), "inner boom") {
		t.Errorf("redaction discarded the transport's message: %v", err)
	}
}

// A non-201 response never went through redaction: apiError renders the body,
// and an intermediary that echoes the request route into its error message puts
// the still-live code straight on the operator's terminal.
func TestConvertManifestRedactsTheCodeFromAnErrorBody(t *testing.T) {
	const code = "super-secret-one-time-code"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		// A proxy echoing the path it could not reach.
		if _, err := fmt.Fprintf(w, `{"message":"upstream failed for %s"}`, r.URL.Path); err != nil {
			t.Errorf("write test response: %v", err)
		}
	}))
	defer srv.Close()

	_, err := convertManifestAt(t.Context(), srv.Client(), srv.URL, code)
	if err == nil {
		t.Fatal("expected an error from a 502")
	}

	if strings.Contains(err.Error(), code) {
		t.Errorf("an error body leaked the one-time code:\n%v", err)
	}
}

// Redaction must not destroy the error chain: replacing the wrapped error with
// errors.New broke errors.Is against context.DeadlineExceeded and every other
// classification a caller might make.
func TestRedactionKeepsTheErrorChainInspectable(t *testing.T) {
	const code = "super-secret-one-time-code"

	sentinel := errors.New("sentinel transport failure")

	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: r.URL.String(), Err: sentinel}
	})}

	_, err := convertManifestAt(t.Context(), client, "https://example.invalid", code)
	if err == nil {
		t.Fatal("expected an error")
	}

	if strings.Contains(err.Error(), code) {
		t.Errorf("the code survived redaction:\n%v", err)
	}

	if !errors.Is(err, sentinel) {
		t.Errorf("redaction broke the error chain; errors.Is could not find the transport error: %v", err)
	}
}

// Sanitizing Error() alone was not enough, and the test above proves why by
// accident: it performs the errors.As and never looks at what it extracted.
//
// The extracted *url.Error is the ORIGINAL, whose URL field still carries the
// live code — so `urlErr, _ := errors.AsType[*url.Error](err); log("cause", urlErr)`
// prints it, as
// does any error reporter that walks causes and serializes them. Redaction has
// to hold for every error reachable from the one returned, not just the outermost.
func TestRedactionSanitizesEveryErrorInTheChain(t *testing.T) {
	const code = "super-secret-one-time-code"

	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: r.URL.String(), Err: errors.New("inner boom")}
	})}

	_, err := convertManifestAt(t.Context(), client, "https://example.invalid", code)
	if err == nil {
		t.Fatal("expected an error")
	}

	for at := err; at != nil; at = errors.Unwrap(at) {
		if strings.Contains(at.Error(), code) {
			t.Errorf("error of type %T renders the one-time code: %v", at, at)
		}
	}

	// The field, not just the rendering: a caller that logs urlErr.URL directly
	// bypasses Error() entirely.
	//
	// errors.As is REQUIRED to succeed first. Guarding the field check on it
	// meant an implementation that dropped every *url.Error from the chain
	// passed — which is the opposite of the inspectability this is here to keep.
	urlErr, ok := errors.AsType[*url.Error](err)
	if !ok {
		t.Fatal("the transport error was discarded rather than sanitized; callers cannot classify the failure")
	}

	if strings.Contains(urlErr.URL, code) {
		t.Errorf("url.Error.URL still carries the one-time code: %s", urlErr.URL)
	}
}

// Every redaction test used a code made entirely of URL-unreserved characters,
// so PathEscape(code) == code and the escaping branch never ran at all. The
// encoded spellings are exactly the case the helper exists for.
func TestRedactionCoversCodesThatNeedEscaping(t *testing.T) {
	for name, code := range map[string]string{
		"slash":      "abc/def",
		"space":      "abc def",
		"percent":    "abc%def",
		"mixed case": "AbC/dEf",
		"plus":       "abc+def",
		"ampersand":  "abc&def=ghi",
	} {
		t.Run(name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return nil, &url.Error{Op: "Post", URL: r.URL.String(), Err: errors.New("boom")}
			})}

			_, err := convertManifestAt(t.Context(), client, "https://example.invalid", code)
			if err == nil {
				t.Fatal("expected an error")
			}

			// Every spelling the value can reach a string in: raw, path-escaped,
			// query-escaped, and each of those escaped again by an intermediary
			// that embeds the URL in another URL.
			for _, spelling := range []string{
				code,
				url.PathEscape(code),
				url.QueryEscape(code),
				url.PathEscape(url.PathEscape(code)),
				url.QueryEscape(url.QueryEscape(code)),
			} {
				for at := err; at != nil; at = errors.Unwrap(at) {
					if strings.Contains(at.Error(), spelling) {
						t.Errorf("spelling %q of the code survived in %T: %v", spelling, at, at)
					}
				}
			}
		})
	}
}

// The conversion endpoint is the one place GitHub ever hands over the private
// key, so its error body is the one body that must never be rendered.
//
// apiError prints the JSON message, or 200 raw bytes when there is none. An
// intermediary that receives the credential-bearing 201 and forwards it with a
// rewritten status — a proxy translating an upstream hiccup into a 502 — would
// put the only copy of the private key on the operator's terminal, while also
// reporting the conversion as failed.
func TestConversionErrorNeverRendersCredentials(t *testing.T) {
	const code = "super-secret-one-time-code"

	body := `{"id":1,"pem":"-----BEGIN RSA PRIVATE KEY-----\nMIIsecretkeymaterial\n-----END RSA PRIVATE KEY-----",` +
		`"webhook_secret":"whsec-do-not-print","client_secret":"cs-do-not-print"}`

	for name, respond := range map[string]func(w http.ResponseWriter) error{
		"json body": func(w http.ResponseWriter) error {
			w.WriteHeader(http.StatusBadGateway)
			_, err := w.Write([]byte(body))

			return err
		},
		"non-json body": func(w http.ResponseWriter) error {
			w.WriteHeader(http.StatusBadGateway)
			_, err := w.Write([]byte("upstream error, original response: " + body))

			return err
		},
		// GitHub's `message` is the one field passed through, so a proxy that
		// quotes the upstream response INTO that field would smuggle the key out
		// through the only opening left.
		"credentials inside the message field": func(w http.ResponseWriter) error {
			w.WriteHeader(http.StatusBadGateway)

			quoted, err := json.Marshal(struct {
				Message string `json:"message"`
			}{Message: "upstream returned: " + body})
			if err != nil {
				return err
			}

			_, err = w.Write(quoted)

			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if err := respond(w); err != nil {
					t.Errorf("write test response: %v", err)
				}
			}))
			defer srv.Close()

			_, err := convertManifestAt(t.Context(), srv.Client(), srv.URL, code)
			if err == nil {
				t.Fatal("expected an error from a 502")
			}

			for at := err; at != nil; at = errors.Unwrap(at) {
				for _, secret := range []string{"MIIsecretkeymaterial", "whsec-do-not-print", "cs-do-not-print"} {
					if strings.Contains(at.Error(), secret) {
						t.Errorf("a conversion error body leaked %q:\n%v", secret, at)
					}
				}
			}

			// The status still has to reach the operator, or the error says nothing.
			if !strings.Contains(err.Error(), "502") {
				t.Errorf("the error dropped the status code: %v", err)
			}
		})
	}
}

// errors.Join builds a TREE, and errors.Unwrap returns nil for one — so a walk
// that knows only the single-error form stops dead at the join and leaves every
// branch below it unredacted. The transport is caller-supplied, so the shape of
// what it returns is not billet's to assume.
func TestRedactionWalksJoinedErrors(t *testing.T) {
	const code = "super-secret-one-time-code"

	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.Join(
			errors.New("primary transport failure"),
			&url.Error{Op: "Post", URL: r.URL.String(), Err: errors.New("secondary")},
		)
	})}

	_, err := convertManifestAt(t.Context(), client, "https://example.invalid", code)
	if err == nil {
		t.Fatal("expected an error")
	}

	if strings.Contains(err.Error(), code) {
		t.Errorf("a joined error leaked the one-time code:\n%v", err)
	}

	// The branch that carried the code is the one to check, so walk the tree
	// rather than trusting the top-level rendering.
	var walk func(error)

	walk = func(at error) {
		if at == nil {
			return
		}

		if strings.Contains(at.Error(), code) {
			t.Errorf("error of type %T in the joined tree renders the code: %v", at, at)
		}

		if joined, ok := at.(interface{ Unwrap() []error }); ok {
			for _, branch := range joined.Unwrap() {
				walk(branch)
			}

			return
		}

		walk(errors.Unwrap(at))
	}

	walk(err)
}

// An OPAQUE leaf — no Unwrap, no structure — that builds its own message from
// the request URL.
//
// The structural rebuild only reaches a *url.Error. A leaf like this has nothing
// to rebuild, so leaving it untouched to preserve its identity left the live
// code reachable through it. Identity is worth keeping; it is not worth keeping
// at the cost of a credential.
func TestRedactionScrubsAnOpaqueLeafThatEmbedsTheURL(t *testing.T) {
	const code = "super-secret-one-time-code"

	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		// No wrapping, no Unwrap: an instrumented transport reporting what it
		// could not reach.
		return nil, errors.New("proxy could not reach " + r.URL.String())
	})}

	_, err := convertManifestAt(t.Context(), client, "https://example.invalid", code)
	if err == nil {
		t.Fatal("expected an error")
	}

	for at := err; at != nil; at = errors.Unwrap(at) {
		if strings.Contains(at.Error(), code) {
			t.Errorf("an opaque leaf of type %T renders the one-time code: %v", at, at)
		}
	}
}

// An error whose Unwrap returns itself must not recurse until the stack dies.
// The transport is caller-supplied, so the shape is not billet's to assume.
func TestRedactionTerminatesOnACyclicChain(t *testing.T) {
	const code = "super-secret-one-time-code"

	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return nil, &selfWrappingError{msg: "cycle at " + r.URL.String()}
	})}

	// A stack overflow is not recoverable, so this failing looks like the whole
	// test binary dying rather than one red test.
	_, err := convertManifestAt(t.Context(), client, "https://example.invalid", code)
	if err == nil {
		t.Fatal("expected an error")
	}

	depth := 0
	for at := err; at != nil && depth < 1000; at = errors.Unwrap(at) {
		if strings.Contains(at.Error(), code) {
			t.Errorf("a cyclic chain leaked the code at depth %d: %v", depth, at)
		}

		depth++
	}

	if depth >= 1000 {
		t.Error("the sanitized chain is still effectively unbounded")
	}
}

// selfWrappingError unwraps to itself, which is the cheapest way to build a
// cycle an error walker can fall into.
type selfWrappingError struct{ msg string }

func (e *selfWrappingError) Error() string { return e.msg }
func (e *selfWrappingError) Unwrap() error { return e }

// An error whose dynamic type is NOT COMPARABLE must not crash the sanitizer.
//
// The cycle guard compared two error interfaces with ==, which panics at runtime
// when the dynamic type contains a slice or a map — an entirely ordinary thing
// for an error type to hold. The guard added to protect the process could take
// it down instead, and a panic mid-onboarding loses the one-time key.
func TestRedactionSurvivesNonComparableErrors(t *testing.T) {
	const code = "super-secret-one-time-code"

	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		// VALUES, not pointers, and that distinction is the whole test: comparing
		// two interfaces holding pointers never panics however uncomparable the
		// pointee is. It is the non-pointer dynamic type that blows up. Two of
		// them, because == is only reached when a node is compared against an
		// ancestor.
		return nil, uncomparableError{
			msg:    "outer at " + r.URL.String(),
			detail: []string{"a", "b"},
			inner:  uncomparableError{msg: "inner", detail: []string{"c"}},
		}
	})}

	_, err := convertManifestAt(t.Context(), client, "https://example.invalid", code)
	if err == nil {
		t.Fatal("expected an error")
	}

	for at := err; at != nil; at = errors.Unwrap(at) {
		if strings.Contains(at.Error(), code) {
			t.Errorf("%T renders the one-time code: %v", at, at)
		}
	}
}

// uncomparableError holds a slice, so `==` on two of these panics.
type uncomparableError struct {
	msg    string
	detail []string
	inner  error
}

func (e uncomparableError) Error() string { return e.msg }
func (e uncomparableError) Unwrap() error { return e.inner }

// url.Error has three fields and only one of them was being cleaned.
//
// The rebuild replaced URL and copied Op verbatim, and it kept the parsed scheme
// and host — so a transport that put the request URL in Op, or a URL whose HOST
// carried the code, walked straight through the one path that is supposed to be
// structurally safe.
func TestRedactionCleansEveryURLErrorField(t *testing.T) {
	const code = "super-secret-one-time-code"

	for name, build := range map[string]func(r *http.Request) error{
		"code in Op": func(r *http.Request) error {
			return &url.Error{Op: "POST " + r.URL.String(), URL: r.URL.String(), Err: errors.New("boom")}
		},
		"code in host": func(_ *http.Request) error {
			return &url.Error{Op: "Post", URL: "https://" + code + ".example.invalid/x", Err: errors.New("boom")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				return nil, build(r)
			})}

			_, err := convertManifestAt(t.Context(), client, "https://example.invalid", code)
			if err == nil {
				t.Fatal("expected an error")
			}

			for at := err; at != nil; at = errors.Unwrap(at) {
				if strings.Contains(at.Error(), code) {
					t.Errorf("%T still renders the one-time code: %v", at, at)
				}
			}

			// errors.As must SUCCEED. Guarding the field checks on it meant an
			// implementation that dropped every *url.Error passed this test while
			// destroying the inspectability it exists to preserve.
			urlErr, ok := errors.AsType[*url.Error](err)
			if !ok {
				t.Fatal("the transport error was discarded rather than cleaned")
			}

			if strings.Contains(urlErr.Op, code) {
				t.Errorf("url.Error.Op carries the code: %q", urlErr.Op)
			}

			if strings.Contains(urlErr.URL, code) {
				t.Errorf("url.Error.URL carries the code: %q", urlErr.URL)
			}
		})
	}
}

// Only a status meaning THIS CODE is unusable may discard the code.
//
// Both ends of this were wrong before. Classifying just 404 and 422 left a kill
// switch open — an injected code drawing a 400 or 414 aborted the flow and threw
// away an honest code queued behind it. Then widening it to every 4xx swallowed
// 429, which is the more expensive mistake: a rate limit says nothing about the
// code, so a VALID code was discarded, the App stayed created, and the loop
// waited out ManifestTTL for a redirect that had already arrived.
func TestOnlyCodeSpecificStatusesDiscardTheCode(t *testing.T) {
	// 404 is the one unambiguous answer: GitHub does not know this code. A forged
	// code is a random string, so this is the status a forged one draws — which
	// is exactly the case that needed handling.
	if err := conversionError(http.StatusNotFound, nil); !errors.Is(err, errCodeRejected) {
		t.Errorf("HTTP 404 was not classified as a rejected code: %v", err)
	}

	// None of these establish that the presented code is bad, so discarding it
	// would throw away a credential over a condition the next code cannot fix.
	// They are AMBIGUOUS — the same code is retried rather than skipped.
	//
	// 422 is the one worth naming: GitHub documents it as "Validation failed, OR
	// the endpoint has been spammed". An attacker feeding forged codes can trip
	// abuse protection and make the honest code's 422 look like a rejection.
	//
	// 414 is here because the callback bounds code length before queueing: a 414
	// for a code billet accepted is an intermediary's own limit, not a statement
	// about the code, so discarding on it would orphan an App that exists.
	for _, status := range []int{
		http.StatusUnprocessableEntity,
		http.StatusTooManyRequests,
		http.StatusBadRequest,
		http.StatusRequestTimeout,
		http.StatusForbidden,
		http.StatusInternalServerError,
		http.StatusBadGateway,
	} {
		t.Run("retries/"+http.StatusText(status), func(t *testing.T) {
			err := conversionError(status, nil)

			if errors.Is(err, errCodeRejected) {
				t.Errorf("HTTP %d discarded the code; it does not establish that the code is bad", status)
			}

			if !errors.Is(err, errCodeAmbiguous) {
				t.Errorf("HTTP %d is neither a rejection nor retryable, so the code is silently lost: %v",
					status, err)
			}
		})
	}

	for _, status := range []int{http.StatusRequestURITooLong, http.StatusUpgradeRequired} {
		t.Run("never discards/"+http.StatusText(status), func(t *testing.T) {
			if err := conversionError(status, nil); errors.Is(err, errCodeRejected) {
				t.Errorf("HTTP %d discarded the code", status)
			}
		})
	}
}

// The classification has to hold through the REAL flow, not just as a table.
//
// Asserting the status map on its own only proves it was copied into the test.
// This drives onboarding end to end against a server that answers 422 twice
// before succeeding — the shape abuse protection produces.
//
// Getting this wrong is credential loss, not a delay. The code exists only in a
// local variable and the loopback listener dies with the flow, so a run that
// gives up has orphaned an App whose key GitHub will not re-issue. "Run the
// command again" builds a SECOND App; it does not recover the first one's key.
func TestAnAmbiguousRejectionRetriesTheSameCode(t *testing.T) {
	shortenBackoff(t)

	fake := newFakeGitHub(t)
	fake.rejectStatus = http.StatusUnprocessableEntity
	fake.ambiguousFirst = 2

	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	browser := &browser{t: t, fake: fake, client: srv.Client()}

	result, err := Onboard(ctx, OnboardOptions{
		Target:       OrganizationTarget("acme"),
		OpenBrowser:  browser.open,
		Log:          func(string, ...any) {},
		Client:       srv.Client(),
		InstallPoll:  20 * time.Millisecond,
		apiBase:      srv.URL,
		OnAppCreated: func(*App) error { return nil },
	})
	if err != nil {
		t.Fatalf("an ambiguous rejection ended the flow and orphaned the App: %v", err)
	}

	if result == nil || result.App == nil || result.App.ID == 0 {
		t.Fatal("onboarding produced no app")
	}

	// Three conversions: two ambiguous answers, then the one that redeemed. Fewer
	// would mean the retries never happened and this proves nothing; the count is
	// what distinguishes "kept the code" from "was handed a second one".
	if n := fake.conversions.Load(); n != 3 {
		t.Errorf("conversions = %d, want 3 (two ambiguous, then success)", n)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The permission set is a security claim billet makes in its README and in the
// CLI's own output. Pin it, so widening it is a deliberate edit to a test rather
// than a quiet change to a map.
func TestPermissionsAreMinimal(t *testing.T) {
	want := map[string]string{
		"metadata":                         "read",
		"organization_self_hosted_runners": "write",
	}

	got := Permissions(ScopeOrganization)

	if len(got) != len(want) {
		t.Fatalf("Permissions has %d entries, want %d: %v", len(got), len(want), got)
	}

	for k, v := range want {
		if got[k] != v {
			t.Errorf("Permissions[%q] = %q, want %q", k, got[k], v)
		}
	}

	// Named explicitly: actions:read would expose workflow runs, logs and
	// artifacts, which is what makes "billet cannot read your code" false.
	for _, forbidden := range []string{"actions", "contents", "administration", "secrets"} {
		if _, ok := got[forbidden]; ok {
			t.Errorf("Permissions must not include %q", forbidden)
		}
	}
}

// Permissions must hand back a copy: it is a security claim, and a caller that
// can mutate it changes the manifest, the CLI's disclosure and the post-install
// validation in one go.
func TestPermissionsReturnsACopy(t *testing.T) {
	Permissions(ScopeOrganization)["contents"] = "write"

	if _, leaked := Permissions(ScopeOrganization)["contents"]; leaked {
		t.Fatal("mutating the returned map changed the canonical permission set")
	}
}

// GitHub marks hook_attributes.url required whenever the object is present, so
// an inactive hook still needs one. Omitting it rejects the whole registration.
func TestManifestCarriesAWebhookURLEvenThoughItIsInactive(t *testing.T) {
	m := NewManifest("billet", "http://127.0.0.1:1/callback", "http://127.0.0.1:1/installed", ScopeOrganization)

	if m.HookAttributes.URL == "" {
		t.Error("hook_attributes.url is required by GitHub even when active is false")
	}

	if !strings.HasPrefix(m.HookAttributes.URL, "https://") {
		t.Errorf("hook url should be a stable https URL, got %q", m.HookAttributes.URL)
	}
}

// setup_on_update would send a later repository-access change to a loopback port
// that stopped existing when onboarding finished.
func TestManifestDoesNotAskForUpdateRedirects(t *testing.T) {
	m := NewManifest("billet", "http://127.0.0.1:1/callback", "http://127.0.0.1:1/installed", ScopeOrganization)

	if m.SetupOnUpdate {
		t.Error("setup_on_update must be off: the setup URL is an ephemeral loopback listener")
	}
}

// billet long-polls; it must not register a webhook, because a webhook is what
// would force a deployment to accept inbound internet traffic.
func TestManifestDisablesWebhook(t *testing.T) {
	m := NewManifest("billet", "http://127.0.0.1:1/callback", "http://127.0.0.1:1/installed", ScopeOrganization)

	if m.HookAttributes == nil {
		t.Fatal("manifest should carry hook_attributes so the webhook is explicitly off")
	}

	if m.HookAttributes.Active {
		t.Error("webhook must be inactive: billet needs no inbound ingress")
	}

	if m.Public {
		t.Error("an app scoped to one organization's runners must not be public")
	}
}

func TestManifestRoundTripsThroughJSON(t *testing.T) {
	m := NewManifest("billet", "http://127.0.0.1:1/callback", "http://127.0.0.1:1/installed", ScopeOrganization)

	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// GitHub documents `url` as required; omitting it fails the registration with
	// a message that does not name the field.
	if decoded["url"] == "" || decoded["url"] == nil {
		t.Error("manifest must carry a url")
	}

	perms, ok := decoded["default_permissions"].(map[string]any)
	if !ok {
		t.Fatalf("default_permissions missing or wrong type: %T", decoded["default_permissions"])
	}

	if perms["organization_self_hosted_runners"] != "write" {
		t.Errorf("self-hosted runners permission = %v, want write", perms["organization_self_hosted_runners"])
	}
}

func TestRegistrationURLEscapesOrg(t *testing.T) {
	got := RegistrationURL("my org/evil", OwnerOrganization, "st/ate+1")

	if strings.Contains(got, "my org") {
		t.Errorf("org was not escaped: %s", got)
	}

	if !strings.HasPrefix(got, "https://github.com/organizations/") {
		t.Errorf("unexpected prefix: %s", got)
	}

	if !strings.Contains(got, "settings/apps/new?state=") {
		t.Errorf("missing state parameter: %s", got)
	}
}

func TestConvertManifestSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}

		if !strings.HasPrefix(r.URL.Path, "/app-manifests/") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}

		if got := r.Header.Get("X-Github-Api-Version"); got == "" {
			t.Error("missing X-GitHub-Api-Version header")
		}

		w.WriteHeader(http.StatusCreated)
		if _, err := w.Write([]byte(`{"id":42,"slug":"billet-acme","name":"billet","html_url":"https://github.com/apps/billet-acme","pem":"-----BEGIN RSA PRIVATE KEY-----\nx\n-----END RSA PRIVATE KEY-----\n"}`)); err != nil {
			t.Errorf("write test response: %v", err)
		}
	}))
	defer srv.Close()

	app, err := convertManifestAt(t.Context(), srv.Client(), srv.URL, "code123")
	if err != nil {
		t.Fatalf("ConvertManifest: %v", err)
	}

	if app.ID != 42 {
		t.Errorf("id = %d, want 42", app.ID)
	}

	if got := app.InstallURL(); got != "https://github.com/apps/billet-acme/installations/new" {
		t.Errorf("InstallURL = %q", got)
	}
}

// App is exactly the sort of value that ends up in a debug print or a wrapped
// error, and a plain %v would otherwise emit the App's private key.
func TestAppFormattingRedactsCredentials(t *testing.T) {
	app := App{
		ID:            42,
		Slug:          "billet-acme",
		PEM:           "-----BEGIN RSA PRIVATE KEY-----\nSECRETKEYMATERIAL\n",
		WebhookSecret: "SECRETWEBHOOK",
		ClientSecret:  "SECRETCLIENT",
	}

	secrets := []string{"SECRETKEYMATERIAL", "SECRETWEBHOOK", "SECRETCLIENT"}

	// Routed through `any` deliberately. Calling fmt.Sprintf on the concrete
	// type makes staticcheck and gocritic suggest app.String() instead — which
	// would test the method and not the FORMATTING PATH, and the formatting path
	// is the whole risk: somebody prints an App without thinking about it.
	render := func(format string, v any) string { return fmt.Sprintf(format, v) }

	// Every verb a caller might reach for, including the pointer forms (a
	// value-receiver method is also in *App's method set) and verbs that make no
	// sense for a struct. %d is the important one: fmt consults Stringer only
	// for %v/%s/%q/%x/%X, and any other verb formats the fields recursively —
	// printing the private key inside its own bad-verb diagnostic.
	for _, rendered := range []string{
		render("%v", app), render("%s", app), render("%#v", app), render("%q", app),
		render("%d", app), render("%x", app), render("%t", app), render("%+v", app),
		render("%v", &app), render("%s", &app), render("%#v", &app), render("%d", &app),
	} {
		for _, secret := range secrets {
			if strings.Contains(rendered, secret) {
				t.Errorf("formatting leaked %q:\n%s", secret, rendered)
			}
		}
	}

	// Still useful for diagnosis.
	if !strings.Contains(render("%v", app), "billet-acme") {
		t.Error("redaction removed the identifying fields too")
	}
}

// fmt.Formatter covers direct formatting and nothing else. billet standardizes
// on log/slog, whose JSON handler uses encoding/json — which reads the exported
// fields and their tags. `logger.Info("created", "app", app)` was a full
// private-key disclosure into wherever the logs go.
func TestAppDoesNotLeakThroughStructuredLogging(t *testing.T) {
	app := App{
		ID:            42,
		Slug:          "billet-acme",
		PEM:           "-----BEGIN RSA PRIVATE KEY-----\nSECRETKEYMATERIAL\n",
		WebhookSecret: "SECRETWEBHOOK",
		ClientSecret:  "SECRETCLIENT",
	}

	secrets := []string{"SECRETKEYMATERIAL", "SECRETWEBHOOK", "SECRETCLIENT"}

	for name, newHandler := range map[string]func(*bytes.Buffer) slog.Handler{
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer

			slog.New(newHandler(&buf)).Info("created", "app", app, "ptr", &app)

			for _, secret := range secrets {
				if strings.Contains(buf.String(), secret) {
					t.Errorf("the %s handler leaked %q:\n%s", name, secret, buf.String())
				}
			}
		})
	}

	// Plain encoding/json too — anything that serializes the struct.
	encoded, err := json.Marshal(app)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	for _, secret := range secrets {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("json.Marshal leaked %q:\n%s", secret, encoded)
		}
	}

	// Decoding must still populate everything, or onboarding cannot work.
	var decoded App
	if err := json.Unmarshal([]byte(`{"id":7,"pem":"KEY"}`), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.PEM != "KEY" {
		t.Errorf("redacting MarshalJSON broke decoding; PEM = %q", decoded.PEM)
	}
}

// Onboard hands this struct back after the key is on disk. Nothing downstream
// needs a credential, and billet never persists the webhook or client secret —
// it registers an inactive webhook and implements no OAuth flow.
func TestForgetClearsEverySecret(t *testing.T) {
	app := &App{
		ID:            42,
		PEM:           "key",
		WebhookSecret: "hook",
		ClientID:      "Iv1.public",
		ClientSecret:  "secret",
	}

	app.Forget()

	switch {
	case app.PEM != "":
		t.Error("Forget left the private key")
	case app.WebhookSecret != "":
		t.Error("Forget left the webhook secret")
	case app.ClientSecret != "":
		t.Error("Forget left the client secret")
	}

	// ClientID is not a secret and is the value billet may later persist.
	if app.ClientID != "Iv1.public" {
		t.Errorf("Forget discarded the non-secret client id: %q", app.ClientID)
	}

	if app.ID != 42 {
		t.Errorf("Forget discarded the app id: %d", app.ID)
	}
}

// A response that parses but carries no id or key is unusable, and accepting it
// would surface much later as an inscrutable auth failure.
func TestConvertManifestRejectsIncompleteResponse(t *testing.T) {
	for name, body := range map[string]string{
		"no id":  `{"slug":"x","pem":"-----BEGIN RSA PRIVATE KEY-----\nx\n-----END RSA PRIVATE KEY-----\n"}`,
		"no pem": `{"id":42,"slug":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				if _, err := w.Write([]byte(body)); err != nil {
					t.Errorf("write test response: %v", err)
				}
			}))
			defer srv.Close()

			if _, err := convertManifestAt(t.Context(), srv.Client(), srv.URL, "code123"); err == nil {
				t.Fatal("expected an error for an incomplete response")
			}
		})
	}
}

// The expired code is the failure an operator actually hits, so it has to be
// explained — but the explanation is billet's own, not GitHub's.
//
// This test used to assert that GitHub's `message` reached the operator. That
// contract was wrong: this endpoint's 201 carries the App private key, and a
// filter that decides whether a `message` is safe to print cannot work, because
// a secret out of its field is just an opaque string. So nothing derived from
// the body is rendered, and the diagnostic is written from the status.
func TestConvertManifestExplainsAnExpiredCodeWithoutTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 404 is what GitHub returns for a code it does not know, which is the
		// expired-and-already-used case an operator actually hits.
		w.WriteHeader(http.StatusNotFound)
		if _, err := w.Write([]byte(`{"message":"The code passed is incorrect or expired."}`)); err != nil {
			t.Errorf("write test response: %v", err)
		}
	}))
	defer srv.Close()

	_, err := convertManifestAt(t.Context(), srv.Client(), srv.URL, "stale")
	if err == nil {
		t.Fatal("expected an error")
	}

	// The operator needs to know it expired and how long the window is.
	for _, want := range []string{"404", "expired", "1h0m0s"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}

	// And it must not be GitHub's text, however harmless this particular body is
	// — the rule is that the body is not read, not that it is filtered.
	if strings.Contains(err.Error(), "The code passed is incorrect") {
		t.Errorf("the response body was rendered: %v", err)
	}
}

func TestConvertManifestRejectsEmptyCode(t *testing.T) {
	if _, err := ConvertManifest(t.Context(), nil, ""); err == nil {
		t.Fatal("expected an error for an empty code")
	}
}
