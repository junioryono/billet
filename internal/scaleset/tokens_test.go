package scaleset

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/fakeactions"
	billetgithub "github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/server"
)

// stubTokens is a source a test drives: the token it hands out next, or a failure.
type stubTokens struct {
	token  atomic.Pointer[string]
	err    atomic.Pointer[error]
	called atomic.Int32
}

func (s *stubTokens) InstallationToken(context.Context) (billetgithub.Token, error) {
	s.called.Add(1)

	if err := s.err.Load(); err != nil {
		return billetgithub.Token{}, *err
	}

	return billetgithub.NewToken(*s.token.Load(), time.Now().Add(time.Hour), nil), nil
}

func newStubTokens(token string) *stubTokens {
	s := &stubTokens{}
	s.token.Store(&token)

	return s
}

// tokenConfig is a client on a token source and no key at all.
func tokenConfig(t *testing.T, f *fakeActions, tokens billetgithub.TokenSource) Config {
	t.Helper()

	cfg := f.config(t)
	cfg.PrivateKey, cfg.ClientID, cfg.Tokens = "", "", tokens

	return cfg
}

// tokenFake answers what a provisioning and one idle poll ask.
func tokenFake(t *testing.T) *fakeActions {
	t.Helper()

	return newFakeActions(t, func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		switch {
		case strings.Contains(path, "runnergroups"):
			writeJSON(t, w, listJSON(map[string]any{"id": 1, "name": testGroup, "isDefaultGroup": false}))
		case r.Method == http.MethodGet && strings.Contains(path, "/queue"):
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// A CLIENT ON A TOKEN SOURCE SENDS THE SOURCE'S TOKEN, and the placeholder it was built
// with never leaves the process. The registration-token request is the one the
// vendored client signs with what it holds, and GitHub's record of it is what proves it.
func TestAClientOnATokenSourceSendsTheSourcesToken(t *testing.T) {
	t.Parallel()

	f := tokenFake(t)
	tokens := newStubTokens("minted-by-the-broker")

	client, err := New(tokenConfig(t, f, tokens), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := client.gh.GetRunnerGroupByName(t.Context(), testGroup); err != nil {
		t.Fatalf("a call that refreshes the client's token: %v", err)
	}

	calls := f.calls("/actions/runners/registration-token")
	if len(calls) != 1 {
		t.Fatalf("the registration-token endpoint was asked %d times", len(calls))
	}

	if got := calls[0].Authorization; got != "Bearer minted-by-the-broker" {
		t.Fatalf("the registration-token request carried %q", got)
	}

	if tokens.called.Load() == 0 {
		t.Fatal("the source was never asked")
	}

	for _, call := range f.calls("/") {
		if strings.Contains(call.Authorization, tokenPlaceholder) {
			t.Fatalf("%s %s carried the placeholder", call.Method, call.Path)
		}
	}

	if len(f.calls("/access_tokens")) != 0 {
		t.Fatal("a client holding no key asked for an access token, so something signed a JWT")
	}
}

// A SOURCE THAT CANNOT ANSWER STOPS THE REQUEST, never sends it unauthenticated, and
// names itself: GitHub's 401 for no credential would read as a verdict about the App.
func TestASourceThatCannotAnswerSendsNothingAndSaysSo(t *testing.T) {
	t.Parallel()

	f := tokenFake(t)
	tokens := newStubTokens("unused")

	down := billetgithub.ErrTokenUnavailable
	tokens.err.Store(&down)

	client, err := New(tokenConfig(t, f, tokens), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.EnsureScaleSet(t.Context(), testSetName, testGroup, []string{testSetName})
	if !errors.Is(err, billetgithub.ErrTokenUnavailable) {
		t.Fatalf("got %v, want %v", err, billetgithub.ErrTokenUnavailable)
	}

	if !billetgithub.Undecided(err) {
		t.Errorf("Undecided(%v) is false, and this is could-not-tell", err)
	}

	if strings.Contains(err.Error(), "unsupported protocol scheme") {
		t.Errorf("the error is the transport's words about a scheme: %v", err)
	}

	if calls := f.calls("/actions/runners/registration-token"); len(calls) != 0 {
		t.Fatalf("%d requests reached GitHub with no token to send", len(calls))
	}

	// ONCE, not through the retry budget: a stopped request is not a flaky one.
	if n := tokens.called.Load(); n != 1 {
		t.Errorf("the source was asked %d times for one call", n)
	}

	// AND A SOURCE THAT COMES BACK SERVES THE NEXT CALL.
	tokens.err.Store(nil)

	if _, err := client.gh.GetRunnerGroupByName(t.Context(), testGroup); err != nil {
		t.Fatalf("after the source came back: %v", err)
	}
}

// AN OPEN SESSION RIDES OUT A SOURCE THAT CANNOT ANSWER: a poll carries the Actions
// service's own token and never asks the source, so a broker outage costs a deployment
// nothing until a refresh is due.
func TestAPollDoesNotAskTheSource(t *testing.T) {
	t.Parallel()

	var (
		polled atomic.Int32
		f      *fakeActions
	)

	f = newFakeActions(t, func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		switch {
		case strings.HasSuffix(path, "/sessions"):
			writeJSON(t, w, fakeactions.SessionJSON("3f8a1c22-0000-4000-8000-000000000001", "billet",
				scaleSetJSON(7, testSetName), f.URL+"/queue", "queue-token"))
		case strings.HasPrefix(path, "/queue"):
			polled.Add(1)
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	tokens := newStubTokens("minted-by-the-broker")

	client, err := New(tokenConfig(t, f, tokens), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	session, err := client.Session(t.Context(), 7, "billet")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}

	before := tokens.called.Load()

	down := billetgithub.ErrTokenUnavailable
	tokens.err.Store(&down)

	if _, err := session.GetMessage(t.Context(), 0, 0); !errors.Is(err, server.ErrNoMessage) {
		t.Fatalf("a poll with the source down answered %v, want %v", err, server.ErrNoMessage)
	}

	if polled.Load() == 0 {
		t.Fatal("no poll reached the service, so this proves nothing")
	}

	if after := tokens.called.Load(); after != before {
		t.Errorf("a poll asked the source %d more times", after-before)
	}
}

// A RETRY ASKS AGAIN, because the attempt before it may have waited out a Retry-After
// longer than the token it sent had left. Driven through the hook itself, with the
// header a first attempt leaves behind.
func TestARetryOfARegistrationTakesAFreshToken(t *testing.T) {
	t.Parallel()

	tokens := newStubTokens("the-second-token")
	hook := sourceInstallationToken(tokens, &tokenFailures{}, slog.Default())

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.github.com/orgs/acme/actions/runners/registration-token", http.NoBody)
	req.Header.Set("Authorization", "Bearer the-first-token")

	hook(nil, req, 1)

	if got := req.Header.Get("Authorization"); got != "Bearer the-second-token" {
		t.Fatalf("the retry carried %q", got)
	}

	// EVERY OTHER REQUEST CARRIES SOMETHING THE SOURCE DID NOT ISSUE, and is left alone.
	other := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://broker.actions.githubusercontent.com/queue", http.NoBody)
	other.Header.Set("Authorization", "Bearer queue-token")

	before := tokens.called.Load()

	hook(nil, other, 0)

	if got := other.Header.Get("Authorization"); got != "Bearer queue-token" {
		t.Errorf("a poll's credential became %q", got)
	}

	if tokens.called.Load() != before {
		t.Error("a poll asked the source")
	}
}

// BOTH CREDENTIALS IS A MISTAKE, refused rather than quietly preferring one.
func TestAClientRefusesAKeyAndASourceTogether(t *testing.T) {
	t.Parallel()

	f := tokenFake(t)

	cfg := f.config(t)
	cfg.Tokens = newStubTokens("unused")

	if _, err := New(cfg, nil); err == nil || !strings.Contains(err.Error(), "both an App key and a token source") {
		t.Fatalf("New answered %v", err)
	}
}
