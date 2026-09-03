package awsjson

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/awssig"
)

const (
	theSecret = "wJalrXUtnFEMI-SECRET-DO-NOT-LEAK"
	theToken  = "SESSION-TOKEN-DO-NOT-LEAK"
)

// storedCreds is a source that keeps its credential in FIELDS and redacts
// nothing, as anything implemented outside billet would.
//
// IT EXISTS SO THE CLIENT'S OWN RENDERING IS WHAT IS UNDER TEST. A source that
// redacts itself, or one that holds nothing, both make every container rendering
// clean for a reason that is not the container's doing — which is the "the member
// must be able to leak" rule that cost three review findings to learn.
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

// EVERY RENDERING OF THE SHARED CLIENT HIDES ITS CREDENTIALS.
//
// THE CLIENT IS NOW THE TYPE THAT HOLDS THE SOURCE, so it is the type this rule
// attaches to. Every verb is exercised because each ignores the others: slog's
// JSON handler never consults fmt, `%#v` never consults String, and implementing
// Format means fmt never consults String or GoString at all — which is why String
// and GoString are also called DIRECTLY.
func TestEveryRenderingOfTheSharedClientHidesItsCredentials(t *testing.T) {
	c := New("codebuild", "us-west-2", storedCreds{secret: theSecret, token: theToken})

	render := func(format string, v any) string { return fmt.Sprintf(format, v) }

	renderings := map[string]string{
		"%v value":            render("%v", *c),
		"%v pointer":          render("%v", c),
		"%+v value":           render("%+v", *c),
		"%+v pointer":         render("%+v", c),
		"%#v value":           render("%#v", *c),
		"%#v pointer":         render("%#v", c),
		"%s value":            render("%s", *c),
		"%d value":            render("%d", *c),
		"%q value":            render("%q", *c),
		"String() direct":     c.String(),
		"GoString() direct":   c.GoString(),
		"LogValue() direct":   c.LogValue().String(),
		"slog attr":           renderSlog(t, *c),
		"slog attr pointer":   renderSlog(t, c),
		"json.Marshal":        marshal(t, *c),
		"json.Marshal ptr":    marshal(t, c),
		"inside a struct":     fmt.Sprintf("%+v", struct{ C Client }{C: *c}),
		"inside a struct ptr": fmt.Sprintf("%+v", struct{ C *Client }{C: c}),
		"inside a slice":      fmt.Sprintf("%+v", []Client{*c}),
		"inside a map":        fmt.Sprintf("%+v", map[string]Client{"c": *c}),
	}

	for name, got := range renderings {
		for _, secret := range []string{theSecret, theToken} {
			if strings.Contains(got, secret) {
				t.Errorf("%s leaked a credential: %s", name, got)
			}
		}

		// AND IT STILL SAYS SOMETHING USEFUL. A redaction that renders nothing
		// turns every diagnostic about this client into a blank, so the name it was
		// built with — an identifier rather than a secret — has to survive.
		if !strings.Contains(got, "codebuild") {
			t.Errorf("%s renders nothing identifying (%q), so a diagnostic about this "+
				"client would say nothing", name, got)
		}
	}
}

// AND THE ASSERTION HAS TO BE ABLE TO FAIL.
//
// A redaction test whose subject never held a secret is the vacuous case: it
// passes against a type with no methods at all.
func TestTheSharedClientRedactionAssertionWouldCatchALeak(t *testing.T) {
	type leaky struct {
		Name   string
		Secret string
	}

	got := fmt.Sprintf("%+v", leaky{Name: "codebuild", Secret: theSecret})
	if !strings.Contains(got, theSecret) {
		t.Fatal("the control case did not leak, so the assertions above prove nothing")
	}
}

func renderSlog(t *testing.T, v any) string {
	t.Helper()

	var b strings.Builder

	slog.New(slog.NewJSONHandler(&b, nil)).Info("rendering", "client", v)

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
