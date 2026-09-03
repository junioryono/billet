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
	"fmt"
	"strings"
	"time"
)

// appJWTLifetime is how long a generated App JWT is valid.
//
// GitHub rejects anything over 10 minutes. Nine leaves room for clock skew in
// the direction that matters: GitHub also rejects a token whose `iat` is in the
// future, so the claim is backdated rather than issued at exactly now.
const appJWTLifetime = 9 * time.Minute

// appJWTBackdate absorbs a fast local clock. A minute of skew is common on a
// machine whose NTP has not settled, and the resulting failure ("'Issued at'
// claim is in the future") reads like a credential problem rather than a clock
// problem, which sends people down the wrong path entirely.
const appJWTBackdate = 60 * time.Second

// SignAppJWT mints a GitHub App JWT (RS256) from the app's PEM private key.
//
// Written against the standard library rather than pulling in a JWT dependency:
// the whole of it is a header, two claims and one signature, and billet holds
// this key precisely because it is the most sensitive thing in a deployment.
func SignAppJWT(appID int64, privateKeyPEM []byte, now time.Time) (string, error) {
	if appID <= 0 {
		return "", fmt.Errorf("github: app id must be positive, got %d", appID)
	}

	key, err := parseRSAPrivateKey(privateKeyPEM)
	if err != nil {
		return "", err
	}

	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("github: encode jwt header: %w", err)
	}

	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-appJWTBackdate).Unix(),
		"exp": now.Add(appJWTLifetime).Unix(),
		// GitHub accepts the numeric app id here. It also accepts the client id,
		// but the numeric form is what the manifest conversion returns.
		"iss": fmt.Sprintf("%d", appID),
	})
	if err != nil {
		return "", fmt.Errorf("github: encode jwt claims: %w", err)
	}

	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)

	digest := sha256.Sum256([]byte(signingInput))

	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("github: sign jwt: %w", err)
	}

	return signingInput + "." + enc.EncodeToString(signature), nil
}

// ValidatePrivateKey reports whether a PEM is a usable App key.
//
// Exported so `billet check` can prove the configured key WORKS rather than
// merely exists. A truncated PEM — what an interrupted write leaves behind — is
// otherwise not discovered until the first API call, long after the operator has
// been told the deployment is healthy.
func ValidatePrivateKey(pemBytes []byte) error {
	_, err := parseRSAPrivateKey(pemBytes)

	return err
}

// parseRSAPrivateKey accepts both PEM encodings GitHub has produced: PKCS#1
// ("RSA PRIVATE KEY", what the manifest conversion returns today) and PKCS#8
// ("PRIVATE KEY", which a key downloaded from the web UI can be). Accepting only
// one produces a baffling parse error on an otherwise valid key.
func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("github: private key is not PEM-encoded")
	}

	key, pkcs1Err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if pkcs1Err == nil {
		return key, nil
	}

	parsed, pkcs8Err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if pkcs8Err != nil {
		// Report BOTH failures. A corrupted key produces two different complaints
		// depending on which format it was meant to be, and showing only the
		// second sends the reader looking at the wrong one.
		return nil, fmt.Errorf(
			"github: parse private key (PEM block %q): as PKCS#1: %w; as PKCS#8: %w",
			block.Type, pkcs1Err, pkcs8Err)
	}

	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("github: private key is %T, want *rsa.PrivateKey", parsed)
	}

	return key, nil
}

// redactPEM renders a key for human output without disclosing it. Used in
// diagnostics so a wrong-file mistake is still diagnosable.
func redactPEM(pemBytes []byte) string {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return "<not PEM>"
	}

	return "<" + strings.ToLower(block.Type) + ", " + fmt.Sprintf("%d", len(block.Bytes)) + " bytes>"
}
