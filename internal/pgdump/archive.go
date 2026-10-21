package pgdump

import (
	"context"

	"github.com/junioryono/billet/internal/deployarchive"
)

// Archive is a Client as internal/deployarchive's PostgresLedger: the database
// an archive's dump is restored into, or a recovery replaces the ledger in.
//
// ONE ADAPTER, for the command and the harnesses alike, so the end-to-end
// suite restores through the same seam `billet local restore` does.
type Archive struct{ *Client }

var _ deployarchive.PostgresLedger = Archive{}

// Dump crosses this host's fence: the archive calls it only for a recovery's
// copy of the ledger it replaces, behind the fence that recovery raised.
func (a Archive) Dump(ctx context.Context, dest string) (deployarchive.LedgerDump, error) {
	d, err := a.Client.Dump(ctx, "", dest)
	if err != nil {
		return deployarchive.LedgerDump{}, err
	}

	return d.LedgerDump(), nil
}

// LedgerDump is what pg_dump took, as an archive records it.
func (d Dumped) LedgerDump() deployarchive.LedgerDump {
	return deployarchive.LedgerDump{
		Facts: deployarchive.DumpFacts{
			Schema:        d.Schema,
			ServerVersion: d.ServerVersion,
			DumpVersion:   d.DumpVersion,
			Binding:       d.Binding,
		},
		Migrations: d.Migrations,
	}
}
