// tfclassify reads a Terraform plan and says which of its changes billet has to
// be drained for.
//
//	terraform -chdir=terraform/modules/billet plan -out=tfplan
//	terraform -chdir=terraform/modules/billet show -json tfplan > plan.json
//	go run ./scripts/tfclassify -plan plan.json
//
// It exits 2 when the plan holds a draining or destructive change and
// -acknowledge was not given, so it can gate an apply. Exit 1 is tfclassify
// itself failing — a plan it cannot parse, a resource nobody classified — and
// the two are separated because a pipeline treats them differently: one is a
// finding, the other is a broken gate.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/junioryono/billet/internal/tfclass"
)

// exitBlocked is "this plan needs a drain first", as distinct from exit 1, which
// is tfclassify unable to answer.
const exitBlocked = 2

func main() {
	err := run(os.Args[1:], os.Stdout)

	// THE TWO OUTCOMES ARE SEPARATED HERE rather than by an os.Exit buried in
	// run, so run stays a function a test can call. A blocked plan is a FINDING —
	// tfclassify worked — and anything else is the gate failing to answer.
	//
	// AsType rather than As: it returns the value WITH the bool, so the target
	// cannot be used outside the branch that proved it was found.
	blockage, blocked := errors.AsType[*blockedError](err)
	if blocked {
		fmt.Fprintf(os.Stderr, "tfclassify: %v\n", blockage)
		os.Exit(exitBlocked)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "tfclassify: %v\n", err)
		os.Exit(1)
	}
}

// blockedError is a plan that needs a drain. It is a distinct type so main can
// give it its own status without inspecting a message.
type blockedError struct{ n int }

func (e *blockedError) Error() string {
	return fmt.Sprintf("%d change(s) need a drain before this plan is applied", e.n)
}

func run(args []string, out *os.File) error {
	fs := flag.NewFlagSet("tfclassify", flag.ContinueOnError)
	planPath := fs.String("plan", "",
		"the plan as JSON: `terraform show -json <planfile>` (required)")
	tablePath := fs.String("table", defaultTable(),
		"the committed classification table, relative to the repository root")
	module := fs.String("module", "",
		"the module address billet was called at in this plan (`module.billet`), when the "+
			"plan is for a root that merely CONTAINS billet. Empty means the plan IS the "+
			"billet module, which is the documented invocation")
	acknowledge := fs.Bool("acknowledge", false,
		"report draining and destructive changes without failing — for an operator who has "+
			"already drained this deployment")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *planPath == "" {
		return fmt.Errorf("-plan is required: `terraform show -json <planfile> > plan.json`")
	}

	table, err := tfclass.Load(*tablePath)
	if err != nil {
		// SAID WHERE IT LOOKED, because the default is relative to the working
		// directory rather than to this program, and `go run ./scripts/tfclassify`
		// from anywhere but the repository root fails on a path that reads like a
		// missing file rather than like a missing -table.
		return fmt.Errorf("%w\n(the default table is %s, relative to the repository root; "+
			"pass -table to point somewhere else)", err, defaultTable())
	}

	planJSON, err := os.ReadFile(*planPath)
	if err != nil {
		return fmt.Errorf("read the plan: %w", err)
	}

	findings, err := tfclass.Classify(table, tfclass.Scope(*module), planJSON)
	if err != nil {
		return err
	}

	fmt.Fprint(out, tfclass.Report(findings))

	blocking := tfclass.Blocking(findings)
	if len(blocking) == 0 || *acknowledge {
		return nil
	}

	// SAID BEFORE THE STATUS, because a non-zero exit with no sentence is a gate
	// somebody disables rather than satisfies.
	fmt.Fprintf(out, "\n%d change(s) above stop compute or lose data.\n", len(blocking))
	fmt.Fprintf(out, "Drain the deployment first — `billet drain --wait` seals admission and\n")
	fmt.Fprintf(out, "waits for what is running, with no time limit — then re-run with\n")
	fmt.Fprintf(out, "-acknowledge. A Terraform timeout is not permission to terminate a job.\n")

	return &blockedError{n: len(blocking)}
}

// defaultTable is the committed table's path RELATIVE TO THE REPOSITORY ROOT,
// which is where `go run ./scripts/tfclassify` is invoked from — the same
// assumption every other program under scripts/ makes.
//
// NOT RELATIVE TO THIS PROGRAM, and an earlier comment said it was. It cannot
// be: `go run` builds into a temporary directory, so os.Executable() names
// nothing near the repository. Embedding the table instead would give the CLI a
// second copy of the bytes internal/tfclass's drift test exists to keep single.
// So the default is a working-directory path, the failure names it, and -table
// is what an invocation from elsewhere passes.
func defaultTable() string {
	return filepath.Join("terraform", "modules", "billet", "classification.json")
}
