package main

import (
	"os"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// THE HELD TABLE NAMES THE PROCESS HOLDING EACH LEASE, AND SAYS WHEN IT WAS
// REPLACED — with "unknown" for a lease nothing recorded a holder for, because
// "cannot tell" rendered as the first process would send an operator to force a
// lease whose holder may be perfectly alive.
func TestHeldLeasesNameTheirHolderAndWhetherItWasReplaced(t *testing.T) {
	held := []alloc.HeldLease{
		{
			ID: "aaaa", Tier: "billet-2vcpu", Node: "epyc-1", State: alloc.PhaseTeardown,
			VCPU: 2, Memory: 8 * config.GiB,
			Holder: alloc.Holder{
				Incarnation: "3333333333333333aaaa", NodeIncarnation: "5555555555555555bbbb",
				NodeKnown: true, NodeLive: true,
			},
		},
		{
			ID: "bbbb", Tier: "billet-2vcpu", Node: "epyc-1", State: alloc.PhaseCustody,
			VCPU: 2, Memory: 8 * config.GiB,
			Holder: alloc.Holder{
				Incarnation: "5555555555555555bbbb", NodeIncarnation: "5555555555555555bbbb",
				NodeKnown: true, NodeLive: true,
			},
		},
		{
			ID: "cccc", Tier: "billet-2vcpu", Node: "old-host", State: alloc.PhaseQuarantine,
			VCPU: 2, Memory: 8 * config.GiB,
			Holder: alloc.Holder{},
		},
	}

	out := capture(t, func() {
		printHeld(held)
		printHolderNote(os.Stdout, held)
	})

	for _, want := range []string{
		"HOLDER",
		"process 333333333333, REPLACED by 555555555555",
		"unknown",
		"A holder marked REPLACED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the held table does not say %q:\n%s", want, out)
		}
	}

	// A CURRENT HOLDER IS NAMED AND NOT CALLED REPLACED.
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "bbbb") {
			continue
		}
		if !strings.Contains(line, "process 555555555555") || strings.Contains(line, "REPLACED") {
			t.Errorf("a current holder is misreported: %s", line)
		}
	}

	if strings.Contains(out, "process ,") || strings.Contains(out, "process \t") {
		t.Errorf("an unrecorded holder was rendered as a process:\n%s", out)
	}

	// THE NOTE IS PRINTED ONLY WHEN SOMETHING WAS REPLACED. A table with live
	// holders alone must not carry a paragraph about dead ones.
	quiet := capture(t, func() { printHolderNote(os.Stdout, held[1:]) })
	if quiet != "" {
		t.Errorf("the replaced-holder note was printed with nothing replaced:\n%s", quiet)
	}
}

// `billet status` REPORTS A RUNNING LEASE WHOSE NODE PROCESS WAS REPLACED.
//
// Driven through cmdStatus, because the printer being correct says nothing
// about whether status calls it. The lease is in `busy`, so it appears under no
// other line: without this section the slot it takes is visible and nothing
// says why.
func TestStatusReportsARunningLeaseWhoseHolderWasReplaced(t *testing.T) {
	stateDir := t.TempDir()
	cfgPath := writeCAConfig(t, stateDir)
	ctx := t.Context()

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	db, err := state.Open(ctx, stateDir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}

	a, err := alloc.New(db, alloc.Limits{
		MaxVCPU: cfg.Server.MaxVCPU, MaxMemory: cfg.Server.MaxMemory, Nodes: cfg.NodePolicies(),
	}, cfg.Tiers)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	host := alloc.NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, VCPU: 8, Memory: 32 * config.GiB,
		Incarnation: "process-one",
	}
	if _, err := a.RegisterNode(ctx, host); err != nil {
		t.Fatalf("register: %v", err)
	}

	lease, err := a.Reserve(ctx, cfg.Tiers[0].Label)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := a.Bind(ctx, lease.ID, lease.Epoch, "epyc-1"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	for _, phase := range []alloc.Phase{
		alloc.PhaseAssigned, alloc.PhaseLaunching, alloc.PhaseOnline, alloc.PhaseBusy,
	} {
		if err := a.Advance(ctx, lease.ID, lease.Epoch, phase); err != nil {
			t.Fatalf("advance to %s: %v", phase, err)
		}
	}

	// THE SAME PROCESS REGISTERING AGAIN — what every control-plane restart
	// produces — moves the epoch and replaces nobody.
	if _, err := a.RegisterNode(ctx, host); err != nil {
		t.Fatalf("re-register the same process: %v", err)
	}

	// A HOLDER STILL RUNNING IS REPORTED NOWHERE. Asserted first, so the section
	// below cannot pass by printing unconditionally.
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	before := capture(t, func() {
		if err := cmdStatus(ctx, []string{"--config", cfgPath}); err != nil {
			t.Errorf("status: %v", err)
		}
	})
	if strings.Contains(before, "bound ") {
		t.Errorf("status reports a replaced holder while the holder is current:\n%s", before)
	}

	// A DIFFERENT PROCESS REGISTERS UNDER THE HOST'S NAME: the process that was
	// given the job is no longer the one the deployment talks to, and the lease
	// it holds is still charged.
	db, err = state.Open(ctx, stateDir)
	if err != nil {
		t.Fatalf("reopen the ledger: %v", err)
	}
	a, err = alloc.New(db, alloc.Limits{
		MaxVCPU: cfg.Server.MaxVCPU, MaxMemory: cfg.Server.MaxMemory, Nodes: cfg.NodePolicies(),
	}, cfg.Tiers)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	host.Incarnation = "process-two"
	if _, err := a.RegisterNode(ctx, host); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	out := capture(t, func() {
		if err := cmdStatus(ctx, []string{"--config", cfgPath}); err != nil {
			t.Errorf("status: %v", err)
		}
	})

	for _, want := range []string{
		"bound     1 running lease(s) whose node process was replaced",
		lease.ID,
		"process process-one, REPLACED by process-two",
		"busy",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status does not say %q:\n%s", want, out)
		}
	}
}
