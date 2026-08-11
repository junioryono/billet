package config

import (
	"fmt"
	"strings"
	"time"
)

// defaultDrainTimeout bounds how long a stopping billet waits for the jobs it is
// already running before it destroys them.
//
// SIX HOURS BECAUSE THAT IS HOW LONG A JOB MAY RUN, not because it is a round
// number: GitHub's jobs.<job_id>.timeout-minutes defaults to 360. A drain
// shorter than the longest ordinary job is a drain that routinely fails to
// drain, and the operator's evidence would be a killed job rather than a
// message.
//
// This is a DEFAULT and not a policy, and the distinction is load-bearing.
// node.max_custody's comment records that "self-hosted runners are routinely
// configured past GitHub's six-hour default" — which is exactly the fleet that
// needs to raise this, because a restart would otherwise kill the long job the
// drain exists to protect.
//
// Overrunning it is not a disaster: billet stops waiting and destroys what is
// left, which is precisely what it did before a drain existed. That is why a
// finite default is right here while node.max_custody defaults to no bound at
// all — exceeding this one degrades to the old behaviour, whereas letting the
// wait run forever hands the deadline to the service manager, whose expiry is a
// SIGKILL that skips the teardown and strands the containers.
const defaultDrainTimeout = 6 * time.Hour

// maxDrainTimeout is a ceiling on the configured value, because at this
// magnitude a typo is likelier than the intent. "8760h" is a year; a unit file
// whose TimeoutStopSec is sized from it would let the service manager wait
// effectively forever on a host that is never going to finish draining.
const maxDrainTimeout = 24 * time.Hour

// DrainTimeoutDuration parses Server.DrainTimeout, reporting the default when unset.
//
// Parsed on demand rather than at load time, so the config type stays a plain
// data shape — but Validate calls it too, so a typo is reported when the file is
// read rather than at the shutdown that needed it.
func (s *ServerConfig) DrainTimeoutDuration() (time.Duration, error) {
	if s == nil {
		return defaultDrainTimeout, nil
	}

	return parseDrainTimeout("server.drain_timeout", s.DrainTimeout)
}

// DrainTimeoutDuration parses Node.DrainTimeout, reporting the default when unset.
//
// Separate from the server's because a node and a control plane are restarted
// for different reasons and need not wait the same amount of time.
func (n *NodeConfig) DrainTimeoutDuration() (time.Duration, error) {
	if n == nil {
		return defaultDrainTimeout, nil
	}

	return parseDrainTimeout("node.drain_timeout", n.DrainTimeout)
}

func parseDrainTimeout(key, raw string) (time.Duration, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return defaultDrainTimeout, nil
	}

	// The message names the shape that works. time.ParseDuration's own error for
	// a bare number is "missing unit in duration", which is true and leaves the
	// reader to guess which units exist.
	d, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration such as \"90m\", \"6h\" or \"30s\": %w",
			key, raw, err)
	}

	// Zero is refused rather than read as "use the default". Someone who writes
	// 0s means "do not wait", and silently waiting six hours instead is the
	// divergence between the file and the behaviour that this check exists to
	// stop. Refusing it is only an obstacle unless the message says how to get
	// what they asked for, so it does.
	if d == 0 {
		return 0, fmt.Errorf(
			"%s: 0 would stop billet waiting for its running jobs at all. That is what a "+
				"second signal is for, on the one shutdown that needs it — remove the key to "+
				"wait the default %s", key, defaultDrainTimeout)
	}

	if d < 0 {
		return 0, fmt.Errorf("%s: %q must be positive", key, raw)
	}

	// Refused rather than clamped: a clamp runs the drain on a number the
	// operator never wrote and cannot find in their config.
	if d > maxDrainTimeout {
		return 0, fmt.Errorf(
			"%s: %q is longer than 24h, which is more likely a typo than a job that runs "+
				"that long; a service manager's stop timeout sized from it would wait "+
				"effectively forever", key, raw)
	}

	return d, nil
}
