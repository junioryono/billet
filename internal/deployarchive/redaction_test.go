package deployarchive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// EVERY RENDERING PATH REDACTS THE KEY. fmt consults Stringer for some verbs
// and formats the fields for the rest, %#v consults GoString, encoding/json
// consults neither, and slog's JSON handler consults only LogValue; each
// method covers one of them and the test walks all of them with a key whose
// bytes are recognisable.
func TestTheKeyBearingRequestTypesRedactOnEveryPath(t *testing.T) {
	t.Parallel()

	const marker = "-----BEGIN RSA PRIVATE KEY-----\nSECRETKEYBYTES\n-----END RSA PRIVATE KEY-----\n"

	key := TargetKey{
		Name:      "personal",
		GitHub:    GitHubIdentity{Repository: "someone/widgets", AppID: 7, InstallationID: 8},
		AppKeyPEM: []byte(marker),
	}

	req := BackupRequest{
		Dest:         "/tmp/out",
		StateDir:     "/var/lib/billet/server",
		DeploymentID: "dep",
		GitHub:       GitHubIdentity{Org: "acme", AppID: 1, InstallationID: 2},
		AppKeyPEM:    []byte(marker),
		Targets:      []TargetKey{key},
		Snapshot:     func(context.Context, string) error { return nil },
	}

	for name, subject := range map[string]any{"target key": key, "backup request": req} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d", "%x", "%q"} {
				if out := fmt.Sprintf(verb, subject); strings.Contains(out, "SECRETKEYBYTES") ||
					strings.Contains(out, "53454352") {
					t.Errorf("%s rendered the key: %s", verb, out)
				}
			}

			out, err := json.Marshal(subject)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}

			if bytes.Contains(out, []byte("SECRETKEYBYTES")) || bytes.Contains(out, []byte("U0VDUkVU")) {
				t.Errorf("JSON rendered the key: %s", out)
			}

			var log bytes.Buffer

			slog.New(slog.NewJSONHandler(&log, nil)).Info("subject", "value", subject)

			if strings.Contains(log.String(), "SECRETKEYBYTES") || strings.Contains(log.String(), "U0VDUkVU") {
				t.Errorf("slog rendered the key: %s", log.String())
			}

			if !strings.Contains(log.String(), "[redacted]") {
				t.Errorf("slog did not go through LogValue: %s", log.String())
			}
		})
	}
}
