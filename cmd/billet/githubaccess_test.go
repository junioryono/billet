package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// githubAccessConfig is a config whose App key is real enough to sign a JWT,
// pointed at whatever stub the test stands up.
func githubAccessConfig(t *testing.T) *config.Config {
	t.Helper()

	cfg, err := config.Load(writeCheckConfig(t))
	if err != nil {
		t.Fatalf("load the test config: %v", err)
	}

	return cfg
}

// errStartupFailure is the shape a real one has: the 404 arrives several wrappers
// deep inside the scale-set client, naming nothing an operator can act on.
var errStartupFailure = errors.New(`server: refuse trusted tier billet-4vcpu: scaleset: ` +
	`find trusted runner group "billet-trial": failed to get runner registration token ` +
	`on refresh: failed to fetch access token: request POST ` +
	`https://api.github.com/app/installations/42/access_tokens failed(status="404 Not Found")`)

// keepsTheFailure asserts the contract every branch owes the caller: the
// original error is still there, still matchable, and still FIRST.
//
// ORDER IS PART OF IT. A wrapper that put the diagnostic in front would satisfy
// errors.Is and still bury the account of what billet was actually doing under a
// paragraph about something it merely suspected.
func keepsTheFailure(t *testing.T, got error) {
	t.Helper()

	if got == nil {
		t.Fatal("a startup failure was turned into success")
	}

	if !errors.Is(got, errStartupFailure) {
		t.Fatalf("the original failure is no longer matchable: %v", got)
	}

	if !strings.HasPrefix(got.Error(), errStartupFailure.Error()) {
		t.Fatalf("the diagnostic was put in front of the failure it explains:\n%v", got)
	}
}

// AN UNINSTALLED APP IS NAMED, and the failure that hid it is kept.
//
// This is the case measured on a real deployment: the App had been removed from
// the organization, every token request 404'd inside the scale-set client, and
// the control plane crash-looped on an error that named neither the App, the
// organization, nor the one thing to do about it.
func TestAStartupFailureNamesAnUninstalledApp(t *testing.T) {
	// 404 on the org's installation: exactly what GitHub answers for an App that
	// was removed from the organization.
	pointGitHubAt(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	got := explainGitHubAccess(t.Context(), githubAccessConfig(t), errStartupFailure)

	keepsTheFailure(t, got)

	// AND THE EXPLANATION IS THE ACTIONABLE PART: the App, the organization, and
	// the word an operator searches for.
	for _, want := range []string{"not installed", "acme", "billet check"} {
		if !strings.Contains(got.Error(), want) {
			t.Errorf("the explanation does not mention %q:\n%v", want, got)
		}
	}
}

// A STALE INSTALLATION ID IS ANSWERED WITH THE RIGHT NUMBER.
//
// The other half of the same trap, and the more confusing one: reinstalling the
// App mints a NEW installation, so a config that still names the old one fails
// identically — a 404 that looks like a credential problem and is a stale
// integer. GitHub knows the answer; billet now asks.
func TestAStartupFailureNamesTheInstallationIdItShouldHave(t *testing.T) {
	pointGitHubAt(t, func(w http.ResponseWriter, _ *http.Request) {
		// The config written by writeCheckConfig names 42.
		_, _ = fmt.Fprint(w, `{"id": 99, "permissions": {"metadata": "read",
			"organization_self_hosted_runners": "write"}}`)
	})

	got := explainGitHubAccess(t.Context(), githubAccessConfig(t), errStartupFailure)

	keepsTheFailure(t, got)

	if !strings.Contains(got.Error(), "99") {
		t.Errorf("the explanation does not name the installation the App actually has:\n%v", got)
	}
}

// GITHUB BEING DOWN IS NOT A VERDICT ABOUT THE CREDENTIAL.
//
// The failure mode this guards is a diagnostic that is confidently wrong: a 502
// reported as "your App is not installed" sends an operator to reinstall
// something that was never broken, during an outage that has nothing to do with
// them.
func TestAnUnreachableGitHubIsNotReportedAsABadCredential(t *testing.T) {
	pointGitHubAt(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})

	got := explainGitHubAccess(t.Context(), githubAccessConfig(t), errStartupFailure)

	keepsTheFailure(t, got)

	text := got.Error()

	if strings.Contains(text, "not installed") {
		t.Errorf("an unreachable GitHub was reported as an uninstalled App:\n%v", got)
	}

	if !strings.Contains(text, "may be unrelated") {
		t.Errorf("the explanation does not say it could not reach a verdict:\n%v", got)
	}
}

// A HEALTHY CREDENTIAL IS RULED OUT RATHER THAN LEFT AMBIGUOUS.
//
// When the App verifies, the failure is something else — and saying so is worth
// a line, because the credential is where anyone would look first.
func TestAVerifiedAppIsSaidToBeFineSoTheSearchMovesOn(t *testing.T) {
	pointGitHubAt(t, exactInstallation)

	got := explainGitHubAccess(t.Context(), githubAccessConfig(t), errStartupFailure)

	keepsTheFailure(t, got)

	if !strings.Contains(got.Error(), "not the problem") {
		t.Errorf("a verified App was not ruled out:\n%v", got)
	}
}

// A STOPPING CONTROL PLANE IS NOT DIAGNOSED AT ALL.
//
// Shutdown is not a credential failure, so nothing about GitHub belongs on one —
// and the check would fail for the same reason the run ended, which is how a
// diagnostic becomes noise at exactly the moment an operator is reading
// carefully.
//
// THE KEY IS MISSING ON PURPOSE, because that is the only way to SEE the guard.
// Without it the note is still empty — every request fails on the dead context
// and that is reported as no verdict — so the ordinary case cannot tell whether
// the guard is there. What it cannot absorb is a failure BEFORE the request:
// reading the key. Ungated, a stopping control plane with an unreadable key is
// told its key is unreadable, which is true, irrelevant, and the last thing it
// should be reading about.
func TestAStoppingControlPlaneIsNotDiagnosedAtAll(t *testing.T) {
	pointGitHubAt(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("GitHub was asked about a cancelled run")
	})

	cfg := githubAccessConfig(t)
	cfg.GitHub.PrivateKeyPath = filepath.Join(t.TempDir(), "not-here.pem")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	got := explainGitHubAccess(ctx, cfg, errStartupFailure)

	if got == nil || got.Error() != errStartupFailure.Error() {
		t.Fatalf("a cancelled run's failure was changed: %v", got)
	}

	// THE SAME ERROR VALUE, not a wrap and not a copy of its text. errors.Is
	// alone would accept fmt.Errorf("%w", …), which reads identically in a log
	// and is a different value; asserting that it unwraps to nothing says it was
	// returned rather than rebuilt, which is what a pass-through owes.
	if !errors.Is(got, errStartupFailure) || errors.Unwrap(got) != nil {
		t.Fatalf("the cancelled path did not return the failure it was given: %v", got)
	}
}

// NOTHING IS ADDED WHERE THERE IS NOTHING TO ADD.
func TestNoGitHubSectionAndNoFailureAreLeftAlone(t *testing.T) {
	pointGitHubAt(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("GitHub was asked about a config with no github section")
	})

	got := explainGitHubAccess(t.Context(), &config.Config{}, errStartupFailure)
	if got == nil || got.Error() != errStartupFailure.Error() || !errors.Is(got, errStartupFailure) {
		t.Errorf("a config with no github section changed the failure: %v", got)
	}

	if got := explainGitHubAccess(t.Context(), githubAccessConfig(t), nil); got != nil {
		t.Errorf("a successful run was given an error: %v", got)
	}
}

// A CONFIGURED client_id IS NOT WHAT THIS CHECK SIGNED WITH, and the report has
// to say so.
//
// The scale-set client issues its JWT under github.client_id when one is set;
// this check signs with app_id, because that is what internal/github takes. So a
// STALE client_id fails the control plane while the App itself verifies
// perfectly — and a flat "the credential is not the problem" would then be a
// confident answer about a different credential, sending an operator away from
// the one line of config that is actually wrong.
func TestAVerifiedAppDoesNotVouchForAClientIdItNeverUsed(t *testing.T) {
	// COUNTED, because every assertion below is about what the report SAYS, and
	// an implementation that skipped GitHub and returned the qualification
	// unconditionally would satisfy all of them.
	var asked int

	pointGitHubAt(t, func(w http.ResponseWriter, r *http.Request) {
		asked++
		exactInstallation(w, r)
	})

	cfg := githubAccessConfig(t)
	cfg.GitHub.ClientID = "Iv1.deadbeefdeadbeef"

	got := explainGitHubAccess(t.Context(), cfg, errStartupFailure)

	keepsTheFailure(t, got)

	text := got.Error()

	if strings.Contains(text, "not the problem") {
		t.Errorf("a healthy installation was reported as clearing a client_id this check "+
			"never signed with:\n%v", got)
	}

	if !strings.Contains(text, "client_id") {
		t.Errorf("the report does not name the credential it did not check:\n%v", got)
	}

	if asked == 0 {
		t.Error("the report was written without asking GitHub anything")
	}
}
