package github

import "testing"

// A RUN'S JOBS OF ONE NAME ARE READ AS GITHUB KEEPS THEM, for a job GitHub may have
// stopped offering (#911): a completed run is not asked for its jobs, an unfinished
// one's jobs are read whole and only those of the name are kept, with each one's
// status and runner, and a job that does not say its status is could-not-tell.
func TestARunsJobsOfOneNameAreReadAsGitHubKeepsThem(t *testing.T) {
	t.Parallel()

	t.Run("a completed run", func(t *testing.T) {
		t.Parallel()

		s := newRerunServer(t)
		s.run = `{"id":31,"status":"completed","run_attempt":1}`
		s.jobs = `{"total_count":0,"jobs":[]}`

		got, err := s.JobsNamed(t.Context(), "acme", "api", 31, "Lint App")
		if err != nil || !got.Completed || got.Jobs != nil {
			t.Fatalf("JobsNamed = %+v, %v; want completed and no jobs", got, err)
		}
		if n := s.jobReads.Load(); n != 0 {
			t.Fatalf("a completed run's jobs were read %d times", n)
		}
	})

	t.Run("an unfinished run", func(t *testing.T) {
		t.Parallel()

		s := newRerunServer(t)
		s.run = `{"id":31,"status":"in_progress","run_attempt":1}`
		s.jobs = `{"total_count":3,"jobs":[
			{"id":1,"name":"Detect Changes","status":"completed","runner_name":"billet-a"},
			{"id":2,"name":"Lint App","status":"queued","runner_name":null},
			{"id":3,"name":"Lint App","status":"in_progress","runner_name":"billet-b"}]}`

		got, err := s.JobsNamed(t.Context(), "acme", "api", 31, "Lint App")
		if err != nil {
			t.Fatalf("JobsNamed: %v", err)
		}
		want := []NamedJob{{Status: JobStatusQueued}, {Status: "in_progress", RunnerName: "billet-b"}}
		if got.Completed || len(got.Jobs) != 2 || got.Jobs[0] != want[0] || got.Jobs[1] != want[1] {
			t.Fatalf("JobsNamed = %+v, want %+v", got, want)
		}
		if filter := s.seenFilter.Load(); filter != "latest" {
			t.Fatalf("the job list was read with filter %v, want the latest attempt", filter)
		}
	})

	t.Run("a job that does not say its status", func(t *testing.T) {
		t.Parallel()

		s := newRerunServer(t)
		s.run = `{"id":31,"status":"queued","run_attempt":1}`
		s.jobs = `{"total_count":1,"jobs":[{"id":2,"name":"Lint App","runner_name":null}]}`

		if got, err := s.JobsNamed(t.Context(), "acme", "api", 31, "Lint App"); err == nil {
			t.Fatalf("JobsNamed = %+v, want an error for a job with no status", got)
		}
	})
}
