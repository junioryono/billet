package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/regularfile"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/state"
)

// N4 of PR 6b commit 3: CONFIGURATION REPLACEMENT REACHES NOTHING THE PROCESS
// PUBLISHES, through the production assembly. The node's client is built from
// configuration A the way the command builds it (newNodeClientFor), the loop
// registers against a fake plane through a recording transport and publishes,
// configuration B is installed at the path, the same loop re-registers, and
// the request URL, the accessor and the newly published record all still say
// A, while the inspector over the installed file reports B installed beside a
// known registration naming A.

type stubCompute struct{}

func (stubCompute) Launch(context.Context, *alloc.Lease, *nodeapi.TierSpec, server.Job) error {
	return errors.New("no launches here")
}
func (stubCompute) Destroy(context.Context, int64) error                     { return nil }
func (stubCompute) Recover(context.Context) error                            { return nil }
func (stubCompute) Instances(context.Context) ([]string, error)              { return nil, nil }
func (stubCompute) Sweep(context.Context) error                              { return nil }
func (stubCompute) KeepAlive(ctx context.Context)                            { <-ctx.Done() }
func (stubCompute) Tend(context.Context) error                               { return nil }
func (stubCompute) AssumeCustody(context.Context, *alloc.Lease, int64) error { return nil }
func (stubCompute) Holding() bool                                            { return false }
func (stubCompute) Superseded()                                              {}
func (stubCompute) DestroyCompleted(context.Context, int64, string) error    { return nil }

type stubJIT struct{}

func (stubJIT) Describe(context.Context, string, string) (*nodeplane.JITSet, []string, error) {
	return nil, nil, nil
}

func (stubJIT) JITConfig(context.Context, int, string, string) (nodeplane.JITRegistration, error) {
	return nil, errors.New("no github in this test")
}

func (stubJIT) RemoveRunner(context.Context, int64, string) error { return nil }

func (stubJIT) RecoverRunner(context.Context, string) (nodeplane.JITRunnerRecovery, error) {
	return nodeplane.JITRunnerRecovery{}, nil
}

type stubStore struct{}

func (stubStore) Bind(context.Context, string, int64, string) error         { return nil }
func (stubStore) Advance(context.Context, string, int64, alloc.Phase) error { return nil }
func (stubStore) MarkDeregistered(context.Context, string) error            { return nil }
func (stubStore) Heartbeat(context.Context, string, int64) error            { return nil }
func (stubStore) MarkFailure(context.Context, string, int64, string) error  { return nil }
func (stubStore) Resize(context.Context, string, int64, string, int, config.ByteSize) error {
	return nil
}
func (stubStore) Release(context.Context, string, int64, alloc.Phase) error { return nil }
func (stubStore) RecordCacheObservation(context.Context, string, int64, alloc.CacheObservation) error {
	return nil
}
func (stubStore) Lease(context.Context, string) (*alloc.Lease, error) { return nil, nil }
func (stubStore) QuarantinedLeaseIDs(context.Context, string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (stubStore) EndedLeaseNode(context.Context, string) (string, error) {
	return "", alloc.ErrLeaseNotFound
}

func (stubStore) LaunchedLeaseIDs(context.Context, string) (map[string]bool, error) {
	return nil, nil
}

// recordingRoundTripper records every request URL and answers in memory
// through the plane, whatever host the URL names.
type recordingRoundTripper struct {
	handler http.Handler
	answer  func(path string) (int, string, bool)
	mu      sync.Mutex
	urls    []string
}

func (rt *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.urls = append(rt.urls, req.URL.String())
	rt.mu.Unlock()

	rec := httptest.NewRecorder()

	if rt.answer != nil {
		if code, body, ok := rt.answer(req.URL.Path); ok {
			rec.Header().Set("Content-Type", "application/json")
			rec.WriteHeader(code)
			_, _ = rec.WriteString(body) //nolint:errcheck // a recorder's buffer never fails

			return rec.Result(), nil
		}
	}

	rt.handler.ServeHTTP(rec, req)

	return rec.Result(), nil
}

func (rt *recordingRoundTripper) registrations() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	var out []string

	for _, u := range rt.urls {
		if strings.HasSuffix(u, "/v1/register") {
			out = append(out, u)
		}
	}

	return out
}

// The rooted spelling of a DNS name (the fixture list's second case) cannot be
// a certless node's server_addr, which validation holds to a literal loopback
// address, so that case is the representation's own (endpoint_test.go, N2).
func TestAConfigurationInstalledAfterTheStartChangesNothingTheNodePublishes(t *testing.T) {
	for name, addressB := range map[string]string{
		"another address": "127.0.0.1:7719",
	} {
		t.Run(name, func(t *testing.T) {
			f := newInspectFixture(t)
			nodeState := filepath.Join(f.dir, "node-state")
			deployment, err := state.DeploymentID(nodeState)
			if err != nil {
				t.Fatal(err)
			}

			// A dials a loopback address, as a certless node must; B is another.
			addressA := "127.0.0.1:7717"

			configFor := func(addr string) string {
				return "node:\n  name: node-a\n  server_addr: " + addr + "\n  provider: docker\n  state_dir: " + nodeState +
					"\ntiers:\n  - label: billet-2vcpu\n    provider: docker\n    vcpu: 2\n    memory: 8GiB\n    image: ubuntu:24.04\n"
			}

			f.writeConfig(t, configFor(addressA))

			cfgA, err := config.Load(f.configPath)
			if err != nil {
				t.Fatal(err)
			}

			// THE PRODUCTION CONSTRUCTOR, then the recording transport.
			client, err := newNodeClientFor(cfgA, nil)
			if err != nil {
				t.Fatal(err)
			}

			log := slog.New(slog.DiscardHandler)
			plane := nodeplane.New(log, deployment, time.Minute,
				nodeplane.WithCommandTimeout(5*time.Second),
				nodeplane.WithTierCatalog([]config.Tier{{
					Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
					VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
				}}))
			plane.SetPollWindowForTest(60 * time.Millisecond)

			var unregisteredOnce atomic.Bool

			rt := &recordingRoundTripper{handler: nodeplane.Handler(log, plane, stubStore{}, stubJIT{})}
			rt.answer = func(path string) (int, string, bool) {
				if strings.HasSuffix(path, "/poll") && unregisteredOnce.CompareAndSwap(true, false) {
					return http.StatusNotFound, `{"code":"` + nodeapi.CodeUnregistered + `","message":"who"}`, true
				}

				return 0, "", false
			}

			nodeclient.ReplaceTransportForTest(client, rt)

			recordDir := filepath.Join(f.dir, "registration")
			if err := os.Mkdir(recordDir, 0o750); err != nil {
				t.Fatal(err)
			}

			recordPath := filepath.Join(recordDir, "current")

			var clock atomic.Int64

			clock.Store(time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC).UnixNano())

			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)

			go func() {
				done <- nodeclient.Run(ctx, client, stubCompute{}, nodeclient.LoopOptions{
					VCPU: 4, Memory: 8 * config.GiB, Provider: config.ProviderDocker,
					GuestOS: []config.GuestOS{config.GuestLinux}, Deployment: deployment, Log: log,
					Backoff: 20 * time.Millisecond, RegistrationRecordPath: recordPath,
					Now: func() time.Time { return time.Unix(0, clock.Load()).UTC() },
				})
			}()

			t.Cleanup(func() {
				cancel()
				<-done
			})

			first := awaitRecordAt(t, recordPath, "")
			firstIno := inodeAt(t, recordPath)

			// CONFIGURATION B IS INSTALLED, the clock advanced, and the SAME loop
			// re-registers on the plane's unregistered answer.
			f.writeConfig(t, configFor(addressB))
			clock.Store(time.Unix(0, clock.Load()).Add(time.Minute).UnixNano())
			unregisteredOnce.Store(true)

			firstAt, ok := first["registered_at"].(string)
			if !ok {
				t.Fatalf("the first record's registered_at is %T, want a string", first["registered_at"])
			}

			second := awaitRecordAt(t, recordPath, firstAt)
			if inodeAt(t, recordPath) == firstIno {
				t.Error("the second publication reused the first inode")
			}

			wantA := "http://" + addressA
			if second["endpoint"] != wantA || client.Endpoint().String() != wantA {
				t.Errorf("after B was installed the record says %v and the accessor %s; want A %s", second["endpoint"], client.Endpoint(), wantA)
			}

			regs := rt.registrations()
			if len(regs) < 2 {
				t.Fatalf("the loop registered %d times, want two", len(regs))
			}

			for _, u := range regs {
				if u != wantA+"/v1/register" {
					t.Errorf("a registration went to %q, want %q", u, wantA+"/v1/register")
				}
			}

			// THE INSPECTOR over the installed file: B installed, a known
			// registration naming A. The node "runs" as the fixture's process.
			f.unitAbsent(t, "billet-server.service")
			f.unitRunning(t, "billet-node.service", "node", f.configPath, nil)
			f.process(t, []string{f.binPath, "node", "--config", f.configPath}, nil)
			f.touchBeforeStart(t, f.configPath)

			// The record the loop wrote names its own invocation (empty outside
			// systemd), so the unit's is set empty too... which the inspector
			// refuses as no invocation. The fixture rewrites the record's
			// invocation to the unit's, keeping everything else, so what is
			// judged is the endpoint the process published.
			rewriteRecordInvocation(t, recordPath, "0123456789abcdef0123456789abcdef")

			savedPath, savedOpen := registrationRecordPath, registrationOpen
			t.Cleanup(func() { registrationRecordPath, registrationOpen = savedPath, savedOpen })

			registrationRecordPath = recordPath
			registrationOpen = func(path string) (*os.File, os.FileInfo, error) {
				file, info, err := regularfile.Open(path, regularfile.Options{NoFollow: true})
				if err != nil {
					return nil, nil, err
				}

				st, ok := info.Sys().(*syscall.Stat_t)
				if !ok {
					return file, info, nil
				}

				asRoot := *st
				asRoot.Uid = 0

				return file, ownedInfo{FileInfo: info, sys: &asRoot}, nil
			}

			r := f.report(t)
			if got := mustKnown(t, "installed_endpoint", r.Host.InstalledEndpoint); got != "http://"+addressB {
				t.Errorf("installed_endpoint = %v, want B", got)
			}

			if rec := registrationOf(t, r); rec.Endpoint != wantA {
				t.Errorf("the registration reports %q, want A", rec.Endpoint)
			}
		})
	}
}

func awaitRecordAt(t *testing.T, path, prior string) map[string]any {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		if body, err := os.ReadFile(path); err == nil {
			var m map[string]any
			if err := json.Unmarshal(body, &m); err == nil {
				if at, ok := m["registered_at"].(string); ok && at != prior {
					return m
				}
			}
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("no record beyond %q appeared at %s", prior, path)

	return nil
}

func inodeAt(t *testing.T, path string) uint64 {
	t.Helper()

	return inodeOfPath(t, path)
}

func rewriteRecordInvocation(t *testing.T, path, invocation string) {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}

	m["invocation_id"] = invocation

	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}
