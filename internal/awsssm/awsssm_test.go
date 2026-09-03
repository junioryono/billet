package awsssm

import (
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

	"github.com/junioryono/billet/internal/awssig"
)

const (
	theSecret = "wJalrXUtnFEMI-SECRET-DO-NOT-LEAK"
	theToken  = "SESSION-TOKEN-DO-NOT-LEAK"
	theValue  = "-----BEGIN EC PRIVATE KEY-----THE-DEPLOYMENTS-OWN-KEY-----"
)

// storedCreds keeps its credential in FIELDS and redacts nothing, as anything
// implemented outside billet would. See the redaction test.
type storedCreds struct{ secret, token string }

func (c storedCreds) Credentials(context.Context) (awssig.Credentials, error) {
	return awssig.Credentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: c.secret,
		SessionToken:    c.token,
	}, nil
}

// fakeSSM is a Parameter Store that records what it was asked and answers what
// the test told it to.
type fakeSSM struct {
	t *testing.T

	targets []string
	bodies  []map[string]any

	status int
	reply  string
}

func (f *fakeSSM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("read the request body: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		f.t.Errorf("the request body is not JSON: %v", err)
	}

	f.targets = append(f.targets, r.Header.Get("X-Amz-Target"))
	f.bodies = append(f.bodies, decoded)

	status := f.status
	if status == 0 {
		status = http.StatusOK
	}

	w.WriteHeader(status)

	if _, err := io.WriteString(w, f.reply); err != nil {
		f.t.Errorf("write the reply: %v", err)
	}
}

func newFake(t *testing.T) (*fakeSSM, *Client) {
	t.Helper()

	f := &fakeSSM{t: t}
	srv := httptest.NewServer(f)

	t.Cleanup(srv.Close)

	c := New("us-west-2", storedCreds{secret: theSecret, token: theToken})
	c.endpoint = srv.URL + "/"

	return f, c
}

// A READ ASKS FOR DECRYPTION AND RETURNS THE VALUE AND ITS VERSION.
//
// WithDecryption is not optional, because every value billet stores here is a
// SecureString: a caller handed ciphertext would have to detect that and could do
// nothing useful with it.
func TestAReadDecryptsAndReportsTheVersion(t *testing.T) {
	f, c := newFake(t)
	f.reply = `{"Parameter":{"Name":"/billet/x","Value":"` + theValue + `","Version":7}}`

	got, err := c.Get(t.Context(), "/billet/x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.Value != theValue {
		t.Errorf("Value = %q, want the stored value", got.Value)
	}

	if got.Version != 7 {
		t.Errorf("Version = %d, want 7", got.Version)
	}

	if want := "AmazonSSM.GetParameter"; f.targets[0] != want {
		t.Errorf("X-Amz-Target = %q, want %q", f.targets[0], want)
	}

	if decrypt, ok := f.bodies[0]["WithDecryption"].(bool); !ok || !decrypt {
		t.Errorf("the request did not ask for decryption: %v", f.bodies[0])
	}
}

// AN ABSENT PARAMETER IS ITS OWN ERROR.
//
// Absence is an ordinary state for one caller — a deployment that has never
// published its authority — and a fault for another, so the client refuses to
// decide which and gives them a sentinel to branch on.
func TestAnAbsentParameterIsItsOwnError(t *testing.T) {
	f, c := newFake(t)
	f.status = http.StatusBadRequest
	f.reply = `{"__type":"ParameterNotFound","message":"Parameter /billet/x not found."}`

	if _, err := c.Get(t.Context(), "/billet/x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get on an absent parameter = %v, want ErrNotFound", err)
	}
}

// AN EMPTY VALUE IS NOT AN ABSENT ONE.
//
// Parameter Store refuses to store an empty string, so this is a response billet
// cannot explain rather than a state it should act on — and collapsing the two
// would let a caller install an empty authority as though nothing were there.
func TestAnEmptyValueIsRefusedRatherThanReadAsAbsent(t *testing.T) {
	f, c := newFake(t)
	f.reply = `{"Parameter":{"Name":"/billet/x","Value":"","Version":1}}`

	_, err := c.Get(t.Context(), "/billet/x")
	if err == nil {
		t.Fatal("an empty value was accepted")
	}

	if errors.Is(err, ErrNotFound) {
		t.Errorf("an empty value was reported as an absent parameter: %v", err)
	}
}

// A NO-OVERWRITE WRITE THAT FINDS THE NAME TAKEN IS ITS OWN ERROR.
//
// It is the whole point of the flag: the GitHub App private key is issued once
// and can never be re-fetched, so publishing it must never replace anything, and
// the caller has to be able to tell "it is already there" from "it failed".
func TestARefusedOverwriteIsItsOwnError(t *testing.T) {
	f, c := newFake(t)
	f.status = http.StatusBadRequest
	f.reply = `{"__type":"ParameterAlreadyExists","message":"already exists"}`

	_, err := c.Put(t.Context(), "/billet/x", theValue, PutOptions{})
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("Put over an existing parameter = %v, want ErrAlreadyExists", err)
	}
}

// A WRITE ASKS FOR A SECURESTRING AND A TIER THAT WILL HOLD THE VALUE.
//
// THE TIER IS MEASURED RATHER THAN CHOSEN. A standard parameter caps its value at
// 4096 characters, which is what stopped the CodeBuild backend running a single
// job until it was found; an authority document carrying two certificates and two
// private keys exceeds that comfortably.
func TestAWriteIsASecureStringOnATierThatWillHoldIt(t *testing.T) {
	f, c := newFake(t)
	f.reply = `{"Version":3}`

	version, err := c.Put(t.Context(), "/billet/x", theValue, PutOptions{
		Overwrite: true,
		KMSKeyID:  "alias/billet",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if version != 3 {
		t.Errorf("Put returned version %d, want 3", version)
	}

	body := f.bodies[0]

	for key, want := range map[string]any{
		"Type":      "SecureString",
		"Tier":      "Intelligent-Tiering",
		"Overwrite": true,
		"KeyId":     "alias/billet",
	} {
		if body[key] != want {
			t.Errorf("%s = %v, want %v", key, body[key], want)
		}
	}
}

// AND A WRITE THAT DEFAULTS ITS KEY DOES NOT NAME ONE.
//
// An empty KeyId is not the same request as an absent one: AWS reads a named key
// as a choice, and sending an empty string would be naming a key that does not
// exist rather than accepting the account default.
func TestAWriteWithNoKeyDoesNotNameOne(t *testing.T) {
	f, c := newFake(t)
	f.reply = `{"Version":1}`

	if _, err := c.Put(t.Context(), "/billet/x", theValue, PutOptions{}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, named := f.bodies[0]["KeyId"]; named {
		t.Errorf("a write with no configured key still named one: %v", f.bodies[0])
	}
}

// A DELETE OF SOMETHING THAT IS NOT THERE HAS PRODUCED THE OUTCOME IT WANTED.
func TestDeletingAnAbsentParameterSucceeds(t *testing.T) {
	f, c := newFake(t)
	f.status = http.StatusBadRequest
	f.reply = `{"__type":"ParameterNotFound"}`

	if err := c.Delete(t.Context(), "/billet/x"); err != nil {
		t.Fatalf("Delete on an absent parameter: %v", err)
	}
}

// A FAILED WRITE NEVER CARRIES THE API'S OWN PROSE.
//
// THE REQUEST BODY IS THE CREDENTIAL. A service that echoed a rejected request
// back — or a proxy that rendered one — is not a thing to find out about from a
// log, so the code survives and the message does not.
func TestAFailedWriteKeepsTheCodeAndDropsTheProse(t *testing.T) {
	f, c := newFake(t)
	f.status = http.StatusBadRequest
	f.reply = `{"__type":"ValidationException","message":"rejected value ` + theValue + `"}`

	_, err := c.Put(t.Context(), "/billet/x", theValue, PutOptions{})
	if err == nil {
		t.Fatal("the write succeeded")
	}

	if !strings.Contains(err.Error(), "ValidationException") {
		t.Errorf("the error dropped the code an operator acts on: %v", err)
	}

	if strings.Contains(err.Error(), theValue) {
		t.Errorf("the error carried the value the request was refused for: %v", err)
	}
}

// EVERY RENDERING OF THE CLIENT HIDES ITS CREDENTIALS.
//
// The source is two levels down — inside awsjson.Client, held here in an
// UNEXPORTED field — and reflect refuses to call methods through one, so without
// this type's own five methods `%+v` walks straight past that redaction into the
// source's fields. The awscreds.IMDS trap, two packages along.
func TestEveryRenderingOfTheSSMClientHidesItsCredentials(t *testing.T) {
	c := New("us-west-2", storedCreds{secret: theSecret, token: theToken})

	render := func(format string, v any) string { return fmt.Sprintf(format, v) }

	var logged strings.Builder

	slog.New(slog.NewJSONHandler(&logged, nil)).Info("rendering", "client", *c)

	body, err := json.Marshal(*c)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	renderings := map[string]string{
		"%v value":        render("%v", *c),
		"%v pointer":      render("%v", c),
		"%+v value":       render("%+v", *c),
		"%+v pointer":     render("%+v", c),
		"%#v value":       render("%#v", *c),
		"%s value":        render("%s", *c),
		"%d value":        render("%d", *c),
		"String() direct": c.String(),
		"GoString()":      c.GoString(),
		"LogValue()":      c.LogValue().String(),
		"slog":            logged.String(),
		"json.Marshal":    string(body),
		"inside a struct": fmt.Sprintf("%+v", struct{ C Client }{C: *c}),
		"inside a slice":  fmt.Sprintf("%+v", []Client{*c}),
	}

	for name, got := range renderings {
		for _, secret := range []string{theSecret, theToken} {
			if strings.Contains(got, secret) {
				t.Errorf("%s leaked a credential: %s", name, got)
			}
		}

		if !strings.Contains(got, "ssm") {
			t.Errorf("%s renders nothing identifying (%q)", name, got)
		}
	}
}

// A PARAMETER NAME IS A PATH, and a doubled or missing separator is a different
// parameter rather than a formatting nicety.
func TestPathForJoinsExactlyOnce(t *testing.T) {
	for _, tc := range []struct{ prefix, leaf, want string }{
		{"/billet/prod", "ca", "/billet/prod/ca"},
		{"/billet/prod/", "ca", "/billet/prod/ca"},
		{"/billet/prod", "/ca", "/billet/prod/ca"},
		{"/billet/prod/", "/ca", "/billet/prod/ca"},
	} {
		if got := PathFor(tc.prefix, tc.leaf); got != tc.want {
			t.Errorf("PathFor(%q, %q) = %q, want %q", tc.prefix, tc.leaf, got, tc.want)
		}
	}
}
