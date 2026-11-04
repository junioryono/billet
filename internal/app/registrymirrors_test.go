package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// aCertificatePEM is one self-signed certificate, an authority or not.
func aCertificatePEM(t *testing.T, authority bool) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "billet test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: authority, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create a certificate: %v", err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestARegistryMirrorAuthorityReachesAGuestOnlyAsCertificates(t *testing.T) {
	t.Parallel()

	authority := aCertificatePEM(t, true)
	const key = "-----BEGIN PRIVATE KEY-----\nc2VjcmV0\n-----END PRIVATE KEY-----\n"

	for _, tc := range []struct {
		name, body, refusal string
	}{
		{name: "an authority", body: authority},
		{name: "two authorities", body: authority + aCertificatePEM(t, true)},
		{name: "a private key after it", body: authority + key, refusal: "not a certificate"},
		{name: "a private key", body: key, refusal: "not a certificate"},
		{name: "a certificate that is no authority", body: aCertificatePEM(t, false), refusal: "no authority"},
		{name: "text after it", body: authority + "and more\n", refusal: "not a PEM certificate"},
		{name: "nothing", body: "\n", refusal: "no certificate"},
		{name: "a block that does not parse", refusal: "does not parse",
			body: "-----BEGIN CERTIFICATE-----\nc2VjcmV0\n-----END CERTIFICATE-----\n"},
		{name: "more than a guest's metadata should carry", refusal: "larger than the limit",
			body: authority + strings.Repeat(" ", registryMirrorsCALimit)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "ca.pem")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write the file: %v", err)
			}
			got, err := ReadRegistryMirrorsCA(path)
			if tc.refusal == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if got != tc.body {
					t.Fatalf("read %q, want the file whole", got)
				}

				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("error = %v, want one saying %q", err, tc.refusal)
			}
			if got != "" {
				t.Fatalf("a refusal still answered %q", got)
			}
			// THE REFUSAL NAMES THE FILE, never what is in it.
			if strings.Contains(err.Error(), "c2VjcmV0") {
				t.Fatalf("the refusal quoted the file: %v", err)
			}
		})
	}
}

func TestNoRegistryMirrorAuthorityIsNotARefusal(t *testing.T) {
	t.Parallel()

	if got, err := ReadRegistryMirrorsCA(""); got != "" || err != nil {
		t.Fatalf("ReadRegistryMirrorsCA(\"\") = %q, %v", got, err)
	}
}

func TestAnUnreadableRegistryMirrorAuthorityIsARefusal(t *testing.T) {
	t.Parallel()

	if _, err := ReadRegistryMirrorsCA(filepath.Join(t.TempDir(), "absent.pem")); err == nil {
		t.Fatal("an absent authority answered nothing wrong")
	}
}
