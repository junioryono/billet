package tokenbroker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/github"
)

const (
	deploymentA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	deploymentB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	appID       = int64(11)
)

// fakeGitHub is what a broker mints against: it answers the installation lookup and
// the token exchange, and records what was asked.
type fakeGitHub struct {
	*httptest.Server

	mu       sync.Mutex
	scopes   []string
	lookup   func(w http.ResponseWriter, r *http.Request)
	mint     func(w http.ResponseWriter, r *http.Request)
	lookups  atomic.Int32
	minted   atomic.Int32
	tokenFor func(n int32) string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()

	f := &fakeGitHub{tokenFor: func(n int32) string { return fmt.Sprintf("minted-token-%d", n) }}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)

	return f
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/access_tokens"):
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		f.mu.Lock()
		f.scopes = append(f.scopes, string(body))
		f.mu.Unlock()

		if f.mint != nil {
			f.mint(w, r)

			return
		}

		w.WriteHeader(http.StatusCreated)

		var scope struct {
			Permissions map[string]string `json:"permissions"`
		}
		if err := json.Unmarshal(body, &scope); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		answerJSON(w, map[string]any{
			"token": f.tokenFor(f.minted.Add(1)), "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"permissions": scope.Permissions,
		})
	case strings.HasSuffix(r.URL.Path, "/installation"):
		f.lookups.Add(1)

		if f.lookup != nil {
			f.lookup(w, r)

			return
		}

		id := int64(22)
		if strings.Contains(r.URL.Path, "/repos/") {
			id = 33
		}

		perms := map[string]string{"metadata": "read", "organization_self_hosted_runners": "write"}
		if strings.Contains(r.URL.Path, "/repos/") {
			perms = map[string]string{"metadata": "read", "administration": "write"}
		}

		answerJSON(w, map[string]any{
			"id": id, "account": map[string]string{"login": "acme", "type": "Organization"}, "permissions": perms,
		})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) scopesSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.scopes)
}

// testAppKey writes a throwaway App key where the broker's hardened reader accepts it.
func testAppKey(t *testing.T) (string, []byte) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	body := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	return path, body
}

// testClient is one host: a key on disk the way `billet broker client-key` makes it,
// and its fingerprint.
func testClient(t *testing.T) (tls.Certificate, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "client.key")

	fingerprint, err := CreateClientKey(path)
	if err != nil {
		t.Fatalf("CreateClientKey: %v", err)
	}

	key, err := ReadClientKey(path)
	if err != nil {
		t.Fatalf("ReadClientKey: %v", err)
	}

	cert, err := ClientCertificate(key)
	if err != nil {
		t.Fatalf("ClientCertificate: %v", err)
	}

	return cert, fingerprint
}

// testBroker is a broker on a real handshake, requiring a certificate and verifying no
// chain, as it does in production.
type testBroker struct {
	*httptest.Server

	server *Server
	logged *lockedBuffer
	key    []byte
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func newTestBroker(t *testing.T, gh *fakeGitHub, deployments []config.BrokerDeployment) *testBroker {
	t.Helper()

	keyPath, keyBody := testAppKey(t)
	logged := &lockedBuffer{}

	server, err := NewServer(&config.BrokerConfig{
		Listen: "unused", CertFile: "unused", KeyFile: "unused",
		Apps:        []config.BrokerApp{{AppID: appID, PrivateKeyPath: keyPath, APIURL: gh.URL}},
		Deployments: deployments,
	}, slog.New(slog.NewTextHandler(logged, nil)))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	srv := httptest.NewUnstartedServer(server)
	srv.TLS = &tls.Config{
		ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS13, VerifyConnection: server.VerifyConnection,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return &testBroker{Server: srv, server: server, logged: logged, key: keyBody}
}

func (b *testBroker) client(t *testing.T, cert tls.Certificate, identity func() (string, bool, error)) *Client {
	t.Helper()

	roots := x509.NewCertPool()
	roots.AddCert(b.Certificate())

	return NewClient(ClientOptions{URL: b.URL, Certificate: cert, Roots: roots, Identity: identity})
}

func org(installationID int64) config.BrokerInstallation {
	return config.BrokerInstallation{AppID: appID, InstallationID: installationID, Target: "acme"}
}

// THE REFUSAL THIS BROKER EXISTS FOR. A deployment gets a token for what is registered
// to it and for nothing else, whether that is another deployment's installation or no
// one's, in the same words for both, and GitHub is never asked.
func TestABrokerRefusesAnInstallationNotRegisteredToTheCaller(t *testing.T) {
	t.Parallel()

	gh := newFakeGitHub(t)
	certA, fingerprintA := testClient(t)
	_, fingerprintB := testClient(t)

	broker := newTestBroker(t, gh, []config.BrokerDeployment{
		{ID: deploymentA, Clients: []string{fingerprintA}, Installations: []config.BrokerInstallation{org(22)}},
		{ID: deploymentB, Clients: []string{fingerprintB}, Installations: []config.BrokerInstallation{org(44)}},
	})

	client := broker.client(t, certA, nil)

	var messages []string

	for name, ask := range map[string]github.TokenSource{
		"another deployment's installation": client.Tokens(github.OrganizationTarget("acme"), appID, 44),
		"nobody's installation":             client.Tokens(github.OrganizationTarget("acme"), appID, 99),
		"its installation, another target":  client.Tokens(github.OrganizationTarget("other"), appID, 22),
		"its installation, another App":     client.Tokens(github.OrganizationTarget("acme"), 12, 22),
	} {
		_, err := ask.InstallationToken(t.Context())
		if !errors.Is(err, github.ErrTokenRefused) {
			t.Errorf("%s: got %v, want %v", name, err, github.ErrTokenRefused)

			continue
		}

		messages = append(messages, err.Error())
	}

	// THE SAME WORDS EVERY TIME: an answer that told another deployment's from nobody's
	// would say who else this broker serves.
	for _, m := range messages {
		if m != messages[0] {
			t.Errorf("two refusals differ, so one says more than the other:\n%s\n%s", m, messages[0])
		}
	}

	if n := gh.minted.Load(); n != 0 {
		t.Errorf("GitHub was asked to mint %d tokens for callers this broker refused", n)
	}
	if n := gh.lookups.Load(); n != 0 {
		t.Errorf("GitHub was asked about %d installations for callers this broker refused", n)
	}

	// And the one that is registered is minted, on the same connection and key.
	token, err := client.Tokens(github.OrganizationTarget("acme"), appID, 22).InstallationToken(t.Context())
	if err != nil || token.Bearer() != "minted-token-1" {
		t.Fatalf("the registered installation answered %v, %v", token.Bearer(), err)
	}
}

// A KEY THE BROKER DOES NOT KNOW GETS NOTHING, and a caller with no key does not get
// past the handshake.
func TestABrokerRefusesAKeyItDoesNotKnow(t *testing.T) {
	t.Parallel()

	gh := newFakeGitHub(t)
	_, known := testClient(t)
	stranger, _ := testClient(t)

	broker := newTestBroker(t, gh, []config.BrokerDeployment{
		{ID: deploymentA, Clients: []string{known}, Installations: []config.BrokerInstallation{org(22)}},
	})

	// REFUSED IN THE HANDSHAKE, so a stranger never holds a place in the budget, and it
	// reaches the caller as nothing it can read as an answer, naming what to look at.
	_, err := broker.client(t, stranger, nil).Tokens(github.OrganizationTarget("acme"), appID, 22).
		InstallationToken(t.Context())
	if !errors.Is(err, github.ErrTokenUnavailable) || !strings.Contains(err.Error(), "client-key --show") {
		t.Fatalf("a stranger's key got %v, want could-not-tell naming the key", err)
	}

	if !strings.Contains(broker.logged.String(), "refused a client this broker does not know") {
		t.Errorf("the broker says nothing of the stranger:\n%s", broker.logged.String())
	}

	// AND THE HANDLER REFUSES ONE TOO, which is what stands if a listener is ever built
	// without the check.
	strangerLeaf, err := x509.ParseCertificate(stranger.Certificate[0])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tokenRoute, strings.NewReader(`{}`))
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{strangerLeaf}}
	broker.server.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("the handler answered a stranger %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	if n := gh.minted.Load(); n != 0 {
		t.Errorf("GitHub was asked to mint %d tokens for a stranger", n)
	}

	// NO CERTIFICATE AT ALL ends in the handshake. It is could-not-tell to the caller,
	// since the alert reaches it as nothing it can read as an answer.
	bare := NewClient(ClientOptions{URL: broker.URL, Roots: func() *x509.CertPool {
		roots := x509.NewCertPool()
		roots.AddCert(broker.Certificate())

		return roots
	}()})

	_, err = bare.Tokens(github.OrganizationTarget("acme"), appID, 22).InstallationToken(t.Context())
	if !errors.Is(err, github.ErrTokenUnavailable) {
		t.Fatalf("no certificate got %v, want %v", err, github.ErrTokenUnavailable)
	}
}

// THE TOKEN IS SCOPED TO WHAT THE DEPLOYMENT NEEDS: exactly billet's permissions for
// the registered target, and for a repository that repository alone.
func TestABrokerScopesATokenToWhatTheDeploymentNeeds(t *testing.T) {
	t.Parallel()

	gh := newFakeGitHub(t)
	cert, fingerprint := testClient(t)

	broker := newTestBroker(t, gh, []config.BrokerDeployment{{
		ID: deploymentA, Clients: []string{fingerprint},
		Installations: []config.BrokerInstallation{
			org(22),
			{AppID: appID, InstallationID: 33, Target: "acme/api"},
		},
	}})

	client := broker.client(t, cert, nil)

	organization, err := client.Tokens(github.OrganizationTarget("acme"), appID, 22).InstallationToken(t.Context())
	if err != nil {
		t.Fatalf("organization token: %v", err)
	}

	if got := organization.Permissions(); got["organization_self_hosted_runners"] != "write" || got["metadata"] != "read" || len(got) != 2 {
		t.Errorf("the organization token holds %v", got)
	}

	if _, err := client.Tokens(github.RepositoryTarget("acme", "api"), appID, 33).InstallationToken(t.Context()); err != nil {
		t.Fatalf("repository token: %v", err)
	}

	scopes := gh.scopesSeen()
	if len(scopes) != 2 {
		t.Fatalf("GitHub was asked %d times: %v", len(scopes), scopes)
	}

	var repo struct {
		Permissions  map[string]string `json:"permissions"`
		Repositories []string          `json:"repositories"`
	}
	if err := json.Unmarshal([]byte(scopes[1]), &repo); err != nil {
		t.Fatalf("the repository's scope: %v: %s", err, scopes[1])
	}

	if !slices.Equal(repo.Repositories, []string{"api"}) {
		t.Errorf("the repository token reaches %v, want only api", repo.Repositories)
	}
	if repo.Permissions["administration"] != "write" || len(repo.Permissions) != 2 {
		t.Errorf("the repository token holds %v", repo.Permissions)
	}
}

// A BROKER THAT CANNOT ANSWER IS NEVER "THE INSTALLATION IS GONE". GitHub being down
// is could-not-tell, both while proving the installation and while minting, and only
// GitHub's own word says the App is not there.
func TestAFailureToMintIsNeverAnAbsentInstallation(t *testing.T) {
	t.Parallel()

	down := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }

	for name, tc := range map[string]struct {
		lookup, mint func(http.ResponseWriter, *http.Request)
		want         error
		notWant      error
	}{
		"GitHub down while proving": {lookup: down, want: github.ErrTokenUnavailable, notWant: github.ErrNotInstalled},
		"GitHub down while minting": {mint: down, want: github.ErrTokenUnavailable, notWant: github.ErrNotInstalled},
		"GitHub says it is not installed": {
			lookup: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
			want:   github.ErrNotInstalled, notWant: github.ErrTokenUnavailable,
		},
		// GITHUB'S OWN WORD ON THE MINT IS AS GOOD AS ON THE PROOF: an App uninstalled after
		// its proof is not could-not-tell for the next hour.
		"uninstalled since it was proved": {
			mint: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
			want: github.ErrNotInstalled, notWant: github.ErrTokenUnavailable,
		},
		"GitHub refuses the mint": {
			mint: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				answerJSON(w, map[string]string{"message": "Resource not accessible by integration"})
			},
			want: github.ErrTokenRefused, notWant: github.ErrTokenUnavailable,
		},
		"GitHub cannot process the mint": {
			mint: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnprocessableEntity) },
			want: github.ErrTokenRefused, notWant: github.ErrTokenUnavailable,
		},
		// A THROTTLE ANSWERS 403 AND IS NOT A REFUSAL.
		"GitHub throttles the mint": {
			mint: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				answerJSON(w, map[string]string{"message": "API rate limit exceeded"})
			},
			want: github.ErrTokenUnavailable, notWant: github.ErrTokenRefused,
		},
		"another installation at that target": {
			lookup: func(w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, map[string]any{
					"id": 999, "account": map[string]string{"login": "acme", "type": "Organization"},
					"permissions": map[string]string{"metadata": "read", "organization_self_hosted_runners": "write"},
				})
			},
			want: github.ErrTokenRefused, notWant: github.ErrTokenUnavailable,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			gh := newFakeGitHub(t)
			gh.lookup, gh.mint = tc.lookup, tc.mint
			cert, fingerprint := testClient(t)

			broker := newTestBroker(t, gh, []config.BrokerDeployment{
				{ID: deploymentA, Clients: []string{fingerprint}, Installations: []config.BrokerInstallation{org(22)}},
			})

			_, err := broker.client(t, cert, nil).Tokens(github.OrganizationTarget("acme"), appID, 22).
				InstallationToken(t.Context())

			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if errors.Is(err, tc.notWant) {
				t.Fatalf("got %v, which must not read as %v", err, tc.notWant)
			}

			if errors.Is(tc.want, github.ErrTokenUnavailable) && !github.Undecided(err) {
				t.Errorf("Undecided(%v) is false, and this is could-not-tell", err)
			}
		})
	}
}

// AN INSTALLATION PROVED ONCE STAYS PROVED while GitHub cannot be asked again, and is
// asked no more than the broker's interval needs.
func TestAProvedInstallationIsNotAskedAboutAgain(t *testing.T) {
	t.Parallel()

	gh := newFakeGitHub(t)
	cert, fingerprint := testClient(t)

	broker := newTestBroker(t, gh, []config.BrokerDeployment{
		{ID: deploymentA, Clients: []string{fingerprint}, Installations: []config.BrokerInstallation{org(22)}},
	})

	now := time.Now()
	broker.server.now = func() time.Time { return now }

	client := broker.client(t, cert, nil)
	source := client.Tokens(github.OrganizationTarget("acme"), appID, 22)

	if _, err := source.InstallationToken(t.Context()); err != nil {
		t.Fatalf("first token: %v", err)
	}

	if _, err := client.Tokens(github.OrganizationTarget("acme"), appID, 22).InstallationToken(t.Context()); err != nil {
		t.Fatalf("second caller: %v", err)
	}

	if n := gh.lookups.Load(); n != 1 {
		t.Fatalf("GitHub was asked about the installation %d times, want once in the interval", n)
	}
	if n := gh.minted.Load(); n != 1 {
		t.Fatalf("GitHub minted %d tokens for two questions; a fresh one is handed out again", n)
	}

	// PAST THE INTERVAL it is asked again, and GitHub not answering is not a verdict.
	now = now.Add(verifyEvery + time.Minute)
	gh.lookup = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }

	if _, err := client.Tokens(github.OrganizationTarget("acme"), appID, 22).InstallationToken(t.Context()); err != nil {
		t.Fatalf("a proved installation refused on GitHub not answering: %v", err)
	}
	if n := gh.lookups.Load(); n != 2 {
		t.Fatalf("GitHub was asked %d times, want it asked again past the interval", n)
	}
}

// A TOKEN IS NEVER LOGGED, and neither is the key, whatever happens to the request.
func TestABrokerLogsNoCredential(t *testing.T) {
	t.Parallel()

	gh := newFakeGitHub(t)
	cert, fingerprint := testClient(t)

	broker := newTestBroker(t, gh, []config.BrokerDeployment{
		{ID: deploymentA, Clients: []string{fingerprint}, Installations: []config.BrokerInstallation{org(22)}},
	})

	token, err := broker.client(t, cert, nil).Tokens(github.OrganizationTarget("acme"), appID, 22).
		InstallationToken(t.Context())
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	logged := broker.logged.String()
	if !strings.Contains(logged, "served an installation token") {
		t.Fatalf("the broker logged nothing about the mint, so this proves nothing:\n%s", logged)
	}

	if strings.Contains(logged, token.Bearer()) {
		t.Errorf("the token reached the log:\n%s", logged)
	}

	if strings.Contains(logged, "PRIVATE KEY") {
		t.Errorf("the App key reached the log:\n%s", logged)
	}
}

// NOR WHEN IT GOES WRONG, which is where a broker logs the most. GitHub echoes what it was
// sent into this refusal, so the JWT that asked is in the error the broker logs unless
// something took it out.
func TestABrokerLogsNoCredentialWhenAMintFails(t *testing.T) {
	t.Parallel()

	var (
		mu  sync.Mutex
		jwt string
	)

	gh := newFakeGitHub(t)
	gh.mint = func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

		mu.Lock()
		jwt = bearer
		mu.Unlock()

		w.WriteHeader(http.StatusForbidden)
		answerJSON(w, map[string]string{"message": "refused " + bearer})
	}

	cert, fingerprint := testClient(t)

	broker := newTestBroker(t, gh, []config.BrokerDeployment{
		{ID: deploymentA, Clients: []string{fingerprint}, Installations: []config.BrokerInstallation{org(22)}},
	})

	if _, err := broker.client(t, cert, nil).Tokens(github.OrganizationTarget("acme"), appID, 22).
		InstallationToken(t.Context()); err == nil {
		t.Fatal("a refused mint answered a token")
	}

	logged := broker.logged.String()
	if !strings.Contains(logged, "could not mint an installation token") {
		t.Fatalf("the broker logged nothing about the failure, so this proves nothing:\n%s", logged)
	}

	mu.Lock()
	defer mu.Unlock()

	if jwt == "" {
		t.Fatal("GitHub never saw a JWT, so this proves nothing")
	}

	if strings.Contains(logged, jwt) {
		t.Errorf("the App's JWT reached the log:\n%s", logged)
	}
}

// A FAILURE IS NOT ASKED ABOUT AGAIN FOR A WHILE. Every deployment on an App shares its
// budget at GitHub, so one deployment retrying a mint that cannot succeed spends all of
// theirs.
func TestAFailureIsAnsweredFromMemoryAndThenAskedAgain(t *testing.T) {
	t.Parallel()

	var mints atomic.Int64

	gh := newFakeGitHub(t)
	gh.mint = func(w http.ResponseWriter, _ *http.Request) {
		mints.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}

	cert, fingerprint := testClient(t)

	broker := newTestBroker(t, gh, []config.BrokerDeployment{
		{ID: deploymentA, Clients: []string{fingerprint}, Installations: []config.BrokerInstallation{org(22)}},
	})

	now := time.Now()

	var clock sync.Mutex

	broker.server.now = func() time.Time {
		clock.Lock()
		defer clock.Unlock()

		return now
	}

	source := broker.client(t, cert, nil).Tokens(github.OrganizationTarget("acme"), appID, 22)

	for range 3 {
		if _, err := source.InstallationToken(t.Context()); !errors.Is(err, github.ErrTokenUnavailable) {
			t.Fatalf("got %v, want could-not-tell", err)
		}
	}

	if got := mints.Load(); got != 1 {
		t.Errorf("GitHub was asked %d times for three requests inside the memory", got)
	}

	clock.Lock()
	now = now.Add(failureMemory + time.Second)
	clock.Unlock()

	if _, err := source.InstallationToken(t.Context()); !errors.Is(err, github.ErrTokenUnavailable) {
		t.Fatalf("got %v, want could-not-tell", err)
	}

	if got := mints.Load(); got != 2 {
		t.Errorf("GitHub was asked %d times, and the memory has passed", got)
	}
}

// A STALLED GITHUB DOES NOT KEEP A HELD TOKEN FROM ITS CALLER. A proof falls due at the hour
// with most of a token's life left, and a lookup that hangs past what a client waits would
// otherwise time out every request, for as long as GitHub stalls, with the token in hand.
func TestAStalledProofDoesNotStarveAHeldToken(t *testing.T) {
	t.Parallel()

	var lookups atomic.Int64

	stalled := make(chan struct{})
	t.Cleanup(func() { close(stalled) })

	gh := newFakeGitHub(t)
	cert, fingerprint := testClient(t)

	broker := newTestBroker(t, gh, []config.BrokerDeployment{
		{ID: deploymentA, Clients: []string{fingerprint}, Installations: []config.BrokerInstallation{org(22)}},
	})

	now := time.Now()

	var clock sync.Mutex

	broker.server.now = func() time.Time {
		clock.Lock()
		defer clock.Unlock()

		return now
	}

	source := func() github.TokenSource {
		return broker.client(t, cert, nil).Tokens(github.OrganizationTarget("acme"), appID, 22)
	}

	if _, err := source().InstallationToken(t.Context()); err != nil {
		t.Fatalf("the first token: %v", err)
	}

	// THE PROOF'S BOUND IS LONG ENOUGH FOR ITS REQUEST TO ARRIVE. At 50 ms a loaded
	// race-and-coverage runner did not get the request to the stand-in before the
	// bound ended it, so the proof failed with nothing asked and the count read 0
	// (#773). Two seconds still ends every stalled proof, well inside a client's
	// wait; and each lookup is signalled, so the count is read once the stand-in
	// has seen it.
	asked := make(chan struct{}, 8)

	broker.server.proofBound = 2 * time.Second
	gh.lookup = func(_ http.ResponseWriter, r *http.Request) {
		lookups.Add(1)
		asked <- struct{}{}

		select {
		case <-stalled:
		case <-r.Context().Done():
		}
	}

	// awaitLookup waits for the next lookup, the bound only a guard against one
	// that never comes.
	awaitLookup := func(which string) {
		t.Helper()

		select {
		case <-asked:
		case <-time.After(30 * time.Second):
			t.Fatalf("GitHub was never asked for the %s proof", which)
		}
	}

	clock.Lock()
	now = now.Add(verifyEvery + time.Minute)
	clock.Unlock()

	for range 3 {
		if _, err := source().InstallationToken(t.Context()); err != nil {
			t.Fatalf("a held token did not reach its caller while GitHub stalled: %v", err)
		}
	}

	awaitLookup("due")

	if got := lookups.Load(); got != 1 {
		t.Errorf("GitHub was asked to prove it %d times in one retry period", got)
	}

	clock.Lock()
	now = now.Add(proofRetry + time.Second)
	clock.Unlock()

	if _, err := source().InstallationToken(t.Context()); err != nil {
		t.Fatalf("after the retry period: %v", err)
	}

	awaitLookup("overdue")

	if got := lookups.Load(); got != 2 {
		t.Errorf("the overdue proof was asked for %d times, want it tried again after its retry period", got)
	}
}

// A BURST PASSES THE MEMORY ONE AT A TIME. Together they would each find nothing remembered
// and each ask GitHub, which is the spending the memory exists to stop.
func TestABurstAgainstAFailingInstallationAsksOnce(t *testing.T) {
	t.Parallel()

	var mints atomic.Int64

	gh := newFakeGitHub(t)
	gh.mint = func(w http.ResponseWriter, _ *http.Request) {
		mints.Add(1)
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusBadGateway)
	}

	cert, fingerprint := testClient(t)

	broker := newTestBroker(t, gh, []config.BrokerDeployment{
		{ID: deploymentA, Clients: []string{fingerprint}, Installations: []config.BrokerInstallation{org(22)}},
	})

	var burst sync.WaitGroup

	for range 8 {
		source := broker.client(t, cert, nil).Tokens(github.OrganizationTarget("acme"), appID, 22)

		burst.Go(func() {
			if _, err := source.InstallationToken(t.Context()); !errors.Is(err, github.ErrTokenUnavailable) {
				t.Errorf("got %v, want could-not-tell", err)
			}
		})
	}

	burst.Wait()

	if got := mints.Load(); got != 1 {
		t.Errorf("GitHub was asked %d times by a burst of 8", got)
	}
}

// A SUCCESS ENDS THE MEMORY: the next caller is not told about a failure that has passed.
func TestASuccessForgetsAFailure(t *testing.T) {
	t.Parallel()

	var failing atomic.Bool

	failing.Store(true)

	gh := newFakeGitHub(t)

	gh.mint = func(w http.ResponseWriter, _ *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		w.WriteHeader(http.StatusCreated)
		answerJSON(w, map[string]any{
			"token": gh.tokenFor(gh.minted.Add(1)), "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}

	cert, fingerprint := testClient(t)

	broker := newTestBroker(t, gh, []config.BrokerDeployment{
		{ID: deploymentA, Clients: []string{fingerprint}, Installations: []config.BrokerInstallation{org(22)}},
	})

	now := time.Now()

	var clock sync.Mutex

	broker.server.now = func() time.Time {
		clock.Lock()
		defer clock.Unlock()

		return now
	}

	source := broker.client(t, cert, nil).Tokens(github.OrganizationTarget("acme"), appID, 22)

	if _, err := source.InstallationToken(t.Context()); !errors.Is(err, github.ErrNotInstalled) {
		t.Fatalf("got %v, want not installed", err)
	}

	failing.Store(false)

	clock.Lock()
	now = now.Add(failureMemory + time.Second)
	clock.Unlock()

	if _, err := source.InstallationToken(t.Context()); err != nil {
		t.Fatalf("the mint works again and answered %v", err)
	}

	inst := broker.server.installations[keyOf(deploymentA, appID, 22, "acme")]
	if !inst.failedUntil.IsZero() {
		t.Errorf("a success left a failure remembered until %v", inst.failedUntil)
	}
}

// A REGISTRATION'S actions_read AND actions_write ARE WHAT ITS TOKEN CARRIES, in every
// direction: without either a deployment is refused GitHub's job records, with read
// the broker has to ask for them, and with write it asks for the re-runs too.
func TestARegistrationsActionsReadReachesTheScope(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		read, write bool
		want        string
	}{
		"granted":       {read: true, want: "read"},
		"not granted":   {want: ""},
		"write granted": {write: true, want: "write"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			held := "read"
			if tc.write {
				held = "write"
			}

			gh := newFakeGitHub(t)
			gh.lookup = func(w http.ResponseWriter, _ *http.Request) {
				answerJSON(w, map[string]any{
					"id": 22, "account": map[string]string{"login": "acme", "type": "Organization"},
					"permissions": map[string]string{
						"metadata": "read", "organization_self_hosted_runners": "write", "actions": held,
					},
				})
			}
			cert, fingerprint := testClient(t)

			reg := org(22)
			reg.ActionsRead, reg.ActionsWrite = tc.read, tc.write

			broker := newTestBroker(t, gh, []config.BrokerDeployment{
				{ID: deploymentA, Clients: []string{fingerprint}, Installations: []config.BrokerInstallation{reg}},
			})

			token, err := broker.client(t, cert, nil).Tokens(github.OrganizationTarget("acme"), appID, 22).
				InstallationToken(t.Context())
			if err != nil {
				t.Fatalf("token: %v", err)
			}

			if got := token.Permissions()["actions"]; got != tc.want {
				t.Errorf("registered read %v and write %v, and the token holds %v", tc.read, tc.write,
					token.Permissions())
			}
		})
	}
}

// TWO DEPLOYMENTS SHARING AN INSTALLATION, one re-running jobs and one not, are each
// served: the installation is proved against the write the first needs, and each token
// carries only what its own registration grants.
func TestASharedInstallationServesEachRegistrationItsOwnScope(t *testing.T) {
	t.Parallel()

	gh := newFakeGitHub(t)
	gh.lookup = func(w http.ResponseWriter, _ *http.Request) {
		answerJSON(w, map[string]any{
			"id": 22, "account": map[string]string{"login": "acme", "type": "Organization"},
			"permissions": map[string]string{
				"metadata": "read", "organization_self_hosted_runners": "write", "actions": "write",
			},
		})
	}
	writer, writerPrint := testClient(t)
	reader, readerPrint := testClient(t)

	write, read := org(22), org(22)
	write.ActionsWrite, read.ActionsRead = true, true

	broker := newTestBroker(t, gh, []config.BrokerDeployment{
		{ID: deploymentA, Clients: []string{writerPrint}, Installations: []config.BrokerInstallation{write}},
		{ID: deploymentB, Clients: []string{readerPrint}, Installations: []config.BrokerInstallation{read}},
	})

	for name, tc := range map[string]struct {
		cert tls.Certificate
		want string
	}{
		"the deployment re-running jobs": {writer, "write"},
		"the deployment reading only":    {reader, "read"},
	} {
		token, err := broker.client(t, tc.cert, nil).Tokens(github.OrganizationTarget("acme"), appID, 22).
			InstallationToken(t.Context())
		if err != nil {
			t.Fatalf("%s: token: %v", name, err)
		}
		if got := token.Permissions()["actions"]; got != tc.want {
			t.Errorf("%s: the token holds actions %q, want %q", name, got, tc.want)
		}
	}
}

// A HOST WHOSE KEY IS REGISTERED UNDER ANOTHER DEPLOYMENT finds out from its first
// answer, and uses nothing it was handed.
func TestAHostRefusesATokenMintedForAnotherDeployment(t *testing.T) {
	t.Parallel()

	gh := newFakeGitHub(t)
	cert, fingerprint := testClient(t)

	broker := newTestBroker(t, gh, []config.BrokerDeployment{
		{ID: deploymentA, Clients: []string{fingerprint}, Installations: []config.BrokerInstallation{org(22)}},
	})

	identity := func(id string, found bool, err error) func() (string, bool, error) {
		return func() (string, bool, error) { return id, found, err }
	}

	for name, tc := range map[string]struct {
		identity func() (string, bool, error)
		want     error
	}{
		"another deployment's": {identity(deploymentB, true, nil), github.ErrTokenRefused},
		"its own":              {identity(deploymentA, true, nil), nil},
		"not minted yet":       {identity("", false, nil), nil},
		"could not be read":    {identity("", false, errors.New("the disk went away")), github.ErrTokenUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := broker.client(t, cert, tc.identity).Tokens(github.OrganizationTarget("acme"), appID, 22).
				InstallationToken(t.Context())
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// THE HELD TOKEN RIDES OUT THE BROKER, and only silence does: a definite refusal is
// reported at once however long the old token has left.
func TestAHeldTokenRidesOutABrokerThatDoesNotAnswer(t *testing.T) {
	t.Parallel()

	status := atomic.Int32{}
	status.Store(http.StatusOK)

	var expires atomic.Int64

	expires.Store(time.Now().Add(3 * time.Minute).Unix())

	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if code := int(status.Load()); code != http.StatusOK {
			w.WriteHeader(code)
			answerJSON(w, errorResponse{Code: codeUnavailable, Message: "no"})

			return
		}

		answerJSON(w, tokenResponse{
			Deployment: deploymentA, Token: "held",
			ExpiresAt: time.Unix(expires.Load(), 0).UTC().Format(time.RFC3339),
		})
	}))
	t.Cleanup(broker.Close)

	client := NewClient(ClientOptions{URL: broker.URL})
	source := client.Tokens(github.OrganizationTarget("acme"), appID, 22)

	token, err := source.InstallationToken(t.Context())
	if err != nil || token.Bearer() != "held" {
		t.Fatalf("first token: %v, %v", token.Bearer(), err)
	}

	// INSIDE ITS REFRESH WINDOW, the broker silent: the held token is still good.
	status.Store(http.StatusServiceUnavailable)

	token, err = source.InstallationToken(t.Context())
	if err != nil || token.Bearer() != "held" {
		t.Fatalf("a held token did not ride out the outage: %v, %v", token.Bearer(), err)
	}

	// A DEFINITE REFUSAL IS NOT RIDDEN OUT, AND THE TOKEN GOES WITH IT: the silence after
	// it hands nothing back, with minutes left on what it held.
	status.Store(http.StatusForbidden)

	if _, err := source.InstallationToken(t.Context()); !errors.Is(err, github.ErrTokenRefused) {
		t.Fatalf("a refusal answered %v, want it reported", err)
	}

	status.Store(http.StatusServiceUnavailable)

	if token, err := source.InstallationToken(t.Context()); !errors.Is(err, github.ErrTokenUnavailable) {
		t.Fatalf("silence after a refusal answered %q, %v; the refused token came back", token.Bearer(), err)
	}

	// AND A TOKEN PAST ITS LAST USABLE MINUTE IS NOT HANDED OUT.
	state, ok := source.(brokerTokens)
	if !ok {
		t.Fatalf("the source is a %T", source)
	}

	state.token = github.NewToken("held", time.Now().Add(30*time.Second), nil)

	if _, err := source.InstallationToken(t.Context()); !errors.Is(err, github.ErrTokenUnavailable) {
		t.Fatalf("a nearly expired token with the broker silent answered %v", err)
	}
}

// AN ANSWER THAT IS NOT A TOKEN IS COULD-NOT-TELL, and its bytes are not quoted, since
// they may hold the token the parse failed on.
func TestAnAnswerThatIsNotATokenIsCouldNotTell(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"not json":         `<html>secret-looking-token</html>`,
		"no token":         `{"deployment":"` + deploymentA + `","expires_at":"2099-01-01T00:00:00Z"}`,
		"no expiry":        `{"deployment":"` + deploymentA + `","token":"secret-looking-token"}`,
		"a bad expiry":     `{"deployment":"` + deploymentA + `","token":"secret-looking-token","expires_at":"soon"}`,
		"already expiring": `{"deployment":"` + deploymentA + `","token":"secret-looking-token","expires_at":"2000-01-01T00:00:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if _, err := io.WriteString(w, body); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
				}
			}))
			t.Cleanup(broker.Close)

			_, err := NewClient(ClientOptions{URL: broker.URL}).
				Tokens(github.OrganizationTarget("acme"), appID, 22).InstallationToken(t.Context())

			if !errors.Is(err, github.ErrTokenUnavailable) {
				t.Fatalf("got %v, want %v", err, github.ErrTokenUnavailable)
			}
			if strings.Contains(err.Error(), "secret-looking-token") {
				t.Errorf("the error quotes the answer: %v", err)
			}
		})
	}
}

// A REDIRECT IS NOT FOLLOWED: the question would go somewhere the operator did not name.
func TestAClientDoesNotFollowARedirect(t *testing.T) {
	t.Parallel()

	var elsewhere atomic.Int32

	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	t.Cleanup(other.Close)

	broker := httptest.NewServer(http.RedirectHandler(other.URL, http.StatusTemporaryRedirect))
	t.Cleanup(broker.Close)

	_, err := NewClient(ClientOptions{URL: broker.URL}).Tokens(github.OrganizationTarget("acme"), appID, 22).
		InstallationToken(t.Context())
	if !errors.Is(err, github.ErrTokenUnavailable) {
		t.Fatalf("got %v, want %v", err, github.ErrTokenUnavailable)
	}

	if elsewhere.Load() != 0 {
		t.Fatal("the client followed the redirect")
	}
}

// EVERY RENDERING OF A SOURCE IS REDACTED, the way every other credential's is.
func TestATokenSourceIsRedactedOnEveryRenderingPath(t *testing.T) {
	t.Parallel()

	source := brokerTokens{&brokerState{token: github.NewToken("cached-installation-secret", time.Now(), nil)}}

	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var logged bytes.Buffer

	slog.New(slog.NewJSONHandler(&logged, nil)).Info("source", "value", source)

	for name, got := range map[string]string{
		"%v":   fmt.Sprintf("%v", source), //nolint:gocritic // the verb's own path is what is under test
		"%+v":  fmt.Sprintf("%+v", source),
		"%#v":  fmt.Sprintf("%#v", source),
		"%s":   fmt.Sprintf("%s", source), //nolint:gocritic // the verb's own path is what is under test
		"%x":   fmt.Sprintf("%x", source),
		"json": string(encoded),
		"slog": logged.String(),
	} {
		if strings.Contains(got, "cached-installation-secret") ||
			strings.Contains(got, hex.EncodeToString([]byte("cached-installation-secret"))) {
			t.Errorf("%s leaked the token: %s", name, got)
		}
		if !strings.Contains(got, "redacted") {
			t.Errorf("%s does not say it redacted anything: %s", name, got)
		}
	}
}

// A HOST'S KEY IS WRITTEN ONCE, its owner's alone, and read back to the fingerprint it
// was made with.
func TestAClientKeyIsWrittenOnceAndReadBackToItsFingerprint(t *testing.T) {
	t.Parallel()

	// A DIRECTORY THAT IS NOT THERE IS NOT MADE: a root run would leave one the service
	// account cannot enter.
	if _, err := CreateClientKey(filepath.Join(t.TempDir(), "absent", "client.key")); err == nil ||
		!strings.Contains(err.Error(), "service account can enter") {
		t.Fatalf("a key under no directory answered %v", err)
	}

	path := filepath.Join(t.TempDir(), "client.key")

	made, err := CreateClientKey(path)
	if err != nil {
		t.Fatalf("CreateClientKey: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %04o, want 0600", info.Mode().Perm())
	}

	if _, err := CreateClientKey(path); !errors.Is(err, ErrClientKeyExists) {
		t.Fatalf("a second create answered %v, want %v", err, ErrClientKeyExists)
	}

	key, err := ReadClientKey(path)
	if err != nil {
		t.Fatalf("ReadClientKey: %v", err)
	}

	read, err := Fingerprint(key)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if read != made {
		t.Fatalf("read back %s, made %s", read, made)
	}

	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, err := ReadClientKey(path); err == nil || !strings.Contains(err.Error(), "readable beyond its owner") {
		t.Fatalf("a group-readable key answered %v", err)
	}
}

// THE BROKER'S ANSWERS CARRY NO KEY, on any route, because no route has one to give.
func TestNoAnswerOfTheBrokerCarriesAKey(t *testing.T) {
	t.Parallel()

	gh := newFakeGitHub(t)
	cert, fingerprint := testClient(t)

	broker := newTestBroker(t, gh, []config.BrokerDeployment{
		{ID: deploymentA, Clients: []string{fingerprint}, Installations: []config.BrokerInstallation{org(22)}},
	})

	roots := x509.NewCertPool()
	roots.AddCert(broker.Certificate())

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{cert}, RootCAs: roots, MinVersion: tls.VersionTLS13,
	}}}

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	for _, ask := range []struct{ method, path, body string }{
		{http.MethodGet, "/", ""},
		{http.MethodGet, "/key", ""},
		{http.MethodGet, tokenRoute, ""},
		{http.MethodPost, "/v1/key", `{"app_id":11}`},
		{http.MethodPost, tokenRoute, `{"app_id":11,"installation_id":22,"target":"acme"}`},
		{http.MethodPost, tokenRoute, `not json`},
		{http.MethodPost, tokenRoute, `{"app_id":11,"installation_id":22,"target":"acme","give_me":"the key"}`},
	} {
		req, err := http.NewRequestWithContext(ctx, ask.method, broker.URL+ask.path, strings.NewReader(ask.body))
		if err != nil {
			t.Fatalf("request: %v", err)
		}

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", ask.method, ask.path, err)
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if err != nil {
			t.Fatalf("read: %v", err)
		}

		if bytes.Contains(body, []byte("PRIVATE KEY")) || bytes.Contains(body, broker.key) {
			t.Errorf("%s %s answered with a key: %s", ask.method, ask.path, body)
		}
	}
}

// A CALLER GOING AWAY SAYS NOTHING ABOUT THE INSTALLATION. A proof already held stands,
// so an outage after a hung-up caller still has it to fall back on.
func TestACallerGoingAwayKeepsAnInstallationsProof(t *testing.T) {
	t.Parallel()

	gh := newFakeGitHub(t)
	cert, fingerprint := testClient(t)

	broker := newTestBroker(t, gh, []config.BrokerDeployment{
		{ID: deploymentA, Clients: []string{fingerprint}, Installations: []config.BrokerInstallation{org(22)}},
	})

	inst := broker.server.installations[keyOf(deploymentA, appID, 22, "acme")]

	now := time.Now()
	broker.server.now = func() time.Time { return now }

	if _, err := broker.client(t, cert, nil).Tokens(github.OrganizationTarget("acme"), appID, 22).
		InstallationToken(t.Context()); err != nil {
		t.Fatalf("first token: %v", err)
	}

	proved := inst.verified
	if proved.IsZero() {
		t.Fatal("the first token proved nothing, so this has nothing to keep")
	}

	now = now.Add(verifyEvery + time.Minute)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	req := httptest.NewRequestWithContext(ctx, http.MethodPost, tokenRoute, http.NoBody)

	broker.server.verify = func(*http.Request, *installation) error { return context.Canceled }

	if status, code, _ := broker.server.proved(req, inst); status != http.StatusServiceUnavailable || code != codeUnavailable {
		t.Errorf("a caller gone answered %d %s", status, code)
	}

	if !inst.verified.Equal(proved) {
		t.Errorf("a caller gone moved the proof to %v from %v", inst.verified, proved)
	}
}

// A BASE WITH A TRAILING SLASH STILL REACHES THE ROUTE, because config admits one.
func TestABrokerURLWithATrailingSlashIsTheSameBroker(t *testing.T) {
	t.Parallel()

	gh := newFakeGitHub(t)
	cert, fingerprint := testClient(t)

	broker := newTestBroker(t, gh, []config.BrokerDeployment{
		{ID: deploymentA, Clients: []string{fingerprint}, Installations: []config.BrokerInstallation{org(22)}},
	})

	roots := x509.NewCertPool()
	roots.AddCert(broker.Certificate())

	client := NewClient(ClientOptions{URL: broker.URL + "/", Certificate: cert, Roots: roots})

	if _, err := client.Tokens(github.OrganizationTarget("acme"), appID, 22).InstallationToken(t.Context()); err != nil {
		t.Fatalf("a base with a trailing slash: %v", err)
	}
}

// answerJSON is a fake's answer, which reports a failure to encode as the status a
// client would read it by.
func answerJSON(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
