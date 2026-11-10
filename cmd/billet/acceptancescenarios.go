package main

import (
	"fmt"
	"slices"
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

// judgeShrinkIdle: the pool itself asked for an idle host's release after the newest job
// on it ended, the release completed, and no job on the host was disrupted.
func judgeShrinkIdle(s *matrixScenario, doc acceptanceEvidenceDoc, scope matrixScope) {
	orders := scenarioOrders(s, doc)
	var released []acceptancePoolOrder
	for i := range orders {
		if orders[i].ReleaseRequested != "" && strings.HasPrefix(orders[i].ReleaseReason, "idle since ") {
			released = append(released, orders[i])
		}
	}
	if len(released) == 0 {
		s.Reason = "no order of the pool was released by its own shrink"

		return
	}

	var wrong []string
	for i := range released {
		o := &released[i]
		s.Orders = append(s.Orders, o.ID)
		asked, err := time.Parse(time.RFC3339Nano, o.ReleaseRequested)
		if err != nil {
			wrong = append(wrong, fmt.Sprintf("%s: its release time %q does not read", o.ID, o.ReleaseRequested))

			continue
		}
		if !slices.Contains([]string{alloc.PoolReleased, alloc.PoolGone}, o.State) {
			wrong = append(wrong, fmt.Sprintf("%s: released by the shrink and still %s", o.ID, o.State))
		}
		jobs := nodeJobs(o.Node, doc, scope)
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
	}
	if len(wrong) > 0 {
		s.Verdict, s.Reason = verdictFail, strings.Join(wrong, "; ")

		return
	}
	s.Verdict = verdictPass
	s.Reason = "released by the pool's own shrink after its host's last job ended, no job disrupted"
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
