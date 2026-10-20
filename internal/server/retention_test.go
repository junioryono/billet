package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// THE CONTROL PLANE DELETES JOB DETAIL PAST ITS RETENTION, as a background loop
// in Run: what the allocator's tests prove works is used.
func TestTheControlPlaneRunsTheJobDetailRetention(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
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
				if s, ok := arg.(*ast.SelectorExpr); ok && s.Sel.Name == "retireJobDetail" {
					started = true
				}
			}
			return true
		})
	}
	if !started {
		t.Error("(*Server).Run starts no background loop running retireJobDetail")
	}
}

// server.job_detail_retention REACHES THE LOOP, and its absence is the default.
func TestTheConfiguredRetentionReachesTheControlPlane(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		key  string
		want time.Duration
	}{
		{"", config.DefaultJobDetailRetention},
		{"168h", 168 * time.Hour},
	} {
		cfg := &config.Config{Server: &config.ServerConfig{JobDetailRetention: tc.key}}
		opts, err := OptionsFromConfig(cfg)
		if err != nil {
			t.Fatalf("OptionsFromConfig(%q): %v", tc.key, err)
		}
		s := New(nil, nil, nil, "test", nil, opts...)
		if s.detailRetention != tc.want {
			t.Errorf("job_detail_retention %q reached the control plane as %s, want %s", tc.key,
				s.detailRetention, tc.want)
		}
	}
}

// A PASS RETIRES THE DETAIL OF A JOB RECORDED LONGER AGO THAN THE RETENTION,
// and leaves a job inside it alone.
func TestARetentionPassRetiresOnlyWhatIsPastIt(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11)
	runner := p.launchedFor11(t)
	lease, err := p.a.Lease(t.Context(), runner.LeaseID)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	usage := alloc.JobUsage{Source: alloc.UsageSourceHost, Unmeasured: []string{alloc.UsageEnergy},
		Samples: 2, IntervalMillis: 1000, WindowMillis: 2000}
	if err := p.a.RecordLeaseUsage(t.Context(), lease.ID, lease.Epoch, usage,
		&alloc.UsageSeries{Codec: alloc.UsageSeriesCodec, Data: []byte{1}}); err != nil {
		t.Fatalf("RecordLeaseUsage: %v", err)
	}

	s := New(p.a, &fakeProvisioner{}, nil, "test", nil, WithJobDetailRetention(time.Hour))
	s.retireJobDetailOnce(t.Context(), time.Now().Add(30*time.Minute))
	if job, err := p.a.Job(t.Context(), lease.ID); err != nil || job.DetailRetiredAt != "" {
		t.Fatalf("a pass inside the retention retired the job (%q, %v)", job.DetailRetiredAt, err)
	}

	s.retireJobDetailOnce(t.Context(), time.Now().Add(2*time.Hour))
	job, err := p.a.Job(t.Context(), lease.ID)
	if err != nil || !strings.HasPrefix(job.DetailRetiredAt, "20") {
		t.Errorf("a pass past the retention left the job's detail_retired_at %q (%v)", job.DetailRetiredAt, err)
	}
}
