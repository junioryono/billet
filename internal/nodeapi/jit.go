package nodeapi

// The JIT half of the wire.
//
// A NODE HOLDS NO GITHUB CREDENTIAL, and that is a security property rather than
// a convenience. The App private key can register runners against the whole
// organisation; keeping it on the control plane alone means a compromised
// compute host — the machine actually running untrusted pull-request code —
// cannot mint runners, only ask for one.
//
// So the node asks and the server mints. That puts a SECRET on this wire: a JIT
// configuration is a credential until the runner consumes it. It is single-use
// for one ephemeral pool registration, which may consume any job GitHub routes
// to that pool. It is the reason the plain HTTP transport is refused anywhere
// but loopback, and the reason mTLS is not optional before a node runs on
// another machine.

// DescribeRequest asks for a tier's scale set.
type DescribeRequest struct {
	Name  string `json:"name"`
	Group string `json:"group,omitempty"`
}

// DescribeResponse names the scale set, when there is one.
//
// Found distinguishes "no such scale set" from "a scale set with id 0", which a
// bare zero could not. The runner treats absence as a reason to stop rather than
// as an id to launch against.
type DescribeResponse struct {
	Found bool     `json:"found"`
	ID    int      `json:"id,omitempty"`
	Name  string   `json:"name,omitempty"`
	Names []string `json:"names,omitempty"`
}

// TrustedRunnerGroupRequest asks the control plane to revalidate the policy a
// trusted pool relies on before the node asks it to mint a registration.
type TrustedRunnerGroupRequest struct {
	Group     string   `json:"group"`
	Workflows []string `json:"workflows"`
}

// JITRequest asks for one runner registration.
type JITRequest struct {
	ScaleSetID int    `json:"scale_set_id"`
	RunnerName string `json:"runner_name"`
	WorkFolder string `json:"work_folder"`
}

// JITResponse carries the minted registration.
//
// Config IS THE CREDENTIAL. It must never be logged, and the node hands it
// straight to the instance it is starting. The response is deliberately minimal
// so nothing else about it invites being kept.
type JITResponse struct {
	Config     string `json:"config"`
	RunnerID   int64  `json:"runner_id"`
	RunnerName string `json:"runner_name"`
}

// RemoveRunnerRequest identifies the exact GitHub registration to withdraw.
type RemoveRunnerRequest struct {
	RunnerID   int64  `json:"runner_id"`
	RunnerName string `json:"runner_name"`
}

// RecoverRunnerRequest names the deterministic registration of quarantined
// compute whose pre-journal launch may have survived an upgrade.
type RecoverRunnerRequest struct {
	RunnerName string `json:"runner_name"`
}

// RecoverRunnerResponse says whether the registration is durably tracked,
// proven busy, or safely retired before compute teardown.
type RecoverRunnerResponse struct {
	State string `json:"state"`
}

const (
	RunnerRecoveryTracked = "tracked"
	RunnerRecoveryBusy    = "busy"
	RunnerRecoveryRetired = "retired"
)
