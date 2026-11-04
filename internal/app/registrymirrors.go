package app

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"

	"github.com/junioryono/billet/internal/regularfile"
)

// registryMirrorsCALimit bounds the authority a node reads: it travels whole in
// every guest's metadata, and one self-signed certificate is under a kilobyte.
const registryMirrorsCALimit = 16 << 10

// ReadRegistryMirrorsCA reads the authority node.registry_mirrors_ca names, and
// answers empty when it names none.
//
// ONLY CA CERTIFICATES PASS, and the whole file must be them, because what this
// returns is handed to every guest at the site: a role or an operator pointing
// the key at a file that also holds a private key would publish that key to every
// job. So nothing it refuses is quoted either.
func ReadRegistryMirrorsCA(path string) (string, error) {
	if path == "" {
		return "", nil
	}

	raw, err := regularfile.ReadFile(path, registryMirrorsCALimit, regularfile.Options{})
	if err != nil {
		return "", fmt.Errorf("read node.registry_mirrors_ca: %w", err)
	}

	count := 0
	for rest := raw; ; count++ {
		block, next := pem.Decode(rest)
		if block == nil {
			if len(bytes.TrimSpace(rest)) != 0 {
				return "", fmt.Errorf("node.registry_mirrors_ca %s holds something that is not a PEM certificate",
					path)
			}

			break
		}
		if block.Type != "CERTIFICATE" {
			return "", fmt.Errorf("node.registry_mirrors_ca %s holds a PEM block that is not a certificate, "+
				"and every guest at the site would be handed it", path)
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", fmt.Errorf("node.registry_mirrors_ca %s holds a certificate that does not parse", path)
		}
		if !certificate.IsCA {
			return "", fmt.Errorf("node.registry_mirrors_ca %s holds a certificate that is no authority", path)
		}
		rest = next
	}
	if count == 0 {
		return "", errors.New("node.registry_mirrors_ca " + path + " holds no certificate")
	}

	return string(raw), nil
}
