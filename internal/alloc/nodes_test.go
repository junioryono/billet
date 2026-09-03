package alloc

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// WHAT A HOST SAID ABOUT ITS BUILD SURVIVES TO THE REPORT THAT ACTS ON IT.
//
// `billet status` reads these columns to say which hosts still hold an old
// protocol open, and a later release reads the same answer to decide it may stop
// supporting one. A value that never reached the row, or one an upgrade did not
// replace, produces a fleet that reads converged while it is not — and the cost
// is a protocol retired out from under a live machine.
func TestARegistrationRecordsWhatTheNodeSaidAboutItsBuild(t *testing.T) {
	t.Parallel()

	a, err := New(openState(t), Limits{MaxVCPU: 32, MaxMemory: 128 * config.GiB}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	base := NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, VCPU: 8, Memory: 32 * config.GiB,
	}

	// A HOST ON THE RELEASED PROTOCOL, which has no release to give and one
	// version to speak. This is the entire installed fleet on the day the
	// negotiated wire ships.
	old := base
	old.WireMin, old.WireMax, old.WireVersion = 12, 12, 12

	if _, err := a.RegisterNode(t.Context(), old); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	// AND A HOST THAT NEVER SAID ANYTHING, which is what a row written by a
	// binary from before these columns existed looks like. Zero here means NOT
	// RECORDED, and the report renders it as unknown rather than as version zero.
	silent := base
	silent.Name = "epyc-0"

	if _, err := a.RegisterNode(t.Context(), silent); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	got := wireOf(t, a)

	want := NodeWire{Name: "epyc-1", Live: true, Min: 12, Max: 12, Negotiated: 12, Epoch: 1}
	if got["epyc-1"] != want {
		t.Errorf("the released-protocol host recorded %+v, want %+v", got["epyc-1"], want)
	}

	if want := (NodeWire{Name: "epyc-0", Live: true, Epoch: 1}); got["epyc-0"] != want {
		t.Errorf("a host that said nothing recorded %+v, want %+v", got["epyc-0"], want)
	}

	// A FIRST REGISTRATION'S EPOCH IS NOT ZERO, and taking the column default was a
	// defect that hid for as long as nothing read the number for anything but
	// equality. Zero is what every other recorded-at-registration field in this row
	// means by "the binary that wrote me did not record this", and a rollout asks
	// whether a host's CURRENT epoch is higher than the one an instruction was sent
	// against — so an epoch that is legitimately zero is indistinguishable from one
	// that was never recorded. That is the ORDINARY case, not an edge: a node
	// registers once, an operator starts a rollout, and the host is dispatched at
	// its first epoch. No rollback on that machine could ever be detected.
	for name, node := range got {
		if node.Epoch == 0 {
			t.Errorf("%s registered and its epoch is zero, which every reader takes to "+
				"mean nothing was recorded", name)
		}
	}

	// AN UPGRADE REPLACES THEM, like capacity and unlike the provider. The answer
	// to "what is this machine running" is whatever it just said; keeping the old
	// value would leave a converged host reported as still holding the fleet back.
	upgraded := base
	upgraded.Release = "v9.9.9"
	upgraded.WireMin, upgraded.WireMax, upgraded.WireVersion = 12, 13, 13

	if _, err := a.RegisterNode(t.Context(), upgraded); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	before := got["epyc-1"].Epoch
	got = wireOf(t, a)

	want = NodeWire{
		Name: "epyc-1", Live: true, Release: "v9.9.9", Min: 12, Max: 13, Negotiated: 13,
		Epoch: got["epyc-1"].Epoch,
	}
	if got["epyc-1"] != want {
		t.Errorf("after an upgrade the host recorded %+v, want %+v", got["epyc-1"], want)
	}

	// AND THE EPOCH MOVED, which is the whole reason it is reported here. A rollout
	// watching a host it told to upgrade cannot otherwise tell one still draining
	// from one that came back on the release it started with: both are live and
	// both name the old version. Only a NEWER registration proves the host
	// restarted, so a re-registration that left this alone would strand a
	// rolled-back host in `draining` forever.
	if got["epyc-1"].Epoch <= before {
		t.Errorf("re-registering left the epoch at %d (was %d), so nothing distinguishes "+
			"a host that came back from one that never left",
			got["epyc-1"].Epoch, before)
	}
}

// A WIRE RECORD THAT CANNOT DESCRIBE ANY BUILD IS REFUSED, NOT CLAMPED.
//
// RegisterNode is exported, so it cannot assume its input came through the wire.
// Nothing authorises compute from these columns, but `billet status` decides
// from them whether an old protocol is safe to stop supporting — and a tuple
// like min=14, negotiated=13, max=12 reads there as a converged host, which
// retires a protocol a live machine still needs.
func TestAnImpossibleWireRecordIsRefused(t *testing.T) {
	t.Parallel()

	a, err := New(openState(t), Limits{MaxVCPU: 32, MaxMemory: 128 * config.GiB}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	base := NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, VCPU: 8, Memory: 32 * config.GiB,
	}

	for _, tc := range []struct {
		name            string
		min, negotiated int
		maxVersion      int
		wantRefused     bool
	}{
		{"all zero is the unrecorded row", 0, 0, 0, false},
		{"a properly nested tuple", 12, 13, 13, false},
		{"one version, one build", 12, 12, 12, false},

		{"a floor above what was negotiated", 14, 13, 12, true},
		{"negotiated above the newest", 12, 14, 13, true},
		{"negotiated below the floor", 12, 11, 13, true},
		{"a floor of zero beside real numbers", 0, 13, 13, true},
		{"a negative floor", -1, 13, 13, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := base
			reg.Name = "n-" + tc.name
			reg.WireMin, reg.WireVersion, reg.WireMax = tc.min, tc.negotiated, tc.maxVersion

			_, err := a.RegisterNode(t.Context(), reg)

			switch {
			case tc.wantRefused && err == nil:
				t.Fatalf("min=%d negotiated=%d newest=%d was accepted; no build can be all "+
					"three", tc.min, tc.negotiated, tc.maxVersion)
			case !tc.wantRefused && err != nil:
				t.Fatalf("min=%d negotiated=%d newest=%d was refused: %v",
					tc.min, tc.negotiated, tc.maxVersion, err)
			}

			if !tc.wantRefused {
				return
			}

			// AND NOTHING WAS WRITTEN. A refusal that had already upserted the row
			// would leave the contradictory record it exists to keep out.
			for _, got := range wireOf(t, a) {
				if got.Name == reg.Name {
					t.Errorf("a refused registration still recorded %+v", got)
				}
			}
		})
	}
}

func wireOf(t *testing.T, a *Allocator) map[string]NodeWire {
	t.Helper()

	rows, err := a.NodeWireVersions(t.Context())
	if err != nil {
		t.Fatalf("NodeWireVersions: %v", err)
	}

	out := make(map[string]NodeWire, len(rows))
	for _, row := range rows {
		out[row.Name] = row
	}

	return out
}

func TestRegisteredNodesReportSiteAndLivenessInNameOrder(t *testing.T) {
	t.Parallel()

	a, err := New(openState(t), Limits{MaxVCPU: 32, MaxMemory: 128 * config.GiB}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	registrations := []NodeRegistration{
		{Name: "epyc-2", Provider: config.ProviderFirecracker, Site: "home", VCPU: 8, Memory: 32 * config.GiB},
		{Name: "edge-1", Provider: config.ProviderFirecracker, Site: "edge", VCPU: 8, Memory: 32 * config.GiB},
		{Name: "epyc-1", Provider: config.ProviderFirecracker, Site: "home", VCPU: 8, Memory: 32 * config.GiB},
		{Name: "legacy", Provider: config.ProviderDocker, VCPU: 8, Memory: 32 * config.GiB},
	}
	var edgeEpoch int64
	for _, registration := range registrations {
		epoch, err := a.RegisterNode(t.Context(), registration)
		if err != nil {
			t.Fatalf("RegisterNode(%s): %v", registration.Name, err)
		}
		if registration.Name == "edge-1" {
			edgeEpoch = epoch
		}
	}
	if err := a.NodeGone(t.Context(), "edge-1", edgeEpoch); err != nil {
		t.Fatalf("NodeGone: %v", err)
	}

	got, err := a.RegisteredNodes(t.Context())
	if err != nil {
		t.Fatalf("RegisteredNodes: %v", err)
	}
	want := []RegisteredNode{
		{Name: "edge-1", Provider: config.ProviderFirecracker, Site: "edge", Live: false},
		{Name: "epyc-1", Provider: config.ProviderFirecracker, Site: "home", Live: true},
		{Name: "epyc-2", Provider: config.ProviderFirecracker, Site: "home", Live: true},
		{Name: "legacy", Provider: config.ProviderDocker, Site: "", Live: true},
	}
	if len(got) != len(want) {
		t.Fatalf("RegisteredNodes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("RegisteredNodes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRegisteredNodesRefuseCorruptPlacementIdentity(t *testing.T) {
	// The corruption is written by turning CHECK constraints off, and PostgreSQL
	// has no equivalent switch — the value simply cannot be stored there. The
	// same fail-closed reading on that engine needs the constraint dropped and
	// re-added by name, which is its own test rather than a translation of this.
	skipOnPostgres(t, "PRAGMA ignore_check_constraints is how the illegal row is written")

	t.Parallel()

	for _, tc := range []struct {
		name    string
		corrupt func(*testing.T, *Allocator)
		wantErr string
	}{
		{
			name: "unknown provider",
			corrupt: func(t *testing.T, a *Allocator) {
				t.Helper()
				if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
					_, err := tx.ExecContext(t.Context(),
						`UPDATE nodes SET provider = 'bogus' WHERE name = 'z-corrupt'`)

					return err
				}); err != nil {
					t.Fatalf("corrupt provider: %v", err)
				}
			},
			wantErr: `unknown provider "bogus"`,
		},
		{
			name: "invalid liveness",
			corrupt: func(t *testing.T, a *Allocator) {
				t.Helper()
				if err := a.db.Tx(t.Context(), func(tx *sql.Tx) error {
					if _, err := tx.ExecContext(t.Context(),
						`PRAGMA ignore_check_constraints = ON`); err != nil {
						return err
					}
					_, updateErr := tx.ExecContext(t.Context(),
						`UPDATE nodes SET live = 2 WHERE name = 'z-corrupt'`)
					_, resetErr := tx.ExecContext(t.Context(),
						`PRAGMA ignore_check_constraints = OFF`)

					return errors.Join(updateErr, resetErr)
				}); err != nil {
					t.Fatalf("corrupt liveness: %v", err)
				}
			},
			wantErr: "invalid liveness 2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a, err := New(openState(t), Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, nil)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			for _, name := range []string{"a-valid", "z-corrupt"} {
				if _, err := a.RegisterNode(t.Context(), NodeRegistration{
					Name: name, Provider: config.ProviderDocker, Site: "home",
					VCPU: 8, Memory: 32 * config.GiB,
				}); err != nil {
					t.Fatalf("RegisterNode(%s): %v", name, err)
				}
			}
			tc.corrupt(t, a)

			got, err := a.RegisteredNodes(t.Context())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("RegisteredNodes error = %v, want one containing %q", err, tc.wantErr)
			}
			if got != nil {
				t.Fatalf("RegisteredNodes returned partial results after corruption: %+v", got)
			}
		})
	}
}
