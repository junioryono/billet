// Package tokenbroker lets a control plane take installation tokens from a process
// that holds the App's key, so that a deployment holds none.
//
// An operator running many deployments under one GitHub App would otherwise put the
// one key on every control plane, and a compromise of any of them could mint tokens
// for every installation of the App. The broker holds the key and mints only what a
// deployment's own registration names, scoped to what that deployment needs. It never
// hands out the key, and has no route that could.
package tokenbroker

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/regularfile"
	"github.com/junioryono/billet/internal/wirecert"
)

// tokenRoute is the broker's one route.
const tokenRoute = "/v1/installation-token"

// tokenRequest is what a control plane asks for. It says nothing about who is asking,
// which the broker reads from the handshake and from nowhere a caller writes.
type tokenRequest struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	Target         string `json:"target"`
}

// tokenResponse is a minted token, and which deployment the broker minted it for, so
// a host registered under the wrong deployment finds out from its first answer.
type tokenResponse struct {
	Deployment  string            `json:"deployment"`
	Token       string            `json:"token"`
	ExpiresAt   string            `json:"expires_at"`
	Permissions map[string]string `json:"permissions,omitempty"`
}

// The closed set of refusals a caller branches on. Each has one status, and a message
// carries nothing the broker read from GitHub or from another deployment.
const (
	codeUnknownClient = "unknown-client"
	codeNotRegistered = "not-registered"
	codeNotInstalled  = "not-installed"
	codeMismatch      = "installation-mismatch"
	codeUnavailable   = "unavailable"
	codeBadRequest    = "bad-request"
)

type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// maxRequest bounds what is read of a request: three small fields.
const maxRequest = 4 << 10

// mintFresh is how much life a token the broker hands out has left, so that a caller
// holding one has a long while to come back during a broker outage.
const mintFresh = 10 * time.Minute

// verifyEvery is how long one proof that an installation is the registered target's
// stands. Minting needs the check only rarely, and GitHub's own refusal covers the rest.
const verifyEvery = time.Hour

const (
	// proofBound is the longest one proof may take, well inside what a client waits for an
	// answer: a held token must reach its caller while GitHub is stalled.
	proofBound = 10 * time.Second
	// proofRetry is how long an overdue proof that GitHub could not answer waits before
	// being asked again, so that a stall is not paid for by every request.
	proofRetry = 5 * time.Minute
)

// installation is one registered (deployment, app, installation, target).
type installation struct {
	// gate lets one request at a time through the failure memory, the proof, the mint and
	// the publishing of how that went. Without it a burst passes the memory together and
	// each asks GitHub, and a success can land beside a failure it then answers with.
	gate chan struct{}

	// failed and failedUntil are the failure this installation is answering with, under gate.
	failed      failure
	failedUntil time.Time

	deployment     string
	appID          int64
	installationID int64
	target         github.Target
	actions        github.ActionsAccess
	key            github.AppKey
	base           string

	tokens github.TokenSource

	mu       sync.Mutex
	verified time.Time
	// proofRetryAt is when an overdue proof GitHub could not answer is next asked for.
	proofRetryAt time.Time
}

// Server is the broker's handler. It holds the keys and the registry, and is what
// serves behind a listener that requires a client certificate in the handshake.
type Server struct {
	clients       map[string]string
	installations map[installationKey]*installation
	log           *slog.Logger
	now           func() time.Time
	proofBound    time.Duration
	// verify proves an installation is the registered target's, a field so a test
	// counts it; production is github.VerifyAppAt.
	verify func(r *http.Request, inst *installation) error
}

type installationKey struct {
	deployment     string
	appID          int64
	installationID int64
	target         string
}

// NewServer builds the broker from its config, reading every App key once with the
// hardened reader a control plane's would have gone through.
func NewServer(cfg *config.BrokerConfig, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}

	type held struct {
		key  github.AppKey
		base string
	}

	apps := make(map[int64]held, len(cfg.Apps))

	for i := range cfg.Apps {
		key, err := github.ReadPrivateKeyFile(cfg.Apps[i].PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("app %d: %w", cfg.Apps[i].AppID, err)
		}

		apps[cfg.Apps[i].AppID] = held{key: key, base: cfg.Apps[i].APIURL}
	}

	s := &Server{
		clients:       map[string]string{},
		installations: map[installationKey]*installation{},
		log:           log,
		now:           time.Now,
		proofBound:    proofBound,
		verify:        verifyInstallation,
	}

	for i := range cfg.Deployments {
		d := &cfg.Deployments[i]

		for _, client := range d.Clients {
			s.clients[client] = d.ID
		}

		for j := range d.Installations {
			reg := &d.Installations[j]

			target, err := targetOf(reg.Target)
			if err != nil {
				return nil, err
			}

			app, ok := apps[reg.AppID]
			if !ok {
				return nil, fmt.Errorf("deployment %s names app %d, which this broker does not hold", d.ID, reg.AppID)
			}

			s.installations[keyOf(d.ID, reg.AppID, reg.InstallationID, reg.Target)] = &installation{
				gate:       make(chan struct{}, 1),
				deployment: d.ID, appID: reg.AppID, installationID: reg.InstallationID, target: target,
				actions: actionsOf(reg), key: app.key, base: app.base,
				tokens: github.NewAppKeyTokensFor(app.base, reg.AppID, reg.InstallationID, app.key,
					github.ScopeFor(target, actionsOf(reg)), mintFresh),
			}
		}
	}

	return s, nil
}

// actionsOf is how much of `actions` a registration grants its tokens: write includes
// read.
func actionsOf(reg *config.BrokerInstallation) github.ActionsAccess {
	switch {
	case reg.ActionsWrite:
		return github.ActionsWrite
	case reg.ActionsRead:
		return github.ActionsRead
	default:
		return github.ActionsNone
	}
}

func keyOf(deployment string, appID, installationID int64, target string) installationKey {
	return installationKey{deployment, appID, installationID, strings.ToLower(target)}
}

func targetOf(path string) (github.Target, error) {
	if !strings.Contains(path, "/") {
		return github.OrganizationTarget(path), nil
	}

	owner, name, ok := config.SplitRepository(path)
	if !ok {
		return github.Target{}, fmt.Errorf("target %q is neither `owner` nor `owner/name`", path)
	}

	return github.RepositoryTarget(owner, name), nil
}

// VerifyConnection refuses a key this broker does not know in the handshake, on a
// resumed connection as on a fresh one.
//
// THE CONNECTION BUDGET IS CHARGED ONLY AFTER A HANDSHAKE SUCCEEDS, and any self-signed
// certificate satisfies RequireAnyClientCert, so checking this in the handler alone would
// let a stranger hold every place a registered deployment needs.
func (s *Server) VerifyConnection(state tls.ConnectionState) error {
	if len(state.PeerCertificates) == 0 {
		return errors.New("tokenbroker: no client certificate")
	}

	fingerprint := wirecert.FingerprintOfCert(state.PeerCertificates[0])
	if _, known := s.clients[fingerprint]; !known {
		s.log.Warn("refused a client this broker does not know", "fingerprint", fingerprint)

		return fmt.Errorf("tokenbroker: this broker does not know the key %s", fingerprint)
	}

	return nil
}

// ServeHTTP answers the one route.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != tokenRoute || r.Method != http.MethodPost {
		s.refuse(w, http.StatusNotFound, codeBadRequest, "this broker serves one route")

		return
	}

	// WHO IS ASKING COMES FROM THE HANDSHAKE AND FROM NOWHERE A CALLER WRITES. The
	// listener requires a certificate and verifies no chain, because a key is what is
	// registered: the handshake proved the caller holds it.
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		s.refuse(w, http.StatusUnauthorized, codeUnknownClient, "present the client certificate this broker knows")

		return
	}

	fingerprint := wirecert.FingerprintOfCert(r.TLS.PeerCertificates[0])

	deployment, known := s.clients[fingerprint]
	if !known {
		s.log.Warn("a client this broker does not know asked for a token", "fingerprint", fingerprint)
		s.refuse(w, http.StatusUnauthorized, codeUnknownClient,
			"this broker does not know the key "+fingerprint+"; register it with the deployment it proves")

		return
	}

	var req tokenRequest

	dec := json.NewDecoder(io.LimitReader(r.Body, maxRequest))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&req); err != nil {
		s.refuse(w, http.StatusBadRequest, codeBadRequest, "the request is not one this broker reads")

		return
	}

	// THE REFUSAL THIS BROKER EXISTS FOR. It says the same whether the installation
	// is another deployment's or nobody's, so a caller learns nothing about who else
	// this broker serves.
	inst, registered := s.installations[keyOf(deployment, req.AppID, req.InstallationID, req.Target)]
	if !registered {
		s.log.Warn("a deployment asked for a token for an installation not registered to it",
			"deployment", deployment, "app", req.AppID, "installation", req.InstallationID, "target", req.Target)
		s.refuse(w, http.StatusForbidden, codeNotRegistered,
			"this installation and target are not registered to this deployment")

		return
	}

	token, failed := s.tokenFor(r, inst)
	if failed.status != 0 {
		s.refuse(w, failed.status, failed.code, failed.message)

		return
	}

	s.log.Info("served an installation token", "deployment", deployment, "app", req.AppID,
		"installation", req.InstallationID, "expires", token.ExpiresAt().Format(time.RFC3339))

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	if err := json.NewEncoder(w).Encode(tokenResponse{
		Deployment: deployment, Token: token.Bearer(), ExpiresAt: token.ExpiresAt().Format(time.RFC3339),
		Permissions: maps.Clone(token.Permissions()),
	}); err != nil {
		s.log.Warn("could not write a token to its caller", "deployment", deployment, "error", err)
	}
}

// tokenFor is the token this registration holds, or why there is none.
//
// A FAILURE IS ANSWERED ONCE AND THEN REMEMBERED BRIEFLY. Nothing else stands between one
// deployment's retries and the App's budget at GitHub, which every deployment on that App
// shares.
func (s *Server) tokenFor(r *http.Request, inst *installation) (github.Token, failure) {
	select {
	case inst.gate <- struct{}{}:
		defer func() { <-inst.gate }()
	case <-r.Context().Done():
		return github.Token{}, failure{http.StatusServiceUnavailable, codeUnavailable,
			"a token could not be minted just now"}
	}

	if inst.failedUntil.After(s.now()) {
		return github.Token{}, inst.failed
	}

	if status, code, message := s.proved(r, inst); status != 0 {
		failed := failure{status, code, message}
		s.noteFailure(r, inst, failed)

		return github.Token{}, failed
	}

	token, err := inst.tokens.InstallationToken(r.Context())
	if err != nil {
		s.log.Error("could not mint an installation token", "deployment", inst.deployment, "app", inst.appID,
			"installation", inst.installationID, "error", err)

		failed := mintFailure(err)
		if failed.status != http.StatusServiceUnavailable {
			s.forgetProof(inst)
		}

		s.noteFailure(r, inst, failed)

		return github.Token{}, failed
	}

	inst.failedUntil = time.Time{}

	return token, failure{}
}

// failureMemory is how long a broker repeats a failure rather than asking GitHub again.
// Short, because the next answer may be different and a control plane's refresh waits
// on it; a held token rides this out.
const failureMemory = 30 * time.Second

// failure is a refusal as a caller reads it.
type failure struct {
	status        int
	code, message string
}

// noteFailure remembers one, unless the caller going away is what caused it: that says
// nothing about the installation.
func (s *Server) noteFailure(r *http.Request, inst *installation, failed failure) {
	if r.Context().Err() != nil {
		return
	}

	inst.failed, inst.failedUntil = failed, s.now().Add(failureMemory)
}

func (s *Server) forgetProof(inst *installation) {
	inst.mu.Lock()
	defer inst.mu.Unlock()

	inst.verified = time.Time{}
}

// mintFailure reads why GitHub would not mint. ONLY GITHUB'S OWN DEFINITE WORD IS A
// REFUSAL: its 5xx, a throttle and a failure to reach it are could-not-tell, and so is
// anything that is not GitHub's answer at all.
func mintFailure(err error) failure {
	api, answered := errors.AsType[*github.APIError](err)

	switch {
	case !answered, github.Undecided(err):
		return failure{http.StatusServiceUnavailable, codeUnavailable, "a token could not be minted just now"}
	case api.Status == http.StatusNotFound:
		return failure{http.StatusNotFound, codeNotInstalled, "the App is not installed there"}
	default:
		return failure{http.StatusForbidden, codeMismatch, "GitHub refused this App a token for that installation"}
	}
}

// proved checks that an installation really is the registered target's before a
// token is minted for it, at most once per verifyEvery. A registry names ids an
// operator typed, and a wrong one would mint another owner's token for this caller.
//
// COULD-NOT-TELL IS NOT NO. GitHub not answering says nothing about the installation:
// once proved it stays proved, and never proved answers unavailable, not refused.
func (s *Server) proved(r *http.Request, inst *installation) (int, string, string) {
	inst.mu.Lock()
	defer inst.mu.Unlock()

	if !inst.verified.IsZero() && (s.now().Sub(inst.verified) < verifyEvery || s.now().Before(inst.proofRetryAt)) {
		return 0, "", ""
	}

	// ITS OWN BOUND, because this answers a caller that waits a limited time for a token this
	// broker may already hold, and a stalled GitHub must not take that wait.
	ctx, cancel := context.WithTimeout(r.Context(), s.proofBound)
	defer cancel()

	err := s.verify(r.WithContext(ctx), inst)

	switch {
	case err == nil:
		inst.verified, inst.proofRetryAt = s.now(), time.Time{}

		return 0, "", ""
	// THE CALLER WENT AWAY, which says nothing about the installation, and must not
	// erase a proof a later outage would need.
	case r.Context().Err() != nil:
		return http.StatusServiceUnavailable, codeUnavailable, "the installation could not be checked just now"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, github.ErrAppUnverifiable), github.Undecided(err):
		s.log.Warn("could not prove an installation is the one registered", "deployment", inst.deployment,
			"installation", inst.installationID, "error", err)

		if !inst.verified.IsZero() {
			inst.proofRetryAt = s.now().Add(proofRetry)

			return 0, "", ""
		}

		return http.StatusServiceUnavailable, codeUnavailable, "the installation could not be checked just now"
	default:
		s.log.Error("an installation is not what its registration says", "deployment", inst.deployment,
			"installation", inst.installationID, "target", inst.target.Path(), "error", err)

		inst.verified = time.Time{}

		if errors.Is(err, github.ErrNotInstalled) {
			return http.StatusNotFound, codeNotInstalled, "the App is not installed there"
		}

		return http.StatusForbidden, codeMismatch, "the installation is not what this broker's registration says"
	}
}

func verifyInstallation(r *http.Request, inst *installation) error {
	_, err := github.VerifyAppAt(r.Context(), nil, inst.base, inst.appID, inst.key, inst.target,
		inst.installationID, inst.actions)

	return err //nolint:wrapcheck // read through by proved, and logged whole.
}

func (s *Server) refuse(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(errorResponse{Code: code, Message: message}); err != nil {
		s.log.Warn("could not write a refusal to its caller", "code", code, "error", err)
	}
}

// loadBounded reads a small PEM file through one descriptor no FIFO can hang.
func loadBounded(path string) ([]byte, error) {
	f, _, err := regularfile.Open(path, regularfile.Options{})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	body, err := regularfile.ReadAllLimited(f, path, maxPEM)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return body, nil
}

// maxPEM bounds a key or a bundle read: past this it is not one.
const maxPEM = 1 << 20
