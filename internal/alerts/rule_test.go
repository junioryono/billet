package alerts

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

var ruleNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// EACH CONDITION'S THREE ANSWERS, the third being the one that matters most: what
// cannot be known yet records nothing, so a job still running is not a success and a
// wait nobody measured is not a short one.
func TestEachConditionJudgesAJob(t *testing.T) {
	t.Parallel()

	rule := Rule{RunningOver: time.Hour, WaitedOver: 10 * time.Minute}
	started := ruleNow.Add(-2 * time.Hour)
	queued := func(wait time.Duration) time.Time { return started.Add(-wait) }

	for _, tc := range []struct {
		name      string
		condition Condition
		job       Job
		want      Verdict
	}{
		{"running, failed", Failed, Job{}, Later},
		{"failed, failed", Failed, Job{Result: "failed"}, Hit},
		{"a word GitHub adds tomorrow, failed", Failed, Job{Result: "exploded"}, Hit},
		{"succeeded, failed", Failed, Job{Result: "succeeded"}, Miss},
		{"cancelled, failed", Failed, Job{Result: "canceled"}, Miss},

		{"running, cancelled", Cancelled, Job{}, Later},
		{"cancelled, cancelled", Cancelled, Job{Result: "canceled"}, Hit},
		{"failed, cancelled", Cancelled, Job{Result: "failed"}, Miss},

		{"disrupted while running", Disrupted, Job{Disruption: "reclaimed"}, Hit},
		{"disrupted and ended", Disrupted, Job{Disruption: "reclaimed", Result: "failed"}, Hit},
		{"ended undisrupted", Disrupted, Job{Result: "failed"}, Miss},
		{"running undisrupted", Disrupted, Job{}, Later},

		{"running past it", RanLong, Job{StartedAt: started}, Hit},
		{"running under it", RanLong, Job{StartedAt: ruleNow.Add(-time.Minute)}, Later},
		{"ended after it", RanLong, Job{StartedAt: started, Result: "succeeded", ResultAt: started.Add(90 * time.Minute)}, Hit},
		{"ended under it", RanLong, Job{StartedAt: started, Result: "succeeded", ResultAt: started.Add(time.Minute)}, Miss},
		{"never started", RanLong, Job{}, Later},

		{"waited past it", WaitedLong, Job{StartedAt: started, QueuedAt: queued(time.Hour)}, Hit},
		{"waited under it", WaitedLong, Job{StartedAt: started, QueuedAt: queued(time.Minute)}, Miss},
		{"exactly at it", WaitedLong, Job{StartedAt: started, QueuedAt: queued(10 * time.Minute)}, Miss},
		{"no queue time recorded", WaitedLong, Job{StartedAt: started}, Later},

		{"a condition this binary does not know", Condition("newer"), Job{Result: "failed"}, Later},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := rule.Judge(tc.condition, &tc.job, ruleNow); got != tc.want {
				t.Errorf("Judge = %d, want %d", got, tc.want)
			}
		})
	}
}

// THE ZERO VERDICT DECIDES NOTHING, so a verdict nobody set cannot count or fire.
func TestTheZeroVerdictIsLater(t *testing.T) {
	t.Parallel()

	var zero Verdict
	if zero != Later {
		t.Error("the zero verdict decides something")
	}
}

// A RULE NARROWS BY WHAT IT NAMES AND NOTHING ELSE, each field held by the row that
// differs in it alone.
func TestARuleMatchesWhatItNames(t *testing.T) {
	t.Parallel()

	job := Job{Repository: "acme/api", Tier: "billet-8vcpu", JobName: "deploy (eu/west)",
		WorkflowRef: "acme/api/.github/workflows/deploy.yml@refs/heads/main"}

	for _, tc := range []struct {
		name  string
		match config.AlertMatch
		want  bool
	}{
		{"nothing named", config.AlertMatch{}, true},
		{"the repository", config.AlertMatch{Repository: "acme/api"}, true},
		{"a repository glob", config.AlertMatch{Repository: "acme/*"}, true},
		{"another repository", config.AlertMatch{Repository: "other/*"}, false},
		{"the tier", config.AlertMatch{Tier: "billet-*"}, true},
		{"another tier", config.AlertMatch{Tier: "billet-2vcpu"}, false},
		{"the job, across a slash", config.AlertMatch{Job: "deploy*"}, true},
		{"another job", config.AlertMatch{Job: "build*"}, false},
		{"one character", config.AlertMatch{Job: "deploy (eu?west)"}, true},
		{"the workflow's file", config.AlertMatch{Workflow: "deploy.yml"}, true},
		{"a file glob", config.AlertMatch{Workflow: "dep*.yml"}, true},
		{"the workflow's path", config.AlertMatch{Workflow: ".github/workflows/deploy.yml"}, true},
		{"another workflow", config.AlertMatch{Workflow: "ci.yml"}, false},
		{"all together", config.AlertMatch{Repository: "acme/*", Tier: "billet-*", Job: "deploy*", Workflow: "deploy.yml"}, true},
		{"all but one", config.AlertMatch{Repository: "acme/*", Tier: "billet-*", Job: "build*", Workflow: "deploy.yml"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rule := Rule{Match: tc.match}
			if got := rule.MatchesJob(&job); got != tc.want {
				t.Errorf("MatchesJob = %v, want %v", got, tc.want)
			}
		})
	}
}

// THE BRANCH AND THE STEPS ARE ASKED OF GITHUB ONLY FOR THE RULE THAT NAMES THEM.
func TestOnlyARuleNamingThemNeedsGitHub(t *testing.T) {
	t.Parallel()

	plain := Rule{Match: config.AlertMatch{Repository: "acme/*"}}
	if plain.NeedsBranch() || plain.NeedsSteps() {
		t.Error("a rule naming neither asks GitHub")
	}
	narrow := Rule{Match: config.AlertMatch{Branch: "main", Step: "Deploy*"}}
	if !narrow.NeedsBranch() || !narrow.NeedsSteps() {
		t.Error("a rule naming them does not ask")
	}
	if !narrow.MatchesBranch("main") || narrow.MatchesBranch("feature") {
		t.Error("the branch is not judged")
	}
}

// A STEP MATCHES WHEN IT ENDED THE WAY THE CONDITION IS ABOUT: a job that went red in
// some other step is not this rule's.
func TestAStepMatchesOnlyTheWayTheConditionIsAbout(t *testing.T) {
	t.Parallel()

	rule := Rule{Match: config.AlertMatch{Step: "Deploy*"}}
	steps := []Step{{Name: "Build", Conclusion: "failure"}, {Name: "Deploy to eu", Conclusion: "cancelled"},
		{Name: "Deploy to us", Conclusion: "success"}}

	if rule.MatchesSteps(steps, Failed) {
		t.Error("a failure in another step matched")
	}
	if !rule.MatchesSteps(steps, Cancelled) {
		t.Error("the cancelled step did not match")
	}
	if !rule.MatchesSteps([]Step{{Name: "Deploy to eu", Conclusion: "failure"}}, Failed) {
		t.Error("the failed step did not match")
	}
	if rule.MatchesSteps(nil, Failed) {
		t.Error("no steps matched")
	}
}

func TestGlob(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		pattern, value string
		want           bool
	}{
		{"", "anything", true},
		{"", "", true},
		{"a", "", false},
		{"*", "", true},
		{"*", "a/b c", true},
		{"a*c", "abbbc", true},
		{"a*c", "abbbd", false},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"*a*b*", "xaybz", true},
		{"*a*b*", "xbya", false},
		{"[ab]", "a", false},
		{"[ab]", "[ab]", true},
		{"exact", "exact", true},
		{"exact", "exact2", false},
		{"**", "x", true},
		{"a**", "a", true},
	} {
		if got := glob(tc.pattern, tc.value); got != tc.want {
			t.Errorf("glob(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}

	// A NAME A WORKFLOW CHOSE CANNOT MAKE A PATTERN EXPENSIVE: an exponential matcher
	// would not finish this.
	long := strings.Repeat("a", 4096)
	if glob(strings.Repeat("*a", 64)+"b", long) {
		t.Error("the long pattern matched")
	}
}

// A JOB'S WORKFLOW IS ITS FILE, read from the ref; its key is the job, by name, of a
// workflow of a repository, so a rule's streak follows the job across runs and tiers.
func TestAJobsWorkflowAndKey(t *testing.T) {
	t.Parallel()

	job := Job{Repository: "acme/api", JobName: "build", Tier: "a", LeaseID: "1",
		WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main"}
	if got := job.Workflow(); got != ".github/workflows/ci.yml" {
		t.Errorf("Workflow = %q", got)
	}

	same := job
	same.Tier, same.LeaseID = "b", "2"
	if job.Key() != same.Key() {
		t.Error("another tier and lease changed the key")
	}
	for name, other := range map[string]Job{
		"repository": {Repository: "acme/web", JobName: "build", WorkflowRef: job.WorkflowRef},
		"job":        {Repository: "acme/api", JobName: "test", WorkflowRef: job.WorkflowRef},
		"workflow":   {Repository: "acme/api", JobName: "build", WorkflowRef: "acme/api/.github/workflows/x.yml@refs/heads/main"},
	} {
		if job.Key() == other.Key() {
			t.Errorf("another %s kept the key", name)
		}
	}

	if got := (&Job{}).Workflow(); got != "" {
		t.Errorf("no ref = %q", got)
	}
}

// A NEW RULE TAKES WHAT THE CONFIG SAYS, with its defaults, and watches exactly the
// conditions it names.
func TestNewRule(t *testing.T) {
	t.Parallel()

	got, err := NewRule(&config.AlertRule{
		Name: "deploys", Match: config.AlertMatch{Repository: "acme/*"}, Notify: []string{"oncall"},
		On: config.AlertConditions{Failed: true, RunningLongerThan: "2h", WaitedLongerThan: "15m"},
	})
	if err != nil {
		t.Fatalf("NewRule: %v", err)
	}
	if got.Name != "deploys" || got.RunningOver != 2*time.Hour || got.WaitedOver != 15*time.Minute ||
		got.InARow != 1 || got.Cooldown != config.DefaultAlertCooldown || got.Match.Repository != "acme/*" ||
		len(got.Destinations) != 1 {
		t.Errorf("rule = %+v", got)
	}

	want := []Condition{Failed, RanLong, WaitedLong}
	conditions := got.Conditions()
	if len(conditions) != len(want) {
		t.Fatalf("conditions = %v", conditions)
	}
	for i := range want {
		if conditions[i] != want[i] {
			t.Errorf("conditions = %v, want %v", conditions, want)
		}
	}

	if _, err := NewRule(&config.AlertRule{Cooldown: "soon"}); err == nil {
		t.Error("an unparsable cooldown was accepted")
	}
}

// A NOTICE IS A CONTRACT, and each kind of destination gets its own body.
func TestANoticeRendersForItsDestination(t *testing.T) {
	t.Parallel()

	rule := Rule{Name: "deploys"}
	job := Job{LeaseID: "lease-1", Tier: "billet-8vcpu", Node: "epyc-1", Repository: "acme/api",
		WorkflowRef: "acme/api/.github/workflows/deploy.yml@refs/heads/main", JobName: "deploy", RunID: 31,
		Result: "failed", Disruption: "reclaimed", Rerun: "requested"}

	notice := NewNotice(&rule, Failed, &job, "main", ruleNow)

	raw, err := notice.Render(config.AlertWebhook)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for key, want := range map[string]any{
		"billet_alert": float64(1), "rule": "deploys", "condition": "failed", "repository": "acme/api",
		"workflow": ".github/workflows/deploy.yml", "job": "deploy", "branch": "main", "tier": "billet-8vcpu",
		"node": "epyc-1", "lease": "lease-1", "run_id": float64(31),
		"run_url": "https://github.com/acme/api/actions/runs/31", "result": "failed", "disruption": "reclaimed",
		"rerun": "requested", "seconds": float64(0), "fired_at": "2026-11-04T12:00:00Z",
	} {
		if fields[key] != want {
			t.Errorf("%s = %v, want %v", key, fields[key], want)
		}
	}

	slack, err := notice.Render(config.AlertSlack)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var body map[string]string
	if err := json.Unmarshal(slack, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body) != 1 {
		t.Errorf("a Slack body has %d fields, want text alone: %v", len(body), body)
	}
	for _, want := range []string{"deploys", "job deploy failed", "acme/api", "deploy.yml", "branch main", "billet-8vcpu",
		"reclaimed", "https://github.com/acme/api/actions/runs/31"} {
		if !strings.Contains(body["text"], want) {
			t.Errorf("the text does not say %q: %s", want, body["text"])
		}
	}
}

// A DURATION CONDITION SAYS HOW LONG, measured to the end where there is one.
func TestANoticeSaysHowLong(t *testing.T) {
	t.Parallel()

	rule := Rule{Name: "slow"}
	started := ruleNow.Add(-3 * time.Hour)

	running := Job{StartedAt: started}
	if got := NewNotice(&rule, RanLong, &running, "", ruleNow); got.Seconds != 3*3600 {
		t.Errorf("running = %d", got.Seconds)
	}

	ended := Job{StartedAt: started, ResultAt: started.Add(time.Hour)}
	if got := NewNotice(&rule, RanLong, &ended, "", ruleNow); got.Seconds != 3600 {
		t.Errorf("ended = %d", got.Seconds)
	}

	waited := Job{StartedAt: started, QueuedAt: started.Add(-time.Minute)}
	if got := NewNotice(&rule, WaitedLong, &waited, "", ruleNow); got.Seconds != 60 {
		t.Errorf("waited = %d", got.Seconds)
	}
	if got := NewNotice(&rule, Failed, &ended, "", ruleNow); got.Seconds != 0 {
		t.Errorf("a failure = %d", got.Seconds)
	}
	if got := NewNotice(&rule, Failed, &Job{RunID: 31}, "", ruleNow); got.RunURL != "" {
		t.Errorf("no repository = %q", got.RunURL)
	}
}

// A JOB'S NAME IS A WORKFLOW AUTHOR'S TO CHOOSE, and a channel's to read: it cannot
// mention the channel, a person, or make a link of itself.
func TestANoticeCannotMentionAChannelInSlack(t *testing.T) {
	t.Parallel()

	rule := Rule{Name: "r"}
	job := Job{JobName: "<!channel> <@U123> <https://evil.example|click> a&b", Repository: "acme/<!here>", RunID: 1}
	notice := NewNotice(&rule, Failed, &job, "<!everyone>", ruleNow)

	raw, err := notice.Render(config.AlertSlack)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var body map[string]string
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	text := body["text"]
	if strings.ContainsAny(text, "<>") {
		t.Errorf("a control character reached Slack: %s", text)
	}
	for _, want := range []string{"&lt;!channel&gt;", "&lt;@U123&gt;", "a&amp;b", "&lt;!everyone&gt;", "acme/&lt;!here&gt;"} {
		if !strings.Contains(text, want) {
			t.Errorf("the text does not say %q: %s", want, text)
		}
	}
}

// EVERY CONDITION HAS ITS SENTENCE, so a channel never reads a bare token.
func TestEveryConditionHasASentence(t *testing.T) {
	t.Parallel()

	for condition, want := range map[Condition]string{
		Failed: "failed", Cancelled: "was cancelled", Disrupted: "was disrupted", RanLong: "has run for 2m0s",
		WaitedLong: "waited 2m0s", Condition("newer"): "newer",
	} {
		if got := condition.sentence(120); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want it to say %q", condition, got, want)
		}
	}
}
