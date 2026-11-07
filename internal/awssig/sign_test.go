package awssig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
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

// S3 CHECKS AN UNSIGNED PAYLOAD ONLY AGAINST WHAT TLS CARRIED, so it never goes without.
func TestAnUnsignedPayloadIsOnlySentOverHTTPS(t *testing.T) {
	t.Parallel()
	creds := Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"}
	plain, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "http://bucket.example/key", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if err := SignStream(plain, creds, "us-west-2", "s3", time.Unix(1, 0)); err == nil {
		t.Fatal("an unsigned payload was signed for plain http")
	}
	if got := plain.Header.Get("Authorization"); got != "" {
		t.Fatalf("the refused request was signed anyway: %q", got)
	}

	secure, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "https://bucket.example/key", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if err := SignStream(secure, creds, "us-west-2", "s3", time.Unix(1, 0)); err != nil {
		t.Fatalf("SignStream: %v", err)
	}
	if got := secure.Header.Get("X-Amz-Content-Sha256"); got != UnsignedPayload {
		t.Fatalf("x-amz-content-sha256 = %q", got)
	}
}
