package github

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

// RunQueue reads what a run's jobs of one name are doing, for a job GitHub may have
// stopped offering (#911). It needs only `actions: read`.
type RunQueue interface {
	JobsNamed(ctx context.Context, owner, repository string, runID int64, name string) (NamedJobs, error)
}

// NamedJobs is a run as GitHub records it now, and the jobs of its latest attempt with
// one name.
type NamedJobs struct {
	// Completed is a run with no job left to finish; Jobs is then not read.
	Completed bool
	Jobs      []NamedJob
}

// NamedJob is one job of a run's latest attempt: GitHub's status for it, and the
// runner it ran on, empty for one no runner took.
type NamedJob struct {
	Status     string
	RunnerName string
}

// JobStatusQueued is the status of a job no runner has taken.
const JobStatusQueued = "queued"

var _ RunQueue = (*runnerGroupPolicyClient)(nil)

// strandedMaxJobs bounds one JobsNamed to the run and four pages of jobs, so a
// background read of a large run costs five requests, not forty-one. A latest attempt
// with more jobs is an error: could-not-tell.
const strandedMaxJobs = 4 * runJobsPerPage

// JobsNamed reads a run's status and, unless the run is completed, every job of its
// latest attempt named name. The whole list or an error, as listRunJobs is: a job
// missing from a prefix is not a job GitHub stopped offering, and nor is one whose
// record does not say its name, since it could be one of this name.
func (c *runnerGroupPolicyClient) JobsNamed(ctx context.Context, owner, repository string, runID int64,
	name string,
) (NamedJobs, error) {
	if !c.configured() {
		return NamedJobs{}, ErrRunEvidenceUnavailable
	}
	if runID <= 0 {
		return NamedJobs{}, fmt.Errorf("github: a workflow run needs a positive id, not %d", runID)
	}
	if name == "" {
		return NamedJobs{}, errors.New("github: a job is read by its name, and none was given")
	}
	endpoint, err := repositoryEndpoint(c.base, owner, repository)
	if err != nil {
		return NamedJobs{}, err
	}

	var body struct {
		ID     *int64  `json:"id"`
		Status *string `json:"status"`
	}
	if err := c.getJSON(ctx, endpoint+"/actions/runs/"+strconv.FormatInt(runID, 10),
		"read workflow run", &body); err != nil {
		return NamedJobs{}, err
	}
	if body.ID == nil || body.Status == nil {
		return NamedJobs{}, errors.New("github: the workflow run response was incomplete")
	}
	if *body.ID != runID {
		return NamedJobs{}, fmt.Errorf("github: asked for workflow run %d and was answered with %d", runID, *body.ID)
	}
	if *body.Status == RunStatusCompleted {
		return NamedJobs{Completed: true}, nil
	}

	var out NamedJobs
	err = c.listRunJobs(ctx, owner, repository, runID, "latest", strandedMaxJobs, func(record jobRecord,
		runner string,
	) error {
		if record.Name == nil {
			return errors.New("github: a job of the run does not say its name")
		}
		if *record.Name != name {
			return nil
		}
		if record.Status == nil {
			return fmt.Errorf("github: a job named %q does not say its status", name)
		}
		out.Jobs = append(out.Jobs, NamedJob{Status: *record.Status, RunnerName: runner})

		return nil
	})
	if err != nil {
		return NamedJobs{}, err
	}

	return out, nil
}
