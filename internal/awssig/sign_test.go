package awssig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestCredentialsNeverRenderTheirSecrets(t *testing.T) {
	t.Parallel()

	credentials := Credentials{
		AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret-access-key", SessionToken: "session-token",
	}
	jsonValue, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	slog.New(slog.NewJSONHandler(&log, nil)).Info("credentials", "aws", credentials)
	outputs := []string{
		credentials.String(), fmt.Sprintf("%+v", credentials), fmt.Sprintf("%#v", credentials),
		string(jsonValue), log.String(),
	}
	for _, output := range outputs {
		if strings.Contains(output, credentials.SecretAccessKey) ||
			strings.Contains(output, credentials.SessionToken) {
			t.Fatalf("credential rendering exposed a secret: %s", output)
		}
		if !strings.Contains(output, "REDACTED") {
			t.Fatalf("credential rendering did not make redaction explicit: %s", output)
		}
	}
}
