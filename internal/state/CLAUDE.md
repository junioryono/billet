# internal/state

The control-plane ledger: SQLite or PostgreSQL behind one seam, the migrations, the sqlc query set, the locks and fences, and the controller claim. Load `billet-state` before changing anything here, and `billet-add-migration` to add a migration or a query.

- A write is `DB.Tx`, which begins IMMEDIATE and holds the single writer slot. An operation that only reads uses `DB.View`, never `DB.Tx`; a read that decides or validates a write stays inside that same `DB.Tx`, through `ReadQueries(tx)`, or the decision and its write come apart.
- Production files reach no network, run no subprocess and import no upper layer: the `ledgerwriters` depguard rule refuses it, because a call inside a transaction holds the writer slot. Test files are exempt, and a second process that proves a lock belongs there.
- A published migration is never edited, reformatting included: add the next dense version, as a SQLite file and its PostgreSQL twin, then run `make sqlc`.
- SQL lives only in `queries/*.sql`, and `ledgerdb/` is what sqlc generates from it; never edit `ledgerdb/`.
- The claim's epoch is re-read inside every write transaction, and a lost claim stops the process, not only the write.

Gates: `make check`; `make sqlc-check` after a query or migration; `BILLET_TEST_POSTGRES_DSN` to run the PostgreSQL suite.
