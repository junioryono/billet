package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/codebuild"
	"github.com/junioryono/billet/internal/state"
)

// fakeParameterStore is a Parameter Store that lists one level under a path and
// deletes by name.
type fakeParameterStore struct {
	t *testing.T

	mu     sync.Mutex
	params map[string]string
	// writtenAt is what every parameter reports as its LastModifiedDate.
	writtenAt time.Time
}

// field reads a request field the fake expects to be a string, naming the missing
// case rather than leaving it to comma-ok's zero value.
func field(in map[string]any, name string) string {
	s, ok := in[name].(string)
	if !ok {
		return ""
	}

	return s
}

// answer writes a JSON response, and REPORTS a failure to: a fake that silently
// writes nothing makes the client's own timeout the observed behaviour.
func (f *fakeParameterStore) answer(w http.ResponseWriter, v map[string]any) {
	body, err := json.Marshal(v)
	if err != nil {
		f.t.Errorf("encode a response: %v", err)

		return
	}

	if _, err := w.Write(body); err != nil {
		f.t.Errorf("write a response: %v", err)
	}
}

func (f *fakeParameterStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var in map[string]any
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, `{"__type":"SerializationException"}`, http.StatusBadRequest)

		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	w.Header().Set("Content-Type", "application/x-amz-json-1.1")

	switch target := r.Header.Get("X-Amz-Target"); {
	case strings.HasSuffix(target, ".GetParametersByPath"):
		prefix := strings.TrimSuffix(field(in, "Path"), "/") + "/"

		var names []string

		for name := range f.params {
			if rel, ok := strings.CutPrefix(name, prefix); ok && rel != "" && !strings.Contains(rel, "/") {
				names = append(names, name)
			}
		}

		slices.Sort(names)

		params := make([]map[string]any, 0, len(names))
		for _, name := range names {
			params = append(params, map[string]any{
				"Name": name, "Type": "SecureString", "Value": "AQICAHh",
				// Written long ago by AWS's clock, so the parameter's own age proof
				// holds and the ledger's answer is what this test exercises.
				"LastModifiedDate": float64(f.writtenAt.Unix()),
			})
		}

		f.answer(w, map[string]any{"Parameters": params})

	case strings.HasSuffix(target, ".DeleteParameter"):
		name := field(in, "Name")
		if _, ok := f.params[name]; !ok {
			http.Error(w, `{"__type":"ParameterNotFound"}`, http.StatusBadRequest)

			return
		}

		delete(f.params, name)

		f.answer(w, map[string]any{})

	default:
		http.Error(w, `{"__type":"InvalidAction"}`, http.StatusBadRequest)
	}
}

func (f *fakeParameterStore) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, 0, len(f.params))
	for name := range f.params {
		out = append(out, name)
	}

	slices.Sort(out)

	return out
}

// sweepTestTier is a tier a codebuild node can serve.
func sweepTestTier() config.Tier {
	return config.Tier{
		Label:       "billet-4vcpu-codebuild",
		Provider:    config.ProviderCodeBuild,
		VCPU:        4,
		Memory:      7 * config.GiB,
		Image:       "aws/codebuild/amazonlinux-x86_64-standard:5.0",
		GuestOS:     config.GuestLinux,
		Command:     []string{"./run.sh"},
		Trust:       config.WorkloadTrusted,
		Workflows:   []string{"acme/cloud/.github/workflows/ci.yml@refs/heads/main"},
		RunnerGroup: "trusted",
	}
}

// THE DRIVER RECORDS EVERY PATH, CONTINUES PAST ONE THAT FAILS, AND HANDS THE
// SWEEPER THE LEDGER'S ANSWER AND NOTHING ELSE.
//
// Two paths are registered. One holds a registration for a lease the ledger closed
// long ago and one for a lease it has never seen; the other's sweeper refuses to be
// built. Both passes are recorded, the failing one with its reason, and the pass on
// the first path is not skipped because the second failed.
func TestTheControllerSweepRecordsEveryPathAndContinuesPastAFailingOne(t *testing.T) {
	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	var (
		clockMu sync.Mutex
		now     = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	)

	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()

		return now
	}

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 256, MaxMemory: 1024 * config.GiB},
		[]config.Tier{sweepTestTier()}, alloc.WithClock(clock))
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	shapes := []config.RemoteShape{{Type: "BUILD_GENERAL1_MEDIUM", VCPU: 4, Memory: 7 * config.GiB, PriceUSDPerHour: 10000}}

	for _, n := range []struct{ name, path string }{
		{"cb-a", "/billet/a/jit"}, {"cb-b", "/billet/b/jit"}, {"cb-old", ""},
	} {
		region := "us-west-2"
		if n.path == "" {
			region = ""
		}

		if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
			Name: n.name, Provider: config.ProviderCodeBuild,
			VCPU: 64, Memory: 256 * config.GiB, EC2Shapes: shapes,
			CodeBuildJITPath: n.path, CodeBuildRegion: region,
		}); err != nil {
			t.Fatalf("register %s: %v", n.name, err)
		}
	}

	// A lease closed at t0, and a clock that then moves past the service window.
	lease, err := a.Reserve(t.Context(), sweepTestTier().Label)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := a.Release(t.Context(), lease.ID, lease.Epoch, alloc.PhaseFailed); err != nil {
		t.Fatalf("Release: %v", err)
	}

	clockMu.Lock()
	now = now.Add(codebuild.ServiceInventoryWindow + time.Hour)
	clockMu.Unlock()

	dead := "/billet/a/jit/" + provider.InstanceName(lease.ID)
	unknown := "/billet/a/jit/" + provider.InstanceName("00000000000000000000000000000000")

	store := &fakeParameterStore{
		t:         t,
		params:    map[string]string{dead: "x", unknown: "y"},
		writtenAt: now.Add(-codebuild.ServiceInventoryWindow - 2*time.Hour),
	}
	srv := httptest.NewServer(store)
	t.Cleanup(srv.Close)

	sweep := newControllerCredentialSweep(a, db,
		awscreds.Static{AccessKeyID: "AKID", SecretAccessKey: "s"}, slog.Default())
	sweep.now = clock

	sweep.newSweeper = func(region, path string) (*codebuild.RegistrationSweeper, error) {
		if path == "/billet/b/jit" {
			return nil, errors.New("scripted refusal of path b")
		}

		s, err := codebuild.NewRegistrationSweeper(region, path,
			awscreds.Static{AccessKeyID: "AKID", SecretAccessKey: "s"},
			codebuild.SweepWithHTTPClient(srv.Client()), codebuild.SweepWithClock(clock))
		if err != nil {
			return nil, err
		}

		codebuild.SetSweeperSSMEndpointForTest(s, srv.URL+"/")

		return s, nil
	}

	err = sweep.SweepStagedCredentials(t.Context())
	if err == nil || !strings.Contains(err.Error(), "scripted refusal of path b") {
		t.Fatalf("the failing path's error was not returned: %v", err)
	}

	// THE PROVED-DEAD ONE IS GONE AND THE UNKNOWN ONE IS NOT.
	if got := store.names(); !slices.Equal(got, []string{unknown}) {
		t.Fatalf("parameters after the sweep = %v, want only the one the ledger cannot place", got)
	}

	recs, err := db.CredentialSweeps(t.Context())
	if err != nil {
		t.Fatalf("CredentialSweeps: %v", err)
	}

	if len(recs) != 2 {
		t.Fatalf("%d passes recorded, want one per path: %+v", len(recs), recs)
	}

	passA, passB := recs[0], recs[1]

	if passA.Path != "/billet/a/jit" || passA.Removed != 1 || passA.Unaccounted != 1 || passA.Error != "" {
		t.Errorf("path a recorded as %+v", passA)
	}

	if passB.Path != "/billet/b/jit" || !strings.Contains(passB.Error, "scripted refusal") {
		t.Errorf("path b recorded as %+v; a path whose sweeper could not be built must say so", passB)
	}

	// AND STATUS SAYS ALL OF IT, including the host nothing sweeps after.
	out := capture(t, func() { printCredentialSweeps(t.Context(), a, db) })

	for _, want := range []string{
		"/billet/a/jit (us-west-2): 1 registration(s) removed in total",
		"1 naming leases this ledger has never seen",
		"the last pass stopped: scripted refusal of path b",
		"node cb-old registered without its registration path",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status does not say %q:\n%s", want, out)
		}
	}
}
