package github

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A RUN'S WHOLE LATEST ATTEMPT IS READ AS GITHUB KEEPS IT, for a recovery (#911): a
// completed run is not asked for its jobs, an unfinished one's are read whole with
// each one's name, status, attempt and runner, and a record that does not say one of
// those is could-not-tell.
func TestARunsQueuedAttemptIsReadAsGitHubKeepsIt(t *testing.T) {
	t.Parallel()

	// A COMPLETED RUN SAYS HOW IT ENDED: cancelled is one a cancel ended, and one that
	// does not say is could-not-tell.
	for _, tc := range []struct {
		name, run string
		cancelled bool
		refused   bool
	}{
		{name: "a run a cancel ended", run: `{"id":31,"status":"completed","conclusion":"cancelled","run_attempt":2}`,
			cancelled: true},
		{name: "a run that ended on its own", run: `{"id":31,"status":"completed","conclusion":"failure","run_attempt":2}`},
		{name: "a completed run that does not say how", run: `{"id":31,"status":"completed","run_attempt":2}`,
			refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newRerunServer(t)
			s.run = tc.run

			got, err := s.QueuedAttempt(t.Context(), "acme", "api", 31)
			if tc.refused {
				if err == nil {
					t.Fatalf("QueuedAttempt = %+v, want an error", got)
				}

				return
			}
			if err != nil || !got.Completed || got.Cancelled != tc.cancelled || got.Attempt != 2 || got.Jobs != nil {
				t.Fatalf("QueuedAttempt = %+v, %v; want completed on attempt 2, cancelled %v, no jobs", got, err,
					tc.cancelled)
			}
			if n := s.jobReads.Load(); n != 0 {
				t.Fatalf("a completed run's jobs were read %d times", n)
			}
		})
	}

	t.Run("an unfinished run", func(t *testing.T) {
		t.Parallel()

		s := newRerunServer(t)
		s.run = `{"id":31,"status":"queued","run_attempt":1}`
		s.jobs = `{"total_count":2,"jobs":[
			{"id":1,"name":"Detect Changes","status":"completed","run_attempt":1,"runner_name":"billet-a"},
			{"id":2,"name":"Lint App","status":"queued","run_attempt":1,"runner_name":null}]}`

		got, err := s.QueuedAttempt(t.Context(), "acme", "api", 31)
		if err != nil {
			t.Fatalf("QueuedAttempt: %v", err)
		}
		want := []QueuedJob{{Name: "Detect Changes", Status: "completed", Attempt: 1, RunnerName: "billet-a"},
			{Name: "Lint App", Status: JobStatusQueued, Attempt: 1}}
		if got.Completed || got.Attempt != 1 || len(got.Jobs) != 2 || got.Jobs[0] != want[0] || got.Jobs[1] != want[1] {
			t.Fatalf("QueuedAttempt = %+v, want %+v", got, want)
		}
		if filter := s.seenFilter.Load(); filter != "latest" {
			t.Fatalf("the job list was read with filter %v, want the latest attempt", filter)
		}
	})

	for _, tc := range []struct{ name, run, jobs string }{
		{"a run that does not say its attempt", `{"id":31,"status":"queued"}`, `{"total_count":0,"jobs":[]}`},
		{"a job that does not say its attempt", `{"id":31,"status":"queued","run_attempt":1}`,
			`{"total_count":1,"jobs":[{"id":2,"name":"Lint App","status":"queued","runner_name":null}]}`},
		{"a job that does not say its name", `{"id":31,"status":"queued","run_attempt":1}`,
			`{"total_count":1,"jobs":[{"id":2,"status":"queued","run_attempt":1,"runner_name":null}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newRerunServer(t)
			s.run, s.jobs = tc.run, tc.jobs

			if got, err := s.QueuedAttempt(t.Context(), "acme", "api", 31); err == nil {
				t.Fatalf("QueuedAttempt = %+v, want an error", got)
			}
		})
	}

	// A RUN PAST THE BOUND, every record of it whole, is refused for its size on the
	// first page: the read never costs more than its bound.
	t.Run("a run past the bound", func(t *testing.T) {
		t.Parallel()

		s := newRerunServer(t)
		s.run = `{"id":31,"status":"queued","run_attempt":1}`
		s.jobs = `{"total_count":101,"jobs":[{"id":2,"name":"Lint App","status":"queued","run_attempt":1,` +
			`"runner_name":null}]}`

		_, err := s.QueuedAttempt(t.Context(), "acme", "api", 31)
		if err == nil || !strings.Contains(err.Error(), "lists 101 jobs, more than the 100 billet reads") {
			t.Fatalf("QueuedAttempt = %v, want a refusal for the run's size", err)
		}
		if n := s.jobReads.Load(); n != 1 {
			t.Fatalf("read %d pages of a run past the bound, want the one that counts it", n)
		}
	})
}

// A CANCEL IS TAKEN ON GITHUB'S 202 ALONE, and a cancel of a run already over is told
// from every other refusal, as measured on 2026-11-22.
func TestACancelsAnswerIsClassified(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		status    int
		body      string
		accepted  bool
		completed bool
	}{
		{name: "accepted", status: http.StatusAccepted, accepted: true},
		{name: "a 201 is not a cancel's word", status: http.StatusCreated},
		{name: "a run already over", status: http.StatusConflict, completed: true,
			body: `{"message":"Cannot cancel a workflow run that is completed.","status":"409"}`},
		{name: "no actions: write", status: http.StatusForbidden,
			body: `{"message":"Resource not accessible by integration"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newRerunServer(t)
			s.answer = func(w http.ResponseWriter) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}

			err := s.CancelRun(t.Context(), "acme", "api", 31)
			if (err == nil) != tc.accepted {
				t.Fatalf("CancelRun = %v, want accepted %v", err, tc.accepted)
			}
			if got := errors.Is(err, ErrRunAlreadyCompleted); got != tc.completed {
				t.Fatalf("completed = %v, want %v: %v", got, tc.completed, err)
			}
			if n := s.cancels.Load(); n != 1 {
				t.Fatalf("the cancel route was asked %d times, want once", n)
			}
		})
	}

	// A REFUSAL WHOSE BODY BROKE OFF IS STILL GITHUB'S REFUSAL, read from its status: a
	// cancel counted as one that may have landed would let billet take someone else's
	// cancel for its own.
	for _, tc := range []struct {
		name      string
		status    string
		code      int
		completed bool
	}{
		{"a 409 cut short", "409 Conflict", http.StatusConflict, true},
		{"a 403 cut short", "403 Forbidden", http.StatusForbidden, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newRerunServer(t)
			s.answer = cutShort(t, tc.status)

			err := s.CancelRun(t.Context(), "acme", "api", 31)
			api, ok := errors.AsType[*APIError](err)
			if !ok || api.Status != tc.code {
				t.Fatalf("CancelRun = %v, want GitHub's refusal with status %d", err, tc.code)
			}
			if got := errors.Is(err, ErrRunAlreadyCompleted); got != tc.completed {
				t.Fatalf("completed = %v, want %v: %v", got, tc.completed, err)
			}
		})
	}

	// AND A RE-RUN'S IS NOT: its 403 needs the words that tell a run already going from
	// a refused App, so one cut short stays a lost answer, as it was.
	t.Run("a re-run's 403 cut short", func(t *testing.T) {
		t.Parallel()

		s := newRerunServer(t)
		s.answer = cutShort(t, "403 Forbidden")

		err := s.RerunFailedJobs(t.Context(), "acme", "api", 31)
		if api, ok := errors.AsType[*APIError](err); ok || err == nil {
			t.Fatalf("RerunFailedJobs = %v (refusal %+v), want a lost answer, not GitHub's refusal", err, api)
		}
	})

	// A CANCEL THAT NEVER LEFT SAYS SO: no installation token, no request.
	t.Run("no token", func(t *testing.T) {
		t.Parallel()

		s := newRerunServer(t)
		s.tokenFails.Store(true)

		if err := s.CancelRun(t.Context(), "acme", "api", 31); !errors.Is(err, ErrNotSent) {
			t.Fatalf("CancelRun = %v, want ErrNotSent", err)
		}
		if n := s.cancels.Load(); n != 0 {
			t.Fatalf("the cancel route was asked %d times without a token", n)
		}
	})

	// AND A RE-RUN'S 409 IS NOT A CANCEL'S: only the cancel reads it as a run over.
	s := newRerunServer(t)
	s.answer = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"message":"conflict"}`)
	}
	if err := s.RerunFailedJobs(t.Context(), "acme", "api", 31); err == nil || errors.Is(err, ErrRunAlreadyCompleted) {
		t.Fatalf("a re-run's 409 = %v, want a refusal that is not a run already over", err)
	}
}

// cutShort answers with status and a body that breaks off after a few bytes.
func cutShort(t *testing.T, status string) func(http.ResponseWriter) {
	t.Helper()

	return func(w http.ResponseWriter) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("the test server cannot hand over its connection")

			return
		}
		conn, buf, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)

			return
		}
		fmt.Fprintf(buf, "HTTP/1.1 %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"mess", status)
		_ = buf.Flush()
		_ = conn.Close()
	}
}
