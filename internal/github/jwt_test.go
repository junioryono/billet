package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func testKeyPKCS1(t *testing.T) ([]byte, *rsa.PrivateKey) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	encoded := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	return encoded, key
}

// The signature must actually verify. A JWT that merely has three
// dot-separated segments is indistinguishable from a correct one until GitHub
// rejects it, at which point the error says nothing about the cause.
func TestSignAppJWTProducesAVerifiableSignature(t *testing.T) {
	pemBytes, key := testKeyPKCS1(t)

	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)

	token, err := SignAppJWT(1234, pemBytes, now)
	if err != nil {
		t.Fatalf("SignAppJWT: %v", err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}

	signingInput := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signingInput))

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}

	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}

	var header map[string]string
	decodeSegment(t, parts[0], &header)

	if header["alg"] != "RS256" {
		t.Errorf("alg = %q, want RS256", header["alg"])
	}

	var claims map[string]any
	decodeSegment(t, parts[1], &claims)

	if claims["iss"] != "1234" {
		t.Errorf("iss = %v, want \"1234\"", claims["iss"])
	}
}

// GitHub rejects a token whose iat is in the future, and a minute of fast clock
// is common on a host whose NTP has not settled. The resulting error reads like
// a credential problem, which sends people down entirely the wrong path.
func TestSignAppJWTBackdatesIssuedAt(t *testing.T) {
	pemBytes, _ := testKeyPKCS1(t)

	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)

	token, err := SignAppJWT(99, pemBytes, now)
	if err != nil {
		t.Fatalf("SignAppJWT: %v", err)
	}

	var claims map[string]any
	decodeSegment(t, strings.Split(token, ".")[1], &claims)

	iat := int64(claims["iat"].(float64))
	exp := int64(claims["exp"].(float64))

	if iat >= now.Unix() {
		t.Errorf("iat = %d, want strictly before %d so a fast clock still verifies", iat, now.Unix())
	}

	// GitHub's hard limit is 10 minutes; exceeding it is rejected outright.
	if lifetime := time.Duration(exp-now.Unix()) * time.Second; lifetime > 10*time.Minute {
		t.Errorf("token lives %s, want <= 10m", lifetime)
	}
}

// A key downloaded from the web UI can be PKCS#8 while the manifest conversion
// returns PKCS#1. Accepting only one produces a baffling parse error on a key
// that is perfectly valid.
func TestSignAppJWTAcceptsPKCS8(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}

	encoded := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	if _, err := SignAppJWT(7, encoded, time.Now()); err != nil {
		t.Fatalf("SignAppJWT with a PKCS#8 key: %v", err)
	}
}

func TestSignAppJWTRejectsBadInput(t *testing.T) {
	valid, _ := testKeyPKCS1(t)

	if _, err := SignAppJWT(0, valid, time.Now()); err == nil {
		t.Error("app id 0 should be rejected")
	}

	if _, err := SignAppJWT(-1, valid, time.Now()); err == nil {
		t.Error("a negative app id should be rejected")
	}

	if _, err := SignAppJWT(1, []byte("not a pem file"), time.Now()); err == nil {
		t.Error("non-PEM input should be rejected")
	}

	notRSA := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("garbage")})
	if _, err := SignAppJWT(1, notRSA, time.Now()); err == nil {
		t.Error("undecodable key material should be rejected")
	}
}

// Diagnostics must never disclose the key itself.
func TestRedactPEMDisclosesNothing(t *testing.T) {
	pemBytes, _ := testKeyPKCS1(t)

	got := redactPEM(pemBytes)

	if strings.Contains(got, "MII") || strings.Contains(got, "BEGIN") {
		t.Errorf("redactPEM leaked key material: %q", got)
	}

	if !strings.Contains(got, "rsa private key") {
		t.Errorf("redactPEM should name the block type, got %q", got)
	}

	if got := redactPEM([]byte("nonsense")); got != "<not PEM>" {
		t.Errorf("redactPEM(non-PEM) = %q", got)
	}
}

func decodeSegment(t *testing.T, segment string, into any) {
	t.Helper()

	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatalf("decode segment: %v", err)
	}

	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("unmarshal segment: %v", err)
	}
}
