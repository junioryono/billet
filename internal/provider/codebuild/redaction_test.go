package codebuild

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/awsjson"
	"github.com/junioryono/billet/internal/awssig"
)

// theSecret and theToken are what staticCreds carries, restated here so this test's
// assertions are about a value it names rather than about a value it fetched.
const (
	theSecret = "wJalrXUtnFEMI-SECRET-DO-NOT-LEAK"
	theToken  = "SESSION-TOKEN-DO-NOT-LEAK"
)

// EVERY TYPE THAT HOLDS A CREDENTIAL REDACTS ITSELF, AND THE TEST RENDERS THE
// CONTAINER RATHER THAN THE CREDENTIAL.
//
// This is the shape of test that has missed a leak twice in this repository, and the
// reason is worth restating where somebody will read it. fmt cannot invoke methods
// through an UNEXPORTED field — reflect refuses — so a credential type's own
// redaction is never consulted when the struct around it is printed structurally. On
// the ec2 side, `%+v` on a client holding a value-typed source printed the secret
// access key in full, past three layers of redaction that all worked in isolation.
// A test that rendered only the credential type would have been green throughout.
//
// SO THE SUBJECT HERE IS THE CLIENT, and every verb is exercised because each
// ignores the others: slog's JSON handler never consults fmt, `%#v` never consults
// String, and implementing Format means fmt never consults String or GoString at all
// — which is why String and GoString are also called DIRECTLY.
func TestEveryRenderingOfTheClientHidesItsCredentials(t *testing.T) {
	// A SOURCE THAT STORES ITS SECRET, which is what makes this test able to fail
	// at all. staticCreds is an EMPTY struct — its secret exists only inside
	// Credentials() — so rendering it can never leak, and mutating client.String
	// to print `%#v` of its source SURVIVED: every rendering was clean because
	// there was nothing in the field to render. The subject of a redaction test
	// has to be able to leak.
	//
	// AND THE SOURCE IS NOW TWO LEVELS DOWN, which does not weaken this test — it
	// is the reason it still has to exist. The credential lives inside
	// awsjson.Client, which this struct holds in an UNEXPORTED field, and reflect
	// refuses to call methods through one: without this client's own five methods,
	// `%+v` walks straight past awsjson.Client's redaction into its fields. That is
	// the awscreds.IMDS trap exactly, one package over.
	c := client{
		api:      awsjson.New(service, "us-west-2", storedCreds{secret: theSecret, token: theToken}),
		endpoint: "https://codebuild.us-west-2.amazonaws.com/",
		ssm:      "https://ssm.us-west-2.amazonaws.com/",
	}

	// BOTH THE VALUE AND THE POINTER. A pointer-receiver String is not consulted
	// when a VALUE is formatted, which is the mistake the ec2 client made on its
	// first attempt — leaving `%+v` on a dereferenced client printing the secret
	// while the pointer form was clean.
	// EVERY VERB GOES THROUGH THE FORMATTER, WHICH IS WHAT IS BEING TESTED. A direct
	// c.String() proves the method; it does not prove that `%v` REACHES it, and the
	// difference is the whole defect this test exists for — the ec2 client's `%d`
	// printed its secret while its String was clean. render() takes an `any`, so the
	// call site cannot be rewritten into the String() call a linter suggests, which
	// would quietly turn four of these rows into the same assertion.
	render := func(format string, v any) string { return fmt.Sprintf(format, v) }

	renderings := map[string]string{
		"%v value":            render("%v", c),
		"%v pointer":          render("%v", &c),
		"%+v value":           render("%+v", c),
		"%+v pointer":         render("%+v", &c),
		"%#v value":           render("%#v", c),
		"%#v pointer":         render("%#v", &c),
		"%s value":            render("%s", c),
		"%s pointer":          render("%s", &c),
		"%d value":            render("%d", c),
		"%q value":            render("%q", c),
		"String() direct":     c.String(),
		"GoString() direct":   c.GoString(),
		"LogValue() direct":   c.LogValue().String(),
		"slog attr":           renderSlog(t, c),
		"slog attr pointer":   renderSlog(t, &c),
		"json.Marshal":        marshal(t, c),
		"json.Marshal ptr":    marshal(t, &c),
		"inside a struct":     fmt.Sprintf("%+v", struct{ C client }{C: c}),
		"inside a struct ptr": fmt.Sprintf("%+v", struct{ C *client }{C: &c}),
		"inside a slice":      fmt.Sprintf("%+v", []client{c}),
		"inside a map":        fmt.Sprintf("%+v", map[string]client{"c": c}),
	}

	for name, got := range renderings {
		for _, secret := range []string{theSecret, theToken} {
			if strings.Contains(got, secret) {
				t.Errorf("%s leaked a credential: %s", name, got)
			}
		}

		// AND IT STILL SAYS SOMETHING USEFUL. A redaction that renders nothing
		// turns every diagnostic about this client into a blank, so the endpoint —
		// which is an identifier rather than a secret, the same judgement the ec2
		// credentials make about an access key id — has to survive.
		if !strings.Contains(got, "codebuild") {
			t.Errorf("%s renders nothing identifying (%q), so a diagnostic about this "+
				"client would say nothing", name, got)
		}
	}
}

// THE SWEEPER IS A SECOND CONTAINER AROUND THE SAME CREDENTIAL, one struct further
// out: it holds a *client through an unexported field, and fmt cannot reach through
// that either. Same subject rule — a source that STORES its secret — and the same
// verbs, because a type added beside the client without its own methods is exactly
// how a redacted type stops being redacted.
func TestEveryRenderingOfTheSweeperHidesItsCredentials(t *testing.T) {
	s := RegistrationSweeper{
		api: &client{
			api:      awsjson.New(service, "us-west-2", storedCreds{secret: theSecret, token: theToken}),
			endpoint: "https://codebuild.us-west-2.amazonaws.com/",
			ssm:      "https://ssm.us-west-2.amazonaws.com/",
		},
		region: "us-west-2",
		path:   "/billet/jit",
	}

	render := func(format string, v any) string { return fmt.Sprintf(format, v) }

	renderings := map[string]string{
		"%v value":          render("%v", s),
		"%v pointer":        render("%v", &s),
		"%+v value":         render("%+v", s),
		"%+v pointer":       render("%+v", &s),
		"%#v value":         render("%#v", s),
		"%#v pointer":       render("%#v", &s),
		"%d value":          render("%d", s),
		"String() direct":   s.String(),
		"GoString() direct": s.GoString(),
		"LogValue() direct": s.LogValue().String(),
		"slog attr":         renderSlog(t, s),
		"slog attr pointer": renderSlog(t, &s),
		"json.Marshal":      marshal(t, s),
		"json.Marshal ptr":  marshal(t, &s),
		"inside a struct":   fmt.Sprintf("%+v", struct{ S RegistrationSweeper }{S: s}),
		"inside a slice":    fmt.Sprintf("%+v", []RegistrationSweeper{s}),
	}

	for name, got := range renderings {
		for _, secret := range []string{theSecret, theToken} {
			if strings.Contains(got, secret) {
				t.Errorf("%s leaked a credential: %s", name, got)
			}
		}

		if !strings.Contains(got, "/billet/jit") {
			t.Errorf("%s renders nothing identifying (%q)", name, got)
		}
	}
}

// AND THE TEST HAS TO BE ABLE TO FAIL.
//
// A redaction test whose subject never held a secret is the vacuous case: it passes
// against a type with no methods at all. This renders a struct that deliberately does
// NOT redact and confirms the assertion above would have caught it — which is the
// same reasoning as breaking an invariant once to prove the test has teeth, done
// inline because the "invariant" here is a method set.
func TestTheRedactionAssertionWouldCatchALeak(t *testing.T) {
	type leaky struct {
		Endpoint string
		Secret   string
	}

	got := fmt.Sprintf("%+v", leaky{
		Endpoint: "https://codebuild.us-west-2.amazonaws.com/",
		Secret:   theSecret,
	})

	if !strings.Contains(got, theSecret) {
		t.Fatal("the control case did not leak, so the assertions above prove nothing about " +
			"whether a leak would be caught")
	}
}

// A CREDENTIAL MUST NOT REACH AN ERROR EITHER, and the launch path is the one that
// carries one — so its errors are checked as well as its renderings.
func TestNoLaunchErrorRendersACredential(t *testing.T) {
	f := newFakeAWS(t)
	f.startErr = []apiFault{{status: 400, code: "InvalidInputException"}}

	p := newTestProvider(t, f, nil)

	_, err := p.Launch(t.Context(), launchSpec("billet-abc"))
	if err == nil {
		t.Fatal("the launch succeeded")
	}

	for _, secret := range []string{theSecret, theToken, theRegistration} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("a launch error rendered a credential: %v", err)
		}
	}
}

// AND THE API'S OWN PROSE IS DROPPED FROM THE ONE ACTION THAT CARRIES A CREDENTIAL
// REFERENCE.
//
// The code is from a fixed enumeration and is what an operator acts on; the message
// is free text composed by whatever answered, and StartBuild's request carries the
// whole generated buildspec. A service that echoed a rejected request back is not a
// thing to find out about from a log.
func TestALaunchErrorKeepsTheCodeAndDropsTheProse(t *testing.T) {
	f := newFakeAWS(t)
	f.startErr = []apiFault{{status: 400, code: "InvalidInputException"}}

	p := newTestProvider(t, f, nil)

	_, err := p.Launch(t.Context(), launchSpec("billet-abc"))
	if err == nil {
		t.Fatal("the launch succeeded")
	}

	if !strings.Contains(err.Error(), "InvalidInputException") {
		t.Errorf("the launch error dropped the API code an operator acts on: %v", err)
	}

	// The fake's message is the literal "scripted"; a launch error carrying it would
	// be carrying whatever the service chose to say.
	if strings.Contains(err.Error(), "scripted") {
		t.Errorf("the launch error carried the API's own message: %v", err)
	}
}

func renderSlog(t *testing.T, v any) string {
	t.Helper()

	var b strings.Builder

	log := slog.New(slog.NewJSONHandler(&b, nil))
	log.Info("rendering", "client", v)

	return b.String()
}

func marshal(t *testing.T, v any) string {
	t.Helper()

	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	return string(body)
}

// storedCreds is a source that keeps its credential in FIELDS and redacts nothing,
// as anything implemented outside billet would.
//
// IT EXISTS SO THE CLIENT'S OWN RENDERING IS WHAT IS UNDER TEST. A source that
// redacts itself, or one that holds nothing, both make every container rendering
// clean for a reason that is not the container's doing — which is the "render the
// containers" rule one level out.
type storedCreds struct {
	secret string
	token  string
}

func (c storedCreds) Credentials(context.Context) (awssig.Credentials, error) {
	return awssig.Credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: c.secret,
		SessionToken:    c.token,
	}, nil
}
