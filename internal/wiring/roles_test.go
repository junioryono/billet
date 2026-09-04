package wiring_test

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/junioryono/godi/v5"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wiring"
	"github.com/junioryono/billet/internal/wiringtest"
)

// resolveEverything walks a collection's own inventory and resolves every
// entry, reporting each failure rather than stopping at the first.
//
// THE REGISTRY, NOT A LIST WRITTEN HERE. A test naming services is a second
// source of truth that a new registration silently escapes; walking ToSlice
// covers a service the moment it is added. Group members carry a numeric key,
// so the group case is checked before the keyed one.
func resolveEverything(t *testing.T, c godi.Collection, from godi.Provider) {
	t.Helper()

	registered := c.ToSlice()
	if len(registered) == 0 {
		t.Fatal("no registrations enumerated, so nothing below could fail")
	}

	for _, svc := range registered {
		var err error

		switch {
		case svc.Group != "":
			_, err = from.GetGroup(svc.ServiceType, svc.Group)
		case svc.Key != nil:
			_, err = from.GetKeyed(svc.ServiceType, svc.Key)
		default:
			_, err = from.Get(svc.ServiceType)
		}

		if err != nil {
			t.Errorf("%s is registered and does not resolve: %v", svc.ServiceType, err)
		}
	}
}

// EVERY ROLE'S GRAPH BUILDS, AND EVERY REGISTRATION IN IT RESOLVES. A missing
// registration or a cycle fails here rather than in `billet server` on a host.
// The control-plane and node roles join this table as their modules land.
func TestEveryRolesGraphBuilds(t *testing.T) {
	t.Parallel()

	roles := map[string]func(path wiring.ConfigPath) []godi.ModuleOption{
		"operator": func(path wiring.ConfigPath) []godi.ModuleOption {
			return wiring.OperatorModules(path)
		},
		"operator with the allocator, the client and the issuer": func(path wiring.ConfigPath) []godi.ModuleOption {
			return wiring.OperatorModules(path,
				wiring.CapacityModule(), wiring.GitHubModule(), wiring.IdentityModule(),
				wiring.AuthorityModule(), wiring.IssuerModule())
		},
		"decision":    wiring.DecisionModules,
		"maintenance": wiring.MaintenanceModules,
	}

	for name, compose := range roles {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// A DEPLOYMENT OF ITS OWN, so parallel subtests share no ledger. A
			// ledger exists before the set is built, because the decision reader
			// refuses a directory with none, which is its own test below.
			fixture := writeDeployment(t)
			path := wiring.ConfigPath(fixture.configPath)

			db, err := state.Open(t.Context(), fixture.stateDir)
			if err != nil {
				t.Fatalf("found the ledger: %v", err)
			}

			if err := db.Close(); err != nil {
				t.Fatalf("close the founding handle: %v", err)
			}

			modules := compose(path)
			c := wiring.Collect(modules...)

			// A FLOOR ON THE INVENTORY, so a role set that lost its modules does
			// not pass by resolving nothing.
			if c.Count() < 8 {
				t.Fatalf("%s registers %d services; the smallest role set is larger, so "+
					"enumeration broke rather than the graph shrinking", name, c.Count())
			}

			provider := wiringtest.Build(t, modules, nil)

			resolveEverything(t, c, provider)
		})
	}
}

// EACH ROLE SET REGISTERS EXACTLY ONE LEDGER HANDLE, which is what stops an
// operator command being handed the control plane's.
func TestEachRoleSetRegistersExactlyOneLedgerHandle(t *testing.T) {
	t.Parallel()

	path := wiring.ConfigPath("unused")

	dbType := reflect.TypeFor[*state.DB]()

	for name, modules := range map[string][]godi.ModuleOption{
		"operator":    wiring.OperatorModules(path),
		"decision":    wiring.DecisionModules(path),
		"maintenance": wiring.MaintenanceModules(path),
	} {
		handles := 0

		for _, svc := range wiring.Collect(modules...).ToSlice() {
			if svc.ServiceType == dbType {
				handles++
			}
		}

		if handles != 1 {
			t.Errorf("%s registers %d *state.DB, want exactly one", name, handles)
		}
	}
}

// EVERY LEDGER MODE NAMES THE RUNNING RELEASE, EXCEPT THE DECISION READER.
//
// The watermark refuses a proved older binary at every open and records a
// newer one only at the claim; an open that names no release gets neither, and
// nothing at run time would notice the omission. This replaces the structural
// test that parsed cmd/billet/ledger.go for state.WithRunningRelease: a ledger
// whose watermark is newer than the release injected here must refuse every
// mode, and the decision reader (a standby's older timer reading what it should
// become) must not.
func TestEveryLedgerModeNamesTheRunningReleaseExceptTheDecisionReader(t *testing.T) {
	t.Parallel()

	fixture := writeDeployment(t)
	path := wiring.ConfigPath(fixture.configPath)

	// A CONTROL PLANE ON A NEWER RELEASE CLAIMS, which is the one write that
	// raises the watermark.
	deployment, err := state.DeploymentID(fixture.stateDir)
	if err != nil {
		t.Fatalf("mint the identity: %v", err)
	}

	newer, err := state.Open(t.Context(), fixture.stateDir, state.WithRunningRelease("v9.9.9"))
	if err != nil {
		t.Fatalf("open as the newer release: %v", err)
	}

	if _, err := newer.ClaimController(t.Context(), "newer", deployment); err != nil {
		t.Fatalf("claim as the newer release: %v", err)
	}

	if err := newer.Close(); err != nil {
		t.Fatalf("close the newer release's handle: %v", err)
	}

	older := wiring.CoreModule(wiring.Core{ConfigPath: path, Release: "v0.1.0"})

	for name, mode := range map[string]wiring.LedgerMode{
		"control plane": wiring.LedgerControlPlane,
		"probe":         wiring.LedgerProbe,
		"operator":      wiring.LedgerOperator,
	} {
		_, err := wiring.Build(t.Context(), older, wiring.AWSModule(), wiring.LedgerModule(mode))
		if !errors.Is(err, state.ErrReleaseBehind) {
			t.Errorf("%s mode opened an older release over a newer watermark: %v", name, err)
		}
	}

	// THE DECISION READER OPENS, because it names no release.
	provider, err := wiring.Build(t.Context(), older, wiring.AWSModule(),
		wiring.LedgerModule(wiring.LedgerDecision))
	if err != nil {
		t.Fatalf("the decision reader was refused by a watermark it must not consult: %v", err)
	}

	if err := provider.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// AND THE REFUSAL IS THE WATERMARK'S, not something else about the fixture:
	// the same modes open under a release that is not behind.
	current := wiring.CoreModule(wiring.Core{ConfigPath: path, Release: "v9.9.9"})

	provider, err = wiring.Build(t.Context(), current, wiring.AWSModule(),
		wiring.LedgerModule(wiring.LedgerOperator))
	if err != nil {
		t.Fatalf("the operator mode was refused under the watermark's own release: %v", err)
	}

	if err := provider.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// THE DECISION READER CREATES NOTHING. The package enables the timer that runs
// it on every host, including one whose server has never run, and an operator
// open of an empty state directory would mint a root-owned ledger there.
func TestTheDecisionReaderRefusesADirectoryWithNoLedger(t *testing.T) {
	t.Parallel()

	fixture := writeDeployment(t)

	_, err := wiring.Build(t.Context(), wiring.DecisionModules(wiring.ConfigPath(fixture.configPath))...)
	if !errors.Is(err, wiring.ErrNoLedgerYet) {
		t.Fatalf("the decision reader did not refuse an empty state directory: %v", err)
	}

	if _, statErr := os.Lstat(state.LedgerPath(fixture.stateDir)); statErr == nil {
		t.Fatal("the decision reader created a ledger while refusing")
	}
}

// A BUILD FAILURE NAMES THE SERVICE AND UNWRAPS TO THE CONSTRUCTOR'S ERROR, so
// a caller can act on the sentinel and an operator reads the sentence.
func TestABuildFailureNamesTheServiceAndKeepsTheCause(t *testing.T) {
	t.Parallel()

	fixture := writeDeployment(t)

	_, err := wiring.Build(t.Context(), wiring.DecisionModules(wiring.ConfigPath(fixture.configPath))...)

	built, ok := errors.AsType[*wiring.BuildError](err)
	if !ok {
		t.Fatalf("the failure is not a wiring.BuildError: %T %v", err, err)
	}

	if built.Service != "*state.DB" {
		t.Errorf("the failure names %q, want *state.DB", built.Service)
	}

	if !errors.Is(built.Err, wiring.ErrNoLedgerYet) {
		t.Errorf("the failure's cause is not the constructor's own: %v", built.Err)
	}
}
