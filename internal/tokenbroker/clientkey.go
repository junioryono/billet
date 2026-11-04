package tokenbroker

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/regularfile"
	"github.com/junioryono/billet/internal/wirecert"
)

// ErrClientKeyExists means a host's broker key is already there, and is never
// replaced: the broker knows it by fingerprint, so a new one locks this host out
// until somebody registers it.
var ErrClientKeyExists = errors.New("tokenbroker: this host already has a broker key")

// CreateClientKey makes the key a host proves its deployment with, at a path nothing
// holds, and answers its fingerprint.
func CreateClientKey(path string) (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("tokenbroker: generate a key: %w", err)
	}

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("tokenbroker: encode the key: %w", err)
	}

	encoded := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	// NOT CREATED HERE. A root run would leave a directory only root can enter, and handing
	// back a directory is not this command's to decide, so the service could never read
	// the key this printed a registration for.
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return "", fmt.Errorf("tokenbroker: %s, which is to hold this key, cannot be used; create it where the "+
			"service account can enter it: %w", filepath.Dir(path), err)
	}

	_, err = durablefile.Installer{}.InstallNew(filepath.Dir(path), filepath.Base(path), 0o600,
		func(w io.Writer) error {
			_, err := w.Write(encoded)

			return err //nolint:wrapcheck // the installer names the file.
		})
	if errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("%w: %s. Its fingerprint is what the broker knows; `billet broker client-key "+
			"--show` prints it", ErrClientKeyExists, path)
	}
	if err != nil {
		return "", fmt.Errorf("tokenbroker: install the key at %s: %w", path, err)
	}

	return fingerprintOf(key)
}

// ReadClientKey reads a host's key with the rules an App key has: one descriptor,
// a regular file, its owner's alone.
func ReadClientKey(path string) (crypto.Signer, error) {
	f, info, err := regularfile.Open(path, regularfile.Options{})
	if err != nil {
		return nil, fmt.Errorf("server.token_broker.client_key_path %s: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return nil, fmt.Errorf("server.token_broker.client_key_path %s is mode %04o; it is readable beyond "+
				"its owner. Run: chmod 600 %s", path, perm, path)
		}
	}

	body, err := regularfile.ReadAllLimited(f, path, maxPEM)
	if err != nil {
		return nil, fmt.Errorf("read server.token_broker.client_key_path %s: %w", path, err)
	}

	block, _ := pem.Decode(body)
	if block == nil {
		return nil, fmt.Errorf("server.token_broker.client_key_path %s is not PEM", path)
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// NOT WRAPPED: a parser's error describes the bytes of a private key.
		return nil, fmt.Errorf("server.token_broker.client_key_path %s does not parse as a private key", path)
	}

	signer, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("server.token_broker.client_key_path %s is not a key that signs", path)
	}

	return signer, nil
}

// Fingerprint is what a broker knows a key by, in the form billet's other
// fingerprints take, so two people compare them the same way.
func Fingerprint(key crypto.Signer) (string, error) {
	return fingerprintOf(key)
}

func fingerprintOf(key crypto.Signer) (string, error) {
	spki, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return "", fmt.Errorf("tokenbroker: encode the public key: %w", err)
	}

	return wirecert.Fingerprint(spki), nil
}

// clientCertificateLife is how long the in-memory certificate says it lasts. It is
// never checked: the listener verifies no chain and the broker pins the key, so it
// exists only because TLS has nowhere else to carry a public key.
const clientCertificateLife = 10 * 365 * 24 * time.Hour

// ClientCertificate wraps a host's key in the self-signed certificate TLS needs, made
// fresh at each start and kept nowhere.
func ClientCertificate(key crypto.Signer) (tls.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tokenbroker: a serial: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "billet control plane"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(clientCertificateLife),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tokenbroker: sign the certificate: %w", err)
	}

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// LoadRoots reads the PEM bundle a broker's certificate is verified against. An empty
// path is the system's roots, which is a nil pool.
func LoadRoots(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil //nolint:nilnil // a nil pool is how crypto/tls spells the system's roots.
	}

	body, err := loadBounded(path)
	if err != nil {
		return nil, fmt.Errorf("server.token_broker.ca_file: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(body) {
		return nil, fmt.Errorf("server.token_broker.ca_file %s holds no certificate", path)
	}

	return pool, nil
}

// ServerTLS is the broker's side of the handshake: its certificate, and one the
// caller must present.
//
// RequireAnyClientCert, NOT A CHAIN: a deployment's key is registered by fingerprint,
// so no authority decides who may connect, and the handshake proves the caller holds
// the key it presents.
func ServerTLS(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("tokenbroker: load the broker's certificate: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}
