package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/wirecert"
)

// hostPrepareAnswer is what `billet local prepare --json` prints: the published
// authority status the installer decides its starts and enables from, and what
// the bootstrap did.
type hostPrepareAnswer struct {
	Schema   int      `json:"schema"`
	Status   string   `json:"status"`
	Closed   bool     `json:"closed"`
	Variant  string   `json:"variant,omitempty"`
	Created  bool     `json:"created_identity_dir"`
	Repaired []string `json:"repaired"`
	Account  string   `json:"account"`
}

// cmdLocalPrepare moves a host onto the global authority exclusion, or repairs
// its metadata: the privileged bootstrap the package's postinstall, `local up`
// and the host role run. IT AUTHORISES NOTHING ELSE. A closed status (a retired
// controller) is published back to the caller, which then refuses whatever a
// closed authority forbids, and the record is restored either way.
//
// Linux only, root only. The account is the packaged one unless the caller
// names another (the role passes the account it validated); the identity
// directory is the configuration's server directory when a configuration
// exists, else the packaged default, so a fresh package install prepares the
// directory the server will use before any metadata exists beside it.
func cmdLocalPrepare(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("billet local prepare", flag.ContinueOnError)
	cfgPath := addServiceConfigFlag(flags)
	account := flags.String("account", initconfig.ServiceGroup, "the service account the units run as")
	group := flags.String("group", "", "the service group (defaults to the account's name)")
	asJSON := flags.Bool("json", false, "print the answer as JSON")
	wait := flags.Duration("wait", time.Minute, "how long to wait for a lock another billet holds")

	if err := flags.Parse(args); err != nil {
		return err
	}

	if !retirement.Supported(hostOS) {
		return fmt.Errorf("billet local prepare: the global authority exclusion is Linux's; a %s host keeps "+
			"the identity directory's own lock and needs no preparation", hostOS)
	}

	if os.Geteuid() != 0 {
		return errors.New("billet local prepare: only root prepares a host (run it with sudo)")
	}

	if *group == "" {
		*group = *account
	}

	identityDir := serverIdentityDirForPrepare(*cfgPath)

	acct, err := lookupServiceAccount(*account, *group)
	if err != nil {
		return err
	}

	res, err := retirement.Bootstrap(ctx, retirement.BootstrapRequest{
		Account:     acct,
		IdentityDir: identityDir,
		InnerLock:   wirecert.AuthorityLockPath(identityDir),
		Wait:        *wait,
	})
	if err != nil {
		return fmt.Errorf("billet local prepare: %w", err)
	}

	ans := hostPrepareAnswer{
		Schema: 1, Status: "absent", Created: res.Created, Repaired: res.Repaired,
		Account: fmt.Sprintf("%s:%s", acct.User, acct.Group),
	}

	if res.Presence == retirement.StatusPresent {
		ans.Status = string(res.Status.Phase)
		ans.Closed = res.Status.Phase.Closed()
		ans.Variant = string(res.Status.Variant)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)

		return enc.Encode(ans)
	}

	fmt.Printf("prepared %s for %s\n", identityDir, ans.Account)

	if res.Created {
		fmt.Printf("created  %s\n", identityDir)
	}

	for _, path := range res.Repaired {
		fmt.Printf("own      %s\n", path)
	}

	if ans.Closed {
		fmt.Printf("closed   this controller's authority is closed (retirement at %s); nothing here starts it\n", ans.Status)
	}

	return nil
}

// serverIdentityDirForPrepare is the server's identity directory: the
// configuration's when one loads, else the packaged default, because the
// package's postinstall runs before any configuration says anything true.
func serverIdentityDirForPrepare(cfgPath string) string {
	if cfg, err := config.Load(cfgPath); err == nil && cfg.Server != nil && cfg.Server.IdentityDir != "" {
		return cfg.Server.IdentityDir
	}

	return config.DefaultServerStateDir()
}

// lookupServiceAccount resolves the account the installer validated into the
// record the exclusion consults; an account that does not exist is a refusal
// naming it, never a guess.
func lookupServiceAccount(user, group string) (retirement.ServiceAccount, error) {
	uid, gid, err := converge().Identity(lifeops.UpRequest{ServiceUser: user, ServiceGroup: group})
	if err != nil {
		return retirement.ServiceAccount{}, fmt.Errorf("billet local prepare: the service account %s:%s: %w", user, group, err)
	}

	return retirement.ServiceAccount{User: user, UID: uid, Group: group, GID: gid}, nil
}
