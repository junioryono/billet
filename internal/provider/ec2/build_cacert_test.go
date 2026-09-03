package ec2

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
)

// mintCert returns a self-signed certificate PEM. isCA sets the basic-constraint
// that decides whether it is a CA — the property canonicalizeCACert turns on.
func mintCert(t *testing.T, isCA bool) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "billet-cache-ca-test"},
		IsCA:                  isCA,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// mintKeyPEM returns an EC PRIVATE KEY PEM — what an operator must never pass to
// --ca-cert, and what canonicalizeCACert exists to catch.
func mintKeyPEM(t *testing.T) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

// A --ca-cert BUNDLE IS VALIDATED BEFORE A PAID BUILDER LAUNCHES.
//
// The failure modes are asymmetric and each has a distinct cost: a private key
// would be baked into a machine image; a leaf certificate would install an anchor
// that signs nothing; unparseable bytes or trailing shell would produce an image
// that cannot reach the cache — or run injected commands as root. Each is refused
// up front, by what is wrong with it.
func TestCanonicalizeCACertRefusesWhatIsNotACABundle(t *testing.T) {
	t.Parallel()

	ca := mintCert(t, true)
	ca2 := mintCert(t, true)
	leaf := mintCert(t, false)
	key := mintKeyPEM(t)
	// A CERTIFICATE block with valid armor but a corrupted base64 body: pem.Decode
	// skips it silently when a valid block follows, so the count check must catch it.
	mangled := mangleCertBody(t, mintCert(t, true))

	for _, tc := range []struct {
		name    string
		pem     string
		wantErr string // "" means it must be accepted
	}{
		{"empty is optional", "", ""},
		{"a single CA", ca, ""},
		{"a CA plus an intermediate CA", ca + ca2, ""},
		{"a CA bundled with a leaf", ca + leaf, ""},
		{"a leading comment is skipped", "# my cache root\n" + ca, ""},
		{"only a leaf", leaf, "none is a CA"},
		{"a private key", key, "only CA"},
		{"a CA followed by a key", ca + key, "only CA"},
		{"not PEM at all", "this is not a certificate", "no PEM CERTIFICATE block"},
		// THE INJECTION: a valid cert followed by shell. The old validator ignored
		// trailing bytes and copied them verbatim into a heredoc.
		{"trailing shell after a real cert", ca + "\npoweroff\nrm -rf /\n", "trailing data"},
		{"a mangled cert between two valid ones", ca + mangled + ca2, "malformed"},
		{"a mangled cert after a valid one", ca + mangled, "malformed"},
		{"a PEM block that is not a certificate", string(pem.EncodeToMemory(
			&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not-der")})), "not a valid X.509"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := canonicalizeCACert(tc.pem)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("rejected a valid bundle: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("accepted an invalid bundle, want an error naming %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("the error does not name %q: %v", tc.wantErr, err)
			}
		})
	}
}

// THE OUTPUT IS BILLET'S OWN RE-ENCODING, not the operator's bytes. A leading
// comment and any surrounding text are gone; what remains parses back to the same
// certificate. This is the structural half of the injection fix: nothing the
// operator wrote reaches the provisioning script.
func TestCanonicalizeCACertReturnsCanonicalBytes(t *testing.T) {
	t.Parallel()

	ca := mintCert(t, true)
	// A leading comment is skipped by pem.Decode; it must not survive into the
	// canonical output. (A TRAILING comment would be trailing data and rejected —
	// covered separately — so the round-trip input keeps the comment up front.)
	input := "# a comment the operator wrote\n" + ca

	out, err := canonicalizeCACert(input)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if strings.Contains(out, "a comment the operator wrote") {
		t.Error("the operator's comment survived into the canonical output")
	}
	if _, err := canonicalizeCACert(out); err != nil {
		t.Fatalf("the canonical output does not itself validate: %v", err)
	}
}

// lineOf returns the index of the one EXECUTABLE script line containing substr,
// failing if it is absent or appears on more than one such line. Shell comment
// lines are skipped, so a commented-out occurrence cannot stand in for the real
// command; combined with the uniqueness check, the match is the executable line.
func lineOf(t *testing.T, lines []string, substr string) int {
	t.Helper()

	found := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}

		if strings.Contains(l, substr) {
			if found >= 0 {
				t.Fatalf("%q appears on more than one line (%d and %d); the ordering check is ambiguous",
					substr, found, i)
			}

			found = i
		}
	}
	if found < 0 {
		t.Fatalf("the script never does %q on an executable line", substr)
	}

	return found
}

// GIVEN A CA, THE IMAGE TRUSTS IT — the exact certificate, in the host store, in
// the right order, and still under EC2's user-data limit.
func TestProvisionScriptInstallsTheCACert(t *testing.T) {
	t.Parallel()

	ca := mintCert(t, true)

	// STAGED, WHICH IS THE SHAPE A PARITY BUILD ACTUALLY HAS. The pinned
	// declaration no longer compresses into EC2's 16384 bytes with the installers
	// embedded -- measured at 16453 -- so a build without a payload bucket is
	// refused, by name, and TestTheEmbeddedPathRefusesWhenItCannotFit is what
	// covers that. What this test is about is the CA ordering, so it must not also
	// be the one test standing on a delivery shape parity has outgrown.
	script, err := provisionScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest,
		RunnerVersion: "2.328.0",
		Arch:          "x64",
		CACertPEM:     ca,
	})
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}

	// DELIVERABLE, NOT PLAIN. This asserted `len(script) <= maxUserData`, which was
	// the right property under the old contract and is the wrong one now: a script
	// over the plain budget is compressed and delivered, so the plain assertion
	// would fail on a correct build the moment parity pushes past 16 KiB. What must
	// hold is that the script can be CARRIED -- which is what packUserData answers.
	if _, err := packUserData(script); err != nil {
		t.Fatalf("the script with a CA cannot be delivered as user data: %v", err)
	}

	// THE COMPLETE ORDERING, by line. Each step depends on the one before: the apt
	// transaction provides ca-certificates and therefore update-ca-certificates,
	// the anchor must exist before the refresh reads it, the refresh must be proved
	// to have taken, and the whole thing must land before the runner so a first
	// job's cache request already trusts the endpoint. The CA-specific lines are
	// unique (lineOf enforces it); the runner entry point recurs, so its FIRST line
	// is the boundary the CA install must precede.
	//
	// THE ANCHOR PATH AND THE REFRESH ARE DEBIAN'S. On the dnf base these were
	// /etc/pki/ca-trust and `update-ca-trust extract`; writing there on Ubuntu
	// leaves a file nothing reads, so the paths are part of the contract rather
	// than an incidental spelling.
	lines := strings.Split(script, "\n")
	firstInstall := firstLineOf(t, lines, "apt-get -o DPkg::Lock::Timeout=600 install")
	anchor := lineOf(t, lines, "base64 -d > /usr/local/share/ca-certificates/billet-cache-ca.crt")
	extract := lineOf(t, lines, "update-ca-certificates")
	// THE ORDERING ONLY. Whether this check can actually fail is decided by
	// TestTheAnchorProofRefusesABundleWithoutTheAnchor, which runs it; three
	// versions of it satisfied a test like this one and proved nothing.
	proof := lineOf(t, lines, "if ! awk -v A=")

	firstRunner := -1
	for i, l := range lines {
		if strings.Contains(l, "/usr/local/bin/billet-runner") {
			firstRunner = i

			break
		}
	}
	if firstRunner < 0 {
		t.Fatal("the script never references the runner entry point")
	}

	sequence := []struct {
		name string
		at   int
	}{
		{"apt install", firstInstall},
		{"anchor write", anchor},
		{"update-ca-certificates", extract},
		{"proof the anchor took", proof},
		{"runner entry point", firstRunner},
	}
	for i := 1; i < len(sequence); i++ {
		if sequence[i].at <= sequence[i-1].at {
			t.Errorf("%s (line %d) must come after %s (line %d)",
				sequence[i].name, sequence[i].at, sequence[i-1].name, sequence[i-1].at)
		}
	}

	want, err := canonicalizeCACert(ca)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}

	// THE CERTIFICATE'S OWN BYTES MUST NOT APPEAR: it travels as a base64 blob, so
	// any line of it in the script would mean the operator's input reached the
	// shell unencoded — the exact shape the injection fix removes.
	//
	// THE ARMOR IS NOT THE TEST, and asserting on it was wrong. This checked for
	// `-----BEGIN CERTIFICATE-----` as a proxy for "the PEM is here", which held
	// only while nothing else in the script had a reason to name it. The anchor
	// proof now matches on that exact string to find certificate boundaries, so
	// the proxy fails on a correct build while a script carrying the real bytes
	// under different armor would have passed. Assert the BODY, which is the part
	// that is actually the operator's.
	for _, line := range strings.Split(strings.TrimSpace(want), "\n") {
		if strings.HasPrefix(line, "-----") || line == "" {
			continue
		}

		if strings.Contains(script, line) {
			t.Errorf("a line of the certificate body appears in the script rather than "+
				"only inside the base64 blob: %q", line)
		}
	}

	// THE BLOB IS THE RIGHT CERTIFICATE, not merely some base64. Decode what the
	// script pipes into base64 -d and require it to equal billet's canonical PEM,
	// so a bug that embedded an empty or wrong blob is caught.
	blob := blobFromScript(t, lines[anchor])
	decoded, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		t.Fatalf("the embedded blob is not valid base64: %v", err)
	}
	if string(decoded) != want {
		t.Errorf("the embedded blob decodes to something other than the canonical CA PEM")
	}
}

// blobFromScript pulls the base64 payload out of the anchor line, which has the
// shape: printf '%s' '<blob>' | base64 -d > <path>
func blobFromScript(t *testing.T, line string) string {
	t.Helper()

	const open = "printf '%s' '"
	i := strings.Index(line, open)
	if i < 0 {
		t.Fatalf("the anchor line is not the expected printf pipeline: %q", line)
	}

	rest := line[i+len(open):]
	j := strings.Index(rest, "'")
	if j < 0 {
		t.Fatalf("the anchor line has no closing quote: %q", line)
	}

	return rest[:j]
}

// A CRLF-ENCODED (WINDOWS) CA BUNDLE IS NOT FALSELY REJECTED, and neither is
// trailing whitespace — but trailing non-whitespace still is. A real CA file an
// operator exports may carry either; the injection guard must not turn a legitimate
// bundle away.
func TestCanonicalizeCACertAcceptsCRLFAndTrailingWhitespace(t *testing.T) {
	t.Parallel()

	ca := mintCert(t, true)

	if _, err := canonicalizeCACert(strings.ReplaceAll(ca, "\n", "\r\n")); err != nil {
		t.Errorf("a CRLF CA bundle was rejected: %v", err)
	}
	if _, err := canonicalizeCACert(ca + "\n\n  \n"); err != nil {
		t.Errorf("trailing whitespace was rejected: %v", err)
	}
	if _, err := canonicalizeCACert(ca + "poweroff\n"); err == nil {
		t.Error("trailing non-whitespace shell was accepted")
	}
}

// WITHOUT A CA, NOTHING ABOUT THE TRUST STORE CHANGES. The feature is opt-in, so
// an ordinary build must not carry ca-certificates handling it did not ask for —
// making the install unconditional must fail this test.
func TestProvisionScriptOmitsTheCACertWhenNoneIsGiven(t *testing.T) {
	t.Parallel()

	script := mustScript(t)

	// THE ANCHOR, NOT THE PACKAGE. `ca-certificates` is now installed on every
	// image because the machine needs a trust store to reach github.com at all --
	// so asserting the package name absent, as this once did, would fail on every
	// correct build. What must be absent when nobody asked for a private CA is the
	// ANCHOR: a certificate written into the trust store and the refresh that makes
	// the machine believe it.
	for _, unwanted := range []string{
		"billet-cache-ca.crt",
		"update-ca-certificates",
		"/usr/local/share/ca-certificates",
		// THE OLD RED HAT SHAPE TOO. If these ever come back, the anchor is being
		// written where Ubuntu does not read it -- which is not a missing anchor
		// but a silently ineffective one.
		"ca-trust",
		"update-ca-trust",
	} {
		if strings.Contains(script, unwanted) {
			t.Errorf("a build with no --ca-cert still emits %q:\n%s", unwanted, script)
		}
	}
}

// AN OVERSIZED BUNDLE IS REFUSED BEFORE ANY PAID INSTANCE. A CA bundle large
// enough to push the script past EC2's user-data limit must fail the build, and
// crucially must do so before RunInstances — a build that launched a paying
// instance and only then discovered it could not carry its own provisioning would
// be the worst outcome.
func TestAnOversizedCACertIsRefusedBeforeAnyLaunch(t *testing.T) {
	t.Parallel()

	// One valid CA repeated until the bundle is past what a trust store may be.
	//
	// THIS USED TO RELY ON THE USER-DATA LIMIT and no longer can. A bundle of one
	// certificate repeated sixty times is the most compressible input imaginable,
	// so once the script began to be gzipped it fitted comfortably and this test
	// failed — asserting a refusal that had quietly stopped happening. The bound is
	// now on the bundle itself, which is the thing actually worth bounding: every
	// certificate in it becomes an authority every job on the image believes.
	bundle := strings.Repeat(mintCert(t, true), 60)

	if len(bundle) <= maxCACertPEM {
		t.Fatalf("the fixture is %d bytes and the limit is %d, so this test cannot exercise "+
			"the refusal it is named for", len(bundle), maxCACertPEM)
	}

	b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available"}

	f := newFakeEC2(t)
	f.respond = b.reply

	p := newTestProvider(t, f, nil)

	_, err := p.BuildImage(t.Context(), BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest,
		BaseImage: "ami-base", InstanceType: "c7i.xlarge",
		Arch: "x64", RunnerVersion: "2.328.0", Name: "test-image",
		CACertPEM: bundle,
	})
	if err == nil {
		t.Fatal("an oversized CA bundle produced no error")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("the error does not name the user-data limit: %v", err)
	}

	if n := f.countOf("RunInstances"); n != 0 {
		t.Errorf("%d paid instances were launched for a build that could never fit its user-data", n)
	}
}

// mangleCertBody puts a non-base64 character into a certificate PEM's body,
// leaving its BEGIN/END armor intact. pem.Decode cannot base64-decode it and so
// SKIPS the whole block silently when a valid one follows — the exact case the
// delimiter count exists to catch (a merely wrong-DER body would instead be
// RETURNED and rejected by x509.ParseCertificate, a different path).
func mangleCertBody(t *testing.T, certPEM string) string {
	t.Helper()

	lines := strings.Split(certPEM, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "-----") || l == "" {
			continue
		}
		// '!' is not in the base64 alphabet, so the block no longer decodes.
		lines[i] = "!" + l[1:]

		return strings.Join(lines, "\n")
	}

	t.Fatal("no base64 body line to mangle")

	return ""
}

// THE BOUND ADMITS THE CERTIFICATE AN OPERATOR ACTUALLY HAS.
//
// Every other test here mints ECDSA P-256, which at ~558 bytes is the smallest
// certificate that exists -- so a bound could shrink to a third of a real root
// and the whole suite would stay green. A private cache CA is overwhelmingly an
// RSA root, and the size of one is what decides whether maxCACertPEM is a bound
// or an obstruction.
//
// AND THE COMMENT ON THE CONSTANT IS WHAT THIS PINS. It reasons in measured
// numbers -- an RSA-2048 root fits, an RSA-2048 root plus intermediate does not
// -- and reasoning in a comment goes stale silently. Here it fails.
func TestTheCABoundAdmitsARealRSARoot(t *testing.T) {
	t.Parallel()

	for _, bits := range []int{2048, 4096} {
		t.Run(fmt.Sprintf("rsa-%d", bits), func(t *testing.T) {
			t.Parallel()

			key, err := rsa.GenerateKey(rand.Reader, bits)
			if err != nil {
				t.Fatalf("key: %v", err)
			}

			tmpl := &x509.Certificate{
				SerialNumber:          big.NewInt(1),
				Subject:               pkix.Name{CommonName: "billet-cache-ca-rsa"},
				IsCA:                  true,
				BasicConstraintsValid: true,
				KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
			}

			der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
			if err != nil {
				t.Fatalf("cert: %v", err)
			}

			pemData := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))

			if len(pemData) > maxCACertPEM {
				t.Fatalf("an RSA-%d self-signed root is %d bytes and maxCACertPEM is %d, so "+
					"billet refuses the certificate a private cache CA actually is. The "+
					"bound is measured against the script's share of a 16384-byte user-data "+
					"budget; if the script grew again the fix is not a smaller bound.",
					bits, len(pemData), maxCACertPEM)
			}

			if _, err := canonicalizeCACert(pemData); err != nil {
				t.Fatalf("canonicalizeCACert refused an RSA-%d root: %v", bits, err)
			}
		})
	}
}
