package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// NodePowerConfig lowers this host's CPU ceiling while the node is idle: no
// instance running, none launching and none held in custody, for IdleAfter. A
// launch restores what was there before the VMM starts.
//
// ABSENT MEANS OFF, and off is the node it always was: nothing under
// /sys/devices/system/cpu is read or written, except that a node finding the
// record of an earlier process's cap under its state directory restores it,
// whatever this block says now.
type NodePowerConfig struct {
	// IdleAfter is how long the node must be idle before it lowers the ceiling,
	// a Go duration string. Empty means DefaultPowerIdleAfter.
	IdleAfter string `yaml:"idle_after,omitempty"`
	// DisableBoost writes 0 to /sys/devices/system/cpu/cpufreq/boost while
	// idle. A host without that file (intel_pstate's no_turbo is a different
	// switch) refuses to start with it set.
	DisableBoost bool `yaml:"disable_boost,omitempty"`
	// IdleMaxFreqKHz is the scaling_max_freq written to every cpufreq policy
	// while idle, in kHz as sysfs spells it. Zero leaves the ceiling alone. A
	// value the host cannot honour exactly (not one of a policy's
	// scaling_available_frequencies, or outside its scaling range where the
	// driver lists none) refuses the node's start rather than being rounded by
	// the kernel to a frequency nobody wrote.
	IdleMaxFreqKHz int `yaml:"idle_max_freq_khz,omitempty"`
}

const (
	// DefaultPowerIdleAfter is a minute without work.
	DefaultPowerIdleAfter = time.Minute
	// MinPowerIdleAfter keeps the ceiling from flapping between the jobs of
	// one burst: a queue that refills within it never sees the host capped.
	MinPowerIdleAfter = 10 * time.Second
)

// IdleAfterDuration is how long the node waits idle before it caps, with the
// default applied.
func (p *NodePowerConfig) IdleAfterDuration() (time.Duration, error) {
	if p == nil || strings.TrimSpace(p.IdleAfter) == "" {
		return DefaultPowerIdleAfter, nil
	}

	d, err := time.ParseDuration(strings.TrimSpace(p.IdleAfter))
	if err != nil {
		return 0, fmt.Errorf("node.power.idle_after %q is not a duration (write e.g. 60s or 5m)", p.IdleAfter)
	}
	if d < MinPowerIdleAfter {
		return 0, fmt.Errorf("node.power.idle_after %s is below the %s minimum, which keeps the ceiling from "+
			"moving between the jobs of one burst", d, MinPowerIdleAfter)
	}

	return d, nil
}

// validatePowerNode refuses a power block this node could not honour.
//
// ONLY WHERE THE WORK RUNS ON THIS MACHINE'S CPUs AND THEY HAVE CPUFREQ: ec2
// and codebuild run jobs elsewhere, and a tart host is macOS, which has no
// cpufreq. Whether the host is Linux at all, and whether it has the files and
// the frequency asked for, is the node's to check at its start, since this file
// may be validated elsewhere.
func (c *Config) validatePowerNode() []error {
	p := c.Node.Power
	if p == nil {
		return nil
	}

	var errs []error
	switch c.Node.Provider {
	case ProviderFirecracker, ProviderDocker:
	default:
		errs = append(errs, fmt.Errorf("node.power is set but this node's provider is %s, and only a "+
			"firecracker or docker node runs its jobs on a Linux host's own CPUs", c.Node.Provider))
	}
	if _, err := p.IdleAfterDuration(); err != nil {
		errs = append(errs, err)
	}
	switch {
	case p.IdleMaxFreqKHz < 0:
		errs = append(errs, fmt.Errorf("node.power.idle_max_freq_khz is %d; leave it unset to keep the "+
			"ceiling where it is", p.IdleMaxFreqKHz))
	case p.IdleMaxFreqKHz == 0 && !p.DisableBoost:
		// A BLOCK THAT CHANGES NOTHING is refused, as an inert block is everywhere
		// else: it reads as a node that saves power and is one that does not.
		errs = append(errs, errors.New("node.power changes nothing: set disable_boost, idle_max_freq_khz, "+
			"or both"))
	}

	return errs
}
