package alloc

import "github.com/junioryono/billet/internal/lease"

// The lease vocabulary lives in internal/lease, so the node wire and the node
// runtime can name a lease without importing the ledger writer. These aliases
// keep every existing caller compiling; #356 Phase 2 removes them once the
// callers name lease directly.
type (
	Phase            = lease.Phase
	Lease            = lease.Lease
	ImageCache       = lease.ImageCache
	ActionsCache     = lease.ActionsCache
	BuildCache       = lease.BuildCache
	BuildCaches      = lease.BuildCaches
	CacheObservation = lease.CacheObservation
	Disruption       = lease.Disruption
	JobUsage         = lease.JobUsage
	UsageSeries      = lease.UsageSeries
	JobCounters      = lease.JobCounters
	JobDestinations  = lease.JobDestinations
	JobDestination   = lease.JobDestination
	TapTotals        = lease.TapTotals
)

const (
	PhaseCapacity           = lease.PhaseCapacity
	PhaseAssigned           = lease.PhaseAssigned
	PhaseLaunching          = lease.PhaseLaunching
	PhaseOnline             = lease.PhaseOnline
	PhaseBusy               = lease.PhaseBusy
	PhaseCustody            = lease.PhaseCustody
	PhaseTeardown           = lease.PhaseTeardown
	PhaseQuarantine         = lease.PhaseQuarantine
	PhaseDone               = lease.PhaseDone
	PhaseFailed             = lease.PhaseFailed
	ImageCacheWarm          = lease.ImageCacheWarm
	ImageCacheCold          = lease.ImageCacheCold
	ImageCacheUnavailable   = lease.ImageCacheUnavailable
	ImageCacheUnused        = lease.ImageCacheUnused
	ActionsCacheServed      = lease.ActionsCacheServed
	ActionsCacheSpliced     = lease.ActionsCacheSpliced
	ActionsCacheDisabled    = lease.ActionsCacheDisabled
	ActionsCacheUnavailable = lease.ActionsCacheUnavailable
	ActionsCacheOff         = lease.ActionsCacheOff
	ActionsCacheUnused      = lease.ActionsCacheUnused
	BuildCacheWarm          = lease.BuildCacheWarm
	BuildCacheCold          = lease.BuildCacheCold
	BuildCacheDisabled      = lease.BuildCacheDisabled
	BuildCacheUnavailable   = lease.BuildCacheUnavailable
	BuildCacheUnused        = lease.BuildCacheUnused
	DisruptionNodeForgotten = lease.DisruptionNodeForgotten
	DisruptionGuestAbsent   = lease.DisruptionGuestAbsent
	DisruptionReclaimed     = lease.DisruptionReclaimed
	DisruptionHeldPastLimit = lease.DisruptionHeldPastLimit
	UsageCPU                = lease.UsageCPU
	UsageMemory             = lease.UsageMemory
	UsageOOM                = lease.UsageOOM
	UsageIO                 = lease.UsageIO
	UsageNet                = lease.UsageNet
	UsageThreads            = lease.UsageThreads
	UsagePressure           = lease.UsagePressure
	UsageEnergy             = lease.UsageEnergy
	EnergyRAPL              = lease.EnergyRAPL
	EnergyRAPLUnsplit       = lease.EnergyRAPLUnsplit
	EnergyProcess           = lease.EnergyProcess
	UsageSourceHost         = lease.UsageSourceHost
	UsageSeriesCodec        = lease.UsageSeriesCodec
	UsageSeriesCodecClocked = lease.UsageSeriesCodecClocked
	MaxUsageSeriesBytes     = lease.MaxUsageSeriesBytes
	MaxJobDestinations      = lease.MaxJobDestinations
)

// ErrLeaseNotFound means the lease does not exist, or is already terminal.
var ErrLeaseNotFound = lease.ErrLeaseNotFound
