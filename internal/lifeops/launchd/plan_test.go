package launchd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/lifeops"
)

// THE SHIPPED AGENTS ARE READ BY THE PARSER THAT COMPARES THEM, which is what
// makes the comparison mean anything.
//
// These plists are mostly comments — the reasoning is the point of them — and
// every value in them appears in that prose as well as in the key that matters.
// A parser that searched would be satisfied by the explanation, so it would
// report a loaded job as matching with the real settings deleted.
func TestDeclaredAgentReadsWhatBilletShips(t *testing.T) {
	t.Parallel()

	for label, body := range map[string]string{
		deploy.NodeAgentLabel:   deploy.NodeAgent,
		deploy.ServerAgentLabel: deploy.ServerAgent,
	} {
		t.Run(label, func(t *testing.T) {
			t.Parallel()

			got, err := declaredAgent(body)
			if err != nil {
				t.Fatalf("declaredAgent: %v", err)
			}

			if got.Label != label {
				t.Errorf("Label = %q, want %q", got.Label, label)
			}

			// THE ARGUMENTS CARRY --config, which is the whole reason they are
			// compared: an agent loaded against another config is a second
			// deployment sharing a machine.
			if len(got.Arguments) == 0 {
				t.Fatal("no ProgramArguments, so this agent runs nothing")
			}

			if !strings.Contains(strings.Join(got.Arguments, " "), "--config") {
				t.Errorf("the arguments carry no --config: %v", got.Arguments)
			}

			// THE DRAIN GRACE. Absent, launchd kills a draining node after five
			// seconds — measured, against a man page that says twenty.
			if !got.ExitTimeoutKnown || got.ExitTimeout == 0 {
				t.Errorf("ExitTimeOut = %d (known %v); without it launchd SIGKILLs a draining "+
					"node after its own five-second default", got.ExitTimeout, got.ExitTimeoutKnown)
			}

			if !got.RunAtLoad {
				t.Error("RunAtLoad is not set, so this agent does not start at login")
			}
		})
	}
}

// AND THE NODE'S PATH IS PART OF WHAT IS COMPARED.
//
// The node resolves tart and softnet out of PATH. A loaded job whose PATH lost
// the Homebrew prefix registers and then refuses all work, and the same omission
// breaks untrusted isolation — so this is not a cosmetic field.
func TestDeclaredAgentReadsTheNodesPath(t *testing.T) {
	t.Parallel()

	got, err := declaredAgent(deploy.NodeAgent)
	if err != nil {
		t.Fatalf("declaredAgent: %v", err)
	}

	path, ok := got.Environment["PATH"]
	if !ok {
		t.Fatal("the node agent declares no PATH, so launchd's own applies and tart is invisible")
	}

	for _, prefix := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		if !strings.Contains(path, prefix) {
			t.Errorf("PATH omits %s: %q", prefix, path)
		}
	}
}

// A LOADED JOB THAT IS NOT WHAT THE AGENT DECLARES IS REFUSED, and the
// environment is compared WHOLE.
//
// This is the measurement the backend turns on: launchd reads a plist once at
// bootstrap and keeps what it read, so a node can be running a stale definition
// while the file on disk is byte-identical to the one billet ships. Comparing
// the FILE would certify it.
//
// The environment matters as much as the timeout, and picking which variables to
// compare is picking which a future agent may quietly add. tart's VM store is
// per-user and selected by TART_HOME, so a loaded job carrying a redirected one
// inspects a DIFFERENT store, reports live guests absent, and the control plane
// frees their leases and sells the capacity again.
func TestLoadedRefusalsCatchesADefinitionThatDrifted(t *testing.T) {
	t.Parallel()

	want, err := declaredAgent(deploy.NodeAgent)
	if err != nil {
		t.Fatalf("declaredAgent: %v", err)
	}

	matching := Job{
		Program:          want.Arguments[0],
		Arguments:        want.Arguments,
		Environment:      want.Environment,
		ExitTimeout:      want.ExitTimeout,
		ExitTimeoutKnown: true,
	}

	// A HEALTHY HOST IS NOT REFUSED, kept beside the rest because a comparison
	// that refuses everything passes every one of the cases below.
	c := &Converger{}
	if got := c.loadedRefusals(deploy.NodeAgentLabel, matching, deploy.NodeAgent); len(got) != 0 {
		t.Fatalf("a job loaded from the shipped agent was refused: %v", got)
	}

	drift := func(f func(*Job)) Job {
		j := matching
		j.Environment = map[string]string{}

		for k, v := range matching.Environment {
			j.Environment[k] = v
		}

		f(&j)

		return j
	}

	for name, tc := range map[string]struct {
		job  Job
		want string
	}{
		"a stale drain grace": {
			job:  drift(func(j *Job) { j.ExitTimeout = 20 }),
			want: "SIGKILL",
		},
		"a different binary": {
			job:  drift(func(j *Job) { j.Program = "/opt/other/billet" }),
			want: "/opt/other/billet",
		},
		"a different config": {
			job: drift(func(j *Job) {
				j.Arguments = []string{j.Program, "node", "--config", "/tmp/other.yaml"}
			}),
			want: "arguments",
		},
		// THE ONE THAT RESELLS CAPACITY: a redirected VM store makes the node's
		// inventory describe somebody else's machine.
		"a redirected tart store": {
			job:  drift(func(j *Job) { j.Environment["TART_HOME"] = "/tmp/elsewhere" }),
			want: "TART_HOME",
		},
		"a redirected home": {
			job:  drift(func(j *Job) { j.Environment["HOME"] = "/tmp/elsewhere" }),
			want: "HOME",
		},
		"a PATH that cannot find tart": {
			job:  drift(func(j *Job) { j.Environment["PATH"] = "/usr/bin:/bin" }),
			want: "PATH",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := c.loadedRefusals(deploy.NodeAgentLabel, tc.job, deploy.NodeAgent)
			if len(got) == 0 {
				t.Fatal("a job loaded from a definition that is not the shipped one was accepted")
			}

			joined := got[0].Error()
			if !strings.Contains(joined, tc.want) {
				t.Errorf("the refusal does not name what drifted (%q): %s", tc.want, joined)
			}

			// AND IT SAYS WHAT TO DO. Replacing the file changes nothing about a
			// loaded job, so an operator told only "it differs" would edit the
			// plist, see no change, and have no next step.
			if !strings.Contains(joined, "billet local down") {
				t.Errorf("the refusal does not say how to fix it: %s", joined)
			}
		})
	}
}

// launchd's OWN VARIABLES ARE NOT EVIDENCE OF ANYTHING.
//
// Every job gets XPC_SERVICE_NAME and OSLogRateLimit whether the agent asks or
// not. Counting them as drift would refuse every correctly loaded service —
// which is the direction that makes a comparison useless rather than unsafe, and
// is exactly how a check like this gets deleted a month later.
func TestLoadedRefusalsIgnoresWhatLaunchdAddsToEveryJob(t *testing.T) {
	t.Parallel()

	want, err := declaredAgent(deploy.NodeAgent)
	if err != nil {
		t.Fatalf("declaredAgent: %v", err)
	}

	env := map[string]string{"XPC_SERVICE_NAME": deploy.NodeAgentLabel, "OSLogRateLimit": "64"}
	for k, v := range want.Environment {
		env[k] = v
	}

	job := Job{
		Program:          want.Arguments[0],
		Arguments:        want.Arguments,
		Environment:      env,
		ExitTimeout:      want.ExitTimeout,
		ExitTimeoutKnown: true,
	}

	c := &Converger{}
	if got := c.loadedRefusals(deploy.NodeAgentLabel, job, deploy.NodeAgent); len(got) != 0 {
		t.Errorf("launchd's own variables were counted as drift: %v", got)
	}
}

// ENABLING INSTALLS THE AGENT AND CLEARS THE OVERRIDE, and undoing it takes back
// exactly what this run did.
func TestEnableInstallsAndDisableUndoesWhatThisRunDid(t *testing.T) {
	t.Parallel()

	f := &fake{t: t, replies: map[string][]reply{
		"print-disabled": {{out: "\n\tdisabled services = {\n\t}\n"}},
		"enable":         {{}},
		"disable":        {{}},
	}}

	c := f.converger(t)
	label := deploy.NodeAgentLabel
	path := filepath.Join(c.agentsDir, label+".plist")

	if err := c.Enable(t.Context(), label); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the agent was not installed: %v", err)
	}

	if !sameAgent(string(body), deploy.NodeAgent) {
		t.Error("the installed agent is not the one billet ships")
	}

	// UNDOING AN INSTALL IS A REMOVAL, not a disabled override. A label disabled
	// with no plist to explain it is the landmine: a later install bootstraps a
	// service launchd silently refuses to run.
	if err := c.Disable(t.Context(), label); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the agent this run installed was not removed (%v)", err)
	}

	// THE EXACT CALL, not a substring: `print-disabled` contains "disable", so a
	// contains-check here matched the override being READ and passed whatever
	// the undo actually did.
	for _, call := range f.calls {
		if call == "disable "+c.target(label) {
			t.Errorf("undoing an install wrote a disabled override, which is the landmine: %v",
				f.calls)
		}
	}
}

// AND UNDOING AN ENABLEMENT THIS RUN DID NOT MAKE WRITES THE OVERRIDE INSTEAD.
//
// If the agent was already installed, all this run did was clear an override, so
// that is all there is to put back. Removing the file would delete something the
// operator installed.
func TestDisableWritesAnOverrideWhenItDidNotInstallTheAgent(t *testing.T) {
	t.Parallel()

	f := &fake{t: t, replies: map[string][]reply{
		"print-disabled": {{out: "\n\tdisabled services = {\n\t}\n"}},
		"disable":        {{}},
	}}
	c := f.converger(t)
	label := deploy.NodeAgentLabel
	path := filepath.Join(c.agentsDir, label+".plist")

	if err := os.WriteFile(path, []byte(deploy.NodeAgent), 0o600); err != nil {
		t.Fatalf("stage an already-installed agent: %v", err)
	}

	if err := c.Disable(t.Context(), label); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("an agent this run did not install was removed: %v", err)
	}

	if !strings.Contains(strings.Join(f.calls, "|"), "disable") {
		t.Errorf("nothing stopped it starting at login: %v", f.calls)
	}
}

// UNINSTALL REMOVES THE PLIST FIRST AND CLEARS THE OVERRIDE SECOND.
//
// The order is the whole thing, and it is the opposite of what reads naturally.
// Clearing first opens a window in which the label is enabled and its plist is
// still there — a login or reboot inside it starts the node being uninstalled,
// with nothing left watching it.
func TestUninstallRemovesTheAgentBeforeClearingTheOverride(t *testing.T) {
	t.Parallel()

	f := &fake{t: t, replies: map[string][]reply{"enable": {{}}}}
	c := f.converger(t)
	label := deploy.NodeAgentLabel
	path := filepath.Join(c.agentsDir, label+".plist")

	if err := os.WriteFile(path, []byte(deploy.NodeAgent), 0o600); err != nil {
		t.Fatalf("stage an installed agent: %v", err)
	}

	// The override is cleared by `launchctl enable`, and this records whether the
	// plist was still there when that happened.
	var plistPresentAtClear bool

	f.before = func(args []string) {
		if args[0] == "enable" {
			_, err := os.Stat(path)
			plistPresentAtClear = err == nil
		}
	}

	if err := c.Uninstall(t.Context(), label); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the agent is still installed (%v)", err)
	}

	if plistPresentAtClear {
		t.Error("the override was cleared while the plist was still there: a login in that " +
			"window starts the node being uninstalled")
	}

	if !strings.Contains(strings.Join(f.calls, "|"), "enable") {
		t.Errorf("the disabled override was never cleared, which breaks the next install: %v",
			f.calls)
	}
}

// AND IT LEAVES A PLIST THAT IS NOT BILLET'S.
func TestUninstallLeavesAnAgentBilletDidNotWrite(t *testing.T) {
	t.Parallel()

	f := &fake{t: t, replies: map[string][]reply{"enable": {{}}}}
	c := f.converger(t)
	label := deploy.NodeAgentLabel
	path := filepath.Join(c.agentsDir, label+".plist")

	if err := os.WriteFile(path, []byte("<plist>somebody's own</plist>"), 0o600); err != nil {
		t.Fatalf("stage an edited agent: %v", err)
	}

	err := c.Uninstall(t.Context(), label)
	if err == nil {
		t.Fatal("an agent billet did not write was removed")
	}

	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("the edited agent was removed anyway: %v", statErr)
	}

	// AND NOTHING ELSE WAS UNDONE EITHER. A half-uninstalled service — no
	// override, a plist billet will not touch — is worse than one still
	// installed, because the next `up` refuses it and the next login starts it.
	if strings.Contains(strings.Join(f.calls, "|"), "enable") {
		t.Errorf("the override was cleared for a service that was left installed: %v", f.calls)
	}

	if !strings.Contains(err.Error(), "left alone") {
		t.Errorf("the error does not say what happened to the file: %v", err)
	}
}

// A PLAN NAMES WHAT IS WRONG WITH THE MACHINE, not just with the services.
func TestPlanRefusesDirectoriesNothingCanWrite(t *testing.T) {
	t.Parallel()

	f := &fake{t: t, replies: map[string][]reply{
		"print":          {{out: "", code: notLoaded}},
		"print-disabled": {{out: "\n\tdisabled services = {\n\t}\n"}},
	}}

	c := f.converger(t)
	c.logDir = filepath.Join(t.TempDir(), "does-not-exist")

	plan, err := c.Plan(t.Context(), lifeops.UpRequest{
		ConfigPath: filepath.Join(c.agentsDir, "billet.yaml"),
		WantNode:   true,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var found bool

	for _, r := range plan.Refusals {
		if strings.Contains(r.What, "does-not-exist") {
			found = true

			// THE COMMAND THAT FIXES IT. /usr/local is root-owned on a stock Mac
			// and these commands refuse root, so an operator who is only told
			// "it is missing" has nothing to do next.
			if !strings.Contains(r.Remedy, "mkdir -p") {
				t.Errorf("the refusal does not name the command that fixes it: %s", r.Remedy)
			}
		}
	}

	if !found {
		t.Errorf("a missing log directory was not refused; launchd creates neither the "+
			"directory nor the files, and an agent whose log directory is absent fails to "+
			"spawn with no log to say so: %+v", plan.Refusals)
	}
}

// AND IT DECLARES THAT THIS MANAGER CANNOT START A DISABLED SERVICE.
func TestPlanDeclaresTheInvertedOrder(t *testing.T) {
	t.Parallel()

	f := &fake{t: t, replies: map[string][]reply{
		"print":          {{out: "", code: notLoaded}},
		"print-disabled": {{out: "\n\tdisabled services = {\n\t}\n"}},
	}}

	c := f.converger(t)
	c.logDir = c.agentsDir

	plan, err := c.Plan(t.Context(), lifeops.UpRequest{
		ConfigPath: filepath.Join(c.agentsDir, "billet.yaml"),
		WantNode:   true,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if len(plan.Refusals) != 0 {
		t.Fatalf("a healthy host was refused: %+v", plan.Refusals)
	}

	if len(plan.Units) != 1 {
		t.Fatalf("Units = %+v, want the node", plan.Units)
	}

	unit := plan.Units[0]

	if !unit.EnableBeforeStart {
		t.Error("the plan does not declare that this manager cannot start a disabled service, " +
			"so `up` would bootstrap before clearing the override and launchd would refuse it")
	}

	if !unit.Start {
		t.Error("a service launchd does not have was not planned to start")
	}

	if !unit.Enable {
		t.Error("a service with no agent installed was not planned to be enabled")
	}
}

// A SERVICE THAT IS STILL STOPPING IS NEITHER RUNNING NOR STOPPED, and `up`
// refuses rather than deciding.
//
// `SIGTERMed` is the launchd twin of systemd's `deactivating`: the process is
// ALIVE and on its way out. Measured, a node sits in it for as long as its drain
// takes, which can be hours. Treating it as running makes `up` decide there is
// nothing to start and report the host up — while the node finishes draining and
// stops, leaving a machine that says it is up and is running nothing. Treating
// it as stopped is worse: `up` bootstraps a second one on top.
//
// `spawn scheduled` is the same shape from the other side — launchd INTENDS to
// start a process and has not — and any state this build has no rule for is
// refused for the same reason: nothing established what it means.
func TestPlanRefusesAServiceThatIsNeitherRunningNorStopped(t *testing.T) {
	t.Parallel()

	for name, state := range map[string]string{
		"still draining":       "SIGTERMed",
		"about to be started":  "spawn scheduled",
		"a state with no rule": "quiescing",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := &fake{t: t, replies: map[string][]reply{
				// LOADED FROM THE SHIPPED AGENT IN EVERY OTHER RESPECT, so the
				// only thing this can refuse for is the state. A stub that
				// differed elsewhere would be refused for that instead, and the
				// test would pass without the rule it names existing.
				"print":          {{out: nodePrint(t, state, 4242)}},
				"print-disabled": {{out: "\n\tdisabled services = {\n\t}\n"}},
			}}

			c := f.converger(t)
			c.logDir = c.agentsDir

			if err := os.WriteFile(filepath.Join(c.agentsDir, deploy.NodeAgentLabel+".plist"),
				[]byte(deploy.NodeAgent), 0o600); err != nil {
				t.Fatalf("stage the agent: %v", err)
			}

			plan, err := c.Plan(t.Context(), lifeops.UpRequest{
				ConfigPath: filepath.Join(c.agentsDir, "billet.yaml"),
				WantNode:   true,
			})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}

			if len(plan.Units) != 0 {
				t.Errorf("`up` planned to act on a service that is %s: %+v", state, plan.Units)
			}

			if len(plan.Refusals) != 1 {
				t.Fatalf("a service that is %s produced %d refusals, want exactly the one about "+
					"its state: %+v", state, len(plan.Refusals), plan.Refusals)
			}

			if !strings.Contains(plan.Refusals[0].Error(), state) {
				t.Errorf("the refusal does not name the state it saw: %s", plan.Refusals[0].Error())
			}
		})
	}
}

// nodePrint renders a `launchctl print` reply for a node loaded from exactly the
// agent billet ships, in the given state.
func nodePrint(t *testing.T, state string, pid int) string {
	t.Helper()

	want, err := declaredAgent(deploy.NodeAgent)
	if err != nil {
		t.Fatalf("declaredAgent: %v", err)
	}

	var b strings.Builder

	fmt.Fprintf(&b, "gui/501/%s = {\n", deploy.NodeAgentLabel)
	fmt.Fprintf(&b, "\tstate = %s\n", state)
	fmt.Fprintf(&b, "\tpid = %d\n", pid)
	fmt.Fprintf(&b, "\truns = 1\n")
	fmt.Fprintf(&b, "\texit timeout = %d\n", want.ExitTimeout)
	fmt.Fprintf(&b, "\tprogram = %s\n", want.Arguments[0])
	b.WriteString("\targuments = {\n")

	for _, a := range want.Arguments {
		fmt.Fprintf(&b, "\t\t%s\n", a)
	}

	b.WriteString("\t}\n\tenvironment = {\n")

	for _, k := range sortedKeys(want.Environment) {
		fmt.Fprintf(&b, "\t\t%s => %s\n", k, want.Environment[k])
	}

	b.WriteString("\t}\n}\n")

	return b.String()
}

// THE DIRECTORY LIST DE-DUPLICATES, AND CARRIES NO EMPTY NAME.
//
// Duplicates are the ORDINARY case rather than a corner: a single-machine
// deployment puts its config, key, state and lock directories under one root, so
// most of the list collapses to a handful.
//
// The empty name is the case that was found by RUNNING this on a real Mac. It
// produced a refusal about a directory named "" whose remedy was `sudo mkdir -p
// && sudo chown junioryono ` — and the cause was this package's own logDir,
// which New() never initialised. Every caller guarded its own inputs; the value
// nobody thought to guard was the one that was wrong, which is why the guard now
// sits where all of them pass through.
func TestRequiredDirsSurvivesItsOwnDeduplication(t *testing.T) {
	t.Parallel()

	c := &Converger{agentsDir: "/agents", logDir: "/logs"}

	got := c.requiredDirs(lifeops.UpRequest{
		// Every one of these shares a parent, which is what makes the list
		// collapse and the aliasing bite.
		ConfigPath:     "/one/root/billet.yaml",
		KeyPaths:       []string{"/one/root/app-private-key.pem"},
		ServerStateDir: "/one/root/server",
		NodeStateDir:   "/one/root/server",
		NodeLockDir:    "/one/root/locks",
	})

	want := []string{"/agents", "/logs", "/one/root", "/one/root/locks", "/one/root/server"}

	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("requiredDirs = %q, want %q", got, want)
	}

	// AND NOT ONE EMPTY NAME, said as its own assertion because that is the
	// value that produced a dangerous command rather than a wrong one.
	for _, dir := range got {
		if dir == "" {
			t.Fatalf("an empty directory name reached the plan: %q", got)
		}
	}
}

// AND AN ABSENT PATH IS NOT A DIRECTORY REQUIREMENT.
//
// A node-only host has no server state directory and a config with no App key
// has no key path. Asking for "" would refuse every such host, with a remedy
// naming nothing.
func TestRequiredDirsIgnoresPathsTheConfigDoesNotSet(t *testing.T) {
	t.Parallel()

	c := &Converger{agentsDir: "/agents", logDir: "/logs"}

	got := c.requiredDirs(lifeops.UpRequest{NodeStateDir: "/node"})

	for _, dir := range got {
		if dir == "" {
			t.Fatalf("an unset path became a directory requirement: %q", got)
		}
	}

	if len(got) != 3 {
		t.Errorf("requiredDirs = %q, want just the agents, log and node directories", got)
	}
}

// A REMEDY ASKS FOR ROOT ONLY WHERE ROOT IS WHAT IS MISSING.
//
// /usr/local is root-owned on a stock Mac, so those directories genuinely need
// `sudo`. A path inside the operator's own home does not — and telling somebody
// to `sudo chown` their own home directory to themselves is at best a no-op, and
// teaches them to paste sudo at anything billet prints.
func TestTheDirectoryRemedyAsksForRootOnlyWhenItIsNeeded(t *testing.T) {
	t.Parallel()

	c := &Converger{}

	// A directory this account owns: its parent is writable, so no sudo.
	mine := filepath.Join(t.TempDir(), "not-there-yet")
	if got := c.makeDirCommand(mine); strings.Contains(got, "sudo") {
		t.Errorf("the remedy asks for root inside a directory this account owns: %s", got)
	}

	// A directory under a root-owned tree: sudo, and a chown, because the
	// directory `sudo mkdir` creates would belong to root.
	got := c.makeDirCommand("/usr/local/var/log/billet")

	if !strings.Contains(got, "sudo mkdir -p") {
		t.Errorf("the remedy does not create the directory with root: %s", got)
	}

	if !strings.Contains(got, "chown") {
		t.Errorf("the remedy creates a root-owned directory and never hands it back, so the "+
			"agent still cannot write it: %s", got)
	}
}

// New FILLS EVERY PATH IT WILL LATER CHECK.
//
// THIS IS THE TEST THE ESCAPED BUG NEEDED. `logDir` was declared, documented,
// given an option — and never set by New(), so it was "" on every real run. The
// plan then asked whether "" was writable, refused because it was not, and told
// the operator to `sudo mkdir -p  && sudo chown junioryono `. Nothing caught it
// because every test that reached the plan set the field explicitly, which is
// exactly the shape of a constructor bug: the tests configure what production
// leaves to the default.
//
// So this asserts the DEFAULTS rather than a configured converger, and it checks
// the paths against the plists that have to agree with them.
func TestNewFillsEveryPathItChecks(t *testing.T) {
	t.Parallel()

	c := New()

	if c.agentsDir == "" {
		t.Error("agentsDir is empty, so billet would install agents into /Library/LaunchAgents " +
			"or check a directory named \"\"")
	}

	if c.logDir == "" {
		t.Fatal("logDir is empty; the plan checks it, so every macOS `up` refuses with a " +
			"remedy naming no directory")
	}

	// THE SAME LITERAL THE AGENTS NAME. launchd performs no variable
	// substitution, so a plist path is a literal and billet checking a different
	// one checks a directory nothing uses.
	for name, agent := range map[string]string{
		deploy.ServerAgentName: deploy.ServerAgent,
		deploy.NodeAgentName:   deploy.NodeAgent,
	} {
		if !strings.Contains(agent, c.logDir) {
			t.Errorf("%s does not write its log under %s, which is the directory billet "+
				"checks and creates", name, c.logDir)
		}
	}

	// AND THE AGENTS DIRECTORY IS THE ONE launchd SCANS AT LOGIN, under a home
	// resolved from the account database rather than from $HOME — which anybody
	// can set, and which would have billet install into a directory nothing
	// loads while every surface reported success.
	if !strings.HasSuffix(c.agentsDir, filepath.Join("Library", "LaunchAgents")) {
		t.Errorf("agentsDir = %q, which is not where launchd looks at login", c.agentsDir)
	}
}

// A bootstrap IS NOT A SPAWN, and waiting for the process is what makes a
// healthy Mac's first `up` work.
//
// `launchctl bootstrap` returns once launchd has ACCEPTED the job; the process
// appears afterwards, and a freshly bootstrapped agent reports `spawn scheduled`
// with no pid. Sampling once and concluding there was nothing to prove is not
// merely a bad message: launchd's plan sets EnableBeforeStart, so the failure
// unwinds — and unwinding an install REMOVES the agent. A healthy first `up`
// would have failed and deleted the plist it had just written, every time.
func TestStartAndProveWaitsForLaunchdToGiveTheJobAProcess(t *testing.T) {
	t.Parallel()

	f := &fake{
		t:     t,
		ticks: 20,
		replies: map[string][]reply{
			"bootstrap": {{}},
			"print": {
				// Accepted, not yet started. This is what launchd really says.
				{out: nodePrint(t, "spawn scheduled", 0)},
				{out: nodePrint(t, "spawn scheduled", 0)},
				// And now it has a process, which does not change again.
				{out: nodePrint(t, "running", 4242)},
			},
		},
	}

	c := f.converger(t)

	if err := os.WriteFile(filepath.Join(c.agentsDir, deploy.NodeAgentLabel+".plist"),
		[]byte(deploy.NodeAgent), 0o600); err != nil {
		t.Fatalf("stage the agent: %v", err)
	}

	proof, err := c.StartAndProve(t.Context(), deploy.NodeAgentLabel)
	if err != nil {
		t.Fatalf("a service launchd had accepted but not yet started was reported broken: %v", err)
	}

	// AND WHAT IT SAYS IT PROVED IS WHAT IT PROVED. launchd has no readiness
	// notification, so the sentence must not claim one.
	if strings.Contains(proof, "ready") {
		t.Errorf("the proof claims readiness, which launchd cannot report: %q", proof)
	}

	if !strings.Contains(proof, "settle window") {
		t.Errorf("the proof does not say what was actually established: %q", proof)
	}
}

// AND AN UNINSTALL THAT COULD NOT CLEAR THE OVERRIDE SAYS SO.
//
// The override outlives the plist, so a failure here is the landmine left armed:
// the agent is gone and the label is still disabled, and the NEXT install
// bootstraps a service launchd silently refuses to run. Reporting the removal as
// a success would leave nothing on the machine that explains it.
func TestUninstallReportsAnOverrideItCouldNotClear(t *testing.T) {
	t.Parallel()

	// BOTH WAYS launchctl CAN FAIL, because they are separate branches and the
	// consequence is identical: the agent is gone and the label is still
	// disabled. A test covering one leaves the other free to be deleted.
	for name, staged := range map[string]reply{
		"launchctl refused":     {out: "Could not enable service", code: 1},
		"launchctl did not run": {err: errors.New("launchctl: no such file")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := &fake{t: t, replies: map[string][]reply{"enable": {staged}}}

			c := f.converger(t)
			label := deploy.NodeAgentLabel

			if err := os.WriteFile(filepath.Join(c.agentsDir, label+".plist"),
				[]byte(deploy.NodeAgent), 0o600); err != nil {
				t.Fatalf("stage an installed agent: %v", err)
			}

			err := c.Uninstall(t.Context(), label)
			if err == nil {
				t.Fatal("an uninstall that left the label disabled reported success")
			}

			// THE COMMAND THAT FIXES IT, because nothing else on the machine
			// will say what is wrong when the next install fails.
			if !strings.Contains(err.Error(), "launchctl enable") {
				t.Errorf("the error does not name how to clear the override: %v", err)
			}
		})
	}
}
