// Package guestattach is the terminal server in a job's VM, `billet guest attach-serve`: it
// listens on vsock, which a node reaches through the VMM's own socket and nothing on any
// network can, and runs a shell as the job's user for each connection.
//
// WHATEVER PASSES THROUGH A SESSION IS THE GUEST'S. The job is root in its VM and can replace
// this server, so a session is a terminal on a machine the job controls, and an operator
// reads it as that. The node and the control plane carry its bytes without reading them, and
// no billet error quotes one. It is offered only on a trusted tier, which config enforces.
package guestattach

// Port is the vsock port the server listens on and a node connects to.
const Port = 5151

// maxSessions bounds the terminals one guest serves at once.
const maxSessions = 4

// Config is what the server runs for each session.
type Config struct {
	// User is the account a session's shell runs as, the job's own.
	User string
	// Shell is the shell it runs.
	Shell string
	// Dir is where the shell starts: the runner's work directory.
	Dir string
}
