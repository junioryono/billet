package config

import (
	"strings"
	"testing"
)

// SILENCE MEANS FAIR, which is what every config written before this key
// existed says, and what a deployment with one tier size cannot tell apart.
func TestAnAbsentAdmissionOrderIsFair(t *testing.T) {
	cfg, err := loadServer(t, serverWith("  state_dir: "+t.TempDir()+"\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := cfg.Server.Order(); got != AdmissionFair {
		t.Errorf("admission order = %q, want %q", got, AdmissionFair)
	}
}

// AND A SERVER BLOCK THAT IS NOT THERE AT ALL IS FAIR TOO, because a node's
// config has none and the control-plane assembly reads this without knowing
// which blocks a file carries.
func TestAnAbsentServerBlockIsFair(t *testing.T) {
	var absent *ServerConfig

	if got := absent.Order(); got != AdmissionFair {
		t.Errorf("admission order with no server block = %q, want %q", got, AdmissionFair)
	}
}

// BOTH POLICIES LOAD.
func TestEachAdmissionOrderIsAccepted(t *testing.T) {
	for _, want := range []AdmissionOrder{AdmissionFair, AdmissionFill} {
		cfg, err := loadServer(t, serverWith(
			"  state_dir: "+t.TempDir()+"\n  admission_order: "+string(want)+"\n"))
		if err != nil {
			t.Fatalf("load %q: %v", want, err)
		}

		if got := cfg.Server.Order(); got != want {
			t.Errorf("admission order = %q, want %q", got, want)
		}
	}
}

// A TYPO MUST NOT SILENTLY BECOME THE DEFAULT.
//
// "fifo" falling through to fair is the failure this package has made before: a
// deployment that believes it configured something. An operator who chose fill
// would never learn their largest tier was holding the line instead.
func TestAMisspelledAdmissionOrderIsRefused(t *testing.T) {
	_, err := loadServer(t, serverWith(
		"  state_dir: "+t.TempDir()+"\n  admission_order: fifo\n"))
	if err == nil {
		t.Fatal("an unknown admission order was accepted")
	}

	// THE DIAGNOSTIC NAMES BOTH VALUES, because the operator has to pick one.
	for _, want := range []string{"admission_order", "fifo", "fair", "fill"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q; got: %v", want, err)
		}
	}
}
