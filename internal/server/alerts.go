package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/alerts"
	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// JobSteps reads the steps of the job one runner ran. A target's Provisioner
// implements it over the App's REST client, and it needs `actions: read`.
type JobSteps interface {
	RunnerJobSteps(ctx context.Context, owner, repository string, runID int64, runnerName string) ([]alerts.Step, error)
}

// AlertDestination is one place a rule may notify: how to reach it, and what shape
// of body it takes.
type AlertDestination struct {
	Sender *alerts.Sender
	Kind   config.AlertDestinationKind
}

// Alerts is `server.alerts`, parsed and with its addresses read.
type Alerts struct {
	Rules        []alerts.Rule
	Destinations map[string]AlertDestination
}

// WithAlerts gives the control plane its alert rules. Nil, or a set with no rule,
// starts no loop.
func WithAlerts(a *Alerts) ControlPlaneOption {
	return func(s *Server) {
		if a != nil && len(a.Rules) > 0 {
			s.alerts = a
		}
	}
}

// WithAlertPacing sets how often the loops look for work, and the first wait after a
// delivery that failed. Exposed for WithCleanupRetry's reason.
func WithAlertPacing(evaluate, deliver, retry time.Duration) ControlPlaneOption {
	return func(s *Server) {
		s.alertEvaluateEvery, s.alertDeliverEvery, s.alertRetryFirst = evaluate, deliver, retry
	}
}

const (
	// alertEvaluateEvery is how often the rules look at the ledger.
	alertEvaluateEvery = 30 * time.Second
	// alertDeliverEvery is how often a destination's queue is looked at.
	alertDeliverEvery = 5 * time.Second
	// alertRunningWindow is how long a running job is watched. A job longer than this
	// has outgrown any rule that could still be useful about it.
	alertRunningWindow = 7 * 24 * time.Hour
	// alertEndedWindow is how far back an ended job is judged. A control plane down for
	// longer loses what ended meanwhile, which by then is nobody's news.
	alertEndedWindow = 6 * time.Hour
	// alertJobsRead bounds each of a pass's two reads. A backlog loses its oldest.
	alertJobsRead = 5000
	// alertReadWindow is how long a job waits for GitHub to answer what a rule needs,
	// after which it is judged unknown and said so, rather than waiting on an App that
	// lacks `actions: read` until it falls out of the read unnoticed.
	alertReadWindow = time.Hour
	// alertRunsPerInstallation is how many of GitHub's records one pass reads through
	// one App installation, each cached for the pass. It decides nothing about a
	// runner, so it is as small as the queue-time sweep's share.
	alertRunsPerInstallation = 3
	// alertReadsRemembered bounds the branches kept between passes: a run's branch does
	// not change, and a rule about a branch asks of every run.
	alertReadsRemembered = 4096
	// alertMarkRetention is how long a judgement is kept, past every window above, and
	// a settled notice with it.
	alertMarkRetention = 8 * 24 * time.Hour
	// alertStreakRetention is how long a job nobody has run keeps its streak.
	alertStreakRetention = 30 * 24 * time.Hour
	// alertPruneEvery is how often what is past those is deleted.
	alertPruneEvery = time.Hour
	// alertDeliveriesAPass bounds what one look at a destination's queue sends, so a
	// stop is heard between them.
	alertDeliveriesAPass = 20
	// alertMaxAttempts is how many tries a notice gets. A failure to deliver is
	// reported, and never retried forever.
	alertMaxAttempts = 5
	// alertRetryFirst is the wait after a failed try, doubled after each.
	alertRetryFirst = 30 * time.Second
	// alertFailureBytes bounds what a failed notice records.
	alertFailureBytes = 300
)

// errAlertNotRead says a pass's share of GitHub's records has run out, or GitHub did
// not answer: the job is judged on a later pass, and nothing is decided about it.
var errAlertNotRead = errors.New("server: GitHub's record of the job was not read")

// alertSweep is what the evaluation remembers between passes.
type alertSweep struct {
	firstSeen map[string]time.Time
	judged    map[alloc.AlertMark]bool
	branches  map[rerunRun]string
	lastPrune time.Time
	// started is whether the ledger's rules and marks have been read, which a pass
	// retries until it can: judging before they are known would repeat every judgement.
	started bool
}

// alertRead is what one pass may still ask of GitHub.
type alertRead struct {
	spent   map[string]int
	budgets map[string][]RESTHeadroom
}

// evaluateAlerts judges jobs against the rules until ctx ends. It can stop nothing,
// and everything it decides is written before anything is sent.
func (s *Server) evaluateAlerts(ctx context.Context, targets map[string]Target) {
	every := alertEvaluateEvery
	if s.alertEvaluateEvery > 0 {
		every = s.alertEvaluateEvery
	}

	sweep := &alertSweep{judged: map[alloc.AlertMark]bool{}, branches: map[rerunRun]string{}}
	tick := time.NewTicker(every)
	defer tick.Stop()

	for {
		s.evaluateAlertsOnce(ctx, targets, sweep, time.Now())

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// evaluateAlertsOnce is one pass at now.
func (s *Server) evaluateAlertsOnce(ctx context.Context, targets map[string]Target, sweep *alertSweep, now time.Time) {
	if !s.startAlerts(ctx, sweep, now) {
		return
	}

	jobs, err := s.alloc.AlertJobs(ctx, now.Add(-alertRunningWindow), now.Add(-alertEndedWindow), alertJobsRead)
	if err != nil {
		s.log.Warn("could not list the jobs for alerts; trying again", "error", err)

		return
	}

	read := &alertRead{spent: map[string]int{}, budgets: installationBudgets(targets)}

	for r := range s.alerts.Rules {
		rule := &s.alerts.Rules[r]
		for _, condition := range rule.Conditions() {
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				s.judgeJob(ctx, rule, condition, &jobs[j], targets, sweep, read, now)
			}
		}
	}

	if now.Sub(sweep.lastPrune) >= alertPruneEvery {
		sweep.lastPrune = now
		if err := s.alloc.PruneAlerts(ctx, now.Add(-alertMarkRetention), now.Add(-alertStreakRetention)); err != nil {
			s.log.Warn("could not prune what alerts have finished with; trying again", "error", err)
		}
	}
}

// startAlerts reads what the ledger already holds, once, and says whether it has.
func (s *Server) startAlerts(ctx context.Context, sweep *alertSweep, now time.Time) bool {
	if sweep.started {
		return true
	}

	names := make([]string, 0, len(s.alerts.Rules))
	for i := range s.alerts.Rules {
		names = append(names, s.alerts.Rules[i].Name)
	}

	seen, err := s.alloc.EnsureAlertRules(ctx, names)
	if err != nil {
		s.log.Warn("could not record the alert rules; trying again", "error", err)

		return false
	}

	marks, err := s.alloc.AlertMarks(ctx, now.Add(-alertMarkRetention))
	if err != nil {
		s.log.Warn("could not read what the alert rules have judged; trying again", "error", err)

		return false
	}

	sweep.firstSeen = seen
	for _, mark := range marks {
		sweep.judged[mark] = true
	}
	sweep.started = true

	return true
}

// judgeJob takes one rule's one condition as far as it can go on one job.
func (s *Server) judgeJob(ctx context.Context, rule *alerts.Rule, condition alerts.Condition, row *alloc.AlertJob,
	targets map[string]Target, sweep *alertSweep, read *alertRead, now time.Time,
) {
	mark := alloc.AlertMark{Rule: rule.Name, LeaseID: row.LeaseID, Condition: string(condition)}
	if sweep.judged[mark] {
		return
	}

	// A RULE IS ABOUT JOBS FROM WHEN IT WAS FIRST SEEN, so adding one does not page
	// somebody about the last day's failures.
	if first, ok := sweep.firstSeen[rule.Name]; !ok || row.StartedAt.Before(first) {
		return
	}

	job := alertJob(row)
	if !rule.MatchesJob(&job) {
		return
	}

	verdict := rule.Judge(condition, &job, now)
	if verdict == alerts.Later {
		return
	}

	branch, outcome, err := s.narrowAlert(ctx, rule, condition, &job, verdict, targets, sweep, read)
	if err != nil {
		// COULD NOT TELL IS NOT A MISS. It waits, and only for so long: what cannot be
		// read in the window is recorded as unknown and said out loud, since a rule that
		// silently stops firing is the failure an alert exists to prevent.
		if now.Sub(alertJudgedFrom(&job)) <= alertReadWindow {
			return
		}
		outcome = alloc.AlertUnknown
		s.log.Warn("could not read what an alert rule needs from GitHub in time; this job is not judged, and "+
			"a rule naming a branch or a step needs the App to hold `actions: read`",
			"rule", rule.Name, "lease", job.LeaseID, "condition", condition, "error", err)
	}

	judgement := &alloc.AlertJudgement{Rule: rule.Name, LeaseID: job.LeaseID, Condition: string(condition), Key: job.Key(),
		Outcome: outcome, InARow: rule.InARow, Cooldown: rule.Cooldown}
	if outcome == alloc.AlertHit {
		judgement.Notices = s.renderAlert(rule, condition, &job, branch, now)
	}

	decision, err := s.alloc.RecordAlertJudgement(ctx, judgement)
	if err != nil {
		s.log.Warn("could not record an alert judgement; trying again", "rule", rule.Name, "lease", job.LeaseID, "error", err)

		return
	}

	sweep.judged[mark] = true

	if decision.Fired {
		s.log.Info("an alert rule fired", "rule", rule.Name, "condition", condition, "lease", job.LeaseID,
			"repository", job.Repository, "job", job.JobName, "destinations", len(judgement.Notices))
	}
}

// alertJudgedFrom is when a job's verdict became knowable, which the read window is
// counted from.
func alertJudgedFrom(j *alerts.Job) time.Time {
	if !j.ResultAt.IsZero() {
		return j.ResultAt
	}

	return j.StartedAt
}

// alertJob is a ledger row in a rule's terms.
func alertJob(row *alloc.AlertJob) alerts.Job {
	return alerts.Job{LeaseID: row.LeaseID, Tier: row.Tier, Node: row.Node, Repository: row.Repository,
		WorkflowRef: row.WorkflowRef, JobName: row.JobName, RunID: row.RunID, Result: row.Result,
		Disruption: row.Disruption, Rerun: row.Rerun, StartedAt: row.StartedAt, ResultAt: row.ResultAt,
		QueuedAt: row.QueuedAt}
}

// narrowAlert asks GitHub what a rule needs beyond the ledger, and says what the
// verdict comes to. A job on a branch the rule does not name is not its job at all,
// and a hit whose failed step the rule does not name is a miss; an error is
// could-not-tell. A rule asking for neither asks GitHub nothing.
func (s *Server) narrowAlert(ctx context.Context, rule *alerts.Rule, condition alerts.Condition, job *alerts.Job,
	verdict alerts.Verdict, targets map[string]Target, sweep *alertSweep, read *alertRead,
) (string, alloc.AlertOutcome, error) {
	outcome := alloc.AlertMiss
	if verdict == alerts.Hit {
		outcome = alloc.AlertHit
	}
	if !rule.NeedsBranch() && !rule.NeedsSteps() {
		return "", outcome, nil
	}

	target, known := targets[job.Tier]
	if !known {
		return "", "", fmt.Errorf("server: tier %s has no target this control plane serves", job.Tier)
	}

	owner, repository, ok := config.SplitRepository(job.Repository)
	if !ok {
		return "", "", fmt.Errorf("server: %q is not a repository", job.Repository)
	}

	branch := ""
	if rule.NeedsBranch() {
		var err error

		branch, err = s.alertBranch(ctx, target, owner, repository, job, sweep, read)
		if err != nil {
			return "", "", err
		}
		if !rule.MatchesBranch(branch) {
			return branch, alloc.AlertOther, nil
		}
	}

	if verdict == alerts.Hit && rule.NeedsSteps() {
		steps, err := s.alertSteps(ctx, target, owner, repository, job, read)
		if err != nil {
			return "", "", err
		}
		if !rule.MatchesSteps(steps, condition) {
			return branch, alloc.AlertMiss, nil
		}
	}

	return branch, outcome, nil
}

// spendAlertRead takes one of the pass's reads through a target's installation,
// leaving the reserve to what decides whether a runner starts.
func spendAlertRead(target Target, read *alertRead, now time.Time) bool {
	installation := installationOf(target)
	if read.spent[installation] >= alertRunsPerInstallation {
		return false
	}
	if _, _, _, low := inReserve(read.budgets[installation], now); low {
		return false
	}
	read.spent[installation]++

	return true
}

// alertBranch is the branch of a job's run, GitHub's own, kept since it cannot change.
func (s *Server) alertBranch(ctx context.Context, target Target, owner, repository string, job *alerts.Job,
	sweep *alertSweep, read *alertRead,
) (string, error) {
	run := rerunRun{repository: job.Repository, run: job.RunID}
	if branch, ok := sweep.branches[run]; ok {
		return branch, nil
	}

	evidence, ok := target.Provisioner.(RunEvidence)
	if !ok {
		return "", errors.New("server: this target cannot read a workflow run")
	}
	if !spendAlertRead(target, read, time.Now()) {
		return "", errAlertNotRead
	}

	record, err := evidence.WorkflowRun(ctx, owner, repository, job.RunID)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errAlertNotRead, err)
	}

	if len(sweep.branches) >= alertReadsRemembered {
		clear(sweep.branches)
	}
	sweep.branches[run] = record.HeadBranch

	return record.HeadBranch, nil
}

// alertSteps are the steps of a job, read by the runner's name, which is unique.
func (s *Server) alertSteps(ctx context.Context, target Target, owner, repository string, job *alerts.Job,
	read *alertRead,
) ([]alerts.Step, error) {
	reader, ok := target.Provisioner.(JobSteps)
	if !ok {
		return nil, errors.New("server: this target cannot read a job's steps")
	}
	if !spendAlertRead(target, read, time.Now()) {
		return nil, errAlertNotRead
	}

	steps, err := reader.RunnerJobSteps(ctx, owner, repository, job.RunID, provider.InstanceName(job.LeaseID))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errAlertNotRead, err)
	}

	return steps, nil
}

// renderAlert is what each of a rule's destinations is owed. A destination that
// cannot be rendered for is logged and left out: the others still hear.
func (s *Server) renderAlert(rule *alerts.Rule, condition alerts.Condition, job *alerts.Job, branch string,
	now time.Time,
) []alloc.AlertNotice {
	notice := alerts.NewNotice(rule, condition, job, branch, now)

	out := make([]alloc.AlertNotice, 0, len(rule.Destinations))
	for _, name := range rule.Destinations {
		destination, ok := s.alerts.Destinations[name]
		if !ok {
			s.log.Error("an alert rule names a destination this control plane does not have", "rule", rule.Name,
				"destination", name)

			continue
		}

		body, err := notice.Render(destination.Kind)
		if err != nil {
			s.log.Error("could not render an alert notice", "rule", rule.Name, "destination", name, "error", err)

			continue
		}
		out = append(out, alloc.AlertNotice{Destination: name, Payload: body})
	}

	return out
}

// deliverAlerts sends one destination what it is owed, until ctx ends.
//
// ONE OF THESE PER DESTINATION, each with one request in flight, so a destination that
// hangs delays only its own queue, by one request's bound at a time.
func (s *Server) deliverAlerts(ctx context.Context, name string, sender *alerts.Sender) {
	every := alertDeliverEvery
	if s.alertDeliverEvery > 0 {
		every = s.alertDeliverEvery
	}

	tick := time.NewTicker(every)
	defer tick.Stop()

	for {
		s.deliverAlertsOnce(ctx, name, sender, time.Now())

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// deliverAlertsOnce sends what is due at now.
func (s *Server) deliverAlertsOnce(ctx context.Context, name string, sender *alerts.Sender, now time.Time) {
	due, err := s.alloc.DueAlertNotices(ctx, name, now, alertDeliveriesAPass)
	if err != nil {
		s.log.Warn("could not read what a destination is owed; trying again", "destination", name, "error", err)

		return
	}

	first := alertRetryFirst
	if s.alertRetryFirst > 0 {
		first = s.alertRetryFirst
	}

	for i := range due {
		if ctx.Err() != nil {
			return
		}

		notice := &due[i]

		err := sender.Send(ctx, notice.Payload)

		switch {
		case err == nil:
			if err := s.alloc.SettleAlertNotice(ctx, notice.ID, true, ""); err != nil {
				// DELIVERED AND NOT RECORDED: the next pass sends it again, because a
				// duplicate is better than a notice that may have been lost.
				s.log.Warn("delivered an alert notice but could not record it; it may be sent twice",
					"destination", name, "rule", notice.Rule, "error", err)
			}
		case ctx.Err() != nil:
			return
		case errors.Is(err, alerts.ErrPermanent), notice.Attempts+1 >= alertMaxAttempts:
			s.log.Warn("gave up on an alert notice; nobody will hear of this job from this destination",
				"destination", name, "rule", notice.Rule, "lease", notice.LeaseID, "condition", notice.Condition,
				"attempts", notice.Attempts+1, "error", err)
			if err := s.alloc.SettleAlertNotice(ctx, notice.ID, false, alertFailureText(err)); err != nil {
				s.log.Warn("could not record a failed alert notice; trying again", "destination", name, "error", err)
			}
		default:
			wait := first << notice.Attempts
			if err := s.alloc.RetryAlertNotice(ctx, notice.ID, now.Add(wait), alertFailureText(err)); err != nil {
				s.log.Warn("could not record a failed try of an alert notice; trying again", "destination", name,
					"error", err)
			}
		}
	}
}

// alertFailureText is what a failed notice records. The sender's errors are built from
// fixed words and quote nothing of the address, so this bounds rather than scrubs.
func alertFailureText(err error) string {
	text := err.Error()
	if len(text) > alertFailureBytes {
		return text[:alertFailureBytes]
	}

	return text
}
