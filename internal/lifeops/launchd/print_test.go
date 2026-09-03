package launchd

import (
	"strings"
	"testing"
)

// realPrint is `launchctl print` output captured from a REAL launch agent on
// macOS 26, shaped exactly like the node agent billet ships: the same program,
// the same `--config` argument, the same PATH, ExitTimeOut 88200.
//
// CAPTURED RATHER THAN WRITTEN, because a fixture written from a reading of the
// format tests the reading. This one carries the two things that break a naive
// parser and that nobody would think to invent: three separate environment
// blocks, and several nested blocks whose keys collide with the top-level ones
// this parser wants.
//
// It is an agent whose program does not exist, which is why it is not running —
// that is the state a mistyped binary path leaves, and `last exit code = 78:
// EX_CONFIG` is how it says so.
const realPrint = `gui/501/sh.billet.capture = {
	active count = 0
	path = /private/tmp/claude-501/-Users-junioryono-Documents-Projects-billet/21d983c2-12da-4848-b887-d3683d89bad1/scratchpad/sh.billet.capture.plist
	type = LaunchAgent
	state = spawn scheduled

	program = /usr/local/bin/billet
	arguments = {
		/usr/local/bin/billet
		node
		--config
		/usr/local/etc/billet/billet.yaml
	}

	stdout path = /dev/null
	stderr path = /dev/null
	inherited environment = {
		SSH_AUTH_SOCK => /private/tmp/com.apple.launchd.yI0G9AEOcZ/Listeners
	}

	default environment = {
		PATH => /usr/bin:/bin:/usr/sbin:/sbin
	}

	environment = {
		OSLogRateLimit => 64
		PATH => /opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
		XPC_SERVICE_NAME => sh.billet.capture
	}

	domain = gui/501 [100023]
	asid = 100023
	minimum runtime = 5
	exit timeout = 88200
	runs = 1
	last exit code = 78: EX_CONFIG

	semaphores = {
		successful exit => 0
	}

	event triggers = {
		sh.billet.capture => {
			keepalive = 0
			service = sh.billet.capture
			stream = com.apple.launchd.helper
			monitor = com.apple.UserEventAgent-Aqua
			descriptor = {
				"Executable" => "/usr/local/bin/billet"
				"SkipImmediatePoll" => false
			}
		}
	}

	event channels = {
		"com.apple.launchd.helper" = {
			port = 0x0
			active = 0
			managed = 1
			reset = 0
			hide = 0
			watching = 0
		}
	}

	resource coalition = {
		ID = 713839
		type = resource
		state = active
		active count = 1
		name = sh.billet.capture
	}

	jetsam coalition = {
		ID = 713840
		type = jetsam
		state = active
		active count = 1
		name = sh.billet.capture
	}

	spawn type = interactive (4)
	jetsam priority = 40
	jetsam memory limit (active) = (unlimited)
	jetsam memory limit (inactive) = (unlimited)
	jetsamproperties category = daemon
	jetsam thread limit = 32
	cpumon = default

	properties = runatload | penalty box | inferred program
}`

// THE PARSER IS TESTED AGAINST WHAT launchd ACTUALLY EMITS.
func TestParsePrintReadsARealAgent(t *testing.T) {
	t.Parallel()

	job, err := parsePrint("gui/501/sh.billet.capture", realPrint)
	if err != nil {
		t.Fatalf("parsePrint: %v", err)
	}

	if job.Label != "sh.billet.capture" {
		t.Errorf("Label = %q, want the service name out of the domain target", job.Label)
	}

	if job.State != "spawn scheduled" {
		t.Errorf("State = %q, want launchd's own word for it", job.State)
	}

	if job.Program != "/usr/local/bin/billet" {
		t.Errorf("Program = %q", job.Program)
	}

	// THE LOADED ExitTimeOut, which is the whole reason this parser exists: it
	// is the number that decides whether a draining node is SIGKILLed, and it
	// belongs to the job launchd loaded rather than to the plist on disk.
	if !job.ExitTimeoutKnown || job.ExitTimeout != 88200 {
		t.Errorf("ExitTimeout = %d (known %v), want 88200", job.ExitTimeout, job.ExitTimeoutKnown)
	}

	if !job.RunsKnown || job.Runs != 1 {
		t.Errorf("Runs = %d (known %v), want 1", job.Runs, job.RunsKnown)
	}

	// `last exit code = 78: EX_CONFIG`. A reader that took the whole value as a
	// number would answer "unknown" for every job that has ever exited.
	if !job.LastExitKnown || job.LastExit != 78 {
		t.Errorf("LastExit = %d (known %v), want 78 out of `78: EX_CONFIG`",
			job.LastExit, job.LastExitKnown)
	}

	// NOT RUNNING, and the evidence is the absent pid rather than the state.
	if job.PIDKnown {
		t.Errorf("PIDKnown = true for a job launchd printed no pid for (PID %d)", job.PID)
	}

	if job.Running() {
		t.Error("a job with no pid was reported as running")
	}

	want := []string{"/usr/local/bin/billet", "node", "--config", "/usr/local/etc/billet/billet.yaml"}
	if strings.Join(job.Arguments, " ") != strings.Join(want, " ") {
		t.Errorf("Arguments = %q, want %q", job.Arguments, want)
	}
}

// THE ENVIRONMENT TRAP, which is the reason the parser tracks depth at all.
//
// launchd prints THREE environment blocks — `inherited environment`, `default
// environment` and `environment` — and only the last is the job's. The default
// one contains a PATH, and it is launchd's own: /usr/bin:/bin:/usr/sbin:/sbin,
// with no Homebrew prefix. A parser that matched "environment" loosely reads
// that one, and billet would then certify a node whose PATH cannot find tart —
// the exact failure the node agent's PATH exists to prevent.
func TestParsePrintTakesTheJobsEnvironmentAndNotTheOthers(t *testing.T) {
	t.Parallel()

	job, err := parsePrint("gui/501/sh.billet.capture", realPrint)
	if err != nil {
		t.Fatalf("parsePrint: %v", err)
	}

	const want = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

	if got := job.Environment["PATH"]; got != want {
		t.Errorf("PATH = %q, want the JOB's; launchd's own default is "+
			"/usr/bin:/bin:/usr/sbin:/sbin and reading that one certifies a node "+
			"that cannot find tart", got)
	}

	// AND NOTHING FROM THE INHERITED BLOCK, which is the session's.
	if _, ok := job.Environment["SSH_AUTH_SOCK"]; ok {
		t.Error("the inherited environment leaked into the job's")
	}
}

// AND NOTHING FROM A NESTED BLOCK IS TAKEN FOR A TOP-LEVEL FACT.
//
// The real output nests several levels: `event triggers` carries a `descriptor`
// holding "Executable", and `resource coalition` carries `state = active` and
// `active count`. A reader that searched the whole blob for `state` would report
// this stopped agent as active.
func TestParsePrintIgnoresNestedBlocksWithCollidingKeys(t *testing.T) {
	t.Parallel()

	job, err := parsePrint("gui/501/sh.billet.capture", realPrint)
	if err != nil {
		t.Fatalf("parsePrint: %v", err)
	}

	// `resource coalition` and `jetsam coalition` both carry `state = active`.
	if job.State == "active" {
		t.Error("a nested coalition's state was read as the job's; this agent is not running")
	}

	// The fixture really does contain the trap, so this test cannot pass by the
	// trap being absent.
	if !strings.Contains(realPrint, "state = active") {
		t.Fatal("the fixture no longer contains a nested `state = active`, so this proves nothing")
	}

	if !strings.Contains(realPrint, `"Executable"`) {
		t.Fatal("the fixture no longer contains a nested descriptor, so this proves nothing")
	}
}

// A REPLY BILLET CANNOT FULLY READ IS AN ERROR, NOT AN EMPTY JOB.
//
// `launchctl print` exits 113 for a service that is not loaded, and that case is
// handled by the exit status. Everything here is a reply that LOOKS like output
// and is not usable, and the danger is uniform: a Job built out of zero values
// says "not running, no pid, no timeout", which reads as a service proved idle.
// Acting on that stops a host while a process nobody looked at keeps running.
func TestParsePrintRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()

	const target = "gui/501/sh.billet.node"

	for name, in := range map[string]string{
		"empty":    "",
		"an error": `Could not find service "sh.billet.node" in domain for login`,
		// Fields with no service line around them.
		"no service line": "\tstate = running\n\tpid = 42\n",
		// TRUNCATED. The first version returned a valid zero Job for this.
		"cut off after the opener": target + " = {\n",
		"cut off mid-description":  target + " = {\n\tstate = running\n\tpid = 42\n",
		// A reply about a DIFFERENT service than the one asked about.
		"another service": "gui/501/com.example.other = {\n\tstate = running\n}\n",
		// Two services in one reply.
		"two services": target + " = {\n}\n" + target + " = {\n}\n",
		// Content after the description ended.
		"trailing content": target + " = {\n}\n\tstate = running\n",
		// A number field that is present and unreadable is NOT the same as an
		// absent one, and must not silently become "billet does not know".
		"an unreadable pid":     target + " = {\n\tpid = later\n}\n",
		"an unreadable runs":    target + " = {\n\truns = many\n}\n",
		"an unreadable timeout": target + " = {\n\texit timeout = never\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := parsePrint(target, in); err == nil {
				t.Error("billet built a Job out of a reply it could not read; a zero Job reads " +
					"as a service proved idle")
			}
		})
	}
}

// AND ITS OWN DATA CANNOT CHANGE ITS STRUCTURE.
//
// This is why the parser keys off INDENTATION rather than off the text of a
// line. An argument that is literally `}` closed the arguments block early in
// the first version, and every field after it was then read at the wrong depth —
// silently, producing a Job that parsed cleanly and described something else.
func TestParsePrintIsNotConfusedByItsOwnValues(t *testing.T) {
	t.Parallel()

	const target = "gui/501/sh.billet.node"

	job, err := parsePrint(target, target+` = {
	program = /usr/local/bin/billet
	arguments = {
		/usr/local/bin/billet
		}
		--config
		{
		/usr/local/etc/billet/billet.yaml
	}
	state = running
	pid = 4242
	environment = {
		TRICK => a => b
		PATH => /opt/homebrew/bin
	}
}
`)
	if err != nil {
		t.Fatalf("parsePrint: %v", err)
	}

	// EVERY argument SURVIVED, braces included.
	want := []string{"/usr/local/bin/billet", "}", "--config", "{", "/usr/local/etc/billet/billet.yaml"}
	if strings.Join(job.Arguments, "|") != strings.Join(want, "|") {
		t.Errorf("Arguments = %q, want %q", job.Arguments, want)
	}

	// AND THE FIELDS AFTER THE BLOCK WERE STILL READ AT THE RIGHT DEPTH, which
	// is the failure a `}` argument caused.
	if job.State != "running" {
		t.Errorf("State = %q; the fields after the arguments block were lost", job.State)
	}
	if !job.PIDKnown || job.PID != 4242 {
		t.Errorf("PID = %d (known %v), want 4242", job.PID, job.PIDKnown)
	}

	// An environment VALUE containing another ` => ` keeps its whole value.
	if got := job.Environment["TRICK"]; got != "a => b" {
		t.Errorf("TRICK = %q, want the whole value", got)
	}
}

// AN ABSENT pid AND AN UNREADABLE ONE ARE DIFFERENT FACTS.
//
// THE FIRST VERSION OF THIS TEST COULD NOT FAIL for the case it was named after.
// It asserted `if job.Running() && !known { error }` — and an unreadable pid
// produced Running() == false, so the assertion was never reached. The real
// answer is that an unreadable pid is not a Job at all; it is a reply billet
// cannot read, and it is refused above.
func TestParsePrintSeparatesAnAbsentPidFromAZeroOne(t *testing.T) {
	t.Parallel()

	const target = "gui/501/sh.billet.node"

	for name, tc := range map[string]struct {
		line    string
		known   bool
		pid     int
		running bool
	}{
		"a real pid":  {"\tpid = 4242\n", true, 4242, true},
		"no pid line": {"", false, 0, false},
		// launchd naming pid 0 is not a process, but it IS an answer — and it
		// is not the same fact as declining to name one.
		"pid zero": {"\tpid = 0\n", true, 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			job, err := parsePrint(target, target+" = {\n\tstate = running\n"+tc.line+"}\n")
			if err != nil {
				t.Fatalf("parsePrint: %v", err)
			}

			if job.PIDKnown != tc.known || job.PID != tc.pid {
				t.Errorf("PID = %d (known %v), want %d (known %v)",
					job.PID, job.PIDKnown, tc.pid, tc.known)
			}

			if job.Running() != tc.running {
				t.Errorf("Running() = %v, want %v", job.Running(), tc.running)
			}
		})
	}
}
