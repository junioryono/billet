package nodeplane

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
)

// releasedPoolRegistrar refuses every registration as the ledger refuses one under a pool
// order whose host is sealed, released or gone.
type releasedPoolRegistrar struct {
	countingRegistrar
}

func (releasedPoolRegistrar) RegisterNode(context.Context, alloc.NodeRegistration) (int64, error) {
	return 0, fmt.Errorf("%w: node n1's pool order is sealed", alloc.ErrPoolHostReleased)
}

// A NODE WHOSE POOL HOST WAS RELEASED IS REFUSED FOR GOOD (ADR-030): nothing on it can
// resolve the refusal, so it is ErrRefused, which stops the node, rather than an outage
// it would retry forever.
func TestARegistrationUnderAReleasedPoolHostIsRefused(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithRegistrar(releasedPoolRegistrar{}))

	_, err := p.Register(t.Context(), registerAt(""))
	if !errors.Is(err, ErrRefused) || !errors.Is(err, alloc.ErrPoolHostReleased) {
		t.Fatalf("a registration under a released pool host was answered %v, want ErrRefused", err)
	}
}
