package config

import (
	"fmt"
	"strings"
	"time"
)

// TierLogs is tiers[].logs: whether this tier's jobs' step logs are kept. Off unless a
// tier says, because a log is as sensitive as its workflow makes it.
type TierLogs struct {
	// Keep keeps the step logs of this tier's jobs on the node that ran them and indexes
	// them on the control plane (ADR-026).
	Keep bool `yaml:"keep,omitempty"`
}

// ServerLogsConfig is server.logs: the bounds of this control plane's index of what jobs
// wrote.
type ServerLogsConfig struct {
	// Retention is a Go duration string, at least an hour; absent, DefaultLogRetention.
	Retention string `yaml:"retention,omitempty"`
	// MaxBytes bounds the index on disk, at least 64MiB; absent, DefaultLogIndexMaxBytes.
	MaxBytes ByteSize `yaml:"max_bytes,omitempty"`
}

// NodeLogsConfig is node.logs: the bounds of the step logs this node keeps.
type NodeLogsConfig struct {
	// Retention is a Go duration string, at least an hour; absent, DefaultLogRetention.
	Retention string `yaml:"retention,omitempty"`
	// MaxBytes bounds what the node keeps in all, at least 64MiB; absent,
	// DefaultNodeLogMaxBytes.
	MaxBytes ByteSize `yaml:"max_bytes,omitempty"`
	// MaxJobBytes bounds what one job may keep, at least 1MiB; absent,
	// DefaultNodeLogJobMaxBytes. A job can write as fast as its disk.
	MaxJobBytes ByteSize `yaml:"max_job_bytes,omitempty"`
}

const (
	// DefaultLogRetention is how long a job's log is kept and indexed: the week #534 asks
	// a search to reach, twice over.
	DefaultLogRetention = 14 * 24 * time.Hour
	// DefaultLogIndexMaxBytes bounds the control plane's index, about three times the
	// text it covers (measured 2026-11-04). It is outside the ledger, so it costs a
	// control plane disk and never its startup scan.
	DefaultLogIndexMaxBytes = 32 * GiB
	// DefaultNodeLogMaxBytes bounds what a node keeps, compressed.
	DefaultNodeLogMaxBytes = 64 * GiB
	// DefaultNodeLogJobMaxBytes bounds one job's log, compressed.
	DefaultNodeLogJobMaxBytes = 64 * MiB

	minLogRetention   = time.Hour
	minLogMaxBytes    = 64 * MiB
	minLogJobMaxBytes = MiB
)

// ServerLogLimits are server.logs with its defaults applied.
type ServerLogLimits struct {
	Retention time.Duration
	MaxBytes  ByteSize
}

// NodeLogLimits are node.logs with its defaults applied.
type NodeLogLimits struct {
	Retention   time.Duration
	MaxBytes    ByteSize
	MaxJobBytes ByteSize
}

// Limits is server.logs with its defaults applied; nil reads as every default.
func (l *ServerLogsConfig) Limits() (ServerLogLimits, error) {
	out := ServerLogLimits{Retention: DefaultLogRetention, MaxBytes: DefaultLogIndexMaxBytes}
	if l == nil {
		return out, nil
	}

	var err error
	if out.Retention, err = logRetention("server.logs.retention", l.Retention); err != nil {
		return ServerLogLimits{}, err
	}

	if out.MaxBytes, err = logBound("server.logs.max_bytes", l.MaxBytes, DefaultLogIndexMaxBytes, minLogMaxBytes); err != nil {
		return ServerLogLimits{}, err
	}

	return out, nil
}

// Limits is node.logs with its defaults applied; nil reads as every default.
func (l *NodeLogsConfig) Limits() (NodeLogLimits, error) {
	out := NodeLogLimits{Retention: DefaultLogRetention, MaxBytes: DefaultNodeLogMaxBytes,
		MaxJobBytes: DefaultNodeLogJobMaxBytes}
	if l == nil {
		return out, nil
	}

	var err error
	if out.Retention, err = logRetention("node.logs.retention", l.Retention); err != nil {
		return NodeLogLimits{}, err
	}

	if out.MaxBytes, err = logBound("node.logs.max_bytes", l.MaxBytes, DefaultNodeLogMaxBytes, minLogMaxBytes); err != nil {
		return NodeLogLimits{}, err
	}

	if out.MaxJobBytes, err = logBound("node.logs.max_job_bytes", l.MaxJobBytes, DefaultNodeLogJobMaxBytes,
		minLogJobMaxBytes); err != nil {
		return NodeLogLimits{}, err
	}

	if out.MaxJobBytes > out.MaxBytes {
		return NodeLogLimits{}, fmt.Errorf("node.logs.max_job_bytes: %s is more than node.logs.max_bytes, %s, so the "+
			"first job to reach it would evict every other job's log", out.MaxJobBytes, out.MaxBytes)
	}

	return out, nil
}

func logRetention(key, raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return DefaultLogRetention, nil
	}

	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration such as \"336h\": %w", key, raw, err)
	}

	if d < minLogRetention {
		return 0, fmt.Errorf("%s: %s is shorter than an hour, which would delete a job's log before anyone could "+
			"read it; give at least \"1h\"", key, d)
	}

	return d, nil
}

func logBound(key string, value, fallback, least ByteSize) (ByteSize, error) {
	if value == 0 {
		return fallback, nil
	}

	if value < least {
		return 0, fmt.Errorf("%s: %s is under %s, which holds too little to search", key, value, least)
	}

	return value, nil
}

// validateLogs refuses a tier asking for what its backend cannot give: only the Linux
// Firecracker guest runs the in-VM agent that reads a job's log, and a setting that
// quietly did nothing would read as logs that exist.
func (t Tier) validateLogs(where string) []error {
	if t.Logs == nil || !t.Logs.Keep {
		return nil
	}

	if !t.servesGuestCaches() {
		return []error{fmt.Errorf("%s: logs.keep needs a Linux Firecracker tier and no other backend, whose guest "+
			"runs the agent that reads a job's log", where)}
	}

	return nil
}

// KeepsJobLogs reports whether any tier asks for its jobs' logs to be kept, which is
// what makes a control plane hold an index.
func (c *Config) KeepsJobLogs() bool {
	for i := range c.Tiers {
		if l := c.Tiers[i].Logs; l != nil && l.Keep {
			return true
		}
	}

	return false
}
