package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/server"
)

// WHAT THE ACTIONS SERVICE ANSWERS AT REPOSITORY SCOPE, MEASURED.
//
// A repository-scoped target registers through /repos/{owner}/{repo} and holds
// an App with administration:write, and everything billet does after that goes
// through the vendored client's Actions-service calls, whose behaviour at
// repository scope nothing documents: the runner-group lookup EnsureScaleSet
// resolves the default group through (`_apis/runtime/runnergroups/?groupName=`),
// the scale-set create, the message session, the delete. Every one of them was
// established against organizations, and a repository has no runner groups in
// its settings, so whether the service answers a "default" group there decides
// whether provisioning at repository scope needs a branch of its own.
//
// IT CANNOT BE SUBSTITUTED WITH A FAKE: a fake answers what billet's author
// expects, which is the thing being measured.
//
// SO THIS RECORDS RATHER THAN ASSERTS. Each finding is written to a JSON report;
// what it does assert is the mechanics the findings rest on: the client is built
// for a repository target, the calls reach the service, and whatever was created
// is deleted before the run ends, so a measurement never leaves a scale set on
// somebody's repository.
//
// It needs a real repository-scoped App (billet github-app create --repository)
// and is skipped without one:
//
//	BILLET_LIVE_REPOSITORY=owner/name \
//	BILLET_LIVE_APP_ID=123456 \
//	BILLET_LIVE_INSTALLATION_ID=7891011 \
//	BILLET_LIVE_APP_KEY=/path/to/private-key.pem \
//	BILLET_LIVE_SCALE_SET=billet-scope-probe \
//	BILLET_LIVE_REPORT_DIR=/tmp/billet-scope-probe \
//	go test ./internal/integration/ -run TestLiveRepositoryScope -v -count=1
//
// IT RAN ON 2026-09-04 against a private repository under a personal account,
// and the answers are in upstream-references.md and the protocol skill.
func TestLiveRepositoryScope(t *testing.T) {
	repository := os.Getenv("BILLET_LIVE_REPOSITORY")
	if repository == "" {
		t.Skip("BILLET_LIVE_REPOSITORY is unset; this test needs a real repository-scoped " +
			"GitHub App and is opt-in")
	}

	owner, name, ok := config.SplitRepository(repository)
	if !ok {
		t.Fatalf("BILLET_LIVE_REPOSITORY %q is not owner/name", repository)
	}

	keyPath := liveRequired(t, "BILLET_LIVE_APP_KEY")
	setName := liveRequired(t, "BILLET_LIVE_SCALE_SET")

	appID, err := strconv.ParseInt(liveRequired(t, "BILLET_LIVE_APP_ID"), 10, 64)
	if err != nil {
		t.Fatalf("BILLET_LIVE_APP_ID: %v", err)
	}

	installationID, err := strconv.ParseInt(liveRequired(t, "BILLET_LIVE_INSTALLATION_ID"), 10, 64)
	if err != nil {
		t.Fatalf("BILLET_LIVE_INSTALLATION_ID: %v", err)
	}

	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read the App private key: %v", err)
	}

	report := &repositoryScopeReport{
		Repository: repository,
		ScaleSet:   setName,
		StartedAt:  time.Now().UTC(),
	}

	t.Cleanup(func() { report.write(t) })

	// THE INSTALLATION IS CONFIRMED THROUGH THE REST API FIRST, the way `billet
	// check` confirms it, so a finding below is about the Actions service and not
	// about a key that never verified.
	inst, err := github.VerifyAppAt(probeCtx(t), nil, "https://api.github.com", appID, key,
		github.RepositoryTarget(owner, name), installationID)
	if err != nil {
		t.Fatalf("verify the App against the repository: %v", err)
	}

	report.InstallationAccountType = inst.Account.Type
	report.InstallationPermissions = inst.Permissions

	t.Logf("installation %d verified: account type %q, permissions %v",
		installationID, inst.Account.Type, inst.Permissions)

	client, err := scaleset.New(scaleset.Config{
		Target:         github.RepositoryTarget(owner, name),
		ClientID:       strconv.FormatInt(appID, 10),
		InstallationID: installationID,
		PrivateKey:     string(key),
		AppID:          appID,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("build the scale-set client: %v", err)
	}

	// THE RUNNER-GROUP LOOKUP IS THE UNKNOWN. This is the call EnsureScaleSet
	// makes first, recorded on its own so the answer is legible whatever the
	// create does afterwards.
	group, err := client.LookupRunnerGroup(probeCtx(t), "")

	switch {
	case errors.Is(err, scaleset.ErrRunnerGroupAbsent):
		report.RunnerGroupLookup = "no group answered"
	case err != nil:
		report.RunnerGroupLookup = "error: " + err.Error()
	default:
		report.RunnerGroupLookup = "answered"
		report.RunnerGroupID = group.ID
		report.RunnerGroupName = group.Name
		report.RunnerGroupIsDefault = group.IsDefault
	}

	t.Logf("RECORDED: runner-group lookup for %q at repository scope: %s (id=%d name=%q default=%v)",
		scaleset.DefaultRunnerGroup, report.RunnerGroupLookup,
		report.RunnerGroupID, report.RunnerGroupName, report.RunnerGroupIsDefault)

	// A NAMED GROUP IS REFUSED BEFORE THE SERVICE IS ASKED; recorded as the
	// mechanism it is, because the measurement above is only meaningful if the
	// path a tier with a group would take is the refusal and not a lookup.
	if _, err := client.LookupRunnerGroup(probeCtx(t), "billet-trusted"); err != nil {
		report.NamedGroupRefusal = err.Error()
	} else {
		report.NamedGroupRefusal = "NOT REFUSED"
		t.Errorf("a named runner group on a repository target was looked up rather than refused")
	}

	// ENSURE, THE SAME CALL THE CONTROL PLANE MAKES, and delete whatever it made
	// whether or not the rest of this run gets to say so.
	set, err := client.EnsureScaleSet(probeCtx(t), setName, "", []string{setName})
	if err != nil {
		report.EnsureScaleSet = "error: " + err.Error()
		t.Logf("RECORDED: EnsureScaleSet at repository scope failed: %v", err)

		return
	}

	report.EnsureScaleSet = "created or found"
	report.ScaleSetID = set.ID
	report.ScaleSetGroup = set.Group

	t.Logf("RECORDED: scale set %q is id %d in group %q", set.Name, set.ID, set.Group)

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 60*time.Second)
		defer cancel()

		deleted, err := client.DeleteScaleSet(ctx, set.Name, "", []string{set.Name}, false)
		if err != nil {
			report.DeleteScaleSet = "error: " + err.Error()
			t.Errorf("delete the probe scale set %d: %v (it is still on %s)", set.ID, err, repository)

			return
		}

		if !deleted {
			report.DeleteScaleSet = "nothing to delete"
		} else {
			report.DeleteScaleSet = "deleted"
		}

		if _, _, err := client.Describe(ctx, set.Name, ""); err != nil {
			report.DescribeAfterDelete = "error: " + err.Error()
		} else {
			report.DescribeAfterDelete = "absent"
		}
	})

	described, labels, err := client.Describe(probeCtx(t), setName, "")

	switch {
	case err != nil:
		report.Describe = "error: " + err.Error()
	case described == nil:
		report.Describe = "absent"
	default:
		report.Describe = "present"
		report.DescribedLabels = strings.Join(labels, ",")
	}

	// ONE MESSAGE SESSION, OPENED AND CLOSED, because a listener at repository
	// scope is the whole point and a session is what it holds.
	session, err := client.Session(probeCtx(t), set.ID, "billet-scope-probe")
	if err != nil {
		report.Session = "error: " + err.Error()
		t.Logf("RECORDED: opening a session at repository scope failed: %v", err)

		return
	}

	report.Session = "opened"
	report.SessionStatistics = describeStatistics(session)

	closeCtx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	if err := session.Close(closeCtx); err != nil {
		report.SessionClose = "error: " + err.Error()
		t.Errorf("close the probe session: %v", err)
	} else {
		report.SessionClose = "closed"
	}

	t.Logf("RECORDED: session %s, %s; statistics %s", report.Session, report.SessionClose,
		report.SessionStatistics)
}

// describeStatistics renders what the session reported on open.
func describeStatistics(session server.Session) string {
	stats := session.Statistics()
	if stats == nil {
		return "none"
	}

	b, err := json.Marshal(stats)
	if err != nil {
		return "unrenderable: " + err.Error()
	}

	return string(b)
}

func liveRequired(t *testing.T, name string) string {
	t.Helper()

	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is required once BILLET_LIVE_REPOSITORY is set", name)
	}

	return v
}

// repositoryScopeReport is what one run measured.
//
// A RECORD, NOT AN ASSERTION: every field is an answer about GitHub's service at
// a scope its documentation does not cover, and the value of a run is the
// answer rather than the pass.
type repositoryScopeReport struct {
	Repository string    `json:"repository"`
	ScaleSet   string    `json:"scale_set"`
	StartedAt  time.Time `json:"started_at"`

	InstallationAccountType string            `json:"installation_account_type,omitempty"`
	InstallationPermissions map[string]string `json:"installation_permissions,omitempty"`

	// RunnerGroupLookup is what the vendored client's lookup of the default group
	// returned at repository scope: "answered" with the group, "no group
	// answered", or the error.
	RunnerGroupLookup    string `json:"runner_group_lookup"`
	RunnerGroupID        int    `json:"runner_group_id,omitempty"`
	RunnerGroupName      string `json:"runner_group_name,omitempty"`
	RunnerGroupIsDefault bool   `json:"runner_group_is_default,omitempty"`
	NamedGroupRefusal    string `json:"named_group_refusal,omitempty"`

	EnsureScaleSet string `json:"ensure_scale_set,omitempty"`
	ScaleSetID     int    `json:"scale_set_id,omitempty"`
	ScaleSetGroup  string `json:"scale_set_group,omitempty"`

	Describe        string `json:"describe,omitempty"`
	DescribedLabels string `json:"described_labels,omitempty"`

	Session           string `json:"session,omitempty"`
	SessionStatistics string `json:"session_statistics,omitempty"`
	SessionClose      string `json:"session_close,omitempty"`

	DeleteScaleSet      string `json:"delete_scale_set,omitempty"`
	DescribeAfterDelete string `json:"describe_after_delete,omitempty"`
}

// write records the report where BILLET_LIVE_REPORT_DIR says, and logs it
// either way, so a run whose directory could not be written still says what it
// found.
func (r *repositoryScopeReport) write(t *testing.T) {
	t.Helper()

	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Errorf("render the report: %v", err)

		return
	}

	t.Logf("REPORT:\n%s", b)

	dir := os.Getenv("BILLET_LIVE_REPORT_DIR")
	if dir == "" {
		return
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Errorf("create the report directory: %v", err)

		return
	}

	path := filepath.Join(dir, "repository-scope-"+r.StartedAt.Format("20060102T150405Z")+".json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Errorf("write the report: %v", err)

		return
	}

	t.Logf("report written to %s", path)
}
