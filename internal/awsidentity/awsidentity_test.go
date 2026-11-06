package awsidentity

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// signer is a key and the certificate file that names it for one region.
type signer struct {
	key *rsa.PrivateKey
	pem string
}

func newSigner(t *testing.T, region string) signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{Organization: []string{"test"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	return signer{key: key, pem: "region: " + region + "\n" + string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

func (s signer) sign(t *testing.T, doc []byte) string {
	t.Helper()
	sum := sha256.Sum256(doc)
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.StdEncoding.EncodeToString(sig)

	// AS IMDS SERVES IT: wrapped across lines.
	return enc[:64] + "\n" + enc[64:128] + "\n" + enc[128:]
}

func document(t *testing.T, fields map[string]string) []byte {
	t.Helper()
	d := map[string]string{
		"accountId": "000000000000", "region": "us-west-2", "availabilityZone": "us-west-2a",
		"instanceId": "i-0123456789abcdef0", "instanceType": "c7i.metal-24xl", "imageId": "ami-0abc",
		"privateIp": "10.0.0.7", "architecture": "x86_64", "pendingTime": "2026-11-06T14:26:45Z",
	}
	for k, v := range fields {
		if v == "" {
			delete(d, k)
		} else {
			d[k] = v
		}
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

// A DOCUMENT SIGNED UNDER ITS REGION'S CERTIFICATE IS READ, whatever lines the
// signature is wrapped across; one the certificate does not verify, or that names
// another region, or whose region the verifier holds no certificate for, or that is
// signed but malformed, is unverified: could-not-tell, never a contradiction.
func TestADocumentVerifiesOnlyUnderItsRegionsCertificate(t *testing.T) {
	t.Parallel()
	west, east := newSigner(t, "us-west-2"), newSigner(t, "us-east-1")
	v, err := FromCertificates([]byte("# header\n\n" + west.pem + "\n" + east.pem))
	if err != nil || v.Regions() != 2 {
		t.Fatalf("parse: %v, %d regions", err, v.Regions())
	}
	doc := document(t, nil)
	got, err := v.Verify("us-west-2", doc, west.sign(t, doc))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	want := Document{
		AccountID: "000000000000", Region: "us-west-2", AvailabilityZone: "us-west-2a",
		InstanceID: "i-0123456789abcdef0", InstanceType: "c7i.metal-24xl", ImageID: "ami-0abc",
		PrivateIP: "10.0.0.7", Architecture: "x86_64", PendingTime: time.Date(2026, 11, 6, 14, 26, 45, 0, time.UTC),
	}
	if got != want {
		t.Fatalf("read %+v, want %+v", got, want)
	}

	tampered := append([]byte(nil), doc...)
	tampered[len(tampered)-3] ^= 1
	east1 := document(t, map[string]string{"region": "us-east-1"})
	for name, c := range map[string]struct {
		region    string
		doc       []byte
		signature string
	}{
		"another region's certificate": {"us-east-1", doc, west.sign(t, doc)},
		"a changed byte":               {"us-west-2", tampered, west.sign(t, doc)},
		"another key":                  {"us-west-2", doc, east.sign(t, doc)},
		"a region with no certificate": {"eu-west-1", doc, west.sign(t, doc)},
		"a document of another region": {"us-west-2", east1, west.sign(t, east1)},
		"not base64":                   {"us-west-2", doc, "!!!"},
		"no signature":                 {"us-west-2", doc, ""},
		"signed but not JSON":          {"us-west-2", []byte("not json"), west.sign(t, []byte("not json"))},
		"signed with no instance": {"us-west-2", document(t, map[string]string{"instanceId": ""}),
			west.sign(t, document(t, map[string]string{"instanceId": ""}))},
		"signed with no pendingTime": {"us-west-2", document(t, map[string]string{"pendingTime": ""}),
			west.sign(t, document(t, map[string]string{"pendingTime": ""}))},
	} {
		if _, err := v.Verify(c.region, c.doc, c.signature); !errors.Is(err, ErrUnverified) {
			t.Errorf("%s: %v, want ErrUnverified", name, err)
		}
	}

	// PAST THE BOUND, A SIGNED DOCUMENT THAT WOULD OTHERWISE BE READ IS REFUSED BY THE
	// BOUND: valid JSON naming the instance, padded past MaxDocument.
	big := document(t, map[string]string{"imageId": strings.Repeat("a", MaxDocument)})
	if _, err := v.Verify("us-west-2", big, west.sign(t, big)); !errors.Is(err, ErrUnverified) ||
		!strings.Contains(err.Error(), "a document of") {
		t.Errorf("a document past the bound: %v, want refused by its size", err)
	}
	if _, err := v.Verify("us-west-2", doc, strings.Repeat("A", MaxSignature+1)); !errors.Is(err, ErrUnverified) ||
		!strings.Contains(err.Error(), "a signature of") {
		t.Errorf("a signature past the bound: %v, want refused by its size", err)
	}
}

// A CERTIFICATE FILE THAT CANNOT BE READ WHOLE IS REFUSED, each for its own reason: a
// region with no certificate after it, a region listed twice, a key that is not RSA, a
// region line with nothing after it, or nothing at all.
func TestACertificateFileIsReadWholeOrRefused(t *testing.T) {
	t.Parallel()
	a := newSigner(t, "us-west-2")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ec := "region: eu-west-1\n" + string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	for name, c := range map[string]struct{ raw, says string }{
		"nothing":               {"# only a header\n", "no certificates"},
		"a region and no pem":   {"\nregion: us-west-2\nnothing here\n", "no certificate follows"},
		"a region twice":        {"\n" + a.pem + "\n" + a.pem, "listed twice"},
		"a trailing region":     {"\n" + a.pem + "\nregion: us-east-1", "ends the certificates"},
		"a key that is not RSA": {"\n" + ec, "not RSA"},
	} {
		if _, err := FromCertificates([]byte(c.raw)); err == nil || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: %v, want a refusal saying %q", name, err, c.says)
		}
	}
}

// AWS'S CERTIFICATES ARE ALL READ, one RSA key for every region AWS listed on
// 2026-11-06, and us-west-2's is the key that verified a real document that day.
func TestAWSsCertificatesAreReadForEveryRegion(t *testing.T) {
	t.Parallel()
	v, err := AWS()
	if err != nil {
		t.Fatal(err)
	}
	if v.Regions() != 39 {
		t.Fatalf("%d regions, want the 39 AWS listed", v.Regions())
	}
	der, err := x509.MarshalPKIXPublicKey(v.keys["us-west-2"])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	if got := hex.EncodeToString(sum[:]); got != "8a61ba393a4aa067e73b9b5e75851ea4345aa5e6dab02538e8c1acd435b1f93f" {
		t.Fatalf("us-west-2's key is %s, not the one that verified a real document", got)
	}
}

// A REAL DOCUMENT, OPT-IN: BILLET_TEST_AWS_IDENTITY_DIR names a directory holding an
// instance's `document` and `signature` as IMDS served them, which verify under the
// embedded certificate for the document's region. Kept out of the tree, because a
// document names its account.
func TestARealDocumentVerifies(t *testing.T) {
	t.Parallel()
	dir := os.Getenv("BILLET_TEST_AWS_IDENTITY_DIR")
	if dir == "" {
		t.Skip("BILLET_TEST_AWS_IDENTITY_DIR is not set")
	}
	doc, err := os.ReadFile(filepath.Join(dir, "document"))
	if err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile(filepath.Join(dir, "signature"))
	if err != nil {
		t.Fatal(err)
	}
	var head struct {
		Region string `json:"region"`
	}
	if err := json.Unmarshal(doc, &head); err != nil {
		t.Fatal(err)
	}
	v, err := AWS()
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Verify(head.Region, doc, string(sig))
	if err != nil {
		t.Fatalf("a real document: %v", err)
	}
	if got.InstanceID == "" || got.PrivateIP == "" || got.PendingTime.IsZero() {
		t.Fatalf("read %+v", got)
	}
}
