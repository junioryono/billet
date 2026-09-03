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
	"slices"
	"strconv"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// Version is the newest wire this build speaks, and MinVersion the oldest.
//
// A RANGE RATHER THAN A NUMBER. A node and a server are separately deployed and
// WILL be different builds — that is the point of splitting them — and an
// equality check makes the first protocol change a fleet-wide maintenance
// window: every node in the field is refused the instant the control plane is
// replaced, permanently, because a refusal is not something a node retries. So
// registration negotiates the highest version both builds speak, and a pair with
// no overlap is refused with a message naming which side to upgrade rather than
// a decode error halfway through a launch.
//
// THE FLOOR IS A PROMISE ABOUT MEANING, NOT ABOUT DECODING. Every paragraph
// below records a version whose SEMANTICS changed, and the ones under the floor
// are unsafe rather than merely old — a build must not offer to speak one just
// because the fields still parse.
//
// VERSION 2 ADDED THE CAPACITY A NODE CONTRIBUTES, and its site. A version 1
// node reports neither, and a server that read the absent numbers as zero would
// register a host nothing can ever be placed on — silently, because a node with
// no capacity is indistinguishable from a busy one. Refusing the registration is
// the correct outcome rather than an inconvenience, which is what this bump buys.
//
// VERSION 7 CHANGED WHAT A DESTROY RESULT MEANS, WITHOUT CHANGING ITS SHAPE, and
// that is exactly why it needs a bump. CommandResult.Custody is an old field
// that decodes fine in every direction; what is new is that a DESTROY may now
// set it. Both mixed pairings are unsafe, and neither produces an error:
//
//	old node -> new server   The old node reports OK on a terminate AWS merely
//	                         accepted, so the server has nothing to infer custody
//	                         from and releases the lease while the guest is still
//	                         running. The bug this version exists to fix, intact.
//	new node -> old server   The node sets Custody; a server built before this
//	                         change does not read it on a DESTROY result — it read
//	                         it only on a launch — so it treats the answer as an
//	                         ordinary failure and retries, and the retry is
//	                         answered by a node that now holds the lease in
//	                         custody, which that server again cannot see.
//
// A silent wrong answer is the case a version refusal is for. Two builds that
// disagree about what a result MEANS are as incompatible as two that disagree
// about its fields, and only one of those is visible to a decoder.
//
// VERSION 8 ADDS THE ORDERED EC2 SHAPES TO REGISTRATION AND MAKES A LEASE
// explicitly carry its requested size, charged size, and selected instance type.
// An older control plane would account only the tier while the node buys the
// shape, under-counting every lease silently, so mixed versions are refused.
//
// VERSION 9 ADDS THE BUILDKIT CACHE-MOUNT CEILING TO A LAUNCH. Silently dropping
// it would publish state outside the tier's storage policy, so mixed versions
// are refused rather than treating the field as an optional optimisation.
//
// VERSION 10 ADDS EACH EC2 SHAPE'S PRICE TO REGISTRATION. A new server paired
// with an old node would otherwise record every price as zero and understate the
// deployment-wide cost report without an error.
//
// VERSION 12 MAKES TRUST AND CACHE SCOPE PROPERTIES OF A POOL, AND CARRIES THE
// trusted pool's workflow boundary. An old server would still derive authority
// from the assignment that caused a pooled runner to launch, even though GitHub
// may give that runner another job. Mixed pairings are refused.
//
// VERSION 13 MAKES THE WIRE A NEGOTIATED RANGE, and has a registration name the
// node's release. Nothing a server sends changed, which is what makes 12 a
// version a 13 control plane can still speak rather than one it merely tolerates.
//
// VERSION 14 ADDS THE COMPUTE BARRIER: a control plane may ask a host what it is
// running, and treat a fenced, continuously empty answer as evidence that the
// host holds no compute. It is ADDITIVE — nothing a 12 or 13 pairing does
// changes — so those versions stay speakable, and what a 12/13 node cannot do is
// ANSWER. That absence is reported rather than assumed: `billet drain --wait`
// names such a host as too old to be asked and refuses to call the fleet clear
// on its behalf, because "did not answer" and "is running nothing" are the two
// facts this whole mechanism exists to keep apart.
//
// VERSION 15 ADDS THE UPGRADE COMMAND, and adds nothing a node must understand in
// order to keep working. A node below it is never SENT one — the server gates on
// VersionNodeUpgrade — so it goes on launching, destroying, sweeping and tending
// exactly as before, and converges when somebody upgrades it by hand or through
// the packaged updater. That is what keeps 12 through 14 versions a 15 control
// plane genuinely speaks rather than ones it merely tolerates, and it is why this
// change does not move MinVersion.
//
// VERSION 17 HAS A REGISTRATION NAME THE CODEBUILD FLEET A NODE DRAWS ON, and it
// is REFUSED rather than reported because it authorises capacity. A reserved
// CodeBuild fleet is one shared pool of instances, so two nodes naming it each
// advertise the whole of it and the deployment promises GitHub twice what AWS will
// run. Neither config file is wrong on its own and only the control plane can see
// both, which is why the fact travels here. MinVersion does not move: `codebuild`
// did not exist below 17, so no build in the field can register as one and omit the
// field, and every 12-through-16 pairing behaves exactly as before.
//
// VERSION 18 HAS A CODEBUILD REGISTRATION NAME THE PARAMETER STORE PATH AND
// REGION THE NODE STAGES RUNNER REGISTRATIONS IN, and it is REPORTED rather than
// refused. A registration a dead node never removed can only be deleted on the
// ledger's authority — the lease terminal, and closed longer ago than any build
// could still run — and only the control plane holds that, while only the node's
// config holds the path. So the path travels here, and the control plane sweeps
// under it. What the field decides is where the sweep LOOKS; every deletion is
// authorised per lease, so a host that sends none loses nothing but the sweep,
// and `billet status` names it as unswept rather than the fleet refusing it. Every
// 12-through-17 pairing behaves exactly as before.
//
// VERSION 16 HAS A REGISTRATION NAME THE MANIFEST THAT PRODUCED THE NODE'S
// BINARY, not just the version it was built as. A rollout resolves a channel
// once, to one immutable signed manifest, and persists that manifest's digest so
// every host installs the same bytes — but convergence was read from the version
// string, which two builds can share and which a moved tag makes identical. It is
// ADDITIVE and REPORTED rather than refused: a node below 16, or one billet did
// not install, names no manifest, and that absence is the entire installed fleet
// on the day this ships — including the hosts that would deliver the build that
// can name one. What the field changes is that a digest which DISAGREES is now a
// fact a rollout can act on, where before there was nothing to disagree with.
//
// VERSION 19 LETS A NODE WITHDRAW FROM PLACEMENT WHEN IT STOPS CLEANLY. A node
// that drains and exits used to say nothing, and nothing in the plane told a
// clean exit from a partition — so the control plane went on assigning work to
// the stopped host for a whole silence window, and each such job waited that
// window out before being placed elsewhere. Silence still means nothing: what
// this adds is a deliberate, authenticated message from the process that holds
// the name, which the plane may act on at once because the node is the authority
// on its own intent. It releases no capacity and marks no disruption; it only
// takes the host out of placement. ADDITIVE, and checked where it is EMITTED: a
// node negotiated below this simply stops as before, because an older control
// plane answers the route with a bare 404 and a node that sent anyway would log
// a decode failure on every clean stop. MinVersion does not move.
const (
	// MinVersion is the oldest wire this build still speaks.
	//
	// TWELVE IS THE FLOOR BECAUSE ELEVEN IS WRONG, not because it is old.
	// Under 11 a pooled runner's trust and cache scope come from the assignment
	// that caused it to launch, and GitHub may hand that runner a different job
	// — so a build offering to speak 11 would be offering to be unsafe. The
	// paragraphs above are the record of which versions those are; consult them
	// before ever lowering this.
	MinVersion = 12

	// Version is the newest wire this build speaks, and the one it prefers.
	Version = 19

	// VersionNodeRelease is the version from which a registration names the
	// node's release.
	//
	// WHAT IT DECIDES IS HOW AN ABSENCE READS, not whether one is refused. Below
	// it a build has no release to give, which is the whole installed fleet; at
	// or above it a build that sends none is not saying what it is. Refusing
	// would take a working host out of the fleet over a field that authorises
	// nothing, so `billet status` reports the two differently instead.
	//
	// THE FIRST ENTRY IN A TABLE THE NEXT PROTOCOL CHANGE EXTENDS. A field or a
	// command introduced above MinVersion needs a constant here and a check
	// where it is emitted or read; without one, a node that negotiated an older
	// version silently sends nothing and the far side reads that absence as a
	// zero — the whole failure mode a negotiated wire has and an equality check
	// did not. Whether that check REFUSES or merely reports is decided by what
	// the field authorises: capacity, fencing, identity, custody and destruction
	// fail closed, and a diagnostic does not.
	VersionNodeRelease = 13

	// VersionComputeBarrier is the version from which a node can answer an
	// inventory command.
	//
	// CHECKED WHERE THE COMMAND IS EMITTED, not where it is read. A node below
	// this refuses an unknown kind — correctly, since that is what a refusal is
	// for — but a refusal is not an inventory, and a plane that sent one anyway
	// would burn that host's single command slot on every barrier round for a
	// question it can never answer.
	//
	// alloc.BarrierWireVersion is the same number, held there because
	// internal/alloc cannot import this package. TestBarrierVersionsAgree pins
	// them together.
	VersionComputeBarrier = 14

	// VersionNodeUpgrade is the version from which a node can be told to replace
	// its own billet.
	//
	// GATED RATHER THAN REPORTED, because this one authorises DESTRUCTION of a
	// kind: it stops services and replaces a binary. A node that negotiated an
	// older wire has no handler for the command and would report it unknown —
	// which is the correct refusal, and the gate is what makes the server not send
	// it rather than relying on the node to say no.
	VersionNodeUpgrade = 15

	// VersionNodeDigest is the version from which a registration names the
	// manifest that produced the node's binary.
	//
	// REPORTED, NOT REFUSED, for the reason VersionNodeRelease is: it decides how
	// a rollout READS a host, not capacity, fencing, identity, custody or
	// destruction. Below it a build has no manifest to name, which is every host
	// in the field the day this ships; at or above it a build that names none has
	// no record of what installed it, which is every host installed from a package
	// or built from source. Neither is refused, and `billet status` tells the two
	// apart from a host that named one.
	VersionNodeDigest = 16

	// VersionCodeBuildFleet is the version from which a registration names the
	// reserved-capacity CodeBuild fleet a node draws on.
	//
	// REFUSED RATHER THAN REPORTED, because it authorises CAPACITY. A reserved fleet
	// has one shared pool of instances, so two nodes drawing on the same fleet each
	// advertise the whole of it and the deployment promises GitHub twice what AWS
	// will run — the overcommit escrow exists to prevent, arriving from two config
	// files neither of which is wrong on its own. Only the control plane can see
	// both, so the fleet has to be on the wire and a duplicate has to be refused.
	//
	// THE FLOOR DOES NOT MOVE FOR IT, and this is the one bump so far where that
	// needs no argument about mixed pairings: `codebuild` did not exist below this
	// version, so there is no build in the field that can register as one and omit
	// the field. Every 12-through-16 pairing behaves exactly as before.
	VersionCodeBuildFleet = 17

	// VersionCodeBuildRegistrationPath is the version from which a codebuild
	// registration names the Parameter Store path and region it stages runner
	// registrations in.
	//
	// REPORTED, NOT REFUSED, for the reason VersionNodeRelease is: the field decides
	// where the control plane's sweep of leaked registrations LOOKS, and every
	// deletion it makes is authorised per lease by the ledger — a terminal lease
	// closed longer ago than any build could still be running. A host below this
	// version, or one that sends none, therefore costs nothing but the sweep of its
	// own path, and `billet status` names it as unswept rather than taking a working
	// host out of the fleet over a field that authorises nothing on its own.
	VersionCodeBuildRegistrationPath = 18

	// VersionNodeWithdrawal is the version from which a node can tell the control
	// plane it is leaving.
	//
	// CHECKED WHERE THE MESSAGE IS SENT, like VersionComputeBarrier and for the
	// same reason: below it the route does not exist, an older control plane
	// answers it with a 404 that carries no error body, and a node that sent it
	// anyway would report a decode failure on every clean stop. A node negotiated
	// below this exits exactly as it did before, and the plane forgets it by
	// silence — which is the behaviour this version exists to shorten, not to
	// remove.
	VersionNodeWithdrawal = 19
)

// Range is the span of wire versions a build speaks, inclusive at both ends.
type Range struct {
	Min int
	Max int
}

// Self is the range this build speaks.
func Self() Range { return Range{Min: MinVersion, Max: Version} }

// Speaks reports whether a version is inside this range.
func (r Range) Speaks(v int) bool { return v >= r.Min && v <= r.Max }

// String renders a range the way both sides' diagnostics name it.
func (r Range) String() string { return strconv.Itoa(r.Min) + "-" + strconv.Itoa(r.Max) }

// DeclaredRange reads the range a peer claimed, reporting false if what it
// claimed is not a range at all.
//
// AN ABSENT MINIMUM MEANS EXACTLY max, NEVER ZERO. A build that declares no
// floor implements one version; reading the zero as "and everything down to 0"
// would record a range that peer never implemented, and the report that decides
// when an old protocol may be RETIRED is read straight off these numbers. A zero
// there says a host is holding open versions nobody has ever run.
//
// AND THERE ARE THREE ANSWERS HERE, NOT TWO. Absent, valid, and CONTRADICTORY
// are different facts, and the first version of this collapsed the third into
// the first: a peer declaring a minimum of 14 and a newest of 12 was normalised
// to "speaks exactly 12", so the server would settle on a version that peer had
// just said it does not implement — the semantically-unsafe pairing MinVersion
// exists to refuse, admitted by the function meant to describe it. A peer that
// declares no version at all is the same kind of nothing. Both are refused by
// the caller before any of registration's side effects run.
func DeclaredRange(minVersion, maxVersion int) (Range, bool) {
	if maxVersion <= 0 || minVersion > maxVersion {
		return Range{}, false
	}

	if minVersion < 0 {
		return Range{}, false
	}

	if minVersion == 0 {
		return Range{Min: maxVersion, Max: maxVersion}, true
	}

	return Range{Min: minVersion, Max: maxVersion}, true
}

// Negotiate reports the highest version both builds speak.
//
// THE BRIDGE RUNS ONE WAY, and that is a property of the wire rather than a
// policy choice. The control plane decodes request bodies with
// DisallowUnknownFields while a node decodes responses leniently, so an OLD
// node's registration is a subset of a new server's struct and decodes cleanly,
// and a new server's response decodes on an old node. The reverse does not: a
// new node's body reaches an old server's strict decoder and is rejected before
// any version check can run. So an upgrade is SERVER FIRST, a node-first attempt
// is refused, and neither side may pretend otherwise.
func Negotiate(a, b Range) (int, bool) {
	high := min(a.Max, b.Max)
	low := max(a.Min, b.Min)

	if high < low {
		return 0, false
	}

	return high, true
}

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

	// CommandUpgrade asks a node to replace its own billet with a named release.
	//
	// THE NODE DOES NOT FOLLOW A CHANNEL. The control plane resolved one to an
	// immutable target when the rollout was created and persisted its digest, so
	// what travels here is an EXACT version — otherwise every node would resolve
	// the channel separately and a fleet could end up on two releases from one
	// decision.
	//
	// IT RETURNS IMMEDIATELY. A node executes commands one at a time and each
	// command's timeout starts when it is QUEUED, so an upgrade carried out inline
	// would hold the node's single slot for as long as the drain takes — which is
	// as long as the longest job. The node execs the transactional updater
	// detached and reports that it started; where the upgrade got to is read from
	// the rollout, not from this command's result.
	CommandUpgrade CommandKind = "upgrade"

	// CommandInventory asks a node what compute it is running, right now.
	//
	// THE ONE QUESTION THE LEDGER CANNOT ANSWER. A lease that has already gone
	// takes its compute out of the control plane's view — an in-memory destroy
	// obligation, or a launch whose lease was reclaimed — but the instance keeps
	// the name billet gave it, so the host's own provider still lists it.
	//
	// IT IS ASKED, NOT WAITED FOR. A node already reports its inventory on a
	// sweep, and that report is telemetry: the node lists its provider and THEN
	// posts, so a snapshot taken before a fence can arrive after it. This command
	// travels through the node's serial queue behind every launch already
	// dispatched, and its answer is recorded against a fence captured before it
	// was sent — which is what turns bounded staleness into an ordering.
	CommandInventory CommandKind = "inventory"
)

// RegisterRequest is a node introducing itself.
//
// Every field is a CLAIM, not a fact. The server records what a node says about
// itself and validates it against its own configuration — a node cannot talk its
// way into more capacity than the operator gave it, because the ledger's limits
// come from the server's config and never from here.
type RegisterRequest struct {
	// Version is the newest wire this node speaks, and MinVersion the oldest.
	// A build from before the wire was a range sends only Version — see
	// DeclaredRange for why that must not be read as a floor of zero.
	Version    int `json:"version"`
	MinVersion int `json:"min_version"`
	// Release is the node binary's release, so an operator can see which hosts
	// are still holding an old protocol open and what to upgrade. Required from
	// VersionNodeRelease; a node below it has none and is recorded as unknown.
	Release string `json:"release"`
	// InstalledDigest is the sha256 of the signed release manifest that produced
	// this node's binary, or empty when nothing on that machine can say.
	//
	// A CLAIM ABOUT BYTES RATHER THAN ABOUT A NAME, which is the whole reason it
	// exists beside Release. The node proves the record still describes the binary
	// it is running before sending one — see internal/provenance — so an empty
	// value means "nothing here can tell", never "this is unverified".
	//
	// EMPTY IS ORDINARY. A host installed from a package, built from source, or
	// upgraded before this existed sends none, and a rollout converges such a host
	// on its version while recording that no manifest proved it.
	InstalledDigest string              `json:"installed_digest,omitempty"`
	Node            string              `json:"node"`
	Provider        config.ProviderKind `json:"provider"`
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
	// EC2Shapes are the ordered purchasable shapes of whichever REMOTE backend this
	// node runs — EC2 instance types, or CodeBuild compute types. Empty for a
	// host-backed provider, whose capacity is the machine; a remote registration
	// without them cannot back an advertisement.
	//
	// THE FIELD NAME IS HISTORICAL AND STAYS. It is the wire spelling every node in
	// the field already sends and the ledger column already stores, and renaming a
	// shipped field to tidy a name is a flag day for nothing. What matters is that
	// one validator sees every catalogue — see config.CheckRemoteShapes, which takes
	// the provider so its diagnostics name the config key the operator wrote.
	EC2Shapes []config.RemoteShape `json:"ec2_shapes,omitempty"`
	// CodeBuildFleet is the arn of the reserved-capacity fleet this node's builds
	// run on, or empty for on-demand compute.
	//
	// SENT SO THE CONTROL PLANE CAN REFUSE A SECOND NODE DRAWING ON THE SAME POOL.
	// A reserved fleet's capacity is shared, so two nodes naming one fleet each
	// advertise all of it — and that is not visible from either node's own config,
	// because neither is wrong by itself. Empty is the ordinary on-demand case and
	// carries no such claim, so nothing is refused for it.
	CodeBuildFleet string `json:"codebuild_fleet,omitempty"`
	// CodeBuildJITParameterPath and CodeBuildRegion are where a codebuild node
	// stages each build's single-use runner registration, and the region it does
	// it in.
	//
	// SENT SO THE CONTROL PLANE CAN SWEEP THE PATH. A node that dies between staging
	// a registration and removing it leaks one parameter, and only the ledger can
	// authorise deleting it: the lease terminal, closed longer ago than any build
	// could still run. The control plane holds the ledger and no node.codebuild
	// block, so the path has to arrive here. Empty for every other backend, and for
	// a codebuild node below VersionCodeBuildRegistrationPath — which is reported
	// by `billet status` as unswept, never refused.
	CodeBuildJITParameterPath string `json:"codebuild_jit_parameter_path,omitempty"`
	CodeBuildRegion           string `json:"codebuild_region,omitempty"`
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
	// Version is the NEGOTIATED version — the highest both builds speak, not
	// the control plane's own. It governs what this node may send and what it
	// may be asked to do for the life of this incarnation.
	Version int `json:"version"`
	// MinVersion and MaxVersion are the control plane's own range, so a node can
	// log the window it joined and an operator reading one node's log can see
	// how much room the rollout has left. Informational: the node acts on
	// Version. Absent from a control plane that predates the negotiated wire,
	// which is why they are not what Version is checked against.
	MinVersion int `json:"min_version,omitempty"`
	MaxVersion int `json:"max_version,omitempty"`
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

	// BarrierID is the compute barrier an inventory command belongs to.
	//
	// ECHOED BACK so an answer cannot be attributed to a barrier it was not asked
	// under. The command id already makes each dispatch unique; this makes the
	// association legible on the wire and lets the recording transaction refuse a
	// result for a barrier a waiter has since replaced.
	BarrierID string `json:"barrier_id,omitempty"`
	// RequestID is what a destroy names. Destroy is by request rather than by
	// lease because it must work for compute whose lease is already gone.
	RequestID int64 `json:"request_id,omitempty"`
	// JobResult is GitHub's authoritative conclusion for a completion-triggered
	// destroy. It is empty for shutdown, sweep and other teardown paths.
	JobResult string `json:"job_result,omitempty"`

	// Upgrade is the release an upgrade command names, and the rollout that asked
	// for it.
	Upgrade *UpgradeSpec `json:"upgrade,omitempty"`
}

// UpgradeSpec is one instruction to replace a node's own billet.
type UpgradeSpec struct {
	// Version is the exact release, never a channel. See CommandUpgrade.
	Version string `json:"version"`

	// ManifestSHA256 is the digest of that release's manifest, so the node
	// verifies the same bytes the control plane decided on rather than whatever a
	// channel says now.
	ManifestSHA256 string `json:"manifest_sha256"`

	// RolloutID and Generation are the fence.
	//
	// A DELAYED INSTRUCTION MUST NOT INSTALL A RELEASE THE FLEET HAS MOVED PAST.
	// Commands cross a network and are retried; without a generation a node that
	// was unreachable for an hour would come back, receive a queued instruction
	// from a rollout that has since been aborted, and take itself off to a
	// version nobody is converging on any more.
	RolloutID  string `json:"rollout_id"`
	Generation int64  `json:"generation"`
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
	Label string               `json:"label"`
	Image string               `json:"image"`
	Trust config.WorkloadTrust `json:"trust"`
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
	// Workflows is the exact workflow allowlist revalidated before a trusted
	// registration is minted.
	Workflows []string `json:"workflows,omitempty"`
	// BuildKitCacheMountLimit is the operator's ceiling for each persistent
	// BuildKit cache-mount record in this tier.
	BuildKitCacheMountLimit config.ByteSize `json:"buildkit_cache_mount_limit"`
	// Intercept enables the authenticated Actions results proxy for this tier.
	Intercept  bool               `json:"intercept,omitempty"`
	CacheScope *config.CacheScope `json:"cache_scope,omitempty"`
}

// TierSpecOf renders the parts of a tier that travel to the selected provider's
// node. The selection happens before this shape crosses the wire, so nodes do not
// need a second copy of the catalogue or its other backends' image names.
func TierSpecOf(t config.Tier, provider config.ProviderKind) *TierSpec {
	return &TierSpec{
		Label:       t.Label,
		Image:       t.ImageFor(provider),
		Trust:       t.Trust.Effective(),
		Command:     t.RunnerCommandFor(provider),
		Disk:        t.Disk,
		SHM:         t.SHM,
		RunnerGroup: t.RunnerGroup,
		Workflows:   slices.Clone(t.Workflows),
		Intercept:   t.Intercept,
		CacheScope:  t.CacheScope,
		BuildKitCacheMountLimit: func() config.ByteSize {
			if t.BuildKitCacheMountLimit > 0 {
				return t.BuildKitCacheMountLimit
			}

			return config.DefaultBuildKitCacheMountLimit
		}(),
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

// WithdrawRequest is a node saying it will not poll again.
//
// EMPTY, AND STILL A BODY. The node is named by its certificate and the path,
// and the process by HeaderIncarnation, exactly as on every other request; what
// the message carries is the intent, and sending `{}` keeps it decodable by the
// plane's strict decoder while leaving room for a field a later version needs.
//
// WHAT IT AUTHORISES IS NARROW. A withdrawal removes the host from PLACEMENT and
// nothing else: it releases no lease, marks no disruption, proves nothing to a
// compute barrier and does not decommission the host. It is accepted only from
// the process currently registered under the name — a superseded incarnation
// withdrawing would take its replacement out of the fleet — and it is fenced in
// the ledger on both the registration epoch and that incarnation.
type WithdrawRequest struct{}

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

// CachePolicyResponse is the control plane's current interception decision.
type CachePolicyResponse struct {
	Allowed bool `json:"allowed"`
}

// Job is the scale-set assignment a launch is for.
//
// Declared here rather than reusing internal/server's so the wire has its own
// stable shape: the server's struct is free to grow fields that are nobody
// else's business, and a protocol that silently follows an internal type is one
// refactor away from a breaking change nobody noticed.
type Job struct {
	RequestID   int64  `json:"request_id"`
	RunID       int64  `json:"run_id"`
	Event       string `json:"event"`
	Owner       string `json:"owner"`
	Repository  string `json:"repository"`
	WorkflowRef string `json:"workflow_ref"`
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

	// BarrierID echoes the inventory command's barrier, and Instances is what the
	// host's provider actually holds.
	//
	// OK CARRIES WHETHER THE LIST MEANS ANYTHING, and there is deliberately no
	// second flag beside it. An empty list with OK is "I looked and found
	// nothing"; anything else is "I could not tell", which resets the run. A
	// separate `known` bool would be two ways to spell those and one of them
	// would eventually be read as the other — the mistake RegisterRequest.
	// InventoryKnown exists to prevent, not to be copied.
	BarrierID string   `json:"barrier_id,omitempty"`
	Instances []string `json:"instances,omitempty"`
}
