package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/junioryono/billet/internal/alloc"
)

// cmdJobs is the operator's view of what jobs did.
func cmdJobs(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet jobs show <lease>")
	}
	if args[0] == "show" {
		return cmdJobsShow(ctx, args[1:])
	}

	return fmt.Errorf("unknown jobs command %q; try show", args[0])
}

// cmdJobsShow prints one job's history and what the host measured it do.
func cmdJobsShow(ctx context.Context, args []string) error {
	fs := newFlagSet("billet jobs show")
	cfgPath := addConfigFlag(fs)
	if err := parseWithArgs(fs, args, 1); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: billet jobs show [--config PATH] <lease>")
	}
	leaseID := fs.Arg(0)

	a, closeDB, err := controlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()

	rec, err := a.Job(ctx, leaseID)
	if err != nil {
		return err
	}
	var measured *alloc.RecordedUsage
	switch u, err := a.LeaseUsage(ctx, leaseID); {
	case err == nil:
		measured = &u
	case !errors.Is(err, alloc.ErrLeaseNotFound):
		return err
	}

	renderJob(os.Stdout, rec, measured)

	return nil
}

// renderJob writes one job's record.
//
// EVERY STRING GITHUB OR A WORKFLOW CHOSE IS QUOTED: a job name is whatever the
// workflow file says, and an unquoted newline in it would forge a line of this
// report.
func renderJob(w io.Writer, rec alloc.JobRecord, u *alloc.RecordedUsage) {
	line := func(label, format string, args ...any) {
		fmt.Fprintf(w, "%-11s%s\n", label, fmt.Sprintf(format, args...))
	}
	quoted := func(s string) string {
		if s == "" {
			return "not recorded"
		}
		return strconv.Quote(s)
	}

	line("lease", "%s", rec.LeaseID)
	shape := ""
	if rec.VCPU > 0 {
		shape = fmt.Sprintf(", %d vCPU, %s", rec.VCPU, humanBytes(rec.Memory))
	}
	line("tier", "%s on %s (%s%s)", rec.Tier, orUnknown(rec.Node), orUnknown(rec.ChosenProvider), shape)
	// NO REQUEST ID FOR A POOLED LEASE, for `billet leases failures`' reason:
	// its negative id is billet's scheduler identity, not anything GitHub shows.
	request := ""
	if rec.RequestID > 0 {
		request = fmt.Sprintf(", request %d", rec.RequestID)
	}
	line("job", "%s (github job %s, run %s%s)", quoted(rec.Job.Name),
		orUnknown(rec.Job.JobID), identifier(rec.RunID), request)
	line("repository", "%s", quoted(rec.Repo))
	line("workflow", "%s", quoted(rec.Job.WorkflowRef))
	line("event", "%s", quoted(rec.Job.Event))
	line("result", "github %s; billet %s", quoted(rec.Result), orUnknown(rec.Conclusion))
	line("times", "queued %s, assigned %s, started %s, finished %s", orUnknown(rec.QueuedAt),
		orUnknown(rec.AssignedAt), orUnknown(rec.StartedAt), orUnknown(rec.FinishedAt))

	fmt.Fprintln(w)
	if u == nil {
		fmt.Fprintln(w, "usage      not measured (node.monitoring was off, or the job ended before a report)")
		return
	}
	line("usage", "measured by the host %s, %d samples every %s over %s",
		orUnknown(u.Node), u.Samples, time.Duration(u.IntervalMillis)*time.Millisecond,
		time.Duration(u.WindowMillis)*time.Millisecond)

	group := func(name, label string, render func() string) {
		if !u.Measured(name) {
			line(label, "not measured")
			return
		}
		line(label, "%s", render())
	}
	group(alloc.UsageCPU, "cpu", func() string {
		return fmt.Sprintf("user %s, system %s", seconds(u.CPUUserMicros), seconds(u.CPUSystemMicros))
	})
	group(alloc.UsageThreads, "vmm split", func() string {
		return fmt.Sprintf("guest vCPUs %s, VMM %s", seconds(u.GuestCPUMicros), seconds(u.VMMCPUMicros))
	})
	group(alloc.UsageMemory, "memory", func() string {
		return fmt.Sprintf("peak %s, oom kills %d", humanBytes(u.MemoryPeakBytes), u.OOMKills)
	})
	group(alloc.UsageIO, "disk", func() string {
		return fmt.Sprintf("read %s, written %s (host io, not the guest's page cache)",
			humanBytes(u.DiskReadBytes), humanBytes(u.DiskWriteBytes))
	})
	group(alloc.UsageNet, "network", func() string {
		return fmt.Sprintf("received %s, sent %s (the guest's view)",
			humanBytes(u.NetRxBytes), humanBytes(u.NetTxBytes))
	})
	group(alloc.UsagePressure, "stalled", func() string {
		return fmt.Sprintf("cpu %s, memory %s (full %s), io %s (full %s)", seconds(u.CPUSomeMicros),
			seconds(u.MemorySomeMicros), seconds(u.MemoryFullMicros), seconds(u.IOSomeMicros),
			seconds(u.IOFullMicros))
	})
	group(alloc.UsageEnergy, "energy", func() string {
		if u.EnergySource == alloc.EnergyRAPL {
			return fmt.Sprintf("%s active, %s idle (package RAPL, a model rather than a meter)",
				joules(u.EnergyActiveMicrojoules), joules(u.EnergyIdleMicrojoules))
		}
		return fmt.Sprintf("%s (package RAPL with no idle baseline, so idle is inside it; source %s)",
			joules(u.EnergyActiveMicrojoules), strconv.Quote(u.EnergySource))
	})
}

func seconds(micros int64) string { return fmt.Sprintf("%.1fs", float64(micros)/1e6) }

func joules(microjoules int64) string {
	j := float64(microjoules) / 1e6
	if j >= 1000 {
		return fmt.Sprintf("%.1f kJ", j/1000)
	}

	return fmt.Sprintf("%.1f J", j)
}
