package app

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/regularfile"
)

// registryMirrorsCALimit bounds the authority a node reads: it travels whole in
// every guest's metadata, and one self-signed certificate is under a kilobyte.
const registryMirrorsCALimit = 16 << 10

// NodeRegistryMirrorsCA is the authority this node's guests trust their mirrors
// by: the one node.registry_mirrors_ca names, else the one the host role installs
// when there is one. Empty means none, so the mirrors' certificates are publicly
// trusted or a guest pulls from the registry.
//
// THE ROLE NAMES NO AUTHORITY IN THE CONFIGURATION, because a billet older than
// that key refuses a file carrying it, and the role writes this file for whatever
// billet the host runs, including one a rollout or a revert moved it back to. So
// the path is fixed and an older billet simply never looks there.
func NodeRegistryMirrorsCA(n *config.NodeConfig) (string, error) {
	if n.RegistryMirrorsCA != "" {
		return ReadRegistryMirrorsCA(n.RegistryMirrorsCA)
	}

	authority, err := ReadRegistryMirrorsCA(config.DefaultRegistryMirrorsCA)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}

	return authority, err
}

// ReadRegistryMirrorsCA reads an authority file and answers its certificates,
// rebuilt from what was validated rather than the file's own bytes.
//
// WHAT THIS RETURNS IS HANDED TO EVERY GUEST AT THE SITE, so a file pointed here
// that also holds a private key must not publish it, and nothing it refuses is
// quoted. pem.Decode silently skips whatever precedes a block it can read,
// damaged key blocks included, so each block must begin where the last one ended,
// and a skipped damaged certificate still reaches nobody.
func ReadRegistryMirrorsCA(path string) (string, error) {
	raw, err := regularfile.ReadFile(path, registryMirrorsCALimit, regularfile.Options{})
	if err != nil {
		return "", fmt.Errorf("read the registry mirrors' authority: %w", err)
	}

	var bundle bytes.Buffer
	for rest := bytes.TrimSpace(raw); len(rest) > 0; rest = bytes.TrimSpace(rest) {
		block, next := pem.Decode(rest)
		if block == nil || !bytes.HasPrefix(rest, []byte("-----BEGIN CERTIFICATE-----")) ||
			block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return "", fmt.Errorf("the registry mirrors' authority %s holds something that is not a PEM "+
				"certificate, and every guest at the site would be handed it", path)
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", fmt.Errorf("the registry mirrors' authority %s holds a certificate that does not parse",
				path)
		}
		if !certificate.IsCA {
			return "", fmt.Errorf("the registry mirrors' authority %s holds a certificate that is no authority",
				path)
		}
		if err := pem.Encode(&bundle, &pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes}); err != nil {
			return "", fmt.Errorf("encode the registry mirrors' authority: %w", err)
		}
		rest = next
	}
	if bundle.Len() == 0 {
		return "", errors.New("the registry mirrors' authority " + path + " holds no certificate")
	}

	return bundle.String(), nil
}
