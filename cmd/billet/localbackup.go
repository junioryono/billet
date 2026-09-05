package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/deployarchive"
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
// THE UNIT IS FOUR THINGS AND EACH IS USELESS WITHOUT THE OTHERS: the SQLite
// ledger, the deployment identity, the GitHub App private key and the node-wire
// certificate authority. A ledger without its identity is a fresh authority that
// cannot see the compute the old one launched; an identity without the CA cannot
// issue a node certificate; a CA without the App key cannot get a token. So this
// captures all four or it captures nothing.
//
// IT RUNS AGAINST A LIVE CONTROL PLANE. The ledger copy is SQLite's VACUUM INTO,
// which is a consistent snapshot of a database somebody is writing to, and the
// handle it uses is the operator one that does not need the directory lock the
// server is holding.
//
// WHAT IT DELIBERATELY DOES NOT CAPTURE is said out loud at the end: node-local
// custody state, which must never be restored blindly while compute may still
// exist, and cache data, which repopulates.
func cmdLocalBackup(ctx context.Context, args []string) error {
	fs := newFlagSet("billet local backup")
	cfgPath := addServiceConfigFlag(fs)
	out := fs.String("out", "", "the directory to write the backup to (created, must be empty)")
	noUpload := fs.Bool("no-upload", false,
		"write the archive and do NOT copy it to backup.s3, even though the config names one")

	if err := parse(fs, args); err != nil {
		return err
	}

	if *out == "" {
		return errors.New("billet local backup needs --out <dir>, a directory to write the " +
			"backup to")
	}

	return runLocalBackup(ctx, backupOptions{
		configPath: *cfgPath, out: *out, noUpload: *noUpload,
	})
}

func runLocalBackup(ctx context.Context, o backupOptions) error {
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
	appKey, err := resolveAppKey(ctx, cfg, targets[0])
	if err != nil {
		return err
	}

	// AND EVERY FURTHER TARGET'S, all or none: an archive missing one owner's key
	// restores a control plane that serves the others and fails that one hours
	// later with a bare 401.
	further := make([]deployarchive.TargetKey, 0, len(targets))

	for _, target := range targets[1:] {
		pem, err := resolveAppKey(ctx, cfg, target)
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

	db, err := openStateAdmin(ctx, cfg)
	if err != nil {
		return fmt.Errorf("server state: %w", err)
	}

	defer func() { _ = db.Close() }()

	host, err := os.Hostname()
	if err != nil {
		host = ""
	}

	fmt.Printf("backup   %s\n", dest)
	fmt.Printf("         deployment %s\n", deployment)
	if cfg.Server.LedgerBackend() != config.StatePostgres {
		fmt.Printf("         the ledger snapshot holds the single writer slot while it runs\n\n")
	}

	// THE LEDGER IS BILLET'S TO COPY ONLY WHEN BILLET HOLDS IT.
	//
	// SQLite's VACUUM INTO produces a consistent copy of the whole ledger as one
	// file. There is no equivalent billet should own for PostgreSQL: a consistent
	// copy there is pg_dump or the provider's snapshot, both the operator's to run
	// and to restore, and copying rows through this connection would produce an
	// archive that LOOKS like a backup and is not.
	//
	// UNTIL THIS EXISTED THE WHOLE COMMAND FAILED on such a deployment — so the
	// half billet DOES own went uncaptured too, and for a control plane built by
	// the control-plane-postgres module that is the only recovery path there is:
	// no ledger volume by design, a root volume that is delete_on_termination,
	// and an App key GitHub issues exactly once.
	snapshot, external, err := ledgerSource(ctx, cfg, db)
	if err != nil {
		return err
	}

	if external != nil {
		fmt.Printf("         the ledger is %s and is NOT in this archive; your database's own\n",
			external.Backend)
		fmt.Printf("         backup is the other half\n\n")
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
		Snapshot:       snapshot,
		ExternalLedger: external,
		Now:            time.Now,
		Hostname:       host,
	})
	if err != nil {
		return err
	}

	printBackup(m, dest)

	// AFTER THE LOCAL ARCHIVE IS WRITTEN AND VERIFIED, and never instead of it.
	// A backup nobody can see is not one, so the directory is always there before
	// anything is asked of a network.
	if o.noUpload {
		fmt.Println()
		fmt.Printf("note     --no-upload: this archive was NOT copied to backup.s3, so it is on\n")
		fmt.Printf("         the same disk as the deployment it protects\n")

		return nil
	}

	return uploadArchive(ctx, cfg, dest, deployment)
}

// ledgerSource decides whether this deployment's ledger travels in the archive.
//
// EXACTLY ONE OF THE TWO IS NON-NIL, which BackupRequest.validate re-checks: an
// archive carries its ledger or declares it external, and a request claiming
// both would write a snapshot AND a manifest saying there is none.
//
// THE MIGRATION LIST FOR AN EXTERNAL LEDGER COMES FROM THE LIVE DATABASE,
// because there is no snapshot to read it back from. It is provenance rather
// than a description of an artifact — see deployarchive.LedgerFacts for why a
// stale answer is safe in the only direction it can be stale.
func ledgerSource(
	ctx context.Context, cfg *config.Config, db *state.DB,
) (func(context.Context, string) error, *deployarchive.ExternalLedger, error) {
	if cfg.Server.LedgerBackend() != config.StatePostgres {
		return db.SnapshotInto, nil, nil
	}

	applied, err := db.AppliedMigrations(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read what this ledger has applied: %w", err)
	}

	external := &deployarchive.ExternalLedger{
		Backend:    string(config.StatePostgres),
		Migrations: applied,
	}

	// THE VARIABLE'S NAME, NEVER ITS VALUE. A DSN carries a password and this
	// manifest travels off-site; the indirection is the rule PostgresStateConfig
	// already states.
	if cfg.Server.State != nil && cfg.Server.State.Postgres != nil {
		external.DSNEnv = cfg.Server.State.Postgres.DSNEnv
	}

	return nil, external, nil
}

// printBackup reports what was captured, and what an archive is not.
func printBackup(m deployarchive.Manifest, dest string) {
	for _, f := range m.Files {
		fmt.Printf("wrote    %s (%d bytes)\n", filepath.Join(dest, f.Path), f.Size)
	}

	fmt.Println()
	fmt.Printf("ledger   schema %d (%d migrations applied)\n",
		m.Ledger.HighestVersion(), len(m.Ledger.Migrations))
	fmt.Printf("app      %s\n", m.GitHub)
	fmt.Printf("ca       %s, expires %s\n", m.Authority.Fingerprint, m.Authority.NotAfter)

	if m.Authority.Rotating {
		fmt.Printf("         A ROTATION IS RUNNING: the previous authority (%s) is in this backup\n",
			m.Authority.PreviousFingerprint)
		fmt.Printf("         too, because its key signs what the control plane presents until\n")
		fmt.Printf("         every node has renewed.\n")
	}

	// NAMED RATHER THAN SUMMARISED. A leftover from an interrupted rotation is
	// not authority state and is deliberately not in the archive, so an operator
	// who has one needs to hear about it here rather than wonder later why it did
	// not travel.
	if leftovers := wirecert.RotationLeftovers(m.Authority.UnexpectedFilesPresent); len(leftovers) > 0 {
		fmt.Println()

		for _, path := range leftovers {
			fmt.Printf("note     %s looks like an interrupted `billet ca rotate`. It is NOT in\n", path)
			fmt.Printf("         this backup — it is not authority state — and is worth looking at.\n")
		}
	} else {
		for _, path := range m.Authority.UnexpectedFilesPresent {
			fmt.Printf("note     %s is not part of the authority and was not captured\n", path)
		}
	}

	fmt.Println()
	fmt.Println("Restore it with:")
	fmt.Printf("\n  billet local restore --from %s --dry-run\n\n", dest)
	fmt.Println("NOT in this backup, deliberately:")
	fmt.Println("         node custody state, which must never be restored blindly while compute")
	fmt.Println("         from after the backup may still be running")
	fmt.Println("         cache data, which repopulates, and guest images, which are pulled again")
}
