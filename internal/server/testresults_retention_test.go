package server

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// THE CONTROL PLANE DELETES TEST RESULTS PAST THEIR RETENTION, as a background loop in
// Run: what the allocator's tests prove works is used.
func TestTheControlPlaneRunsTheTestResultsRetention(t *testing.T) {
	t.Parallel()

	file, err := parser.ParseFile(token.NewFileSet(), "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	started := false
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "Run" || fd.Recv == nil {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Background" {
				return true
			}
			for _, arg := range call.Args {
				if s, ok := arg.(*ast.SelectorExpr); ok && s.Sel.Name == "retireTestResults" {
					started = true
				}
			}
			return true
		})
	}
	if !started {
		t.Error("(*Server).Run starts no background loop running retireTestResults")
	}
}

// THE CONFIGURED RETENTION AND BOUND REACH THE CONTROL PLANE, which is the difference
// between a setting and a comment.
func TestTheConfiguredTestResultsRetentionReachesTheControlPlane(t *testing.T) {
	t.Parallel()

	opts, err := OptionsFromConfig(&config.Config{Server: config.ServerConfig{
		TestResultsRetention: "48h", TestResultsMaxBytes: 2 * config.MiB,
	}})
	if err != nil {
		t.Fatalf("OptionsFromConfig: %v", err)
	}
	s := &Server{}
	for _, opt := range opts {
		opt(s)
	}
	if s.testRetention != 48*time.Hour || s.testBound != int64(2*config.MiB) {
		t.Errorf("the control plane has %s and %d bytes, want 48h and 2MiB", s.testRetention, s.testBound)
	}
}

// seedTestedLease keeps test results of data for the runner's lease.
func seedTestedLease(t *testing.T, p *runnerlessPool, leaseID string, data []byte) {
	t.Helper()

	lease, err := p.a.Lease(t.Context(), leaseID)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if err := p.a.RecordGuestReport(t.Context(), lease.ID, lease.Epoch, alloc.GuestReport{
		Codec: alloc.GuestReportCodec, TestsCodec: alloc.TestResultsCodec, Tests: data,
	}); err != nil {
		t.Fatalf("RecordGuestReport: %v", err)
	}
}

// A PASS DELETES WHAT IS PAST THE RETENTION AND WHAT IS PAST THE BOUND, each from its
// own read, and leaves the rest.
func TestARetentionPassDeletesTestResultsPastEitherLimit(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11)
	runner := p.launchedFor11(t)
	seedTestedLease(t, p, runner.LeaseID, []byte{1, 2, 3})

	held := func() bool {
		t.Helper()

		_, err := p.a.LeaseTestResults(t.Context(), runner.LeaseID)
		if err != nil && !errors.Is(err, alloc.ErrLeaseNotFound) {
			t.Fatalf("LeaseTestResults: %v", err)
		}

		return err == nil
	}

	s := New(p.a, &fakeProvisioner{}, nil, "test", nil, WithTestResultsRetention(time.Hour, int64(config.MiB)))
	s.retireTestResultsOnce(t.Context(), time.Now().Add(30*time.Minute))
	if !held() {
		t.Fatal("a pass inside the retention and the bound deleted the row")
	}

	s.retireTestResultsOnce(t.Context(), time.Now().Add(2*time.Hour))
	if held() {
		t.Fatal("a pass past the retention left the row")
	}

	seedAgain := newRunnerlessPool(t, job11)
	again := seedAgain.launchedFor11(t)
	seedTestedLease(t, seedAgain, again.LeaseID, []byte{1, 2, 3})
	small := New(seedAgain.a, &fakeProvisioner{}, nil, "test", nil, WithTestResultsRetention(time.Hour, 1))
	small.retireTestResultsOnce(t.Context(), time.Now())
	if _, err := seedAgain.a.LeaseTestResults(t.Context(), again.LeaseID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Errorf("a pass past the bound left the row: %v", err)
	}
}
