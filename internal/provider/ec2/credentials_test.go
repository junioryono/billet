package ec2

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testSecret = "wJalrXUtnFEMI-thisIsTheSecret"

// A SECRET ACCESS KEY IS A DURABLE CREDENTIAL FOR A WHOLE AWS ACCOUNT, and it
// reaches a log through one careless verb on a struct that happens to contain it.
//
// Every rendering path billet can reach is covered, and the reason there are so
// many is that each one ignores the others: slog's JSON handler never consults
// fmt, %#v never consults String, and a pointer-receiver String is not consulted
// when a VALUE is formatted. This is the same discipline the GitHub App key gets.
func TestCredentialsAreRedactedOnEveryRenderingPath(t *testing.T) {
	creds := Credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: testSecret,
		SessionToken:    "session-token-value",
	}

	// A struct holding them, because that is how the leak actually happens: nobody
	// prints a credential deliberately.
	holder := struct {
		Where string
		Creds Credentials
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
// `type StaticCredentials Credentials` is a DEFINED TYPE, and a defined type does
// not inherit the methods of the type it is defined from — so it had every
// exported secret field and none of the redaction, and printing one rendered the
// secret access key in full. The test above covered Credentials and said nothing
// about it.
//
// Written as a table over `any` so that a third type carrying a secret has to be
// added here to be trusted, rather than being redacted by whoever remembers.
func TestEveryCredentialTypeIsRedacted(t *testing.T) {
	const secret = "wJalrXUtnFEMI-thisIsTheSecret"

	full := Credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: secret,
		SessionToken:    "session-token-value",
	}

	for name, value := range map[string]any{
		"Credentials":       full,
		"StaticCredentials": StaticCredentials(full),
		// A POINTER, because fmt consults a value method through a pointer but not
		// the reverse, and a caller holding one is ordinary.
		"a pointer to Credentials":       &full,
		"a pointer to StaticCredentials": func() *StaticCredentials { s := StaticCredentials(full); return &s }(),
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

	imds := newFakeIMDS(t, "AKID-FROM-ROLE", time.Now().Add(time.Hour))

	chain := ChainCredentials{EnvCredentials{}, &IMDSCredentials{Endpoint: imds.URL}}

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
	t.Setenv("AWS_ACCESS_KEY_ID", "  AKID-PADDED  ")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "  "+testSecret+"  ")
	t.Setenv("AWS_SESSION_TOKEN", " tok ")

	got, err := EnvCredentials{}.Credentials(t.Context())
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}

	if got.AccessKeyID != "AKID-PADDED" || got.SecretAccessKey != testSecret {
		t.Errorf("a padded key or secret was not trimmed: %q / (secret)", got.AccessKeyID)
	}

	if got.SessionToken != " tok " {
		t.Errorf("session token = %q, want it byte for byte", got.SessionToken)
	}

	// READ ON EVERY CALL, so an operator who rewrites a service's environment is
	// not served a value from before.
	t.Setenv("AWS_ACCESS_KEY_ID", "AKID-SECOND")

	again, err := EnvCredentials{}.Credentials(t.Context())
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}

	if again.AccessKeyID != "AKID-SECOND" {
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
	for name, env := range map[string]map[string]string{
		"no secret": {"AWS_ACCESS_KEY_ID": "AKID", "AWS_SECRET_ACCESS_KEY": ""},
		"no key id": {"AWS_ACCESS_KEY_ID": "", "AWS_SECRET_ACCESS_KEY": "s"},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range env {
				t.Setenv(k, v)
			}

			err := func() error {
				_, err := (EnvCredentials{}).Credentials(t.Context())

				return err
			}()

			if err == nil {
				t.Fatal("half an environment credential was accepted")
			}

			if errors.Is(err, errNoCredentials) {
				t.Errorf("a partial environment reported ABSENCE, which lets the chain fall "+
					"through to the instance role: %v", err)
			}

			// The message has to name the variable that is missing, or an operator
			// is told only that something is wrong with credentials they can see.
			for _, want := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not name %s: %v", want, err)
				}
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

	imds := newFakeIMDS(t, "AKID-FROM-ROLE", time.Now().Add(time.Hour))

	chain := ChainCredentials{EnvCredentials{}, &IMDSCredentials{Endpoint: imds.URL}}

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

	// tokens counts how many session tokens were issued, which is how a test can
	// see whether a value was served from cache.
	tokens int
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

	f := &fakeIMDS{}

	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			if f.v1Only {
				w.WriteHeader(http.StatusNotFound)

				return
			}

			f.tokens++
			write(t, w, "imds-session-token")

		case r.Header.Get("X-Aws-Ec2-Metadata-Token") == "" && !f.v1Only:
			// V2 ONLY. An unauthenticated GET is exactly the request an SSRF can
			// make on billet's behalf, and it is what would return the role's
			// credentials — so this host refuses one, and the v1Only host below
			// deliberately does not.
			w.WriteHeader(http.StatusUnauthorized)

		case r.URL.Path == "/latest/meta-data/iam/security-credentials/":
			write(t, w, "billet-node-role\n")

		case r.URL.Path == "/latest/meta-data/iam/security-credentials/billet-node-role":
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
	imds := newFakeIMDS(t, "AKID-FROM-ROLE", time.Now().Add(time.Hour))

	src := &IMDSCredentials{Endpoint: imds.URL}

	got, err := src.Credentials(t.Context())
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}

	if got.AccessKeyID != "AKID-FROM-ROLE" || got.SessionToken != "imds-session" {
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
	imds := newFakeIMDS(t, "AKID-FROM-ROLE", time.Now().Add(time.Hour))
	imds.v1Only = true

	got, err := (&IMDSCredentials{Endpoint: imds.URL}).Credentials(t.Context())
	if err == nil {
		t.Fatalf("billet fell back to unauthenticated metadata reads and got %v", got)
	}
}

// CACHED UNTIL SHORTLY BEFORE EXPIRY, and replaced before they lapse: the request
// this signs may be the RunInstances a job is waiting on, so credentials that
// expire in flight fail it.
func TestRoleCredentialsAreCachedAndReplacedBeforeTheyLapse(t *testing.T) {
	now := time.Now()
	imds := newFakeIMDS(t, "AKID-FROM-ROLE", now.Add(time.Hour))

	clock := now
	src := &IMDSCredentials{Endpoint: imds.URL, now: func() time.Time { return clock }}

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
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")

	imds := newFakeIMDS(t, "", time.Time{})
	imds.v1Only = true

	chain := ChainCredentials{EnvCredentials{}, &IMDSCredentials{Endpoint: imds.URL}}

	_, err := chain.Credentials(t.Context())
	if err == nil {
		t.Fatal("a chain with no credentials anywhere reported success")
	}

	if !errors.Is(err, errNoCredentials) {
		t.Errorf("the environment's reason was lost from the chain's error: %v", err)
	}

	if !strings.Contains(err.Error(), "imds") {
		t.Errorf("the instance role's reason was lost from the chain's error: %v", err)
	}
}

// THE CREDENTIAL DOCUMENT IS NEVER IN AN ERROR. It is the credential, and one
// shared error format string is how it reaches a log.
func TestAMalformedCredentialDocumentIsNotEchoed(t *testing.T) {
	const leaked = "SECRET-THAT-MUST-NOT-APPEAR"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			write(t, w, "t")
		case strings.HasSuffix(r.URL.Path, "security-credentials/"):
			write(t, w, "role\n")
		default:
			write(t, w, `{"SecretAccessKey":"`+leaked+`", this is not json`)
		}
	}))

	t.Cleanup(srv.Close)

	_, err := (&IMDSCredentials{Endpoint: srv.URL}).Credentials(t.Context())
	if err == nil {
		t.Fatal("a malformed credential document was accepted")
	}

	if strings.Contains(err.Error(), leaked) {
		t.Errorf("the error carries the credential document: %v", err)
	}
}

// An instance with no role attached gets a sentence naming the two ways to fix
// it, rather than a 404 from a path an operator has never heard of.
func TestAnInstanceWithNoRoleSaysSo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			write(t, w, "t")

			return
		}

		write(t, w, "\n")
	}))

	t.Cleanup(srv.Close)

	_, err := (&IMDSCredentials{Endpoint: srv.URL}).Credentials(t.Context())
	if err == nil {
		t.Fatal("an instance with no role reported credentials")
	}

	if !strings.Contains(err.Error(), "AWS_ACCESS_KEY_ID") {
		t.Errorf("the error does not say what to do instead: %v", err)
	}
}

// THE SENTINEL'S MEANING HAS TO HOLD FOR EVERY SOURCE, not just the one where it
// was noticed. errNoCredentials means "nothing here, try the next"; anything else
// stops the chain. A half-filled source reporting an absence falls through to a
// different AWS identity, which is the whole failure the distinction prevents.
func TestNoSourceReportsAbsenceWhenItIsMerelyHalfConfigured(t *testing.T) {
	for name, src := range map[string]CredentialSource{
		"static with no secret": StaticCredentials{AccessKeyID: "AKID"},
		"static with no key id": StaticCredentials{SecretAccessKey: "s"},
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
	if _, err := (StaticCredentials{}).Credentials(t.Context()); !errors.Is(err, errNoCredentials) {
		t.Errorf("an empty source did not report absence, so a chain would stop at it: %v", err)
	}
}
