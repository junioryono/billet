package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/junioryono/billet/internal/alloc"
)

// cmdLeasesFailures shows jobs that did not succeed while billet's own
// infrastructure was disrupted.
//
// ATTRIBUTION, NOT ACTION. billet does not re-run any of these and will not:
// re-running is a side effect on somebody's repository, and a deploy or a
// migration must not happen twice because a machine went away. GitHub does not
// requeue a job whose runner vanished mid-execution either, so what is missing
// without this view is not a retry — it is any way at all for the person whose
// build went red to tell a broken host from a broken test.
//
// It reports two facts and joins them nowhere else: GitHub's own conclusion for
// the job, and what billet observed happening to that lease. The footer says
// out loud that the pairing is circumstantial, because a view that reads as a
// verdict is worse than no view.
func cmdLeasesFailures(ctx context.Context, args []string) error {
	fs := newFlagSet("billet leases failures")
	cfgPath := addConfigFlag(fs)
	since := fs.Duration("since", 24*time.Hour, "how far back to look")
	limit := fs.Int("limit", 50, "most rows to print")

	if err := parse(fs, args); err != nil {
		return err
	}

	if *since <= 0 {
		return fmt.Errorf("--since must be positive, got %s", *since)
	}

	if *limit <= 0 {
		return fmt.Errorf("--limit must be positive, got %d", *limit)
	}

	a, closeDB, err := controlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}

	defer closeDB()

	failures, err := a.AttributedFailures(ctx, time.Now().Add(-*since), *limit)
	if err != nil {
		return err
	}

	if len(failures) == 0 {
		fmt.Printf("No job in the last %s failed while billet's infrastructure was disrupted.\n",
			shortDuration(*since))

		return nil
	}

	printAttributedFailures(failures)

	fmt.Printf("\nThese jobs did not succeed, and billet's own infrastructure was disrupted while\n")
	fmt.Printf("their leases could still have been running them. That is CIRCUMSTANTIAL: billet\n")
	fmt.Printf("cannot tell a broken host from a broken build, and a job on this list may have\n")
	fmt.Printf("failed on its own merits.\n\n")
	fmt.Printf("Nothing has been re-run. GitHub does not requeue a job whose runner vanished\n")
	fmt.Printf("mid-execution, and billet does not re-run one for you — re-running is a side\n")
	fmt.Printf("effect on your repository, and a deploy or a migration must not happen twice\n")
	fmt.Printf("because a machine went away. Re-run failed jobs from the workflow run page if\n")
	fmt.Printf("the disruption explains the failure.\n")

	return nil
}

func printAttributedFailures(failures []alloc.AttributedFailure) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	// THE LEASE LEADS, as in every other `billet leases` view, so a row here can be
	// carried straight to `billet leases held` — a job whose teardown is still
	// wedged on the host that vanished appears in both.
	//
	// NO REQUEST COLUMN, deliberately. A lease's request id is billet's SCHEDULER
	// identity, and for a pooled runner it is a negative synthetic one issued
	// before GitHub chose the job — so printing it beside the workflow run that
	// actually executed pairs two different identities under headings that read as
	// one, and shows an operator "-3" where they expect a GitHub id. The run is
	// what finds the build; the lease is what finds it here.
	//
	// THE VARIABLE-LENGTH FIELD IS LAST, because the reclaim detail is a sentence a
	// node wrote and every column after it would be pushed around by its width.
	fmt.Fprintln(w, "LEASE\tTIER\tNODE\tRUN\tGITHUB SAID\tDISRUPTED\tBILLET SAW")

	for i := range failures {
		f := &failures[i]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			f.LeaseID, f.Tier, orUnknown(f.Node),
			identifier(f.RunID),
			// QUOTED. GitHub's result is a free-form string from an upstream
			// billet does not control, and the reclaim detail beside it is text a
			// NODE supplied — both are printed into a report an operator reads as
			// billet's own output, where an unquoted newline forges a row.
			strconv.Quote(f.Result), ago(f.DisruptedAt),
			explainDisruption(f.Disruption, f.Detail))
	}

	_ = w.Flush()
}

// explainDisruption renders one disruption in an operator's language.
//
// TOTAL OVER ANYTHING IT DOES NOT KNOW. The vocabulary is closed in Go rather
// than in the schema, so a row written by a newer binary — or by hand — reaches
// this. Printing the raw token quoted is honest; dropping the row would hide the
// very thing it was recorded for.
func explainDisruption(d alloc.Disruption, detail string) string {
	switch d {
	case alloc.DisruptionNodeForgotten:
		return "its host stopped answering this control plane"
	case alloc.DisruptionGuestAbsent:
		return "its host reported an inventory without this guest"
	case alloc.DisruptionReclaimed:
		if detail != "" {
			return "the machine was reclaimed: " + strconv.Quote(detail)
		}

		return "the machine was reclaimed"
	case alloc.DisruptionHeldPastLimit:
		return "billet destroyed it under the node's configured custody limit"
	}

	return strconv.Quote(string(d))
}

// identifier renders the workflow run, or a dash when none was ever recorded — a
// bare 0 reads as an id. A row without one still names the lease, the host and
// the time, which is what an operator has to go on when GitHub's completion
// carried no run.
func identifier(id int64) string {
	if id == 0 {
		return "-"
	}

	return strconv.FormatInt(id, 10)
}

func orUnknown(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

// ago renders how long ago a recorded timestamp was.
//
// The raw stamp is printed unchanged when it cannot be parsed, rather than
// silently becoming "0s ago" — a report that invents a time is worse than one
// that shows an operator the bytes it could not read.
func ago(stamp string) string {
	t, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return stamp
	}

	d := time.Since(t)
	if d < time.Minute {
		return "<1m ago"
	}

	return d.Round(time.Minute).String() + " ago"
}

// shortDuration renders a flag value the way an operator typed it, so the empty
// report names the window it actually looked at.
//
// NEVER ROUNDED TO ZERO. `--since 1ns` reporting "the last 0s" tells an operator
// billet looked at no time at all, when what it did was look at the window they
// asked for and find nothing — two different answers.
func shortDuration(d time.Duration) string {
	rounded := d.Round(time.Second)
	if d >= time.Hour {
		rounded = d.Round(time.Minute)
	}

	if rounded == 0 {
		return d.String()
	}

	return rounded.String()
}
