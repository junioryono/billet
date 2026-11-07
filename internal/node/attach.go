package node

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/junioryono/billet/internal/guestattach"
	"github.com/junioryono/billet/internal/provider"
)

// OpenGuestTerminal connects to the terminal server in a lease's guest. What comes back
// carries the guest's bytes, which this process never reads. A guest with nothing listening
// says so, since an image built before attach and a job that stopped the server look alike,
// and neither says the guest is gone.
func (r *Runner) OpenGuestTerminal(ctx context.Context, leaseID string) (io.ReadWriteCloser, error) {
	dialer, ok := r.provider.(provider.GuestDialer)
	if !ok {
		return nil, fmt.Errorf("node: this node's %s provider cannot reach a guest's terminal", r.provider.Kind())
	}

	stream, err := dialer.DialGuest(ctx, provider.InstanceName(leaseID), guestattach.Port)
	if errors.Is(err, provider.ErrNoGuestListener) {
		return nil, errors.New("node: nothing in this job's VM is serving a terminal: its image has no attach " +
			"server, or the job stopped it")
	}

	if err != nil {
		return nil, fmt.Errorf("node: reach this job's VM: %w", err)
	}

	return stream, nil
}
