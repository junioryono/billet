package state

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// UNREACHABILITY IS A PROPERTY OF THE ERROR, NOT OF THE STEP THAT PRODUCED IT.
// A caller that waits out an outage rather than refusing needs to know the
// database could not be reached; a ping that failed because the file is not a
// database is the database answering, and reading that as unreachability would
// have such a caller wait out a corrupt ledger for ever.
func TestUnreachableIsAskedOfTheErrorAndNotOfTheStep(t *testing.T) {
	// A LEDGER THAT IS NOT ONE: the ping fails, and it fails with positive
	// evidence about the file.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "billet.db"), []byte("this is not a database"), 0o600); err != nil {
		t.Fatalf("write the ledger: %v", err)
	}

	_, err := OpenAdmin(t.Context(), dir)
	if err == nil {
		t.Fatal("a file that is not a database opened")
	}

	if Unreachable(err) {
		t.Fatalf("a corrupt ledger reads as unreachable: %v", err)
	}

	// A POSTGRESQL DSN NOTHING LISTENS ON: the connection cannot be
	// established, which is what the word means.
	_, err = OpenPostgresCompletion(t.Context(), t.TempDir(),
		"postgres://billet:billet@127.0.0.1:1/billet?sslmode=disable&connect_timeout=2")
	if err == nil {
		t.Fatal("a port nothing listens on opened")
	}

	if !Unreachable(err) {
		t.Fatalf("a refused connection does not read as unreachable: %v", err)
	}

	if Unreachable(nil) {
		t.Fatal("no error reads as unreachable")
	}

	// AND A REFUSAL THE LEDGER ITSELF COMPOSED IS NOT ONE.
	if Unreachable(ErrSchemaAhead) || Unreachable(ErrForeignLedger) {
		t.Fatal("a refusal the ledger answered with reads as unreachable")
	}
}

// EVERY ROW OF THE CLASSIFIER IS A MEASUREMENT, and this is where it is taken:
// pgx's shapes are not a documented contract, and the ones that matter most are
// the ones reading alone gets wrong — a wrong password and a database that does
// not exist arrive INSIDE a ConnectError, a context deadline satisfies
// net.Error, and a certificate the client refuses looks from the outside like a
// server that is not there. EACH CASE ASSERTS THE SHAPE IT PRODUCED as well as
// the verdict, so a pgx change that moves an error from one shape to another
// fails here rather than silently turning a credential an operator must fix
// into a retirement that waits for ever.
type errorShape struct {
	connect  bool
	network  bool
	deadline bool
	// sqlstate is the server's own code, empty when it said nothing.
	sqlstate string
}

func shapeOf(err error) errorShape {
	var shape errorShape

	//nolint:errcheck // the discarded value is the typed error itself; the bool is the answer.
	if _, ok := errors.AsType[*pgconn.ConnectError](err); ok {
		shape.connect = true
	}

	//nolint:errcheck // as above.
	if _, ok := errors.AsType[net.Error](err); ok {
		shape.network = true
	}

	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		shape.sqlstate = pgErr.Code
	}

	shape.deadline = errors.Is(err, context.DeadlineExceeded)

	return shape
}

func TestUnreachableIsMeasuredAgainstPgxsOwnShapes(t *testing.T) {
	base := requirePostgresSchema(t, "unreachable_shapes")

	cases := []struct {
		name  string
		want  bool
		shape errorShape
		// certificate says the case must have produced a TLS verification
		// failure, which no shape of pgx's own records.
		certificate bool
		run         func(t *testing.T) error
	}{{
		name: "a port nothing listens on", want: true,
		shape: errorShape{connect: true, network: true},
		run: func(t *testing.T) error {
			t.Helper()

			_, err := OpenPostgresCompletion(t.Context(), t.TempDir(),
				"postgres://billet:billet@127.0.0.1:1/billet?sslmode=disable&connect_timeout=2")

			return err
		},
	}, {
		// INSIDE a ConnectError, which is why the server's own error is asked
		// about first: 28P01 is the server refusing this client.
		name: "a password the server rejects", want: false,
		shape: errorShape{connect: true, sqlstate: "28P01"},
		run: func(t *testing.T) error {
			t.Helper()

			_, err := OpenPostgresCompletion(t.Context(), t.TempDir(), replaceDSNPassword(t, base, "wrong"))

			return err
		},
	}, {
		name: "a database that does not exist", want: false,
		shape: errorShape{connect: true, sqlstate: "3D000"},
		run: func(t *testing.T) error {
			t.Helper()

			_, err := OpenPostgresCompletion(t.Context(), t.TempDir(), replaceDSNDatabase(t, base, "nosuchdb"))

			return err
		},
	}, {
		name: "a relation that does not exist", want: false,
		shape: errorShape{sqlstate: "42P01"},
		run: func(t *testing.T) error {
			t.Helper()

			conn := openProbeConn(t, base)

			//billet:ignore rawsql // the measurement needs a statement the server refuses
			_, err := conn.ExecContext(t.Context(), `SELECT * FROM no_such_table`)

			return err
		},
	}, {
		// A SLOW QUERY IS A DATABASE ANSWERING, and the deadline is the
		// caller's own bound: pgx wraps it in errTimeout, which satisfies
		// net.Error. The connection is established FIRST, so what the deadline
		// cuts off is the statement and not the session's setup.
		name: "a statement the caller's deadline cut off", want: false,
		shape: errorShape{network: true, deadline: true},
		run: func(t *testing.T) error {
			t.Helper()

			conn := openProbeConn(t, base)

			//billet:ignore rawsql // the measurement needs an established session before its deadline
			if err := conn.PingContext(t.Context()); err != nil {
				t.Fatalf("establish the session: %v", err)
			}

			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()

			//billet:ignore rawsql // the statement the deadline cuts off
			_, err := conn.ExecContext(ctx, `SELECT pg_sleep(3)`)

			return err
		},
	}, {
		// SQLSTATE 57P01: the server saying it is going away, which is an
		// availability answer and not a refusal to act on.
		name: "a backend the server terminated", want: true,
		shape: errorShape{sqlstate: "57P01"},
		run: func(t *testing.T) error {
			t.Helper()

			victim, other := openProbeConn(t, base), openProbeConn(t, base)
			victim.SetMaxOpenConns(1)

			var pid int

			//billet:ignore rawsql // the measurement needs the session's own pid
			if err := victim.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatalf("read the session's pid: %v", err)
			}

			terminated := make(chan error, 1)

			go func() {
				time.Sleep(300 * time.Millisecond)

				//billet:ignore rawsql // the measurement terminates the session under its query
				_, err := other.ExecContext(context.WithoutCancel(t.Context()), `SELECT pg_terminate_backend($1)`, pid)
				terminated <- err
			}()

			//billet:ignore rawsql // the statement the termination lands under
			_, err := victim.ExecContext(t.Context(), `SELECT pg_sleep(3)`)

			// THE TERMINATION IS PART OF THE MEASUREMENT: a case whose setup
			// failed would otherwise be measuring the sleep's own end.
			if failed := <-terminated; failed != nil {
				t.Fatalf("terminate the session: %v", failed)
			}

			return err
		},
	}, {
		// SQLSTATE 57014, IN THE SAME CLASS AS A SHUTDOWN AND NOT THE SAME
		// THING: the server cancelled this statement because the deployment's
		// own statement_timeout said to, which is a limit to fix and not an
		// outage to wait out. It is why the availability states are a list and
		// not a class prefix.
		name: "a statement the server's own timeout cancelled", want: false,
		shape: errorShape{sqlstate: "57014"},
		run: func(t *testing.T) error {
			t.Helper()

			conn := openProbeConn(t, base)
			conn.SetMaxOpenConns(1)

			//billet:ignore rawsql // the measurement sets the server's own bound
			if _, err := conn.ExecContext(t.Context(), `SET statement_timeout = '100ms'`); err != nil {
				t.Fatalf("set the server's statement timeout: %v", err)
			}

			//billet:ignore rawsql // the statement that bound cancels
			_, err := conn.ExecContext(t.Context(), `SELECT pg_sleep(3)`)

			return err
		},
	}, {
		// A CERTIFICATE THE CLIENT WILL NOT ACCEPT: the session never reaches
		// the server's own vocabulary, so there is no SQLSTATE, and the
		// ConnectError around it must not be read as an outage.
		name: "a certificate the client refuses", want: false,
		shape: errorShape{connect: true}, certificate: true,
		run: func(t *testing.T) error {
			t.Helper()

			_, err := OpenPostgresCompletion(t.Context(), t.TempDir(), selfSignedTLSDSN(t, base))

			return err
		},
	}, {
		// THE CONNECTION GOES AWAY WITHOUT THE SERVER SAYING SO, which is the
		// shape a host that lost its network produces: database/sql discards
		// the dead connection and opens another, and the proxy is gone by
		// then, so what comes back is a connection that could not be made.
		name: "a connection cut under a statement", want: true,
		shape: errorShape{connect: true, network: true},
		run: func(t *testing.T) error {
			t.Helper()

			proxied, cut := proxiedDSN(t, base)

			conn := openProbeConn(t, proxied)
			conn.SetMaxOpenConns(1)

			//billet:ignore rawsql // the session the cut lands on
			if err := conn.PingContext(t.Context()); err != nil {
				t.Fatalf("establish the session through the proxy: %v", err)
			}

			go func() {
				time.Sleep(300 * time.Millisecond)
				cut()
			}()

			//billet:ignore rawsql // the statement the cut lands under
			_, err := conn.ExecContext(t.Context(), `SELECT pg_sleep(3)`)

			return err
		},
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.run(t)
			if err == nil {
				t.Fatal("the case produced no error, so it measured nothing")
			}

			// THE SHAPE FIRST: a case that produced another error entirely
			// can classify the same way and prove nothing about the rule the
			// classifier is written from.
			if got := shapeOf(err); got != c.shape {
				t.Fatalf("the shape is %+v, want %+v, for: %v", got, c.shape, err)
			}

			if c.certificate && !certificateRejected(err) {
				t.Fatalf("the case did not produce a certificate rejection: %v", err)
			}

			if got := Unreachable(err); got != c.want {
				t.Fatalf("Unreachable is %v, want %v, for: %v", got, c.want, err)
			}
		})
	}
}

// openProbeConn is a connection through the same stack billet uses, closed
// with the test.
func openProbeConn(t *testing.T, dsn string) *sql.DB {
	t.Helper()

	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open a connection: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// replaceDSNPassword and replaceDSNDatabase change one field of the DSN
// through the parser, so a case cannot silently measure nothing because a
// literal it replaced was spelled another way.
func replaceDSNPassword(t *testing.T, dsn, password string) string {
	t.Helper()

	u := parseDSN(t, dsn)
	u.User = url.UserPassword(u.User.Username(), password)

	return u.String()
}

func replaceDSNDatabase(t *testing.T, dsn, database string) string {
	t.Helper()

	u := parseDSN(t, dsn)
	u.Path = "/" + database

	return u.String()
}

func parseDSN(t *testing.T, dsn string) *url.URL {
	t.Helper()

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse the DSN: %v", err)
	}

	if u.User == nil || u.User.Username() == "" || u.Path == "" || u.Path == "/" {
		t.Fatalf("the DSN carries no user or database to replace: %s", dsn)
	}

	return u
}

// selfSignedTLSDSN answers a DSN pointing at a listener that speaks
// PostgreSQL's TLS negotiation and then presents a certificate no root this
// client trusts has signed. The session never reaches the server's own
// vocabulary, which is the point.
func selfSignedTLSDSN(t *testing.T, base string) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create a certificate: %v", err)
	}

	var listener net.ListenConfig

	ln, err := listener.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			go func() {
				defer func() { _ = conn.Close() }()

				// The SSLRequest packet is eight bytes: its own length, then
				// the code 80877103. A negotiation that ever stops starting
				// this way must fail the case rather than quietly become some
				// other kind of failure.
				request := make([]byte, 8)
				if _, err := io.ReadFull(conn, request); err != nil {
					return
				}

				if binary.BigEndian.Uint32(request[0:4]) != 8 ||
					binary.BigEndian.Uint32(request[4:8]) != 80877103 {
					t.Errorf("the client did not open with an SSLRequest: %v", request)

					return
				}

				if _, err := conn.Write([]byte("S")); err != nil {
					return
				}

				server := tls.Server(conn, &tls.Config{
					Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
					MinVersion:   tls.VersionTLS12,
				})

				// THE HANDSHAKE IS EXPECTED TO FAIL: the client is the one
				// that refuses, and what this listener exists to produce is
				// that refusal on the client's side.
				_ = server.HandshakeContext(context.WithoutCancel(t.Context())) //nolint:errcheck // the client's refusal is the measurement
			}()
		}
	}()

	u := parseDSN(t, base)
	u.Host = ln.Addr().String()

	query := u.Query()
	query.Set("sslmode", "verify-full")
	u.RawQuery = query.Encode()

	return u.String()
}

// proxiedDSN forwards to the real server until cut is called, which closes
// every connection it holds AND the listener, so the retry database/sql makes
// has nowhere to go.
func proxiedDSN(t *testing.T, base string) (string, func()) {
	t.Helper()

	u := parseDSN(t, base)

	var listener net.ListenConfig

	ln, err := listener.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var (
		mu    sync.Mutex
		held  []net.Conn
		gone  bool
		onion = u.Host
	)

	cut := func() {
		mu.Lock()
		defer mu.Unlock()

		if gone {
			return
		}

		gone = true
		_ = ln.Close()

		for _, c := range held {
			_ = c.Close()
		}
	}

	t.Cleanup(cut)

	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}

			var dialer net.Dialer

			out, err := dialer.DialContext(context.WithoutCancel(t.Context()), "tcp", onion)
			if err != nil {
				_ = in.Close()

				return
			}

			mu.Lock()
			held = append(held, in, out)
			mu.Unlock()

			//nolint:errcheck // a proxy that is about to be cut: what each copy ends with is the cut itself
			go func() { _, _ = io.Copy(out, in) }()
			//nolint:errcheck // as above
			go func() { _, _ = io.Copy(in, out) }()
		}
	}()

	u.Host = ln.Addr().String()

	return u.String(), cut
}

// A COMPLETION WRITES ONE ROW AND CLAIMS NOTHING. Its caller is a host whose
// controller has been retired: an admin open would take the deployment's
// controller exclusion and migrate the shared schema whenever the survivor
// happened to be down, which is the one moment a retired host must not become
// this deployment's control plane.
func TestOpenPostgresCompletionClaimsNothingAndMigratesNothing(t *testing.T) {
	dsn := requirePostgres(t)

	// A ledger a control plane has already migrated and left.
	plane, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	if _, err := plane.ClaimController(t.Context(), "control-a", "dddddddddddddddddddddddddddddddd"); err != nil {
		t.Fatalf("ClaimController: %v", err)
	}

	if err := plane.Close(); err != nil {
		t.Fatalf("close the plane: %v", err)
	}

	// The retired host's own directory is the archive: nothing else locks it,
	// so the controller exclusion is free for the taking — and is not taken.
	var db *DB

	ops := sideEffects(t, func() {
		db, err = OpenPostgresCompletion(t.Context(), t.TempDir(), dsn)
	})
	if err != nil {
		t.Fatalf("OpenPostgresCompletion: %v", err)
	}

	defer func() { _ = db.Close() }()

	if slices.Contains(ops, "claim") || slices.Contains(ops, "migrate") || slices.Contains(ops, "watermark") {
		t.Fatalf("a completion claimed or migrated: %v", ops)
	}

	// AND IT CAN WRITE, which is the whole reason it is not an inspection.
	if _, _, err := db.CompleteRetirement(t.Context(), RetirementCompletion{
		Deployment: "dddddddddddddddddddddddddddddddd", Retiring: "control-a", Survivor: "control-b",
		TransitionID: "0123456789abcdef0123456789abcdef", ReservedAt: "2026-09-11T10:00:00Z",
		CompletedBy: "control-a", At: time.Now(),
	}); err != nil {
		t.Fatalf("complete the row through a completion handle: %v", err)
	}
}

// AND IT REFUSES A SCHEMA THAT IS NOT EXACTLY THIS BINARY'S rather than
// migrating it, which is what lets the caller hand the write to the survivor:
// a ledger the survivor has already migrated past is ErrSchemaAhead, and one
// behind this binary is ErrSchemaBehind — neither is this host's to move.
func TestOpenPostgresCompletionRefusesASchemaItWouldHaveToMove(t *testing.T) {
	dsn := requirePostgresSchema(t, "completion_behind")

	plane, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	if err := plane.Close(); err != nil {
		t.Fatalf("close the plane: %v", err)
	}

	// ONE APPLIED VERSION REMOVED FROM THE RECORD is a ledger behind this
	// binary without touching a table: what the open compares is the recorded
	// set against its own.
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open a connection: %v", err)
	}

	//billet:ignore rawsql // the fixture rewinds the bookkeeping table this open reads
	if _, err := conn.ExecContext(t.Context(),
		`DELETE FROM schema_migrations WHERE version = (SELECT max(version) FROM schema_migrations)`); err != nil {
		t.Fatalf("rewind the recorded schema: %v", err)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("close the connection: %v", err)
	}

	_, err = OpenPostgresCompletion(t.Context(), t.TempDir(), dsn)
	if !errors.Is(err, ErrSchemaBehind) {
		t.Fatalf("a completion over a ledger behind this binary: %v", err)
	}
}

// THE OPEN'S OWN BUDGET IS A DEADLINE, and a caller that must tell an outage
// from a refusal depends on it looking like one: the open gives itself
// `startupTimeout` for the ping, the backend's preparation and the schema, and
// a connection that accepts and then says nothing ends there rather than
// waiting on whatever the caller's context allows. The retirement's tail reads
// exactly this shape — a deadline neither of ITS contexts set — as the ledger
// being out of reach.
func TestAnOpenThatTimesOutOnItsOwnBudgetSaysSo(t *testing.T) {
	base := requirePostgresSchema(t, "startup_budget")

	// A listener that completes the TCP connection and answers nothing, which
	// is what an unresponsive server looks like from here.
	var listener net.ListenConfig

	ln, err := listener.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			t.Cleanup(func() { _ = conn.Close() })
		}
	}()

	saved := startupTimeout
	startupTimeout = 300 * time.Millisecond

	t.Cleanup(func() { startupTimeout = saved })

	u := parseDSN(t, base)
	u.Host = ln.Addr().String()

	_, err = OpenPostgresCompletion(t.Context(), t.TempDir(), u.String())
	if err == nil {
		t.Fatal("an open against a listener that says nothing succeeded")
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the open's own budget did not end it as a deadline: %v", err)
	}

	// AND NOT AS UNREACHABILITY, which is the whole reason the tail decides
	// that from its contexts rather than from the error: the attempt was cut
	// short, so nothing about the database was established, even though pgx
	// reports it as a ConnectError.
	if Unreachable(err) {
		t.Fatalf("a deadline reads as unreachable: %v", err)
	}

	// AND THE CANCELLATION IS THE WHOLE OF IT, which is what lets the caller
	// read a deadline neither of its own contexts set as the ledger being out
	// of reach rather than as a refusal.
	if !OnlyCancellation(err) {
		t.Fatalf("the open's own budget left something else in the error: %v", err)
	}
}

// THE ALLOWLIST'S MEMBERSHIP, code by code. The measurement above pins what
// pgx produces for the situations billet actually meets; this pins which of
// the server's own answers mean "not now" rather than "not like this", which
// is a judgement about PostgreSQL's vocabulary and not about pgx's shapes.
func TestUnreachableSQLStatesAreAnAllowlist(t *testing.T) {
	for code, want := range map[string]bool{
		// Availability: the connection, or the server's readiness.
		"08001": true, // the client could not establish the connection
		"08003": true, // the connection does not exist
		"08006": true, // the connection failed
		"08007": true, // the transaction's resolution is unknown
		"53300": true, // too many connections
		"57P01": true, // the administrator ended this backend
		"57P02": true, // a crash ended it
		"57P03": true, // the server cannot accept connections yet

		// The same two CLASSES, and none of these is an outage.
		"08004": false, // the server rejected this client (pg_hba)
		"08P01": false, // a protocol violation
		"57014": false, // the server cancelled the statement (statement_timeout)
		"57P04": false, // the database was dropped

		// And the ordinary refusals.
		"28P01": false, // the password was rejected
		"3D000": false, // the database does not exist
		"42P01": false, // the relation does not exist
		"53400": false, // a configuration limit was exceeded
		"":      false,
	} {
		if got := unreachableSQLState(code); got != want {
			t.Errorf("SQLSTATE %q reads as unreachable=%v, want %v", code, got, want)
		}
	}
}

// AN ERROR TREE IS NOT ORDERED EVIDENCE. `errors.Join` puts several causes
// beside each other with no precedence of its own, and billet's own opens join
// a startup failure with a cleanup failure; a classifier that answered from
// the FIRST match it found would say one thing for a tree and the opposite for
// its mirror image. A refusal dominates wherever it sits.
func TestTheReachVerdictIsTakenFromTheWholeTree(t *testing.T) {
	var (
		available = &pgconn.PgError{Code: "57P03", Message: "the database system is starting up"}
		rejected  = &pgconn.PgError{Code: "28P01", Message: "password authentication failed"}
		cancelled = &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}
		transport = &net.OpError{Op: "dial", Err: errors.New("connection refused")}
		cleanup   = errors.New("close the pools: still in use")
	)

	cases := map[string]struct {
		err         error
		unreachable bool
		// cancellation is what OnlyCancellation must answer: a caller that
		// waits out an expiry asks it, and a tree holding anything else is not
		// one.
		cancellation bool
	}{
		"an availability state alone": {err: available, unreachable: true},
		"a refusal alone":             {err: rejected, unreachable: false},

		// BOTH ORDERS, because the answer must not depend on which cause a
		// walk reaches first.
		"a refusal joined after an availability state": {
			err: errors.Join(available, rejected), unreachable: false,
		},
		"a refusal joined before an availability state": {
			err: errors.Join(rejected, available), unreachable: false,
		},

		// A cancelled statement is in class 57 and is not an outage; a
		// transport failure beside it does not make it one.
		"a cancelled statement beside a transport failure": {
			err: errors.Join(cancelled, transport), unreachable: false,
		},

		// AND A CLEANUP FAILURE IS ITS OWN BRANCH, whichever sentinel stands
		// beside it. `errors.Join` puts unrelated failures next to each other
		// and billet's own opens join a startup failure with their close, so a
		// tree holding an outage AND this host's pools failing to close is not
		// an outage a caller may wait out: it carries something that host must
		// fix, and the fault is not a survivor's to finish. Asking the whole
		// tree at once answered otherwise, because a sibling that established
		// nothing set no flag — and the identical cleanup beside a schema
		// refusal refused, which is the asymmetry this closes.
		"a transport failure joined with a cleanup failure": {
			err: errors.Join(transport, cleanup), unreachable: false,
		},
		"a transport sentinel joined with a cleanup failure": {
			err: errors.Join(driver.ErrBadConn, cleanup), unreachable: false,
		},

		// The cleanup's own branch is what refuses, so the same cleanup under
		// a wrapper answers the same way, and the transport branch alone is
		// still an outage.
		"a transport sentinel joined with a wrapped cleanup failure": {
			err:         errors.Join(driver.ErrBadConn, fmt.Errorf("close the pools: %w", cleanup)),
			unreachable: false,
		},
		"a transport sentinel under a wrapper": {
			err: fmt.Errorf("open the ledger: %w", driver.ErrBadConn), unreachable: true,
		},

		// A JOIN OF TWO OUTAGES IS STILL AN OUTAGE: the rule is that every
		// branch must establish one, not that there may be only one.
		//
		// AND EACH BRANCH IS JUDGED WITH THE EVIDENCE ABOVE IT. `net.OpError`
		// unwraps to a bare cause and so does pgx's `ConnectError`, so a rule
		// that split the tree at its join and then looked only at what it
		// found underneath would discard the very shapes the measurement reads
		// and answer false for both of these.
		"two transport failures joined": {
			err: errors.Join(transport, driver.ErrBadConn), unreachable: true,
		},
		"a join under a transport failure": {
			err: &net.OpError{Op: "dial", Err: errors.Join(errors.New("one address"),
				errors.New("another address"))},
			unreachable: true,
		},

		// AND A JOIN UNDER A WRAPPER IS FOUND, because the wrapper is not the
		// branch: a classifier that stopped at the first single-cause node
		// would read the whole join as one branch and answer from the flags
		// its two halves set together.
		"a join under a wrapper": {
			err:         fmt.Errorf("open the ledger: %w", errors.Join(transport, cleanup)),
			unreachable: false,
		},

		// AND A DEADLINE DOES NOT SURVIVE AN INDEPENDENT CAUSE: this is the
		// tree the retirement's tail must not read as its own bound expiring.
		"a deadline joined with a refusal": {
			err: errors.Join(context.DeadlineExceeded, rejected), unreachable: false,
		},
		"a deadline joined with a cleanup failure": {
			err: errors.Join(context.DeadlineExceeded, cleanup), unreachable: false,
		},
		"a deadline alone":            {err: context.DeadlineExceeded, cancellation: true},
		"a deadline under a wrapper":  {err: fmt.Errorf("ping: %w", context.DeadlineExceeded), cancellation: true},
		"a cancellation under a join": {err: errors.Join(context.Canceled, nil), cancellation: true},

		// AN ATTEMPT CUT SHORT ESTABLISHES NOTHING, so a transport failure
		// beside a cancellation is could-not-tell rather than an outage: pgx
		// reports a connect that ran out of time as a ConnectError, and
		// reading that as an outage would turn an operator's interruption into
		// a fact about the deployment. The real shape of it is measured by the
		// startup-budget test below.
		"a transport failure beside a cancellation": {
			err: errors.Join(transport, context.Canceled),
		},

		// A wrapper carries whatever it wraps.
		"a refusal under two wrappers": {
			err: fmt.Errorf("open: %w", fmt.Errorf("connect: %w", rejected)),
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := Unreachable(c.err); got != c.unreachable {
				t.Errorf("Unreachable is %v, want %v", got, c.unreachable)
			}

			if got := OnlyCancellation(c.err); got != c.cancellation {
				t.Errorf("OnlyCancellation is %v, want %v", got, c.cancellation)
			}
		})
	}
}

// A CYCLE IN AN ERROR TREE IS A WALK THAT DOES NOT END, and a classifier that
// hangs is worse than one that says it could not tell. Nothing in billet
// builds one; a driver or a library can.
func TestTheReachVerdictSurvivesACycle(t *testing.T) {
	loop := &loopingError{}
	loop.cause = loop

	// Both answers are the conservative one: nothing is claimed about reaching
	// a ledger, and a cancellation is not claimed to be the whole of it.
	if Unreachable(loop) {
		t.Fatal("a cyclic error reads as unreachable")
	}

	if OnlyCancellation(loop) {
		t.Fatal("a cyclic error reads as a cancellation")
	}

	// AND A CYCLE UNDER A REAL CAUSE does not make its evidence usable either.
	if Unreachable(errors.Join(&pgconn.PgError{Code: "57P01"}, loop)) {
		t.Fatal("a tree whose walk could not finish answered from the part it saw")
	}
}

type loopingError struct{ cause error }

func (*loopingError) Error() string { return "a cause that is its own cause" }

func (e *loopingError) Unwrap() error { return e.cause }
