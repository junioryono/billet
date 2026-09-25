package server

import (
	"testing"

	"github.com/junioryono/billet/internal/alloc"
)

const ciRef = "acme/api/.github/workflows/ci.yml"

func authorityBinding(event, ref string) alloc.PoolRunner {
	return alloc.PoolRunner{
		LeaseID: "lease-1", JobID: "job-1", RunID: 31,
		Identity: alloc.JobIdentity{Owner: "acme", Repository: "api", Event: event,
			WorkflowRef: ciRef + "@" + ref},
	}
}

func authorityRun(event, headBranch string) WorkflowRun {
	return WorkflowRun{ID: 31, Event: event, HeadBranch: headBranch,
		HeadRepository: "acme/api", Repository: "acme/api",
		Path: ".github/workflows/ci.yml", HeadSHA: "abc"}
}

// EVERY ROW OF THE RULE, each one a way GitHub's evidence can agree or not.
// The rows that must not publish are the reason the table exists: each is a
// path by which a job that is not a reviewed default-branch run could write
// what the default branch reads.
func TestTheCacheAuthorityRule(t *testing.T) {
	t.Parallel()

	pr := func() WorkflowRun {
		run := authorityRun("pull_request", "feature")
		run.PullRequests = []RunPullRequest{{Number: 7, Base: "release"}}
		return run
	}

	for name, tc := range map[string]struct {
		binding    alloc.PoolRunner
		completion *Job
		run        WorkflowRun
		dflt       string
		want       CacheAuthority
	}{
		"a push to the default branch publishes": {
			binding: authorityBinding("push", "refs/heads/main"),
			run:     authorityRun("push", "main"), dflt: "main",
			want: CacheAuthority{Ref: "refs/heads/main", DefaultRef: "refs/heads/main",
				Proven: true, WriteOwnRef: true, PublishDefault: true},
		},
		"a schedule on the default branch publishes": {
			binding: authorityBinding("schedule", "refs/heads/main"),
			run:     authorityRun("schedule", "main"), dflt: "main",
			want: CacheAuthority{Ref: "refs/heads/main", DefaultRef: "refs/heads/main",
				Proven: true, WriteOwnRef: true, PublishDefault: true},
		},
		"a push to a feature branch writes only its own ref": {
			binding: authorityBinding("push", "refs/heads/feature"),
			run:     authorityRun("push", "feature"), dflt: "main",
			want: CacheAuthority{Ref: "refs/heads/feature", DefaultRef: "refs/heads/main",
				Proven: true, WriteOwnRef: true},
		},
		"a pull request writes its merge ref and restores from its base": {
			binding: authorityBinding("pull_request", "refs/pull/7/merge"),
			run:     pr(), dflt: "main",
			want: CacheAuthority{Ref: "refs/pull/7/merge", BaseRef: "refs/heads/release",
				DefaultRef: "refs/heads/main", Proven: true, WriteOwnRef: true},
		},
		"pull_request_target on the default branch reads it and writes nothing": {
			binding: authorityBinding("pull_request_target", "refs/heads/main"),
			run:     authorityRun("pull_request_target", "feature"), dflt: "main",
			want: CacheAuthority{Ref: "refs/heads/main", DefaultRef: "refs/heads/main",
				Proven: true},
		},
		"workflow_run on the default branch reads it and writes nothing": {
			binding: authorityBinding("workflow_run", "refs/heads/main"),
			run:     authorityRun("workflow_run", "main"), dflt: "main",
			want: CacheAuthority{Ref: "refs/heads/main", DefaultRef: "refs/heads/main",
				Proven: true},
		},
		"issue_comment off the default branch is not proven": {
			binding: authorityBinding("issue_comment", "refs/heads/feature"),
			run:     authorityRun("issue_comment", "feature"), dflt: "main",
		},
		"an event in neither set is not proven": {
			binding: authorityBinding("release", "refs/tags/v1"),
			run:     authorityRun("release", "v1"), dflt: "main",
		},
		"a fork's pull request is never proven": {
			binding: authorityBinding("pull_request", "refs/pull/7/merge"),
			run: func() WorkflowRun {
				run := pr()
				run.HeadRepository = "mallory/api"
				return run
			}(), dflt: "main",
		},
		"a reusable workflow pinned to main from a feature branch writes nothing": {
			binding: func() alloc.PoolRunner {
				b := authorityBinding("push", "refs/heads/main")
				b.Identity.WorkflowRef = "acme/api/.github/workflows/build.yml@refs/heads/main"
				return b
			}(),
			run: func() WorkflowRun {
				run := authorityRun("push", "feature")
				run.ReferencedWorkflows = []ReferencedWorkflow{
					{Path: "acme/api/.github/workflows/build.yml@refs/heads/main", SHA: "main-sha"}}
				return run
			}(), dflt: "main",
		},
		"a tag named like the default branch calling a pinned reusable workflow writes nothing": {
			binding: func() alloc.PoolRunner {
				b := authorityBinding("push", "refs/heads/main")
				b.Identity.WorkflowRef = "acme/api/.github/workflows/build.yml@refs/heads/main"
				return b
			}(),
			run: func() WorkflowRun {
				run := authorityRun("push", "main")
				run.ReferencedWorkflows = []ReferencedWorkflow{
					{Path: "acme/api/.github/workflows/build.yml@refs/heads/main", SHA: "main-sha"}}
				return run
			}(), dflt: "main",
		},
		"a local reusable workflow at the run's own commit is the run's own ref": {
			binding: func() alloc.PoolRunner {
				b := authorityBinding("push", "refs/heads/main")
				b.Identity.WorkflowRef = "acme/api/.github/workflows/build.yml@refs/heads/main"
				return b
			}(),
			run: func() WorkflowRun {
				run := authorityRun("push", "main")
				run.ReferencedWorkflows = []ReferencedWorkflow{
					{Path: "acme/api/.github/workflows/build.yml@refs/heads/main", SHA: "abc"}}
				return run
			}(), dflt: "main",
			want: CacheAuthority{Ref: "refs/heads/main", DefaultRef: "refs/heads/main",
				Proven: true, WriteOwnRef: true, PublishDefault: true},
		},
		"a cross-repository workflow is never the job's ref": {
			binding: func() alloc.PoolRunner {
				b := authorityBinding("push", "refs/heads/main")
				b.Identity.WorkflowRef = "acme/shared/.github/workflows/ci.yml@refs/heads/main"
				return b
			}(),
			run: authorityRun("push", "main"), dflt: "main",
		},
		"a tag push writes nothing": {
			binding: authorityBinding("push", "refs/tags/main"),
			run:     authorityRun("push", "main"), dflt: "main",
		},
		"a run GitHub records under another event writes nothing": {
			binding: authorityBinding("push", "refs/heads/main"),
			run:     authorityRun("workflow_dispatch", "main"), dflt: "main",
		},
		"another run writes nothing": {
			binding: authorityBinding("push", "refs/heads/main"),
			run: func() WorkflowRun {
				run := authorityRun("push", "main")
				run.ID = 32
				return run
			}(), dflt: "main",
		},
		"an unknown default branch writes nothing": {
			binding: authorityBinding("push", "refs/heads/main"),
			run:     authorityRun("push", "main"),
		},
		"a renamed default branch makes main an ordinary branch": {
			binding: authorityBinding("push", "refs/heads/main"),
			run:     authorityRun("push", "main"), dflt: "trunk",
			want: CacheAuthority{Ref: "refs/heads/main", DefaultRef: "refs/heads/trunk",
				Proven: true, WriteOwnRef: true},
		},
		"a binding with no identity writes nothing": {
			binding: alloc.PoolRunner{LeaseID: "lease-1", JobID: "job-1", RunID: 31},
			run:     authorityRun("push", "main"), dflt: "main",
		},
		"a completion for another job writes nothing": {
			binding: authorityBinding("push", "refs/heads/main"),
			completion: &Job{JobID: "job-2", RunID: 31, Owner: "acme", Repository: "api",
				Event: "push", WorkflowRef: ciRef + "@refs/heads/main"},
			run: authorityRun("push", "main"), dflt: "main",
		},
		"a completion that agrees with the binding publishes": {
			binding: authorityBinding("push", "refs/heads/main"),
			completion: &Job{JobID: "job-1", RunID: 31, Owner: "ACME", Repository: "api",
				Event: "push", WorkflowRef: ciRef + "@refs/heads/main"},
			run: authorityRun("push", "main"), dflt: "main",
			want: CacheAuthority{Ref: "refs/heads/main", DefaultRef: "refs/heads/main",
				Proven: true, WriteOwnRef: true, PublishDefault: true},
		},
		"a completion restored without its identity writes nothing": {
			binding:    authorityBinding("push", "refs/heads/main"),
			completion: &Job{RunID: 31},
			run:        authorityRun("push", "main"), dflt: "main",
		},
	} {
		got := DecideCacheAuthority("lease-1", tc.binding, tc.completion, tc.run, tc.dflt)
		want := tc.want
		want.LeaseID, want.JobID, want.RunID = "lease-1", tc.binding.JobID, tc.binding.RunID
		want.Owner, want.Repository = tc.binding.Identity.Owner, tc.binding.Identity.Repository
		want.Event = tc.binding.Identity.Event
		if got != want {
			t.Errorf("%s:\n got %+v\nwant %+v", name, got, want)
		}
	}
}

// THE ALLOWLIST IS GITHUB'S, NO WIDER AND NO NARROWER. Pinned both ways, so a
// trigger added to the set fails here by name as surely as one removed.
func TestTheDefaultBranchWritersAreGitHubsOwn(t *testing.T) {
	t.Parallel()

	want := []string{"delete", "page_build", "push", "registry_package",
		"repository_dispatch", "schedule", "workflow_dispatch"}
	if len(defaultBranchWriters) != len(want) {
		t.Errorf("writers = %v, want exactly %v", defaultBranchWriters, want)
	}
	for _, event := range want {
		if !defaultBranchWriters[event] {
			t.Errorf("%s is missing from the writers", event)
		}
	}
}
