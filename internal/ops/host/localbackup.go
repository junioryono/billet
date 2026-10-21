package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/junioryono/billet/internal/app"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/deployarchive"
	"github.com/junioryono/billet/internal/hostauthority"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// backupOptions is what runLocalBackup acts on.
type backupOptions struct {
	configPath string
	out        string
	noUpload   bool
}

// cmdLocalBackup captures this deployment as ONE unit.
//
// THE UNIT IS FOUR THINGS AND EACH IS USELESS WITHOUT THE OTHERS: the ledger (a
// SQLite file, or a PostgreSQL database's dump), the deployment identity, the
// GitHub App private key and the node-wire certificate authority. A ledger
// without its identity is a fresh authority that cannot see the compute the old
// one launched; an identity without the CA cannot issue a node certificate; a CA
// without the App key cannot get a token. So this captures all four, or, on a
// PostgreSQL host that cannot run pg_dump, the other three and a failure that
// says the ledger is missing.
//
// IT RUNS AGAINST A LIVE CONTROL PLANE. The ledger copy is SQLite's VACUUM INTO
// or pg_dump, each a consistent snapshot of a database somebody is writing to,
// and the handle it uses is the operator one that does not need the directory
// lock the server is holding.
//
// WHAT IT DELIBERATELY DOES NOT CAPTURE is said out loud at the end: node-local
// custody state, which must never be restored blindly while compute may still
// exist, and cache data, which repopulates.
func cmdLocalBackup(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet local backup", env.Stdout)
	cfgPath := addServiceConfigFlag(fs)
	out := fs.String("out", "", "the directory to write the backup to (created, must be empty)")
	noUpload := fs.Bool("no-upload", false,
		"write the archive and do NOT copy it to backup.s3, even though the config names one")

	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	if *out == "" {
		return errors.New("billet local backup needs --out <dir>, a directory to write the " +
			"backup to")
	}

	return runLocalBackup(ctx, env, backupOptions{
		configPath: *cfgPath, out: *out, noUpload: *noUpload,
	})
}

func runLocalBackup(ctx context.Context, env cli.Env, o backupOptions) error {
	cfg, err := config.Load(o.configPath)
	if err != nil {
		return err
	}

	if cfg.Server == nil {
		return fmt.Errorf("%s declares no control plane, and a backup is of one: the ledger, the "+
			"deployment identity and the node-wire authority all live in server.state_dir. Run "+
			"this on the host that runs `billet server`", o.configPath)
	}

	targets := cfg.GitHubTargets()
	if len(targets) == 0 {
		return fmt.Errorf("%s has no github section and no targets, so there is no App key to "+
			"capture — and the key is part of the deployment unit, not an extra. Run `billet "+
			"github-app create` first", o.configPath)
	}

	dest, err := filepath.Abs(o.out)
	if err != nil {
		return fmt.Errorf("resolve --out %s: %w", o.out, err)
	}

	// THE BLANKET REFUSAL THAT USED TO BE HERE IS GONE, and deleting it is half of
	// the fix rather than tidying. It returned before anything was written, which
	// was right while an identity-only archive did not exist — the package had no
	// form for one, and deployarchive.Write refused from INSIDE, after the
	// identity, the authority and the App key had already landed in the
	// destination.
	//
	// The package has that form now (schema 2), so the refusal is what stands
	// between this deployment and the only copy of its identity there is. Left in
	// place it also made every line below unreachable for precisely the
	// deployment they were written for — found by review, and invisible to the
	// tests, which drove deployarchive.Write directly.

	// PEEKED, NEVER MINTED. state.DeploymentID CREATES an identity when a
	// directory has none, so using it here would make `billet local backup`
	// commission an uncommissioned host as a side effect of being asked about it.
	deployment, found, err := state.PeekDeploymentID(cfg.Server.IdentityDir)
	if err != nil {
		return err
	}

	if !found {
		return fmt.Errorf("%s holds no deployment identity, so there is no deployment here to "+
			"back up yet. A control plane mints one the first time it starts",
			cfg.Server.IdentityDir)
	}

	// THE HARDENED READ, not os.ReadFile. One descriptor opened O_NONBLOCK so a
	// FIFO cannot hang this, regular-file, bounded, mode-checked and actually
	// parsed — the same rules `billet check` and `billet server` apply, because a
	// second implementation of them is a second thing to keep right.
	appKey, err := app.ResolveAppKey(ctx, cfg, targets[0])
	if err != nil {
		return err
	}

	// AND EVERY FURTHER TARGET'S, all or none: an archive missing one owner's key
	// restores a control plane that serves the others and fails that one hours
	// later with a bare 401.
	further := make([]deployarchive.TargetKey, 0, len(targets))

	for _, target := range targets[1:] {
		pem, err := app.ResolveAppKey(ctx, cfg, target)
		if err != nil {
			return fmt.Errorf("target %s: %w", target.Name, err)
		}

		further = append(further, deployarchive.TargetKey{
			Name: target.Name, GitHub: archiveIdentity(target), AppKeyPEM: pem,
		})
	}

	// COPIED FOR REFERENCE AND NEVER INSTALLED BY A RESTORE. It records what the
	// deployment looked like, and its paths belong to this host.
	configBody, err := os.ReadFile(o.configPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", o.configPath, err)
	}

	// THE EXCLUSION BEFORE THE LEDGER OPEN, which creates on a fresh directory,
	// and lent to the archive's write below so it does not take it again.
	acc, err := hostauthority.Open(ctx, cfg.Server.IdentityDir, hostauthority.Intent{Wait: hostauthority.Wait})
	if err != nil {
		return err
	}

	defer func() {
		if err := acc.Release(); err != nil {
			fmt.Fprintf(env.Stdout, "warn     could not release the authority exclusion: %v\n", err)
		}
	}()

	db, err := openStateAdmin(ctx, cfg)
	if err != nil {
		return fmt.Errorf("server state: %w", err)
	}

	defer func() { _ = db.Close() }()

	host, err := os.Hostname()
	if err != nil {
		host = ""
	}

	fmt.Fprintf(env.Stdout, "backup   %s\n", dest)
	fmt.Fprintf(env.Stdout, "         deployment %s\n", deployment)
	if cfg.Server.LedgerBackend() != config.StatePostgres {
		fmt.Fprintf(env.Stdout, "         the ledger snapshot holds the single writer slot while it runs\n\n")
	}

	// THE LEDGER TRAVELS AS ITS ENGINE'S OWN CONSISTENT COPY.
	//
	// SQLite's VACUUM INTO produces one as a file. For PostgreSQL it is pg_dump,
	// never rows copied through this connection, which would produce an archive
	// that LOOKS like a backup and is not (ADR-008). For a control plane built by
	// the control-plane-postgres module this archive is the only recovery path
	// there is for the identity half — no ledger volume by design, a root volume
	// that is delete_on_termination, and an App key GitHub issues exactly once —
	// and with the dump beside it, for the ledger half too.
	src, err := ledgerSource(ctx, cfg, db)
	if err != nil {
		return err
	}

	switch {
	case src.dump != nil:
		fmt.Fprintf(env.Stdout, "         the ledger is PostgreSQL and is copied by its own pg_dump, in one\n")
		fmt.Fprintf(env.Stdout, "         snapshot, without holding any write\n\n")
	case src.external != nil:
		fmt.Fprintf(env.Stdout, "warn     the ledger is PostgreSQL and this host cannot copy it: %v\n", src.clientErr)
		fmt.Fprintf(env.Stdout, "         The identity, the authority and the App key are captured anyway, and\n")
		fmt.Fprintf(env.Stdout, "         this command fails afterwards so the missing ledger is not mistaken for\n")
		fmt.Fprintf(env.Stdout, "         a backup\n\n")
	}

	m, err := deployarchive.Write(ctx, deployarchive.BackupRequest{
		Dest:           dest,
		StateDir:       cfg.Server.IdentityDir,
		ConfigPath:     o.configPath,
		DeploymentID:   deployment,
		GitHub:         archiveIdentity(targets[0]),
		Targets:        further,
		AppKeyPEM:      appKey,
		ConfigBody:     configBody,
		Snapshot:       src.snapshot,
		Dump:           src.dump,
		ExternalLedger: src.external,
		Now:            time.Now,
		Hostname:       host,
		Authority:      acc.Lock(),
	})
	if err != nil {
		return err
	}

	printBackup(env, m, dest)

	// AFTER THE LOCAL ARCHIVE IS WRITTEN AND VERIFIED, and never instead of it.
	// A backup nobody can see is not one, so the directory is always there before
	// anything is asked of a network.
	if o.noUpload {
		fmt.Fprintln(env.Stdout)
		fmt.Fprintf(env.Stdout, "note     --no-upload: this archive was NOT copied to backup.s3, so it is on\n")
		fmt.Fprintf(env.Stdout, "         the same disk as the deployment it protects\n")

		return src.missingLedger(dest)
	}

	return errors.Join(uploadArchive(ctx, env, cfg, dest, deployment), src.missingLedger(dest))
}

// ledgerCopy is how a backup's ledger travels: exactly one of the three is set.
type ledgerCopy struct {
	snapshot func(context.Context, string) error
	dump     func(context.Context, string) (deployarchive.LedgerDump, error)
	// external is an identity-only archive's description of the ledger, taken
	// only when this host cannot run pg_dump; clientErr says why.
	external  *deployarchive.ExternalLedger
	clientErr error
}

// missingLedger is the failure a backup reports after it has written and sent
// an identity-only archive, naming what to install.
func (l ledgerCopy) missingLedger(dest string) error {
	if l.clientErr == nil {
		return nil
	}

	return fmt.Errorf("the archive at %s holds the deployment identity, the node-wire "+
		"authority and the App key and NOT the PostgreSQL ledger, because this host cannot run "+
		"pg_dump for it: %w", dest, l.clientErr)
}

// ledgerSource decides how this deployment's ledger travels in the archive.
//
// EXACTLY ONE WAY, which BackupRequest.validate re-checks: a SQLite ledger is a
// VACUUM INTO snapshot, a PostgreSQL ledger is a pg_dump of the snapshot billet
// read its migrations in.
//
// THE CLIENT PROGRAM IS PROVED BEFORE ANYTHING IS WRITTEN, and a host that
// cannot run it still captures the rest. pg_dump is the operator's, installed
// with the PostgreSQL client; when it is absent or older than the server, the
// archive is the identity-only one older billets wrote (the identity, the
// authority and the App key GitHub issues once, with the ledger recorded as
// external), and the command then fails naming what to install. Refusing
// outright would leave a packaged timer capturing nothing at all on the day the
// client went missing, which is exactly when the identity half is the only
// half a replacement host can be rebuilt from.
func ledgerSource(ctx context.Context, cfg *config.Config, db *state.DB) (ledgerCopy, error) {
	if cfg.Server.LedgerBackend() != config.StatePostgres {
		return ledgerCopy{snapshot: db.SnapshotInto}, nil
	}

	client, err := postgresClient(cfg)
	if err != nil {
		return ledgerCopy{}, err
	}

	if clientErr := client.CheckDump(ctx); clientErr != nil {
		applied, err := db.AppliedMigrations(ctx)
		if err != nil {
			return ledgerCopy{}, errors.Join(clientErr,
				fmt.Errorf("read what this ledger has applied: %w", err))
		}

		return ledgerCopy{
			external: &deployarchive.ExternalLedger{
				Backend:    string(config.StatePostgres),
				DSNEnv:     cfg.Server.LedgerDSNEnv(),
				Migrations: applied,
			},
			clientErr: clientErr,
		}, nil
	}

	dump := func(ctx context.Context, dest string) (deployarchive.LedgerDump, error) {
		// THE IDENTITY DIRECTORY'S FENCE IS HONOURED, as SnapshotInto honours
		// its handle's: a ledger an upgrade or a restore has closed is not copied.
		d, err := client.Dump(ctx, cfg.Server.IdentityDir, dest)
		if err != nil {
			return deployarchive.LedgerDump{}, err
		}

		out := d.LedgerDump()
		// THE VARIABLE'S NAME, NEVER ITS VALUE. A DSN carries a password and this
		// manifest travels off-site.
		out.DSNEnv = cfg.Server.LedgerDSNEnv()

		return out, nil
	}

	return ledgerCopy{dump: dump}, nil
}

// printBackup reports what was captured, and what an archive is not.
func printBackup(env cli.Env, m deployarchive.Manifest, dest string) {
	for _, f := range m.Files {
		fmt.Fprintf(env.Stdout, "wrote    %s (%d bytes)\n", filepath.Join(dest, f.Path), f.Size)
	}

	fmt.Fprintln(env.Stdout)
	fmt.Fprintf(env.Stdout, "ledger   schema %d (%d migrations applied)\n",
		m.Ledger.HighestVersion(), len(m.Ledger.Migrations))

	if d := m.Ledger.Dump; d != nil {
		fmt.Fprintf(env.Stdout, "         a pg_dump %d copy of PostgreSQL schema %s (server %d)\n",
			d.DumpVersion, d.Schema, d.ServerVersion)
	}

	fmt.Fprintf(env.Stdout, "app      %s\n", m.GitHub)
	fmt.Fprintf(env.Stdout, "ca       %s, expires %s\n", m.Authority.Fingerprint, m.Authority.NotAfter)

	if m.Authority.Rotating {
		fmt.Fprintf(env.Stdout, "         A ROTATION IS RUNNING: the previous authority (%s) is in this backup\n",
			m.Authority.PreviousFingerprint)
		fmt.Fprintf(env.Stdout, "         too, because its key signs what the control plane presents until\n")
		fmt.Fprintf(env.Stdout, "         every node has renewed.\n")
	}

	// NAMED RATHER THAN SUMMARISED. A leftover from an interrupted rotation is
	// not authority state and is deliberately not in the archive, so an operator
	// who has one needs to hear about it here rather than wonder later why it did
	// not travel.
	if leftovers := wirecert.RotationLeftovers(m.Authority.UnexpectedFilesPresent); len(leftovers) > 0 {
		fmt.Fprintln(env.Stdout)

		for _, path := range leftovers {
			fmt.Fprintf(env.Stdout, "note     %s looks like an interrupted `billet ca rotate`. It is NOT in\n", path)
			fmt.Fprintf(env.Stdout, "         this backup — it is not authority state — and is worth looking at.\n")
		}
	} else {
		for _, path := range m.Authority.UnexpectedFilesPresent {
			fmt.Fprintf(env.Stdout, "note     %s is not part of the authority and was not captured\n", path)
		}
	}

	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "Restore it with:")
	fmt.Fprintf(env.Stdout, "\n  billet local restore --from %s --dry-run\n\n", dest)
	fmt.Fprintln(env.Stdout, "NOT in this backup, deliberately:")
	fmt.Fprintln(env.Stdout, "         node custody state, which must never be restored blindly while compute")
	fmt.Fprintln(env.Stdout, "         from after the backup may still be running")
	fmt.Fprintln(env.Stdout, "         cache data, which repopulates, and guest images, which are pulled again")
}
