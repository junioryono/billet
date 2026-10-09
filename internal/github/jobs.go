package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// JobRecords reads GitHub's REST record of the job one runner ran: its id, its
// name and its steps.
//
// FOUND BY THE RUNNER'S NAME IN THE RUN'S JOB LIST, NOT BY THE SCALE-SET
// MESSAGE'S jobId. That jobId is the Actions service's own identifier, a GUID
// (`1f4eedc2-3509-5c36-aac9-928a6a01b883` in the JobAssigned, JobStarted and
// JobCompleted messages published in actions/scaleset#75, read 2026-10-09), and
// the REST route for one job takes the numeric id, which no scale-set message
// carries. A billet runner is named after its lease, which is unique by
// construction, and GitHub records that name on the job the runner ran.
//
// Needs the App's `actions: read`, which billet requests by default.
type JobRecords interface {
	RunnerJob(ctx context.Context, owner, repository string, runID int64,
		runnerName string) (WorkflowJob, error)
}

// WorkflowJob is the part of GitHub's job record `billet jobs show` reads.
type WorkflowJob struct {
	ID         int64
	Name       string
	RunAttempt int64
	Steps      []JobStep
}

// JobStep is one step of a job as GitHub records it. StartedAt and
// CompletedAt are zero where GitHub gave no time, which a reader must say
// rather than read as an instant. GitHub's times are whole seconds.
type JobStep struct {
	Number      int64
	Name        string
	Status      string
	Conclusion  string
	StartedAt   time.Time
	CompletedAt time.Time
}

// ErrNoRunnerJob says the run's complete job list names no job on the runner.
var ErrNoRunnerJob = errors.New("github: the run lists no job on that runner")

// errJobRecordsUnavailable says the client was built without the credentials
// to read anything.
var errJobRecordsUnavailable = errors.New("github: job records are not configured")

// The bounds on one RunnerJob. A page is also bounded by maxPolicyResponse, so
// runJobsPerPage is small enough that a page of jobs with many steps each
// stays under it.
const (
	runJobsPerPage = 25
	// MaxRunJobs is the most jobs one run may list, across every attempt.
	MaxRunJobs = 1000
	// MaxJobSteps is the most steps the job may list.
	MaxJobSteps = 1000
	// maxRunJobPages is what MaxRunJobs takes at runJobsPerPage, so a server
	// answering a few jobs a page cannot make the read one request per job.
	maxRunJobPages = (MaxRunJobs + runJobsPerPage - 1) / runJobsPerPage
)

// NewJobRecordsAt builds the job-record reader for one target on the REST API
// at base, empty meaning the real GitHub. It is the policy client under a
// narrower name: one HTTP client, one installation token, one set of bounds.
func NewJobRecordsAt(base string, target Target, appID, installationID int64,
	privateKey []byte,
) JobRecords {
	if base == "" {
		base = apiBase
	}

	return newRunnerGroupPolicyClient(defaultPolicyBounds, base, target, appID, installationID, privateKey)
}

// jobRecord is one entry of GitHub's job list, every field a pointer so an
// absent one is told from an empty one. RunnerName is raw so that an absent
// field (could not tell) is told from null (no runner yet).
type jobRecord struct {
	ID         *int64          `json:"id"`
	Name       *string         `json:"name"`
	RunAttempt *int64          `json:"run_attempt"`
	RunnerName json.RawMessage `json:"runner_name"`
	Steps      *[]struct {
		Number      *int64  `json:"number"`
		Name        *string `json:"name"`
		Status      *string `json:"status"`
		Conclusion  *string `json:"conclusion"`
		StartedAt   *string `json:"started_at"`
		CompletedAt *string `json:"completed_at"`
	} `json:"steps"`
}

// RunnerJob reads the one job of a run that ran on runnerName, across every
// attempt of the run.
//
// THE WHOLE LIST IS READ, never the first match: a name that appeared twice
// would make either answer a guess, so two matches are refused, and a list
// longer than MaxRunJobs is could-not-tell rather than a search of its prefix.
func (c *runnerGroupPolicyClient) RunnerJob(
	ctx context.Context, owner, repository string, runID int64, runnerName string,
) (WorkflowJob, error) {
	if !c.configured() {
		return WorkflowJob{}, errJobRecordsUnavailable
	}
	if runID <= 0 {
		return WorkflowJob{}, fmt.Errorf("github: a workflow run needs a positive id, not %d", runID)
	}
	if strings.TrimSpace(runnerName) == "" {
		return WorkflowJob{}, errors.New("github: a runner's job needs the runner's name")
	}
	endpoint, err := repositoryEndpoint(c.base, owner, repository)
	if err != nil {
		return WorkflowJob{}, err
	}
	endpoint += "/actions/runs/" + strconv.FormatInt(runID, 10) + "/jobs"

	var matches []WorkflowJob
	listed := map[int64]bool{}
	total, read := -1, 0
	for page := 1; total < 0 || read < total; page++ {
		if page > maxRunJobPages {
			return WorkflowJob{}, fmt.Errorf("github: run %d's %d jobs did not arrive in %d pages",
				runID, total, maxRunJobPages)
		}
		query := url.Values{"filter": {"all"}, "per_page": {strconv.Itoa(runJobsPerPage)},
			"page": {strconv.Itoa(page)}}
		var body struct {
			TotalCount *int         `json:"total_count"`
			Jobs       *[]jobRecord `json:"jobs"`
		}
		if err := c.getJSON(ctx, endpoint+"?"+query.Encode(), "list the run's jobs", &body); err != nil {
			return WorkflowJob{}, err
		}
		if body.TotalCount == nil || body.Jobs == nil || *body.TotalCount < 0 {
			return WorkflowJob{}, errors.New("github: the run's job list was incomplete")
		}
		switch {
		case total < 0:
			total = *body.TotalCount
			if total > MaxRunJobs {
				return WorkflowJob{}, fmt.Errorf("github: run %d lists %d jobs, more than the %d billet reads",
					runID, total, MaxRunJobs)
			}
		case *body.TotalCount != total:
			return WorkflowJob{}, fmt.Errorf("github: run %d's job count changed from %d to %d while it was read",
				runID, total, *body.TotalCount)
		}
		if len(*body.Jobs) == 0 && read < total {
			return WorkflowJob{}, fmt.Errorf("github: run %d's job list ended after %d of the %d jobs it counts",
				runID, read, total)
		}
		read += len(*body.Jobs)
		if read > total {
			return WorkflowJob{}, fmt.Errorf("github: run %d listed more jobs than the %d it counts", runID, total)
		}
		for _, record := range *body.Jobs {
			// COUNTED BY ID, NOT BY ENTRY: a list that shifted between pages can
			// hand one job back twice, and two copies of one job would stand in for
			// a job never read.
			if record.ID == nil || *record.ID <= 0 {
				return WorkflowJob{}, fmt.Errorf("github: run %d lists a job with no id", runID)
			}
			if listed[*record.ID] {
				return WorkflowJob{}, fmt.Errorf("github: run %d listed job %d twice while it was read",
					runID, *record.ID)
			}
			listed[*record.ID] = true
			name, err := record.runner()
			if err != nil {
				return WorkflowJob{}, fmt.Errorf("github: run %d: %w", runID, err)
			}
			if name != runnerName {
				continue
			}
			job, err := workflowJobOf(record)
			if err != nil {
				return WorkflowJob{}, err
			}
			matches = append(matches, job)
		}
	}

	switch len(matches) {
	case 0:
		return WorkflowJob{}, fmt.Errorf("%w: run %d, runner %q", ErrNoRunnerJob, runID, runnerName)
	case 1:
		return matches[0], nil
	default:
		return WorkflowJob{}, fmt.Errorf("github: run %d lists %d jobs on runner %q; refusing to guess",
			runID, len(matches), runnerName)
	}
}

// runner is the name of the runner a listed job ran on, empty for a job GitHub
// gave none (null). A job whose record leaves the field out, or gives it as
// something other than a string, is refused: it could be the one asked for.
func (r jobRecord) runner() (string, error) {
	if len(r.RunnerName) == 0 {
		return "", errors.New("a job in the list does not say which runner it ran on")
	}
	var name *string
	if err := json.Unmarshal(r.RunnerName, &name); err != nil {
		return "", fmt.Errorf("a job in the list names its runner unreadably: %w", err)
	}
	if name == nil {
		return "", nil
	}

	return *name, nil
}

// workflowJobOf reads one matched job. Its id, name and step list are
// required; a step's times are not, because GitHub leaves them out of a step
// that never ran, but a time that is present and unreadable is refused rather
// than read as absent.
func workflowJobOf(record jobRecord) (WorkflowJob, error) {
	if record.ID == nil || *record.ID <= 0 || record.Name == nil || record.Steps == nil {
		return WorkflowJob{}, errors.New("github: the runner's job record was incomplete")
	}
	if len(*record.Steps) > MaxJobSteps {
		return WorkflowJob{}, fmt.Errorf("github: job %d lists %d steps, more than the %d billet reads",
			*record.ID, len(*record.Steps), MaxJobSteps)
	}
	job := WorkflowJob{ID: *record.ID, Name: *record.Name}
	if record.RunAttempt != nil {
		job.RunAttempt = *record.RunAttempt
	}
	for _, s := range *record.Steps {
		if s.Number == nil || s.Name == nil {
			return WorkflowJob{}, fmt.Errorf("github: job %d lists an incomplete step", *record.ID)
		}
		step := JobStep{Number: *s.Number, Name: *s.Name}
		if s.Status != nil {
			step.Status = *s.Status
		}
		if s.Conclusion != nil {
			step.Conclusion = *s.Conclusion
		}
		var err error
		if step.StartedAt, err = stepTime(s.StartedAt); err != nil {
			return WorkflowJob{}, fmt.Errorf("github: job %d step %d: started_at: %w", *record.ID, *s.Number, err)
		}
		if step.CompletedAt, err = stepTime(s.CompletedAt); err != nil {
			return WorkflowJob{}, fmt.Errorf("github: job %d step %d: completed_at: %w", *record.ID, *s.Number, err)
		}
		job.Steps = append(job.Steps, step)
	}

	return job, nil
}

// stepTime reads a step time GitHub may leave null.
func stepTime(raw *string) (time.Time, error) {
	if raw == nil || *raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, *raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("not an RFC 3339 time: %w", err)
	}

	return t, nil
}
