package lease

import (
	"fmt"
	"time"
)

// GuestReportCodec is the one encoding of a guest report's Data this control
// plane keeps. The node writes it and the reader that decodes it is the node's
// own counterpart; the ledger never decodes it.
const GuestReportCodec = 1

// MaxGuestReportBytes bounds one lease's encoded guest report. Base64 makes it
// about 350 KiB on the wire, well inside the node wire's 1 MiB body limit.
const MaxGuestReportBytes = 256 << 10

// MaxGuestAgentVersion bounds the agent's version string, in bytes.
const MaxGuestAgentVersion = 64

// MaxGuestSchema bounds the schema number an agent may claim.
const MaxGuestSchema = 1<<16 - 1

// GuestReport is what the agent inside a job's guest told the node about the
// job, as the node collected it, carried to the ledger once when the compute is
// destroyed (VersionGuestReport).
//
// THE GUEST'S OWN, UNVERIFIED VIEW. The job is root in its VM and can say
// anything through the agent, so nothing in Data, AgentVersion or Schema may
// drive a decision about capacity, fencing, identity, custody or destruction;
// they are kept for a reader, labelled as the guest's. What the node itself
// counted (the batches it accepted and refused, the bytes it dropped, whether
// it saw the hello and the final batch, whether it restarted, and when on its
// own clock the first and last batch arrived) is the node's view of that
// channel, and is no more a measurement of the job than Data is.
//
// NEVER FOLDED INTO JobUsage, whose source is the host alone.
type GuestReport struct {
	// AgentVersion and Schema are what the agent said it is in its hello; empty
	// and zero when it said nothing.
	AgentVersion string `json:"agent_version"`
	Schema       int    `json:"schema"`
	// Codec names how Data is encoded (GuestReportCodec), and Data is the
	// agent's batches as the node encoded them, opaque to the control plane and
	// at most MaxGuestReportBytes.
	Codec int    `json:"codec"`
	Data  []byte `json:"data"`
	// Accepted and Refused count the batches the node took from the agent and
	// the ones it would not take; DroppedBytes is what it took and could not
	// keep within Data's bound.
	Accepted     int64 `json:"accepted"`
	Refused      int64 `json:"refused"`
	DroppedBytes int64 `json:"dropped_bytes"`
	// Hello says the agent introduced itself, FinalSeen that its last batch
	// arrived, and NodeRestarted that the node's process restarted while the job
	// ran, so what it holds starts there.
	Hello         bool `json:"hello"`
	FinalSeen     bool `json:"final_seen"`
	NodeRestarted bool `json:"node_restarted"`
	// FirstReceived and LastReceived are when the first and last batch arrived,
	// on the node's clock; both zero when none did.
	FirstReceived time.Time `json:"first_received"`
	LastReceived  time.Time `json:"last_received"`
}

// Validate refuses a guest report the ledger could not keep faithfully: Data
// over its bound, an encoding this control plane does not know, a version that
// is not one short line of printable ASCII, a schema out of range, a negative
// count, or arrival times that are one-sided, out of order or outside what a
// timestamp can spell.
func (g GuestReport) Validate() error {
	if g.Codec != GuestReportCodec {
		return fmt.Errorf("alloc: guest report codec %d is not one this control plane keeps", g.Codec)
	}
	if len(g.Data) > MaxGuestReportBytes {
		return fmt.Errorf("alloc: a guest report of %d bytes is over the %d byte bound",
			len(g.Data), MaxGuestReportBytes)
	}
	// NO SPACE AND NO CONTROL CHARACTER, because a reader prints it on one line
	// beside fields the guest does not control. The refusal names where, never
	// what: the guest wrote it, and a refusal travels back to the node and into
	// its log.
	if len(g.AgentVersion) > MaxGuestAgentVersion {
		return fmt.Errorf("alloc: a guest agent version of %d bytes is over the %d byte bound",
			len(g.AgentVersion), MaxGuestAgentVersion)
	}
	for i := range len(g.AgentVersion) {
		if c := g.AgentVersion[i]; c <= ' ' || c > '~' {
			return fmt.Errorf("alloc: the guest agent version is not printable ASCII at byte %d", i)
		}
	}
	if g.Schema < 0 || g.Schema > MaxGuestSchema {
		// THE RANGE, NOT THE VALUE: the guest chose it.
		return fmt.Errorf("alloc: a guest report's schema is outside 0 to %d", MaxGuestSchema)
	}
	if g.Accepted < 0 || g.Refused < 0 || g.DroppedBytes < 0 {
		return fmt.Errorf("alloc: a guest report has a negative count (accepted %d, refused %d, "+
			"dropped %d bytes)", g.Accepted, g.Refused, g.DroppedBytes)
	}

	return g.validateTimes()
}

func (g GuestReport) validateTimes() error {
	// THE WALL CLOCK IN UTC, which is all the wire and the ledger keep: a
	// monotonic reading would order two times a clock step had reversed, and
	// the report would then be kept in-process and refused once it crossed the
	// wire.
	first, last := g.FirstReceived.UTC().Round(0), g.LastReceived.UTC().Round(0)
	if first.IsZero() && last.IsZero() {
		return nil
	}
	if first.IsZero() || last.IsZero() {
		return fmt.Errorf("alloc: a guest report names one arrival time and not the other "+
			"(first %s, last %s)", first.Format(time.RFC3339Nano), last.Format(time.RFC3339Nano))
	}
	// A YEAR RFC 3339 CAN SPELL, or the wire refuses to encode it and the
	// ledger to read it back.
	for _, t := range []time.Time{first, last} {
		if y := t.Year(); y < 1 || y > 9999 {
			return fmt.Errorf("alloc: a guest report's arrival time is in the year %d", y)
		}
	}
	if first.After(last) {
		return fmt.Errorf("alloc: a guest report's first batch arrived after its last (%s after %s)",
			first.Format(time.RFC3339Nano), last.Format(time.RFC3339Nano))
	}

	return nil
}
