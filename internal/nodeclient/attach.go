package nodeclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/relay"
)

// attachOpenTimeout bounds reaching a guest and dialling the stream back. A session itself
// has no bound here: it ends with either side, or with this process.
const attachOpenTimeout = 20 * time.Second

// GuestAttacher is a Compute that can reach the terminal server in a lease's guest.
type GuestAttacher interface {
	OpenGuestTerminal(ctx context.Context, leaseID string) (io.ReadWriteCloser, error)
}

// DialStream dials back the stream a command asked for. THE NODE'S OWN OUTBOUND CONNECTION, on
// its identity, so nothing on this host listens for it. Authenticated as every request is, with
// the single-use token proving a command asked for this stream.
func (c *Client) DialStream(ctx context.Context, stream, token string) (io.ReadWriteCloser, error) {
	path := c.nodePath("/streams/" + url.PathEscape(stream))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("nodeclient: build %s: %w", path, err)
	}

	req.Header.Set(nodeapi.HeaderIncarnation, c.incarnation)
	req.Header.Set(nodeapi.HeaderStreamToken, token)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", nodeapi.StreamProtocol)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nodeclient: dial a stream: %w", err)
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = resp.Body.Close()

		return nil, fmt.Errorf("nodeclient: the control plane refused a stream: %s", resp.Status)
	}

	// A CANCELLED ctx NO LONGER REACHES THE CONNECTION: net/http gives an upgraded
	// connection to its caller.
	stream2, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		_ = resp.Body.Close()

		return nil, errors.New("nodeclient: an upgraded connection cannot be written to")
	}

	return stream2, nil
}

// attachGuest answers a command to reach a lease's guest: it opens the terminal, dials the
// stream back, and leaves the two joined, tracked in sessions and ended with ctx. It READS
// NOTHING OF WHAT THEY CARRY, and the answer waits only for the join.
func attachGuest(ctx context.Context, c *Client, compute Compute, cmd nodeapi.Command,
	sessions *sync.WaitGroup,
) nodeapi.CommandResult {
	res := nodeapi.CommandResult{ID: cmd.ID}

	attacher, ok := compute.(GuestAttacher)
	if !ok || cmd.Attach == nil {
		res.Error = "this node cannot open a guest's terminal"

		return res
	}

	openCtx, cancel := context.WithTimeout(ctx, attachOpenTimeout)
	defer cancel()

	guest, err := attacher.OpenGuestTerminal(openCtx, cmd.Attach.Lease)
	if err != nil {
		res.Error = err.Error()

		return res
	}

	stream, err := c.DialStream(openCtx, cmd.Attach.Stream, cmd.Attach.Token)
	if err != nil {
		_ = guest.Close()
		res.Error = err.Error()

		return res
	}

	sessions.Go(func() { relay.Bridge(ctx, guest, stream) })

	res.OK = true

	return res
}
