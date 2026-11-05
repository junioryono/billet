package server

import (
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// THE CONTROL PLANE DELETES JOB DETAIL PAST ITS RETENTION AND NUMBERS OLD USAGE, each
// as a background loop in Run: what the allocator's tests prove works is used.
func TestTheControlPlaneRunsItsLedgerUpkeep(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	started := map[string]bool{}
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
				if s, ok := arg.(*ast.SelectorExpr); ok {
					started[s.Sel.Name] = true
				}
			}
			return true
		})
	}
	for _, loop := range []string{"retireJobDetail", "numberUsage"} {
		if !started[loop] {
			t.Errorf("(*Server).Run starts no background loop running %s", loop)
		}
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
	if err := p.a.Release(t.Context(), lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
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

// A PASS NUMBERS MORE THAN ONE BATCH of the leases that finished before there were
// numbers, oldest first, and the next pass has nothing to do.
func TestAPassNumbersFinishedLeasesPastOneBatch(t *testing.T) {
	t.Parallel()

	db, err := state.Open(t.Context(), ledgertest.Dir(t))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB},
		[]config.Tier{tier("billet-4vcpu-a")})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	const leases = usageNumberBatch + 3

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for i := range leases {
			if _, err := tx.ExecContext(t.Context(), fmt.Sprintf(`INSERT INTO job_history
				(lease_id, tier, queued_at, finished_at) VALUES ('%032d', 'billet-4vcpu-a', 't', '%032d')`,
				i, i)); err != nil {
				return err
			}
		}

		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s := New(a, &fakeProvisioner{}, nil, "test", nil)
	s.numberUsageOnce(t.Context())

	rows, err := db.UsageSince(t.Context(), 0, leases+1)
	if err != nil {
		t.Fatalf("UsageSince: %v", err)
	}
	if len(rows) != leases {
		t.Fatalf("a pass numbered %d leases, want %d", len(rows), leases)
	}
	for i, r := range rows {
		if want := fmt.Sprintf("%032d", i); r.LeaseID != want {
			t.Fatalf("number %d went to %s, want %s", r.Sequence, r.LeaseID, want)
		}
	}

	waiting, err := db.UsageStillNumbering(t.Context())
	if err != nil || waiting {
		t.Fatalf("UsageStillNumbering = %v, %v; nothing is left", waiting, err)
	}

	s.numberUsageOnce(t.Context())

	again, err := db.UsageSince(t.Context(), rows[len(rows)-1].Sequence, 1)
	if err != nil || len(again) != 0 {
		t.Fatalf("a second pass numbered %d more (%v)", len(again), err)
	}
}
