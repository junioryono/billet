package config

import (
	"strings"
	"testing"
)

// postgresServer is the smallest server block whose ledger a second machine can
// reach, which is what an active/passive pair needs.
func postgresServer(t *testing.T, extra string) string {
	t.Helper()

	return serverWith("  identity_dir: " + t.TempDir() + "\n" +
		"  state:\n    backend: postgres\n    postgres:\n      dsn_env: BILLET_STATE_DSN\n" +
		extra)
}

// SILENCE MEANS ONE CONTROLLER, which is what every config written before this
// key existed says.
//
// NORMALIZED RATHER THAN LEFT EMPTY, so no reader has to remember what an absent
// value means — the rule this package already follows for a value nothing
// outside the process has an opinion about.
func TestAnAbsentControllerLayoutIsSingle(t *testing.T) {
	cfg, err := loadServer(t, serverWith("  state_dir: "+t.TempDir()+"\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := cfg.Server.Controllers; got != ControllersSingle {
		t.Errorf("controllers = %q, want %q", got, ControllersSingle)
	}
}

// AN ACTIVE/PASSIVE PAIR NEEDS A LEDGER A SECOND MACHINE CAN REACH.
//
// A SQLite ledger is a file on local storage — billet refuses to put one on a
// network filesystem at all — so there is nothing for a second control plane to
// take over, and a standby there would wait forever for a lock only its own
// host's other process could hold. Accepting the key and doing nothing with it
// is the failure this package has already made three times: a deployment that
// believes it configured something.
func TestActivePassiveIsRefusedOnASQLiteLedger(t *testing.T) {
	_, err := loadServer(t, serverWith(
		"  state_dir: "+t.TempDir()+"\n  controllers: active-passive\n"))
	if err == nil {
		t.Fatal("active-passive was accepted beside a SQLite ledger")
	}

	// THE DIAGNOSTIC NAMES THE WAY OUT, because an operator who wrote this key
	// wants two controllers and needs to know what actually gets them one.
	for _, want := range []string{"active-passive", "sqlite", "postgres"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q; got: %v", want, err)
		}
	}
}

// AND IT IS ACCEPTED ON A POSTGRESQL ONE.
//
// The other direction, kept because a validator that refused everything would
// pass the test above and take every correct deployment with it.
func TestActivePassiveIsAcceptedOnAPostgresLedger(t *testing.T) {
	cfg, err := loadServer(t, postgresServer(t, "  controllers: active-passive\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := cfg.Server.Controllers; got != ControllersActivePassive {
		t.Errorf("controllers = %q, want %q", got, ControllersActivePassive)
	}
}

// A LAYOUT BILLET DOES NOT KNOW IS REFUSED BY NAME.
//
// Not silently treated as single: a config saying `controllers: activepassive`
// means an operator who wanted a pair and would get one controller, with the
// standby host reporting itself healthy and refusing to start.
func TestAnUnknownControllerLayoutIsRefused(t *testing.T) {
	_, err := loadServer(t, postgresServer(t, "  controllers: activepassive\n"))
	if err == nil {
		t.Fatal("an unknown controller layout was accepted")
	}

	for _, want := range []string{"activepassive", "single", "active-passive"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q; got: %v", want, err)
		}
	}
}
