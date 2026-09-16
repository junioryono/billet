package main

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/retirement"
)

func TestRetiredMaskedAccountUsesRecordedLocalIdentity(t *testing.T) {
	useRetirementRoot(t)
	account := retirement.ServiceAccount{User: "billet", UID: 123, Group: "billet", GID: 456}
	mustOK(t, retirement.WriteServiceAccount(account))
	savedInspector, savedUser, savedGroup := retireOperationInspector, retireLookupUser, retireLookupGroup
	t.Cleanup(func() {
		retireOperationInspector, retireLookupUser, retireLookupGroup = savedInspector, savedUser, savedGroup
	})
	for _, scenario := range []string{"masked", "absent", "runtime mask", "unknown", "uid", "gid", "home", "group name", "missing user"} {
		t.Run(scenario, func(t *testing.T) {
			load, state := "masked", "masked"
			switch scenario {
			case "absent":
				load, state = "not-found", ""
			case "runtime mask":
				state = "masked-runtime"
			case "unknown":
				load = "future-state"
			}
			retireOperationInspector = func() *lifeops.Inspector {
				return lifeops.NewInspector(lifeops.WithCommandRunner(func(_ context.Context, _ string, args []string) ([]byte, error) {
					if !strings.Contains(strings.Join(args, " "), "LoadState") || !strings.Contains(strings.Join(args, " "), "UnitFileState") {
						return nil, errors.New("missing property query")
					}
					return []byte("LoadState=" + load + "\nUnitFileState=" + state + "\n"), nil
				}))
			}
			retireLookupUser = func(name string) (*user.User, error) {
				if scenario == "absent" {
					t.Fatal("absent server demanded executable account evidence")
				}
				if name != account.User {
					t.Fatal("looked up inventory account instead of recorded name")
				}
				u := &user.User{Username: name, Uid: "123", Gid: "456", HomeDir: retirement.Root}
				switch scenario {
				case "uid":
					u.Uid = "789"
				case "gid":
					u.Gid = "789"
				case "home":
					u.HomeDir = "/unrelated"
				case "missing user":
					return nil, errors.New("account missing")
				}
				return u, nil
			}
			retireLookupGroup = func(name string) (*user.Group, error) {
				if scenario == "group name" {
					name = "unrelated"
				}
				return &user.Group{Name: name, Gid: "456"}, nil
			}
			r := proveRetireMaskedAccount(t.Context())
			if scenario == "masked" || scenario == "absent" {
				if r != nil {
					t.Fatalf("supported retired account refused: %+v", r)
				}
			} else if r == nil || !strings.HasPrefix(r.Reason, "retired-account-") {
				t.Fatalf("%s was not refused by account observation: %+v", scenario, r)
			}
		})
	}
}

// Removing proveRetireMaskedAccount from observeRetireOrdinaryEntry must turn
// each account refusal into the same admission as the matching-mask control.
func TestRetirementNodeConfigChecksMaskedAccountAtOrdinaryEntry(t *testing.T) {
	f, j := settledNodeConfigFixture(t)
	mask := filepath.Join(t.TempDir(), serverUnit)
	mustOK(t, os.Symlink("/dev/null", mask))
	f.manager.set(serverUnit, "LoadState", "masked")
	f.manager.set(serverUnit, "UnitFileState", "masked")
	setRetireEffect(t, f, serverUnit, "FragmentPath", mask)
	setRetireEffect(t, f, serverUnit, "User", "")
	setRetireEffect(t, f, serverUnit, "Group", "")
	account := retirement.ServiceAccount{User: "billet", UID: 123, Group: "billet", GID: 456}
	savedUser, savedGroup := retireLookupUser, retireLookupGroup
	t.Cleanup(func() { retireLookupUser, retireLookupGroup = savedUser, savedGroup })
	for _, scenario := range []string{"matching", "uid", "gid", "home", "missing record"} {
		t.Run(scenario, func(t *testing.T) {
			mustOK(t, retirement.WriteServiceAccount(account))
			if scenario == "missing record" {
				mustOK(t, os.Remove(retirement.ServiceAccountPath()))
			}
			retireLookupUser = func(name string) (*user.User, error) {
				if name != account.User {
					t.Fatalf("looked up an unrecorded user: %q", name)
				}
				u := &user.User{Username: name, Uid: "123", Gid: "456", HomeDir: retirement.Root}
				switch scenario {
				case "uid":
					u.Uid = "789"
				case "gid":
					u.Gid = "789"
				case "home":
					u.HomeDir = "/unrelated"
				}
				return u, nil
			}
			retireLookupGroup = func(name string) (*user.Group, error) {
				if name != account.Group {
					t.Fatalf("looked up an unrecorded group: %q", name)
				}
				return &user.Group{Name: name, Gid: "456"}, nil
			}
			forbidNodeConfigWrites(t, f)
			out, code := runNodeConfigCheck(t, f, j, nodeConfigDocument(t, f, j, mustRead(t, f.cfg), emptyNodeOperations()))
			if scenario == "matching" {
				if code != 0 || retireAnswer(t, out)["outcome"] != "admitted" {
					t.Fatalf("persistent-mask control refused: %s", out)
				}
				return
			}
			wantCode, wantReason := exitRefused, "retired-account-mismatch"
			if scenario == "missing record" {
				wantCode, wantReason = exitUnknown, "retired-account-observation"
			}
			if code != wantCode || retireAnswer(t, out)["reason"] != wantReason {
				t.Fatalf("%s reached ordinary admission or another refusal: %s", scenario, out)
			}
		})
	}
}
