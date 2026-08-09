// Package nodeapi is the wire between a control plane and a compute host.
//
// THE NODE DIALS OUT AND NEVER LISTENS, which is the property the whole
// deployment story rests on: a host behind NAT, on a home network, or in a
// locked-down VPC needs no inbound reachability, no port forward and no tunnel.
// GitHub's own runner works this way and so does billet's scale-set listener, so
// this is the third place in the codebase using the same shape rather than a new
// idea.
//
// That inverts the obvious client/server roles for half the traffic. The server
// has work for the node — launch this, destroy that — but cannot call it, so the
// node long-polls for commands. Everything else (registering, reading and
// writing leases) is an ordinary request in the direction the connection was
// opened.
//
// WHY NOT gRPC: billet already speaks long-poll to GitHub and already has to
// reason about its failure modes — a poll that outlives its lease TTL, a
// redelivered message, a session that ends mid-poll. Reusing that shape means
// one set of failure modes to understand instead of two, and keeps the single
// static binary free of a protobuf toolchain.
package nodeapi

import (
	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// Version is the protocol version a node announces when it registers.
//
// A node and a server are separately deployed and WILL be different builds —
// that is the point of splitting them — so the mismatch has to be a refusal with
// a readable message rather than a decode error halfway through a launch.
const Version = 1

// CommandKind names what the server is asking a node to do.
type CommandKind string

const (
	// CommandLaunch starts compute for a lease that is already durable and
	// already counted against the budget.
	CommandLaunch CommandKind = "launch"
	// CommandDestroy removes whatever a launch started for a request.
	CommandDestroy CommandKind = "destroy"
)

// RegisterRequest is a node introducing itself.
//
// Every field is a CLAIM, not a fact. The server records what a node says about
// itself and validates it against its own configuration — a node cannot talk its
// way into more capacity than the operator gave it, because the ledger's limits
// come from the server's config and never from here.
type RegisterRequest struct {
	Version  int                 `json:"version"`
	Node     string              `json:"node"`
	Provider config.ProviderKind `json:"provider"`
	// GuestOS is what this host can boot. Placement already checks it at Bind;
	// sending it lets the server refuse an obviously wrong pairing at
	// registration instead of at the first launch.
	GuestOS []config.GuestOS `json:"guest_os,omitempty"`
	// Deployment is the identity the node labels its compute with. Sent so the
	// server can refuse a node that belongs to a different installation rather
	// than accepting commands it will label unrecognisably.
	Deployment string `json:"deployment"`
}

// RegisterResponse tells a node what it needs to behave correctly.
type RegisterResponse struct {
	Version int `json:"version"`
	// LeaseTTLSeconds is how long a lease survives without a heartbeat. The node
	// needs it to pick its own renewal cadence; it is NOT free to invent one,
	// because the reaper on the other side is what enforces it.
	LeaseTTLSeconds int `json:"lease_ttl_seconds"`
	// PollSeconds bounds a command long-poll, so a node and a server agree on
	// when silence means "nothing to do" rather than "the connection is dead".
	PollSeconds int `json:"poll_seconds"`
}

// Command is one instruction, delivered in answer to a long poll.
//
// ID exists so the node can report the outcome of a specific instruction and so
// a REDELIVERY is recognisable. Redelivery is expected rather than exceptional:
// a node that dies between receiving a launch and reporting it will be told
// again, and the runner's adoption path is what makes that safe.
type Command struct {
	ID   string      `json:"id"`
	Kind CommandKind `json:"kind"`

	// Lease is carried in full for a launch rather than by id.
	//
	// The node needs vCPU, memory, guest OS and provider to start anything, and
	// fetching them separately would open a window where the lease changed
	// between the command and the read. It also keeps a launch answerable from
	// one message, which is what makes redelivery idempotent.
	Lease *alloc.Lease `json:"lease,omitempty"`
	Job   *Job         `json:"job,omitempty"`

	// RequestID is what a destroy names. Destroy is by request rather than by
	// lease because it must work for compute whose lease is already gone.
	RequestID int64 `json:"request_id,omitempty"`
}

// Job is the scale-set assignment a launch is for.
//
// Declared here rather than reusing internal/server's so the wire has its own
// stable shape: the server's struct is free to grow fields that are nobody
// else's business, and a protocol that silently follows an internal type is one
// refactor away from a breaking change nobody noticed.
type Job struct {
	RequestID int64  `json:"request_id"`
	RunID     int64  `json:"run_id"`
	Event     string `json:"event"`
}

// CommandResult reports what happened.
//
// Error is a STRING because it crosses a process boundary: the node's error
// values mean nothing on the other side, and the server's only sane responses
// are to log it and release the lease. Anything the server must branch on gets
// its own field rather than being matched out of prose.
type CommandResult struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}
