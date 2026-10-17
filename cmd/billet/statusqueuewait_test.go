package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// STATUS PRINTS EACH TIER'S LAST HOUR OF QUEUE WAIT FROM THE LEDGER. Driven
// through cmdStatus over a job the real allocator started, so the line cannot
// stay right while the command stops reading the waits.
func TestStatusPrintsTheLastHoursQueueWait(t *testing.T) {
	stateDir := t.TempDir()
	path := writeCAConfig(t, stateDir)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, cfg.Tiers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "one-slot", Provider: config.ProviderDocker, VCPU: 2, Memory: 8 * config.GiB,
	}); err != nil {
		t.Fatal(err)
	}

	label := cfg.Tiers[0].Label
	lease, err := a.Reserve(t.Context(), label)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 0, 1); err != nil {
		t.Fatal(err)
	}
	name := "billet-" + lease.ID
	if err := a.RegisterPoolRunner(t.Context(), alloc.PoolRunner{LeaseID: lease.ID, Tier: label,
		LaunchRequestID: 1, RunnerName: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.StartPoolRunner(t.Context(), lease.ID, label, 1, name, 1, 9, "j1", alloc.JobIdentity{}); err != nil {
		t.Fatal(err)
	}
	if err := a.RecordJobIdentity(t.Context(), lease.ID, alloc.HistoryJob{JobID: "j1",
		QueuedAt: time.Now().Add(-20 * time.Minute)}); err != nil {
		t.Fatal(err)
	}

	out := capture(t, func() {
		if err := cmdStatus(t.Context(), processEnv(), []string{"--config", path}); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(out, "queue wait p99 20m0s over the last hour's 1 job(s)") {
		t.Errorf("status does not print the tier's queue wait:\n%s", out)
	}
}

// AN HOUR OF JOBS WITH NO GITHUB QUEUE TIME IS UNKNOWN, never a zero wait, and a
// tier that started nothing says nothing.
func TestStatusSaysAnUnrecordedWaitIsUnknown(t *testing.T) {
	now := time.Now()

	var unknown bytes.Buffer
	printTierWait(&unknown, []alloc.JobWait{{LeaseID: "l1", StartedAt: now}})
	if !strings.Contains(unknown.String(), "queue wait unknown: 1 job(s)") ||
		strings.Contains(unknown.String(), "p99") {
		t.Errorf("an unrecorded wait reads %q", unknown.String())
	}

	var mixed bytes.Buffer
	printTierWait(&mixed, []alloc.JobWait{
		{LeaseID: "l1", StartedAt: now},
		{LeaseID: "l2", QueuedAt: now.Add(-time.Minute), StartedAt: now},
	})
	if !strings.Contains(mixed.String(), "p99 1m0s over the last hour's 1 job(s)") ||
		!strings.Contains(mixed.String(), "1 more unknown") {
		t.Errorf("a mixed hour reads %q", mixed.String())
	}

	var quiet bytes.Buffer
	printTierWait(&quiet, nil)
	if quiet.Len() != 0 {
		t.Errorf("a tier that started nothing printed %q", quiet.String())
	}
}
