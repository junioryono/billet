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

// Run returns at once: there is nothing to listen to.
func (*Tracker) Run(context.Context, *Accountant) {}

// Flows reports that it could not read any.
func (*Tracker) Flows(netip.Addr) ([]Flow, error) { return nil, errNoTracker }
