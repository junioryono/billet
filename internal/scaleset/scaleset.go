// Package scaleset is the ONLY place billet imports GitHub's scale-set client.
//
// It exists to keep a public-preview dependency at arm's length. The upstream
// module's own README says its interfaces and examples may change, and a preview
// type reaching into the scheduler is how someone else's release note turns into
// a rewrite here. A depguard rule in .golangci.yml enforces the confinement, the
// same way one confines the SQLite driver to internal/state.
//
// Everything in here is translation: vendor types in, billet types out. There is
// no policy — the escrow-before-advertise decision lives in internal/server,
// where it can be tested without a GitHub organization to point at.
package scaleset

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	gh "github.com/actions/scaleset"

	billetgithub "github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/version"
)

// clientVersion is reported to GitHub as part of the client's system info, so a
// deployment showing up in their telemetry is identifiable as billet.
//
// A FUNCTION, NOT A CONST, and that is the whole point of the change. It was
// `const Version = "0.0.0-dev"`, which nothing else in this repository reads —
// the only observer is GitHub's telemetry, so every release would have reported
// itself as a development build and nothing here would ever have noticed.
func clientVersion() string { return version.Version() }

// systemInfo is what billet tells GitHub about itself on every poll.
//
// ASSEMBLED IN ONE PLACE SO A TEST CAN SEE IT. Built inline in New, the only
// assertion available was that clientVersion() agreed with version.Version() —
// which stays true while the struct literal in New says something else entirely.
// The value that reaches GitHub is the one worth testing.
func systemInfo() gh.SystemInfo {
	return gh.SystemInfo{
		System:    "billet",
		Version:   clientVersion(),
		Subsystem: "listener",
	}
}

// Config is what billet needs to reach one organization's scale sets.
type Config struct {
	// ConfigURL is the organization URL, e.g. https://github.com/acme.
	ConfigURL string
	// ClientID is the App's client id. The App's numeric id is also accepted —
	// GitHubAppAuth documents it as "the Client ID of the application (app id
	// also works)" — which is why billet's client_id config field is optional.
	ClientID string
	// InstallationID is the installation the App was installed as.
	InstallationID int64
	// PrivateKey is the App private key, PEM encoded.
	PrivateKey string
	// Org and AppID authenticate policy reads against GitHub's organization API.
	Org   string
	AppID int64
	// APIURL overrides GitHub.com's REST base for GitHub Enterprise Server.
	APIURL string
}

// Client is billet's handle on the scale-set API.
type Client struct {
	gh     *gh.Client
	policy billetgithub.RunnerGroupPolicyClient
	log    *slog.Logger

	// openMessageSession is the vendored client's session call, as a field so a
	// test can drive Session itself rather than the recognition underneath it.
	//
	// THE SEAM IS AT THE DEPENDENCY BOUNDARY, which is the only place it can be.
	// What billet owns here is one decision — whether a failure is "another control
	// plane still holds this session", which must be waited out, or anything else,
	// which must be reported at once — and a test that calls isSessionConflict
	// directly leaves that decision untested: deleting the check from Session would
	// keep every such test green while a restarted control plane went back to
	// failing to start. Faking GitHub instead would mean standing up App JWT
	// exchange and config discovery to exercise one branch of billet's own code.
	openMessageSession func(context.Context, int, string) (*gh.MessageSessionClient, error)
}

// New builds a client from GitHub App credentials.
func New(cfg Config, log *slog.Logger) (*Client, error) {
	if log == nil {
		log = slog.Default()
	}

	// THE VENDORED CLIENT GETS A QUIETENED LOGGER, not billet's own. It hands this
	// to a retryable HTTP transport that reports every failed request at Error,
	// and a shutdown cancels the long poll in flight — so an ordinary restart
	// would end with an ERROR line every time. See quieten.
	quiet := slog.New(quieten(log.Handler()))
	httpClient := newValidatedRetryableHTTPClient()

	c, err := gh.NewClientWithGitHubApp(gh.ClientWithGitHubAppConfig{
		GitHubConfigURL: cfg.ConfigURL,
		GitHubAppAuth: gh.GitHubAppAuth{
			ClientID:       cfg.ClientID,
			InstallationID: cfg.InstallationID,
			PrivateKey:     cfg.PrivateKey,
		},
		SystemInfo: systemInfo(),
	}, gh.WithRetryableHTTPClint(httpClient), gh.WithLogger(quiet))
	if err != nil {
		// NOT wrapped with the config: GitHubAppAuth holds the private key, and a
		// validation error that renders the struct it validated would put it in
		// the log. The message alone says which field is wrong.
		return nil, fmt.Errorf("scaleset: build client: %w", err)
	}

	// ONE SPELLING OF THE DEFAULT, now that the constructor holds it. This branch
	// was the workaround for a base taken verbatim — the other caller,
	// cmd/billet's `check`, did not have it and its runner-group verification
	// failed on every real deployment for want of a scheme.
	var policy billetgithub.RunnerGroupPolicyClient
	if cfg.Org != "" && cfg.AppID > 0 {
		policy = billetgithub.NewRunnerGroupPolicyClientAt(cfg.APIURL, cfg.Org, cfg.AppID,
			cfg.InstallationID, []byte(cfg.PrivateKey))
	}

	client := &Client{gh: c, policy: policy, log: log}
	client.openMessageSession = func(ctx context.Context, id int, owner string) (
		*gh.MessageSessionClient, error,
	) {
		return c.MessageSessionClient(ctx, id, owner)
	}

	return client, nil
}

// session adapts *gh.MessageSessionClient to server.Session.
type session struct {
	gh *gh.MessageSessionClient
}

// Session opens a message session for one scale set and adapts it to the
// interface internal/server consumes.
func (c *Client) Session(ctx context.Context, scaleSetID int, owner string) (server.Session, error) {
	s, err := c.openMessageSession(ctx, scaleSetID, owner)
	if err != nil {
		if isSessionConflict(err) {
			return nil, fmt.Errorf("%w: scale set %d: %w",
				server.ErrSessionHeld, scaleSetID, err)
		}

		return nil, fmt.Errorf("scaleset: open session for scale set %d: %w", scaleSetID, err)
	}

	return &session{gh: s}, nil
}

// sessionConflictStatus is the STRUCTURED status fragment the upstream client
// writes, quoted exactly as its formatter emits it.
//
// THE QUOTES ARE THE POINT. newRequestResponseError builds `status=%q` and then
// APPENDS THE RESPONSE BODY, so a bare search for `409 Conflict` can be satisfied
// by text GitHub put in the body of some entirely different failure — which would
// send a real fault into an unbounded retry and hide it forever. The quoted form
// is produced by the formatter rather than by the server, so matching it asks
// about the status of THIS response.
const (
	sessionConflictStatus    = `status="409 Conflict"`
	sessionConflictException = "RunnerScaleSetSessionConflictException"
)

// isSessionConflict reports whether GitHub refused because a session for this
// scale set and owner is already outstanding.
//
// MEASURED AGAINST THE REAL API, not read: a live conformance run against a real
// organization produced `status="409 Conflict" ...
// RunnerScaleSetSessionConflictException ... already has an active session for
// owner`, which is what a control plane meets when it restarts before the session
// its predecessor abandoned has expired.
//
// MATCHED ON TEXT BECAUSE UPSTREAM OFFERS NOTHING ELSE. github.com/actions/scaleset
// carries sentinels for AgentExists, AgentNotFound and JobStillRunning and NOT for
// this one, so a session conflict falls through to a generic wrapper and the only
// thing that survives is the formatted string. Both fragments are required, and
// the status is matched in its structured form, so an unrelated failure that
// merely quotes these words in its body is not read as this — the cost of that
// mistake is a control plane waiting forever on a fault it should have reported.
func isSessionConflict(err error) bool {
	if err == nil {
		return false
	}

	text := err.Error()

	return strings.Contains(text, sessionConflictStatus) &&
		strings.Contains(text, sessionConflictException)
}

// Statistics reports what GitHub said about the scale set when the session
// opened.
//
// This is the ONLY view of a backlog that predates the session. A restart does
// not replay individual messages for work already assigned, so a listener that
// waits for messages to tell it what is outstanding will sit idle in front of a
// queue.
func (s *session) Statistics() *server.Statistics {
	stats := s.gh.Session().Statistics
	if stats == nil {
		return nil
	}

	return &server.Statistics{
		TotalAvailableJobs:     stats.TotalAvailableJobs,
		TotalAcquiredJobs:      stats.TotalAcquiredJobs,
		TotalAssignedJobs:      stats.TotalAssignedJobs,
		TotalRunningJobs:       stats.TotalRunningJobs,
		TotalRegisteredRunners: stats.TotalRegisteredRunners,
		TotalBusyRunners:       stats.TotalBusyRunners,
		TotalIdleRunners:       stats.TotalIdleRunners,
	}
}

// GetMessage long-polls, advertising maxCapacity.
//
// The vendor takes message ids as `int` while billet carries them as int64. The
// conversion is safe on every platform billet targets and is done in one place
// rather than leaking the vendor's width into the scheduler's types.
func (s *session) GetMessage(ctx context.Context, lastMessageID int64, maxCapacity int) (*server.Message, error) {
	msg, err := s.gh.GetMessage(ctx, int(lastMessageID), maxCapacity)
	if err != nil {
		return nil, fmt.Errorf("scaleset: get message: %w", err)
	}

	// The upstream client reports a timed-out long poll as (nil, nil). That is
	// translated here rather than propagated: a nil message beside a nil error
	// is indistinguishable from a silent failure, and every caller above would
	// have to remember which one it is holding.
	if msg == nil {
		return nil, server.ErrNoMessage
	}

	return translate(msg), nil
}

func (s *session) DeleteMessage(ctx context.Context, messageID int64) error {
	if err := s.gh.DeleteMessage(ctx, int(messageID)); err != nil {
		return fmt.Errorf("scaleset: delete message %d: %w", messageID, err)
	}

	return nil
}

func (s *session) AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error) {
	acquired, err := s.gh.AcquireJobs(ctx, requestIDs)
	if err != nil {
		return nil, fmt.Errorf("scaleset: acquire jobs: %w", err)
	}

	return acquired, nil
}

func (s *session) Close(ctx context.Context) error {
	if err := s.gh.Close(ctx); err != nil {
		return fmt.Errorf("scaleset: close session: %w", err)
	}

	return nil
}

// translate converts one vendor message into billet's own.
//
// JobAvailable is NOT dropped. Available is the offer — the message whose
// RunnerRequestID goes to AcquireJobs, which is how a scale set claims work.
// Assigned is the later confirmation that a claim succeeded, so acquiring from
// Assigned asks GitHub to claim work it has already handed over, and drops every
// offer on the floor.
func translate(msg *gh.RunnerScaleSetMessage) *server.Message {
	out := &server.Message{MessageID: int64(msg.MessageID)}

	if msg.Statistics != nil {
		out.Statistics = &server.Statistics{
			TotalAvailableJobs:     msg.Statistics.TotalAvailableJobs,
			TotalAcquiredJobs:      msg.Statistics.TotalAcquiredJobs,
			TotalAssignedJobs:      msg.Statistics.TotalAssignedJobs,
			TotalRunningJobs:       msg.Statistics.TotalRunningJobs,
			TotalRegisteredRunners: msg.Statistics.TotalRegisteredRunners,
			TotalBusyRunners:       msg.Statistics.TotalBusyRunners,
			TotalIdleRunners:       msg.Statistics.TotalIdleRunners,
		}
	}

	for _, j := range msg.JobAvailableMessages {
		if j == nil {
			continue
		}

		out.Available = append(out.Available, server.Job{
			RequestID:   j.RunnerRequestID,
			RunID:       j.WorkflowRunID,
			JobID:       j.JobID,
			Event:       j.EventName,
			Owner:       j.OwnerName,
			Repository:  j.RepositoryName,
			WorkflowRef: j.JobWorkflowRef,
		})
	}

	for _, j := range msg.JobAssignedMessages {
		if j == nil {
			continue
		}

		out.Assigned = append(out.Assigned, server.Job{
			RequestID:   j.RunnerRequestID,
			RunID:       j.WorkflowRunID,
			JobID:       j.JobID,
			Event:       j.EventName,
			Owner:       j.OwnerName,
			Repository:  j.RepositoryName,
			WorkflowRef: j.JobWorkflowRef,
		})
	}

	for _, j := range msg.JobStartedMessages {
		if j == nil {
			continue
		}

		out.Started = append(out.Started, server.Job{
			RequestID:   j.RunnerRequestID,
			RunID:       j.WorkflowRunID,
			JobID:       j.JobID,
			RunnerID:    int64(j.RunnerID),
			RunnerName:  j.RunnerName,
			Event:       j.EventName,
			Owner:       j.OwnerName,
			Repository:  j.RepositoryName,
			WorkflowRef: j.JobWorkflowRef,
		})
	}

	for _, j := range msg.JobCompletedMessages {
		if j == nil {
			continue
		}

		out.Completed = append(out.Completed, server.Job{
			RequestID:   j.RunnerRequestID,
			RunID:       j.WorkflowRunID,
			JobID:       j.JobID,
			Event:       j.EventName,
			Owner:       j.OwnerName,
			Repository:  j.RepositoryName,
			WorkflowRef: j.JobWorkflowRef,
			Result:      j.Result,
			RunnerID:    int64(j.RunnerID),
			RunnerName:  j.RunnerName,
		})
	}

	return out
}
