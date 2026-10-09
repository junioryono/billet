// Command knownanswer compares billet's per-job measurements with jobs whose
// answer is known, and the jobs' attributed energy with the package's.
//
//	knownanswer collect --out DIR [--expectations DIR] [--power-log CSV] [--billet PATH] [--config PATH]
//	knownanswer check   --expectations DIR --records DIR [--discard N]
//	knownanswer energy  --power-log CSV --records DIR --idle-watts W [--quiet-rows N]
//
// The expectations are what .github/workflows/known-answer.yml's jobs upload
// (scripts/known-answer-job.sh writes them); the records are `billet jobs show
// --json`, one <lease>.json each, which collect writes; the power log is
// scripts/power-log.sh's CSV. docs/operating/measurement-validation.md is the
// runbook, and says why each tolerance is what it is.
//
// EXIT STATUS IS THE VERDICT, three ways: 0 every comparison passed, 1 at least
// one failed, 3 none failed and at least one could not be made (a metric billet
// did not measure, a reference job missing). 2 is the checker unable to run:
// bad flags or unreadable inputs. collect exits 0 when every lease was
// collected and 1 when any was not.
//
// IT IS NOT PART OF THE BILLET BINARY. It is run by an operator, against
// files, and reads billet only through the command line it prints.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const (
	exitPass       = 0
	exitFail       = 1
	exitUsage      = 2
	exitUnmeasured = 3
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: knownanswer collect|check|energy [flags]")

		return exitUsage
	}
	var code int
	var err error
	switch args[0] {
	case "collect":
		code, err = runCollect(ctx, args[1:], stdout, stderr)
	case "check":
		code, err = runCheck(args[1:], stdout, stderr)
	case "energy":
		code, err = runEnergy(args[1:], stdout, stderr)
	default:
		err = fmt.Errorf("unknown command %q; try collect, check or energy", args[0])
	}
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(stderr, "knownanswer: %v\n", err)
		}

		return exitUsage
	}

	return code
}

func verdictCode(v verdict) int {
	switch v {
	case pass:
		return exitPass
	case fail:
		return exitFail
	case unmeasured:
		return exitUnmeasured
	}

	return exitUsage
}

// records reads collected records from dir.
func records(dir string) recordSource {
	return func(lease string) (record, error) { return loadRecord(dir, lease) }
}

func runCheck(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("knownanswer check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	expDir := fs.String("expectations", "", "the directory `gh run download` wrote the expectations into")
	recDir := fs.String("records", "", "the directory collect wrote billet's records into")
	discard := fs.Int("discard", 0, "drop this many of the earliest runs (the warmup)")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if *expDir == "" || *recDir == "" || fs.NArg() != 0 || *discard < 0 {
		return 0, errors.New("usage: knownanswer check --expectations DIR --records DIR [--discard N]")
	}
	exps, err := loadExpectations(*expDir)
	if err != nil {
		return 0, err
	}
	kept, dropped := discardRuns(exps, *discard)
	if len(kept) == 0 {
		return 0, fmt.Errorf("--discard %d leaves no run to check", *discard)
	}
	results := evaluate(kept, records(*recDir))
	if len(results) == 0 {
		return 0, errors.New("no loaded job to check: only baselines were found")
	}
	report(stdout, results, dropped)

	return verdictCode(overall(results)), nil
}

func runEnergy(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("knownanswer energy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	logPath := fs.String("power-log", "", "scripts/power-log.sh's CSV")
	recDir := fs.String("records", "", "the directory collect wrote billet's records into")
	idleWatts := fs.Float64("idle-watts", 0, "the node's node.monitoring.idle_package_watts")
	quiet := fs.Int("quiet-rows", 10, "rows at each end of the log that must hold no microVM")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if *logPath == "" || *recDir == "" || *idleWatts <= 0 || *quiet < 1 || fs.NArg() != 0 {
		return 0, errors.New("usage: knownanswer energy --power-log CSV --records DIR --idle-watts W [--quiet-rows N]")
	}
	rows, err := readPowerLog(*logPath)
	if err != nil {
		return 0, err
	}
	res := reconcile(rows, *idleWatts, *quiet, records(*recDir))
	res.write(stdout)

	return verdictCode(worst(res.verdict, res.idleVerdict)), nil
}

func runCollect(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("knownanswer collect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "the directory to write <lease>.json into")
	expDir := fs.String("expectations", "", "collect every lease these expectations name")
	logPath := fs.String("power-log", "", "collect every lease this power log saw")
	billet := fs.String("billet", "billet", "the billet binary")
	config := fs.String("config", "", "the control plane's billet.yaml, passed to billet jobs show")
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if *out == "" || (*expDir == "" && *logPath == "") || fs.NArg() != 0 {
		return 0, errors.New("usage: knownanswer collect --out DIR [--expectations DIR] [--power-log CSV] " +
			"[--billet PATH] [--config PATH]")
	}
	var exps []expectation
	var rows []powerRow
	var err error
	if *expDir != "" {
		if exps, err = loadExpectations(*expDir); err != nil {
			return 0, err
		}
	}
	if *logPath != "" {
		if rows, err = readPowerLog(*logPath); err != nil {
			return 0, err
		}
	}
	leases := collectLeases(exps, rows)
	if len(leases) == 0 {
		return 0, errors.New("the inputs name no lease to collect")
	}
	failed, err := collect(ctx, stdout, *billet, *config, *out, leases)
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(stdout, "collected %d of %d\n", len(leases)-failed, len(leases))
	if failed > 0 {
		return exitFail, nil
	}

	return exitPass, nil
}
