package scaleset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
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
// does. What is defensible is that it is single-use and scoped to one job.
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
