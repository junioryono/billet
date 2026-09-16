package main

import (
	"context"
	"os/user"
	"strconv"

	"github.com/junioryono/billet/internal/retirement"
)

var retireLookupUser = user.Lookup
var retireLookupGroup = user.LookupGroup

// A persistent mask has no executable User/Group. Its installed account is
// proved from the trusted retirement record and today's local account database.
func proveRetireMaskedAccount(ctx context.Context) *retireRefusal {
	props, err := retireOperationInspector().UnitProperties(ctx, serverUnit, "LoadState", "UnitFileState")
	if err != nil || len(props["LoadState"]) != 1 || len(props["UnitFileState"]) != 1 {
		return retireUnknown("retired-account-observation", "server load state could not be read", "")
	}
	switch firstProp(props, "LoadState") {
	case "not-found", "loaded":
		return nil
	case "masked":
		if firstProp(props, "UnitFileState") != "masked" {
			return retireRefuse("retired-account-mask", "only a persistent server mask admits the retired account observation", "")
		}
	default:
		return retireUnknown("retired-account-observation", "server load state is unsupported", "")
	}
	account, err := retirement.ReadRetiredServiceAccount()
	if err != nil {
		return retireUnknown("retired-account-observation", err.Error(), "")
	}
	usr, userErr := retireLookupUser(account.User)
	grp, groupErr := retireLookupGroup(account.Group)
	if userErr != nil || groupErr != nil {
		return retireUnknown("retired-account-observation", "recorded local account or group could not be read", "")
	}
	if usr.Username != account.User || usr.Uid != strconv.Itoa(account.UID) || usr.Gid != strconv.Itoa(account.GID) ||
		grp.Name != account.Group || grp.Gid != strconv.Itoa(account.GID) || usr.HomeDir != retirement.Root {
		return retireRefuse("retired-account-mismatch", "recorded local names, numeric IDs or service-account home disagree", "")
	}
	return nil
}
