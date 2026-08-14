package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/runnerrelease"
)

// cmdRunner reports how close billet's runner is to being refused by GitHub.
//
// THIS EXISTS BECAUSE THE FAILURE IS SILENT AND TERMINAL. GitHub requires a
// self-hosted runner to take up each release within 30 days, and past that the
// Actions service stops handing it jobs — the server refuses the message rather than
// asking the runner to update, so nothing on the runner's side recovers. A fleet
// that worked yesterday takes no work today, every machine looks healthy, and the
// only symptom is jobs queueing.
//
// billet bakes the runner into an image, so taking up a release means rebuilding and
// republishing one. That is a scheduled act, and the thing that makes a scheduled
// act reliable is a machine noticing when it is due rather than a person remembering.
//
// THERE IS NO API FOR WHAT GITHUB WILL REFUSE. It publishes no minimum-version
// endpoint; its own advice is to subscribe to release notifications. So this reads
// the release feed and applies the 30-day rule from the documentation, which is the
// only mechanical signal there is.
//
// THE EXIT CODE IS THE POINT. A cron entry or a monitor reads it: 0 while there is
// nothing to do, 2 once a rebuild is due, and 3 once GitHub is already refusing.
// They are distinct because the second is a task and the third is an outage.
func cmdRunner(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "check" {
		return errors.New("usage: billet runner check")
	}

	fs := newFlagSet("billet runner check")
	quiet := fs.Bool("quiet", false, "print nothing unless something needs doing")

	if err := parse(fs, args[1:]); err != nil {
		return err
	}

	status, err := runnerrelease.Latest(ctx, nil)
	if err != nil {
		// NOT A VERDICT. A machine with no egress cannot find out, and reporting that
		// as "your fleet is about to stop working" is the false alarm that teaches
		// people to ignore the true one.
		return fmt.Errorf("could not find out which runner release is current, so this says "+
			"nothing about whether %s is still accepted: %w", runnerrelease.Pinned(), err)
	}

	now := time.Now()

	switch {
	case status.Current():
		if !*quiet {
			fmt.Printf("runner  %s, which is the current release\n", status.Pinned)
		}

		return nil

	case status.Expired(now):
		fmt.Printf("runner  %s, and GitHub stopped queueing jobs to it on %s\n",
			status.Pinned, status.Deadline.Format(time.DateOnly))
		fmt.Printf("        %s was published %s, and a runner has 30 days to take up a release\n",
			status.Latest, status.Published.Format(time.DateOnly))
		fmt.Println()
		fmt.Println("Rebuild and republish the image, then point the tiers at the new generation:")
		fmt.Println()
		fmt.Printf("  1. put %s in internal/runnerrelease/pinned.txt\n", status.Latest)
		fmt.Println("  2. sudo scripts/build-guest-image.sh   (microVM guests)")
		fmt.Println("  3. billet ami build                    (ec2 nodes)")

		return errExpiredRunner

	case status.Due(now):
		fmt.Printf("runner  %s, and GitHub stops queueing jobs to it on %s (%d days)\n",
			status.Pinned, status.Deadline.Format(time.DateOnly),
			int(status.Remaining(now).Hours()/24))
		fmt.Printf("        %s was published %s\n",
			status.Latest, status.Published.Format(time.DateOnly))
		fmt.Println()
		fmt.Printf("Rebuild while there is time: put %s in internal/runnerrelease/pinned.txt, "+
			"then rebuild the images.\n", status.Latest)

		return errRunnerDue

	default:
		if !*quiet {
			fmt.Printf("runner  %s; %s was published %s, and there are %d days to take it up\n",
				status.Pinned, status.Latest, status.Published.Format(time.DateOnly),
				int(status.Remaining(now).Hours()/24))
		}

		return nil
	}
}

// A DUE REBUILD AND AN ALREADY-REFUSED FLEET ARE DIFFERENT EXIT CODES, because one
// is a task to schedule and the other is an outage to page for, and a monitor that
// cannot tell them apart will treat both like whichever it saw first.
var (
	errRunnerDue     = &exitError{code: 2, msg: "the runner image is due to be rebuilt"}
	errExpiredRunner = &exitError{code: 3, msg: "github is no longer queueing jobs to this runner"}
)
