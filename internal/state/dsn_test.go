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

// A WRONG PASSWORD IS NOT ECHOED BACK EITHER. What a refused login says is
// PostgreSQL's and pgx's to decide, so it is asked of a real server.
func TestARefusedLoginNeverRendersThePassword(t *testing.T) {
	t.Parallel()

	const wrong = "not-the-password-hunter2"

	_, err := OpenPostgresAdmin(t.Context(), t.TempDir(), replaceDSNPassword(t, requirePostgres(t), wrong))
	if err == nil {
		t.Fatal("an open with the wrong password succeeded")
	}

	if strings.Contains(err.Error(), wrong) {
		t.Errorf("the refused login's error carries the password: %v", err)
	}
}
