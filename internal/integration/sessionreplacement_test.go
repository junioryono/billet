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
// SO THIS RECORDS RATHER THAN ASSERTS, at every boundary where billet has no
// documented answer. Its output is the finding; a green run against a fixed
// expectation would be asserting the very thing that is unknown. What it DOES
// assert is that the sessions open, that a replacement succeeds, and that
// statistics come back — the mechanics the measurement depends on, which failing
// silently would make every recorded finding meaningless.
//
// It needs a real organization and is skipped without one:
//
//	BILLET_LIVE_ORG=my-org \
//	BILLET_LIVE_APP_ID=123456 \
//	BILLET_LIVE_INSTALLATION_ID=7891011 \
//	BILLET_LIVE_APP_KEY=/path/to/private-key.pem \
//	BILLET_LIVE_SCALE_SET=billet-conformance \
//	BILLET_LIVE_REPORT_DIR=/tmp/billet-conformance \
//	go test ./internal/integration/ -run TestLiveSessionReplacement -v -count=1
//
// UNTIL IT RUNS, NOTHING MAY LEAN ON CROSS-SESSION REPLAY. That is a constraint on
// the code rather than on this file: every recovery path in billet is written to
// be safe whether or not a message comes back, and docs/operating/upgrades.md says
// which
// behaviours are proved and which are assumed.
func TestLiveSessionReplacement(t *testing.T) {
	client, set := liveScaleSet(t)

	// THE OWNER IS THE HOST'S NAME IN PRODUCTION (`runServer` passes os.Hostname),
	// so the same owner twice is one machine restarting and two owners is a
	// failover to another host. Active-passive makes the second an ordinary
	// operation rather than a curiosity, so both are measured.
	owner := envOr("BILLET_LIVE_OWNER", "billet-conformance")
	successorOwner := envOr("BILLET_LIVE_SUCCESSOR_OWNER", "billet-conformance-successor")

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
	held, err := awaitMessage(t.Context(), first, messageWindow)
	if err != nil {
		t.Fatalf("the first session was handed no message within %s (%v). This measurement "+
			"needs a job QUEUED at label %q before it runs, and no runner serving it",
			messageWindow, err, set.Name)
	}

	report.HeldMessageID = held.MessageID
	report.HeldMessage = describeMessage(held)

	t.Logf("first session holds message %d (%s), deliberately NOT acknowledged",
		held.MessageID, report.HeldMessage)

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
	second, refusal := client.Session(t.Context(), set.ID, owner)
	if refusal != nil {
		report.SameOwnerRefused = refusal.Error()

		t.Logf("RECORDED: a successor under the SAME owner is refused while the abandoned "+
			"session is outstanding: %v", refusal)
	}

	// AND THE OTHER OWNER IS THE FAILOVER, which is a different question with the
	// same shape. A promoted standby is a different host, so it opens under a
	// different name; whether GitHub treats that as a second session for the same
	// scale set or as a fresh holder decides whether a promotion waits out the old
	// leader's session or takes over at once.
	if refusal != nil {
		other, otherErr := client.Session(t.Context(), set.ID, successorOwner)

		switch {
		case otherErr != nil:
			report.OtherOwnerRefused = otherErr.Error()

			t.Logf("RECORDED: a successor under a DIFFERENT owner is refused too: %v", otherErr)
		default:
			report.OtherOwnerOpened = true
			second = other

			t.Log("RECORDED: a successor under a DIFFERENT owner opened while the abandoned " +
				"session was outstanding, so a failover to another host does not wait")
		}
	}

	// WAITING IT OUT IS WHAT billet DOES, so how long that takes is the number an
	// operator needs and the one nothing here has ever measured. Bounded, because
	// a measurement that never ends is not one, and a bound reached is itself a
	// finding: it says the wait is longer than this.
	if second == nil {
		second = awaitSession(t, client, set.ID, owner, report)
	}

	if second == nil {
		return
	}

	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()),
			30*time.Second)
		defer cancel()

		if err := second.Close(closeCtx); err != nil {
			t.Logf("closing the successor session: %v", err)
		}
	})

	report.NewSessionStatistics = second.Statistics()

	t.Logf("successor session opened; statistics=%+v", report.NewSessionStatistics)

	// WHETHER THE ABANDONED SESSION'S MESSAGE COMES BACK is the question. A poll
	// that times out with nothing is a finding, not a failure: it means a
	// successor must not expect a redelivery, which is precisely what every
	// recovery path in billet is written to survive.
	pollCtx, cancel := context.WithTimeout(t.Context(), pollWindow)
	defer cancel()

	msg, err := second.GetMessage(pollCtx, 0, 1)

	switch {
	case errors.Is(err, server.ErrNoMessage):
		report.Redelivered = false
		report.Note = "the successor's first poll returned no message within " +
			pollWindow.String() + ", with message " +
			strconv.FormatInt(held.MessageID, 10) + " left unacknowledged by the abandoned session"
	case err != nil:
		report.Note = "the successor's first poll failed: " + err.Error()

		t.Errorf("polling the successor session: %v", err)
	default:
		report.Redelivered = true
		report.RedeliveredMessageID = msg.MessageID
		report.RedeliveredMessage = describeMessage(msg)
		report.SameMessage = msg.MessageID == held.MessageID
		report.Note = "the successor was handed a message"

		t.Logf("RECORDED: the successor was handed message %d (%s); the abandoned session "+
			"held %d", msg.MessageID, report.RedeliveredMessage, held.MessageID)
	}

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

// awaitMessage polls until GitHub hands this session a message, or the window
// closes.
//
// ONE POLL IS NOT A MEASUREMENT: the service answers a long poll with
// ErrNoMessage whenever its window expires with nothing to say, and a job queued
// moments earlier can miss the first one. The pace exists only so a service that
// answers instantly cannot spin this loop.
func awaitMessage(
	ctx context.Context, session server.Session, window time.Duration,
) (*server.Message, error) {
	deadline := time.Now().Add(window)

	for {
		started := time.Now()

		msg, err := session.GetMessage(ctx, 0, 1)
		if err == nil {
			return msg, nil
		}

		if !errors.Is(err, server.ErrNoMessage) || time.Now().After(deadline) {
			return nil, err
		}

		if elapsed := time.Since(started); elapsed < messagePace {
			timer := time.NewTimer(messagePace - elapsed)

			select {
			case <-ctx.Done():
				timer.Stop()

				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
}

// awaitSession waits out the session an abandoned holder left behind, the way
// server.openSession does, and records how long that took.
//
// NIL RATHER THAN A FAILURE when the bound is reached: "longer than this" is a
// finding about GitHub, and a test that failed on it would be asserting a
// timeout nobody has documented.
func awaitSession(
	t *testing.T, client *scaleset.Client, id int, owner string, report *replacementReport,
) server.Session {
	t.Helper()

	started := time.Now()
	deadline := started.Add(expiryWindow)

	for attempt := 1; ; attempt++ {
		timer := time.NewTimer(sessionPace)

		select {
		case <-t.Context().Done():
			timer.Stop()

			return nil
		case <-timer.C:
		}

		session, err := client.Session(t.Context(), id, owner)
		if err == nil {
			took := time.Since(started)
			report.ExpiredAfter = took.Round(time.Second).String()
			report.ExpiredAfterSeconds = int(took.Round(time.Second).Seconds())

			t.Logf("RECORDED: the abandoned session was gone after %s (attempt %d)",
				report.ExpiredAfter, attempt)

			return session
		}

		if time.Now().After(deadline) {
			report.ExpiredAfter = "not within " + expiryWindow.String()
			report.Note = "the abandoned session was still outstanding after " +
				expiryWindow.String() + "; the last refusal was: " + err.Error()

			t.Logf("RECORDED: still refused after %s: %v", expiryWindow, err)

			return nil
		}

		t.Logf("still refused after %s (attempt %d); waiting",
			time.Since(started).Round(time.Second), attempt)
	}
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
// refusals recorded need not sit through the expiry.
func durationOr(name string, fallback time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return fallback
	}

	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}

	return d
}

// replacementReport is what one run measured.
//
// A RECORD, NOT AN ASSERTION. Every field is something billet currently has no
// documented answer for, and the value of running this is the answer rather than
// a pass.
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
	OtherOwnerOpened  bool   `json:"other_owner_opened"`

	// ExpiredAfter is how long GitHub took to let a successor in.
	ExpiredAfter        string `json:"expired_after,omitempty"`
	ExpiredAfterSeconds int    `json:"expired_after_seconds,omitempty"`

	Redelivered          bool   `json:"redelivered"`
	RedeliveredMessageID int64  `json:"redelivered_message_id,omitempty"`
	RedeliveredMessage   string `json:"redelivered_message,omitempty"`
	// SameMessage says whether what came back is the message that was held.
	SameMessage bool `json:"same_message"`

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
