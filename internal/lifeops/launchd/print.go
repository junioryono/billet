// Package launchd drives macOS launch agents for billet's local lifecycle
// commands, as internal/lifeops drives systemd for Linux.
//
// THE TWO ARE SIBLINGS RATHER THAN ONE ABSTRACTION, deliberately. The ORDER the
// lifecycle commands impose is shared and is their entire safety content; the
// vocabulary underneath it is not, and the facts each service manager reports
// have almost no overlap. lifeops.ServiceFacts carries FragmentPath, DropInPaths,
// ReloadPending and ExecStartFlags because systemd's refusals are computed from
// them, and launchd has none of those while having a durable disabled-override
// database systemd has no equivalent for. What the two share — Refusal,
// Tristate, and the plan vocabulary — is imported from lifeops rather than
// copied.
//
// Every launchd behaviour this package relies on was MEASURED on macOS 26 by
// running real agents, not read from launchd.plist(5), which is wrong about at
// least one of them. The measurements live in reallaunchd_test.go so they
// cannot rot.
package launchd

import (
	"fmt"
	"strconv"
	"strings"
)

// Job is what launchd reports about a service it has LOADED.
//
// THIS IS NOT WHAT THE PLIST ON DISK SAYS, and the difference is the single
// most dangerous thing about this service manager. launchd reads a plist ONCE,
// at bootstrap, and keeps what it read: replacing the file changes nothing about
// the running job. Measured — a loaded agent went on reporting `exit timeout =
// 20` and its original environment after its plist had been rewritten to say 99
// and something else.
//
// So a node can be running with a stale ExitTimeOut while its plist is
// byte-identical to the one billet ships, and comparing the file would certify
// it. At the next logout launchd SIGKILLs that node five seconds into a drain
// that was allowed 88200. These fields are what makes the loaded job checkable
// instead.
type Job struct {
	Label string
	// Path is the plist launchd loaded this job from, which is not necessarily
	// the plist that is there now.
	Path string
	// State is launchd's own word: `running`, `spawn scheduled` (it intends to
	// start this again), `SIGTERMed` (it is stopping and the process is still
	// alive), and others this package does not enumerate — an unrecognised
	// state is never read as proof of anything.
	State string
	// PID is the process launchd currently associates with the job. Known says
	// whether launchd reported one at all, which is a different fact from zero:
	// a job that is between runs has no pid line, and so does one launchd
	// declined to answer about.
	PID      int
	PIDKnown bool
	// Runs is how many times launchd has started this job. It is the closest
	// thing to systemd's NRestarts, and with the pid it is the ONLY evidence
	// available for a stability check — launchctl reports no start timestamp.
	Runs      int
	RunsKnown bool
	// LastExit is the exit status of the previous run. Measured: an agent whose
	// program does not exist reports `last exit code = 78: EX_CONFIG`, which is
	// how a mistyped binary path surfaces.
	LastExit      int
	LastExitKnown bool
	// ExitTimeout is the grace launchd will allow between SIGTERM and SIGKILL,
	// as LOADED. billet's node answers SIGTERM by draining for as long as the
	// jobs take, so this being the value billet expects is a safety property
	// rather than a detail.
	ExitTimeout      int
	ExitTimeoutKnown bool

	Program     string
	Arguments   []string
	Environment map[string]string
}

// Running reports whether launchd has a live process for this job.
//
// A PID IS THE ONLY EVIDENCE THAT COUNTS. `state` is launchd's intent as much as
// its observation — `spawn scheduled` means it means to start one, and
// `SIGTERMed` means a process is alive and stopping — so a caller asking "is
// something executing" is asking about the pid.
func (j Job) Running() bool { return j.PIDKnown && j.PID > 0 }

// parsePrint reads `launchctl print <service-target>` output for target.
//
// STRUCTURE COMES FROM INDENTATION, NOT FROM THE TEXT OF A LINE. launchd indents
// each level with a tab, and keying off that is what makes the parser safe
// against its own data: an ARGUMENT that is literally `}` closed the block early
// in the first version and corrupted every field after it, and any path or
// value ending in `{` opened a block that was never there. Neither can happen
// when the depth of a line is decided before its content is looked at.
//
// THE TARGET IS COMPARED, not merely noted. `launchctl print` is asked about one
// service and its reply names which; accepting a reply about a different one
// would let this package answer a question it was not asked — and the answers
// here decide whether a process is running.
//
// The environment is the specific trap the depth guards: launchd prints THREE
// environment blocks, `inherited environment`, `default environment` and
// `environment`. A prefix or contains match finds the inherited one, which is
// the session's, and a PATH read from it would make billet certify a node that
// cannot find tart.
func parsePrint(target, out string) (Job, error) {
	var (
		job    Job
		opened bool
		closed bool
		// block is the indent-1 key whose braces the current line sits inside.
		block string
	)

	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}

		indent := 0
		for indent < len(raw) && raw[indent] == '\t' {
			indent++
		}

		switch {
		case closed:
			return Job{}, fmt.Errorf("launchd: `launchctl print %s` has content after the "+
				"service description ended, so billet cannot tell what it describes", target)

		case indent == 0 && line == "}":
			if !opened {
				return Job{}, notPrintOutput(target, out)
			}

			closed = true

		case indent == 0:
			head, ok := strings.CutSuffix(line, "{")
			if !ok || opened {
				return Job{}, notPrintOutput(target, out)
			}

			// EXACTLY THE SERVICE THAT WAS ASKED ABOUT.
			if got := blockKey(head); got != target {
				return Job{}, fmt.Errorf("launchd: asked `launchctl print` about %s and it "+
					"answered about %s", target, got)
			}

			job.Label = labelOf(target)
			opened = true

		case !opened:
			return Job{}, notPrintOutput(target, out)

		case indent == 1 && line == "}":
			block = ""

		case indent == 1:
			if head, ok := strings.CutSuffix(line, "{"); ok {
				block = blockKey(head)

				continue
			}

			name, value, ok := strings.Cut(line, " = ")
			if !ok {
				continue
			}

			if err := setField(&job, strings.TrimSpace(name), strings.TrimSpace(value)); err != nil {
				return Job{}, err
			}

		case indent == 2 && block == "arguments":
			// argv, one per line. A line here is an ARGUMENT whatever it says,
			// including `}` — which is why depth decides and content does not.
			job.Arguments = append(job.Arguments, line)

		case indent == 2 && block == "environment":
			// EXACTLY `environment`, never `inherited environment` or `default
			// environment`: those belong to the session and to launchd.
			if name, value, ok := strings.Cut(line, " => "); ok {
				if job.Environment == nil {
					job.Environment = map[string]string{}
				}
				job.Environment[strings.TrimSpace(name)] = strings.TrimSpace(value)
			}
		}
	}

	// A TRUNCATED REPLY IS AN ERROR, NOT AN EMPTY JOB. Without this a reply cut
	// off after its first line parsed into a Job whose zero values say "not
	// running, no pid, no timeout" — which reads as a service proved idle, and
	// is how a stop gets reported against a process nobody looked at.
	if !opened || !closed {
		return Job{}, fmt.Errorf("launchd: `launchctl print %s` was cut off before it finished "+
			"describing the service, so nothing in it can be relied on", target)
	}

	return job, nil
}

// notPrintOutput reports a reply that is not a service description at all.
func notPrintOutput(target, out string) error {
	return fmt.Errorf("launchd: this is not `launchctl print` output for %s: %q",
		target, firstLine(out))
}

// setField records one top-level `name = value`.
//
// A FIELD THAT IS THERE AND UNREADABLE IS AN ERROR, which is not the same as one
// that is absent. An absent pid means launchd is not running the job; a pid
// billet could not parse means launchd said something this build does not
// understand, and continuing would answer "no process" to a question nobody
// actually asked launchd.
func setField(j *Job, name, value string) error {
	var (
		into  *int
		known *bool
		raw   = value
	)

	switch name {
	case "path":
		j.Path = value

		return nil
	case "state":
		j.State = value

		return nil
	case "program":
		j.Program = value

		return nil
	case "pid":
		into, known = &j.PID, &j.PIDKnown
	case "runs":
		into, known = &j.Runs, &j.RunsKnown
	case "exit timeout":
		into, known = &j.ExitTimeout, &j.ExitTimeoutKnown
	case "last exit code":
		// Measured: launchd renders this as `78: EX_CONFIG`, so the symbolic
		// half is cut before parsing. Taking the whole string as a number would
		// answer "unknown" for every job that has ever exited — which is exactly
		// the population worth asking about.
		raw, _, _ = strings.Cut(value, ":")
		raw = strings.TrimSpace(raw)
		into, known = &j.LastExit, &j.LastExitKnown
	default:
		return nil
	}

	// A PARENTHESISED VALUE IS launchd SAYING "there is no value", which is
	// absence rather than corruption. Measured, and it was found by the
	// real-launchd test rather than reasoned about: a job that has never exited
	// reports `last exit code = (never exited)`, and the same convention carries
	// `(unlimited)` for the jetsam limits. Treating those as unreadable made
	// every freshly started agent an error, so billet could not read a single
	// healthy job.
	if strings.HasPrefix(raw, "(") {
		return nil
	}

	n, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("launchd: `launchctl print` reported %s = %q, which is neither a "+
			"number nor one of the parenthesised words launchd uses for absence",
			name, value)
	}

	*into, *known = n, true

	return nil
}

// blockKey takes the key out of the text before a `{`.
//
// THE SPACE IS TRIMMED ON BOTH SIDES OF THE `=`, which is not fussiness: the
// first version trimmed the `=` before the space that precedes it, so every key
// came back with a trailing `=` — the environment block was never recognised,
// and the parser silently returned a job with no environment at all. That is a
// PATH billet would then have found nothing wrong with.
func blockKey(head string) string {
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(head), "="))
}

// labelOf takes the service name out of a domain target: the text after the
// last slash of `gui/501/sh.billet.node`.
func labelOf(target string) string {
	if i := strings.LastIndex(target, "/"); i >= 0 {
		return target[i+1:]
	}

	return target
}

// firstLine bounds what an unparseable reply puts in an error.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if len(line) > 120 {
		return line[:120] + "…"
	}

	return line
}
