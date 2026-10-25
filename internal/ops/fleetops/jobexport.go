package fleetops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/jobtrace"
)

// cmdJobsExport writes a window of the deployment's own history as a trace,
// and the fleet that served it, in the format the replay harness reads
// (internal/jobtrace): `trace.jsonl` and `fleet.json` in --out.
//
// A REPORT, OPENED AS ONE (app.OpenReport): nothing created, locked, claimed
// or migrated, so it runs beside a live control plane. NAMES STAY BEHIND
// unless --names asks for them: a trace is made to be shared, and owners,
// repositories, workflows and job names are the deployment's business.
func cmdJobsExport(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet jobs export", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	since := fs.Duration("since", 7*24*time.Hour, "export the jobs assigned this far back")
	untilFlag := fs.String("until", "", "export the jobs assigned before this instant, in RFC 3339 (default now)")
	names := fs.Bool("names", false, "keep owners, repositories, workflow files and job names rather than "+
		"numbered stand-ins")
	out := fs.String("out", "", "the directory to write trace.jsonl and fleet.json into (it must exist, and "+
		"neither file may)")

	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	switch {
	case *since <= 0:
		return fmt.Errorf("billet jobs export: --since must be positive, got %s", *since)
	case *out == "":
		return errors.New("billet jobs export: --out names the directory to write the trace and the fleet into")
	}

	until := time.Now()
	if *untilFlag != "" {
		parsed, err := time.Parse(time.RFC3339, *untilFlag)
		if err != nil {
			return fmt.Errorf("billet jobs export: --until %q is not RFC 3339: %w", *untilFlag, err)
		}
		until = parsed
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	a, closeDB, err := app.OpenReport(ctx, *cfgPath)
	if err != nil {
		return err
	}

	defer closeDB()

	trace, fleet, gaps, err := exportDeployment(ctx, a, cfg, until.Add(-*since), until, *names)
	if err != nil {
		return err
	}

	if err := writeExport(*out, trace, fleet); err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "billet jobs export: %d jobs and %d hosts written to %s\n", len(trace.Arrivals),
		len(fleet.Hosts), *out)
	renderExportGaps(env, gaps)

	return nil
}

// exportDeployment is the window's trace and the fleet the configuration and
// the registrations describe: every host the deployment expects to answer now,
// which for an --until in the past may not be the fleet of that window.
//
// BOTH VALIDATED BEFORE EITHER IS WRITTEN, so an export never leaves a file
// the replay would refuse (a tier pinned to a host that is not registered).
func exportDeployment(ctx context.Context, a *alloc.Allocator, cfg *config.Config, since, until time.Time,
	names bool,
) (jobtrace.Trace, jobtrace.Fleet, alloc.ExportGaps, error) {
	trace, gaps, err := a.ExportTrace(ctx, since, until, names)
	if err != nil {
		return jobtrace.Trace{}, jobtrace.Fleet{}, alloc.ExportGaps{}, err
	}

	if len(trace.Arrivals) == 0 {
		return jobtrace.Trace{}, jobtrace.Fleet{}, gaps, fmt.Errorf("billet jobs export: no job assigned "+
			"from %s to %s started and completed, so there is nothing to replay", since.UTC().Format(time.RFC3339),
			until.UTC().Format(time.RFC3339))
	}

	if err := trace.Validate(); err != nil {
		return jobtrace.Trace{}, jobtrace.Fleet{}, gaps, fmt.Errorf("billet jobs export: %w", err)
	}

	hosts, err := a.ExportHosts(ctx)
	if err != nil {
		return jobtrace.Trace{}, jobtrace.Fleet{}, alloc.ExportGaps{}, err
	}

	fleet := jobtrace.Fleet{
		Hosts: hosts, MaxVCPU: cfg.Server.MaxVCPU, MaxMemory: cfg.Server.MaxMemory,
		Placement: cfg.Server.Placement, AdmissionOrder: cfg.Server.AdmissionOrder,
	}

	for i := range cfg.Tiers {
		t := &cfg.Tiers[i]
		fleet.Tiers = append(fleet.Tiers, jobtrace.Tier{Label: t.Label, VCPU: t.VCPU, Memory: t.Memory, Node: t.Node})
	}

	if err := fleet.Validate(); err != nil {
		return jobtrace.Trace{}, jobtrace.Fleet{}, gaps, fmt.Errorf("billet jobs export: the fleet is not one "+
			"a replay can take: %w", err)
	}

	return trace, fleet, gaps, nil
}

// writeExport writes the two files, refusing to replace either: an export
// that overwrote an earlier one would lose the window it covered.
func writeExport(dir string, trace jobtrace.Trace, fleet jobtrace.Fleet) error {
	var traceBody, fleetBody bytes.Buffer

	if err := trace.Write(&traceBody); err != nil {
		return err
	}

	if err := fleet.Write(&fleetBody); err != nil {
		return err
	}

	for name, body := range map[string][]byte{"trace.jsonl": traceBody.Bytes(), "fleet.json": fleetBody.Bytes()} {
		path := filepath.Join(dir, name)

		// 0600: even with stand-ins a trace says when the deployment worked.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("billet jobs export: %s already exists; export into a directory without one", path)
		}
		if err != nil {
			return fmt.Errorf("billet jobs export: create %s: %w", path, err)
		}

		if _, err := f.Write(body); err != nil {
			return errors.Join(fmt.Errorf("billet jobs export: write %s: %w", path, err), f.Close())
		}

		if err := f.Close(); err != nil {
			return fmt.Errorf("billet jobs export: write %s: %w", path, err)
		}
	}

	return nil
}

// renderExportGaps says what the export read and left out, on stderr.
func renderExportGaps(env cli.Env, g alloc.ExportGaps) {
	fmt.Fprintf(env.Stderr, "billet jobs export: the window held %d assigned jobs", g.Read)

	if left := g.NeverStarted + g.NeverCompleted + g.NotARuntime + g.UndeclaredTier; left > 0 {
		fmt.Fprintf(env.Stderr, "; left out %d that never started, %d GitHub never reported complete, %d whose "+
			"completion is not after their start and %d of tiers the configuration no longer declares, whose "+
			"shape the fleet cannot give", g.NeverStarted, g.NeverCompleted, g.NotARuntime, g.UndeclaredTier)
	}

	fmt.Fprintln(env.Stderr)

	if g.AssignedAsArrival > 0 {
		fmt.Fprintf(env.Stderr, "billet jobs export: %d jobs had no GitHub queue time on record and arrive when "+
			"billet was assigned them, so their wait at GitHub is not in the trace\n", g.AssignedAsArrival)
	}

	if g.Unidentified > 0 {
		fmt.Fprintf(env.Stderr, "billet jobs export: %d jobs had no repository or workflow on record and carry "+
			"a stand-in for it\n", g.Unidentified)
	}

	if g.Partial {
		fmt.Fprintln(env.Stderr, "billet jobs export: partial: the window held more jobs than one read takes, "+
			"and its newest were left out")
	}
}
