package host

import (
	"context"
	"fmt"

	"github.com/junioryono/billet/internal/lifeops"
)

// The node stop the endpoint commands perform, shared so each proves it the
// same way: admitted on a fresh observation of the process the decision rests
// on, and proved by the command's own observation afterwards.

// admitNodeStop observes the node unit immediately before a stop and admits it
// only for the process judged: the same pid and invocation, not stopping,
// loaded, under a KillMode whose stop proves something, and with the SubState
// and Result the proof will read answered. act names what the stop is for in a
// refusal ("migrated", "repaired"). A command whose stop rests on more of the
// unit names those properties in extra and judges them in judge, on the same
// observation the process is judged on: a second read would leave whichever
// came first stale by the length of the other.
func admitNodeStop(ctx context.Context, insp *lifeops.Inspector, judged unitObservation, act string, extra []string,
	judge func(props map[string][]string) *endpointRefusal,
) *endpointRefusal {
	pre, problem := observeUnit(ctx, insp, nodeUnit, extra...)
	if problem != "" {
		return endpointUnknown(endpointReasonUnit, "before the stop: "+problem, "", stateNothing)
	}

	if pre.ActiveState == "deactivating" {
		return endpointRefuse(endpointReasonStopping, fmt.Sprintf("%s began stopping under the judgement (since %s); "+
			"wait or inspect the drain; nothing to do here", nodeUnit, orUnknownWord(pre.StateChangeTimestamp)),
			"wait for the stop to complete, then check and approve afresh", stateNothing)
	}

	if _, running, problem := runningPID(pre); problem != "" || !running || processMoved(judged, pre) {
		return endpointUnknown(endpointReasonProcess, fmt.Sprintf("%s moved between the judgement and the stop (pid %s, "+
			"invocation %s, %s now; pid %s, invocation %s when judged)", nodeUnit, pre.MainPID, pre.InvocationID,
			pre.ActiveState, judged.MainPID, judged.InvocationID), "check and approve afresh", stateNothing)
	}

	// AN INCOMPLETE OBSERVATION IS COULD-NOT-TELL: a property systemd did not
	// answer is not a disallowed value.
	if pre.KillMode == "" {
		return endpointUnknown(endpointReasonUnit, "systemd answered no KillMode for "+nodeUnit+", so the stop's "+
			"premise cannot be judged", "check and approve afresh", stateNothing)
	}

	switch pre.KillMode {
	case "mixed", "control-group":
	default:
		return endpointRefuse(endpointReasonPolicy, fmt.Sprintf("%s has KillMode=%s, under which a stop proves nothing "+
			"about the processes that remain; only mixed or control-group is %s", nodeUnit, orUnknownWord(pre.KillMode),
			act), "", stateNothing)
	}

	if pre.LoadState == "" {
		return endpointUnknown(endpointReasonUnit, "systemd answered no LoadState for "+nodeUnit+", so whether it can be "+
			"started after the stop cannot be judged", "check and approve afresh", stateNothing)
	}

	if pre.LoadState != "loaded" {
		return endpointRefuse(endpointReasonUnit, fmt.Sprintf("%s has LoadState=%s, so it cannot be started after the "+
			"stop", nodeUnit, orUnknownWord(pre.LoadState)), "", stateNothing)
	}

	// THE OBSERVATION IS COMPLETE: the post-stop judgement reads SubState and
	// Result, and a unit that answers neither now would answer neither then.
	if pre.SubState == "" || pre.Result == "" {
		return endpointUnknown(endpointReasonUnit, "systemd answered no SubState or Result for "+nodeUnit+", so the "+
			"observation is incomplete and the stop cannot be judged", "check and approve afresh", stateNothing)
	}

	if judge != nil {
		return judge(pre.raw)
	}

	return nil
}

// proveNodeStopped is the post-stop observation, the command's own: only
// inactive, dead and success prove the process gone.
func proveNodeStopped(ctx context.Context, insp *lifeops.Inspector) (migrateStopped, *endpointRefusal) {
	post, problem := observeUnit(ctx, insp, nodeUnit)
	if problem != "" {
		return migrateStopped{}, endpointUnknown(endpointReasonUnproved, "after the stop: "+problem, "", stateStopped)
	}

	stopped := migrateStopped{ActiveState: post.ActiveState, SubState: post.SubState, Result: post.Result}

	if _, ok := post.raw["Result"]; !ok || stopped.ActiveState != "inactive" || stopped.SubState != "dead" ||
		stopped.Result != "success" {
		return migrateStopped{}, endpointUnknown(endpointReasonUnproved, fmt.Sprintf("the stop of %s is not proved: "+
			"systemd reports ActiveState=%s SubState=%s Result=%s, and only inactive/dead/success proves the process gone",
			nodeUnit, orUnknownWord(post.ActiveState), orUnknownWord(post.SubState), orUnknownWord(post.Result)),
			"inspect the unit; check and approve afresh", stateStopped)
	}

	return stopped, nil
}
