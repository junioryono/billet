package server

import "github.com/junioryono/billet/internal/dispatch"

// The dispatch vocabulary lives in internal/dispatch, so the node runtime names
// it without importing the scheduler. These aliases keep the plane's callers
// compiling while they move, and go once none is left.
type (
	Runner                     = dispatch.Runner
	CompletionAwareRunner      = dispatch.CompletionAwareRunner
	BoundCompletionAwareRunner = dispatch.BoundCompletionAwareRunner
	Job                        = dispatch.Job
	CacheAuthority             = dispatch.CacheAuthority
)

var (
	ErrCustody           = dispatch.ErrCustody
	ErrHolderUnavailable = dispatch.ErrHolderUnavailable
)
