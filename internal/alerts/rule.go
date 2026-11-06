package alerts

import (
	"encoding/json"
	"path"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// Condition is one thing a rule can be about. The words are what the ledger's marks
// and a notice carry.
type Condition string

const (
	// Failed is a job GitHub reported as anything but succeeded or cancelled.
	Failed Condition = "failed"
	// Cancelled is a job GitHub reported as cancelled.
	Cancelled Condition = "cancelled"
	// Disrupted is a job billet's own infrastructure disrupted.
	Disrupted Condition = "disrupted"
	// RanLong is a job still running past the rule's duration, or ended after it.
	RanLong Condition = "running-long"
	// WaitedLong is a job that started after waiting past the rule's duration.
	WaitedLong Condition = "waited-long"
)

// GitHub's words on a completed job. `succeeded` and `canceled` are the two the
// scale-set messages were measured to carry beside `failed` (2026-11-04,
// docs/reference/records/job-reruns.md). Any other word is read as failed, because
// an unknown result is could-not-tell and a rule must not read it as fine.
const (
	resultSucceeded = "succeeded"
	resultCanceled  = "canceled"
)

// Job is what a rule is judged against: one row of the ledger's job history, in the
// terms a rule reads. An empty string or a zero time is not recorded.
type Job struct {
	LeaseID     string
	Tier        string
	Node        string
	Repository  string
	WorkflowRef string
	JobName     string
	RunID       int64
	Result      string
	Disruption  string
	// Rerun is what billet decided about re-running the job.
	Rerun string
	// HeadBranch is the run's branch as the ledger holds it, empty where it does not.
	HeadBranch string
	StartedAt  time.Time
	ResultAt   time.Time
	QueuedAt   time.Time
}

// Workflow is the job's workflow file within its repository,
// `.github/workflows/x.yml`, read from the ref GitHub gave the job. A job inside a
// reusable workflow carries the called workflow's file, not its caller's.
func (j *Job) Workflow() string {
	ref, _, _ := strings.Cut(j.WorkflowRef, "@")
	if _, tail, ok := strings.Cut(ref, "/.github/"); ok {
		return ".github/" + tail
	}

	return ref
}

// Key is what a rule's streak and cooldown on one condition are counted by: one job
// of one workflow of one repository, whichever tier or attempt ran it, so each
// condition of a rule is counted apart, as the docs promise.
//
// A JSON ARRAY, so no name a workflow author chose can make two jobs one key, and no
// byte PostgreSQL's text refuses, a NUL, is ever written.
func (j *Job) Key(condition Condition) string {
	key, err := json.Marshal([]string{string(condition), j.Repository, j.Workflow(), j.JobName})
	if err != nil {
		// Marshalling strings cannot fail; a key that says so is never a real job's.
		return "unencodable"
	}

	return string(key)
}

// KeyParts is a Key read back: its condition, repository, workflow and job, false
// for one that is not a key.
func KeyParts(key string) (Condition, string, string, string, bool) {
	var parts []string
	if err := json.Unmarshal([]byte(key), &parts); err != nil || len(parts) != 4 {
		return "", "", "", "", false
	}

	return Condition(parts[0]), parts[1], parts[2], parts[3], true
}

// Verdict is what a condition says about a job.
type Verdict int

const (
	// Later is not yet knowable: nothing is recorded, and the job is asked about on a
	// later pass. The zero value, because could-not-tell decides nothing.
	Later Verdict = iota
	// Hit is a job the condition holds for.
	Hit
	// Miss is a job the condition does not hold for, which ends a streak.
	Miss
)

// Rule is one of `server.alerts.rules`, parsed.
type Rule struct {
	Name         string
	Match        config.AlertMatch
	Failed       bool
	Cancelled    bool
	Disrupted    bool
	RunningOver  time.Duration
	WaitedOver   time.Duration
	InARow       int64
	Cooldown     time.Duration
	Destinations []string
}

// NewRule parses a validated rule.
func NewRule(r *config.AlertRule) (Rule, error) {
	running, err := r.On.RunningOver()
	if err != nil {
		return Rule{}, err
	}

	waited, err := r.On.WaitedOver()
	if err != nil {
		return Rule{}, err
	}

	cooldown, err := r.CooldownDuration()
	if err != nil {
		return Rule{}, err
	}

	return Rule{Name: r.Name, Match: r.Match, Failed: r.On.Failed, Cancelled: r.On.Cancelled, Disrupted: r.On.Disrupted,
		RunningOver: running, WaitedOver: waited, InARow: int64(r.InARowOrOne()), Cooldown: cooldown,
		Destinations: r.Notify}, nil
}

// Conditions are the conditions the rule watches.
func (r *Rule) Conditions() []Condition {
	var out []Condition
	if r.Failed {
		out = append(out, Failed)
	}
	if r.Cancelled {
		out = append(out, Cancelled)
	}
	if r.Disrupted {
		out = append(out, Disrupted)
	}
	if r.RunningOver > 0 {
		out = append(out, RanLong)
	}
	if r.WaitedOver > 0 {
		out = append(out, WaitedLong)
	}

	return out
}

// NeedsBranch says whether judging the rule takes the run's branch from GitHub.
func (r *Rule) NeedsBranch() bool { return r.Match.Branch != "" }

// NeedsSteps says whether judging the rule takes the job's steps from GitHub.
func (r *Rule) NeedsSteps() bool { return r.Match.Step != "" }

// MatchesJob judges everything the ledger holds: the repository, the workflow, the
// job and the tier. The branch and the steps are GitHub's, and judged apart.
func (r *Rule) MatchesJob(j *Job) bool {
	return glob(r.Match.Repository, j.Repository) && glob(r.Match.Tier, j.Tier) && glob(r.Match.Job, j.JobName) &&
		matchesWorkflow(r.Match.Workflow, j.Workflow())
}

// MatchesBranch judges the run's branch.
func (r *Rule) MatchesBranch(branch string) bool { return glob(r.Match.Branch, branch) }

// MatchesSteps says whether a step whose name the rule names ended the way the
// condition is about. A job that went red in some other step is not this rule's.
func (r *Rule) MatchesSteps(steps []Step, condition Condition) bool {
	want := "failure"
	if condition == Cancelled {
		want = "cancelled"
	}

	for _, step := range steps {
		if step.Conclusion == want && glob(r.Match.Step, step.Name) {
			return true
		}
	}

	return false
}

// Step is one step of a job, as GitHub records it.
type Step struct {
	Name       string
	Conclusion string
}

// Judge is what one condition says about one job at now.
func (r *Rule) Judge(condition Condition, j *Job, now time.Time) Verdict {
	switch condition {
	case Failed:
		switch j.Result {
		case "":
			return Later
		case resultSucceeded, resultCanceled:
			return Miss
		default:
			return Hit
		}
	case Cancelled:
		switch j.Result {
		case "":
			return Later
		case resultCanceled:
			return Hit
		default:
			return Miss
		}
	case Disrupted:
		// AT ONCE, while the job may still be running: a disruption is recorded only
		// before GitHub's result, and hearing of a lost host minutes before GitHub
		// gives up on the job is what the condition is for.
		switch {
		case j.Disruption != "":
			return Hit
		case j.Result != "":
			return Miss
		default:
			return Later
		}
	case RanLong:
		return judgeDuration(j, r.RunningOver, now)
	case WaitedLong:
		// THE QUEUE TIME COMES FROM GITHUB'S REST RECORD, after the start, or never,
		// and a wait nobody knows is neither long nor short.
		if j.QueuedAt.IsZero() || j.StartedAt.IsZero() {
			return Later
		}
		if j.StartedAt.Sub(j.QueuedAt) > r.WaitedOver {
			return Hit
		}

		return Miss
	}

	return Later
}

// judgeDuration is whether a job ran past over: it did the moment it passes it,
// and a job that ended without doing so is a miss.
func judgeDuration(j *Job, over time.Duration, now time.Time) Verdict {
	if j.StartedAt.IsZero() {
		return Later
	}
	if j.Result == "" {
		if now.Sub(j.StartedAt) > over {
			return Hit
		}

		return Later
	}
	if j.ResultAt.Sub(j.StartedAt) > over {
		return Hit
	}

	return Miss
}

// glob is a pattern with `*` for any run of characters and `?` for one, `/` included,
// since a job's or a step's name is free text. An empty pattern matches everything:
// a rule names only what it narrows by. It backtracks only to the last `*`, so a
// name a workflow wrote costs at most the pattern's length times its own. Character
// by character, not byte by byte, so `?` matches one letter of any script.
func glob(patternText, valueText string) bool {
	if patternText == "" {
		return true
	}

	pattern, value := []rune(patternText), []rune(valueText)

	p, v, star, mark := 0, 0, -1, 0
	for v < len(value) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, v
			p++
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == value[v]):
			p++
			v++
		case star >= 0:
			mark++
			p, v = star+1, mark
		default:
			return false
		}
	}

	for p < len(pattern) && pattern[p] == '*' {
		p++
	}

	return p == len(pattern)
}

// matchesWorkflow judges a workflow against its whole path, or its file name for a
// pattern with no directory in it, which is what a person writes.
func matchesWorkflow(pattern, workflow string) bool {
	if strings.Contains(pattern, "/") {
		return glob(pattern, workflow)
	}

	return glob(pattern, path.Base(workflow))
}
