package main

import (
	"context"
	"errors"
	"os/user"
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
