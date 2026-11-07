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
	drained, err := NewDrainedProof(Holdings{Host: "i-1", Sealed: time.Unix(1, 0)})
	if err != nil {
		t.Fatalf("a sealed host holding nothing was refused: %v", err)
	}
	unjoined, err := NewUnjoinedProof(Unjoined{Host: "i-1", OrderSealed: time.Unix(1, 0)})
	if err != nil {
		t.Fatalf("a sealed order that never joined was refused: %v", err)
	}
	for name, proof := range map[string]ReleaseProof{"drained": drained, "unjoined": unjoined} {
		if err := proof.For("i-1"); err != nil {
			t.Fatalf("%s: the proof did not release its own host: %v", name, err)
		}
		if err := proof.For("i-2"); !errors.Is(err, ErrNoProof) {
			t.Fatalf("%s: the proof released another host: %v", name, err)
		}
	}
}

// A DRAINED PROOF IS BUILT ONLY FROM POSITIVE ANSWERS: sealed against placement, and
// nothing held. Could-not-tell is a negative count, and is refused like a holding.
func TestADrainedProofNeedsASealAndNothingHeld(t *testing.T) {
	t.Parallel()
	at := time.Unix(1, 0)
	for name, h := range map[string]Holdings{
		"no host named":                {Sealed: at},
		"not sealed":                   {Host: "i-1"},
		"a lease still held":           {Host: "i-1", Sealed: at, Leases: 1},
		"quarantine still held":        {Host: "i-1", Sealed: at, Quarantine: 1},
		"custody still held":           {Host: "i-1", Sealed: at, Custody: 1},
		"an instance still reported":   {Host: "i-1", Sealed: at, Inventory: 1},
		"leases could not be read":     {Host: "i-1", Sealed: at, Leases: -1},
		"quarantine could not be read": {Host: "i-1", Sealed: at, Quarantine: -1},
		"custody could not be read":    {Host: "i-1", Sealed: at, Custody: -1},
		"inventory could not be read":  {Host: "i-1", Sealed: at, Inventory: -1},
	} {
		proof, err := NewDrainedProof(h)
		if !errors.Is(err, ErrNoProof) {
			t.Errorf("%s: built a proof (%v)", name, err)
		}
		if proof.For(h.Host) == nil {
			t.Errorf("%s: the refused proof still releases %q", name, h.Host)
		}
	}
}

// A HOST THAT NEVER JOINED is released without a drain, but only when its order is
// sealed against enrollment and no enrollment was ever admitted for it.
func TestAnUnjoinedProofNeedsASealedOrderAndNoAdmission(t *testing.T) {
	t.Parallel()
	at := time.Unix(1, 0)
	for name, u := range map[string]Unjoined{
		"no host named":               {OrderSealed: at},
		"order not sealed":            {Host: "i-1"},
		"an enrollment was admitted":  {Host: "i-1", OrderSealed: at, Admitted: 1},
		"admission could not be read": {Host: "i-1", OrderSealed: at, Admitted: -1},
	} {
		proof, err := NewUnjoinedProof(u)
		if !errors.Is(err, ErrNoProof) {
			t.Errorf("%s: built a proof (%v)", name, err)
		}
		if proof.For(u.Host) == nil {
			t.Errorf("%s: the refused proof still releases %q", name, u.Host)
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
