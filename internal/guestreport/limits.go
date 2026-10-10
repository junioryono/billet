package guestreport

import "time"

// Codec is the encoding Encode and EncodeBatch write and Decode and DecodeBatch
// read: schema 1 as JSON, compressed with DEFLATE. It is the codec a
// lease.GuestReport names.
const Codec = 1

// How often the agent samples and sends. The node does not enforce these; they are
// what a reader of a Report may assume of an undownsampled one.
const (
	SampleInterval  = time.Second
	ProcessInterval = 2 * time.Second
	BatchInterval   = 10 * time.Second
)

// The encoded sizes, and what an encoding may inflate to. A Batch fits one request
// to the node; a Report is what the ledger keeps for a job.
const (
	MaxBatchBytes          = 64 << 10
	MaxReportBytes         = 256 << 10
	MaxBatchInflatedBytes  = 1 << 20
	MaxReportInflatedBytes = 16 << 20
)

// The text bounds, in bytes of UTF-8. A process is named by its comm (never its
// argv), a step by the runner's log, a suite by its result file; the agent cuts each
// with Clean before it sends.
const (
	MaxNameBytes       = 64
	MaxFailedNameBytes = 200
)

// The processes of one sample: the agent keeps KeepProcessesByName names, the
// busiest by CPU, and sums every other process into the sample's Other row. A
// decoder admits up to MaxProcessRows named rows.
const (
	KeepProcessesByName = 20
	MaxProcessRows      = 64
)

// MaxFailedNames bounds the failed test names one suite carries.
const MaxFailedNames = 50

// The bounds of one Batch's sections. Ten seconds of sampling uses ten samples and
// five process samples; the rest is room for an agent that sends late.
const (
	MaxBatchSamples        = 64
	MaxBatchProcessSamples = 32
	MaxBatchSteps          = 64
	MaxBatchSuites         = 16
)

// The bounds of one Report's sections. The samples are what Downsample halves; the
// steps, the suites and the failed names are kept whole, so their bounds are chosen
// to fit MaxReportBytes together even when no byte of them compresses
// (TestTheWholeSectionsFitAtTheirBounds), and Merge drops and counts what is past
// them.
const (
	MaxReportSamples        = 1 << 15
	MaxReportProcessSamples = 1 << 14
	MaxReportSteps          = 512
	MaxReportSuites         = 128
	MaxReportFailedNames    = 256
	MaxReportGaps           = 64
)

// MaxValue bounds every count, counter, level, time and sequence number: 2^53, the
// largest integer a JSON reader holding numbers as doubles reads exactly.
const MaxValue = 1 << 53

// MaxTicksPerSecond bounds the guest's clock tick rate (USER_HZ, 100 on Linux).
const MaxTicksPerSecond = 1_000_000

// MaxStride bounds how many times a Report's samples were halved, as the factor.
const MaxStride = 1 << 30
