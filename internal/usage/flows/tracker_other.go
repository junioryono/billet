//go:build !linux

package flows

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
)

// errNoTracker says this platform has no connection tracker billet reads.
var errNoTracker = errors.New("flows: connection tracking is read only on Linux")

// Tracker is the Linux connection tracker's stand-in elsewhere: it reports
// nothing and says so.
type Tracker struct{}

// NewTracker returns a Tracker that reads nothing.
func NewTracker(*slog.Logger) *Tracker { return &Tracker{} }

// Run marks the events down and returns: there is nothing to listen to.
func (*Tracker) Run(_ context.Context, a *Accountant) { a.Down() }

// Flows reports that it could not read any.
func (*Tracker) Flows(netip.Addr) ([]Flow, error) { return nil, errNoTracker }

// Sync reports that it could not prove anything.
func (*Tracker) Sync(context.Context) error { return errNoTracker }
