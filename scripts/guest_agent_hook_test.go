package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE HOOK IS THE ONLY THING A WORKFLOW EXPRESSION CAN READ.
//
// The agent putting BILLET_ACTIONS_CACHE_URL in the runner's environment is half
// the publication: `cache-to: type=gha,url_v2=${{ env.BILLET_ACTIONS_CACHE_URL }}`
// is evaluated against the env CONTEXT, which only GITHUB_ENV writes reach.
// Delete the hook's two lines and the agent test beside this one stays green
// while every opted-in build fails with the x509 this whole path removes.
//
// So the hook is extracted as it is installed and RUN, with the environment the
// runner gives it.
func TestTheJobStartedHookPublishesTheAdapterURLForWorkflowExpressions(t *testing.T) {
	t.Parallel()

	hook := jobStartedHook(t)

	for _, tc := range []struct {
		name string
		url  string
		want bool
	}{
		{name: "the adapter is serving", url: "http://127.0.0.1:41321/", want: true},
		// THE VARIABLE IS ABSENT, NOT EMPTY, when the adapter did not start, and
		// the hook runs under `set -u`: an unguarded read would fail the hook,
		// which the runner treats as a failed job. Interception is not conditional
		// on the adapter, so that would take the whole cache down with it.
		{name: "the adapter never started"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			source := filepath.Join(dir, "ca.pem")
			environment := filepath.Join(dir, "github.env")
			for _, file := range []string{source, environment} {
				if err := os.WriteFile(file, nil, 0o600); err != nil {
					t.Fatalf("write %s: %v", file, err)
				}
			}

			run := exec.CommandContext(t.Context(), "sh", "-c", hook)
			run.Env = append(os.Environ(),
				"RUNNER_TEMP="+dir,
				"GITHUB_ENV="+environment,
				"BILLET_ACTIONS_CA_SOURCE="+source,
			)
			if tc.url != "" {
				run.Env = append(run.Env, "BILLET_ACTIONS_CACHE_URL="+tc.url)
			}
			if output, err := run.CombinedOutput(); err != nil {
				t.Fatalf("the job-started hook failed: %v\n%s", err, output)
			}

			published, err := os.ReadFile(environment)
			if err != nil {
				t.Fatalf("read the published environment: %v", err)
			}
			// The CA is what the hook has always carried, and it must keep arriving
			// whether or not the adapter did.
			if !strings.Contains(string(published), "NODE_EXTRA_CA_CERTS=") {
				t.Errorf("the hook no longer publishes the trust bundle:\n%s", published)
			}
			entry := "BILLET_ACTIONS_CACHE_URL=" + tc.url
			if got := strings.Contains(string(published), entry); got != tc.want {
				t.Errorf("the workflow context carries the adapter URL=%v, want %v:\n%s",
					got, tc.want, published)
			}
		})
	}
}

// jobStartedHook returns the hook exactly as the agent installs it.
func jobStartedHook(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile("build-guest-image.sh")
	if err != nil {
		t.Fatalf("read build-guest-image.sh: %v", err)
	}

	_, rest, found := strings.Cut(string(raw), "<<'ACTIONS_HOOK'\n")
	if !found {
		t.Fatal("the agent no longer installs the job-started hook from a quoted heredoc")
	}
	hook, _, found := strings.Cut(rest, "\nACTIONS_HOOK\n")
	if !found {
		t.Fatal("the job-started hook has no heredoc terminator")
	}

	return hook
}

// THE SHIM'S DIRECTORY GOES FIRST ON EVERY STEP'S PATH, and every variable a
// client honours names the one trust bundle: the container hook can only mount
// the shim, and a Python client reading certifi's bundle only trusts the node's
// leaf if REQUESTS_CA_BUNDLE says so. Both travel through the same hook.
func TestTheJobStartedHookPutsTheShimFirstOnPathAndPublishesEveryTrustVariable(t *testing.T) {
	t.Parallel()

	hook := jobStartedHook(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "ca.pem")
	environment := filepath.Join(dir, "github.env")
	pathFile := filepath.Join(dir, "github.path")
	for _, file := range []string{source, environment, pathFile} {
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}

	run := exec.CommandContext(t.Context(), "sh", "-c", hook)
	run.Env = append(os.Environ(),
		"RUNNER_TEMP="+dir,
		"GITHUB_ENV="+environment,
		"GITHUB_PATH="+pathFile,
		"BILLET_ACTIONS_CA_SOURCE="+source,
	)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("the job-started hook failed: %v\n%s", err, output)
	}

	published, err := os.ReadFile(environment)
	if err != nil {
		t.Fatalf("read the published environment: %v", err)
	}
	bundle := filepath.Join(dir, "billet-actions-cache-ca.pem")
	for _, variable := range []string{"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"} {
		if !strings.Contains(string(published), variable+"="+bundle+"\n") {
			t.Errorf("the hook does not publish %s naming the bundle:\n%s", variable, published)
		}
	}

	prepended, err := os.ReadFile(pathFile)
	if err != nil {
		t.Fatalf("read the published PATH additions: %v", err)
	}
	if string(prepended) != "/opt/billet/bin\n" {
		t.Errorf("GITHUB_PATH received %q, want the shim's directory alone", prepended)
	}
}
