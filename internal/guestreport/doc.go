// Package guestreport is what billet's in-VM agent reports about the job it shares a
// guest with, and the one codec that carries it.
//
// EVERYTHING HERE IS THE GUEST'S OWN, UNVERIFIED VIEW. The job is root in its VM and
// can write any bytes the agent would, so no number in a Batch or a Report may drive
// a billet decision: not capacity, not custody, not a teardown, not a cache
// publication. It is shown to an operator, labelled as the guest's, and nothing
// else. The ledger and the plane carry an encoded Report as opaque bytes
// (lease.GuestReport, codec 1) and never decode it; the depguard rule guestview
// holds the importers of this package to the agent, the node and the two operator
// families that render it.
//
// The agent sends a Batch to its node about every ten seconds: the guest's totals
// once a second, its processes by name every two seconds, the steps the runner's log
// marked, the test results it found and its own CPU. The node keeps the batches of a
// job and, when it destroys the compute, merges them (Merge) into one Report, halves
// its resolution until it fits (Downsample) and hands the encoding on.
//
// Codec 1 is the value as JSON, compressed with DEFLATE. Decoding is strict, and
// every refusal is an *Error wrapping one of this package's sentinels; no refusal
// quotes a byte the guest wrote, because an operator reads them.
package guestreport
