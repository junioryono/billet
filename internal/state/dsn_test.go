package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// EACH REDACTING METHOD IS ASSERTED ON THE VALUE TYPE, because the rendering
// tables cannot see a missing one: slog falls back to MarshalJSON or to fmt, both
// of which redact, and a method moved to a pointer receiver is still found
// through a pointer. A value reached through a field is what each must cover.
var (
	_ fmt.Stringer   = DSN("")
	_ fmt.GoStringer = DSN("")
	_ fmt.Formatter  = DSN("")
	_ json.Marshaler = DSN("")
	_ slog.LogValuer = DSN("")

	_ fmt.Stringer   = postgresBackend{}
	_ fmt.GoStringer = postgresBackend{}
	_ fmt.Formatter  = postgresBackend{}
	_ json.Marshaler = postgresBackend{}
	_ slog.LogValuer = postgresBackend{}
)

const testDSNPassword = "hunter2-the-password"

// A DSN CARRIES THE LEDGER'S PASSWORD, and it reaches a log through one careless
// verb on whatever struct happens to hold it. Every rendering path is covered
// because each ignores the others: slog's JSON handler never consults fmt, %#v
// never consults String, and a bad verb falls back to the raw string unless
// Format takes it. Each of the five methods was neutered once and the path it
// covers went red.
func TestADSNIsRedactedOnEveryRenderingPath(t *testing.T) {
	t.Parallel()

	dsn := DSN("postgres://billet:" + testDSNPassword + "@db.internal/billet")

	// A struct holding it, because that is how the leak actually happens.
	holder := struct {
		Where  string
		Ledger DSN
	}{"opening", dsn}

	rendered := map[string]string{
		"%v":             fmt.Sprintf("%v", dsn), //nolint:gocritic // the verb path is the subject
		"%s":             fmt.Sprintf("%s", dsn), //nolint:gocritic // the verb path is the subject
		"%q":             fmt.Sprintf("%q", dsn),
		"%x":             fmt.Sprintf("%x", dsn),
		"%d":             fmt.Sprintf("%d", dsn),
		"%#v":            fmt.Sprintf("%#v", dsn),
		"%v on a field":  fmt.Sprintf("%v", holder),
		"%+v on a field": fmt.Sprintf("%+v", holder),
		"%#v on a field": fmt.Sprintf("%#v", holder),
		"String()":       dsn.String(),
		"GoString()":     dsn.GoString(),
	}

	encoded, err := json.Marshal(holder)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rendered["json"] = string(encoded)
	rendered["LogValue()"] = dsn.LogValue().String()

	var logged bytes.Buffer

	slog.New(slog.NewJSONHandler(&logged, nil)).Info("opening", "dsn", dsn)

	rendered["slog json"] = logged.String()

	logged.Reset()

	slog.New(slog.NewTextHandler(&logged, nil)).Info("opening", "dsn", dsn)

	rendered["slog text"] = logged.String()

	for path, out := range rendered {
		if strings.Contains(out, testDSNPassword) {
			t.Errorf("%s rendered the password: %s", path, out)
		}

		if !strings.Contains(out, redactedDSN) {
			t.Errorf("%s did not say the value was redacted, so a reader cannot tell a "+
				"redaction from an empty DSN: %s", path, out)
		}
	}
}

// THE VALUE IS STILL THERE FOR THE ONE READER THAT NEEDS IT: the driver. A
// redaction that also blanked the connection string would open nothing.
func TestADSNStillConnectsAsItself(t *testing.T) {
	t.Parallel()

	dsn := DSN("postgres://billet:" + testDSNPassword + "@db.internal/billet")

	if !strings.Contains(string(dsn), testDSNPassword) {
		t.Fatal("converting a DSN back to a string lost the password the driver needs")
	}
}

// AN UNPARSABLE DSN NEVER PUTS ITS PASSWORD IN THE ERROR. pgx's parse error
// quotes the connection string with the password masked by pattern, and the
// pattern misses: measured 2026-10-04 on pgx v5.10.0, each of these came back
// from pgx.ParseConfig with the password, or part of it, in the text. The open
// refuses before it connects, so this needs no server. The first loop keeps the
// fixtures honest: a set pgx no longer leaks on would prove nothing here, and
// its failure is the instruction to measure again.
func TestAnUnparsableDSNNeverRendersItsPassword(t *testing.T) {
	t.Parallel()

	const secret = "hunter2"

	leaking := []DSN{
		"postgres://billet:" + secret + ":x%zz@db.internal/billet",
		"postgres://billet:x@" + secret + "%zz@db.internal/billet",
		"host=db.internal password = " + secret + " port=notaport",
		"host=db.internal PASSWORD=" + secret + " port=notaport",
	}

	for _, dsn := range leaking {
		_, err := pgx.ParseConfig(string(dsn))
		if err == nil || !strings.Contains(err.Error(), secret) {
			t.Errorf("pgx no longer quotes the password for one measured shape (err %v); "+
				"measure again and update registerConn's comment and this set", err)
		}
	}

	for i, dsn := range leaking {
		_, err := OpenPostgresAdmin(t.Context(), t.TempDir(), dsn)
		if !errors.Is(err, errUnparsableDSN) {
			t.Errorf("shape %d: the open answered %v, want the unparsable-DSN refusal", i, err)

			continue
		}

		if strings.Contains(err.Error(), secret) {
			t.Errorf("shape %d: the open's error carries the password: %v", i, err)
		}
	}
}

// AN EMPTY DSN NAMES THE VARIABLE IT CAME FROM, on every open that parses one,
// rather than reaching pgx, which would read it as "connect to the defaults".
func TestAnEmptyDSNIsRefusedNamingItsVariable(t *testing.T) {
	t.Parallel()

	for name, open := range map[string]func() (*DB, error){
		"admin":   func() (*DB, error) { return OpenPostgresAdmin(t.Context(), t.TempDir(), " \t") },
		"inspect": func() (*DB, error) { return OpenPostgresInspect(t.Context(), t.TempDir(), "") },
	} {
		if _, err := open(); err == nil || !strings.Contains(err.Error(), "server.state.postgres.dsn_env") {
			t.Errorf("%s: an empty DSN answered %v, want the refusal naming dsn_env", name, err)
		}
	}
}

// A WRONG PASSWORD IS NOT ECHOED BACK EITHER. What a refused login says is
// PostgreSQL's and pgx's to decide, so it is asked of a real server.
func TestARefusedLoginNeverRendersThePassword(t *testing.T) {
	t.Parallel()

	const wrong = "not-the-password-hunter2"

	_, err := OpenPostgresAdmin(t.Context(), t.TempDir(), replaceDSNPassword(t, requirePostgres(t), wrong))

	// The server's own refusal first, so a timeout or a local failure cannot
	// stand in for the login this test is about.
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "28P01" {
		t.Fatalf("the open answered %v, want PostgreSQL's password refusal (28P01)", err)
	}

	if strings.Contains(err.Error(), wrong) {
		t.Errorf("the refused login's error carries the password: %v", err)
	}
}

// THE BACKEND HOLDING THE DSN REDACTS ITSELF TOO. Its dsn field is unexported,
// and fmt, encoding/json and slog reach an unexported field by reflection
// without calling the field's own methods, so a %+v of the backend printed the
// password while the DSN alone redacted. Value and pointer, because both reach a
// format verb.
func TestThePostgresBackendIsRedactedOnEveryRenderingPath(t *testing.T) {
	t.Parallel()

	be := newPostgresBackend(DSN("postgres://billet:" + testDSNPassword + "@db.internal/billet"))

	rendered := map[string]string{
		"%v":          fmt.Sprintf("%v", *be), //nolint:gocritic // the verb path is the subject
		"%+v":         fmt.Sprintf("%+v", *be),
		"%#v":         fmt.Sprintf("%#v", *be),
		"%d":          fmt.Sprintf("%d", *be),
		"%v pointer":  fmt.Sprintf("%v", be), //nolint:gocritic // the verb path is the subject
		"%+v pointer": fmt.Sprintf("%+v", be),
		"%#v pointer": fmt.Sprintf("%#v", be),
		"String()":    be.String(),
		"GoString()":  be.GoString(),
	}

	for name, v := range map[string]any{"json": *be, "json pointer": be} {
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		rendered[name] = string(encoded)
	}

	rendered["LogValue()"] = be.LogValue().String()

	var logged bytes.Buffer

	slog.New(slog.NewJSONHandler(&logged, nil)).Info("opening", "backend", be)

	rendered["slog json"] = logged.String()

	logged.Reset()

	slog.New(slog.NewTextHandler(&logged, nil)).Info("opening", "backend", *be)

	rendered["slog text"] = logged.String()

	for path, out := range rendered {
		if strings.Contains(out, testDSNPassword) {
			t.Errorf("%s rendered the password: %s", path, out)
		}

		if !strings.Contains(out, redactedDSN) {
			t.Errorf("%s did not say the value was redacted: %s", path, out)
		}
	}
}
