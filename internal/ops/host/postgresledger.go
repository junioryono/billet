package host

import (
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/deployarchive"
	"github.com/junioryono/billet/internal/pgdump"
)

// postgresClient renders this host's ledger connection for the PostgreSQL
// client programs.
func postgresClient(cfg *config.Config) (*pgdump.Client, error) {
	dsn, err := app.LedgerDSN(cfg)
	if err != nil {
		return nil, err
	}

	return pgdump.New(dsn)
}

// reachesArchiveLedger reports whether a restore or recovery of this archive on
// this host reaches a database: only an archive carrying a PostgreSQL dump, and
// only on a host configured for PostgreSQL. An identity-only archive restores
// without the database's connection string, as it always has, and a mismatched
// host is refused by the planner, which names both sides.
func reachesArchiveLedger(cfg *config.Config, a *deployarchive.Archive) bool {
	return a.Manifest.Ledger.IsDump() && cfg.Server.LedgerBackend() == config.StatePostgres
}

// archiveTargetLedger is the database an archive carrying a PostgreSQL dump is
// put into.
func archiveTargetLedger(cfg *config.Config) (deployarchive.PostgresLedger, error) {
	client, err := postgresClient(cfg)
	if err != nil {
		return nil, err
	}

	// ONE SERVER, refused here, before the command seals, fences or copies
	// anything: a restore writes through several connections.
	if err := client.SingleEndpoint(); err != nil {
		return nil, err
	}

	return pgdump.Archive{Client: client}, nil
}
