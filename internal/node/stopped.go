package node

import (
	"time"

	"github.com/junioryono/billet/internal/provider"
)

// noteStopped dates each running entry whose instance the provider lists as not
// running, and forgets the date when it runs again or is no longer an entry.
//
// A RUNNING ENTRY IS LET GO ONLY BY A DESTROY, and a Destroy comes from the
// control plane after GitHub reports the job over. A guest whose VMM ended with
// no such report (a crash, an operator's kill, a runner that never took a job)
// keeps its entry, and Holding() counted it, so a draining node waited on it for
// as long as nobody sent a Destroy. On 2026-09-30 that kept a node out of service
// after its VMM was gone (#287).
//
// NOTHING IS RELEASED OR DESTROYED HERE, deliberately. A stopped instance is also
// what a job that finished normally looks like while its Destroy is on the way,
// so failing its lease would misattribute a successful job; the entry is kept for
// the Destroy that normally follows, and only Holding stops counting it once
// stoppedProved says so. What is left is the next process's Recover to destroy,
// as it destroys anything nothing is waiting for. `Running: false` is the provider's proof, not its silence:
// firecracker answers false only on a refused connection or an absent socket and
// TRUE whenever it cannot tell.
func (r *Runner) noteStopped(instances []*provider.Instance) {
	stopped := make(map[string]bool, len(instances))

	for _, inst := range instances {
		if inst != nil && !inst.Running {
			stopped[inst.Name] = true
		}
	}

	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.stoppedSince == nil {
		r.stoppedSince = make(map[string]time.Time)
		r.stoppedProved = make(map[string]bool)
	}

	current := make(map[string]bool, len(stopped))

	for _, inst := range r.running {
		if inst == nil || !stopped[inst.Name] {
			continue
		}

		current[inst.Name] = true

		since, seen := r.stoppedSince[inst.Name]
		if !seen {
			r.stoppedSince[inst.Name] = now
			continue
		}

		// PROVED BY THIS SWEEP'S OBSERVATION, not by the clock at the moment
		// Holding is asked: two listings strayGrace apart, which rules out one
		// racing the launch that created the instance.
		if now.Sub(since) >= strayGrace && !r.stoppedProved[inst.Name] {
			r.stoppedProved[inst.Name] = true
			r.log.Warn("a running guest's compute has stopped and no destroy has come for it; "+
				"a drain no longer waits on it, and its entry is kept for the destroy",
				"name", inst.Name, "stopped_since", since)
		}
	}

	for name := range r.stoppedSince {
		if !current[name] {
			delete(r.stoppedSince, name)
			delete(r.stoppedProved, name)
		}
	}
}
