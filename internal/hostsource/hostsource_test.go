package hostsource

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

const leaked = "join-token-7Q2XK"

// A FIRST-BOOT PAYLOAD CARRIES THE JOIN TOKEN, so it reaches the source's API and
// nothing else: every rendering of the secret, and of a request holding it, says it is
// redacted.
func TestTheBootPayloadIsRedactedOnEveryRenderingPath(t *testing.T) {
	t.Parallel()
	secret := NewSecret([]byte(leaked))
	req := Request{Order: "o-1", Shape: "m7gd.metal", Boot: secret}

	renderings := map[string]func() string{
		"%v":         func() string { return fmt.Sprintf("%v", secret) }, //nolint:gocritic // fmt's path is what is under test
		"%+v":        func() string { return fmt.Sprintf("%+v", secret) },
		"%#v":        func() string { return fmt.Sprintf("%#v", secret) },
		"%s":         func() string { return fmt.Sprintf("%s", secret) }, //nolint:gocritic // fmt's path is what is under test
		"%x":         func() string { return fmt.Sprintf("%x", secret) },
		"%q":         func() string { return fmt.Sprintf("%q", secret) },
		"request %v": func() string { return fmt.Sprintf("%v", req) },
		"request %+v": func() string {
			return fmt.Sprintf("%+v", req)
		},
		"request %#v": func() string { return fmt.Sprintf("%#v", req) },
		"json": func() string {
			b, err := json.Marshal(req)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			return string(b)
		},
		"slog text": func() string {
			var buf bytes.Buffer
			slog.New(slog.NewTextHandler(&buf, nil)).Info("order", "boot", secret, "req", req)

			return buf.String()
		},
		"slog json": func() string {
			var buf bytes.Buffer
			slog.New(slog.NewJSONHandler(&buf, nil)).Info("order", "boot", secret, "req", req)

			return buf.String()
		},
	}
	hex := fmt.Sprintf("%x", []byte(leaked))
	for name, render := range renderings {
		out := render()
		if strings.Contains(out, leaked) || strings.Contains(out, hex) {
			t.Errorf("%s leaked the payload: %s", name, out)
		}
		if !strings.Contains(out, "redacted") {
			t.Errorf("%s did not say it redacted: %s", name, out)
		}
	}
	if got := string(secret.Reveal()); got != leaked {
		t.Fatalf("Reveal = %q, want the payload", got)
	}
}

// THE ZERO PROOF RELEASES NOTHING, and neither does a proof for another host.
func TestOnlyAProofForThisHostReleasesIt(t *testing.T) {
	t.Parallel()
	if err := (ReleaseProof{}).For("i-1"); !errors.Is(err, ErrNoProof) {
		t.Fatalf("the zero proof released a host: %v", err)
	}
	proof, err := NewReleaseProof("i-1", Drained{Host: "i-1", At: time.Unix(1, 0)}, 0, 0)
	if err != nil {
		t.Fatalf("a drained host holding nothing was refused: %v", err)
	}
	if err := proof.For("i-1"); err != nil {
		t.Fatalf("the proof did not release its own host: %v", err)
	}
	if err := proof.For("i-2"); !errors.Is(err, ErrNoProof) {
		t.Fatalf("the proof released another host: %v", err)
	}
}

// A PROOF IS BUILT ONLY FROM POSITIVE ANSWERS: a drained ending for this host, and
// nothing held. Could-not-tell is a negative count, and is refused like a holding.
func TestAProofNeedsADrainedEndingAndNothingHeld(t *testing.T) {
	t.Parallel()
	at := time.Unix(1, 0)
	for name, c := range map[string]struct {
		host            string
		drained         Drained
		leases, custody int
	}{
		"no host named":             {"", Drained{Host: "", At: at}, 0, 0},
		"no drained ending":         {"i-1", Drained{Host: "i-1"}, 0, 0},
		"another host's ending":     {"i-1", Drained{Host: "i-2", At: at}, 0, 0},
		"a lease still held":        {"i-1", Drained{Host: "i-1", At: at}, 1, 0},
		"custody still held":        {"i-1", Drained{Host: "i-1", At: at}, 0, 1},
		"leases could not be read":  {"i-1", Drained{Host: "i-1", At: at}, -1, 0},
		"custody could not be read": {"i-1", Drained{Host: "i-1", At: at}, 0, -1},
	} {
		proof, err := NewReleaseProof(c.host, c.drained, c.leases, c.custody)
		if !errors.Is(err, ErrNoProof) {
			t.Errorf("%s: built a proof (%v)", name, err)
		}
		if proof.For(c.host) == nil {
			t.Errorf("%s: the refused proof still releases %q", name, c.host)
		}
	}
}

// COULD-NOT-TELL IS THE ZERO VALUE of every answer that frees anything.
func TestTheZeroAnswersHold(t *testing.T) {
	t.Parallel()
	var p Presence
	if p != PresenceUnknown || p.String() != "unknown" {
		t.Fatalf("the zero presence is %v", p)
	}
	var s State
	if s != StateUnknown || s.String() != "unknown" {
		t.Fatalf("the zero state is %v", s)
	}
}
