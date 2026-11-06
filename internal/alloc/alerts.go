package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// AlertJob is one job a rule of `server.alerts` may be judged against. An empty
// string or a zero time is not recorded.
type AlertJob struct {
	LeaseID     string
	Tier        string
	Node        string
	Repository  string
	WorkflowRef string
	JobName     string
	RunID       int64
	Result      string
	Disruption  string
	Rerun       string
	StartedAt   time.Time
	ResultAt    time.Time
	QueuedAt    time.Time
}

// AlertJobs reads the jobs a pass judges: those running that started since
// runningSince, and those whose result arrived since endedSince, each newest first
// and at most limit. Two reads rather than one, each served by its partial index,
// so neither scans a history that only grows.
func (a *Allocator) AlertJobs(ctx context.Context, runningSince, endedSince time.Time, limit int) ([]AlertJob, error) {
	var out []AlertJob

	err := a.db.View(ctx, func(q querier) error {
		reads := state.ReadQueries(q)

		running, err := reads.ListRunningJobsForAlerts(ctx, ledgerdb.ListRunningJobsForAlertsParams{
			Since:   sql.NullString{String: ts(runningSince.UTC()), Valid: true},
			MaxRows: int64(limit),
		})
		if err != nil {
			return fmt.Errorf("alloc: list the running jobs for alerts: %w", err)
		}

		ended, err := reads.ListEndedJobsForAlerts(ctx, ledgerdb.ListEndedJobsForAlertsParams{
			Since: ts(endedSince.UTC()), MaxRows: int64(limit),
		})
		if err != nil {
			return fmt.Errorf("alloc: list the ended jobs for alerts: %w", err)
		}

		out = make([]AlertJob, 0, len(running)+len(ended))
		for i := range running {
			r := &running[i]
			out = append(out, AlertJob{LeaseID: r.LeaseID, Tier: r.Tier, Node: r.Node, Repository: r.Repo,
				WorkflowRef: r.WorkflowRef, JobName: r.JobName, RunID: r.RunID, Result: r.Result, Disruption: r.Disruption,
				Rerun: r.RerunDecision, StartedAt: parseStamp(r.StartedAt), ResultAt: parseStamp(r.ResultAt),
				QueuedAt: parseStamp(r.GithubQueuedAt)})
		}
		for i := range ended {
			r := &ended[i]
			out = append(out, AlertJob{LeaseID: r.LeaseID, Tier: r.Tier, Node: r.Node, Repository: r.Repo,
				WorkflowRef: r.WorkflowRef, JobName: r.JobName, RunID: r.RunID, Result: r.Result, Disruption: r.Disruption,
				Rerun: r.RerunDecision, StartedAt: parseStamp(r.StartedAt), ResultAt: parseStamp(r.ResultAt),
				QueuedAt: parseStamp(r.GithubQueuedAt)})
		}

		return nil
	})

	return out, err
}

// EnsureAlertRules records when this deployment first saw each rule, and returns
// when that was, by name.
func (a *Allocator) EnsureAlertRules(ctx context.Context, names []string) (map[string]time.Time, error) {
	seen := make(map[string]time.Time, len(names))

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)
		at := ts(a.now().UTC())

		for _, name := range names {
			if err := q.InsertAlertRule(ctx, ledgerdb.InsertAlertRuleParams{Name: name, FirstSeenAt: at}); err != nil {
				return fmt.Errorf("alloc: record alert rule %s: %w", name, err)
			}
		}

		rows, err := q.ListAlertRules(ctx)
		if err != nil {
			return fmt.Errorf("alloc: read the alert rules: %w", err)
		}

		clear(seen)
		for i := range rows {
			seen[rows[i].Name] = parseStamp(rows[i].FirstSeenAt)
		}

		return nil
	})

	return seen, err
}

// AlertMark names one judgement already made.
type AlertMark struct {
	Rule, LeaseID, Condition string
}

// AlertMarks reads the judgements made at or after since.
func (a *Allocator) AlertMarks(ctx context.Context, since time.Time) ([]AlertMark, error) {
	var out []AlertMark

	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListAlertMarks(ctx, ts(since.UTC()))
		if err != nil {
			return fmt.Errorf("alloc: read the alert marks: %w", err)
		}

		out = make([]AlertMark, 0, len(rows))
		for i := range rows {
			out = append(out, AlertMark{Rule: rows[i].RuleName, LeaseID: rows[i].LeaseID, Condition: rows[i].AlertCondition})
		}

		return nil
	})

	return out, err
}

// AlertOutcome is what a judgement of one job comes to.
type AlertOutcome string

const (
	// AlertHit is a job the condition holds for, which counts towards the streak.
	AlertHit AlertOutcome = "hit"
	// AlertMiss is a job the condition does not hold for, which ends the streak.
	AlertMiss AlertOutcome = "miss"
	// AlertOther is a job the rule turned out not to be about, known only from GitHub,
	// which leaves the streak alone.
	AlertOther AlertOutcome = "other"
	// AlertUnknown is a job that could not be judged in time. It counts neither way:
	// could-not-tell is neither a hit nor a miss.
	AlertUnknown AlertOutcome = "unknown"
)

// AlertNotice is what one destination is owed if a judgement fires.
type AlertNotice struct {
	Destination string
	Payload     []byte
}

// AlertJudgement is one rule's verdict on one job on one condition.
type AlertJudgement struct {
	Rule, LeaseID, Condition string
	// Key is what the streak and the cooldown are counted by.
	Key      string
	Outcome  AlertOutcome
	InARow   int64
	Cooldown time.Duration
	Notices  []AlertNotice
}

// AlertDecision is what recording a judgement decided.
type AlertDecision struct {
	// Recorded is false for a job already judged, when nothing else changed.
	Recorded bool
	// Fired is a hit that reached the streak, past its cooldown and not muted, whose
	// notices are now owed.
	Fired bool
	// Muted is a hit that would have fired but for the rule's mute.
	Muted bool
	// Streak is the count after this judgement.
	Streak int64
}

// RecordAlertJudgement records one judgement, and fires it if it should.
//
// IN ONE TRANSACTION, and that is the design. The mark is what stops a second pass or
// a second controller counting a job twice, so the streak it moves and the notices it
// owes have to land with it or not at all: a crash after the mark would otherwise lose
// the notice for good, and one before it would repeat it. The clock is the ledger's
// writer's, read inside, so a cooldown is compared with the moment that decides it.
func (a *Allocator) RecordAlertJudgement(ctx context.Context, j *AlertJudgement) (AlertDecision, error) {
	var decision AlertDecision

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		decision = AlertDecision{}
		q := state.WriteQueries(tx)
		now := a.now().UTC()
		at := ts(now)

		marked, err := q.InsertAlertMark(ctx, ledgerdb.InsertAlertMarkParams{RuleName: j.Rule, LeaseID: j.LeaseID,
			AlertCondition: j.Condition, Outcome: string(j.Outcome), DecidedAt: at})
		if err != nil {
			return fmt.Errorf("alloc: mark lease %s judged by rule %s: %w", j.LeaseID, j.Rule, err)
		}
		if marked == 0 {
			return nil
		}
		decision.Recorded = true

		if j.Outcome != AlertHit && j.Outcome != AlertMiss {
			return nil
		}

		streak, err := q.ReadAlertStreak(ctx, ledgerdb.ReadAlertStreakParams{RuleName: j.Rule, JobKey: j.Key})
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("alloc: read rule %s's streak: %w", j.Rule, err)
		}

		count, firedAt, until := int64(0), "", ""
		if j.Outcome == AlertHit {
			count = streak.Streak + 1

			muted, err := a.alertRuleMuted(ctx, q, j.Rule, now)
			if err != nil {
				return err
			}

			if count >= j.InARow && !now.Before(parseStamp(streak.CooldownUntil)) {
				if muted {
					decision.Muted = true
				} else {
					decision.Fired = true
					firedAt, until = at, ts(now.Add(j.Cooldown))
				}
			}
		}
		decision.Streak = count

		if err := q.WriteAlertStreak(ctx, ledgerdb.WriteAlertStreakParams{RuleName: j.Rule, JobKey: j.Key,
			Streak: count, FiredAt: firedAt, CooldownUntil: until, UpdatedAt: at}); err != nil {
			return fmt.Errorf("alloc: write rule %s's streak: %w", j.Rule, err)
		}

		if !decision.Fired {
			return nil
		}

		for _, notice := range j.Notices {
			id, err := newLeaseID()
			if err != nil {
				return err
			}
			if err := q.InsertAlertNotice(ctx, ledgerdb.InsertAlertNoticeParams{ID: id, RuleName: j.Rule,
				Destination: notice.Destination, LeaseID: j.LeaseID, AlertCondition: j.Condition,
				Payload: string(notice.Payload), NextAt: at, CreatedAt: at}); err != nil {
				return fmt.Errorf("alloc: owe %s a notice from rule %s: %w", notice.Destination, j.Rule, err)
			}
		}

		return nil
	})

	return decision, err
}

// alertRuleMuted says whether a rule is muted at now. A rule with no row is not: the
// loop writes every rule's row before it judges anything.
func (a *Allocator) alertRuleMuted(ctx context.Context, q state.ReadOps, rule string, now time.Time) (bool, error) {
	until, err := q.ReadAlertRuleMute(ctx, rule)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("alloc: read rule %s's mute: %w", rule, err)
	}

	return until != "" && now.Before(parseStamp(until)), nil
}

// MuteAlertRule mutes a rule until a moment. It takes effect on the next decision,
// with the control plane running and across a failover, because the decision reads it.
func (a *Allocator) MuteAlertRule(ctx context.Context, rule string, until time.Time, by string) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := state.WriteQueries(tx).MuteAlertRule(ctx, ledgerdb.MuteAlertRuleParams{Name: rule,
			FirstSeenAt: ts(a.now().UTC()), MutedUntil: ts(until.UTC()), MutedBy: by}); err != nil {
			return fmt.Errorf("alloc: mute rule %s: %w", rule, err)
		}

		return nil
	})
}

// UnmuteAlertRule lifts a rule's mute, and says whether the ledger knew the rule.
func (a *Allocator) UnmuteAlertRule(ctx context.Context, rule string) (bool, error) {
	var known bool

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		lifted, err := state.WriteQueries(tx).UnmuteAlertRule(ctx, rule)
		if err != nil {
			return fmt.Errorf("alloc: unmute rule %s: %w", rule, err)
		}
		known = lifted > 0

		return nil
	})

	return known, err
}

// DueAlertNotice is one notice a destination is owed now.
type DueAlertNotice struct {
	ID, Rule, LeaseID, Condition string
	Payload                      []byte
	// Attempts is how many tries it has had.
	Attempts int64
}

// DueAlertNotices reads what one destination is owed at now, oldest first.
func (a *Allocator) DueAlertNotices(ctx context.Context, destination string, now time.Time, limit int,
) ([]DueAlertNotice, error) {
	var out []DueAlertNotice

	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListDueAlertNotices(ctx, ledgerdb.ListDueAlertNoticesParams{
			Destination: destination, Now: ts(now.UTC()), MaxRows: int64(limit),
		})
		if err != nil {
			return fmt.Errorf("alloc: list what %s is owed: %w", destination, err)
		}

		out = make([]DueAlertNotice, 0, len(rows))
		for i := range rows {
			r := &rows[i]
			out = append(out, DueAlertNotice{ID: r.ID, Rule: r.RuleName, LeaseID: r.LeaseID, Condition: r.AlertCondition,
				Payload: []byte(r.Payload), Attempts: r.Attempts})
		}

		return nil
	})

	return out, err
}

// RetryAlertNotice counts one failed try of a notice, and when to make the next.
func (a *Allocator) RetryAlertNotice(ctx context.Context, id string, next time.Time, failure string) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := state.WriteQueries(tx).RetryAlertNotice(ctx, ledgerdb.RetryAlertNoticeParams{ID: id,
			NextAt: ts(next.UTC()), LastError: failure}); err != nil {
			return fmt.Errorf("alloc: count a try of notice %s: %w", id, err)
		}

		return nil
	})
}

// The states of a notice.
const (
	AlertNoticePending   = "pending"
	AlertNoticeDelivered = "delivered"
	AlertNoticeFailed    = "failed"
)

// SettleAlertNotice ends a notice: delivered, or given up on and why.
func (a *Allocator) SettleAlertNotice(ctx context.Context, id string, delivered bool, failure string) error {
	settled := AlertNoticeFailed
	if delivered {
		settled = AlertNoticeDelivered
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := state.WriteQueries(tx).SettleAlertNotice(ctx, ledgerdb.SettleAlertNoticeParams{ID: id, State: settled,
			LastError: failure, SettledAt: ts(a.now().UTC())}); err != nil {
			return fmt.Errorf("alloc: settle notice %s: %w", id, err)
		}

		return nil
	})
}

// PruneAlerts forgets judgements and settled notices older than before, and streaks
// older than streaksBefore.
func (a *Allocator) PruneAlerts(ctx context.Context, before, streaksBefore time.Time) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		if err := q.PruneAlertMarks(ctx, ts(before.UTC())); err != nil {
			return fmt.Errorf("alloc: prune the alert marks: %w", err)
		}
		if err := q.PruneAlertNotices(ctx, ts(before.UTC())); err != nil {
			return fmt.Errorf("alloc: prune the alert notices: %w", err)
		}
		if err := q.PruneAlertStreaks(ctx, ts(streaksBefore.UTC())); err != nil {
			return fmt.Errorf("alloc: prune the alert streaks: %w", err)
		}

		return nil
	})
}

// AlertRuleState is one rule as `billet alerts list` shows it.
type AlertRuleState struct {
	Name       string
	FirstSeen  time.Time
	MutedUntil time.Time
	MutedBy    string
}

// AlertStreak is one job's count under one rule.
type AlertStreak struct {
	Rule          string
	JobKey        string
	Streak        int64
	LastFired     time.Time
	CooldownUntil time.Time
}

// AlertNoticeCount is how many notices a destination has in a state.
type AlertNoticeCount struct {
	Destination, State string
	Notices            int64
}

// AlertFailure is one notice given up on.
type AlertFailure struct {
	Rule, Destination, LeaseID, Condition, Failure string
	At                                             time.Time
}

// AlertReport is everything an operator reads about alerts, in one snapshot.
type AlertReport struct {
	Rules    []AlertRuleState
	Streaks  []AlertStreak
	Notices  []AlertNoticeCount
	Failures []AlertFailure
}

// AlertReport reads what `billet alerts list` shows, on the read-only pool.
func (a *Allocator) AlertReport(ctx context.Context, limit int) (AlertReport, error) {
	var out AlertReport

	err := a.db.View(ctx, func(q querier) error {
		reads := state.ReadQueries(q)
		out = AlertReport{}

		rules, err := reads.ListAlertRules(ctx)
		if err != nil {
			return fmt.Errorf("alloc: read the alert rules: %w", err)
		}
		for i := range rules {
			out.Rules = append(out.Rules, AlertRuleState{Name: rules[i].Name, FirstSeen: parseStamp(rules[i].FirstSeenAt),
				MutedUntil: parseStamp(rules[i].MutedUntil), MutedBy: rules[i].MutedBy})
		}

		streaks, err := reads.ListAlertStreaks(ctx, int64(limit))
		if err != nil {
			return fmt.Errorf("alloc: read the alert streaks: %w", err)
		}
		for i := range streaks {
			out.Streaks = append(out.Streaks, AlertStreak{Rule: streaks[i].RuleName, JobKey: streaks[i].JobKey,
				Streak: streaks[i].Streak, LastFired: parseStamp(streaks[i].LastFiredAt),
				CooldownUntil: parseStamp(streaks[i].CooldownUntil)})
		}

		out.Notices, err = alertNoticeCounts(ctx, reads)
		if err != nil {
			return err
		}

		failures, err := reads.ListFailedAlertNotices(ctx, int64(limit))
		if err != nil {
			return fmt.Errorf("alloc: read the failed notices: %w", err)
		}
		for i := range failures {
			out.Failures = append(out.Failures, AlertFailure{Rule: failures[i].RuleName, Destination: failures[i].Destination,
				LeaseID: failures[i].LeaseID, Condition: failures[i].AlertCondition, Failure: failures[i].LastError,
				At: parseStamp(failures[i].SettledAt)})
		}

		return nil
	})

	return out, err
}

// AlertNoticeCounts counts each destination's notices by state, for a metric.
func (a *Allocator) AlertNoticeCounts(ctx context.Context) ([]AlertNoticeCount, error) {
	var out []AlertNoticeCount

	err := a.db.View(ctx, func(q querier) error {
		var err error

		out, err = alertNoticeCounts(ctx, state.ReadQueries(q))

		return err
	})

	return out, err
}

func alertNoticeCounts(ctx context.Context, q state.ReadOps) ([]AlertNoticeCount, error) {
	rows, err := q.CountAlertNotices(ctx)
	if err != nil {
		return nil, fmt.Errorf("alloc: count the alert notices: %w", err)
	}

	out := make([]AlertNoticeCount, 0, len(rows))
	for i := range rows {
		out = append(out, AlertNoticeCount{Destination: rows[i].Destination, State: rows[i].State, Notices: rows[i].Notices})
	}

	return out, nil
}
