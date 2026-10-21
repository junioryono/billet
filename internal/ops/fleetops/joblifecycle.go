package fleetops

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/provider"
)

// renderLifecycle writes how a job's lease lived: when each part of its life
// began and how long after the one before, the runner GitHub registered, the
// node processes that launched and held it, why it failed, and what each cache
// did. The text report and the job page both word it through here.
func renderLifecycle(line func(label, format string, args ...any), rec alloc.JobRecord) {
	phases := rec.Phases()
	if len(phases) == 0 {
		line("phases", "none recorded")
	}
	for i, p := range phases {
		label := ""
		if i == 0 {
			label = "phases"
		}
		step := ""
		if i > 0 {
			if d, ok := between(phases[i-1].At, p.At); ok {
				step = "  +" + d.String()
			}
		}
		line(label, "%-11s %s%s", p.Name, p.At, step)
	}

	runner := "not recorded"
	if rec.RunnerID > 0 {
		runner = "GitHub runner " + strconv.FormatInt(rec.RunnerID, 10)
	}
	line("runner", "%s, instance %s", runner, provider.InstanceName(rec.LeaseID))
	line("held by", "%s", describeProcesses(rec.LaunchIncarnation, rec.HolderIncarnation, rec.QuarantinedAt != ""))
	line("failure", "%s", describeFailure(rec))
	line("caches", "%s", describeCaches(rec.Caches))
}

// runOf names the run a job belongs to: the one that started on its runner,
// with the run its lease was assigned when GitHub gave the runner another run's
// job.
func runOf(rec alloc.JobRecord) string {
	run := identifier(rec.StartedRun())
	if rec.GitHubRunID > 0 && rec.RunID > 0 && rec.RunID != rec.GitHubRunID {
		run += ", its lease assigned run " + strconv.FormatInt(rec.RunID, 10)
	}

	return run
}

// between is how long after one of billet's stamps another came, false when
// either does not parse.
func between(from, to string) (time.Duration, bool) {
	a, err := time.Parse(time.RFC3339Nano, from)
	if err != nil {
		return 0, false
	}
	b, err := time.Parse(time.RFC3339Nano, to)
	if err != nil {
		return 0, false
	}

	return b.Sub(a).Round(time.Millisecond), true
}

// describeProcesses names the node process that launched a job and the one
// the ledger last recorded holding its compute.
//
// A DIFFERENT HOLDER IS A REPLACEMENT that adopted the compute, the host's next
// process after a restart or an upgrade, and the report says so; an unknown
// side is said in that word, never read as a replacement. A QUARANTINED LEASE'S
// HOLDER IS NOT RECORDED after the quarantine: the process that adopts it
// moves no phase, so the last holder recorded is the one from before, and the
// report says that rather than that it held the compute to the end.
func describeProcesses(launched, held string, quarantined bool) string {
	if quarantined {
		var parts []string
		if launched != "" {
			parts = append(parts, "launched by process "+processName(launched))
		}
		if held != "" {
			parts = append(parts, "last recorded holding it, before its quarantine, process "+processName(held))
		}

		return strings.Join(append(parts, "the process that held it after its quarantine is not recorded"), "; ")
	}

	switch {
	case launched == "" && held == "":
		return "not recorded"
	case held == "":
		return "launched by process " + processName(launched) + "; the process holding it at the end is not recorded"
	case launched == "":
		return "held at the end by process " + processName(held) + "; the process that launched it is not recorded"
	case launched == held:
		return "launched and held to the end by process " + processName(launched)
	}

	return "launched by process " + processName(launched) + ", held at the end by process " +
		processName(held) + ", which replaced it"
}

// processName is a node process's incarnation as `billet leases` shows it, or
// quoted when it is not the id billet mints: a node names its own process, and
// a name with a newline in it would forge a line of the report.
func processName(s string) string {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF-", r) {
			return strconv.Quote(s)
		}
	}

	return shortIncarnation(s)
}

// describeFailure is why billet ended the job's lease as it did, and what its
// own infrastructure did to the job while it ran. Two facts, not a verdict:
// `billet leases failures` says the same of the disruption.
func describeFailure(rec alloc.JobRecord) string {
	var parts []string
	if rec.FailureReason != "" {
		parts = append(parts, "reason "+strconv.Quote(rec.FailureReason))
	}
	if rec.Disruption != "" {
		disruption := "disrupted by billet: " + strconv.Quote(rec.Disruption)
		if rec.DisruptedAt != "" {
			disruption += " at " + rec.DisruptedAt
		}
		parts = append(parts, disruption)
	}
	if len(parts) == 0 {
		return "none recorded"
	}

	return strings.Join(parts, "; ")
}

// describeCaches is what each cache did for the job, as the node observed it.
// A token this binary does not know is a newer binary's, shown quoted rather
// than dropped.
func describeCaches(c alloc.CacheObservation) string {
	image := cacheToken(string(c.ImageCache), c.ImageCache.Valid())
	if c.CacheGeneration != "" {
		image += " (generation " + strconv.Quote(c.CacheGeneration) + ")"
	}

	return fmt.Sprintf("image %s, actions %s, sticky %s, git %s, bazel %s, go %s", image,
		cacheToken(string(c.ActionsCache), c.ActionsCache.Valid()),
		cacheToken(string(c.Sticky), c.Sticky.Valid()), cacheToken(string(c.Git), c.Git.Valid()),
		cacheToken(string(c.Bazel), c.Bazel.Valid()), cacheToken(string(c.Go), c.Go.Valid()))
}

func cacheToken(token string, known bool) string {
	switch {
	case token == "":
		return "not observed"
	case known:
		return token
	}

	return strconv.Quote(token)
}
