package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/github"
)

// explainGitHubAccess adds what GitHub actually knows to a control plane's
// failure.
//
// WHY THE RAW FAILURE IS NOT ENOUGH. Every token billet mints is scoped to an
// installation, so an App that has been uninstalled — or an installation id
// left behind by a reinstall — fails inside the scale-set client, several
// wrappers deep, as a bare 404:
//
//	server: refuse trusted tier billet-2vcpu: scaleset: find trusted runner group
//	"…": … failed to get runner registration token on refresh: failed to fetch
//	access token: request POST …/access_tokens failed(status="404 Not Found" …)
//
// Nothing in that names the App, the organization, or the one thing an operator
// has to do. `billet check` has always answered it — `github.VerifyAppAt`
// produces the right sentence — but nothing on the path that actually fails ever
// asked. MEASURED ON A REAL DEPLOYMENT: an App uninstalled from its org left the
// control plane crash-looping on that 404 more than forty thousand times, and
// the answer was one authenticated GET away the whole time.
//
// IT RUNS ONLY AFTER A FAILURE, and that is deliberate. Verifying at startup
// would add a GitHub round trip to every start and a new way to refuse to boot;
// this costs one request on a path that has already lost, and can only add
// words to an error that was going to be returned anyway.
//
// IT NEVER CLAIMS TO HAVE FOUND THE CAUSE. `Run` covers the control plane's
// whole lifetime, not just its startup, and billet has no typed handle on the
// vendored client's errors with which to tell a credential failure from a
// database one — so this asks a question rather than answering one, and the
// wording says so. Presuming causation would put a paragraph about GitHub in
// front of an operator whose ledger is the thing that broke.
//
// AND THE REQUEST IS BOUNDED, for the same reason: an unrelated failure must not
// be held up behind an unreachable GitHub while the process is trying to exit.
// The bound is on asking GitHub; the key read ahead of it is local.
//
// IT NEVER REPLACES THE ORIGINAL ERROR. What GitHub says here explains the
// failure; it is not itself the failure, and a diagnostic that swallowed the
// real one would be worse than none. Everything below either wraps or returns
// the error it was given.
func explainGitHubAccess(ctx context.Context, cfg *config.Config, err error) error {
	if err == nil || cfg == nil || len(cfg.GitHubTargets()) == 0 {
		return err
	}

	// The caller stopping is not a GitHub problem, and asking GitHub about it
	// would fail for that same reason and say so misleadingly.
	if ctx.Err() != nil {
		return err
	}

	probe, cancel := context.WithTimeout(ctx, gitHubProbeTimeout)
	defer cancel()

	note := gitHubAccessNote(probe, cfg)

	// RE-CHECKED AFTER, because the guard above cannot cover the key read: a
	// cancellation landing during it produces a "could not read the App key"
	// note about a process that is simply stopping.
	if note == "" || ctx.Err() != nil {
		return err
	}

	return fmt.Errorf("%w\n\n%s", err, note)
}

// gitHubProbeTimeout bounds the REQUEST to GitHub. Generous enough for a slow
// answer, short enough that a failure which had nothing to do with GitHub is not
// held behind an unreachable one while the process is exiting.
const gitHubProbeTimeout = 15 * time.Second

// gitHubAccessNote is what GitHub says about every App this deployment holds
// right now, one paragraph per target, or empty when it says nothing worth
// adding.
func gitHubAccessNote(ctx context.Context, cfg *config.Config) string {
	var notes []string

	for _, target := range cfg.GitHubTargets() {
		if note := gitHubTargetAccessNote(ctx, cfg, target); note != "" {
			notes = append(notes, note)
		}
	}

	return strings.Join(notes, "\n\n")
}

// gitHubTargetAccessNote is one target's paragraph.
func gitHubTargetAccessNote(ctx context.Context, cfg *config.Config, gh config.GitHubTarget) string {
	key, err := resolveAppKey(ctx, cfg, gh)
	if err != nil {
		return fmt.Sprintf("billet could not read the App key for target %s at %s to find out why: %v",
			gh.Name, appKeyLocation(cfg, gh), err)
	}

	inst, err := github.VerifyAppAt(ctx, nil, githubAPIBase, gh.AppID, key,
		githubTarget(gh), gh.InstallationID)

	switch {
	case ctx.Err() != nil:
		return ""

	// NOT A VERDICT, so it must not read as one. A 5xx or a throttle says
	// nothing about the App, and reporting it as a credential problem would send
	// an operator to reinstall something that is fine.
	case errors.Is(err, github.ErrAppUnverifiable):
		return fmt.Sprintf("billet asked GitHub about App %d on %s and could not get an "+
			"answer, so this may be unrelated: %v", gh.AppID, gh.Path(), err)

	case err != nil:
		return fmt.Sprintf("billet also asked GitHub about this deployment's App, in case the "+
			"failure above was a credential problem. It is not healthy:\n"+
			"  %v\n\n"+
			"Every token billet mints is scoped to an installation, so if the failure "+
			"above is a 404 from a token or registration endpoint, this is why.\n"+
			"`billet check --config <path>` reports the same thing without starting "+
			"a control plane.", err)

	// HEALTHY, AND THAT IS WORTH SAYING — but only about what was actually
	// checked. A control plane authenticates under TWO issuers: billet's own
	// runner-group policy client signs with app_id, which is what this check
	// signs with, while the vendored scale-set client signs with client_id when
	// one is configured. So with a client_id set, "the credential is fine" would
	// be a claim about one of the two, and the wrong one is exactly the value an
	// operator would then not look at.
	default:
		if gh.ClientID != "" {
			return fmt.Sprintf("billet also asked GitHub about this deployment's App for target %s: "+
				"%d is installed on %s as installation %d, with exactly the permissions billet "+
				"requested. THAT WAS CHECKED UNDER app_id. client_id is also set, "+
				"and the scale-set client signs with that instead — so the installation is "+
				"healthy and a wrong client_id is NOT ruled out.",
				gh.Name, gh.AppID, gh.Path(), inst.ID)
		}

		return fmt.Sprintf("billet also asked GitHub about this deployment's App for target %s, "+
			"and the credential is not the problem: %d is installed on %s as installation %d, "+
			"with exactly the permissions billet requested. Whatever failed above is "+
			"something else.", gh.Name, gh.AppID, gh.Path(), inst.ID)
	}
}
