package nodeplane

import (
	"errors"
	"slices"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
)

// A REGISTRATION'S ARCHITECTURES REACH THE LEDGER FROM THE WIRE THAT CARRIES THEM,
// and below it the node is recorded as reporting none, which places it only for
// tiers that name none (ADR-022).
func TestArchitecturesAreRecordedOnlyFromTheWireThatCarriesThem(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		wire int
		want []config.Arch
	}{
		{wire: nodeapi.VersionArchitectures, want: []config.Arch{config.ArchARM64}},
		{wire: nodeapi.VersionArchitectures - 1, want: nil},
	} {
		reg := &recordingRegistrar{}
		p := testPlane(t, WithRegistrar(reg))

		req := registerAt("")
		req.Version, req.MinVersion = tc.wire, tc.wire
		req.Architectures = []config.Arch{config.ArchARM64}

		if _, err := p.Register(t.Context(), req); err != nil {
			t.Fatalf("register at wire %d: %v", tc.wire, err)
		}

		if got := reg.recorded().Architectures; reg.callCount() != 1 || !slices.Equal(got, tc.want) {
			t.Errorf("at wire %d the ledger was given architectures %v (%d calls), want %v",
				tc.wire, got, reg.callCount(), tc.want)
		}
	}
}

// AND A REPORT PLACEMENT COULD BE MISLED BY IS REFUSED PERMANENTLY, before the
// ledger, whose errors the node would retry as an outage forever.
func TestAnArchitectureReportPlacementCouldBeMisledByIsRefusedBeforeTheLedger(t *testing.T) {
	t.Parallel()

	for _, archs := range [][]config.Arch{
		{config.ArchX64, config.ArchARM64},
		{"aarch64"},
	} {
		reg := &recordingRegistrar{}
		p := testPlane(t, WithRegistrar(reg))

		req := registerAt("")
		req.Architectures = archs

		_, err := p.Register(t.Context(), req)
		if !errors.Is(err, ErrRefused) {
			t.Errorf("a %s node reporting %v was answered %v, want ErrRefused", req.Provider, archs, err)
		}

		if reg.callCount() != 0 {
			t.Errorf("a refused report reached the ledger: %+v", reg.recorded())
		}
	}
}
