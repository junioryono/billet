// Package awsidentity verifies an EC2 instance identity document: what EC2 signs, for
// one instance, about that instance (ADR-030). A pool host sends its own with its
// enrollment, and the plane admits the host only when the document verifies and names
// the instance, account, region, launch and address the order's source describes.
//
// THE SIGNATURE IS THE DOCUMENT'S BASE64 `signature`, RSA-SHA256 over the document's
// bytes, under the RSA certificate AWS publishes for the region. MEASURED 2026-11-06 on
// a t3.micro in us-west-2: that pair verifies, and the same signature under the
// region's RSA-2048 and DSA certificates, or as RSA-SHA1, does not; the document's
// pendingTime was the instance's LaunchTime and its privateIp the instance's private
// address. The certificates are AWS's own (certificates.pem says where from).
//
// A DOCUMENT THAT DOES NOT VERIFY PROVES NOTHING EITHER WAY: AWS rotates a region's
// certificate (us-west-2's was issued in 2024 and lapses in 2029), so a signature
// these certificates refuse may be AWS's under one they do not hold yet. Verify
// answers ErrUnverified for it, which a caller reads as could-not-tell, never as a
// contradiction.
//
// A leaf: the standard library only.
package awsidentity

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxDocument bounds a document and MaxSignature its base64 signature, as a request
// carries them. A document measured 475 bytes and its signature 172 characters (2026-11-06).
const (
	MaxDocument  = 4 << 10
	MaxSignature = 1 << 10
)

// ErrUnverified is a document these certificates cannot vouch for: a region they hold
// none for, or a signature none of them verifies. It is could-not-tell.
var ErrUnverified = errors.New("awsidentity: the identity document does not verify under AWS's certificate for its region")

// Document is what EC2 signed about one instance.
type Document struct {
	AccountID        string
	Region           string
	AvailabilityZone string
	InstanceID       string
	InstanceType     string
	ImageID          string
	PrivateIP        string
	Architecture     string
	PendingTime      time.Time
}

//go:embed certificates.pem
var certificates []byte

// Verifier holds each region's public key.
type Verifier struct{ keys map[string]*rsa.PublicKey }

// AWS is the verifier over the certificates AWS publishes.
func AWS() (*Verifier, error) { return FromCertificates(certificates) }

// FromCertificates reads "region: <name>" lines, each followed by one PEM certificate,
// as certificates.pem holds them. A test builds a verifier over keys of its own here.
func FromCertificates(raw []byte) (*Verifier, error) {
	keys := map[string]*rsa.PublicKey{}
	rest := raw
	for {
		i := bytes.Index(rest, []byte("\nregion: "))
		if i < 0 {
			break
		}
		rest = rest[i+len("\nregion: "):]
		end := bytes.IndexByte(rest, '\n')
		if end < 0 {
			return nil, errors.New("awsidentity: a region line ends the certificates")
		}
		region := string(rest[:end])
		block, after := pem.Decode(rest[end:])
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("awsidentity: no certificate follows region %s", region)
		}
		rest = after
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("awsidentity: the certificate for %s: %w", region, err)
		}
		pub, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("awsidentity: the certificate for %s is not RSA", region)
		}
		if _, dup := keys[region]; dup {
			return nil, fmt.Errorf("awsidentity: region %s is listed twice", region)
		}
		keys[region] = pub
	}
	if len(keys) == 0 {
		return nil, errors.New("awsidentity: no certificates")
	}

	return &Verifier{keys: keys}, nil
}

// Regions is how many regions the verifier holds a key for.
func (v *Verifier) Regions() int { return len(v.keys) }

// Verify checks a document and its base64 signature under region's certificate and
// answers what it says. The document must name that region.
func (v *Verifier) Verify(region string, document []byte, signature string) (Document, error) {
	if len(document) == 0 || len(document) > MaxDocument || len(signature) == 0 || len(signature) > MaxSignature {
		return Document{}, fmt.Errorf("%w: a document of %d bytes and a signature of %d", ErrUnverified,
			len(document), len(signature))
	}
	key, ok := v.keys[region]
	if !ok {
		return Document{}, fmt.Errorf("%w: billet holds no certificate for region %q", ErrUnverified, region)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(signature), ""))
	if err != nil {
		return Document{}, fmt.Errorf("%w: the signature is not base64", ErrUnverified)
	}
	sum := sha256.Sum256(document)
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return Document{}, fmt.Errorf("%w (%s)", ErrUnverified, region)
	}

	// SIGNED, SO WHAT FOLLOWS IS EC2'S. A signed document that does not parse, or that
	// names another region than the certificate's, is still not one billet can read.
	var raw struct {
		AccountID        string `json:"accountId"`
		Region           string `json:"region"`
		AvailabilityZone string `json:"availabilityZone"`
		InstanceID       string `json:"instanceId"`
		InstanceType     string `json:"instanceType"`
		ImageID          string `json:"imageId"`
		PrivateIP        string `json:"privateIp"`
		Architecture     string `json:"architecture"`
		PendingTime      string `json:"pendingTime"`
	}
	if err := json.Unmarshal(document, &raw); err != nil {
		return Document{}, fmt.Errorf("%w: the signed document is not JSON", ErrUnverified)
	}
	pending, err := time.Parse(time.RFC3339, raw.PendingTime)
	if err != nil {
		return Document{}, fmt.Errorf("%w: the signed document's pendingTime %q", ErrUnverified, raw.PendingTime)
	}
	if raw.Region != region || raw.InstanceID == "" || raw.AccountID == "" {
		return Document{}, fmt.Errorf("%w: the signed document names region %q and instance %q", ErrUnverified,
			raw.Region, raw.InstanceID)
	}

	return Document{
		AccountID: raw.AccountID, Region: raw.Region, AvailabilityZone: raw.AvailabilityZone,
		InstanceID: raw.InstanceID, InstanceType: raw.InstanceType, ImageID: raw.ImageID,
		PrivateIP: raw.PrivateIP, Architecture: raw.Architecture, PendingTime: pending.UTC(),
	}, nil
}
