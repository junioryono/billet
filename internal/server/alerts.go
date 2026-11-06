package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/alerts"
	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// JobStep is one step of a job as GitHub records it: its name and how it ended,
// empty while it has not.
type JobStep struct {
	Name, Conclusion string
}

// RunnerSteps is what GitHub's record of one job says of its steps. Completed is
// whether GitHub has concluded the job, and with it every step; until it has, a
// step's conclusion says nothing yet.
type RunnerSteps struct {
	Completed bool
	Steps     []JobStep
}

// RunJobSteps reads every job of a run with its steps, keyed by the name of the
// runner that ran it, in one read. A target's Provisioner implements it over the
// App's REST client, and it needs `actions: read`. A runner the answer does not name
// is could-not-tell.
type RunJobSteps interface {
	RunJobSteps(ctx context.Context, owner, repository string, runID int64) (map[string]RunnerSteps, error)
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

// WithAlerts gives the control plane its alert rules and destinations. Rules start
// the evaluation, and each destination its delivery: a destination with no rule left
// still delivers what it is owed.
func WithAlerts(a *Alerts) ControlPlaneOption {
	return func(s *Server) {
		if a != nil && (len(a.Rules) > 0 || len(a.Destinations) > 0) {
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
	// alertRunningWindow is how long a running job is watched, and how recently an
	// ended job must have started to be judged. A job longer than this has outgrown
	// any rule that could still be useful about it.
	alertRunningWindow = 7 * 24 * time.Hour
	// alertEndedWindow is how far back an ended job is judged. A control plane down for
	// longer loses what ended meanwhile, which by then is nobody's news.
	alertEndedWindow = 6 * time.Hour
	// alertJobsRead bounds each of a pass's two reads. The ended read goes on from
	// where the last pass decided everything, so a backlog is reached in turn.
	alertJobsRead = 5000
	// alertSettledAfter is how old a result must be before the cursor passes it: a
	// result is stamped before its transaction commits, so one stamped earlier can
	// land after a later one was read.
	alertSettledAfter = time.Minute
	// alertReadWindow is how long a judgement waits for GitHub to answer what a rule
	// needs, from the first request actually made for it, after which it is judged
	// unknown and said so, rather than waiting on an App that lacks `actions: read`
	// until it falls out of the read unnoticed. A pass that had no read left for it
	// asked nothing, and costs it no time.
	alertReadWindow = time.Hour
	// alertRunsPerInstallation is how many of GitHub's records one pass reads through
	// one App installation, each cached for the pass. It decides nothing about a
	// runner, so it is as small as the queue-time sweep's share.
	alertRunsPerInstallation = 3
	// alertReadsRemembered bounds the branches kept between passes: a run's branch does
	// not change, and a rule about a branch asks of every run.
	alertReadsRemembered = 4096
	// alertMarkRetention is how long a judgement is kept: past the running window a
	// job is read in and the ended window after it, so a job is never read again once
	// its judgement is gone. A settled notice goes with it.
	alertMarkRetention = alertRunningWindow + alertEndedWindow + 24*time.Hour
	// alertStreakRetention is how long a job nobody has run keeps its streak, unless
	// a cooldown on it is still running.
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

var (
	// errAlertNoRead says the pass had no read of GitHub left for a job, or the
	// installation is down to its reserve: nothing was asked, and nothing is decided.
	errAlertNoRead = errors.New("server: no read of GitHub was left this pass")
	// errAlertNotRead says GitHub was asked and did not answer what a rule needs.
	errAlertNotRead = errors.New("server: GitHub's record of the job was not read")
	// errAlertUnfinished says GitHub's record of a job has not concluded it yet, so its
	// steps say nothing yet.
	errAlertUnfinished = errors.New("server: GitHub has not concluded the job's record yet")
)

// alertSweep is what the evaluation remembers between passes.
type alertSweep struct {
	firstSeen map[string]time.Time
	judged    map[alloc.AlertMark]bool
	branches  map[rerunRun]string
	// firstAsked is when GitHub was first asked what a judgement needs, which its
	// read window counts from.
	firstAsked map[alloc.AlertMark]time.Time
	// ended is how far the ended jobs have been decided.
	ended     alloc.AlertCursor
	lastPrune time.Time
	// started is whether the ledger's rules and marks have been read, which a pass
	// retries until it can: judging before they are known would repeat every judgement.
	started bool
}

func newAlertSweep() *alertSweep {
	return &alertSweep{judged: map[alloc.AlertMark]bool{}, branches: map[rerunRun]string{},
		firstAsked: map[alloc.AlertMark]time.Time{}}
}

// alertPass is what one pass may still ask of GitHub, what it has read, and which
// streaks it has put off.
type alertPass struct {
	spent   map[string]int
	budgets map[string][]RESTHeadroom
	steps   map[rerunRun]map[string]RunnerSteps
	failed  map[rerunRun]error
	// deferred is every streak a job could not be judged on this pass: a later job of
	// the same streak waits too, so a streak counts its jobs in the order they ran.
	deferred map[string]bool
}

// evaluateAlerts judges jobs against the rules until ctx ends. It can stop nothing,
// and everything it decides is written before anything is sent.
func (s *Server) evaluateAlerts(ctx context.Context, targets map[string]Target) {
	every := alertEvaluateEvery
	if s.alertEvaluateEvery > 0 {
		every = s.alertEvaluateEvery
	}

	sweep := newAlertSweep()
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
//
// THE ENDED JOBS OLDEST FIRST, then the running ones: a streak is counted in the order
// its jobs ran, so a success between two failures breaks it however the passes fell.
func (s *Server) evaluateAlertsOnce(ctx context.Context, targets map[string]Target, sweep *alertSweep, now time.Time) {
	if !s.startAlerts(ctx, sweep, now) {
		return
	}

	read, err := s.alloc.AlertJobs(ctx, now.Add(-alertRunningWindow), now.Add(-alertEndedWindow), sweep.ended,
		alertJobsRead)
	if err != nil {
		s.log.Warn("could not list the jobs for alerts; trying again", "error", err)

		return
	}

	pass := &alertPass{spent: map[string]int{}, budgets: installationBudgets(targets),
		steps: map[rerunRun]map[string]RunnerSteps{}, failed: map[rerunRun]error{}, deferred: map[string]bool{}}

	// THE CURSOR PASSES ONLY WHAT IS DECIDED, and only a result old enough that no
	// earlier one can still land: what this pass put off is read again on the next.
	decided := true
	for i := range read.Ended {
		if ctx.Err() != nil {
			return
		}

		settled := s.judgeAll(ctx, &read.Ended[i], targets, sweep, pass, now)
		if decided && settled && now.Sub(read.Ended[i].ResultAt) > alertSettledAfter {
			sweep.ended = read.Ended[i].Cursor
		} else {
			decided = false
		}
	}

	for i := range read.Running {
		if ctx.Err() != nil {
			return
		}

		s.judgeAll(ctx, &read.Running[i], targets, sweep, pass, now)
	}

	s.pruneAlerts(ctx, sweep, now)
}

// judgeAll takes every rule's every condition as far as it can go on one job, and
// says whether nothing about the job is left to decide.
func (s *Server) judgeAll(ctx context.Context, row *alloc.AlertJob, targets map[string]Target, sweep *alertSweep,
	pass *alertPass, now time.Time,
) bool {
	settled := true

	for r := range s.alerts.Rules {
		rule := &s.alerts.Rules[r]
		for _, condition := range rule.Conditions() {
			if !s.judgeJob(ctx, rule, condition, row, targets, sweep, pass, now) {
				settled = false
			}
		}
	}

	return settled
}

// pruneAlerts deletes what is past its retention, and forgets with it what the pass
// remembers of it, so neither the ledger nor the process grows with time.
func (s *Server) pruneAlerts(ctx context.Context, sweep *alertSweep, now time.Time) {
	if now.Sub(sweep.lastPrune) < alertPruneEvery {
		return
	}
	sweep.lastPrune = now

	if err := s.alloc.PruneAlerts(ctx, now.Add(-alertMarkRetention), now.Add(-alertStreakRetention)); err != nil {
		s.log.Warn("could not prune what alerts have finished with; trying again", "error", err)

		return
	}

	// THE MARKS ARE READ AGAIN rather than kept for the process's life: the ledger's are
	// the record, and this is a cache of them.
	marks, err := s.alloc.AlertMarks(ctx, now.Add(-alertMarkRetention))
	if err != nil {
		s.log.Warn("could not read what the alert rules have judged; keeping what is known", "error", err)

		return
	}

	clear(sweep.judged)
	for _, mark := range marks {
		sweep.judged[mark] = true
	}
	for mark, at := range sweep.firstAsked {
		if now.Sub(at) > 2*alertReadWindow {
			delete(sweep.firstAsked, mark)
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
	sweep.lastPrune = now
	sweep.started = true

	return true
}

// judgeJob takes one rule's one condition as far as it can go on one job, and says
// whether it is decided: judged now or before, or not this rule's.
func (s *Server) judgeJob(ctx context.Context, rule *alerts.Rule, condition alerts.Condition, row *alloc.AlertJob,
	targets map[string]Target, sweep *alertSweep, pass *alertPass, now time.Time,
) bool {
	mark := alloc.AlertMark{Rule: rule.Name, LeaseID: row.LeaseID, Condition: string(condition)}
	if sweep.judged[mark] {
		return true
	}

	// A RULE IS ABOUT JOBS FROM WHEN IT WAS FIRST SEEN, so adding one does not page
	// somebody about the last day's failures.
	if first, ok := sweep.firstSeen[rule.Name]; !ok || row.StartedAt.Before(first) {
		return true
	}

	job := alertJob(row)
	if !rule.MatchesJob(&job) {
		return true
	}

	// A STREAK COUNTS ITS JOBS IN ORDER: one put off this pass holds every later one.
	streak := rule.Name + "\x00" + job.Key(condition)
	if pass.deferred[streak] {
		return false
	}

	verdict := rule.Judge(condition, &job, now)
	if verdict == alerts.Later {
		// A WAIT GITHUB NEVER GAVE A QUEUE TIME FOR is judged neither way, and holds the
		// cursor only as long as an answer could still come.
		if condition == alerts.WaitedLong && !job.ResultAt.IsZero() && now.Sub(job.ResultAt) > alertReadWindow {
			return true
		}
		pass.deferred[streak] = true

		return false
	}

	branch, outcome, err := s.narrowAlert(ctx, rule, condition, &job, verdict, mark, targets, sweep, pass, now)
	if err != nil {
		// COULD NOT TELL IS NOT A MISS. It waits, and only for so long once GitHub has
		// been asked: what cannot be read in the window is recorded as unknown and said
		// out loud, since a rule that silently stops firing is the failure an alert
		// exists to prevent. A pass that asked nothing costs it no time.
		asked, ok := sweep.firstAsked[mark]
		if errors.Is(err, errAlertNoRead) || !ok || now.Sub(asked) <= alertReadWindow {
			pass.deferred[streak] = true

			return false
		}
		outcome = alloc.AlertUnknown
		s.log.Warn("GitHub did not answer what an alert rule needs within an hour of being asked; this job is "+
			"not judged, and a rule naming a branch or a step needs the App to hold `actions: read`",
			"rule", rule.Name, "lease", job.LeaseID, "condition", condition, "error", err)
	}

	judgement := &alloc.AlertJudgement{Rule: rule.Name, LeaseID: job.LeaseID, Condition: string(condition),
		Key: job.Key(condition), Outcome: outcome, InARow: rule.InARow, Cooldown: rule.Cooldown}
	if outcome == alloc.AlertHit {
		judgement.Notices = s.renderAlert(rule, condition, &job, branch, now)
	}

	decision, err := s.alloc.RecordAlertJudgement(ctx, judgement)
	if err != nil {
		s.log.Warn("could not record an alert judgement; trying again", "rule", rule.Name, "lease", job.LeaseID, "error", err)
		pass.deferred[streak] = true

		return false
	}

	sweep.judged[mark] = true
	delete(sweep.firstAsked, mark)

	if decision.Fired {
		s.log.Info("an alert rule fired", "rule", rule.Name, "condition", condition, "lease", job.LeaseID,
			"repository", job.Repository, "job", job.JobName, "destinations", len(judgement.Notices))
	}

	return true
}

// alertJob is a ledger row in a rule's terms.
func alertJob(row *alloc.AlertJob) alerts.Job {
	return alerts.Job{LeaseID: row.LeaseID, Tier: row.Tier, Node: row.Node, Repository: row.Repository,
		WorkflowRef: row.WorkflowRef, JobName: row.JobName, RunID: row.RunID, Result: row.Result,
		Disruption: row.Disruption, Rerun: row.Rerun, HeadBranch: row.HeadBranch, StartedAt: row.StartedAt,
		ResultAt: row.ResultAt, QueuedAt: row.QueuedAt}
}

// narrowAlert asks GitHub what a rule needs beyond the ledger, and says what the
// verdict comes to. A job on a branch the rule does not name is not its job at all,
// and a hit whose failed step the rule does not name is a miss; an error is
// could-not-tell. A rule asking for neither asks GitHub nothing.
func (s *Server) narrowAlert(ctx context.Context, rule *alerts.Rule, condition alerts.Condition, job *alerts.Job,
	verdict alerts.Verdict, mark alloc.AlertMark, targets map[string]Target, sweep *alertSweep, pass *alertPass,
	now time.Time,
) (string, alloc.AlertOutcome, error) {
	outcome := alloc.AlertMiss
	if verdict == alerts.Hit {
		outcome = alloc.AlertHit
	}
	if !rule.NeedsBranch() && !rule.NeedsSteps() {
		return job.HeadBranch, outcome, nil
	}

	target, known := targets[job.Tier]
	if !known {
		return "", "", fmt.Errorf("%w: tier %s has no target this control plane serves", errAlertNotRead, job.Tier)
	}

	owner, repository, ok := config.SplitRepository(job.Repository)
	if !ok {
		return "", "", fmt.Errorf("%w: %q is not a repository", errAlertNotRead, job.Repository)
	}

	// ASKED IS RECORDED BEFORE THE ASKING, so the window starts with the first request
	// and a pass with no read left starts nothing.
	ask := func() {
		if _, ok := sweep.firstAsked[mark]; !ok {
			sweep.firstAsked[mark] = now
		}
	}

	branch := job.HeadBranch
	if rule.NeedsBranch() {
		var err error

		branch, err = s.alertBranch(ctx, target, owner, repository, job, sweep, pass, ask)
		if err != nil {
			return "", "", err
		}
		if !rule.MatchesBranch(branch) {
			return branch, alloc.AlertOther, nil
		}
	}

	if verdict == alerts.Hit && rule.NeedsSteps() {
		steps, err := s.alertSteps(ctx, target, owner, repository, job, pass, ask)
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
// leaving the reserve to what decides whether a runner starts. A read is bounded at
// eight pages, as the queue-time sweep's is, against a reserve of a fifth of the
// installation's hour.
func spendAlertRead(target Target, pass *alertPass, now time.Time) bool {
	installation := installationOf(target)
	if pass.spent[installation] >= alertRunsPerInstallation {
		return false
	}
	if _, _, _, low := inReserve(pass.budgets[installation], now); low {
		return false
	}
	pass.spent[installation]++

	return true
}

// alertBranch is the branch of a job's run: the ledger's, which the queue-time sweep
// writes from GitHub, or GitHub's own where the ledger has none yet, kept since it
// cannot change.
func (s *Server) alertBranch(ctx context.Context, target Target, owner, repository string, job *alerts.Job,
	sweep *alertSweep, pass *alertPass, ask func(),
) (string, error) {
	if job.HeadBranch != "" {
		return job.HeadBranch, nil
	}

	run := rerunRun{repository: job.Repository, run: job.RunID}
	if branch, ok := sweep.branches[run]; ok {
		return branch, nil
	}

	evidence, ok := target.Provisioner.(RunEvidence)
	if !ok {
		return "", fmt.Errorf("%w: this target cannot read a workflow run", errAlertNotRead)
	}
	if !spendAlertRead(target, pass, time.Now()) {
		return "", errAlertNoRead
	}
	ask()

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

// alertSteps are the steps of a job, read by the runner's name, which is unique, from
// one read of its run that serves every job of the run this pass.
//
// A JOB GITHUB HAS NOT CONCLUDED is could-not-tell: a step still running has no
// conclusion yet, and reading that as a step that did not fail would be a miss
// nobody could take back.
func (s *Server) alertSteps(ctx context.Context, target Target, owner, repository string, job *alerts.Job,
	pass *alertPass, ask func(),
) ([]alerts.Step, error) {
	run := rerunRun{repository: job.Repository, run: job.RunID}

	jobs, read := pass.steps[run]
	if !read {
		if err, failed := pass.failed[run]; failed {
			ask()

			return nil, err
		}

		reader, ok := target.Provisioner.(RunJobSteps)
		if !ok {
			return nil, fmt.Errorf("%w: this target cannot read a job's steps", errAlertNotRead)
		}
		if !spendAlertRead(target, pass, time.Now()) {
			return nil, errAlertNoRead
		}
		ask()

		var err error

		jobs, err = reader.RunJobSteps(ctx, owner, repository, job.RunID)
		if err != nil {
			err = fmt.Errorf("%w: %w", errAlertNotRead, err)
			pass.failed[run] = err

			return nil, err
		}
		pass.steps[run] = jobs
	}

	ask()

	record, named := jobs[provider.InstanceName(job.LeaseID)]
	switch {
	case !named:
		return nil, fmt.Errorf("%w: run %d does not name the job's runner", errAlertNotRead, job.RunID)
	case !record.Completed:
		return nil, errAlertUnfinished
	}

	steps := make([]alerts.Step, 0, len(record.Steps))
	for _, step := range record.Steps {
		steps = append(steps, alerts.Step{Name: step.Name, Conclusion: step.Conclusion})
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

// settleRemovedDestinations gives up, once, on every notice owed to a destination the
// config no longer names, retrying until it can: no loop will ever try them, and a
// notice owed for ever would hold the pending count up for ever.
func (s *Server) settleRemovedDestinations(ctx context.Context) {
	var configured []string
	if s.alerts != nil {
		for name := range s.alerts.Destinations {
			configured = append(configured, name)
		}
	}
	slices.Sort(configured)

	every := alertDeliverEvery
	if s.alertDeliverEvery > 0 {
		every = s.alertDeliverEvery
	}

	tick := time.NewTicker(every)
	defer tick.Stop()

	for {
		failed, err := s.alloc.FailAlertNoticesOutside(ctx, configured,
			"its destination is no longer in server.alerts")
		if err == nil {
			if failed > 0 {
				s.log.Warn("gave up on alert notices owed to destinations the config no longer names",
					"notices", failed)
			}

			return
		}
		if ctx.Err() == nil {
			s.log.Warn("could not settle the notices of removed alert destinations; trying again", "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
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

// deliverAlertsOnce sends what is due at now. A try that fails is tried again a
// wait after it ended, on the clock moved on by how long this look has taken.
func (s *Server) deliverAlertsOnce(ctx context.Context, name string, sender *alerts.Sender, now time.Time) {
	began := time.Now()

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
			ended := now.Add(time.Since(began))
			if err := s.alloc.RetryAlertNotice(ctx, notice.ID, ended.Add(first<<notice.Attempts),
				alertFailureText(err)); err != nil {
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
