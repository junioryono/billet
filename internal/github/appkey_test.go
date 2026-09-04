package github

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestAppKey is a real PEM, because a redaction proved against a placeholder
// says nothing about the base64 body a key actually carries.
func newTestAppKey(t *testing.T) AppKey {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}

	return AppKey(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

// secretLine is a line from the PEM body, which is what would appear in a log
// if any rendering path printed the key.
func secretLine(t *testing.T, key AppKey) string {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(string(key)), "\n")
	if len(lines) < 3 {
		t.Fatalf("the test key has %d lines; a PEM has a header, a body and a footer", len(lines))
	}

	return lines[1]
}

// THE APP KEY MINTS TOKENS FOR A WHOLE ORGANIZATION, and it reaches a log
// through one careless verb on whatever struct happens to hold it. Every
// rendering path is covered because each ignores the others: slog's JSON handler
// never consults fmt, %#v never consults String, and %x on a byte slice is a hex
// dump unless Format takes it; encoding/json base64-encodes a byte slice, which
// is the key in another spelling. Each of the five methods was neutered once and
// the path it covers went red.
func TestAnAppKeyIsRedactedOnEveryRenderingPath(t *testing.T) {
	t.Parallel()

	key := newTestAppKey(t)
	secret := secretLine(t, key)

	holder := struct {
		Where string
		Key   AppKey
	}{"minting", key}

	rendered := map[string]string{
		"%v":             fmt.Sprintf("%v", key), //nolint:gocritic // the verb path is the subject
		"%s":             fmt.Sprintf("%s", key), //nolint:gocritic // the verb path is the subject
		"%q":             fmt.Sprintf("%q", key),
		"%x":             fmt.Sprintf("%x", key),
		"%d":             fmt.Sprintf("%d", key),
		"%#v":            fmt.Sprintf("%#v", key),
		"%v on a field":  fmt.Sprintf("%v", holder),
		"%+v on a field": fmt.Sprintf("%+v", holder),
		"%#v on a field": fmt.Sprintf("%#v", holder),
		"String()":       key.String(),
		"GoString()":     key.GoString(),
	}

	encoded, err := json.Marshal(holder)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rendered["json"] = string(encoded)

	var logged bytes.Buffer

	slog.New(slog.NewJSONHandler(&logged, nil)).Info("minting", "key", key)

	rendered["slog json"] = logged.String()

	logged.Reset()

	slog.New(slog.NewTextHandler(&logged, nil)).Info("minting", "key", key)

	rendered["slog text"] = logged.String()

	for path, out := range rendered {
		if strings.Contains(out, secret) {
			t.Errorf("%s rendered the key body: %s", path, out)
		}

		if !strings.Contains(out, redactedAppKey) {
			t.Errorf("%s did not say the value was redacted: %s", path, out)
		}
	}
}

// THE BYTES ARE STILL THE KEY for the one reader that needs them: the signer.
func TestAnAppKeyStillSignsAsItself(t *testing.T) {
	t.Parallel()

	key := newTestAppKey(t)

	if err := key.Validate(); err != nil {
		t.Fatalf("a freshly generated key does not validate: %v", err)
	}

	if !strings.Contains(string(key), "PRIVATE KEY") {
		t.Fatal("converting an AppKey back to a string lost the PEM the signer needs")
	}
}

// THE HARDENED READER IS THE ONE READER, so its refusals are asserted here where
// it lives. Each case is a file that looks configured and is not.
func TestReadPrivateKeyFileRefusesWhatIsNotAUsableKey(t *testing.T) {
	t.Parallel()

	valid := newTestAppKey(t)

	for name, setup := range map[string]func(t *testing.T, dir string) (string, string){
		"missing": func(_ *testing.T, dir string) (string, string) {
			return filepath.Join(dir, "absent.pem"), ""
		},
		"directory": func(t *testing.T, dir string) (string, string) {
			t.Helper()

			p := filepath.Join(dir, "adir")
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			return p, "not a regular file"
		},
		"empty": func(t *testing.T, dir string) (string, string) {
			t.Helper()

			p := filepath.Join(dir, "empty.pem")
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			return p, "is empty"
		},
		"truncated": func(t *testing.T, dir string) (string, string) {
			t.Helper()

			p := filepath.Join(dir, "trunc.pem")
			if err := os.WriteFile(p, valid[:80], 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			return p, "not PEM-encoded"
		},
		"oversized": func(t *testing.T, dir string) (string, string) {
			t.Helper()

			p := filepath.Join(dir, "huge.pem")
			if err := os.WriteFile(p, make([]byte, MaxKeySize+1), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			return p, "not an App key"
		},
		"world-readable": func(t *testing.T, dir string) (string, string) {
			t.Helper()

			p := filepath.Join(dir, "open.pem")
			if err := os.WriteFile(p, valid, 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}

			return p, "readable beyond its owner"
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path, want := setup(t, t.TempDir())

			_, err := ReadPrivateKeyFile(path)
			if err == nil {
				t.Fatalf("%s: ReadPrivateKeyFile accepted it", name)
			}

			if want != "" && !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not say %q", name, err, want)
			}
		})
	}

	good := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(good, valid, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	read, err := ReadPrivateKeyFile(good)
	if err != nil {
		t.Fatalf("a mode-0600 valid key was refused: %v", err)
	}

	if !bytes.Equal(read, valid) {
		t.Fatal("the reader returned different bytes from the file it read")
	}
}
