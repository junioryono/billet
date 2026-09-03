package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/runnerrelease"
	"github.com/junioryono/billet/internal/store/ceph"
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
// the release HISTORY and applies the 30-day rule from the documentation, which is
// the only mechanical signal there is — and it is the ordinary window rather than a
// guarantee, since a critical security release may be enforced at once.
//
// THE HISTORY, NOT THE NEWEST RELEASE. The window opens when the FIRST release
// newer than the installed one appears. Counting from the newest one moves a
// deadline that has already passed every time something else ships, which reported
// a fleet GitHub had stopped queueing to as having weeks in hand.
//
// THE EXIT CODE IS THE POINT. A cron entry or a monitor reads it: 0 while there is
// nothing to do, 2 once a rebuild is due, and 3 once GitHub is already refusing.
// They are distinct because the second is a task and the third is an outage.
func cmdRunner(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "check" {
		return errors.New("usage: billet runner check")
	}

	fs := newFlagSet("billet runner check")
	cfgPath := addConfigFlag(fs)
	quiet := fs.Bool("quiet", false, "print nothing unless something needs doing")

	if err := parse(fs, args[1:]); err != nil {
		return err
	}

	// WHAT THE FLEET ACTUALLY RUNS, WHEN THIS MACHINE CAN FIND OUT.
	//
	// The compiled-in pin says what a build WOULD install, and the two part company
	// the moment a scheduled rebuild takes up a newer release — after which an alarm
	// reading the binary reports an expiry that is not happening, or misses one that
	// is. The published image records what it installed; that is the fleet's answer.
	//
	// Falling back to the pin rather than refusing, because a machine with no cluster
	// config is still entitled to be told that the source it builds from is behind.
	// Which source was used is printed, so the number is never unattributed.
	installed, source := runnerrelease.Pinned(), "the version this billet would build"

	fleet, ok, why := fleetRunnerVersion(ctx, *cfgPath)

	switch {
	case ok:
		installed, source = fleet, "the version the published guest image carries"

	case why != nil:
		// A CLUSTER THAT WAS CONFIGURED AND COULD NOT BE READ IS WORTH SAYING, quietly
		// and on stderr, because it is not a verdict. Swallowing it is how a broken
		// lookup came to look exactly like an image with no metadata recorded — which
		// is what happened when this asked rbd for json output it does not accept.
		fmt.Fprintf(os.Stderr, "note: could not read what the published image carries, so this "+
			"is about the version billet would build: %v\n", why)
	}

	fresh, err := resolveRunnerFreshness(ctx, nil, installed)
	if err != nil {
		// NOT A VERDICT. A machine with no egress cannot find out, and reporting that
		// as "your fleet is about to stop working" is the false alarm that teaches
		// people to ignore the true one.
		return fmt.Errorf("could not find out where %s sits in github's release history, so "+
			"this says nothing about whether it is still accepted: %w", installed, err)
	}

	// THE MODEL'S SPELLING FROM HERE ON, not the string handed to it: Resolve
	// normalizes what it was given, and a report naming a version the answer is not
	// about is the same divergence as a config validated in one form and consumed in
	// another.
	if fresh.Installed != "" {
		installed = fresh.Installed
	}

	now := time.Now()

	// THE WINDOW OPENED WHEN THE FIRST NEWER RELEASE APPEARED, and saying which one
	// is not decoration: an operator reading "2.339.0 was published yesterday" beside
	// a deadline three weeks ago has no way to see that the clock started at 2.335.0.
	// The newest release is named separately, as the thing to take up.
	started := func() string {
		line := fmt.Sprintf("        %s was published %s and started the ordinary 30-day "+
			"window", fresh.FirstNewer, fresh.FirstNewerPublished.Format(time.DateOnly))

		if fresh.Latest != fresh.FirstNewer {
			line += fmt.Sprintf("; %s is the newest", fresh.Latest)
		}

		return line
	}

	// THE UNPLACEABLE CASE IS DECIDED BEFORE THE HAPPY ONE, and the order is the
	// whole guard. `Current` means "nothing newer was found", which is ALSO true of a
	// version the history does not name at all — a hand-built runner, a typo in a
	// generation's metadata, or one so old the window does not reach it. Asking
	// `Current` first printed "which is the current release" and exited 0 for exactly
	// the version billet could not place, which is the false negative this command
	// exists to remove.
	switch {
	case fresh.Expired(now):
		fmt.Printf("runner  %s (%s), and GitHub stopped queueing jobs to it on %s\n",
			installed, source, fresh.Deadline().Format(time.DateOnly))
		fmt.Println(started())

		if !fresh.InstalledKnown {
			// THE DEADLINE IS AN UPPER BOUND HERE, so "already past it" is still a
			// proof — the real one was earlier — and saying so keeps the report from
			// claiming a precision it does not have.
			fmt.Printf("        %s is older than the history billet reads, so that date is "+
				"the latest it could have been\n", installed)
		}

		fmt.Println()
		fmt.Println("Rebuild and republish the image, then point the tiers at the new generation:")
		fmt.Println()
		fmt.Printf("  1. put %s in internal/runnerrelease/pinned.txt\n", fresh.Latest)
		fmt.Println("  2. sudo scripts/build-guest-image.sh   (microVM guests)")
		fmt.Println("  3. billet ami build                    (ec2 nodes)")

		return errExpiredRunner

	// BEHIND WITH NO WINDOW TO COUNT. A higher version published EARLIER than the
	// installed release is evidence the fleet is behind and is not an opener, so
	// there is no deadline to report — and falling through to a timed branch printed
	// an empty release name, the year 0001 and a negative number of days, then exited
	// 0. It is a rebuild to schedule rather than an outage, so it takes the same exit
	// code as an ordinary due.
	case fresh.BehindWithoutAWindow():
		fmt.Printf("runner  %s (%s), and %s is newer\n", installed, source, fresh.Latest)
		fmt.Printf("        %s was published before %s, so it was already available when "+
			"this runner shipped and github's ordinary window has no start to count from\n",
			fresh.Latest, installed)
		fmt.Println()
		fmt.Printf("Rebuild: put %s in internal/runnerrelease/pinned.txt, then rebuild the "+
			"images.\n", fresh.Latest)

		return errRunnerDue

	// NEITHER OF THE NEXT TWO IS A VERDICT, and they are the same shape: billet read
	// less of the history than the answer would need. `Expired` above is sound under
	// both, because anything unread can only have opened the window EARLIER — it is
	// the other direction, "nothing to do", that they take away.
	case !fresh.InstalledKnown, !fresh.HistoryComplete:
		// NOT AN ANSWER, SO NOT A VERDICT. A version the history does not name is
		// either older than the window billet reads — in which case the ordinary
		// window opened before the earliest release it can see, so the days it would
		// print would be an over-estimate — or not a published release at all. Expiry
		// is still provable and is handled above; anything short of that is "could
		// not establish", which is an error rather than a quiet zero.
		//
		// AHEAD OF Current, WHICH WOULD OTHERWISE CLAIM IT. `Current` means only that
		// nothing newer was found, and nothing newer is found for a version billet
		// could not place either.
		where := "it is not among the releases billet reads, so it is either older than " +
			"they go or was never published"

		if fresh.InstalledKnown {
			where = "billet reached the end of the history it reads before the end of " +
				"github's, so a release older than that could have opened the window earlier"
		}

		if !fresh.Current() {
			where += fmt.Sprintf("; the earliest release newer than it that billet can see "+
				"appeared %s, and the ordinary window opened no later than that",
				fresh.FirstNewerPublished.Format(time.DateOnly))
		}

		return fmt.Errorf("could not establish where %s sits in github's release history: "+
			"%s. The newest release is %s", installed, where, fresh.Latest)

	case fresh.Current():
		if !*quiet {
			fmt.Printf("runner  %s, which is the current release (%s)\n", installed, source)
		}

		return nil

	case fresh.Due(now):
		fmt.Printf("runner  %s (%s), and GitHub stops queueing jobs to it on %s (%d days)\n",
			installed, source, fresh.Deadline().Format(time.DateOnly),
			int(fresh.Remaining(now).Hours()/24))
		fmt.Println(started())
		fmt.Println()
		fmt.Printf("Rebuild while there is time: put %s in internal/runnerrelease/pinned.txt, "+
			"then rebuild the images.\n", fresh.Latest)

		return errRunnerDue

	default:
		if !*quiet {
			fmt.Printf("runner  %s; %s was published %s, and there are %d days to take it up\n",
				installed, fresh.FirstNewer, fresh.FirstNewerPublished.Format(time.DateOnly),
				int(fresh.Remaining(now).Hours()/24))
		}

		return nil
	}
}

// resolveRunnerFreshness is the one question billet asks about a runner version.
//
// A PACKAGE VARIABLE SO A TEST CAN DRIVE THE COMMAND, the shape openArchiveStore
// already has. What has to be provable here is what the command PRINTS and what it
// EXITS with for each state, and a test that could only call the model would prove
// the model — which has its own tests — and nothing about the command.
var resolveRunnerFreshness = runnerrelease.Resolve

// A DUE REBUILD AND AN ALREADY-REFUSED FLEET ARE DIFFERENT EXIT CODES, because one
// is a task to schedule and the other is an outage to page for, and a monitor that
// cannot tell them apart will treat both like whichever it saw first.
var (
	errRunnerDue     = &exitError{code: 2, msg: "the runner image is due to be rebuilt"}
	errExpiredRunner = &exitError{code: 3, msg: "github is no longer queueing jobs to this runner"}
)

// fleetRunnerVersion reports the OLDEST runner any tier's image boots.
//
// THE OLDEST, NOT THE FIRST FOUND. A deployment can have several tiers on several
// images, and the deadline belongs to whichever is furthest behind: answering with
// the first one that happened to carry metadata would leave a stale tier expiring
// unwatched while the check stayed green about a current one.
//
// EVERY FAILURE HERE IS "CANNOT TELL" RATHER THAN A VERDICT, which is why the caller
// gets a bool: no config, no cluster, a machine that runs no microVMs, a generation
// published before this was recorded. None of those say anything about whether a
// fleet is expiring, and the caller has a fallback that is honest about being one.
func fleetRunnerVersion(ctx context.Context, cfgPath string) (string, bool, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		//nolint:nilerr // an unreadable config means "cannot tell", which the caller handles
		return "", false, nil
	}

	if cfg.Node == nil || cfg.Node.Ceph == nil {
		return "", false, nil
	}

	store, err := ceph.New(*cfg.Node.Ceph)
	if err != nil {
		return "", false, err
	}

	var (
		failures []error
		oldest   string
		found    bool
	)

	for i := range cfg.Tiers {
		image := cfg.Tiers[i].ImageFor(config.ProviderFirecracker)
		if image == "" || !cfg.Tiers[i].AcceptsProvider(config.ProviderFirecracker) {
			continue
		}

		version, ok, err := store.RunnerVersion(ctx, image)

		switch {
		case err != nil:
			failures = append(failures, err)
		case !ok:
			continue
		// THE MODEL'S COMPARATOR, NOT A SECOND ONE. This package used to carry its own
		// version ordering while the freshness calculation ordered versions its own
		// way; two comparators is one comparator that is wrong, and the failure is
		// silent — picking the wrong tier watches the one that is fine while the stale
		// one expires.
		case !found || runnerrelease.Older(version, oldest):
			oldest, found = version, true
		}
	}

	if found {
		return oldest, true, errors.Join(failures...)
	}

	return "", false, errors.Join(failures...)
}
