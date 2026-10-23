package node

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// lastNpm waits for the observer to have been told an npm verdict and answers
// the latest; a verdict a request in flight left for later arrives after its
// handler returns.
func lastNpm(t *testing.T, observer *recordingObserver) alloc.BuildCache {
	t.Helper()

	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		calls := observer.recorded()
		for i := len(calls) - 1; i >= 0; i-- {
			if calls[i].obs.Npm != "" {
				return calls[i].obs.Npm
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no npm verdict was ever reported")
		}
	}
}

func npmSession(service *CacheService, credentials CacheCredentials) *cacheSession {
	service.mu.Lock()
	defer service.mu.Unlock()

	return service.byToken[credentials.Token]
}

// AN EARLIER COLD REQUEST DOES NOT SETTLE THE NPM CACHE WHILE A WARM ONE IS
// STILL IN FLIGHT: the first verdict is the one kept, so settlement leaves it
// to the last request, which reports warm.
func TestAnEarlierColdRequestDoesNotSettleTheNpmCacheBeforeAWarmOneEnds(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		serveText(t, w, `{"name":"held"}`)
	})
	service, credentials := npmService(t, registry)
	observer := &recordingObserver{}
	service.SetCacheObserver(observer)

	npmGet(t, service, credentials, npmRequestLine("/held"))
	assertNpmQuiet(t, service.npmCache())
	if got := noted(service, credentials); got != alloc.BuildCacheCold {
		t.Fatalf("the first fetch noted %q, want cold", got)
	}

	store := service.npmCache().store
	var unlockOnce sync.Once
	store.mu.Lock()
	unlock := func() { unlockOnce.Do(store.mu.Unlock) }
	t.Cleanup(unlock)
	conn, _ := npmTunnel(t, service, credentials, npmRegistryHost)
	if _, err := conn.Write([]byte("HEAD /held HTTP/1.1\r\nHost: registry.npmjs.org\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	session := npmSession(service, credentials)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		session.mu.Lock()
		inflight := session.npmInflight
		session.mu.Unlock()
		if inflight > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the HEAD never reached the cache")
		}
	}

	if err := service.Close(t.Context(), "billet-lease-npm"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, call := range observer.recorded() {
		if call.obs.Npm != "" {
			t.Fatalf("the session settled the npm cache %q while a request was in flight", call.obs.Npm)
		}
	}
	unlock()

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set a read deadline: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodHead})
	if err != nil {
		t.Fatalf("the HEAD was never answered: %v", err)
	}
	_ = resp.Body.Close()
	if got := lastNpm(t, observer); got != alloc.BuildCacheWarm {
		t.Fatalf("the npm cache was reported %q after the warm request ended, want warm", got)
	}
}

// A GUEST THAT REACHED THE NPM CACHE AND COULD NOT TALK TO IT, its handshake
// refused because it does not trust the node's authority, was not served: the
// cache is unavailable for the job, not unused.
func TestAGuestThatRefusesTheNodesCertificateFindsTheNpmCacheUnavailable(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		serveText(t, w, "{}")
	})
	service, credentials := npmService(t, registry)
	observer := &recordingObserver{}
	service.SetCacheObserver(observer)

	proxyServer := httptest.NewServer(service)
	t.Cleanup(proxyServer.Close)
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp", strings.TrimPrefix(proxyServer.URL, "http://"))
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	authority := npmRegistryHost + ":443"
	credential := base64.StdEncoding.EncodeToString([]byte(actionsProxyUser + ":" + credentials.Token))
	if _, err := conn.Write([]byte("CONNECT " + authority + " HTTP/1.1\r\nHost: " + authority +
		"\r\nProxy-Authorization: Basic " + credential + "\r\n\r\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	reader := bufio.NewReader(conn)
	if status, err := reader.ReadString('\n'); err != nil || !strings.Contains(status, " 200 ") {
		t.Fatalf("the CONNECT was answered %q, %v", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
	}
	// THE SYSTEM ROOTS ONLY, as a client the node's authority never reached.
	client := tls.Client(conn, &tls.Config{MinVersion: tls.VersionTLS13, ServerName: npmRegistryHost})
	if err := client.HandshakeContext(t.Context()); err == nil {
		t.Fatal("a client without the node's authority completed its handshake")
	}
	_ = conn.Close()

	session := npmSession(service, credentials)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		session.mu.Lock()
		inflight := session.npmInflight
		session.mu.Unlock()
		if inflight == 0 && noted(service, credentials) != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the refused handshake was never noted")
		}
	}
	if err := service.Close(t.Context(), "billet-lease-npm"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := lastNpm(t, observer); got != alloc.BuildCacheUnavailable {
		t.Fatalf("the npm cache was reported %q, want unavailable", got)
	}
}

// AN OBSERVATION WHOSE REPORT FAILED IS SENT AGAIN by the next settlement,
// although nothing in it changed, before the session's record goes.
func TestAFailedCacheReportIsResentByTheNextSettlement(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		serveText(t, w, "{}")
	})
	service, credentials := npmService(t, registry)
	observer := &recordingObserver{refuse: 1}
	service.SetCacheObserver(observer)

	npmGet(t, service, credentials, npmRequestLine("/resent"))
	assertNpmQuiet(t, service.npmCache())
	service.SettleObservation(t.Context(), "billet-lease-npm")
	if calls := observer.recorded(); len(calls) != 1 {
		t.Fatalf("the first settlement made %d reports, want the one that failed", len(calls))
	}
	if err := service.Close(t.Context(), "billet-lease-npm"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	calls := observer.recorded()
	if len(calls) < 2 || calls[len(calls)-1].obs.Npm != alloc.BuildCacheCold {
		t.Fatalf("after the failed report, the reports were %+v; want the cold verdict sent again", calls)
	}
}

// AN UNREADABLE KILL SWITCH IS NOT A REFUSAL: a cache it keeps from the job is
// unavailable, and disabled only when the switch said no.
func TestAnUnreadableKillSwitchMakesACacheUnavailableNotDisabled(t *testing.T) {
	t.Parallel()

	answers := map[string]struct {
		allowed bool
		err     error
		want    alloc.BuildCache
	}{
		"refused":    {allowed: false, want: alloc.BuildCacheDisabled},
		"unreadable": {err: errors.New("the control plane could not be reached"), want: alloc.BuildCacheUnavailable},
	}

	for name, answer := range answers {
		t.Run("npm "+name, func(t *testing.T) {
			t.Parallel()

			registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				w.Header().Set("Cache-Control", "public, max-age=300")
				serveText(t, w, "{}")
			})
			policy := cachePolicyFunc(func(_ context.Context, kind config.CacheKind, _, _ string) (bool, error) {
				if kind == config.CacheNpm {
					return answer.allowed, answer.err
				}

				return true, nil
			})
			service, credentials := npmServiceWith(t, registry, policy, CacheSessionScope{
				Trust: provider.TrustUntrusted, Owner: "acme", Repository: "api",
				Cache: &config.CacheSpec{Npm: true, Publish: config.CachePublishDefaultBranch, Owner: "acme",
					Repository: "api"},
			})
			if resp, _ := npmGet(t, service, credentials, npmRequestLine("/switched")); resp.StatusCode != http.StatusOK {
				t.Fatalf("the spliced read = %d", resp.StatusCode)
			}
			assertNpmQuiet(t, service.npmCache())
			if got := noted(service, credentials); got != answer.want {
				t.Fatalf("the npm cache noted %q, want %q", got, answer.want)
			}
		})

		t.Run("git "+name, func(t *testing.T) {
			t.Parallel()

			service, session := switchedSession(t, config.CacheGit, answer.allowed, answer.err)
			allowed := service.gitAllowed(t.Context(), session)
			session.mu.Lock()
			got := session.building[config.CacheGit]
			session.mu.Unlock()
			if allowed || got != answer.want {
				t.Fatalf("the git cache was allowed %v and noted %q, want refused and %q", allowed, got, answer.want)
			}
		})

		t.Run("content-addressed "+name, func(t *testing.T) {
			t.Parallel()

			service, session := switchedSession(t, config.CacheGo, answer.allowed, answer.err)
			session.mu.Lock()
			_, err := service.casVolume(t.Context(), session, config.CacheGo)
			session.mu.Unlock()
			want := errCacheOff
			if answer.err != nil {
				want = errCachePolicyUnknown
			}
			if !errors.Is(err, want) {
				t.Fatalf("casVolume answered %v, want %v", err, want)
			}
		})
	}
}

// switchedSession is a cache service and a session that names a repository,
// whose kill switch answers kind with allowed and err and every other kind yes.
func switchedSession(t *testing.T, kind config.CacheKind, allowed bool, err error) (*CacheService, *cacheSession) {
	t.Helper()

	service, _, _, _ := casService(t, provider.TrustTrusted, goBazelCache(), &fakeCacheStore{})
	service.SetCachePolicy(cachePolicyFunc(func(_ context.Context, k config.CacheKind, _, _ string) (bool, error) {
		if k == kind {
			return allowed, err
		}

		return true, nil
	}))
	spec := goBazelCache()
	spec.Git = config.CacheSetting{Enabled: true}
	spec.Owner, spec.Repository = "acme", "api"
	instance := provider.InstanceName("lease-switched")
	if _, err := service.PrepareScoped(instance, CacheSessionScope{
		Trust: provider.TrustTrusted, LeaseID: "lease-switched", Epoch: 1, Owner: "acme", Repository: "api",
		Cache: spec,
	}); err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}
	session := service.sessionOf(instance)

	return service, session
}
