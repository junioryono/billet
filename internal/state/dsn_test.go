package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
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
