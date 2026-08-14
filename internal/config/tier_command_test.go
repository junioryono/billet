package config

import "testing"

// THE DEFAULT RUNNER COMMAND KEEPS THE UPDATE LOOP, and that is the only thing
// about its value that matters.
//
// A self-hosted runner updates itself by EXITING — the listener returns "updating"
// and `run.sh` is the loop that notices and re-execs it with the same arguments,
// including the JIT registration that lets the restarted runner take the job it was
// created for.
//
// Changing this to `bin/Runner.Listener` would look tidier and would drop that loop:
// on a backend where each job gets its own machine, the listener exits to update, the
// machine is destroyed as though the work were done, the job is redelivered, and the
// next machine does the same thing — one guest per attempt, looking like a runner
// that starts and quietly stops.
//
// MEASURED: a JIT configuration from GitHub's REST API carries DisableUpdate = True,
// so that update is never sent today and the loop is insurance. It is pinned anyway
// because the property is invisible in the value, the setting belongs to GitHub
// rather than to billet, and the failure it guards against is silent.
func TestTheDefaultRunnerCommandKeepsTheSelfUpdateLoop(t *testing.T) {
	t.Parallel()

	got := Tier{}.RunnerCommand()

	if len(got) == 0 {
		t.Fatal("a tier with no command would start nothing")
	}

	if got[0] != "./run.sh" {
		t.Errorf("the default runner command is %q; it must be the script that re-execs the "+
			"runner after a self-update, or a runner release day turns every job into a "+
			"destroyed guest and a redelivered job", got[0])
	}
}

// AND AN EXPLICIT COMMAND STILL WINS, because a tier whose image is laid out
// differently has to be able to say so.
func TestATiersOwnCommandIsUsed(t *testing.T) {
	t.Parallel()

	tier := Tier{Command: []string{"/opt/runner/start", "--now"}}

	if got := tier.RunnerCommand(); len(got) != 2 || got[0] != "/opt/runner/start" {
		t.Errorf("a tier's own command was not used: %q", got)
	}
}
