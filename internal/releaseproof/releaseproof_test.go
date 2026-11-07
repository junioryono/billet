package releaseproof

import (
	"errors"
	"testing"
	"time"
)

// A SEAL STANDS FOR ITS PROOF ONLY FOR ITS OWN HOST, and a seal naming no host or no
// moment proves nothing.
func TestASealProvesOnlyItsOwnHost(t *testing.T) {
	t.Parallel()
	proof, err := Sealed("i-1", time.Unix(1, 0))
	if err != nil || proof.For("i-1") != nil || !errors.Is(proof.For("i-2"), ErrNoProof) || proof.Host() != "i-1" {
		t.Fatalf("a seal for i-1: %v, %+v", err, proof)
	}
	for name, c := range map[string]struct {
		host string
		at   time.Time
	}{"no host": {"", time.Unix(1, 0)}, "no moment": {"i-1", time.Time{}}} {
		if p, err := Sealed(c.host, c.at); !errors.Is(err, ErrNoProof) || p.For(c.host) == nil {
			t.Errorf("%s: %v, %+v", name, err, p)
		}
	}
	if !errors.Is((Proof{}).For("i-1"), ErrNoProof) {
		t.Fatal("the zero proof released a host")
	}
}
