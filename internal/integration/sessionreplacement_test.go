package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/server"
)

// WHAT HAPPENS TO A MESSAGE WHEN A SESSION IS REPLACED, MEASURED.
//
// Replacing the controller is an ordinary operation, and every recovery claim in
// that design rests on one question billet cannot answer from its own code: when a
// control plane dies with a message unacknowledged and a NEW session is opened,
// does GitHub redeliver it? The vendored client's README says unacknowledged
// messages are redelivered — WITHIN a session. It says nothing about a session that
// was deleted and recreated, which is exactly what a controller restart produces.
//
// IT CANNOT BE SUBSTITUTED WITH A FAKE, and the reason is not pedantry.
// internal/server's crash-point suite proves what BILLET does across a restart,
// against billet's own Session interface — so it is perfectly capable of
// asserting a redelivery GitHub would never make. A fake cannot be evidence about
// somebody else's service.
//
// SO THIS RECORDS RATHER THAN ASSERTS, at every boundary where GitHub's answer
// is not documented. Its output is the finding; a green run against a fixed
// expectation would be asserting the very thing that is being measured. What it
// DOES assert is the mechanics every finding rests on: that the first session
// opens and is handed a message carrying work, that each probe's refusal is the
// session-held one rather than some other failure, and that statistics come
// back. A bounded wait that never opens is a finding too, not a failure.
//
// It needs a real organization and is skipped without one:
//
//	BILLET_LIVE_ORG=my-org \
//	BILLET_LIVE_APP_ID=123456 \
//	BILLET_LIVE_INSTALLATION_ID=7891011 \
//	BILLET_LIVE_APP_KEY=/path/to/private-key.pem \
//	BILLET_LIVE_SCALE_SET=billet-conformance \
//	BILLET_LIVE_REPORT_DIR=/tmp/billet-conformance \
//	go test ./internal/integration/ -run TestLiveSessionReplacement -v -count=1 -timeout 90m
//
// THE PROCESS TIMEOUT IS NOT DECORATION: the expiry window alone defaults to
// thirty minutes, and Go's default is ten.
//
// IT RAN FOUR TIMES ON 2026-09-04 and the answers are in ADR-006,
// upstream-references.md and the protocol skill: a successor is refused under
// either name, the session was still refused at 60 seconds every time and open at
// 91 or 92, and the unacknowledged message came back to the successor. NOTHING
// LEANS ON THAT REDELIVERY ANYWAY: every recovery path in billet is written to be
// safe whether or not a message comes back, which is what makes the answer a fact
// about GitHub rather than a dependency.
func TestLiveSessionReplacement(t *testing.T) {
	client, set := liveScaleSet(t)

	// THE OWNER IS THE HOST'S NAME IN PRODUCTION (`runServer` passes os.Hostname),
	// so the same owner twice is one machine restarting and two owners is a
	// failover to another host. Active-passive makes the second an ordinary
	// operation rather than a curiosity, so both are measured.
	owner := envOr("BILLET_LIVE_OWNER", "billet-conformance")
	successorOwner := envOr("BILLET_LIVE_SUCCESSOR_OWNER", "billet-conformance-successor")

	// TWO NAMES OR NO FAILOVER OBSERVATION. Pointed at one value by an override,
	// the second probe would measure the first case again under a heading that
	// says otherwise.
	if owner == successorOwner {
		t.Fatalf("BILLET_LIVE_OWNER and BILLET_LIVE_SUCCESSOR_OWNER are both %q, so the "+
			"failover probe would repeat the restart probe", owner)
	}

	report := &replacementReport{
		Organization:   os.Getenv("BILLET_LIVE_ORG"),
		ScaleSet:       set.Name,
		Owner:          owner,
		SuccessorOwner: successorOwner,
		StartedAt:      time.Now().UTC(),
	}

	t.Cleanup(func() { report.write(t) })

	// THE FIRST SESSION IS OPENED AND ABANDONED WITHOUT BEING CLOSED, which is
	// what a killed controller leaves behind. Closing it would be the graceful
	// path, and the graceful path is not the one whose behaviour is unknown.
	first, err := client.Session(t.Context(), set.ID, owner)
	if err != nil {
		t.Fatalf("open the first message session: %v", err)
	}

	report.FirstSessionStatistics = first.Statistics()

	t.Logf("first session opened as %q; statistics=%+v", owner, report.FirstSessionStatistics)

	// AN UNACKNOWLEDGED MESSAGE IS THE WHOLE SUBJECT, SO ONE HAS TO EXIST.
	//
	// A run with nothing queued would record "no redelivery" from a queue that
	// never held anything: a finding satisfied by something other than the thing
	// under test, which is the shape this repository keeps deleting. So the
	// message is REQUIRED, and its absence names the precondition rather than
	// passing. Queue it by dispatching a workflow whose `runs-on` is this scale
	// set's label and leaving the job queued; no runner may take it, because a
	// job that starts is one GitHub has already been acknowledged for.
	held, err := awaitMessage(t, first, messageWindow)
	if err != nil {
		t.Fatalf("the first session was handed no message carrying work within %s (%v). This "+
			"measurement needs a job QUEUED at label %q before it runs, and no runner serving it",
			messageWindow, err, set.Name)
	}

	report.HeldMessageID = held.MessageID
	report.HeldMessage = describeMessage(held)

	t.Logf("first session holds message %d (%s), deliberately NOT acknowledged",
		held.MessageID, report.HeldMessage)

	// ONE ORIGIN FOR EVERY "AFTER" IN THIS RECORD: the moment the session became
	// an abandoned one. Two clocks produced a lower bound measured from before
	// the wait it is quoted against.
	abandonedAt := time.Now()

	// A SESSION IS SINGLE-HOLDER, AND GITHUB DOES NOT LET A SUCCESSOR DISPLACE ONE.
	//
	// MEASURED, 2026-08-30, against a real organization: opening a second session
	// for a scale set whose first was abandoned answers
	//
	//	409 Conflict ... RunnerScaleSetSessionConflictException ...
	//	The runner scale set <name> already has an active session for owner <name>.
	//
	// A FINDING, NOT A FAILURE, which is what this file is for — and treating it as
	// a failure was itself the defect. The recovery path a restart depends on is
	// therefore not "take over" but "wait for the abandoned session to expire", and
	// billet's own session open now waits rather than failing to start: see
	// server.openSession, which existed as a bare error return until this test ran
	// and took a restarted control plane down with it.
	second, sameHeld, refusal := probeSession(t.Context(), client, set.ID, owner)

	switch {
	case refusal == nil:
		t.Log("RECORDED: a successor under the SAME owner opened at once")
	case sameHeld:
		report.SameOwnerRefused = refusal.Error()
		recordRefusal(report, abandonedAt)

		t.Logf("RECORDED: a successor under the SAME owner is refused while the abandoned "+
			"session is outstanding: %v", refusal)
	default:
		// COULD NOT TELL IS NOT A REFUSAL. An expired credential or a 500 recorded
		// as "the abandoned session is still outstanding" would manufacture both
		// this finding and the expiry that is measured from it.
		t.Fatalf("opening a successor answered something other than the session-held "+
			"refusal, so nothing below would be a measurement: %v", refusal)
	}

	// AND THE OTHER OWNER IS THE FAILOVER, which is a different question with the
	// same shape. A promoted standby is a different host, so it opens under a
	// different name; whether GitHub treats that as a second session for the same
	// scale set or as a fresh holder decides whether a promotion waits out the old
	// leader's session or takes over at once.
	//
	// IT IS ASKED WHATEVER THE FIRST PROBE ANSWERED. Owner-sensitivity is the thing
	// this probe exists to find, so skipping it when the same owner got in would
	// leave the failover question unanswered in precisely the case that would have
	// made it interesting.
	other, otherHeld, otherErr := probeSession(t.Context(), client, set.ID, successorOwner)

	switch {
	case otherErr == nil:
		report.OtherOwnerOpened = boolPtr(true)

		t.Log("RECORDED: a successor under a DIFFERENT owner opened while a session was " +
			"outstanding, so a failover to another host does not wait")

		if second == nil {
			second = other
		} else {
			closeSession(t, other, "the failover probe's session")
		}
	case otherHeld:
		report.OtherOwnerOpened = boolPtr(false)
		report.OtherOwnerRefused = otherErr.Error()

		t.Logf("RECORDED: a successor under a DIFFERENT owner is refused too: %v", otherErr)
	default:
		t.Fatalf("the failover probe answered something other than the session-held "+
			"refusal: %v", otherErr)
	}

	// WAITING IT OUT IS WHAT billet DOES, so how long that takes is the number an
	// operator needs and the one nothing here has ever measured. Bounded, because
	// a measurement that never ends is not one, and a bound reached is itself a
	// finding: it says the wait is longer than this.
	// UNDER THE ORIGINAL OWNER, which is the restart. The failover probe above
	// already established what a different name is told while the session is
	// held; carrying it through the wait as well would measure one case twice.
	if second == nil {
		second = awaitSession(t, client, set.ID, owner, report, abandonedAt)
	}

	if second == nil {
		return
	}

	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()),
			30*time.Second)
		defer cancel()

		// A LEFT-OPEN SUCCESSOR IS THE NEXT RUN'S ABANDONED SESSION, so a close
		// that failed is reported rather than logged past.
		if err := second.Close(closeCtx); err != nil {
			report.SuccessorCloseFailed = err.Error()

			t.Errorf("closing the successor session: %v", err)
		}
	})

	report.NewSessionStatistics = second.Statistics()

	t.Logf("successor session opened; statistics=%+v", report.NewSessionStatistics)

	// WHETHER THE ABANDONED SESSION'S MESSAGE COMES BACK is the question, and it
	// is asked for the whole window rather than once: ErrNoMessage ends ONE
	// server-side long poll, measured at about 88 seconds, and treating the first
	// one as the answer would record a redelivery that arrived a second later as
	// "no". A window that closes with nothing is a finding, not a failure: it
	// means a successor must not expect a redelivery, which is what every
	// recovery path in billet is written to survive.
	awaitRedelivery(t, second, held, report)

	// STATISTICS ARE RECORDED AND NOT ACTED ON, and that distinction is the whole
	// point of writing them down. The vendored README calls them the authoritative
	// scaling input, and an earlier attempt in this codebase used them to release a
	// promise — the same defect as the timer it replaced, because a snapshot with
	// no observation time cannot causally postdate a local action. Recording what
	// they say at a replacement boundary keeps that a documented limit rather than
	// a temptation.
	if report.NewSessionStatistics == nil {
		t.Error("the successor session carried no statistics, so nothing here can say " +
			"what GitHub told it about the scale set")
	}
}

// awaitRedelivery polls the successor for the whole window, looking for the id
// the abandoned session was holding.
//
// REDELIVERY MEANS THAT ID, not any message: a notification about something else
// recorded as a redelivery would answer a question nobody asked. Anything else
// that arrives is recorded by id, acknowledged so the queue can move on, and the
// window keeps running.
func awaitRedelivery(
	t *testing.T, session server.Session, held *server.Message, report *replacementReport,
) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), pollWindow)
	defer cancel()

	for {
		msg, err := session.GetMessage(ctx, 0, 1)

		switch {
		case err == nil && msg.MessageID == held.MessageID:
			report.Redelivered = boolPtr(true)
			report.SameMessage = boolPtr(true)
			report.RedeliveredMessageID = msg.MessageID
			report.RedeliveredMessage = describeMessage(msg)
			report.Note = "the successor was handed the message the abandoned session held"

			t.Logf("RECORDED: the successor was handed message %d (%s), which is the one the "+
				"abandoned session held", msg.MessageID, report.RedeliveredMessage)

			return
		case err == nil:
			report.OtherMessageIDs = append(report.OtherMessageIDs, msg.MessageID)

			t.Logf("the successor was handed message %d (%s), which is NOT the held %d; "+
				"acknowledging it and asking again", msg.MessageID, describeMessage(msg),
				held.MessageID)

			if ackErr := session.DeleteMessage(ctx, msg.MessageID); ackErr != nil {
				t.Errorf("acknowledging an unrelated message: %v", ackErr)

				return
			}
		case ctx.Err() != nil:
			report.Redelivered = boolPtr(false)
			report.SameMessage = boolPtr(false)
			report.Note = "no message carrying id " + strconv.FormatInt(held.MessageID, 10) +
				" reached the successor within " + pollWindow.String()

			t.Logf("RECORDED: %s", report.Note)

			return
		case !errors.Is(err, server.ErrNoMessage):
			report.Note = "the successor's poll failed: " + err.Error()

			t.Errorf("polling the successor session: %v", err)

			return
		}
	}
}

// closeSession closes a session this measurement opened and did not otherwise
// need, because what it leaves behind is the next run's abandoned session.
func closeSession(t *testing.T, session server.Session, what string) {
	t.Helper()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()

		if err := session.Close(ctx); err != nil {
			t.Errorf("closing %s: %v", what, err)
		}
	})
}

// recordRefusal writes the lower end of the bracket: a refusal actually observed
// at this moment, measured from the one origin every "after" in the record uses.
func recordRefusal(report *replacementReport, since time.Time) {
	held := time.Since(since).Truncate(time.Second)
	report.LastRefusedAfter = held.String()
	report.LastRefusedAfterSeconds = int(held.Seconds())
}

// awaitMessage polls until GitHub hands this session a message CARRYING WORK, or
// the window closes.
//
// ONE POLL IS NOT A MEASUREMENT: the service answers a long poll with
// ErrNoMessage whenever its window expires with nothing to say, and a job queued
// moments earlier can miss the first one. The pace exists only so a service that
// answers instantly cannot spin this loop.
//
// A MESSAGE WITH NO JOBS IN IT IS NOT THE ONE THIS MEASUREMENT IS ABOUT, and
// holding it unacknowledged would make every finding below a statement about an
// empty queue. Such a message is acknowledged and skipped, which loses nothing:
// what has to stay unacknowledged is the message carrying the job.
func awaitMessage(
	t *testing.T, session server.Session, window time.Duration,
) (*server.Message, error) {
	t.Helper()

	// THE WINDOW BOUNDS THE CALLS, NOT ONLY THE LOOP. A long poll that hangs is
	// exactly what a bound is for, and a deadline checked between requests lets
	// one request outlast every window this test advertises.
	ctx, cancel := context.WithTimeout(t.Context(), window)
	defer cancel()

	for {
		started := time.Now()

		msg, err := session.GetMessage(ctx, 0, 1)

		switch {
		case err == nil && jobsIn(msg) > 0:
			return msg, nil
		case err == nil:
			t.Logf("message %d carried no jobs (%s); acknowledging it and asking again",
				msg.MessageID, describeMessage(msg))

			if ackErr := session.DeleteMessage(ctx, msg.MessageID); ackErr != nil {
				return nil, ackErr
			}
		case ctx.Err() != nil:
			return nil, fmt.Errorf("no message carrying work within %s", window)
		case !errors.Is(err, server.ErrNoMessage):
			return nil, err
		}

		if elapsed := time.Since(started); elapsed < messagePace {
			timer := time.NewTimer(messagePace - elapsed)

			select {
			case <-ctx.Done():
				timer.Stop()

				return nil, fmt.Errorf("no message carrying work within %s", window)
			case <-timer.C:
			}
		}
	}
}

// jobsIn counts the work a message carries, of any kind.
func jobsIn(m *server.Message) int {
	return len(m.Available) + len(m.Assigned) + len(m.Started) + len(m.Completed)
}

// probeSession answers three ways, because a session open does.
//
// OPENED, HELD, OR COULD NOT TELL. Only the second is the refusal this
// measurement is about, and it is the only one production retries
// (server.openSession). Reading an expired credential or a 500 as "still held"
// would fabricate the conflict finding and the expiry measured from it alike.
func probeSession(
	ctx context.Context, client *scaleset.Client, id int, owner string,
) (server.Session, bool, error) {
	session, err := client.Session(ctx, id, owner)
	if err == nil {
		return session, false, nil
	}

	return nil, errors.Is(err, server.ErrSessionHeld), err
}

// boolPtr distinguishes a measured false from a step that never ran, which a
// bare bool in a JSON record cannot.
func boolPtr(v bool) *bool { return &v }

// awaitSession waits out the session an abandoned holder left behind, the way
// server.openSession does, and records how long that took.
//
// NIL RATHER THAN A FAILURE when the bound is reached: "longer than this" is a
// finding about GitHub, and a test that failed on it would be asserting a
// timeout nobody has documented.
func awaitSession(
	t *testing.T, client *scaleset.Client, id int, owner string, report *replacementReport,
	abandonedAt time.Time,
) server.Session {
	t.Helper()

	started := time.Now()

	// ONE DEADLINE FOR THE WHOLE WAIT, carried into every request. A window
	// enforced only between attempts is one a single slow request can outlast.
	ctx, cancel := context.WithTimeout(t.Context(), expiryWindow)
	defer cancel()

	// WHAT A REACHED BOUND ESTABLISHES IS THAT NOTHING OPENED, not that the
	// session was still held at it. The last refusal is the last moment anything
	// was observed, and the window may have closed long after it.
	outOfTime := func() {
		report.Note = "no successful opening was observed within " + expiryWindow.String() +
			"; the last confirmed refusal was at " + refusalOrNone(report)

		t.Logf("RECORDED: %s", report.Note)
	}

	// THE FIRST PROBE IS IMMEDIATE, so a session that expires inside the first
	// pace still leaves a bracket with both ends: the refusal recorded here is
	// what "still refused at" names, and sleeping first would leave it empty.
	for attempt := 1; ; attempt++ {
		session, refusedHeld, err := probeSession(ctx, client, id, owner)

		switch {
		case err == nil:
			// AN UPPER OBSERVATION AND A LOWER ONE, NEVER AN EXPIRY. What this run
			// establishes is that the session was still refused at the last refusal
			// and open at this attempt; the moment between is not observed, and a
			// single number would read as one. The lower end rounds DOWN and the
			// upper end UP, because a bracket that rounds inwards claims a second at
			// which nothing was observed.
			took := ceilSeconds(time.Since(abandonedAt))
			report.OpenedAfter = took.String()
			report.OpenedAfterSeconds = int(took.Seconds())

			t.Logf("RECORDED: the abandoned session was still refused at %s and open at %s "+
				"(attempt %d)", report.LastRefusedAfter, report.OpenedAfter, attempt)

			return session
		case refusedHeld:
			recordRefusal(report, abandonedAt)

			t.Logf("still refused at %s (attempt %d); waiting", report.LastRefusedAfter, attempt)
		case ctx.Err() != nil:
			outOfTime()

			return nil
		default:
			t.Errorf("waiting out the abandoned session answered something other than the "+
				"session-held refusal: %v", err)

			return nil
		}

		remaining := time.Until(started.Add(expiryWindow))
		if remaining <= 0 {
			outOfTime()

			return nil
		}

		timer := time.NewTimer(min(sessionPace, remaining))

		select {
		case <-ctx.Done():
			timer.Stop()
			outOfTime()

			return nil
		case <-timer.C:
		}
	}
}

// refusalOrNone names the last observed refusal, or says there was none, which a
// bare empty string in a sentence would not.
func refusalOrNone(report *replacementReport) string {
	if report.LastRefusedAfter == "" {
		return "no point at all"
	}

	return report.LastRefusedAfter
}

// ceilSeconds rounds a duration UP to the second, so an upper observation names
// a moment the thing had already happened by.
func ceilSeconds(d time.Duration) time.Duration {
	if truncated := d.Truncate(time.Second); truncated != d {
		return truncated + time.Second
	}

	return d
}

// describeMessage says what a message carried, for a record a person reads.
func describeMessage(m *server.Message) string {
	return fmt.Sprintf("available=%d assigned=%d started=%d completed=%d",
		len(m.Available), len(m.Assigned), len(m.Started), len(m.Completed))
}

// envOr is a default this measurement can be pointed away from.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}

	return fallback
}

// pollWindow is how long the successor waits for a redelivery before recording
// that none came.
//
// LONGER THAN A LONG POLL, because the answer "nothing arrived" is only worth
// recording if it outlasted the mechanism that would have delivered something. A
// measured poll against a real organization ran ~88 seconds; this is comfortably
// past that.
const pollWindow = 3 * time.Minute

// messageWindow is how long the first session waits for the queued job's message
// before this run gives up, and messagePace bounds how often it asks.
const (
	messageWindow = 4 * time.Minute
	messagePace   = 5 * time.Second
)

// expiryWindow bounds the wait for GitHub to expire an abandoned session, and
// sessionPace is how often it is asked — the same pace server.openSession uses,
// because what is being waited for is not made sooner by asking more often.
var (
	expiryWindow = durationOr("BILLET_LIVE_EXPIRY_WINDOW", 30*time.Minute)
	sessionPace  = durationOr("BILLET_LIVE_SESSION_PACE", 30*time.Second)
)

// durationOr reads a duration from the environment, so a run that only wants the
// refusals recorded need not sit through the expiry. A value that does not parse
// or is not positive is the fallback rather than a window of zero.
func durationOr(name string, fallback time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}

	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}

	return d
}

// replacementReport is what one run measured.
//
// A RECORD, NOT AN ASSERTION. Each field is something GitHub's own documentation
// does not answer, and the value of a run is the answer rather than the pass.
// The 2026-09-04 run's answers are in ADR-006 and upstream-references.md; a later
// run that disagrees with them is the reason this stays a record.
type replacementReport struct {
	Organization   string    `json:"organization"`
	ScaleSet       string    `json:"scale_set"`
	Owner          string    `json:"owner"`
	SuccessorOwner string    `json:"successor_owner"`
	StartedAt      time.Time `json:"started_at"`

	FirstSessionStatistics *server.Statistics `json:"first_session_statistics,omitempty"`
	NewSessionStatistics   *server.Statistics `json:"new_session_statistics,omitempty"`

	// HeldMessageID is the message the abandoned session was holding
	// unacknowledged. Without one, everything below is about an empty queue.
	HeldMessageID int64  `json:"held_message_id,omitempty"`
	HeldMessage   string `json:"held_message,omitempty"`

	// SameOwnerRefused and OtherOwnerRefused are what GitHub said to a successor
	// opening under the abandoned holder's own name and under another host's.
	SameOwnerRefused  string `json:"same_owner_refused,omitempty"`
	OtherOwnerRefused string `json:"other_owner_refused,omitempty"`
	OtherOwnerOpened  *bool  `json:"other_owner_opened,omitempty"`

	// LastRefusedAfter and OpenedAfter BRACKET the expiry rather than naming it:
	// the session was still held at the first and open at the second, and nothing
	// observes the moment between.
	LastRefusedAfter        string `json:"last_refused_after,omitempty"`
	LastRefusedAfterSeconds int    `json:"last_refused_after_seconds,omitempty"`
	OpenedAfter             string `json:"opened_after,omitempty"`
	OpenedAfterSeconds      int    `json:"opened_after_seconds,omitempty"`

	// POINTERS, so a step that never ran is absent rather than false. A bare
	// bool records "could not tell" as "no", which is the collapse this file
	// exists to avoid.
	Redelivered          *bool  `json:"redelivered,omitempty"`
	RedeliveredMessageID int64  `json:"redelivered_message_id,omitempty"`
	RedeliveredMessage   string `json:"redelivered_message,omitempty"`
	// SameMessage says whether what came back carried the id that was held. The
	// payload is not compared; the shape beside it is what the record shows.
	SameMessage *bool `json:"same_message,omitempty"`
	// SuccessorCloseFailed is set when this run left a session behind.
	SuccessorCloseFailed string `json:"successor_close_failed,omitempty"`
	// OtherMessageIDs are messages the successor was handed that were not the one
	// the abandoned session held.
	OtherMessageIDs []int64 `json:"other_message_ids,omitempty"`

	Note string `json:"note,omitempty"`
}

// write puts the findings where a person can read them.
//
// A FILE RATHER THAN ONLY LOG LINES, because the point of this test is the record
// it produces: the next person deciding whether a recovery path may assume a
// redelivery needs the measurement, not a green tick in a CI run they cannot see.
func (r *replacementReport) write(t *testing.T) {
	t.Helper()

	dir := os.Getenv("BILLET_LIVE_REPORT_DIR")
	if dir == "" {
		dir = t.TempDir()
	}

	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Errorf("render the conformance report: %v", err)

		return
	}

	path := filepath.Join(dir, "session-replacement.json")
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		t.Errorf("write the conformance report: %v", err)

		return
	}

	t.Logf("session-replacement findings written to %s", path)
}

// liveScaleSet builds a real scale-set client, or skips.
//
// ASSEMBLED THE WAY cmd/billet DOES IT, from the same scaleset.Config fields, so
// what this test exercises is the client billet actually runs rather than a
// second arrangement that happens to authenticate.
func liveScaleSet(t *testing.T) (*scaleset.Client, *scaleset.ScaleSet) {
	t.Helper()

	org := os.Getenv("BILLET_LIVE_ORG")
	if org == "" {
		t.Skip("BILLET_LIVE_ORG is unset; this test needs a real GitHub organization " +
			"and is opt-in")
	}

	keyPath := required(t, "BILLET_LIVE_APP_KEY")
	name := required(t, "BILLET_LIVE_SCALE_SET")

	appID, err := strconv.ParseInt(required(t, "BILLET_LIVE_APP_ID"), 10, 64)
	if err != nil {
		t.Fatalf("BILLET_LIVE_APP_ID: %v", err)
	}

	installationID, err := strconv.ParseInt(required(t, "BILLET_LIVE_INSTALLATION_ID"), 10, 64)
	if err != nil {
		t.Fatalf("BILLET_LIVE_INSTALLATION_ID: %v", err)
	}

	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read the App private key: %v", err)
	}

	client, err := scaleset.New(scaleset.Config{
		ConfigURL:      "https://github.com/" + org,
		ClientID:       strconv.FormatInt(appID, 10),
		InstallationID: installationID,
		PrivateKey:     string(key),
		Org:            org,
		AppID:          appID,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("build the scale-set client: %v", err)
	}

	// ENSURED RATHER THAN LOOKED UP, so a first run against a fresh organization
	// works. It is the same call the control plane makes, which is what keeps this
	// measuring billet's own path rather than an arrangement invented here.
	set, err := client.EnsureScaleSet(t.Context(), name, "", []string{name})
	if err != nil {
		t.Fatalf("ensure the conformance scale set: %v", err)
	}

	return client, set
}

func required(t *testing.T, name string) string {
	t.Helper()

	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is required once BILLET_LIVE_ORG is set", name)
	}

	return v
}
