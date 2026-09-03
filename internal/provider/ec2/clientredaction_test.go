package ec2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/awssig"
)

// THE STRUCT AROUND A CREDENTIAL IS A BOUNDARY TOO, and it is the one method-based
// redaction cannot cross on its own: fmt refuses to invoke methods through an
// UNEXPORTED field, so a source's own String is never consulted when the thing
// holding it is printed structurally.
//
// THIS TEST STAYS WITH THE CLIENT rather than moving to internal/awscreds with the
// chain, because the boundary it covers is THIS struct's: the credential source is
// shared now, and the unexported field holding it is not. Every package that stores
// a source needs its own methods and its own test — awscreds cannot supply either,
// which is the same reason the fourth type needed them in the first place.
func TestTheEC2ClientRedactsTheSourceItHolds(t *testing.T) {
	const secret = "wJalrXUtnFEMI-thisIsTheSecret"

	// A SOURCE THAT DOES NOT REDACT ITSELF, which is what makes this test able to
	// fail at all. With an awscreds.Static here — a type that redacts — rendering the
	// client's source leaks nothing, so mutating client.String to print it SURVIVED:
	// the member's own protection stood in for the container's, and the test could
	// not tell the two apart. That is the "render the containers" rule one level out,
	// and a CredentialSource can be implemented outside billet, which is precisely
	// the case the client's own methods exist for.
	c := &client{
		endpoint: "https://ec2.us-west-2.amazonaws.com/",
		region:   "us-west-2",
		creds:    leakyCredentials{secret: secret, token: "tok"},
	}

	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var logged bytes.Buffer

	slog.New(slog.NewJSONHandler(&logged, nil)).Info("calling", "client", c)

	encodedValue, err := json.Marshal(*c)
	if err != nil {
		t.Fatalf("marshal value: %v", err)
	}

	// BOTH FORMS. A pointer receiver is not consulted when a VALUE is formatted —
	// the rule this package's skill states — so the first version of these methods
	// left `%+v` on a dereferenced client printing the secret, and a test that
	// rendered only the pointer could not see it.
	for path, out := range map[string]string{
		"%v":              fmt.Sprintf("%v", c), //nolint:gocritic // the verb path is the subject
		"%+v":             fmt.Sprintf("%+v", c),
		"%#v":             fmt.Sprintf("%#v", c),
		"json":            string(encoded),
		"slog":            logged.String(),
		"%v on a value":   fmt.Sprintf("%v", *c), //nolint:gocritic // the verb path is the subject
		"%+v on a value":  fmt.Sprintf("%+v", *c),
		"%#v on a value":  fmt.Sprintf("%#v", *c),
		"json of a value": string(encodedValue),

		// CALLED DIRECTLY, because the verbs above never reach them. Implementing
		// Format means fmt consults Formatter for EVERY verb — %#v included — and
		// never String or GoString, so a GoString that rendered the source SURVIVED
		// this test until these two lines existed, while the method stayed reachable
		// by any caller holding a Stringer or a GoStringer. The awscreds table makes
		// the same two calls for the same reason.
		"String()":   c.String(),
		"GoString()": c.GoString(),
	} {
		for name, leaked := range map[string]string{
			"secret access key": secret,
			"session token":     "tok",
		} {
			if strings.Contains(out, leaked) {
				t.Errorf("%s rendered the %s through the client: %s", path, name, out)
			}
		}
	}
}

// leakyCredentials is a source implemented WITHOUT redaction, as anything outside
// billet would be. It exists so a container holding a source can be tested for its
// own rendering rather than for its member's.
type leakyCredentials struct {
	secret string
	token  string
}

func (l leakyCredentials) Credentials(context.Context) (awssig.Credentials, error) {
	return awssig.Credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: l.secret,
		SessionToken:    l.token,
	}, nil
}
