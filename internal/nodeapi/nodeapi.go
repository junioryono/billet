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
//
// VERSION 2 ADDED THE CAPACITY A NODE CONTRIBUTES, and its site. A version 1
// node reports neither, and a server that read the absent numbers as zero would
// register a host nothing can ever be placed on — silently, because a node with
// no capacity is indistinguishable from a busy one. Refusing the registration is
// the correct outcome rather than an inconvenience, which is what this bump buys.
//
// VERSION 7 CHANGED WHAT A DESTROY RESULT MEANS, WITHOUT CHANGING ITS SHAPE, and
// that is exactly why it needs a bump (#46). CommandResult.Custody is an old
// field that decodes fine in every direction; what is new is that a DESTROY may
// now set it. Both mixed pairings are unsafe, and neither produces an error:
//
//	old node -> new server   The old node reports OK on a terminate AWS merely
//	                         accepted, so the server has nothing to infer custody
//	                         from and releases the lease while the guest is still
//	                         running. The bug this version exists to fix, intact.
//	new node -> old server   The node sets Custody; the old server's destroy path
//	                         does not read it, treats the answer as an ordinary
//	                         failure, and retries — and the retry is answered by a
//	                         node that now holds the lease in custody, which the
//	                         old server again cannot see.
//
// A silent wrong answer is the case a version refusal is for. Two builds that
// disagree about what a result MEANS are as incompatible as two that disagree
// about its fields, and only one of those is visible to a decoder.
const Version = 7

// CommandKind names what the server is asking a node to do.
type CommandKind string

const (
	// CommandLaunch starts compute for a lease that is already durable and
	// already counted against the budget.
	CommandLaunch CommandKind = "launch"
	// CommandDestroy removes whatever a launch started for a request.
	CommandDestroy CommandKind = "destroy"
	// CommandSweep asks a node to destroy compute whose lease is no longer open.
	//
	// EXISTS BECAUSE REAPING IS WHAT MAKES AN ORPHAN. The control plane sweeps
	// after every reap — the lease it just terminalised is exactly what leaves a
	// container unaccounted for — and it cannot enumerate a remote host itself.
	// Without this the causal link is broken by the split: the server would reap,
	// and the node would notice minutes later on a timer of its own, if at all.
	CommandSweep CommandKind = "sweep"
	// CommandTend advances the compute a node is holding capacity for.
	//
	// The companion to sweep for custody: adopted work that has finished, and
	// discarded work whose cleanup is now confirmed. The node runs this on its own
	// cadence too; the command is what keeps it tied to the server's reap.
	CommandTend CommandKind = "tend"
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
	// Site is where this machine is, or empty in a deployment that has one place.
	Site string `json:"site,omitempty"`
	// VCPU and Memory are what this host CONTRIBUTES, which is what it detected
	// unless its own config said otherwise.
	//
	// REPORTED BY THE HOST, NOT CONFIGURED CENTRALLY, for the same reason the
	// provider is: the machine knows what it has and the person running it knows
	// what it should give, while the control plane knows neither. Required — a
	// registration carrying zero is refused, because a node that contributes
	// nothing joins the fleet, is never chosen, and produces no error to find.
	VCPU   int             `json:"vcpu"`
	Memory config.ByteSize `json:"memory"`
	// Instances are the lease ids this host is ACTUALLY RUNNING right now, taken
	// from the provider rather than from anything the plane told it.
	//
	// PROOF THAT A CONTAINER IS GONE, which nothing else can supply. A lease
	// whose holder stopped heartbeating is quarantined rather than terminalized —
	// its capacity stays charged until the compute is confirmed destroyed —
	// and the node's sweep only fires for a container that still exists. A host
	// that rebooted has none, so without this its quarantined capacity would
	// never come back. A quarantined lease missing from this list has no
	// container by definition.
	Instances []string `json:"instances,omitempty"`
	// InventoryKnown says the list above is complete rather than absent.
	//
	// AN EMPTY LIST AND A MISSING ONE MEAN OPPOSITE THINGS. A host that is
	// genuinely running nothing must be able to free the capacity its quarantined
	// leases hold — that is the reboot case, and the main reason this exists. A
	// host whose provider could not be reached knows nothing, and treating its
	// silence as "running nothing" would free capacity for containers that are
	// still there. Only a list the node vouches for is acted on.
	InventoryKnown bool `json:"inventory_known,omitempty"`
	// Deployment is the identity the node labels its compute with. Sent so the
	// server can refuse a node that belongs to a different installation rather
	// than accepting commands it will label unrecognisably.
	Deployment string `json:"deployment"`
	// Incarnation is a fresh random value for THIS node process.
	//
	// IT DISTINGUISHES A RESTART FROM A DUPLICATE, which the node name alone
	// cannot. A name is configuration and a certificate can be copied, so two
	// hosts can arrive claiming to be the same node — and once they have, the
	// control plane's answer to "whose compute is this" is a coin toss. Commands
	// go to whichever polled last, and each host's reconciliation reasons about
	// leases the other one owns.
	//
	// A restart is the benign case and looks identical at the name level: the new
	// process supersedes the old, which is gone. What separates them is whether
	// the OLD incarnation keeps talking afterwards, and that is a question only a
	// per-process value can answer.
	Incarnation string `json:"incarnation"`
}

// RegisterResponse tells a node what it needs to behave correctly.
type RegisterResponse struct {
	Version int `json:"version"`
	// Deployment is which installation answered.
	//
	// SO THE NODE CAN CHECK TOO. TLS already binds this — the node verifies the
	// server against a per-deployment CA — but a node that never looks has no way
	// to say WHICH installation it is working for, and "am I doing work for the
	// right control plane" is a question an operator asks during an incident.
	Deployment string `json:"deployment,omitempty"`
	// LeaseTTLSeconds is how long a lease survives without a heartbeat. The node
	// needs it to pick its own renewal cadence; it is NOT free to invent one,
	// because the reaper on the other side is what enforces it.
	LeaseTTLSeconds int `json:"lease_ttl_seconds"`
	// PollSeconds bounds a command long-poll, so a node and a server agree on
	// when silence means "nothing to do" rather than "the connection is dead".
	PollSeconds int `json:"poll_seconds"`
}

// HeaderIncarnation carries the node process's identity on every request.
//
// A HEADER RATHER THAN A BODY FIELD, because it has to be on requests that have
// no body — the long poll, the lease reads — and on every one of them. The
// registration is where an incarnation is CLAIMED; this is how each later
// request proves it is still the process that claimed it.
const HeaderIncarnation = "Billet-Incarnation"

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

	// Tier is the shape of the machine to start, carried so a node needs no tier
	// catalogue of its own.
	//
	// THE CONTROL PLANE IS THE ONE AUTHORITY ON WHAT A TIER IS. A node that read
	// its own copy needed that copy to match the server's, and nothing checked:
	// a missing tier refused the launch loudly, but a tier whose `image:` had
	// drifted ran the WRONG image with no error anywhere.
	Tier *TierSpec `json:"tier,omitempty"`

	// RequestID is what a destroy names. Destroy is by request rather than by
	// lease because it must work for compute whose lease is already gone.
	RequestID int64 `json:"request_id,omitempty"`
}

// RequestIDOf is the request a command concerns, wherever it is carried.
//
// A destroy names the request directly; a launch carries it inside the job. The
// two spellings exist because a destroy must work for compute whose lease is
// already gone, and asking each caller to remember which field applies is how
// one of them reads the wrong zero.
func (c Command) RequestIDOf() int64 {
	if c.Job != nil {
		return c.Job.RequestID
	}

	return c.RequestID
}

// TierSpec is everything about a tier a node needs to start an instance and to
// mint a registration for it.
//
// Only the fields the lease does not already carry. vCPU, memory, guest OS and
// the acceptable providers ride on the lease, which is the authority for them
// precisely because it was snapshotted when the reservation was made.
type TierSpec struct {
	Label string `json:"label"`
	Image string `json:"image"`
	// Command starts the runner inside the instance. Empty means the image's
	// stock entrypoint.
	Command []string `json:"command,omitempty"`
	// Disk and SHM size the instance. Zero means the backend's default.
	Disk config.ByteSize `json:"disk,omitempty"`
	SHM  config.ByteSize `json:"shm,omitempty"`
	// RunnerGroup is part of a tier's ADDRESS: resolving a scale set without one
	// silently means "the default group", so a tier deliberately placed elsewhere
	// would have its registrations refused.
	RunnerGroup string `json:"runner_group,omitempty"`
}

// TierSpecOf renders the parts of a tier that travel to a node.
func TierSpecOf(t config.Tier) *TierSpec {
	return &TierSpec{
		Label:       t.Label,
		Image:       t.Image,
		Command:     t.RunnerCommand(),
		Disk:        t.Disk,
		SHM:         t.SHM,
		RunnerGroup: t.RunnerGroup,
	}
}

// EnrollRequest asks to join a deployment.
//
// UNAUTHENTICATED BY DESIGN: a machine that has never been enrolled has no
// certificate to authenticate with. What makes it safe is that asking grants
// nothing — the request sits as `pending` until an operator compares its
// fingerprint against what the node printed and approves it.
type EnrollRequest struct {
	Node   string `json:"node"`
	CSRPEM string `json:"csr_pem"`
	// JoinToken is a short-lived credential from `billet ca token`.
	//
	// It admits nothing on its own — the request still waits for an operator to
	// compare fingerprints — but without one this endpoint is open to anyone who
	// can reach the port, who could then fill the pending list or take a name
	// before the machine that should have it.
	JoinToken string `json:"join_token"`
}

// EnrollResponse is the decision, or that there is not one yet.
type EnrollResponse struct {
	// State is pending, approved or denied.
	State string `json:"state"`
	// Fingerprint is what an operator has to compare, echoed so the node can
	// print the same value the server will show.
	Fingerprint string `json:"fingerprint"`
	// CertPEM and CAPEM are set once approved.
	CertPEM string `json:"cert_pem,omitempty"`
	CAPEM   string `json:"ca_pem,omitempty"`
}

// CAResponse is the authority a node verifies the control plane against.
//
// SERVED UNAUTHENTICATED, and that is not a leak: a CA certificate is public by
// construction — every node already has it and every TLS handshake presents the
// chain. What matters is that the node checks the FINGERPRINT it was given out
// of band before trusting what this returns.
type CAResponse struct {
	CAPEM       string `json:"ca_pem"`
	Fingerprint string `json:"fingerprint"`
	Deployment  string `json:"deployment"`
}

// RenewRequest asks the control plane to sign a new certificate for the node
// that is already authenticated on this connection.
//
// A CSR RATHER THAN A REQUEST FOR A BUNDLE, so the private key never crosses the
// wire: the node generates the key, keeps it, and asks only for a signature.
type RenewRequest struct {
	CSRPEM string `json:"csr_pem"`
}

// RenewResponse is the signed certificate and the authority that signed it.
//
// The CA travels too, because a node that renews across a CA rotation needs the
// new authority to keep verifying the control plane it is talking to.
type RenewResponse struct {
	CertPEM string `json:"cert_pem"`
	CAPEM   string `json:"ca_pem"`
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

	// Custody says the node MAY have started something and is keeping the lease.
	//
	// Its own field because the server branches on it and must not read it out of
	// the prose above. In-process this is server.ErrCustody, and the difference it
	// makes is total: a clean failure releases the lease, while custody means the
	// node has taken the lease into its own janitor, is still heartbeating it, and
	// will release it once the compute is confirmed gone. Releasing as well would
	// re-advertise capacity that a container may still be using.
	//
	// A LOST RESULT MEANS CUSTODY TOO. If the node dies between launching and
	// reporting, the server learns nothing — and "nothing" is indistinguishable
	// from "it started". The ambiguity resolves the same way it does in-process:
	// assume something is running, keep the lease, and let the node's recovery
	// adopt it. The opposite default releases capacity that is genuinely in use.
	Custody bool `json:"custody,omitempty"`
}
