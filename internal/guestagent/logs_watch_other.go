//go:build !linux

package guestagent

import (
	"context"
	"time"
)

// pagesWatch does nothing where the agent does not run: the shipper polls.
type pagesWatch struct{}

func newPagesWatch(string) *pagesWatch { return &pagesWatch{} }

func (*pagesWatch) wait(ctx context.Context, d time.Duration) { sleepFor(ctx, d) }

func (*pagesWatch) close() {}
