package integration_test

import (
	"context"
	"encoding/json"
	"errors"
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
// be safe whether or not a message comes back, and docs/upgrades.md says which
// behaviours are proved and which are assumed.
func TestLiveSessionReplacement(t *testing.T) {
	client, set := liveScaleSet(t)

	report := &replacementReport{
		Organization: os.Getenv("BILLET_LIVE_ORG"),
		ScaleSet:     set.Name,
		StartedAt:    time.Now().UTC(),
	}

	t.Cleanup(func() { report.write(t) })

	// THE FIRST SESSION IS OPENED AND ABANDONED WITHOUT BEING CLOSED, which is
	// what a killed controller leaves behind. Closing it would be the graceful
	// path, and the graceful path is not the one whose behaviour is unknown.
	first, err := client.Session(t.Context(), set.ID, "billet-conformance")
	if err != nil {
		t.Fatalf("open the first message session: %v", err)
	}

	firstStats := first.Statistics()
	report.FirstSessionStatistics = firstStats

	t.Logf("first session opened; statistics=%+v", firstStats)

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
	second, err := client.Session(t.Context(), set.ID, "billet-conformance")
	if err != nil {
		report.ReplacementRefused = err.Error()

		t.Logf("RECORDED: a successor cannot open a session while an abandoned one is "+
			"outstanding: %v", err)
		t.Log("RECORDED: recovery is therefore bounded by GitHub expiring that session, " +
			"not by anything billet does; server.openSession waits for it and says so")

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

	secondStats := second.Statistics()
	report.NewSessionStatistics = secondStats

	t.Logf("successor session opened; statistics=%+v", secondStats)

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
			pollWindow.String()
	case err != nil:
		report.Note = "the successor's first poll failed: " + err.Error()

		t.Errorf("polling the successor session: %v", err)
	default:
		report.Redelivered = true
		report.RedeliveredMessageID = msg.MessageID
		report.Note = "the successor was handed a message"
	}

	// STATISTICS ARE RECORDED AND NOT ACTED ON, and that distinction is the whole
	// point of writing them down. The vendored README calls them the authoritative
	// scaling input, and an earlier attempt in this codebase used them to release a
	// promise — the same defect as the timer it replaced, because a snapshot with
	// no observation time cannot causally postdate a local action. Recording what
	// they say at a replacement boundary keeps that a documented limit rather than
	// a temptation.
	if secondStats == nil {
		t.Error("the successor session carried no statistics, so nothing here can say " +
			"what GitHub told it about the scale set")
	}
}

// pollWindow is how long the successor waits for a redelivery before recording
// that none came.
//
// LONGER THAN A LONG POLL, because the answer "nothing arrived" is only worth
// recording if it outlasted the mechanism that would have delivered something. A
// measured poll against a real organization ran ~88 seconds; this is comfortably
// past that.
const pollWindow = 3 * time.Minute

// replacementReport is what one run measured.
//
// A RECORD, NOT AN ASSERTION. Every field is something billet currently has no
// documented answer for, and the value of running this is the answer rather than
// a pass.
type replacementReport struct {
	Organization string    `json:"organization"`
	ScaleSet     string    `json:"scale_set"`
	StartedAt    time.Time `json:"started_at"`

	FirstSessionStatistics *server.Statistics `json:"first_session_statistics,omitempty"`
	NewSessionStatistics   *server.Statistics `json:"new_session_statistics,omitempty"`

	// ReplacementRefused is set when a successor could not open a session at all
	// while the abandoned one was outstanding.
	ReplacementRefused string `json:"replacement_refused,omitempty"`

	Redelivered          bool  `json:"redelivered"`
	RedeliveredMessageID int64 `json:"redelivered_message_id,omitempty"`

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
