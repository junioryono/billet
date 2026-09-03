package scaleset

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	gh "github.com/actions/scaleset"
)

// The JIT config is a CREDENTIAL until it is consumed, and this type gets the
// scrutiny the App key earned the hard way.
//
// That one leaked three times in succession: through %v, then through %d once
// String was added, then through slog's JSON handler once Format was added —
// each fix correct for the verb in front of it and blind to the next. So this is
// tested against every rendering path at once rather than the one that happens
// to be in use today.
//
// "No registration token ever enters the guest" is a claim billet makes and it
// is true, but it must not be read as "no credential enters the guest". This one
// does. What is defensible is that it is single-use for one ephemeral pool
// registration and consumed before a workflow step.
func TestJITRunnerCannotBeRendered(t *testing.T) {
	const secret = "eyJzZWNyZXQtaml0LWNvbmZpZyI6dHJ1ZX0="

	runner := &JITRunner{RunnerID: 77, Name: "billet-abc123", encodedConfig: secret}

	// Both the pointer AND the value. A pointer-receiver String is not consulted
	// when a VALUE is formatted, which is exactly how the App key escaped.
	for name, subject := range map[string]any{"pointer": runner, "value": *runner} {
		t.Run(name, func(t *testing.T) {
			for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x", "%T"} {
				rendered := sprintf(verb, subject)
				if strings.Contains(rendered, secret) {
					t.Errorf("%s rendered the JIT config: %s", verb, rendered)
				}
			}

			encoded, err := json.Marshal(subject)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			if bytes.Contains(encoded, []byte(secret)) {
				t.Errorf("encoding/json rendered the JIT config: %s", encoded)
			}

			// slog's JSON handler goes through encoding/json and ignores fmt
			// entirely, which is the gap that survived two earlier fixes.
			var buf bytes.Buffer

			slog.New(slog.NewJSONHandler(&buf, nil)).Info("launching", "runner", subject)

			if strings.Contains(buf.String(), secret) {
				t.Errorf("slog JSON rendered the JIT config: %s", buf.String())
			}

			buf.Reset()

			slog.New(slog.NewTextHandler(&buf, nil)).Info("launching", "runner", subject)

			if strings.Contains(buf.String(), secret) {
				t.Errorf("slog text rendered the JIT config: %s", buf.String())
			}
		})
	}

	// And the identifying fields must SURVIVE, or the redaction has made the
	// value useless to log at all and someone will reach past it.
	if got := sprintf("%v", runner); !strings.Contains(got, "billet-abc123") {
		t.Errorf("the runner name was redacted along with the secret: %s", got)
	}

	// The config itself is still reachable where it is needed, deliberately
	// through a call rather than a field.
	if runner.Config() != secret {
		t.Error("Config() does not return the encoded config")
	}
}

func sprintf(verb string, v any) string { return fmt.Sprintf(verb, v) }

// The JIT credential must not be reachable by ANY traversal of the error chain.
//
// This exact leak has now been written twice: once in the App manifest
// conversion, and again here three weeks after it was fixed there. Both times
// Error() was sanitised and Unwrap() handed the original straight back, so the
// secret sat one errors.As away — and any reporter that walks causes and
// serialises them, which is what a structured logger does, renders it.
//
// So this asserts the PROPERTY rather than the implementation: take an error
// whose body is a live credential, and prove the credential appears in no
// rendering of it, at no verb, through no traversal. An implementation that
// sanitises the message and forgets the chain fails here.
func TestTheJITCredentialIsUnreachableThroughTheErrorChain(t *testing.T) {
	// What a proxy forwarding a 200 body under a non-200 status looks like: the
	// vendor's error text carries the whole response, and this endpoint's success
	// body IS the encoded registration.
	const credential = "eyJzZXJ2ZXJVcmwiOiJodHRwczovL2V4YW1wbGUiLCJ0b2tlbiI6IlNVUEVSU0VDUkVUIn0="

	vendor := fmt.Errorf("unexpected status 502, body: %s", credential)
	err := fmt.Errorf("scaleset: jit config for %q: %w", "runner-1", redactBody(vendor))

	renderings := map[string]string{
		"%v":            fmt.Sprintf("%v", err),
		"%s":            fmt.Sprintf("%s", err),
		"%q":            fmt.Sprintf("%q", err),
		"%#v":           fmt.Sprintf("%#v", err),
		"%+v":           fmt.Sprintf("%+v", err),
		"Error()":       err.Error(),
		"errors.Unwrap": renderChain(err),
	}

	// A structured logger serialises whatever it is given, so the JSON encoding
	// of the error is a real rendering and not a hypothetical one.
	if encoded, jsonErr := json.Marshal(struct {
		Err string `json:"err"`
	}{err.Error()}); jsonErr == nil {
		renderings["json.Marshal"] = string(encoded)
	}

	var logged bytes.Buffer

	slog.New(slog.NewJSONHandler(&logged, nil)).Error("jit failed", "error", err)
	renderings["slog"] = logged.String()

	for name, out := range renderings {
		if strings.Contains(out, credential) {
			t.Errorf("%s rendered the JIT credential: %s", name, out)
		}
	}

	// And the chain genuinely stops, rather than merely being hard to print.
	if unwrapped := errors.Unwrap(errors.Unwrap(err)); unwrapped != nil {
		t.Errorf("the redacted error still unwraps to %#v; the credential is one errors.As away",
			unwrapped)
	}

	// The redaction is still an error worth reading — a bare "[redacted]" with no
	// hint of what failed sends an operator to the wrong place.
	if !strings.Contains(err.Error(), "runner-1") {
		t.Errorf("the error names neither the runner nor the operation: %q", err.Error())
	}
}

// renderChain walks every cause and concatenates what each one says, which is
// what a reporter that "prints the full error chain" does.
func renderChain(err error) string {
	var b strings.Builder

	for err != nil {
		b.WriteString(err.Error())
		b.WriteString(" <- ")

		err = errors.Unwrap(err)
	}

	return b.String()
}

// Adoption refuses a scale set whose labels are not the ones asked for.
//
// Name and group identify a scale set; LABELS decide which runs-on values route
// to it. Adopting one with an extra label silently pulls work into a tier billet
// never advertised for, and the tier's capacity accounting is sized for its own
// labels — so the overflow lands as jobs queued behind capacity that was never
// meant for them.
func TestLabelsMustMatchBeforeAScaleSetIsAdopted(t *testing.T) {
	want := []string{"billet-4vcpu-ubuntu-2404", "self-hosted"}

	for name, tc := range map[string]struct {
		have    []string
		wantErr bool
		says    string
	}{
		"exact match":        {have: []string{"billet-4vcpu-ubuntu-2404", "self-hosted"}},
		"different order":    {have: []string{"self-hosted", "billet-4vcpu-ubuntu-2404"}},
		"missing one":        {have: []string{"self-hosted"}, wantErr: true, says: "billet-4vcpu-ubuntu-2404"},
		"one extra":          {have: []string{"billet-4vcpu-ubuntu-2404", "self-hosted", "gpu"}, wantErr: true, says: "gpu"},
		"none at all":        {have: nil, wantErr: true, says: "billet-4vcpu-ubuntu-2404"},
		"entirely different": {have: []string{"macos", "arm64"}, wantErr: true, says: "macos"},
	} {
		t.Run(name, func(t *testing.T) {
			existing := &gh.RunnerScaleSet{}
			for _, l := range tc.have {
				existing.Labels = append(existing.Labels, gh.Label{Name: l})
			}

			err := checkLabels("billet-4vcpu-ubuntu-2404", existing, want)

			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("adopted a scale set labelled %v when %v was asked for", tc.have, want)
			case !tc.wantErr && err != nil:
				t.Fatalf("refused a scale set whose labels match: %v", err)
			case tc.wantErr && !strings.Contains(err.Error(), tc.says):
				t.Errorf("the error does not name the label that differs (%q): %v", tc.says, err)
			}
		})
	}
}
