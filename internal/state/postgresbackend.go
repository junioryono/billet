package state

import (
	"context"
	"database/sql"
	_ "embed" // the bookkeeping DDL is read from schema_migrations_postgres.sql.
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	// The database/sql driver. Imported by name rather than blank because
	// RegisterConnConfig is what adds billet's startup parameters without this
	// package owning a DSN parser. depguard confines both to this package.
	"github.com/jackc/pgx/v5/stdlib"
)

//go:embed schema_migrations_postgres.sql
var bootstrapSchemaMigrationsPostgres string

// postgresBackend is the ledger in a database billet does not run.
//
// WHAT IT BUYS is a controller that is replaceable: the scheduling state
// outlives the machine, so recovery is somebody else's managed backup rather
// than a directory restored onto local storage. What it does NOT buy on its own
// is high availability — exactly one controller may make scheduling decisions
// either way, and a database's ability to serialize writes is not proof that
// only one process is polling GitHub.
type postgresBackend struct {
	// dsn is the connection string, which arrives from the environment rather
	// than from the config file: it carries a password, and a secret in YAML ends
	// up in a backup, a paste buffer and eventually a support thread.
	dsn string

	// lockKey identifies THE LEDGER, and is read from the server rather than
	// derived from anything on this machine. See readLockKey for why that
	// distinction is the whole value of the lock.
	lockKey int64

	// claim is the connection holding the controller claim once this process has
	// taken it. One connection, never idled out: the lock lives in the SESSION,
	// so a recycled connection is a released lock. See claimController for what
	// that does and does not guarantee.
	claim *sql.DB
}

var _ backend = (*postgresBackend)(nil)

func newPostgresBackend(dsn string) *postgresBackend {
	return &postgresBackend{dsn: dsn}
}

func (*postgresBackend) engine() string      { return "postgres" }
func (*postgresBackend) driverName() string  { return "pgx" }
func (*postgresBackend) timeline() *timeline { return pgTimeline }

// sharedLedger is true, and it is the fact this whole backend exists to provide:
// the rows outlive the machine, so a controller is replaceable. The cost is that
// nothing local proves anything about them — a second host takes its own state
// directory's lock happily — which is why the controller exclusion here is a
// separate act with something real to release.
func (*postgresBackend) sharedLedger() bool { return true }

// dataSources adds the two settings that are safety properties rather than
// tuning, one per pool.
//
// lock_timeout BOUNDS THE WRITER'S WAIT so that contention comes back as an
// error this package can classify and retry, on the caller's own terms, instead
// of blocking inside the server past a deadline the caller thought it had. It is
// the same argument as SQLite's deliberately short busy_timeout: the waiting
// belongs in Go, where the context is real.
//
// default_transaction_read_only IS THE READER'S REFUSAL. It is the counterpart
// of SQLite's query_only, and it matters for the same reason: the narrow Querier
// interface stops a caller writing by accident, and this stops the engine
// carrying out a write that somehow reached it anyway.
func (b *postgresBackend) dataSources() (ledgerPools, error) {
	if strings.TrimSpace(b.dsn) == "" {
		return ledgerPools{}, errors.New(
			"state: the PostgreSQL data source is empty; it is read from the environment " +
				"variable named by server.state.postgres.dsn_env")
	}

	writer, err := registerConn(b.dsn, map[string]string{"lock_timeout": "50"})
	if err != nil {
		return ledgerPools{}, err
	}

	reader, err := registerConn(b.dsn, map[string]string{"default_transaction_read_only": "on"})
	if err != nil {
		return ledgerPools{}, err
	}

	return ledgerPools{writer: writer, reader: reader}, nil
}

// registerConn parses the operator's DSN, adds billet's own startup parameters
// and hands back the opaque name database/sql should open.
//
// THROUGH pgx's OWN REGISTRY rather than by editing the DSN text, because an
// operator's DSN may be a URL or a key/value string and appending to either by
// hand is a parser billet would then own. RuntimeParams are sent as startup
// parameters, which is also what keeps them out of reach of a later statement.
func registerConn(dsn string, params map[string]string) (string, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		// THE DSN IS NOT IN THE MESSAGE. It carries a password, and a startup
		// failure is the most likely thing to be pasted into an issue.
		return "", fmt.Errorf("state: the PostgreSQL data source could not be parsed: %w", err)
	}

	for k, v := range params {
		cfg.RuntimeParams[k] = v
	}

	return stdlib.RegisterConnConfig(cfg), nil
}

// verifyDurability is the counterpart of SQLite's pragma readback, and it asks
// the question that survives the change of engine: will a committed transaction
// still be there after the machine loses power.
//
// synchronous_commit=off ANSWERS NO. PostgreSQL acknowledges the commit before
// the WAL record reaches disk, so a crash can lose recently committed
// transactions — which is exactly what SQLite's synchronous=FULL exists to
// prevent, and the capacity ledger is the thing that must survive an unclean
// shutdown for restart reconciliation to work. `local` and `remote_write` and
// above all flush locally, so they are accepted; only `off` is refused.
//
// THE READ-ONLY DEFAULT IS CHECKED ON THE WRITER for the opposite reason: a
// deployment whose role or database has been set read-only would fail every
// scheduling write later, one lease at a time, rather than at startup.
func (*postgresBackend) verifyDurability(ctx context.Context, w *sql.DB) error {
	var errs []error

	var synchronous string
	//billet:ignore rawsql // SHOW reads a server setting; sqlc has no catalogue entry for one
	if err := w.QueryRowContext(ctx, `SHOW synchronous_commit`).Scan(&synchronous); err != nil {
		return fmt.Errorf("read synchronous_commit: %w", err)
	}

	if strings.EqualFold(synchronous, "off") {
		errs = append(errs, errors.New(
			"synchronous_commit is off, so PostgreSQL acknowledges a commit before it is on "+
				"disk and a crash can lose scheduling decisions billet has already acted on; "+
				"set it to on (or local) for this database"))
	}

	var readOnly string
	//billet:ignore rawsql // SHOW reads a server setting; sqlc has no catalogue entry for one
	if err := w.QueryRowContext(ctx, `SHOW default_transaction_read_only`).Scan(&readOnly); err != nil {
		return fmt.Errorf("read default_transaction_read_only: %w", err)
	}

	if strings.EqualFold(readOnly, "on") {
		errs = append(errs, errors.New(
			"default_transaction_read_only is on for the writer, so every scheduling write "+
				"would be refused; check the role and database settings"))
	}

	return errors.Join(errs...)
}

// integrityCheck has no PostgreSQL equivalent billet should run, and saying so
// is more honest than inventing one.
//
// SQLite's quick_check exists because a SQLite database is a file this process
// is solely responsible for, and a torn write leaves it corrupt with nothing to
// notice. PostgreSQL owns its own crash recovery through the WAL and answers
// that question before it accepts a connection at all. The nearest equivalent —
// amcheck — is an extension, and a control plane that refuses to start unless
// an operator has installed one is a control plane that refuses correct
// deployments.
//
// It is not silently empty: PingContext has already proved the connection, and
// verifyDurability has already asked the question that matters here.
func (*postgresBackend) integrityCheck(context.Context, *sql.DB) error { return nil }

func (*postgresBackend) bootstrapSchema() string { return bootstrapSchemaMigrationsPostgres }

// prepare reads the ledger's lock key off the connection, once.
//
// AT OPEN TIME RATHER THAN PER TRANSACTION, because it cannot change while this
// handle is open: it is derived from the database and schema this connection is
// attached to, and a connection that moved between schemas would be a different
// ledger with the same handle.
func (b *postgresBackend) prepare(ctx context.Context, w *sql.DB) error {
	key, err := readLockKey(ctx, w)
	if err != nil {
		return err
	}

	b.lockKey = key

	return nil
}

// checkBookkeepingSchema names the columns schema_migrations must have.
func (*postgresBackend) checkBookkeepingSchema(ctx context.Context, q Querier) error {
	//billet:ignore rawsql // information_schema is not in sqlc's catalogue, and this asks about the bookkeeping table sqlc is told exists
	rows, err := q.QueryContext(ctx,
		`SELECT column_name FROM information_schema.columns `+
			`WHERE table_schema = current_schema() AND table_name = 'schema_migrations'`)
	if err != nil {
		return fmt.Errorf("inspect schema_migrations: %w", err)
	}

	defer rows.Close()

	found := make(map[string]bool)

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan schema_migrations columns: %w", err)
		}

		found[name] = true
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect schema_migrations: %w", err)
	}

	return requireBookkeepingColumns(found)
}

// bookkeepingTableExists answers whether anything has created the schema.
func (*postgresBackend) bookkeepingTableExists(ctx context.Context, q Querier) (bool, error) {
	var exists bool

	//billet:ignore rawsql // to_regclass reads the catalogue, which is not in sqlc's
	err := q.QueryRowContext(ctx,
		`SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("inspect the ledger's schema: %w", err)
	}

	return exists, nil
}

// userTables lists the ledger's own tables.
//
// SCOPED TO current_schema(), because a deployment may share a database with
// something else entirely — that is one of the reasons to run PostgreSQL at all
// — and PeekLedger's caller reads "no rows anywhere" as permission to replace a
// ledger. Counting a neighbour's rows would be wrong in the safe direction;
// MISSING billet's own would not.
func (*postgresBackend) userTables(ctx context.Context, q Querier) ([]string, error) {
	//billet:ignore rawsql // information_schema is not in sqlc's catalogue (it lists the tables sqlc was told about)
	rows, err := q.QueryContext(ctx,
		`SELECT table_name FROM information_schema.tables `+
			`WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' `+
			`ORDER BY table_name`)
	if err != nil {
		return nil, fmt.Errorf("state: list the ledger's tables: %w", err)
	}

	defer rows.Close()

	var out []string

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("state: scan the ledger's tables: %w", err)
		}

		out = append(out, name)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list the ledger's tables: %w", err)
	}

	return out, nil
}

func (*postgresBackend) countRows(ctx context.Context, q Querier, table string) (int64, error) {
	return countTableRows(ctx, q, table)
}

// PostgreSQL SQLSTATEs that mean "somebody else is writing right now".
//
// EACH ONE IS A CLASS THAT RESOLVES BY WAITING, and nothing else belongs here.
// A connection failure, a syntax error and an ambiguous server error are all
// outcomes where retrying forever is the wrong answer — the control plane never
// gives up, so an error folded in here that is not contention is a control plane
// that hangs instead of reporting a fault.
const (
	// lock_not_available: what lock_timeout produces when the writer's advisory
	// lock is held by another transaction.
	sqlStateLockNotAvailable = "55P03"
	// serialization_failure and deadlock_detected. Unreachable while every writer
	// takes the same advisory lock first, and listed anyway because an engine
	// that produced one and had it treated as a fault would stop the listener,
	// which destroys every job on the host.
	sqlStateSerializationFailure = "40001"
	sqlStateDeadlockDetected     = "40P01"
)

// queryCanceled is what a cancelled statement arrives as, and the counterpart of
// SQLite's SQLITE_INTERRUPT.
const sqlStateQueryCanceled = "57014"

func (*postgresBackend) isContention(err error) bool {
	return isPGState(err, sqlStateLockNotAvailable, sqlStateSerializationFailure,
		sqlStateDeadlockDetected)
}

func (*postgresBackend) isCancellation(err error) bool {
	return isPGState(err, sqlStateQueryCanceled)
}

// isPGState matches on the server's own SQLSTATE, never on the message.
//
// THE SAME RULE THE SQLITE SIDE FOLLOWS, and for a sharper reason here: the
// message is localisable, so a server running under a different lc_messages
// would silently turn a retry into a fatal error.
func isPGState(err error, states ...string) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return false
	}

	for _, s := range states {
		if pgErr.Code == s {
			return true
		}
	}

	return false
}

// beginWrite takes a transaction-scoped advisory lock, and it is what makes this
// backend's transactions mean what SQLite's mean.
//
// EVERY ALLOCATION DECISION IS READ-CURRENT, DECIDE, RECORD. SQLite guarantees
// that by taking its write lock at BEGIN IMMEDIATE, so nothing can commit
// between the read and the write. PostgreSQL has no such thing.
//
// THE OBVIOUS MAPPING IS SERIALIZABLE PLUS A RETRY ON 40001, AND IT IS WRONG
// HERE. Retrying a serialization failure means re-executing the caller's
// closure, and DB.Tx's closures are not all pure — they log, they build values
// the caller keeps, and some of them are handed a transaction by code that has
// already decided what it is going to do. An API whose contract is "your
// function may run twice" is a different API from the one every caller in this
// repository was written against, and the failure of getting it wrong is a
// double-charged lease.
//
// So one writer at a time, by construction. Throughput is serialized, which is
// exactly what SQLite already does, so nothing regresses — and a caller that
// waits does so under lock_timeout, is classified as contention, and is retried
// by the same loop with the same asymmetric patience as a busy BEGIN.
//
// TRANSACTION-SCOPED RATHER THAN SESSION-SCOPED, so the lock is released by
// COMMIT, by ROLLBACK, and by the connection dying. A session lock leaked by a
// crashed process would wedge every writer in the deployment until somebody
// noticed.
//
// KEYED ON THE LEDGER, which is read from the server rather than derived from
// anything local — see readLockKey. The key is in a different namespace from the
// controller claim's, which is a SESSION lock held for the process's lifetime;
// the same key would make a controller block its own writes forever.
func (b *postgresBackend) beginWrite(ctx context.Context, tx *sql.Tx) error {
	//billet:ignore rawsql // an advisory lock is a server operation, not a query over billet's tables
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock($1, $2)`, advisoryClassWriter, b.lockKey); err != nil {
		return err
	}

	return nil
}

// snapshotInto refuses, and the refusal is the design rather than a gap.
//
// SQLite's VACUUM INTO produces a consistent copy of the whole ledger as one
// file, which is what makes `billet local backup` able to pair the ledger with
// the deployment identity, the CA and the App key in a single archive. There is
// no equivalent billet should own here: a consistent copy of a PostgreSQL
// database is pg_dump or the provider's snapshot, both of which are the
// operator's to run and to restore, and a half-measure that copied rows through
// this connection would produce an archive that LOOKS like a backup and is not.
//
// AND THE OTHER HALF OF THAT SENTENCE NOW EXISTS. `billet local backup` writes an
// IDENTITY-ONLY archive here — the deployment identity, the node-wire CA and its
// rotation state, and the App private key — with archive schema 2 recording the
// ledger as external and naming the engine and the DSN environment variable. It
// used to fail outright, so the half billet does own went uncaptured too, which
// for a control plane built by control-plane-postgres is the only recovery path
// there is.
//
// A RESTORE THEN REFUSES UNTIL THE HALVES ARE PAIRED: it will not install such an
// archive onto a host whose config names a local ledger, and it requires
// --external-ledger-attached for the part billet cannot check — whether the
// database on the other end of that DSN is back.
func (*postgresBackend) snapshotInto(_ context.Context, _ *DB, path string) error {
	return fmt.Errorf(
		"state: billet does not snapshot a PostgreSQL ledger into %s. A consistent copy is "+
			"your database's own backup — pg_dump, or the managed snapshot your provider "+
			"takes — and billet's archive records the ledger as external and pairs it with "+
			"the deployment identity rather than pretending to have copied it", path)
}

// The two advisory-lock namespaces this backend uses.
//
// A pg_advisory lock's first argument is a caller-chosen classid, and these two
// must differ: the writer's is taken per transaction by every write, and the
// controller claim's is held for the process's lifetime on its own connection.
// Sharing a key would make a controller's own claim block its every write.
const (
	advisoryClassWriter     int32 = 0x62_69_6C_01 // "bil" + 1
	advisoryClassController int32 = 0x62_69_6C_02 // "bil" + 2
)

// readLockKey asks the SERVER which ledger this is.
//
// THE KEY MUST IDENTIFY THE LEDGER, NOT THE PROCESS, and getting that wrong
// costs the lock all of its value. A key derived from anything local — the
// identity directory, the deployment id file, the config path — is a key two
// correct configurations can disagree about while pointing at the same rows:
// two controllers, two different advisory locks, and both inside a write
// transaction against one ledger at once. Reading it from the connection means
// every process that can see these rows computes the same number, and a process
// that cannot see them never computes it at all.
//
// current_database() AND current_schema(), because a deployment may share a
// server with anything and billet's own catalogue questions are already scoped
// to the schema. Two deployments therefore need two schemas, which is the same
// boundary the rest of this backend already draws.
//
// A pg_catalog OID was the obvious alternative and is worse: it is stable only
// until somebody restores from a dump, and a lock key that changes under a
// restore is a lock two processes stop sharing at the moment a deployment is
// most fragile. The digest of the two names is stable for as long as the names
// are.
func readLockKey(ctx context.Context, w *sql.DB) (int64, error) {
	var key int64

	// concat() RATHER THAN ||, and that is not taste: the raw-SQL allowlist is a
	// Markdown table whose cells cannot hold a pipe, and a row it cannot parse is
	// one that drops silently out of the comparison. Measured identical against a
	// real server.
	//billet:ignore rawsql // asks the server which ledger this connection is on, not a query over billet's tables
	err := w.QueryRowContext(ctx,
		`SELECT concat('x', substr(md5(concat(current_database(), '.', current_schema())), 1, 8))::bit(32)::int`,
	).Scan(&key)
	if err != nil {
		return 0, fmt.Errorf("state: identify this PostgreSQL ledger: %w", err)
	}

	return key, nil
}

// claimController takes the deployment's controller exclusion.
//
// A SESSION ADVISORY LOCK ON ITS OWN CONNECTION. The server releases it when
// that connection goes, so a crashed controller frees the deployment without
// anybody having to decide it is dead — no lease, no renewal, no timeout, and no
// stale row that could refuse a correct restart.
//
// pg_TRY_advisory_lock, not the blocking form: a second controller has to be
// TOLD it is a second controller, not left waiting for the first one to stop.
//
// WHAT THIS GUARANTEES IS THAT A SECOND CONTROLLER CANNOT START, AND THAT IS
// NARROWER THAN "EXACTLY ONE CONTROLLER". Stated here rather than left to be
// discovered, because the two are easy to read as the same thing.
//
// The lock lives in a SESSION billet does not control the lifetime of. A server
// restart or failover, an idle_session_timeout, a connection-pooling proxy, or a
// network partition ends that session and releases the lock — and this process
// finds out about none of it, because nothing uses the connection again. A
// replacement can then claim while the first is still scheduling.
//
// DETECTION WOULD NARROW IT AND CANNOT CLOSE IT. Watching the session and
// stopping the controller when it dies still leaves the window between the
// partition and the observation, and a watchdog that stops a healthy control
// plane over a transient blip is its own failure. What closes it is the EPOCH
// this claim records: DB.Tx re-reads it inside every write transaction, so the
// first controller's next write after the replacement's claim is REFUSED rather
// than detected. See checkLeadership for why that is exact on both engines.
//
// So the division of labour is: this is the check that catches the accident — a
// second controller started by a configuration mistake or an operator — and the
// epoch beside it is the fence that catches the one nobody could have caught,
// where the exclusion was lost without either process noticing.
func (b *postgresBackend) claimController(ctx context.Context, db *DB) error {
	// ALREADY HELD IS ALREADY YES, and it has to be: openDir takes this before it
	// migrates and DB.ClaimController takes it again before it records the epoch.
	// A second acquisition would come from a second CONNECTION, which is a second
	// session asking the server for a lock this process already holds — refused,
	// correctly, and the control plane would report itself as its own second
	// controller.
	if b.claim != nil {
		return nil
	}

	pools, err := b.dataSources()
	if err != nil {
		return err
	}

	conn, err := sql.Open(b.driverName(), pools.writer)
	if err != nil {
		return fmt.Errorf("state: open a connection for the controller claim: %w", err)
	}

	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	conn.SetConnMaxLifetime(0)
	conn.SetConnMaxIdleTime(0)

	var taken bool

	//billet:ignore rawsql // an advisory lock is a server operation, not a query over billet's tables
	if err := conn.QueryRowContext(ctx,
		`SELECT pg_try_advisory_lock($1, $2)`, advisoryClassController, b.lockKey,
	).Scan(&taken); err != nil {
		return errors.Join(
			fmt.Errorf("state: take the controller claim: %w", err), conn.Close())
	}

	if !taken {
		return errors.Join(
			fmt.Errorf("%w%s", ErrControllerHeld, describeHolder(ctx, db)), conn.Close())
	}

	b.claim = conn

	return nil
}

// releaseController ends the session holding the claim.
//
// CLOSING THE CONNECTION IS THE RELEASE. pg_advisory_unlock is the polite form
// and is not the guarantee: the lock belongs to the session, so the only thing
// that has to be true is that the session ends.
func (b *postgresBackend) releaseController() error {
	if b.claim == nil {
		return nil
	}

	conn := b.claim
	b.claim = nil

	return conn.Close()
}

// OpenPostgres opens a deployment whose ledger lives in PostgreSQL.
//
// THE STATE DIRECTORY IS STILL A DIRECTORY, and that is not a leftover: the
// deployment identity, the node-wire CA, the process lock and the maintenance
// fence are local files under every backend. Only the SQL rows move. What the
// directory stops holding is billet.db.
//
// THE PROCESS LOCK STILL APPLIES AND IS STILL NOT ENOUGH. It excludes a second
// control plane on THIS machine, which is the whole of the problem on SQLite and
// half of it here — a second controller on another host would take its own
// directory's lock happily. What closes the other half is the pair the ledger
// carries: the session advisory lock in claimController, which stops a second
// controller starting, and the epoch in ControllerClaim, which stops the first
// one writing once a second has legitimately taken over.
func OpenPostgres(ctx context.Context, stateDir, dsn string, opts ...OpenOption) (*DB, error) {
	return openDir(ctx, stateDir, newPostgresBackend(dsn), openMode{}.with(opts))
}

// OpenPostgresAdmin is the operator-command form, with the same asymmetry
// OpenAdmin describes: it proceeds without the directory lock when a control
// plane holds it, and then VERIFIES the schema rather than migrating it.
func OpenPostgresAdmin(ctx context.Context, stateDir, dsn string, opts ...OpenOption) (*DB, error) {
	return openDir(ctx, stateDir, newPostgresBackend(dsn), openMode{admin: true}.with(opts))
}

// OpenPostgresStandby opens the ledger for a control plane that is WAITING to
// become this deployment's controller.
//
// IT TAKES THE DIRECTORY LOCK AND NOTHING ELSE. The lock still means what it
// always did — two billets must not manage one state directory on one host — and
// a standby is a control plane in waiting, so a second one here is the same
// mistake as a second controller. What it does NOT take is the controller
// exclusion, because taking it is precisely what promotion IS.
//
// A HANDLE THAT CANNOT WRITE. Every write transaction is refused with ErrStandby
// until ClaimController succeeds; reads are allowed, so the process can report on
// itself and on the claim it is waiting for.
//
// POSTGRESQL ONLY, AND THERE IS NO SQLITE FORM. A SQLite ledger is a file on
// local storage that a second machine cannot open at all, so a standby there
// would be a second process on one host waiting for a lock its own service
// manager already restarts it to take. Config refuses the pairing, and the
// absence of an entry point here is the same refusal one layer down.
func OpenPostgresStandby(ctx context.Context, stateDir, dsn string, opts ...OpenOption) (*DB, error) {
	return openDir(ctx, stateDir, newPostgresBackend(dsn), openMode{standby: true}.with(opts))
}
