package awscreds

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/awssig"
)

const testSecret = "wJalrXUtnFEMI-thisIsTheSecret"

// write sends a fake IMDS response body.
//
// CHECKED RATHER THAN DISCARDED. A short write to the test client does not
// disappear — it reappears as a JSON parse failure inside the code under test,
// which reads as a bug in the parser rather than as a fake that stopped talking.
func write(t *testing.T, w io.Writer, body string) {
	t.Helper()

	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("write the fake response: %v", err)
	}
}

// A SECRET ACCESS KEY IS A DURABLE CREDENTIAL FOR A WHOLE AWS ACCOUNT, and it
// reaches a log through one careless verb on a struct that happens to contain it.
//
// Every rendering path billet can reach is covered, and the reason there are so
// many is that each one ignores the others: slog's JSON handler never consults
// fmt, %#v never consults String, and a pointer-receiver String is not consulted
// when a VALUE is formatted. This is the same discipline the GitHub App key gets.
func TestCredentialsAreRedactedOnEveryRenderingPath(t *testing.T) {
	creds := awssig.Credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: testSecret,
		SessionToken:    "session-token-value",
	}

	// A struct holding them, because that is how the leak actually happens: nobody
	// prints a credential deliberately.
	holder := struct {
		Where string
		Creds awssig.Credentials
	}{"launching", creds}

	// The VERB is what is under test, so these go through fmt rather than calling
	// String directly: a redaction that only works when somebody remembers to call
	// String is not a redaction.
	rendered := map[string]string{
		"%v":             fmt.Sprintf("%v", creds), //nolint:gocritic // the verb path is the subject
		"%s":             fmt.Sprintf("%s", creds), //nolint:gocritic // the verb path is the subject
		"%q":             fmt.Sprintf("%q", creds),
		"%#v":            fmt.Sprintf("%#v", creds),
		"%d":             fmt.Sprintf("%d", creds),
		"%v on a field":  fmt.Sprintf("%v", holder),
		"%+v on a field": fmt.Sprintf("%+v", holder),
	}

	encoded, err := json.Marshal(holder)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rendered["json"] = string(encoded)

	var logged bytes.Buffer

	slog.New(slog.NewJSONHandler(&logged, nil)).Info("launching", "creds", creds)

	rendered["slog"] = logged.String()

	for path, out := range rendered {
		if strings.Contains(out, testSecret) {
			t.Errorf("%s rendered the secret access key: %s", path, out)
		}

		if strings.Contains(out, "session-token-value") {
			t.Errorf("%s rendered the session token: %s", path, out)
		}
	}

	// The access key ID is an identifier rather than a secret, and printing it is
	// the difference between "billet used the wrong role" and an unactionable
	// "authentication failed".
	if !strings.Contains(rendered["%v"], "AKIDEXAMPLE") {
		t.Errorf("the access key id was redacted too, leaving nothing to diagnose with: %s",
			rendered["%v"])
	}
}

// EVERY CREDENTIAL-CARRYING TYPE IN THIS PACKAGE, not just the one.
//
// `type Static awssig.Credentials` is a DEFINED TYPE, and a defined type does not
// inherit the methods of the type it is defined from — so it had every exported
// secret field and none of the redaction, and printing one rendered the secret
// access key in full. The test above covered the credential type itself and said
// nothing about it.
//
// Written as a table over `any` so that a third type carrying a secret has to be
// added here to be trusted, rather than being redacted by whoever remembers.
func TestEveryCredentialTypeIsRedacted(t *testing.T) {
	const secret = "wJalrXUtnFEMI-thisIsTheSecret"

	full := awssig.Credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: secret,
		SessionToken:    "session-token-value",
	}

	// A SOURCE THAT HAS FETCHED, which is the case the type-level redaction does
	// not cover: `cached` is unexported, and fmt cannot call methods on an
	// unexported field — so `%+v` on this printed the secret in full while every
	// test here passed, because they all rendered the field's TYPE rather than a
	// struct holding it privately.
	fetched := &IMDS{}
	fetched.cached = full

	for name, value := range map[string]any{
		"awssig.Credentials": full,
		"Static":             Static(full),
		// A POINTER, because fmt consults a value method through a pointer but not
		// the reverse, and a caller holding one is ordinary.
		"a pointer to awssig.Credentials": &full,
		"a pointer to Static":             func() *Static { s := Static(full); return &s }(),

		// THE SOURCES, not only the credential. Each of these holds one.
		"a fetched IMDS":      fetched,
		"a chain holding one": Chain{Env{}, fetched},
		"Default":             Default(),
	} {
		t.Run(name, func(t *testing.T) {
			holder := struct {
				Where string
				Creds any
			}{"launching", value}

			encoded, err := json.Marshal(holder)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			var logged bytes.Buffer

			slog.New(slog.NewJSONHandler(&logged, nil)).Info("launching", "creds", value)

			rendered := map[string]string{
				"%v":            fmt.Sprintf("%v", value),
				"%s":            fmt.Sprintf("%s", value),
				"%#v":           fmt.Sprintf("%#v", value),
				"%+v":           fmt.Sprintf("%+v", value),
				"%v on a field": fmt.Sprintf("%v", holder),
				"json":          string(encoded),
				"slog":          logged.String(),
			}

			// CALLED DIRECTLY, because the fmt verbs above do not reach them.
			// Implementing Format means fmt consults Formatter for EVERY verb and
			// never String or GoString — so a mutation that made String return the
			// secret survived this test until these two lines existed, while the
			// method stayed reachable by any caller holding a Stringer.
			if sv, ok := value.(fmt.Stringer); ok {
				rendered["String()"] = sv.String()
			}

			if gv, ok := value.(interface{ GoString() string }); ok {
				rendered["GoString()"] = gv.GoString()
			}

			for path, out := range rendered {
				if strings.Contains(out, secret) {
					t.Errorf("%s rendered the secret access key: %s", path, out)
				}

				if strings.Contains(out, "session-token-value") {
					t.Errorf("%s rendered the session token: %s", path, out)
				}
			}
		})
	}
}

// EXPLICIT BEATS AMBIENT. An operator who set AWS_ACCESS_KEY_ID on a machine that
// also has an instance role meant the key; the other order makes that setting
// silently do nothing.
func TestTheEnvironmentWinsOverTheInstanceRole(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKID-FROM-ENV")
	t.Setenv("AWS_SECRET_ACCESS_KEY", testSecret)

	imds := newFakeIMDS(t, "ASIAFROMROLEEXAMP001", time.Now().Add(time.Hour))

	chain := Chain{Env{}, &IMDS{Endpoint: imds.URL}}

	got, err := chain.Credentials(t.Context())
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}

	if got.AccessKeyID != "AKID-FROM-ENV" {
		t.Errorf("access key = %q, want the one from the environment", got.AccessKeyID)
	}
}

// A session token is an opaque blob that must be presented byte for byte, so it
// is NOT trimmed — unlike the two beside it, whose alphabets make trimming a
// rescue for a value pasted into a unit file with a trailing space.
func TestTheEnvironmentIsReadEachTimeAndTheTokenIsNotTrimmed(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "  ASIAPADDEDEXAMPLE001  ")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "  "+testSecret+"  ")
	t.Setenv("AWS_SESSION_TOKEN", " tok ")

	got, err := Env{}.Credentials(t.Context())
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}

	if got.AccessKeyID != "ASIAPADDEDEXAMPLE001" || got.SecretAccessKey != testSecret {
		t.Errorf("a padded key or secret was not trimmed: %q / (secret)", got.AccessKeyID)
	}

	if got.SessionToken != " tok " {
		t.Errorf("session token = %q, want it byte for byte", got.SessionToken)
	}

	// READ ON EVERY CALL, so this source caches no secret between them. NOT an
	// operator-facing refresh, which is what this comment used to claim: nothing
	// outside a process can change its environment, so editing a unit file still
	// needs a restart. What is proved here is that the SAME process observes a later
	// change — which is the property, and is all a Setenv can demonstrate.
	t.Setenv("AWS_ACCESS_KEY_ID", "ASIASECONDEXAMPLE001")

	again, err := Env{}.Credentials(t.Context())
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}

	if again.AccessKeyID != "ASIASECONDEXAMPLE001" {
		t.Errorf("access key = %q, want the value read fresh", again.AccessKeyID)
	}
}

// HALF A CREDENTIAL IS A MISTAKE, NOT AN ABSENCE, AND THE DIFFERENCE DECIDES
// WHICH AWS ACCOUNT BILLET ACTS AS.
//
// Reporting "no credentials" for a partial environment let the chain fall through
// to the instance role — so a typo in AWS_SECRET_ACCESS_KEY silently switched
// billet to a DIFFERENT and often more privileged identity, and it launched and
// terminated machines as that one. The operator who set those variables had said
// which identity they meant.
//
// An earlier version of this test asserted errNoCredentials here and therefore
// enshrined the fallthrough.
func TestHalfAnEnvironmentCredentialIsAnErrorRatherThanAnAbsence(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"no secret": {
			env:  map[string]string{"AWS_ACCESS_KEY_ID": "AKID", "AWS_SECRET_ACCESS_KEY": ""},
			want: "AWS_SECRET_ACCESS_KEY",
		},
		"no key id": {
			env:  map[string]string{"AWS_ACCESS_KEY_ID": "", "AWS_SECRET_ACCESS_KEY": "s"},
			want: "AWS_ACCESS_KEY_ID",
		},
		// A session token alone is half a credential too, and reading it as an
		// absence let the chain move on to a different identity.
		"only a session token": {
			env: map[string]string{
				"AWS_ACCESS_KEY_ID": "", "AWS_SECRET_ACCESS_KEY": "", "AWS_SESSION_TOKEN": "tok",
			},
			want: "AWS_SESSION_TOKEN",
		},
		// AND A BLANK ONE COUNTS AS SET. The token is opaque and presented byte for
		// byte, so billet has no basis for deciding spaces were not meant.
		"a blank session token": {
			env: map[string]string{
				"AWS_ACCESS_KEY_ID": "", "AWS_SECRET_ACCESS_KEY": "", "AWS_SESSION_TOKEN": " ",
			},
			want: "AWS_SESSION_TOKEN",
		},
	} {
		t.Run(name, func(t *testing.T) {
			// EVERY VARIABLE IS SET EXPLICITLY, including the ones a case leaves
			// empty: these read the real environment, so a developer's own
			// AWS_SESSION_TOKEN would otherwise decide the outcome.
			for _, k := range []string{
				"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
			} {
				t.Setenv(k, tc.env[k])
			}

			err := func() error {
				_, err := (Env{}).Credentials(t.Context())

				return err
			}()

			if err == nil {
				t.Fatal("half an environment credential was accepted")
			}

			if errors.Is(err, errNoCredentials) {
				t.Errorf("a partial environment reported ABSENCE, which lets the chain fall "+
					"through to the instance role: %v", err)
			}

			// The message has to name the variable this case is about, or an
			// operator is told only that something is wrong with credentials they
			// can see.
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not name %s: %v", tc.want, err)
			}
		})
	}
}

// AND THE CHAIN STOPS THERE. It continues past a source that has NOTHING, which
// is what a chain is for; it must not continue past one that is misconfigured, or
// the fallthrough happens one layer up instead.
func TestTheChainDoesNotFallPastAMisconfiguredSource(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKID")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")

	imds := newFakeIMDS(t, "ASIAFROMROLEEXAMP001", time.Now().Add(time.Hour))

	chain := Chain{Env{}, &IMDS{Endpoint: imds.URL}}

	got, err := chain.Credentials(t.Context())
	if err == nil {
		t.Fatalf("the chain fell through a half-configured environment to %v", got)
	}

	if imds.tokens != 0 {
		t.Error("the chain contacted the instance metadata service despite an environment " +
			"credential being half set; billet would act as a different aws identity")
	}
}

// fakeIMDS serves the instance metadata service, IMDSv2 style.
type fakeIMDS struct {
	*httptest.Server

	// role is the instance profile the listing names. Configurable so a test can use
	// a name exercising every character AWS's documented set allows, rather than only
	// the tidy one every other case happens to use.
	role string

	// tokens counts how many session tokens were issued, which is how a test can
	// see whether a value was served from cache.
	tokens int
	// attempts counts every request, so a test can tell "IMDS refused" from
	// "IMDS was never reached".
	attempts int
	// noRole models an instance with no instance profile attached: IMDS works
	// perfectly and simply has nothing to give, which is an ABSENCE rather than a
	// failure and is the only thing a chain may continue past.
	noRole bool
	// v1Only models a host with IMDSv2 turned off: the PUT that obtains a session
	// token fails, AND an unauthenticated GET succeeds.
	//
	// BOTH HALVES, and the second one is the whole point. An earlier version only
	// failed the PUT, so a billet that ignored that failure and carried on with an
	// empty token was refused by the GET — and the test, which asserted only that
	// an error came back, passed while proving nothing about the fallback. A
	// mutation that discarded the token error survived it.
	//
	// Serving credentials to an unauthenticated GET is what makes the test able to
	// fail: a billet that downgrades to v1 now SUCCEEDS, and success is what the
	// assertion refuses.
	v1Only bool
}

func newFakeIMDS(t *testing.T, keyID string, expires time.Time) *fakeIMDS {
	t.Helper()

	f := &fakeIMDS{role: "billet-node-role"}

	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.attempts++

		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			if f.v1Only {
				w.WriteHeader(http.StatusNotFound)

				return
			}

			// THE TTL HEADER IS REQUIRED, and the fake enforces it because the real
			// service does: IMDSv2 answers 400 to a token PUT that does not ask for a
			// lifetime. Without this the fake was more permissive than the thing it
			// stands for, so dropping the header from the request changed no test —
			// and a billet that stopped sending it would fail on every real instance
			// while the suite stayed green. A fake that accepts more than production
			// is a fake that hides exactly this.
			// THE EXACT VALUE, not merely a non-empty header. Real IMDS accepts
			// 1..21600 and refuses anything else, so a fake that takes any string
			// leaves "21600" free to become "0" or "garbage" with the suite green and
			// every real instance refusing the token.
			if r.Header.Get("X-Aws-Ec2-Metadata-Token-Ttl-Seconds") != "21600" {
				w.WriteHeader(http.StatusBadRequest)

				return
			}

			f.tokens++
			write(t, w, "imds-session-token")

		case r.Method != http.MethodGet:
			// THE METHOD IS PART OF THE CONTRACT. Real IMDS answers metadata on GET,
			// and a fake that took any verb left billet free to send another with the
			// suite green.
			w.WriteHeader(http.StatusMethodNotAllowed)

		case r.Header.Get("X-Aws-Ec2-Metadata-Token") != "imds-session-token" && !f.v1Only:
			// THE EXACT TOKEN IT ISSUED, not merely a non-empty header. Accepting any
			// value let billet send an arbitrary one — or the wrong one from a previous
			// fetch — while every real instance refused it.
			// V2 ONLY. An unauthenticated GET is exactly the request an SSRF can
			// make on billet's behalf, and it is what would return the role's
			// credentials — so this host refuses one, and the v1Only host below
			// deliberately does not.
			w.WriteHeader(http.StatusUnauthorized)

		case r.URL.Path == "/latest/meta-data/iam/security-credentials/":
			if f.noRole {
				// A 404, WHICH IS WHAT THE SERVICE ANSWERS. This used to write 200 and
				// a newline — a shape real IMDS does not produce — so billet's
				// "no instance profile attached" branch was only ever reached by a
				// fixture, and the ORDINARY no-role instance took a terminal error path
				// that told an operator "http 404" about a URL they have never seen.
				// A fake more permissive than the service hides exactly this.
				w.WriteHeader(http.StatusNotFound)

				return
			}

			write(t, w, f.role+"\n")

		case r.URL.Path == "/latest/meta-data/iam/security-credentials/"+f.role:
			doc, err := json.Marshal(map[string]any{
				"Code":            "Success",
				"AccessKeyId":     keyID,
				"SecretAccessKey": testSecret,
				"Token":           "imds-session",
				"Expiration":      expires.UTC().Format(time.RFC3339),
			})
			if err != nil {
				t.Errorf("encode the credential document: %v", err)

				return
			}

			write(t, w, string(doc))

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	t.Cleanup(f.Close)

	return f
}

// The instance role is the preferred way to run this backend: the alternative is
// a credential that never expires sitting in a unit file on a host that launches
// machines.
func TestTheInstanceRoleIsReadOverIMDSv2(t *testing.T) {
	imds := newFakeIMDS(t, "ASIAFROMROLEEXAMP001", time.Now().Add(time.Hour))

	src := &IMDS{Endpoint: imds.URL}

	got, err := src.Credentials(t.Context())
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}

	if got.AccessKeyID != "ASIAFROMROLEEXAMP001" || got.SessionToken != "imds-session" {
		t.Errorf("credentials = %+v, want the role's", got)
	}

	if imds.tokens != 1 {
		t.Errorf("issued %d session tokens, want exactly 1; billet must not fall back to "+
			"unauthenticated metadata reads", imds.tokens)
	}
}

// A HOST WITH IMDSv2 TURNED OFF IS REFUSED, NOT DOWNGRADED.
//
// v1 is an unauthenticated GET, so anything that can make this process issue a
// request to an attacker-chosen URL reads the instance role's credentials out of
// it. v2 needs a PUT first, which a request-forgery primitive generally cannot
// perform — so a fallback would hand back exactly the property v2 exists for.
//
// The fake here SERVES CREDENTIALS to an unauthenticated GET, which is what makes
// this test able to fail: a billet that downgrades succeeds, and success is what
// the assertion refuses. Asserting merely that an error came back passed while a
// mutation discarding the token error survived — the error was arriving from the
// wrong place.
func TestIMDSv1IsNotAFallback(t *testing.T) {
	imds := newFakeIMDS(t, "ASIAFROMROLEEXAMP001", time.Now().Add(time.Hour))
	imds.v1Only = true

	got, err := (&IMDS{Endpoint: imds.URL}).Credentials(t.Context())
	if err == nil {
		t.Fatalf("billet fell back to unauthenticated metadata reads and got %v", got)
	}
}

// CACHED UNTIL SHORTLY BEFORE EXPIRY, and replaced before they lapse: the request
// this signs may be the RunInstances a job is waiting on, so credentials that
// expire in flight fail it.
func TestRoleCredentialsAreCachedAndReplacedBeforeTheyLapse(t *testing.T) {
	now := time.Now()
	imds := newFakeIMDS(t, "ASIAFROMROLEEXAMP001", now.Add(time.Hour))

	clock := now
	src := &IMDS{Endpoint: imds.URL, now: func() time.Time { return clock }}

	if _, err := src.Credentials(t.Context()); err != nil {
		t.Fatalf("first: %v", err)
	}

	if _, err := src.Credentials(t.Context()); err != nil {
		t.Fatalf("second: %v", err)
	}

	if imds.tokens != 1 {
		t.Errorf("imds was asked %d times for credentials that had not expired", imds.tokens)
	}

	// Inside the refresh margin, so they are replaced while there is still time
	// for a call signed with them to complete.
	clock = now.Add(time.Hour).Add(-credentialRefreshMargin).Add(time.Second)

	if _, err := src.Credentials(t.Context()); err != nil {
		t.Fatalf("third: %v", err)
	}

	if imds.tokens != 2 {
		t.Errorf("credentials inside the refresh margin were reused; imds was asked %d times",
			imds.tokens)
	}
}

// EVERY SOURCE'S REASON, not just the last one. A chain that reports only its
// final failure sends an operator to a link-local timeout when what they actually
// did was forget an environment variable.
func TestAChainWithNothingReportsWhatEachSourceSaid(t *testing.T) {
	// ALL THREE, because these read the real environment: an ambient
	// AWS_SESSION_TOKEN makes the environment source fail TERMINALLY, so the chain
	// stops before IMDS and this test passes or fails depending on whose machine it
	// runs on.
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		t.Setenv(k, "")
	}

	// AN INSTANCE WITH NO ROLE, not one whose metadata service is broken. The
	// difference is the point: a chain continues past a source that HAS nothing and
	// stops at one that failed, so a broken source here would be testing the
	// opposite property. An earlier version used a v1-only host — a terminal
	// failure — and asserted the sentinel anyway, which enshrined the meaning this
	// distinction was introduced to remove.
	imds := newFakeIMDS(t, "", time.Time{})
	imds.noRole = true

	chain := Chain{Env{}, &IMDS{Endpoint: imds.URL}}

	_, err := chain.Credentials(t.Context())
	if err == nil {
		t.Fatal("a chain with no credentials anywhere reported success")
	}

	if !errors.Is(err, errNoCredentials) {
		t.Errorf("a chain whose every source was empty did not report an absence: %v", err)
	}

	// COUNTED, NOT MERELY PRESENT, and that distinction is the assertion.
	//
	// Both sources here report an absence, and the IMDS one WRAPS the sentinel — so
	// its message alone contains "instance profile" and "no aws credentials"
	// together, and asking whether each string appears is satisfied by returning
	// only the last reason. That is exactly the implementation this test exists to
	// refuse, and an earlier version of it could not.
	//
	// Two occurrences means two sources answered.
	if got := strings.Count(err.Error(), errNoCredentials.Error()); got != 2 {
		t.Errorf("the chain's error carries %d source reasons, want one from each of its two: %v",
			got, err)
	}

	if !strings.Contains(err.Error(), "instance profile") {
		t.Errorf("the instance role's own reason was lost: %v", err)
	}
}

// A CHAIN IS ITSELF A SOURCE, SO ITS ANSWER HAS TO MEAN THE SAME THING.
//
// Default returns a Chain, so one can be nested inside
// another — and errors.Join keeps every branch reachable by errors.Is, so an inner
// chain that STOPPED at a broken source still matched errNoCredentials because an
// earlier source had reported an absence. The outer chain read that as "nothing
// here" and carried on to a different AWS identity: the same fallthrough, one
// level up, through the very function that was supposed to have closed it.
func TestANestedChainDoesNotLaunderATerminalFailureIntoAnAbsence(t *testing.T) {
	// ALL THREE, because these read the real environment: a developer with an
	// AWS_SESSION_TOKEN in their shell would make Env fail terminally
	// before IMDS was reached, and this test would then pass against the very
	// implementation it exists to refuse.
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		t.Setenv(k, "")
	}

	broken := newFakeIMDS(t, "", time.Time{})
	broken.v1Only = true

	// Empty, then broken: the inner chain must report the BREAKAGE.
	inner := Chain{Env{}, &IMDS{Endpoint: broken.URL}}

	if _, err := inner.Credentials(t.Context()); errors.Is(err, errNoCredentials) {
		t.Fatalf("a chain that stopped at a broken source reported an absence: %v", err)
	}

	// AND THE BREAKAGE REALLY CAME FROM IMDS. If the environment had failed
	// terminally instead, the assertion above would hold for the wrong reason.
	if broken.attempts == 0 {
		t.Fatal("the inner chain never reached the metadata service, so the terminal failure " +
			"under test never happened")
	}

	// And an outer chain therefore stops rather than reaching the fallback.
	fallback := &countingSource{}
	outer := Chain{inner, fallback}

	if _, err := outer.Credentials(t.Context()); err == nil {
		t.Fatal("the outer chain fell through a broken inner chain")
	}

	if fallback.calls != 0 {
		t.Errorf("the outer chain consulted its fallback %d time(s) after an inner chain "+
			"failed terminally; billet would run as an identity nobody chose", fallback.calls)
	}
}

// countingSource records whether a chain reached it.
type countingSource struct{ calls int }

func (c *countingSource) Credentials(context.Context) (awssig.Credentials, error) {
	c.calls++

	return awssig.Credentials{AccessKeyID: "AKID-FALLBACK", SecretAccessKey: "s"}, nil
}

// THE METADATA BODY IS BOUNDED, AND AN UNBOUNDED READ IS AN UNBOUNDED ALLOCATION
// DRIVEN BY WHATEVER ANSWERS.
//
// The peer is a link-local address billet has not authenticated. On a real instance
// it is the metadata service; on a host where something else holds that address, or
// where a test endpoint is configured, it is whatever is listening. A credential
// document is well under a kilobyte, so the reader is capped — and nothing tested
// the cap, so removing it changed no test.
//
// WHAT THIS PROVES IS THE BOUND, NOT THAT EVERY OVERSIZED DOCUMENT IS REFUSED, and
// the distinction is worth stating because the weaker claim is the one that is true.
// A response whose JSON is COMPLETE within the first 64 KiB and padded after it is
// still accepted, correctly — billet read a whole document and stopped. The property
// implemented here is bounded allocation.
//
// The fixture puts the closing brace PAST the cap, so an unbounded read parses it
// happily and returns a working credential — that is the mutation, and success is
// what this refuses — while a bounded read truncates mid-document and the parse
// fails.
func TestAnOversizedMetadataDocumentIsNotReadWhole(t *testing.T) {
	const padding = 128 << 10

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			write(t, w, "imds-session-token")

		case r.URL.Path == "/latest/meta-data/iam/security-credentials/":
			write(t, w, "billet-node-role\n")

		default:
			// A VALID document, padded past the cap with whitespace the JSON grammar
			// allows, so the only thing distinguishing the two behaviours is how much
			// of it billet was willing to read.
			//
			// NOT THE CHECKED write() HELPER, and that is the point of this handler
			// rather than an oversight. Everywhere else a failed write means the fake
			// stopped talking and should fail the test; HERE the client is SUPPOSED to
			// stop reading at the cap, so the rest of the body legitimately meets a
			// closed pipe. Using the checked helper made this test fail intermittently
			// — for the very behaviour it exists to prove.
			doc := `{"Code":"Success","AccessKeyId":"ASIAPADDEDEXAMPLE001","SecretAccessKey":"` +
				testSecret + `","Token":"tok","Expiration":"` +
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"` +
				strings.Repeat(" ", padding) + "}"

			//nolint:errcheck // a bounded reader closing the pipe is the subject
			_, _ = io.WriteString(w, doc)
		}
	}))
	defer srv.Close()

	src := &IMDS{Endpoint: srv.URL, HTTP: srv.Client()}

	got, err := src.Credentials(t.Context())
	if err == nil {
		t.Fatalf("a %d-byte metadata document was read whole and accepted as a credential: %v",
			padding, got)
	}

	// AND THE DOCUMENT IS NOT IN THE ERROR, which is the rule one test below: the
	// body on this path IS the credential, padding or not.
	if strings.Contains(err.Error(), testSecret) {
		t.Errorf("the error carries the secret out of the oversized document: %v", err)
	}
}

// THE CREDENTIAL DOCUMENT IS NEVER IN AN ERROR. It is the credential, and one
// shared error format string is how it reaches a log.
func TestAMalformedCredentialDocumentIsNotEchoed(t *testing.T) {
	const leaked = "SECRETTHATMUSTNOTAPPEAR"

	// EACH DOCUMENT PUTS THE MARKER WHERE THAT PATH WOULD ACTUALLY RENDER IT, which
	// the first version of this test did not. It damaged the JSON SYNTAX and put the
	// marker in an earlier field — but encoding/json reports the offending syntax, not
	// a field it had already read, so restoring the `%w` around the decoder's error
	// left the test green. A secrecy test whose marker cannot reach the error it is
	// about is the vacuous case.
	for name, doc := range map[string]string{
		// The decoder quotes the value it could not parse, so the marker has to BE
		// that value. time.Time's unmarshal is the reachable one: every other field
		// here is a string and takes anything.
		"an unparseable expiration": `{"Code":"Success","AccessKeyId":"ASIAMALFORMEDEXAMP01",` +
			`"SecretAccessKey":"s","Token":"t","Expiration":"` + leaked + `"}`,

		// And the status code, which billet used to render whenever it LOOKED like a
		// status — short and alphabetic, which this marker is.
		"a status code": `{"Code":"` + leaked + `","AccessKeyId":"ASIAMALFORMEDEXAMP01",` +
			`"SecretAccessKey":"s","Token":"t","Expiration":"2030-01-01T00:00:00Z"}`,

		// The original shape too: broken syntax with the secret ahead of the break.
		"broken syntax": `{"SecretAccessKey":"` + leaked + `", this is not json`,

		// AND AN ACCESS KEY ID THAT IS NOT ONE. It is the field every redaction here
		// deliberately SHOWS, so it is bounded to AWS's shape before it can be —
		// lowercase makes this one fail that check, and the refusal must not quote it.
		//
		// WHAT THIS DOES NOT PROVE, and the residual is recorded beside the check
		// itself: a hostile responder can still choose 16 to 128 UPPERCASE alphanumeric
		// characters and have them rendered, because that is what an access key id
		// looks like and billet has to print it for the rendering to be worth anything.
		// The bound narrows the channel; it does not close it, and no shape check
		// could.
		"an access key id that is not one": `{"Code":"Success","AccessKeyId":"` +
			strings.ToLower(leaked) +
			`","SecretAccessKey":"s","Token":"t","Expiration":"2030-01-01T00:00:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPut:
					write(t, w, "t")
				case strings.HasSuffix(r.URL.Path, "security-credentials/"):
					write(t, w, "role\n")
				default:
					write(t, w, doc)
				}
			}))

			t.Cleanup(srv.Close)

			_, err := (&IMDS{Endpoint: srv.URL}).Credentials(t.Context())
			if err == nil {
				t.Fatal("a document billet could not use was accepted")
			}

			for _, form := range []string{leaked, strings.ToLower(leaked)} {
				if strings.Contains(err.Error(), form) {
					t.Errorf("the error carries bytes from the credential document: %v", err)
				}
			}
		})
	}
}

// A PROFILE NAME THAT IS NOT ONE NEVER REACHES A URL PATH OR AN ERROR.
//
// THE NAME COMES OUT OF A RESPONSE BODY, and billet was putting it straight into the
// path of its next request. Whatever answers on the link-local address chooses those
// bytes, so a `/` there addresses a different resource than the profile billet thinks
// it is asking about, and a long one lands in a node's log through six different
// error strings.
//
// THE ASSERTION IS ON BOTH HALVES: the request is never made, and the refusal does
// not quote what it refused.
func TestAnInstanceProfileNameThatIsNotOneIsRefused(t *testing.T) {
	for name, profile := range map[string]string{
		"a path traversal": "../../../latest/meta-data/iam",
		"a query string":   "role?x=1",
		"an escape":        "role%2F..%2Fx",
		"a whole sentence": "SECRETTHATMUSTNOTAPPEAR and then more bytes than a role name holds",
		// SIXTY-FIVE VALID CHARACTERS, so the LENGTH bound is the only thing that can
		// refuse it. Every other case here contains a character the set rejects, which
		// left the bound untested — removing it changed no test.
		"too long, but every character allowed": strings.Repeat("a", 65),
		// EVERY CHARACTER HERE IS ALLOWED TOO, and these are the ones the character
		// set alone does not settle: a dot-only segment is a path segment.
		"the current directory": ".",
		"the parent directory":  "..",
	} {
		t.Run(name, func(t *testing.T) {
			var asked []string

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPut:
					write(t, w, "t")
				case strings.HasSuffix(r.URL.Path, "security-credentials/"):
					write(t, w, profile+"\n")
				default:
					asked = append(asked, r.URL.Path)
					write(t, w, "{}")
				}
			}))

			t.Cleanup(srv.Close)

			_, err := (&IMDS{Endpoint: srv.URL}).Credentials(t.Context())
			if err == nil {
				t.Fatal("a profile name that is not one was accepted")
			}

			if len(asked) != 0 {
				t.Errorf("billet built a credential request from it and asked for %q", asked)
			}

			// NOT "does it contain the whole thing", which a rendering of any PREFIX
			// or slice of it would pass. The production refusal is fixed text billet
			// wrote, so the assertion is that it is exactly that — which admits no
			// portion of the input at all.
			const refusal = "awscreds: imds answered the instance-profile listing with " +
				"something that is not a role name; billet will not put it in a request " +
				"path, and does not render it"

			if err.Error() != refusal {
				t.Errorf("the refusal is not billet's fixed text, so bytes from the "+
					"listing may be in it: %v", err)
			}
		})
	}
}

// AND AN ORDINARY ROLE NAME IS STILL ACCEPTED, in every character AWS allows.
//
// The other direction, because a guard that refuses correct input is the failure
// ADR-005 names: an operator whose role is called `billet-node_role.v2@2024` would
// meet a refusal naming nothing they could act on.
func TestAnOrdinaryRoleNameIsAccepted(t *testing.T) {
	const profile = "billet-node_role.v2@2024+edge=a,b"

	f := newFakeIMDS(t, "ASIAROLEEXAMPLE00001", time.Now().Add(time.Hour))
	f.role = profile

	got, err := (&IMDS{Endpoint: f.URL, HTTP: f.Client()}).Credentials(t.Context())
	if err != nil {
		t.Fatalf("a role name using AWS's documented set was refused: %v", err)
	}

	if got.AccessKeyID != "ASIAROLEEXAMPLE00001" {
		t.Errorf("resolved %q, want the role's key", got.AccessKeyID)
	}
}

// AND EXACTLY SIXTY-FOUR CHARACTERS IS A ROLE NAME, which is the boundary the
// rejection table cannot reach.
//
// It rejects 65 and accepts a short one, so `len(s) > 64` could become `>= 64` with
// both green — and every operator whose role name is exactly at AWS's maximum would
// meet a refusal naming nothing they could act on. An off-by-one in a guard is only
// visible from the side it wrongly refuses.
func TestARoleNameAtTheDocumentedMaximumIsAccepted(t *testing.T) {
	f := newFakeIMDS(t, "ASIAMAXEXAMPLE000001", time.Now().Add(time.Hour))
	f.role = strings.Repeat("r", 64)

	if _, err := (&IMDS{Endpoint: f.URL, HTTP: f.Client()}).Credentials(t.Context()); err != nil {
		t.Fatalf("a 64-character role name — AWS's documented maximum — was refused: %v", err)
	}
}

// ONLY THE LISTING'S 404 IS AN ABSENCE. EVERY OTHER 404 STOPS THE CHAIN.
//
// "NO ROLE ATTACHED" IS THE ONE STATUS THAT LETS BILLET ACT AS SOMEBODY ELSE, so the
// three ways to arrive at a 404 have to stay separated. A token PUT that 404s means
// IMDSv2 is off — the thing billet refuses to downgrade from. A credential document
// that 404s means the profile the listing NAMED is not there. And a listing that
// REDIRECTED before 404ing is an answer about a resource billet never asked for:
// http.Client.Do follows redirects, so without comparing the final URL that one read
// as "no role attached" too.
//
// Each asserts !isAbsence, because "an error came back" is satisfied by all of them
// and says nothing about the classification that decides where the job runs.
func TestOnlyTheListings404IsAnAbsence(t *testing.T) {
	for name, handler := range map[string]func(*testing.T, http.ResponseWriter, *http.Request){
		"a 404 on the token PUT": func(t *testing.T, w http.ResponseWriter, r *http.Request) {
			t.Helper()

			if r.Method == http.MethodPut {
				w.WriteHeader(http.StatusNotFound)

				return
			}

			write(t, w, "role\n")
		},
		"a 404 on the credential document": func(t *testing.T, w http.ResponseWriter, r *http.Request) {
			t.Helper()

			switch {
			case r.Method == http.MethodPut:
				write(t, w, "t")
			case strings.HasSuffix(r.URL.Path, "security-credentials/"):
				write(t, w, "role\n")
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		},
		// A 500 ON THE LISTING IS NOT "NO ROLE ATTACHED" EITHER. It is the metadata
		// service failing, and falling past it runs billet as whatever the next source
		// resolves to — so only the ONE status may be an absence, not any status the
		// listing happens to answer with.
		"a 500 on the listing": func(t *testing.T, w http.ResponseWriter, r *http.Request) {
			t.Helper()

			if r.Method == http.MethodPut {
				write(t, w, "t")

				return
			}

			w.WriteHeader(http.StatusInternalServerError)
		},
		"a listing that redirects and then 404s": func(t *testing.T, w http.ResponseWriter, r *http.Request) {
			t.Helper()

			switch {
			case r.Method == http.MethodPut:
				write(t, w, "t")
			case strings.HasSuffix(r.URL.Path, "security-credentials/"):
				http.Redirect(w, r, "/elsewhere", http.StatusFound)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handler(t, w, r)
			}))

			t.Cleanup(srv.Close)

			// THROUGH A CHAIN, because the consequence is what this is about: an
			// absence here would let billet try the next source and run as another AWS
			// identity. The fallback records whether it was reached.
			fallback := &countingSource{}

			_, err := Chain{&IMDS{Endpoint: srv.URL}, fallback}.Credentials(t.Context())
			if err == nil {
				t.Fatal("a refused metadata request produced credentials")
			}

			if fallback.calls != 0 {
				t.Errorf("the chain fell through to another source: this 404 was read as "+
					"an absence (fallback called %d times)", fallback.calls)
			}
		})
	}
}

// A REDIRECT TO ANOTHER HOST AT THE SAME PATH IS NOT AN ANSWER ABOUT THE LISTING.
//
// THE PATH IS THE PART THAT LOOKS SUFFICIENT AND IS NOT. Comparing only the path let
// a redirect to a different authority — another host, port, scheme, or query — pass
// as "this is the resource billet asked for", and the 404 it answered with then
// became "no role attached": an ABSENCE, which sends the chain to a different AWS
// identity. The other redirect case in this file changes the path and so cannot see
// it; this one keeps the path identical and moves the host, which is the only shape
// that separates the two comparisons.
func TestARedirectToAnotherHostAtTheSamePathIsNotTheListing(t *testing.T) {
	const listing = "/latest/meta-data/iam/security-credentials/"

	// The second host answers 404 at the SAME path.
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(elsewhere.Close)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			write(t, w, "t")

			return
		}

		http.Redirect(w, r, elsewhere.URL+listing, http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	fallback := &countingSource{}

	_, err := Chain{&IMDS{Endpoint: srv.URL}, fallback}.Credentials(t.Context())
	if err == nil {
		t.Fatal("a redirected listing produced credentials")
	}

	if fallback.calls != 0 {
		t.Errorf("a 404 from another host at the same path was read as \"no role attached\", "+
			"so the chain went on to another aws identity (fallback called %d times)",
			fallback.calls)
	}
}

// AND `...` IS A ROLE NAME, which is neither special as a path segment nor invalid.
//
// The refusal covers exactly "." and ".."; an earlier version rejected every dot-only
// name, which would refuse input AWS accepts. Without this the narrower rule could be
// widened back with every test green.
func TestATripleDotRoleNameIsAccepted(t *testing.T) {
	f := newFakeIMDS(t, "ASIADOTSEXAMPLE00001", time.Now().Add(time.Hour))
	f.role = "..."

	if _, err := (&IMDS{Endpoint: f.URL, HTTP: f.Client()}).Credentials(t.Context()); err != nil {
		t.Fatalf("`...` is a legal role name and was refused: %v", err)
	}
}

// A FAILED BODY READ REPORTS BILLET'S TEXT, NOT GO'S.
//
// THE ONE PEER-TEXT PATH THAT IS WORTH SANITISING, and the reasoning is what
// separates it from the dial error beside it. Both can carry bytes a peer chose —
// Go builds the message, but a malformed chunk trailer or a truncated body puts the
// peer's shape into it. The difference is what sanitising COSTS: the dial error is
// the diagnosis for the common failure (billet is not on EC2, the link-local address
// is not routed), so replacing it would take the answer away from every operator on
// the path they actually hit. A body-read failure diagnoses nothing an operator can
// act on, so it is replaced and the identity is kept for errors.Is.
//
// The server declares more than it sends and hangs up, which is how a read fails
// without the test having to reach into the transport.
func TestAFailedBodyReadDoesNotCarryTheTransportsText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			write(t, w, "t")

			return
		}

		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		write(t, w, "short")

		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		// Hanging up mid-body is what makes io.ReadAll fail.
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
			}
		}
	}))

	t.Cleanup(srv.Close)

	_, err := (&IMDS{Endpoint: srv.URL}).Credentials(t.Context())
	if err == nil {
		t.Fatal("a truncated metadata response was accepted")
	}

	if !strings.Contains(err.Error(), "could not read the imds response") {
		t.Errorf("the failure is not reported in billet's own words: %v", err)
	}

	// AND GO'S OWN WORDING IS NOT IN IT, which is the half that would regress: the
	// transport's message is exactly what a peer can shape.
	for _, goText := range []string{"unexpected EOF", "connection reset", "http: "} {
		if strings.Contains(err.Error(), goText) {
			t.Errorf("the transport's own text reached the error (%q): %v", goText, err)
		}
	}
}

// AN INSTANCE WITH NO ROLE IS AN ABSENCE, not a failure, and it gets a sentence
// naming the two ways to fix it rather than a 404 from a path an operator has never
// heard of.
//
// THE CLASSIFICATION IS THE HALF THAT MATTERS. "No role attached" is the single most
// ordinary reason IMDS has nothing, and reading it as terminal stops a chain that
// should fall through — so this asserts isAbsence, not merely that an error came
// back. Real IMDS says so with a 404 on the listing, which the fake now answers.
func TestAnInstanceWithNoRoleSaysSo(t *testing.T) {
	// BOTH SHAPES, because billet cannot choose which one it meets. A 404 on the
	// listing is what a real instance with no profile answers; an empty 200 body is
	// what the previous fixture invented, and something in front of the metadata
	// service could still produce it. Each has to be an absence.
	for name, handler := range map[string]http.HandlerFunc{
		"a 404 on the listing, which is what the service answers": func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut {
				write(t, w, "t")

				return
			}

			w.WriteHeader(http.StatusNotFound)
		},
		"an empty listing body": func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut {
				write(t, w, "t")

				return
			}

			write(t, w, "\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			t.Cleanup(srv.Close)

			_, err := (&IMDS{Endpoint: srv.URL}).Credentials(t.Context())
			if err == nil {
				t.Fatal("an instance with no role reported credentials")
			}

			// THE CLASSIFICATION, not merely that an error came back. Read as
			// terminal, this stops a chain that should have fallen through to
			// whatever comes after the instance role.
			if !isAbsence(err) {
				t.Errorf("an instance with no role is a FAILURE rather than an absence, "+
					"so a chain stops here instead of trying the next source: %v", err)
			}

			if !strings.Contains(err.Error(), "AWS_ACCESS_KEY_ID") {
				t.Errorf("the error does not say what to do instead: %v", err)
			}
		})
	}
}

// THE SENTINEL'S MEANING HAS TO HOLD FOR EVERY SOURCE, not just the one where it
// was noticed. errNoCredentials means "nothing here, try the next"; anything else
// stops the chain. A half-filled source reporting an absence falls through to a
// different AWS identity, which is the whole failure the distinction prevents.
func TestNoSourceReportsAbsenceWhenItIsMerelyHalfConfigured(t *testing.T) {
	for name, src := range map[string]Source{
		"static with no secret": Static{AccessKeyID: "AKID"},
		"static with no key id": Static{SecretAccessKey: "s"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := src.Credentials(t.Context())
			if err == nil {
				t.Fatal("half a credential was accepted")
			}

			if errors.Is(err, errNoCredentials) {
				t.Errorf("a half-configured source reported ABSENCE, so a chain would fall "+
					"through it to another identity: %v", err)
			}
		})
	}

	// And genuinely empty still means "try the next one", or a chain stops working.
	if _, err := (Static{}).Credentials(t.Context()); !errors.Is(err, errNoCredentials) {
		t.Errorf("an empty source did not report absence, so a chain would stop at it: %v", err)
	}
}

// A CREDENTIAL DOCUMENT MISSING A FIELD FAILS IN WAYS THAT DO NOT LOOK LIKE A
// CREDENTIAL PROBLEM, which is why each one is refused rather than tolerated.
//
// No Token: temporary credentials cannot authenticate without one, so every call
// comes back 403 and nothing says why. No Expiration: billet reads the zero time
// as "never expires", caches the document for the life of the process, and starts
// failing the moment AWS rotates it — hours later, with nothing having changed.
func TestAnIncompleteIMDSDocumentIsRefusedRatherThanCached(t *testing.T) {
	for name, doc := range map[string]map[string]any{
		"no session token": {
			"Code": "Success", "AccessKeyId": "AKID", "SecretAccessKey": "s",
			"Expiration": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
		"no expiry": {
			"Code": "Success", "AccessKeyId": "AKID", "SecretAccessKey": "s", "Token": "tok",
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPut:
					write(t, w, "t")
				case strings.HasSuffix(r.URL.Path, "security-credentials/"):
					write(t, w, "role\n")
				default:
					body, err := json.Marshal(doc)
					if err != nil {
						t.Errorf("marshal: %v", err)

						return
					}

					write(t, w, string(body))
				}
			}))

			t.Cleanup(srv.Close)

			_, err := (&IMDS{Endpoint: srv.URL}).Credentials(t.Context())
			if err == nil {
				t.Fatal("an incomplete credential document was accepted")
			}

			// AND IT IS NOT AN ABSENCE, or a chain would carry on past a metadata
			// service that answered with something malformed.
			if errors.Is(err, errNoCredentials) {
				t.Errorf("a malformed document reported an absence: %v", err)
			}
		})
	}
}

// A CANCELLED LOOKUP MUST STILL LOOK CANCELLED.
//
// Closing the sentinel-laundering by flattening every error to text also flattened
// context.Canceled and context.DeadlineExceeded, which callers filter on — a rule
// this project already wrote down for the GitHub onboarding chain: identity is
// preserved wherever it can be, and only the node whose own text carries a secret
// is replaced. Here only the earlier ABSENCES need to leave the unwrap graph.
func TestATerminalFailureKeepsItsIdentityThroughTheChain(t *testing.T) {
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		t.Setenv(k, "")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// Env reports an absence, then a cancelled lookup fails terminally behind it.
	chain := Chain{Env{}, cancelledSource{}}

	_, err := chain.Credentials(ctx)
	if err == nil {
		t.Fatal("a cancelled chain reported success")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("the chain lost the cancellation, so a caller filtering on it sees an "+
			"ordinary failure: %v", err)
	}

	// And it still is not an absence, or the laundering would be back.
	if errors.Is(err, errNoCredentials) {
		t.Errorf("a cancelled chain reported an absence: %v", err)
	}
}

type cancelledSource struct{}

func (cancelledSource) Credentials(ctx context.Context) (awssig.Credentials, error) {
	return awssig.Credentials{}, fmt.Errorf("awscreds: read credentials: %w", ctx.Err())
}

// PRINTING A SOURCE MUST NOT BE ABLE TO WEDGE THE NODE.
//
// The reason this type has a String at all is so somebody can print it — and
// Credentials() holds i.mu across the whole fetch, so a redaction that read the
// cached value under the same lock would deadlock the first time anyone logged
// the source from inside that critical section. Which is exactly where a person
// debugging credential resolution would put it.
//
// Rendering it while the lock is held is the shape of that mistake, and it must
// simply return.
func TestPrintingASourceCannotDeadlock(t *testing.T) {
	src := &IMDS{Endpoint: "http://127.0.0.1:1"}

	src.mu.Lock()

	done := make(chan string, 1)

	go func() { done <- fmt.Sprintf("%v|%+v|%s", src, src, src) }()

	select {
	case got := <-done:
		if strings.Contains(got, "SecretAccessKey") {
			t.Errorf("the source rendered its cached credential's fields: %s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rendering a credential source blocked on the lock its own resolution holds; " +
			"one log line inside Credentials() would wedge the node")
	}

	src.mu.Unlock()
}

// A CHAIN RENDERS THE TYPES OF ITS SOURCES, not their values.
//
// Formatting each source recursed forever on a chain containing itself, and made
// this type's safety depend on every source redacting — including one implemented
// outside this package, which nothing here controls.
func TestAChainRendersTypesRatherThanTrustingItsSources(t *testing.T) {
	leaky := leakySource{secret: "wJalrXUtnFEMI-thisIsTheSecret"}
	chain := Chain{Env{}, leaky}

	// EVERY RENDERING PATH, NOT ONLY %v, and the difference was found by mutation
	// rather than by reading. This test used to assert on `fmt.Sprintf("%v", ...)`
	// alone, and Chain has five independent renderings — so making GoString,
	// MarshalJSON or LogValue print its members' VALUES instead of their types
	// SURVIVED, while the one path the test happened to use stayed clean.
	//
	// THE TABLE ABOVE CANNOT COVER THIS. It renders a chain holding a *IMDS, which
	// redacts itself, so printing its value leaks nothing; only a member that does
	// NOT redact can tell the two behaviours apart, and a Source implemented outside
	// this package is exactly that.
	var logged bytes.Buffer

	slog.New(slog.NewJSONHandler(&logged, nil)).Info("resolving", "chain", chain)

	encoded, err := json.Marshal(chain)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// THE VERB PATH IS THE SUBJECT, so these go through fmt rather than calling the
	// method: String() being safe says nothing about what fmt does with the slice.
	// render takes an `any` so the call site cannot be rewritten into the direct
	// call a linter suggests, which would quietly turn four of these rows into the
	// same assertion — the trick internal/provider/codebuild's table already uses.
	render := func(format string, v any) string { return fmt.Sprintf(format, v) }

	rendered := map[string]string{
		"%v":         render("%v", chain),
		"%s":         render("%s", chain),
		"%#v":        render("%#v", chain),
		"%+v":        render("%+v", chain),
		"String()":   chain.String(),
		"GoString()": chain.GoString(),
		"json":       string(encoded),
		"slog":       logged.String(),
	}

	for path, got := range rendered {
		if strings.Contains(got, leaky.secret) {
			t.Errorf("%s rendered a source that does not redact itself: %s", path, got)
		}

		// AND IT STILL SAYS WHICH SOURCES IT HOLDS, which is the whole use of
		// printing one — a rendering that leaks nothing because it says nothing
		// would pass the assertion above and be useless.
		if !strings.Contains(got, "Env") {
			t.Errorf("%s does not say which sources the chain holds: %s", path, got)
		}
	}
}

// THE ABSENCE CLASSIFIER, SHAPE BY SHAPE.
//
// THIS ONE PREDICATE DECIDES WHICH AWS IDENTITY BILLET ACTS AS. Reading a terminal
// failure as an absence lets the chain move to the next source; reading an absence
// as terminal stops a machine that simply has nothing configured. The two chain
// tests below cover the paths a Source actually takes, and this covers the error
// SHAPES — including the ones no source in this repository builds, because the
// errors arriving here come from implementations billet does not own.
//
// EVERY UNCERTAIN ANSWER IS "TERMINAL", which is the safe direction: it stops the
// chain rather than letting it act as somebody else.
//
// THERE IS NO "IMPLEMENTS BOTH UNWRAP FORMS" CASE BECAUSE GO FORBIDS ONE. A type
// cannot declare `Unwrap() error` and `Unwrap() []error` together — same method name,
// different signatures — so the order this walk checks them in can never decide
// anything, and a fixture written for it has to rename a method and then proves
// nothing. Worth stating, because "which form wins" is the first question the walk
// invites.
func TestTheAbsenceClassifierReadsEveryErrorShape(t *testing.T) {
	terminal := errors.New("a source that was configured and could not be used")

	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		// WHAT BILLET CONSTRUCTED is an absence, and nothing else is.
		"an absence billet built": {noCredentials(errNoCredentials), true},
		// The aggregate Chain seals when every source reported nothing, which is the
		// only aggregate anything here needs to recognise — and it is recognised
		// because Chain SAID so, not because this looked inside it.
		"a sealed aggregate": {noCredentials(errors.Join(
			noCredentials(errNoCredentials), noCredentials(errNoCredentials))), true},

		// NOTHING ELSE IS, and each of these was an absence under one of the four
		// inference schemes this replaced.
		"the bare sentinel":              {errNoCredentials, false},
		"a foreign wrap of an absence":   {fmt.Errorf("looked: %w", noCredentials(errNoCredentials)), false},
		"a foreign wrap of the sentinel": {fmt.Errorf("looked: %w", errNoCredentials), false},
		"an UNSEALED join of absences": {errors.Join(
			noCredentials(errNoCredentials), noCredentials(errNoCredentials)), false},

		// THE TWO LIES A WALK COULD NOT CATCH. Both unwrap to an absence — one through
		// Unwrap() error, one through Unwrap() []error — while claiming, via their own
		// Is method, to be a cancellation. Neither is billet's type, so neither is an
		// absence, and no amount of graph structure changes that.
		"a wrapper that claims to be a cancellation": {deceitfulWrap{inner: noCredentials(errNoCredentials)}, false},
		"a multi-error wrapper claiming the same":    {deceitfulJoin{inner: noCredentials(errNoCredentials)}, false},

		"nil":                {nil, false},
		"an unrelated error": {terminal, false},

		// A HOSTILE GRAPH IS NOT WALKED AT ALL, so neither of these can cost anything.
		// They were real hazards while absence was inferred — a cycle looped and sixty
		// self-joins had 2^60 paths through them — and the cycle guard and traversal
		// budget that existed for them are gone with the walk.
		"a cycle": {cyclicError(), false},
		"a small graph with exponentially many paths": {explodingJoin(60), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := isAbsence(tc.err); got != tc.want {
				t.Errorf("isAbsence(%v) = %v, want %v — %s", tc.err, got, tc.want,
					map[bool]string{
						true:  "a source that FAILED would be treated as having nothing, and the chain would act as another aws identity",
						false: "a source that has nothing would stop the chain instead of falling through",
					}[got])
			}
		})
	}
}

// explodingJoin builds an error of n objects with 2^n paths through it, every leaf
// an absence. Small to construct, and ruinous to WALK — which is why the table keeps
// it now that nothing walks: the cheapest proof that inference is gone.
func explodingJoin(n int) error {
	err := noCredentials(errNoCredentials)
	for range n {
		err = errors.Join(err, err)
	}

	return err
}

// deceitfulWrap unwraps to an absence while its own Is method claims the opposite.
//
// LEGAL GO, AND THE REASON ABSENCE IS A TYPE RATHER THAN AN INFERENCE. Any error may
// implement Is; a walk over an arbitrary graph cannot tell an honest wrap of an
// absence from a wrapper that carries a terminal condition somewhere a walk does not
// look.
type deceitfulWrap struct{ inner error }

func (deceitfulWrap) Error() string        { return "unwraps to an absence, claims to be cancelled" }
func (d deceitfulWrap) Unwrap() error      { return d.inner }
func (deceitfulWrap) Is(target error) bool { return errors.Is(target, context.Canceled) }

// deceitfulJoin is deceitfulWrap through the multi-error form, which is the shape
// that survived sealing only the top level while still looking through any Join.
type deceitfulJoin struct{ inner error }

func (deceitfulJoin) Error() string        { return "unwraps to an absence, claims to be cancelled" }
func (d deceitfulJoin) Unwrap() []error    { return []error{d.inner} }
func (deceitfulJoin) Is(target error) bool { return errors.Is(target, context.Canceled) }

// cyclicError builds an error whose unwrap chain returns to itself.
func cyclicError() error {
	c := &selfWrap{}
	c.inner = c

	return c
}

type selfWrap struct{ inner error }

func (*selfWrap) Error() string   { return "an error that unwraps to itself" }
func (s *selfWrap) Unwrap() error { return s.inner }

// A SOURCE THAT JOINS AN ABSENCE TO A TERMINAL FAILURE STOPS THE CHAIN.
//
// THE SENTINEL BEING UNEXPORTED IS NOT WHAT PROTECTS THIS, which is what the first
// version assumed. An EMPTY Chain returns it to any caller, so a Source implemented
// outside this package can hold one and join it to anything — and errors.Is walks
// every branch of a join, so `errors.Join(absence, context.Canceled)` was classified
// as an absence and the chain moved on to the NEXT AWS IDENTITY. A cancelled
// credential lookup is an ordinary way for this to fail, which is what makes the
// combination realistic rather than contrived.
//
// This is the same laundering Chain's own error construction was fixed for; fixing
// it there did nothing about an error arriving FROM a source.
func TestAJoinedTerminalFailureDoesNotReadAsAnAbsence(t *testing.T) {
	// THE ABSENCE IS OBTAINED THE WAY ANY CALLER CAN, rather than by reaching for the
	// unexported sentinel — that is the half the previous reasoning got wrong.
	_, absent := Chain{}.Credentials(t.Context())
	if absent == nil {
		t.Fatal("an empty chain reported no error, so this test cannot construct its subject")
	}

	fallback := &countingSource{}

	_, err := Chain{
		SourceFunc(func(context.Context) (awssig.Credentials, error) {
			return awssig.Credentials{}, errors.Join(absent, context.Canceled)
		}),
		fallback,
	}.Credentials(t.Context())

	if err == nil {
		t.Fatal("a source that failed terminally was treated as having nothing")
	}

	if fallback.calls != 0 {
		t.Errorf("the chain moved on to another source after a terminal failure, which is "+
			"billet acting as an aws identity nobody chose (fallback called %d times)",
			fallback.calls)
	}

	// AND THE IDENTITY SURVIVES, because errors.Is against context.Canceled is what a
	// caller uses to tell a cancelled lookup from a broken one.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the cancellation was lost on the way out: %v", err)
	}
}

// AND A JOIN OF NOTHING-BUT-ABSENCES IS STILL AN ABSENCE.
//
// The other direction, and it is what keeps a NESTED chain working: when every inner
// source reported nothing, Chain returns errors.Join of those absences, and an outer
// chain has to read that as "nothing here" and carry on. A classifier that refused
// every join would turn a two-level chain into a hard failure on a machine that
// simply has no credentials configured.
func TestAJoinOfAbsencesIsStillAnAbsence(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")

	inner := Chain{Env{}, Env{}}

	reached := &countingSource{}

	_, err := Chain{inner, reached}.Credentials(t.Context())

	if reached.calls != 1 {
		t.Errorf("a nested chain whose sources all reported nothing did not fall through "+
			"to the next source (called %d times, want 1): %v", reached.calls, err)
	}
}

// leakySource is a Source implemented without redaction, which is what
// anything outside this package would be.
type leakySource struct{ secret string }

func (leakySource) Credentials(context.Context) (awssig.Credentials, error) {
	return awssig.Credentials{}, errNoCredentials
}
