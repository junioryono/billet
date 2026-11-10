package main

import (
	"cmp"
	"fmt"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// A pool's scenarios (#829): what the pool did as a whole, judged beside the cells from
// the same evidence, the ledger's own record of the pool's orders and the jobs that ran on
// their hosts. A scenario not run says why, and is never a pass.

// matrixScenario is one scenario of one pool source and architecture, and its verdict.
type matrixScenario struct {
	Source string      `json:"source"`
	Arch   config.Arch `json:"arch"`
	Name   string      `json:"scenario"`
	// Owed says a run serving the source and covering the architecture must run it: a
	// scenario not run that is owed fails the run, as a cell does.
	Owed    bool          `json:"owed,omitempty"`
	Verdict matrixVerdict `json:"verdict"`
	Reason  string        `json:"reason,omitempty"`
	// Orders are the pool orders it was judged from.
	Orders []string `json:"orders,omitempty"`
}

// The scenarios, in the order the tables print them.
const (
	scenarioBurst      = "burst"
	scenarioShrinkIdle = "shrink-idle"
	scenarioShrinkBusy = "shrink-while-jobs-run"
	scenarioNoCapacity = "az-out-of-capacity"
	scenarioQuota      = "quota-reached"
)

// poolScenarioNotRun is each scenario the lane does not run, by source, and why.
var poolScenarioNotRun = map[string]map[string]string{
	config.PoolSourceAWSMetal: {
		scenarioShrinkBusy: "a shrink while other hosts run jobs needs a second host kept busy while one drains, " +
			"two z1d.metal for over an hour a run; the lane judges the idle shrink, and a release under a host's own " +
			"running jobs is held by internal/alloc's release proof",
		scenarioNoCapacity: "AWS offers no way to make a zone answer InsufficientInstanceCapacity on demand; how the " +
			"pool reads that refusal is held against the source's fake (internal/hostsource/awsmetal)",
		scenarioQuota: "the account's vCPU quota is shared with other work, and lowering it for a run is a quota " +
			"request, not a test; how the pool reads VcpuLimitExceeded is held against the source's fake",
	},
}

// poolScenarioSourceNotRun is a source the lane orders nothing from, and why.
var poolScenarioSourceNotRun = map[string]string{
	config.PoolSourcePhoenixNAP: "phoenixNAP has taken no order through its API (#828)",
}

// judgeScenarios judges every scenario of every pool source and architecture.
func judgeScenarios(doc acceptanceEvidenceDoc, scope matrixScope) []matrixScenario {
	var out []matrixScenario
	for _, b := range matrixBackends {
		if b.source == "" {
			continue
		}
		for _, a := range matrixArchs {
			for _, name := range []string{scenarioBurst, scenarioShrinkIdle, scenarioShrinkBusy, scenarioNoCapacity,
				scenarioQuota} {
				s := matrixScenario{Source: b.source, Arch: a, Name: name, Verdict: verdictNotRun}
				switch {
				case poolScenarioSourceNotRun[b.source] != "":
					s.Reason = poolScenarioSourceNotRun[b.source]
				case poolScenarioNotRun[b.source][name] != "":
					s.Reason = poolScenarioNotRun[b.source][name]
				case name == scenarioBurst:
					s.Owed = true
					judgeBurst(&s, doc, scope)
				case name == scenarioShrinkIdle:
					s.Owed = true
					judgeShrinkIdle(&s, doc, scope)
				}
				out = append(out, s)
			}
		}
	}

	return out
}

// scenarioOrders are the evidence's orders of a scenario's source and architecture.
func scenarioOrders(s *matrixScenario, doc acceptanceEvidenceDoc) []acceptancePoolOrder {
	var out []acceptancePoolOrder
	for i := range doc.Pool {
		if doc.Pool[i].Source == s.Source && doc.Pool[i].Arch == string(s.Arch) {
			out = append(out, doc.Pool[i])
		}
	}

	return out
}

// nodeJobs are the judged jobs that ran on a node.
func nodeJobs(node string, doc acceptanceEvidenceDoc, scope matrixScope) []acceptanceJob {
	var out []acceptanceJob
	for i := range doc.Jobs {
		j := &doc.Jobs[i]
		if node != "" && j.Node == node && scope.belongs(j) {
			out = append(out, *j)
		}
	}

	return out
}

// judgeBurst: from a floor of none, demand ordered a host, the host joined, and a job
// the run dispatched ran on it as GitHub reported it succeeded.
func judgeBurst(s *matrixScenario, doc acceptanceEvidenceDoc, scope matrixScope) {
	orders := scenarioOrders(s, doc)
	if len(orders) == 0 {
		s.Reason = "the pool placed no order on " + string(s.Arch)

		return
	}
	if floor := scope.poolFloors[s.Arch]; floor > 0 {
		s.Reason = fmt.Sprintf("the pool keeps a floor of %d on %s, so no host it ordered was demand's", floor, s.Arch)

		return
	}
	for i := range orders {
		s.Orders = append(s.Orders, orders[i].ID)
		jobs := nodeJobs(orders[i].Node, doc, scope)
		for k := range jobs {
			if j := &jobs[k]; j.GitHub == alloc.ResultSucceeded {
				s.Verdict = verdictPass
				s.Reason = fmt.Sprintf("order %s's host joined as %s and ran %s's job %s", orders[i].ID,
					orders[i].Node, workflowFile(j.WorkflowRef), j.JobName)

				return
			}
		}
	}
	s.Verdict = verdictFail
	s.Reason = "the pool ordered, and no host it ordered ran a job the run dispatched to success"
}

// judgeShrinkIdle: EVERY HOST THE POOL HELD IT LET GO ITSELF. No order of the source and
// architecture still holds a host when the evidence is read; every host that joined was
// released by the pool's own shrink after the newest job on it ended, taken back on its
// source's word, with no job on it disrupted; and at least one of them ran a job of this
// run. One host's shrink does not stand for another's that never came.
func judgeShrinkIdle(s *matrixScenario, doc acceptanceEvidenceDoc, scope matrixScope) {
	// EVERY JOB ON A HOST IS ITS TO ANSWER FOR, so evidence that may hold only some of
	// them cannot pass it.
	if doc.JobsMayBeMissing {
		s.Verdict = verdictFail
		s.Reason = fmt.Sprintf("the evidence holds the first %d jobs only, so a job on a released host may be "+
			"unread", acceptanceJobLimit)

		return
	}
	orders := scenarioOrders(s, doc)
	var joined, wrong []string
	witnessed := false
	for i := range orders {
		o := &orders[i]
		shrunk := o.ReleaseRequested != "" && strings.HasPrefix(o.ReleaseReason, alloc.PoolShrinkReason)
		switch {
		case shrunk:
		case poolOrderHolds(o.State):
			wrong = append(wrong, fmt.Sprintf("%s: still %s, holding a host the pool did not let go", o.ID, o.State))
		case o.State == alloc.PoolRefused:
			// ITS SOURCE REFUSED IT, so it never held a host.
		default:
			// EVERY OTHER WAY AN ORDER ENDS IS A PERSON'S, joined or not: the pool lets a host
			// go only by its shrink, and holds a lapsed join for a person.
			wrong = append(wrong, fmt.Sprintf("%s: %s other than by the pool's own shrink (%s)", o.ID, o.State,
				cmp.Or(o.ReleaseReason, o.Reason)))
		}
		if o.Node == "" {
			continue
		}
		joined = append(joined, o.ID)
		s.Orders = append(s.Orders, o.ID)
		if !shrunk {
			continue
		}
		if len(nodeJobs(o.Node, doc, scope)) > 0 {
			witnessed = true
		}
		wrong = append(wrong, judgeShrunkOrder(o, doc)...)
	}
	switch {
	case len(wrong) > 0:
		s.Verdict, s.Reason = verdictFail, strings.Join(wrong, "; ")
	case len(joined) == 0:
		s.Reason = "no host the pool ordered joined"
	case !witnessed:
		s.Verdict = verdictFail
		s.Reason = "the pool released its hosts itself, and none had run a job of this run"
	default:
		s.Verdict = verdictPass
		s.Reason = "every host the pool held was released by its own shrink after its last job ended, no job " +
			"disrupted"
	}
}

// judgeShrunkOrder is what is wrong with one order the pool's own shrink released: its
// release not completed on its source's word, a job on its host disrupted, or one that
// ended after the release was asked.
func judgeShrunkOrder(o *acceptancePoolOrder, doc acceptanceEvidenceDoc) []string {
	var wrong []string
	asked, err := time.Parse(time.RFC3339Nano, o.ReleaseRequested)
	if err != nil {
		return append(wrong, fmt.Sprintf("%s: its release time %q does not read", o.ID, o.ReleaseRequested))
	}
	// THE SOURCE TOOK IT BACK, ON ITS OWN WORD: released, or gone as its source said, and
	// never closed by a person, whose close a stalled release could otherwise end in.
	switch {
	case o.State == alloc.PoolReleased:
	case o.State == alloc.PoolGone && o.Reason == alloc.PoolGoneBySource:
	case o.State == alloc.PoolGone:
		wrong = append(wrong, fmt.Sprintf("%s: released by the shrink and closed other than by its source (%s)",
			o.ID, o.Reason))
	default:
		wrong = append(wrong, fmt.Sprintf("%s: released by the shrink and still %s", o.ID, o.State))
	}
	// EVERY JOB ON THE HOST, OF THIS RUN OR NOT: the run's jobs witness the shrink, and
	// any job the release could have cut short is the shrink's to answer for.
	jobs := nodeJobs(o.Node, doc, matrixScope{})
	for k := range jobs {
		j := &jobs[k]
		if j.Disruption != "" {
			wrong = append(wrong, fmt.Sprintf("%s: job %s on its host was disrupted (%s)", o.ID, j.LeaseID,
				j.Disruption))
		}
		finished, err := time.Parse(time.RFC3339Nano, j.FinishedAt)
		if err != nil {
			wrong = append(wrong, fmt.Sprintf("%s: job %s's end %q does not read", o.ID, j.LeaseID, j.FinishedAt))

			continue
		}
		if !finished.Before(asked) {
			wrong = append(wrong, fmt.Sprintf("%s: job %s ended %s, not before the release was asked %s", o.ID,
				j.LeaseID, j.FinishedAt, o.ReleaseRequested))
		}
	}

	return wrong
}

// matrixPoolFloors is the floor the config's pool keeps on each architecture.
func matrixPoolFloors(cfg *config.Config) map[config.Arch]int {
	out := map[config.Arch]int{}
	if cfg.Server == nil || cfg.Server.Pool == nil {
		return out
	}
	for arch, a := range cfg.Server.Pool.Architectures {
		out[arch] = a.Floor
	}

	return out
}
